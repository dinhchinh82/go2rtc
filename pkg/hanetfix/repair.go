// Package hanetfix implements an opt-in RTSP/RTP compatibility repair for
// HANET cameras that advertise H.265 in SDP while emitting H.264 NAL units
// fragmented using malformed HEVC FU headers. No transcoding is performed.
package hanetfix

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync/atomic"

	"github.com/pion/rtp"
)

var (
	// ErrUnconfirmed indicates HANET evidence was not confirmed on the source.
	ErrUnconfirmed = errors.New("hanetfix: HANET signature unconfirmed")
	// ErrCodecMismatch indicates active RTP packets do not match confirmed HANET H.264.
	ErrCodecMismatch = errors.New("hanetfix: active stream switched away from HANET payload")
	// ErrParameterSetChanged indicates initial in-band SPS/PPS differed from probed SDP.
	ErrParameterSetChanged = errors.New("hanetfix: in-band SPS/PPS differs from probed parameters")
)

const (
	// MaxPendingPackets caps buffered fragments for a single NAL unit.
	MaxPendingPackets = 2048
	// MaxPendingBytes caps buffered payload bytes for a single NAL unit.
	MaxPendingBytes = 2 * 1024 * 1024
)

// Stats tracks atomic operational counters per RTSP connection.
type Stats struct {
	Candidate        atomic.Uint64
	ConfirmedHANET   atomic.Uint64
	ConfirmedHEVC    atomic.Uint64
	Passthrough      atomic.Uint64
	ProbeFailures    atomic.Uint64
	RepairedPackets  atomic.Uint64
	SingleNALPackets atomic.Uint64
	DroppedPackets   atomic.Uint64
	Resyncs          atomic.Uint64
	ParamSetChanges  atomic.Uint64
	Reconnects       atomic.Uint64
	CodecMismatches  atomic.Uint64
}

// StatsSnapshot is a plain-value copy of Stats safe to inspect without locks.
type StatsSnapshot struct {
	Candidate        uint64 `json:"candidate"`
	ConfirmedHANET   uint64 `json:"confirmed_hanet"`
	ConfirmedHEVC    uint64 `json:"confirmed_hevc"`
	Passthrough      uint64 `json:"passthrough"`
	ProbeFailures    uint64 `json:"probe_failures"`
	RepairedPackets  uint64 `json:"repaired_packets"`
	SingleNALPackets uint64 `json:"single_nal_packets"`
	DroppedPackets   uint64 `json:"dropped_packets"`
	Resyncs          uint64 `json:"resyncs"`
	ParamSetChanges  uint64 `json:"param_set_changes"`
	Reconnects       uint64 `json:"reconnects"`
	CodecMismatches  uint64 `json:"codec_mismatches"`
}

// Snapshot returns a point-in-time copy of all counters.
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		Candidate:        s.Candidate.Load(),
		ConfirmedHANET:   s.ConfirmedHANET.Load(),
		ConfirmedHEVC:    s.ConfirmedHEVC.Load(),
		Passthrough:      s.Passthrough.Load(),
		ProbeFailures:    s.ProbeFailures.Load(),
		RepairedPackets:  s.RepairedPackets.Load(),
		SingleNALPackets: s.SingleNALPackets.Load(),
		DroppedPackets:   s.DroppedPackets.Load(),
		Resyncs:          s.Resyncs.Load(),
		ParamSetChanges:  s.ParamSetChanges.Load(),
		Reconnects:       s.Reconnects.Load(),
		CodecMismatches:  s.CodecMismatches.Load(),
	}
}

// Evidence holds validated H.264 parameter sets collected from the actual
// source stream during classification.
type Evidence struct {
	SPS       []byte
	PPS       []byte
	SPSInfo   SPSInfo
	PPSInfo   PPSInfo
	Confirmed bool
	SEICount  int
}

// Clone returns a deep copy of the Evidence byte slices.
func (e Evidence) Clone() Evidence {
	return Evidence{
		SPS:       append([]byte(nil), e.SPS...),
		PPS:       append([]byte(nil), e.PPS...),
		SPSInfo:   e.SPSInfo,
		PPSInfo:   e.PPSInfo,
		Confirmed: e.Confirmed,
		SEICount:  e.SEICount,
	}
}

// Fmtp returns the RFC 6184 fmtp string built from the source's own SPS and PPS.
func (e Evidence) Fmtp() string {
	if !e.Confirmed || len(e.SPS) < 5 || len(e.PPS) < 2 {
		return ""
	}
	return "packetization-mode=1;profile-level-id=" + hex.EncodeToString(e.SPS[1:4]) +
		";sprop-parameter-sets=" + base64.StdEncoding.EncodeToString(e.SPS) + "," +
		base64.StdEncoding.EncodeToString(e.PPS)
}

// Observe evaluates a raw RTP payload for HANET evidence. Callers that need
// strict contiguous IDR chain and HEVC disambiguation should prefer Classifier.
func (e *Evidence) Observe(p []byte) bool {
	if len(p) == 0 {
		return e.Confirmed
	}
	switch {
	case p[0] == 0x67:
		if info, ok := ParseH264SPS(p); ok {
			e.SPS = append([]byte(nil), p...)
			e.SPSInfo = info
		}
	case p[0] == 0x68:
		if info, ok := ParseH264PPS(p); ok {
			e.PPS = append([]byte(nil), p...)
			e.PPSInfo = info
		}
	case p[0] == 0x06:
		e.SEICount++
	case len(p) >= 4 && p[0] == 0x63 && p[2] == 0xb2:
		if len(e.SPS) >= 5 && len(e.PPS) >= 2 && e.PPSInfo.SPSID == e.SPSInfo.ID {
			if ValidateH264IDRSliceHeader(append([]byte{p[1]}, p[3:]...), e.SPSInfo, e.PPSInfo) {
				e.Confirmed = true
			}
		}
	}
	return e.Confirmed
}

// Payload restores a malformed HANET FU packet into an RFC 6184 H.264 FU-A
// packet, or passes through an allowed H.264 single NAL unit.
// For FU start fragments only, p[1] (the second byte of the original H.264 NAL)
// is restored immediately after the FU-A header.
func Payload(p []byte) (out []byte, typ byte, start, end, ok bool) {
	if len(p) == 0 {
		return nil, 0, false, false, false
	}

	if p[0] == 0x63 && len(p) >= 4 && (p[2]&0x3f == 48 || p[2]&0x3f == 50) {
		start = p[2]&0x80 != 0
		end = p[2]&0x40 != 0
		if start && end {
			return nil, 0, false, false, false
		}
		nal := (p[0] & 0x81) | ((p[2] & 0x3f) << 1)
		typ = nal & 0x1f
		out = make([]byte, 0, len(p))
		out = append(out, (nal&0xe0)|28, (p[2]&0xc0)|typ)
		if start {
			out = append(out, p[1])
		}
		out = append(out, p[3:]...)
		return out, typ, start, end, true
	}

	switch p[0] {
	case 0x67:
		if len(p) < 4 {
			return nil, 0, false, false, false
		}
		return append([]byte(nil), p...), p[0] & 0x1f, true, true, true
	case 0x68, 0x06, 0x09, 0x61, 0x65, 0x41:
		if len(p) < 2 {
			return nil, 0, false, false, false
		}
		return append([]byte(nil), p...), p[0] & 0x1f, true, true, true
	}

	return nil, 0, false, false, false
}

// Classification represents the verdict of probing an ambiguous rtsp_demo H265 track.
type Classification uint8

const (
	ClassPending Classification = iota
	ClassConfirmedHANET
	ClassConfirmedHEVC
	ClassAmbiguous
)

// Classifier distinguishes malformed HANET streams from genuine single-layer
// HEVC streams during probe observation.
type Classifier struct {
	state Classification

	sps       []byte
	pps       []byte
	spsInfo   SPSInfo
	ppsInfo   PPSInfo
	hasSPS    bool
	hasPPS    bool
	seiCount  int
	hanetFUs  int
	idrChains int

	idrActive bool
	idrSeq    uint16
	idrTS     uint32
	idrSSRC   uint32
	idrPrefix []byte

	hevcVPS        bool
	hevcSPS        bool
	hevcPPS        bool
	hevcVCLCount   int
	hevcFUChains   int
	hevcFUActive   bool
	hevcFUSeq      uint16
	hevcFUTS       uint32
	hevcFUSSRC     uint32
	hevcFUType     uint8
	hevcSeen       int
	unexpectedPkts int
}

// State returns the current Classification verdict.
func (cl *Classifier) State() Classification {
	return cl.state
}

// Evidence returns a copy of the collected HANET Evidence.
func (cl *Classifier) Evidence() Evidence {
	return Evidence{
		SPS:       append([]byte(nil), cl.sps...),
		PPS:       append([]byte(nil), cl.pps...),
		SPSInfo:   cl.spsInfo,
		PPSInfo:   cl.ppsInfo,
		Confirmed: cl.state == ClassConfirmedHANET,
		SEICount:  cl.seiCount,
	}
}

// ObservePacket inspects one RTP packet from the candidate video track and
// updates the classification verdict once conclusive evidence is reached.
func (cl *Classifier) ObservePacket(pkt *rtp.Packet) Classification {
	if cl.state == ClassConfirmedHANET || cl.state == ClassConfirmedHEVC {
		return cl.state
	}
	if pkt == nil || len(pkt.Payload) == 0 {
		return cl.state
	}
	p := pkt.Payload

	// 1. Check genuine single-layer HEVC first (disjoint header space).
	if kind, ok := InspectHEVCPacket(p); ok {
		cl.hevcSeen++
		cl.idrActive = false
		switch kind {
		case HEVCVPS:
			cl.hevcVPS = true
		case HEVCSPS:
			cl.hevcSPS = true
		case HEVCPPS:
			cl.hevcPPS = true
		case HEVCVCL:
			cl.hevcVCLCount++
		case HEVCAP:
			cl.inspectHEVCAP(p)
		case HEVCFU:
			cl.observeHEVCFU(pkt)
		}
		cl.evaluate()
		return cl.state
	}

	cl.hevcFUActive = false

	// 2. Check HANET malformed H.264 signals.
	switch {
	case p[0] == 0x67:
		cl.idrActive = false
		if info, ok := ParseH264SPS(p); ok {
			cl.sps = append([]byte(nil), p...)
			cl.spsInfo = info
			cl.hasSPS = true
		} else {
			cl.unexpectedPkts++
		}

	case p[0] == 0x68:
		cl.idrActive = false
		if info, ok := ParseH264PPS(p); ok {
			cl.pps = append([]byte(nil), p...)
			cl.ppsInfo = info
			cl.hasPPS = true
		} else {
			cl.unexpectedPkts++
		}

	case p[0] == 0x06 && len(p) >= 2:
		cl.idrActive = false
		cl.seiCount++

	case p[0] == 0x63 && len(p) >= 4 && (p[2]&0x3f == 48 || p[2]&0x3f == 50):
		start := p[2]&0x80 != 0
		end := p[2]&0x40 != 0
		if start && end {
			cl.idrActive = false
			cl.unexpectedPkts++
			break
		}
		cl.hanetFUs++
		fuType := p[2] & 0x3f
		if fuType != 50 {
			cl.idrActive = false
			break
		}
		if start {
			cl.idrActive = true
			cl.idrSeq = pkt.SequenceNumber
			cl.idrTS = pkt.Timestamp
			cl.idrSSRC = pkt.SSRC
			prefix := make([]byte, 0, 16)
			prefix = append(prefix, p[1])
			body := p[3:]
			if len(body) > 15 {
				body = body[:15]
			}
			prefix = append(prefix, body...)
			cl.idrPrefix = prefix
		} else if cl.idrActive {
			if uint16(cl.idrSeq+1) != pkt.SequenceNumber || cl.idrTS != pkt.Timestamp || cl.idrSSRC != pkt.SSRC {
				cl.idrActive = false
				cl.idrPrefix = nil
				break
			}
			cl.idrSeq = pkt.SequenceNumber
			if len(cl.idrPrefix) < 16 {
				need := 16 - len(cl.idrPrefix)
				body := p[3:]
				if len(body) > need {
					body = body[:need]
				}
				cl.idrPrefix = append(cl.idrPrefix, body...)
			}
			if end {
				cl.idrActive = false
				if cl.hasSPS && cl.hasPPS && cl.ppsInfo.SPSID == cl.spsInfo.ID &&
					ValidateH264IDRSliceHeader(cl.idrPrefix, cl.spsInfo, cl.ppsInfo) {
					cl.idrChains++
				}
			}
		}

	case p[0] == 0x65 || p[0] == 0x61 || p[0] == 0x41 || p[0] == 0x09:
		cl.idrActive = false
		if p[0] == 0x65 && len(p) >= 3 && cl.hasSPS && cl.hasPPS && cl.ppsInfo.SPSID == cl.spsInfo.ID {
			if ValidateH264IDRSliceHeader(p[1:], cl.spsInfo, cl.ppsInfo) {
				cl.idrChains++
			}
		}

	default:
		cl.idrActive = false
		cl.unexpectedPkts++
	}

	cl.evaluate()
	return cl.state
}

func (cl *Classifier) inspectHEVCAP(p []byte) {
	off := 2
	for off+2 <= len(p) {
		sz := int(p[off])<<8 | int(p[off+1])
		off += 2
		if sz < 2 || off+sz > len(p) {
			return
		}
		inner := p[off : off+sz]
		nut := (inner[0] >> 1) & 0x3f
		switch {
		case nut == 32 && len(inner) >= 22:
			cl.hevcVPS = true
		case nut == 33 && len(inner) >= 15:
			cl.hevcSPS = true
		case nut == 34 && len(inner) >= 4:
			cl.hevcPPS = true
		case nut <= 1 || (nut >= 16 && nut <= 21):
			cl.hevcVCLCount++
		}
		off += sz
	}
}

func (cl *Classifier) observeHEVCFU(pkt *rtp.Packet) {
	p := pkt.Payload
	start := p[2]&0x80 != 0
	end := p[2]&0x40 != 0
	fuType := p[2] & 0x3f
	if start {
		cl.hevcFUActive = true
		cl.hevcFUSeq = pkt.SequenceNumber
		cl.hevcFUTS = pkt.Timestamp
		cl.hevcFUSSRC = pkt.SSRC
		cl.hevcFUType = fuType
		return
	}
	if !cl.hevcFUActive {
		return
	}
	if uint16(cl.hevcFUSeq+1) != pkt.SequenceNumber || cl.hevcFUTS != pkt.Timestamp ||
		cl.hevcFUSSRC != pkt.SSRC || cl.hevcFUType != fuType {
		cl.hevcFUActive = false
		return
	}
	cl.hevcFUSeq = pkt.SequenceNumber
	if end {
		cl.hevcFUActive = false
		cl.hevcFUChains++
	}
}

func (cl *Classifier) evaluate() {
	if cl.hevcVPS || cl.hevcSPS || cl.hevcPPS {
		if (cl.hevcSPS && cl.hevcPPS && (cl.hevcVCLCount > 0 || cl.hevcFUChains > 0)) ||
			(cl.hevcVPS && cl.hevcSPS && cl.hevcPPS) {
			cl.state = ClassConfirmedHEVC
		}
		return
	}

	if cl.hevcFUChains >= 2 && !cl.hasSPS && !cl.hasPPS && cl.hanetFUs == 0 {
		cl.state = ClassConfirmedHEVC
		return
	}

	if cl.hasSPS && cl.hasPPS && cl.ppsInfo.SPSID == cl.spsInfo.ID &&
		cl.idrChains >= 1 && cl.hanetFUs > 0 && cl.hevcSeen == 0 {
		cl.state = ClassConfirmedHANET
	}
}

// Converter repairs malformed HANET video RTP packets into valid RFC 6184 H.264
// RTP packets, buffering fragmented NALs until complete and suppressing P-frames
// until a complete valid IDR has been received.
type Converter struct {
	stats *Stats

	expectedSPS []byte
	expectedPPS []byte

	lastSPS     []byte
	lastPPS     []byte
	lastSPSInfo SPSInfo
	lastPPSInfo PPSInfo
	hasSPS      bool
	hasPPS      bool

	initialValidated bool
	ready            bool

	pending      []*rtp.Packet
	pendingBytes int
	pendingTS    uint32
	pendingSSRC  uint32
	pendingType  byte

	hasLastIn  bool
	lastInSeq  uint16
	lastInSSRC uint32

	outSeqInit bool
	outSeq     uint16
}

// NewConverter constructs a Converter seeded with confirmed probe Evidence.
func NewConverter(ev Evidence, stats *Stats) *Converter {
	c := &Converter{stats: stats}
	c.seedEvidence(ev)
	return c
}

// ResetForReconnect clears transient fragmentation/readiness state for a new
// RTSP session while keeping the outgoing RTP sequence number monotonic.
func (c *Converter) ResetForReconnect(ev Evidence) {
	c.pending = nil
	c.pendingBytes = 0
	c.pendingTS = 0
	c.pendingSSRC = 0
	c.pendingType = 0
	c.hasLastIn = false
	c.lastInSeq = 0
	c.lastInSSRC = 0
	c.ready = false
	c.initialValidated = false
	c.seedEvidence(ev)
}

func (c *Converter) seedEvidence(ev Evidence) {
	c.expectedSPS = append([]byte(nil), ev.SPS...)
	c.expectedPPS = append([]byte(nil), ev.PPS...)
	if len(ev.SPS) >= 5 {
		c.lastSPS = append([]byte(nil), ev.SPS...)
		c.lastSPSInfo = ev.SPSInfo
		c.hasSPS = true
	} else {
		c.lastSPS = nil
		c.hasSPS = false
	}
	if len(ev.PPS) >= 2 {
		c.lastPPS = append([]byte(nil), ev.PPS...)
		c.lastPPSInfo = ev.PPSInfo
		c.hasPPS = true
	} else {
		c.lastPPS = nil
		c.hasPPS = false
	}
}

func (c *Converter) dropPending(resync bool) {
	if n := len(c.pending); n > 0 {
		if c.stats != nil {
			c.stats.DroppedPackets.Add(uint64(n))
		}
		c.pending = nil
		c.pendingBytes = 0
	}
	if resync {
		if c.ready && c.stats != nil {
			c.stats.Resyncs.Add(1)
		}
		c.ready = false
	}
}

func (c *Converter) recordDrop() {
	if c.stats != nil {
		c.stats.DroppedPackets.Add(1)
	}
}

func (c *Converter) nextSeq(inSeq uint16) uint16 {
	if !c.outSeqInit {
		c.outSeqInit = true
		c.outSeq = inSeq
	} else {
		c.outSeq++
	}
	return c.outSeq
}

func (c *Converter) cloneWithPayload(in *rtp.Packet, payload []byte, marker bool) *rtp.Packet {
	clone := *in
	clone.Payload = payload
	clone.Marker = marker
	return &clone
}

func (c *Converter) emitSingle(pkt *rtp.Packet) []*rtp.Packet {
	pkt.SequenceNumber = c.nextSeq(pkt.SequenceNumber)
	if c.stats != nil {
		c.stats.SingleNALPackets.Add(1)
	}
	return []*rtp.Packet{pkt}
}

// Process repairs one incoming RTP video packet and returns zero or more ready,
// sequence-contiguous H.264 RTP packets. A non-nil error indicates a runtime
// codec or parameter-set mismatch requiring session tear-down / renegotiation.
func (c *Converter) Process(in *rtp.Packet) ([]*rtp.Packet, error) {
	if in == nil || len(in.Payload) == 0 {
		c.recordDrop()
		return nil, nil
	}

	// Runtime guard: if the camera switched to genuine single-layer HEVC, fail fast.
	if _, isHEVC := InspectHEVCPacket(in.Payload); isHEVC {
		c.dropPending(true)
		if c.stats != nil {
			c.stats.CodecMismatches.Add(1)
		}
		return nil, ErrCodecMismatch
	}

	// Duplicate packet check (same SSRC & sequence number).
	if c.hasLastIn && in.SSRC == c.lastInSSRC && in.SequenceNumber == c.lastInSeq {
		c.recordDrop()
		return nil, nil
	}

	// SSRC switch resets stream continuity cleanly.
	if c.hasLastIn && in.SSRC != c.lastInSSRC {
		c.dropPending(true)
		c.hasSPS = false
		c.hasPPS = false
		if c.stats != nil {
			c.stats.Resyncs.Add(1)
		}
	}

	out, typ, start, end, ok := Payload(in.Payload)
	if !ok {
		c.dropPending(true)
		c.recordDrop()
		if c.stats != nil {
			c.stats.Resyncs.Add(1)
		}
		c.hasLastIn = true
		c.lastInSeq = in.SequenceNumber
		c.lastInSSRC = in.SSRC
		return nil, nil
	}

	// Check fragment continuity if a fragmented NAL is already buffered.
	if len(c.pending) > 0 {
		if start || uint16(c.lastInSeq+1) != in.SequenceNumber ||
			in.Timestamp != c.pendingTS || in.SSRC != c.pendingSSRC || typ != c.pendingType {
			c.dropPending(true)
			if !start {
				c.recordDrop()
				c.hasLastIn = true
				c.lastInSeq = in.SequenceNumber
				c.lastInSSRC = in.SSRC
				return nil, nil
			}
		}
	} else if c.ready && c.hasLastIn && uint16(c.lastInSeq+1) != in.SequenceNumber {
		// Packet loss between NALs: keep going only on keyframe/parameter boundaries.
		if typ != 5 && typ != 6 && typ != 7 && typ != 8 && typ != 9 {
			c.ready = false
			if c.stats != nil {
				c.stats.Resyncs.Add(1)
			}
		}
	}

	c.hasLastIn = true
	c.lastInSeq = in.SequenceNumber
	c.lastInSSRC = in.SSRC

	// Single NAL unit path.
	if start && end {
		switch typ {
		case 7: // SPS
			info, valid := ParseH264SPS(out)
			if !valid {
				c.recordDrop()
				return nil, nil
			}
			if !c.initialValidated && len(c.expectedSPS) > 0 && !bytes.Equal(out, c.expectedSPS) {
				return nil, ErrParameterSetChanged
			}
			if c.initialValidated && len(c.lastSPS) > 0 && !bytes.Equal(out, c.lastSPS) {
				if c.stats != nil {
					c.stats.ParamSetChanges.Add(1)
				}
				c.ready = false
			}
			c.lastSPS = out
			c.lastSPSInfo = info
			c.hasSPS = true
			if !c.ready {
				return nil, nil
			}
			return c.emitSingle(c.cloneWithPayload(in, out, in.Marker)), nil

		case 8: // PPS
			info, valid := ParseH264PPS(out)
			if !valid {
				c.recordDrop()
				return nil, nil
			}
			if !c.initialValidated && len(c.expectedPPS) > 0 && !bytes.Equal(out, c.expectedPPS) {
				return nil, ErrParameterSetChanged
			}
			if c.initialValidated && len(c.lastPPS) > 0 && !bytes.Equal(out, c.lastPPS) {
				if c.stats != nil {
					c.stats.ParamSetChanges.Add(1)
				}
				c.ready = false
			}
			c.lastPPS = out
			c.lastPPSInfo = info
			c.hasPPS = true
			if !c.ready {
				return nil, nil
			}
			return c.emitSingle(c.cloneWithPayload(in, out, in.Marker)), nil

		case 6, 9: // SEI / AUD
			if !c.ready {
				return nil, nil
			}
			return c.emitSingle(c.cloneWithPayload(in, out, in.Marker)), nil

		case 5: // Single-NAL IDR
			if !c.hasSPS || !c.hasPPS || c.lastPPSInfo.SPSID != c.lastSPSInfo.ID {
				c.recordDrop()
				return nil, nil
			}
			if !ValidateH264IDRSliceHeader(out[1:], c.lastSPSInfo, c.lastPPSInfo) {
				c.recordDrop()
				return nil, nil
			}
			wasReady := c.ready
			c.ready = true
			c.initialValidated = true
			if !wasReady {
				res := make([]*rtp.Packet, 0, 3)
				spsPkt := c.cloneWithPayload(in, append([]byte(nil), c.lastSPS...), false)
				spsPkt.SequenceNumber = c.nextSeq(in.SequenceNumber)
				ppsPkt := c.cloneWithPayload(in, append([]byte(nil), c.lastPPS...), false)
				ppsPkt.SequenceNumber = c.nextSeq(in.SequenceNumber)
				idrPkt := c.cloneWithPayload(in, out, in.Marker)
				idrPkt.SequenceNumber = c.nextSeq(in.SequenceNumber)
				res = append(res, spsPkt, ppsPkt, idrPkt)
				if c.stats != nil {
					c.stats.SingleNALPackets.Add(3)
				}
				return res, nil
			}
			return c.emitSingle(c.cloneWithPayload(in, out, in.Marker)), nil

		case 1: // Single-NAL non-IDR slice
			if !c.ready {
				c.recordDrop()
				return nil, nil
			}
			return c.emitSingle(c.cloneWithPayload(in, out, in.Marker)), nil

		default:
			c.recordDrop()
			return nil, nil
		}
	}

	// Fragmented FU-A path: buffer until END fragment arrives so no partial/corrupt
	// fragments are ever emitted after packet loss.
	if !start && len(c.pending) == 0 {
		c.recordDrop()
		if c.ready && c.stats != nil {
			c.stats.Resyncs.Add(1)
		}
		c.ready = false
		return nil, nil
	}

	if start && typ == 1 && !c.ready {
		c.recordDrop()
		return nil, nil
	}

	if len(c.pending)+1 > MaxPendingPackets || c.pendingBytes+len(out) > MaxPendingBytes {
		c.dropPending(true)
		c.recordDrop()
		return nil, nil
	}

	if start {
		c.pendingTS = in.Timestamp
		c.pendingSSRC = in.SSRC
		c.pendingType = typ
	}

	c.pending = append(c.pending, c.cloneWithPayload(in, out, in.Marker))
	c.pendingBytes += len(out)

	if !end {
		return nil, nil
	}

	pkts := c.pending
	c.pending = nil
	c.pendingBytes = 0

	if typ == 5 {
		if !c.hasSPS || !c.hasPPS || c.lastPPSInfo.SPSID != c.lastSPSInfo.ID {
			if c.stats != nil {
				c.stats.DroppedPackets.Add(uint64(len(pkts)))
			}
			return nil, nil
		}
		if len(pkts[0].Payload) < 3 || !ValidateH264IDRSliceHeader(pkts[0].Payload[2:], c.lastSPSInfo, c.lastPPSInfo) {
			if c.stats != nil {
				c.stats.DroppedPackets.Add(uint64(len(pkts)))
			}
			c.ready = false
			return nil, nil
		}

		wasReady := c.ready
		c.ready = true
		c.initialValidated = true

		if !wasReady {
			outPkts := make([]*rtp.Packet, 0, len(pkts)+2)
			spsPkt := c.cloneWithPayload(in, append([]byte(nil), c.lastSPS...), false)
			spsPkt.SequenceNumber = c.nextSeq(pkts[0].SequenceNumber)
			ppsPkt := c.cloneWithPayload(in, append([]byte(nil), c.lastPPS...), false)
			ppsPkt.SequenceNumber = c.nextSeq(pkts[0].SequenceNumber)
			outPkts = append(outPkts, spsPkt, ppsPkt)
			for _, p := range pkts {
				p.SequenceNumber = c.nextSeq(p.SequenceNumber)
				outPkts = append(outPkts, p)
			}
			if c.stats != nil {
				c.stats.SingleNALPackets.Add(2)
				c.stats.RepairedPackets.Add(uint64(len(pkts)))
			}
			return outPkts, nil
		}
	} else if !c.ready {
		if c.stats != nil {
			c.stats.DroppedPackets.Add(uint64(len(pkts)))
		}
		return nil, nil
	}

	for _, p := range pkts {
		p.SequenceNumber = c.nextSeq(p.SequenceNumber)
	}
	if c.stats != nil {
		c.stats.RepairedPackets.Add(uint64(len(pkts)))
	}
	return pkts, nil
}
