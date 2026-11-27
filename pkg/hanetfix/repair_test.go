package hanetfix

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/pion/rtp"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// real 1280x736 Baseline SPS & PPS from evidence spec (for unit fixture only)
func sampleSPS(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString("Z0IAIJY1QKALtNwEBAQI")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func samplePPS(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString("aM4xsg==")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validIDRPayload(t *testing.T) []byte {
	t.Helper()
	// 0x65 followed by minimal valid IDR slice header (first_mb=0 -> 1, slice_type=7 -> 0001000, pps_id=0 -> 1)
	// Bits: 1 0001000 1 -> 10001000 10000000 = 0x88 0x80
	return []byte{0x65, 0x88, 0x80, 0x21, 0x42}
}

func makeRTP(seq uint16, ts, ssrc uint32, pt uint8, marker bool, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: append([]byte(nil), payload...),
	}
}

func TestPayloadCoreVectors(t *testing.T) {
	vectors := []struct {
		name       string
		inHex      string
		wantHex    string
		wantTyp    byte
		wantStart  bool
		wantEnd    bool
		wantAccept bool
	}{
		{"non_idr_start", "63e2b0aabb", "7c81e2aabb", 1, true, false, true},
		{"non_idr_middle", "63e230aabb", "7c01aabb", 1, false, false, true},
		{"non_idr_end", "63e270aabb", "7c41aabb", 1, false, true, true},
		{"idr_start", "63b8b2aabb", "7c85b8aabb", 5, true, false, true},
		{"idr_middle", "63b832aabb", "7c05aabb", 5, false, false, true},
		{"idr_end", "63b872aabb", "7c45aabb", 5, false, true, true},
		{"sps_single", "67420020abcd", "67420020abcd", 7, true, true, true},
		{"pps_single", "68ce31b2", "68ce31b2", 8, true, true, true},
		{"sei_single", "060504", "060504", 6, true, true, true},
		{"aud_single", "0910", "0910", 9, true, true, true},
		{"reject_s_and_e", "63e2f0aabb", "", 0, false, false, false},
		{"reject_truncated", "63e2", "", 0, false, false, false},
		{"reject_unknown_fu", "63e231aabb", "", 0, false, false, false},
		{"reject_hevc_fu", "620193aabb", "", 0, false, false, false},
		{"reject_hevc_vps", "40010c01ffff016000000300b00000030000030078ac09", "", 0, false, false, false},
		{"reject_hevc_sps", "420101016000000300b00000030000030078a003c0801107cb96", "", 0, false, false, false},
		{"reject_hevc_pps", "4401c172b46240", "", 0, false, false, false},
		{"reject_empty", "", "", 0, false, false, false},
	}

	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			in := mustHex(t, tc.inHex)
			out, typ, start, end, ok := Payload(in)
			if ok != tc.wantAccept {
				t.Fatalf("ok=%v want %v (out=%x)", ok, tc.wantAccept, out)
			}
			if !ok {
				return
			}
			if hex.EncodeToString(out) != tc.wantHex || typ != tc.wantTyp || start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("got out=%x typ=%d start=%v end=%v; want out=%s typ=%d start=%v end=%v",
					out, typ, start, end, tc.wantHex, tc.wantTyp, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestParseH264SPSAndPPS(t *testing.T) {
	sps := sampleSPS(t)
	pps := samplePPS(t)

	sInfo, ok := ParseH264SPS(sps)
	if !ok {
		t.Fatal("failed to parse valid 1280x736 SPS")
	}
	if sInfo.Width != 1280 || sInfo.Height != 736 || sInfo.ProfileIDC != 66 {
		t.Fatalf("unexpected SPS info: %+v", sInfo)
	}

	pInfo, ok := ParseH264PPS(pps)
	if !ok || pInfo.SPSID != sInfo.ID {
		t.Fatalf("failed to parse valid PPS: ok=%v info=%+v", ok, pInfo)
	}

	// Valid IDR slice header
	if !ValidateH264IDRSliceHeader([]byte{0x88, 0x80, 0x00}, sInfo, pInfo) {
		t.Fatal("valid IDR slice header rejected")
	}
	// Non-IDR slice_type=0 (P slice -> bits: first_mb=0(1), slice_type=0(1), pps_id=0(1) => 11100000 = 0xE0)
	if ValidateH264IDRSliceHeader([]byte{0xE0}, sInfo, pInfo) {
		t.Fatal("P-slice header accepted as IDR")
	}
	// Truncated SPS & invalid NAL type
	if _, ok := ParseH264SPS([]byte{0x67, 0x42, 0x00}); ok {
		t.Fatal("truncated SPS accepted")
	}
	if _, ok := ParseH264SPS([]byte{0x68, 0x42, 0x00, 0x20, 0x00}); ok {
		t.Fatal("PPS header accepted as SPS")
	}
	// Fake prefix-only 0x67 payload (not valid Golomb SPS)
	if _, ok := ParseH264SPS([]byte{0x67, 0x42, 0x00, 0x20, 0x00, 0x00, 0x00}); ok {
		t.Fatal("illegal start-code fake SPS accepted")
	}
}

func TestInspectAndRewriteSDP(t *testing.T) {
	const baseSDP = "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:track1\r\n" +
		"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/1\r\n" +
		"a=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188\r\n" +
		"a=control:track2\r\n"

	cand, ok, err := InspectSDP(baseSDP)
	if err != nil || !ok || cand.PayloadType != 96 || cand.Control != "track1" {
		t.Fatalf("unexpected candidate result: cand=%+v ok=%v err=%v", cand, ok, err)
	}

	// Dynamic PT (PT=98) and LF-only line endings
	dynSDP := strings.ReplaceAll(strings.ReplaceAll(baseSDP, "\r\n", "\n"), "96", "98")
	cand, ok, err = InspectSDP(dynSDP)
	if err != nil || !ok || cand.PayloadType != 98 {
		t.Fatalf("dynamic PT failed: cand=%+v ok=%v err=%v", cand, ok, err)
	}

	// Non-candidates
	for _, non := range []string{
		strings.ReplaceAll(baseSDP, "H265/90000", "H264/90000"),
		strings.ReplaceAll(baseSDP, "s=rtsp_demo", "s=normal_cam"),
		strings.ReplaceAll(baseSDP, "a=control:track1", "a=fmtp:96 sprop-vps=AAA;sprop-sps=BBB;sprop-pps=CCC\r\na=control:track1"),
	} {
		_, ok, err := InspectSDP(non)
		if err != nil || ok {
			t.Fatalf("false positive candidate on SDP:\n%s\ngot ok=%v err=%v", non, ok, err)
		}
	}

	// Ambiguous multi-track H265 SDP
	ambig := baseSDP + "m=video 0 RTP/AVP 99\r\na=rtpmap:99 H265/90000\r\na=control:track3\r\n"
	if _, ok, err := InspectSDP(ambig); err == nil || ok {
		t.Fatalf("expected ErrAmbiguousSDP, got ok=%v err=%v", ok, err)
	}

	// Rewrite with source's own SPS/PPS and ensure audio lines stay verbatim
	var ev Evidence
	ev.Observe(sampleSPS(t))
	ev.Observe(samplePPS(t))
	ev.Observe([]byte{0x63, 0x88, 0xb2, 0x80, 0x21})
	if !ev.Confirmed {
		t.Fatal("expected evidence confirmed")
	}

	rewritten, err := RewriteSDP(baseSDP, 96, ev)
	if err != nil {
		t.Fatalf("RewriteSDP failed: %v", err)
	}
	if !strings.Contains(rewritten, "a=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;profile-level-id=420020;sprop-parameter-sets=Z0IAIJY1QKALtNwEBAQI,aM4xsg==") {
		t.Fatalf("unexpected rewritten video SDP:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "a=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188") {
		t.Fatalf("audio fmtp altered:\n%s", rewritten)
	}

	if _, err := RewriteSDP(baseSDP, 96, Evidence{}); err == nil {
		t.Fatal("expected error for unconfirmed evidence")
	}
}

func TestClassifierDisambiguation(t *testing.T) {
	// 1. Genuine HANET: requires SPS + PPS + complete contiguous IDR FU chain
	var cl Classifier
	sps := sampleSPS(t)
	pps := samplePPS(t)

	if st := cl.ObservePacket(makeRTP(10, 1000, 1, 96, false, sps)); st != ClassPending {
		t.Fatalf("expected pending after SPS only, got %v", st)
	}
	if st := cl.ObservePacket(makeRTP(11, 1000, 1, 96, false, pps)); st != ClassPending {
		t.Fatalf("expected pending after PPS only, got %v", st)
	}
	// IDR start only must NOT confirm
	if st := cl.ObservePacket(makeRTP(12, 2000, 1, 96, false, []byte{0x63, 0x88, 0xb2, 0x80, 0x21})); st != ClassPending {
		t.Fatalf("expected pending after IDR start only, got %v", st)
	}
	// Broken seq before IDR end -> still pending
	if st := cl.ObservePacket(makeRTP(14, 2000, 1, 96, true, []byte{0x63, 0x88, 0x72, 0x99})); st != ClassPending {
		t.Fatalf("expected pending after broken IDR chain, got %v", st)
	}
	// Contiguous IDR start -> end
	if st := cl.ObservePacket(makeRTP(15, 3000, 1, 96, false, []byte{0x63, 0x88, 0xb2, 0x80, 0x21})); st != ClassPending {
		t.Fatalf("expected pending on IDR start, got %v", st)
	}
	if st := cl.ObservePacket(makeRTP(16, 3000, 1, 96, true, []byte{0x63, 0x88, 0x72, 0xaa, 0xbb})); st != ClassConfirmedHANET {
		t.Fatalf("expected ClassConfirmedHANET after complete IDR chain, got %v", st)
	}

	// 2. Genuine HEVC without fmtp (VPS/SPS/PPS + VCL)
	var hevcCl Classifier
	vps := mustHex(t, "40010c01ffff016000000300b00000030000030078ac09")
	hevcSPS := mustHex(t, "420101016000000300b00000030000030078a003c0801107cb96")
	hevcPPS := mustHex(t, "4401c172b46240")
	hevcIDR := mustHex(t, "2601af08123456")

	hevcCl.ObservePacket(makeRTP(1, 100, 2, 96, false, vps))
	hevcCl.ObservePacket(makeRTP(2, 100, 2, 96, false, hevcSPS))
	hevcCl.ObservePacket(makeRTP(3, 100, 2, 96, false, hevcPPS))
	if st := hevcCl.ObservePacket(makeRTP(4, 100, 2, 96, true, hevcIDR)); st != ClassConfirmedHEVC {
		t.Fatalf("expected ClassConfirmedHEVC, got %v", st)
	}
}

func TestConverterLifecycleAndWraps(t *testing.T) {
	var stats Stats
	sps := sampleSPS(t)
	pps := samplePPS(t)
	var ev Evidence
	ev.Observe(sps)
	ev.Observe(pps)
	ev.Observe([]byte{0x63, 0x88, 0xb2, 0x80, 0x21})
	if !ev.Confirmed {
		t.Fatal("failed to build test evidence")
	}

	conv := NewConverter(ev, &stats)

	// Pre-IDR P-frame (both fragmented and single) must be suppressed.
	if pkts, err := conv.Process(makeRTP(10, 100, 1, 96, false, []byte{0x63, 0xe2, 0xb0, 0xaa})); err != nil || len(pkts) != 0 {
		t.Fatalf("pre-IDR P start emitted: %v %v", pkts, err)
	}
	if pkts, err := conv.Process(makeRTP(11, 100, 1, 96, true, []byte{0x63, 0xe2, 0x70, 0xbb})); err != nil || len(pkts) != 0 {
		t.Fatalf("pre-IDR P end emitted: %v %v", pkts, err)
	}

	// Readiness only on IDR END, across uint16 seq wrap (65535 -> 0) and uint32 TS wrap.
	tsWrap := uint32(0xFFFFFF00)
	if pkts, err := conv.Process(makeRTP(65535, tsWrap, 1, 96, false, []byte{0x63, 0x88, 0xb2, 0x80, 0x21})); err != nil || len(pkts) != 0 {
		t.Fatalf("IDR start emitted before IDR end: %v %v", pkts, err)
	}
	pkts, err := conv.Process(makeRTP(0, tsWrap, 1, 96, true, []byte{0x63, 0x88, 0x72, 0xcc, 0xdd}))
	if err != nil || len(pkts) != 4 {
		t.Fatalf("expected [SPS, PPS, IDR_start, IDR_end], got len=%d err=%v", len(pkts), err)
	}
	if !bytes.Equal(pkts[0].Payload, sps) || !bytes.Equal(pkts[1].Payload, pps) {
		t.Fatal("prepended SPS/PPS mismatch")
	}
	if pkts[0].Marker || pkts[1].Marker || pkts[2].Marker || !pkts[3].Marker {
		t.Fatal("unexpected Marker flags on initial keyframe")
	}
	// Output sequence numbers must be strictly contiguous mod 65536
	for i := 1; i < len(pkts); i++ {
		if uint16(pkts[i-1].SequenceNumber+1) != pkts[i].SequenceNumber {
			t.Fatalf("non-contiguous out seq at %d: %d -> %d", i, pkts[i-1].SequenceNumber, pkts[i].SequenceNumber)
		}
	}

	// Duplicate packet is dropped silently
	if dup, err := conv.Process(makeRTP(0, tsWrap, 1, 96, true, []byte{0x63, 0x88, 0x72, 0xcc, 0xdd})); err != nil || len(dup) != 0 {
		t.Fatalf("duplicate packet emitted: %v %v", dup, err)
	}

	// Next P-frame across uint32 timestamp wrap
	tsNext := uint32(3600)
	if pkts, err = conv.Process(makeRTP(1, tsNext, 1, 96, false, []byte{0x63, 0xe2, 0xb0, 0xaa})); err != nil || len(pkts) != 0 {
		t.Fatalf("P start should buffer until end, got %d %v", len(pkts), err)
	}
	pkts, err = conv.Process(makeRTP(2, tsNext, 1, 96, true, []byte{0x63, 0xe2, 0x70, 0xbb}))
	if err != nil || len(pkts) != 2 {
		t.Fatalf("expected 2 P-frame fragments, got %d %v", len(pkts), err)
	}
	if pkts[0].Timestamp != tsNext || pkts[1].Timestamp != tsNext || !pkts[1].Marker {
		t.Fatal("P-frame timestamp or marker mismatch")
	}

	// Mid-frame packet loss: seq jumps 2 -> 4 while buffering -> nothing emitted, drops ready
	if pkts, err = conv.Process(makeRTP(3, tsNext+3600, 1, 96, false, []byte{0x63, 0xe2, 0xb0, 0x11})); err != nil || len(pkts) != 0 {
		t.Fatal("unexpected emit on start")
	}
	if pkts, err = conv.Process(makeRTP(5, tsNext+3600, 1, 96, true, []byte{0x63, 0xe2, 0x70, 0x22})); err != nil || len(pkts) != 0 {
		t.Fatal("broken fragment chain emitted")
	}
	// Subsequent P-frame suppressed until next IDR
	if pkts, err = conv.Process(makeRTP(6, tsNext+7200, 1, 96, false, []byte{0x63, 0xe2, 0xb0, 0x33})); err != nil || len(pkts) != 0 {
		t.Fatal("post-loss P start emitted")
	}
	if pkts, err = conv.Process(makeRTP(7, tsNext+7200, 1, 96, true, []byte{0x63, 0xe2, 0x70, 0x44})); err != nil || len(pkts) != 0 {
		t.Fatal("post-loss P end emitted")
	}

	// Next IDR recovers cleanly with contiguous output seq
	prevSeq := uint16(5) // last emitted was pkts[1] of first P-frame (65535..4)
	_ = prevSeq
	if pkts, err = conv.Process(makeRTP(8, tsNext+10800, 1, 96, false, []byte{0x63, 0x88, 0xb2, 0x80, 0x21})); err != nil || len(pkts) != 0 {
		t.Fatal("recovery IDR start emitted prematurely")
	}
	pkts, err = conv.Process(makeRTP(9, tsNext+10800, 1, 96, true, []byte{0x63, 0x88, 0x72, 0x55}))
	if err != nil || len(pkts) != 4 {
		t.Fatalf("recovery IDR failed: %d %v", len(pkts), err)
	}

	// Runtime genuine HEVC packet triggers ErrCodecMismatch
	hevcSPS := mustHex(t, "420101016000000300b00000030000030078a003c0801107cb96")
	if _, err := conv.Process(makeRTP(10, tsNext+14400, 1, 96, true, hevcSPS)); err != ErrCodecMismatch {
		t.Fatalf("expected ErrCodecMismatch on runtime HEVC, got %v", err)
	}

	// Concurrent independent converters under -race
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(ssrc uint32) {
			defer wg.Done()
			var localStats Stats
			c := NewConverter(ev, &localStats)
			_, _ = c.Process(makeRTP(1, 100, ssrc, 96, false, []byte{0x63, 0x88, 0xb2, 0x80, 0x21}))
			out, err := c.Process(makeRTP(2, 100, ssrc, 96, true, []byte{0x63, 0x88, 0x72, 0xaa}))
			if err != nil || len(out) != 4 {
				t.Errorf("concurrent converter failed: len=%d err=%v", len(out), err)
			}
		}(uint32(100 + g))
	}
	wg.Wait()
}

func TestByteExactFFmpegH264Recovery(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}

	raw, err := exec.Command(
		"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", "2", "-c:v", "libx264", "-profile:v", "baseline", "-threads", "1",
		"-pix_fmt", "yuv420p", "-x264-params", "keyint=25:min-keyint=25:scenecut=0:repeat-headers=1",
		"-f", "h264", "pipe:1",
	).Output()
	if err != nil {
		t.Fatalf("ffmpeg generate fixture failed: %v", err)
	}

	nals := splitAnnexB(raw)
	if len(nals) < 50 {
		t.Fatalf("unexpectedly few NALs: %d", len(nals))
	}

	// Build malformed wire RTP stream matching observed HANET behavior
	var seq uint16 = 65520
	var ts uint32 = 0xFFFFF000
	var pkts []*rtp.Packet
	var expectedNALs [][]byte
	seenFirstIDR := false
	for _, nal := range nals {
		typ := nal[0] & 0x1f
		switch typ {
		case 7, 8:
			pkts = append(pkts, makeRTP(seq, ts, 0x11223344, 96, false, nal))
			seq++
			expectedNALs = append(expectedNALs, append([]byte(nil), nal...))
		case 6, 9:
			pkts = append(pkts, makeRTP(seq, ts, 0x11223344, 96, false, nal))
			seq++
			if seenFirstIDR {
				expectedNALs = append(expectedNALs, append([]byte(nil), nal...))
			}
		case 1, 5:
			if len(nal) < 4 {
				continue
			}
			if typ == 5 {
				seenFirstIDR = true
			}
			// Preserve original NRI=3 (0x60|typ) just like HANET 0x61/0x65
			norm := append([]byte(nil), nal...)
			norm[0] = 0x60 | typ
			if seenFirstIDR {
				expectedNALs = append(expectedNALs, norm)
			}

			const chunk = 500
			body := norm[2:]
			var parts [][]byte
			for off := 0; off < len(body); off += chunk {
				end := off + chunk
				if end > len(body) {
					end = len(body)
				}
				parts = append(parts, body[off:end])
			}
			if len(parts) < 2 {
				mid := len(body) / 2
				parts = [][]byte{body[:mid], body[mid:]}
			}
			for i, part := range parts {
				var flags byte
				if i == 0 {
					flags |= 0x80
				}
				if i == len(parts)-1 {
					flags |= 0x40
				}
				wire := make([]byte, 0, 3+len(part))
				wire = append(wire, 0x63, norm[1], flags|((norm[0]>>1)&0x3f))
				wire = append(wire, part...)
				pkts = append(pkts, makeRTP(seq, ts, 0x11223344, 96, i == len(parts)-1, wire))
				seq++
			}
			ts += 3600
		}
	}

	var cl Classifier
	for _, p := range pkts {
		if cl.ObservePacket(p) == ClassConfirmedHANET {
			break
		}
	}
	if cl.State() != ClassConfirmedHANET {
		t.Fatalf("classifier did not confirm HANET, state=%v", cl.State())
	}

	var stats Stats
	conv := NewConverter(cl.Evidence(), &stats)
	var recoveredNALs [][]byte
	var fuBuf []byte
	for _, p := range pkts {
		outs, err := conv.Process(p)
		if err != nil {
			t.Fatalf("Process failed: %v", err)
		}
		for _, out := range outs {
			pl := out.Payload
			if pl[0]&0x1f == 28 {
				if pl[1]&0x80 != 0 {
					fuBuf = []byte{(pl[0] & 0xe0) | (pl[1] & 0x1f)}
				}
				fuBuf = append(fuBuf, pl[2:]...)
				if pl[1]&0x40 != 0 {
					recoveredNALs = append(recoveredNALs, fuBuf)
					fuBuf = nil
				}
			} else {
				recoveredNALs = append(recoveredNALs, append([]byte(nil), pl...))
			}
		}
	}

	// Drop pre-IDR SEI/AUD from expectedNALs (before first IDR) since Converter starts cleanly at SPS+PPS+IDR.
	firstIDR := -1
	for i, nal := range expectedNALs {
		if nal[0]&0x1f == 5 {
			firstIDR = i
			break
		}
	}
	if firstIDR < 0 {
		t.Fatal("no IDR in fixture")
	}
	var wantNALs [][]byte
	for _, nal := range expectedNALs {
		if len(wantNALs) == 0 && (nal[0]&0x1f == 6 || nal[0]&0x1f == 9) {
			continue
		}
		typ := nal[0] & 0x1f
		if typ == 7 || typ == 8 {
			wantNALs = append(wantNALs, nal)
			continue
		}
		if typ == 5 {
			wantNALs = append(wantNALs, expectedNALs[firstIDR:]...)
			break
		}
	}
	if len(recoveredNALs) != len(wantNALs) {
		t.Fatalf("NAL count mismatch: got %d want %d", len(recoveredNALs), len(wantNALs))
	}
	for i, want := range wantNALs {
		got := recoveredNALs[i]
		if !bytes.Equal(got, want) {
			t.Fatalf("NAL %d byte mismatch: got len=%d head=%x, want len=%d head=%x",
				i, len(got), got[:min(8, len(got))], len(want), want[:min(8, len(want))])
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"rtsp://192.0.2.105:554/user:hanet;pwd:Secret123#hanet=1", "rtsp://192.0.2.105:554/user:hanet;pwd:****#hanet=1"},
		{"rtsp://admin:MyPass@192.0.2.105:554/stream?token=abc123", "rtsp://admin:****@192.0.2.105:554/stream?token=****"},
	}
	for _, tc := range cases {
		got := RedactURL(tc.in)
		if got != tc.want {
			t.Fatalf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i < len(data); i++ {
		scLen := 0
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			scLen = 4
		} else if i+2 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			scLen = 3
		}
		if scLen > 0 {
			if start >= 0 && start < i {
				nals = append(nals, data[start:i])
			}
			i += scLen - 1
			start = i + 1
		}
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

func FuzzPayload(f *testing.F) {
	for _, seed := range []string{"63e2b0aabb", "63e230aabb", "63e270aabb", "63b8b2aabb", "67420020abcd", "620193aabb", ""} {
		if b, err := hex.DecodeString(seed); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out, typ, start, end, ok := Payload(data)
		if ok {
			if len(out) == 0 || (start && end && typ == 28) {
				t.Fatalf("invalid output for %x: out=%x typ=%d s=%v e=%v", data, out, typ, start, end)
			}
		}
	})
}

func FuzzInspectSDP(f *testing.F) {
	f.Add("v=0\r\ns=rtsp_demo\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\n")
	f.Add("v=0\ns=normal\nm=video 0 RTP/AVP 96\na=rtpmap:96 H264/90000\n")
	f.Fuzz(func(t *testing.T, sdp string) {
		_, _, _ = InspectSDP(sdp)
	})
}

func FuzzParseH264SPS(f *testing.F) {
	if b, err := base64.StdEncoding.DecodeString("Z0IAIJY1QKALtNwEBAQI"); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		info, ok := ParseH264SPS(data)
		if ok && (info.Width < 16 || info.Height < 16) {
			t.Fatalf("invalid dimensions %+v", info)
		}
	})
}
