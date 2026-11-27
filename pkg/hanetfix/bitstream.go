package hanetfix

// SPSInfo holds validated fields extracted from an H.264 Sequence Parameter Set.
type SPSInfo struct {
	ID              uint32
	ProfileIDC      uint8
	ConstraintFlags uint8
	LevelIDC        uint8
	WidthMbs        uint32
	HeightMbs       uint32
	Width           uint32
	Height          uint32
}

// PPSInfo holds validated fields extracted from an H.264 Picture Parameter Set.
type PPSInfo struct {
	ID    uint32
	SPSID uint32
}

// HEVCPacketKind classifies single-layer HEVC RTP payloads strictly disjoint
// from the malformed HANET stream.
type HEVCPacketKind uint8

const (
	HEVCNone HEVCPacketKind = iota
	HEVCVPS
	HEVCSPS
	HEVCPPS
	HEVCVCL
	HEVCFU
	HEVCAP
)

type bitReader struct {
	data []byte
	pos  int
	bit  uint8
	eof  bool
}

func (r *bitReader) readBit() uint32 {
	if r.eof || r.pos >= len(r.data) {
		r.eof = true
		return 0
	}
	v := uint32((r.data[r.pos] >> (7 - r.bit)) & 1)
	r.bit++
	if r.bit == 8 {
		r.bit = 0
		r.pos++
	}
	return v
}

func (r *bitReader) readBits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = (v << 1) | r.readBit()
		if r.eof {
			return 0
		}
	}
	return v
}

func (r *bitReader) readUE() uint32 {
	var zeros int
	for !r.eof {
		if r.readBit() == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			r.eof = true
			return 0
		}
	}
	if r.eof {
		return 0
	}
	if zeros == 0 {
		return 0
	}
	return ((uint32(1) << zeros) - 1) + r.readBits(zeros)
}

func (r *bitReader) readSE() int32 {
	ue := r.readUE()
	if r.eof {
		return 0
	}
	if ue&1 == 1 {
		return int32((ue + 1) >> 1)
	}
	return -int32(ue >> 1)
}

// ebspToRBSP removes H.264/H.265 emulation prevention 0x03 bytes and rejects
// unescaped Annex B start codes inside a single NAL unit.
func ebspToRBSP(ebsp []byte) ([]byte, bool) {
	rbsp := make([]byte, 0, len(ebsp))
	for i := 0; i < len(ebsp); i++ {
		if i+2 < len(ebsp) && ebsp[i] == 0 && ebsp[i+1] == 0 {
			switch ebsp[i+2] {
			case 0, 1:
				return nil, false
			case 3:
				rbsp = append(rbsp, 0, 0)
				i += 2
				continue
			}
		}
		rbsp = append(rbsp, ebsp[i])
	}
	return rbsp, true
}

func isKnownH264Profile(profile uint8) bool {
	switch profile {
	case 66, 77, 88, 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

func hasHighProfileFields(profile uint8) bool {
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

func skipScalingList(r *bitReader, size int) {
	lastScale := int32(8)
	nextScale := int32(8)
	for j := 0; j < size && !r.eof; j++ {
		if nextScale != 0 {
			delta := r.readSE()
			if r.eof {
				return
			}
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
}

// ParseH264SPS parses and validates an H.264 SPS NAL unit (without Annex B start code).
func ParseH264SPS(nal []byte) (SPSInfo, bool) {
	if len(nal) < 5 {
		return SPSInfo{}, false
	}
	if nal[0]&0x80 != 0 || nal[0]&0x1f != 7 || nal[0]&0x60 == 0 {
		return SPSInfo{}, false
	}
	profileIDC := nal[1]
	if !isKnownH264Profile(profileIDC) {
		return SPSInfo{}, false
	}
	levelIDC := nal[3]
	if levelIDC < 9 || levelIDC > 62 {
		return SPSInfo{}, false
	}

	rbsp, ok := ebspToRBSP(nal[1:])
	if !ok || len(rbsp) < 4 {
		return SPSInfo{}, false
	}

	r := bitReader{data: rbsp[3:]}
	spsID := r.readUE()
	if r.eof || spsID > 31 {
		return SPSInfo{}, false
	}

	chromaFormatIDC := uint32(1)
	if hasHighProfileFields(profileIDC) {
		chromaFormatIDC = r.readUE()
		if r.eof || chromaFormatIDC > 3 {
			return SPSInfo{}, false
		}
		n := 8
		if chromaFormatIDC == 3 {
			_ = r.readBit() // separate_colour_plane_flag
			n = 12
		}
		if r.readUE() > 6 || r.readUE() > 6 {
			return SPSInfo{}, false
		}
		_ = r.readBit() // qpprime_y_zero_transform_bypass_flag
		if r.readBit() == 1 {
			for i := 0; i < n && !r.eof; i++ {
				if r.readBit() == 1 {
					if i < 6 {
						skipScalingList(&r, 16)
					} else {
						skipScalingList(&r, 64)
					}
				}
			}
		}
		if r.eof {
			return SPSInfo{}, false
		}
	}

	if r.readUE() > 12 { // log2_max_frame_num_minus4
		return SPSInfo{}, false
	}

	pocType := r.readUE()
	if r.eof || pocType > 2 {
		return SPSInfo{}, false
	}
	switch pocType {
	case 0:
		if r.readUE() > 12 { // log2_max_pic_order_cnt_lsb_minus4
			return SPSInfo{}, false
		}
	case 1:
		_ = r.readBit() // delta_pic_order_always_zero_flag
		_ = r.readSE()  // offset_for_non_ref_pic
		_ = r.readSE()  // offset_for_top_to_bottom_field
		cycle := r.readUE()
		if r.eof || cycle > 255 {
			return SPSInfo{}, false
		}
		for i := uint32(0); i < cycle && !r.eof; i++ {
			_ = r.readSE()
		}
	}

	maxRefFrames := r.readUE()
	if r.eof || maxRefFrames > 16 {
		return SPSInfo{}, false
	}
	_ = r.readBit() // gaps_in_frame_num_value_allowed_flag

	picWidthMbsMinus1 := r.readUE()
	picHeightMapUnitsMinus1 := r.readUE()
	if r.eof || picWidthMbsMinus1 > 511 || picHeightMapUnitsMinus1 > 511 {
		return SPSInfo{}, false
	}

	frameMbsOnly := r.readBit()
	if frameMbsOnly == 0 {
		_ = r.readBit() // mb_adaptive_frame_field_flag
	}
	_ = r.readBit() // direct_8x8_inference_flag

	var cropL, cropR, cropT, cropB uint32
	if r.readBit() == 1 {
		cropL = r.readUE()
		cropR = r.readUE()
		cropT = r.readUE()
		cropB = r.readUE()
	}
	if r.eof {
		return SPSInfo{}, false
	}

	widthMbs := picWidthMbsMinus1 + 1
	heightMbs := (picHeightMapUnitsMinus1 + 1) * (2 - frameMbsOnly)
	rawW := widthMbs * 16
	rawH := heightMbs * 16

	var subWidthC, subHeightC uint32 = 1, 1
	switch chromaFormatIDC {
	case 1:
		subWidthC, subHeightC = 2, 2
	case 2:
		subWidthC, subHeightC = 2, 1
	}
	cropUnitX := subWidthC
	cropUnitY := subHeightC * (2 - frameMbsOnly)

	cropX := (cropL + cropR) * cropUnitX
	cropY := (cropT + cropB) * cropUnitY
	if cropX >= rawW || cropY >= rawH {
		return SPSInfo{}, false
	}

	width := rawW - cropX
	height := rawH - cropY
	if width < 16 || width > 8192 || height < 16 || height > 8192 {
		return SPSInfo{}, false
	}

	return SPSInfo{
		ID:              spsID,
		ProfileIDC:      profileIDC,
		ConstraintFlags: nal[2],
		LevelIDC:        levelIDC,
		WidthMbs:        widthMbs,
		HeightMbs:       heightMbs,
		Width:           width,
		Height:          height,
	}, true
}

// ParseH264PPS parses and validates an H.264 PPS NAL unit (without Annex B start code).
func ParseH264PPS(nal []byte) (PPSInfo, bool) {
	if len(nal) < 2 {
		return PPSInfo{}, false
	}
	if nal[0]&0x80 != 0 || nal[0]&0x1f != 8 || nal[0]&0x60 == 0 {
		return PPSInfo{}, false
	}
	rbsp, ok := ebspToRBSP(nal[1:])
	if !ok || len(rbsp) == 0 {
		return PPSInfo{}, false
	}

	r := bitReader{data: rbsp}
	ppsID := r.readUE()
	spsID := r.readUE()
	if r.eof || ppsID > 255 || spsID > 31 {
		return PPSInfo{}, false
	}

	_ = r.readBit() // entropy_coding_mode_flag
	_ = r.readBit() // bottom_field_pic_order_in_frame_present_flag
	numSliceGroupsMinus1 := r.readUE()
	if r.eof || numSliceGroupsMinus1 > 7 {
		return PPSInfo{}, false
	}

	if numSliceGroupsMinus1 == 0 {
		if r.readUE() > 31 || r.readUE() > 31 {
			return PPSInfo{}, false
		}
		_ = r.readBit() // weighted_pred_flag
		if r.readBits(2) > 2 {
			return PPSInfo{}, false
		}
		qp := r.readSE()
		qs := r.readSE()
		chromaOff := r.readSE()
		if r.eof || qp < -26 || qp > 25 || qs < -26 || qs > 25 || chromaOff < -12 || chromaOff > 12 {
			return PPSInfo{}, false
		}
		_ = r.readBits(3) // deblocking_filter_control, constrained_intra_pred, redundant_pic_cnt
	}

	if r.eof {
		return PPSInfo{}, false
	}

	return PPSInfo{
		ID:    ppsID,
		SPSID: spsID,
	}, true
}

// ValidateH264IDRSliceHeader verifies that sliceBody (starting right after the
// 1-byte H.264 NAL header) starts with a plausible I/SI slice header referencing pps.
func ValidateH264IDRSliceHeader(sliceBody []byte, sps SPSInfo, pps PPSInfo) bool {
	if len(sliceBody) == 0 || sps.WidthMbs == 0 || sps.HeightMbs == 0 {
		return false
	}
	n := len(sliceBody)
	if n > 16 {
		n = 16
	}
	rbsp, ok := ebspToRBSP(sliceBody[:n])
	if !ok || len(rbsp) == 0 {
		return false
	}

	r := bitReader{data: rbsp}
	firstMB := r.readUE()
	sliceType := r.readUE()
	ppsID := r.readUE()
	if r.eof {
		return false
	}

	maxMBs := sps.WidthMbs * sps.HeightMbs
	if firstMB >= maxMBs {
		return false
	}
	if sliceType > 9 || (sliceType%5 != 2 && sliceType%5 != 4) {
		return false
	}
	if ppsID != pps.ID {
		return false
	}
	return true
}

// InspectHEVCPacket checks if p matches a valid single-layer HEVC (RFC 7798)
// RTP payload. Every valid single-layer HEVC packet has F=0, LayerId=0, TID>=1,
// which is strictly disjoint from all HANET packets.
func InspectHEVCPacket(p []byte) (HEVCPacketKind, bool) {
	if len(p) < 2 {
		return HEVCNone, false
	}
	if p[0]&0x80 != 0 {
		return HEVCNone, false
	}
	layerID := ((p[0] & 0x01) << 5) | (p[1] >> 3)
	tid := p[1] & 0x07
	if layerID != 0 || tid == 0 {
		return HEVCNone, false
	}

	nut := (p[0] >> 1) & 0x3f
	switch nut {
	case 32:
		if len(p) >= 22 {
			return HEVCVPS, true
		}
	case 33:
		if len(p) >= 15 {
			return HEVCSPS, true
		}
	case 34:
		if len(p) >= 4 {
			return HEVCPPS, true
		}
	case 49:
		if len(p) < 4 {
			return HEVCNone, false
		}
		start := p[2]&0x80 != 0
		end := p[2]&0x40 != 0
		if start && end {
			return HEVCNone, false
		}
		fuType := p[2] & 0x3f
		if fuType <= 21 {
			return HEVCFU, true
		}
	case 48:
		if len(p) < 6 {
			return HEVCNone, false
		}
		off := 2
		count := 0
		for off+2 <= len(p) {
			sz := int(p[off])<<8 | int(p[off+1])
			off += 2
			if sz < 2 || off+sz > len(p) {
				return HEVCNone, false
			}
			inner := p[off : off+sz]
			if inner[0]&0x80 != 0 {
				return HEVCNone, false
			}
			innerLayer := ((inner[0] & 0x01) << 5) | (inner[1] >> 3)
			innerTID := inner[1] & 0x07
			if innerLayer != 0 || innerTID == 0 {
				return HEVCNone, false
			}
			count++
			off += sz
		}
		if off == len(p) && count >= 1 {
			return HEVCAP, true
		}
	default:
		if (nut <= 1 || (nut >= 16 && nut <= 21)) && len(p) >= 4 {
			return HEVCVCL, true
		}
	}
	return HEVCNone, false
}
