package rtsp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/hanetfix"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type mediaFixtures struct {
	h264NALs  [][]byte
	h264SPS   []byte
	h264PPS   []byte
	hevcNALs  [][]byte
	hevcVPS   []byte
	hevcSPS   []byte
	hevcPPS   []byte
	aacFrames [][]byte // raw AAC frames (ADTS header stripped)
}

var (
	cachedFixturesOnce sync.Once
	cachedFixtures     *mediaFixtures
	cachedFixturesErr  error
)

func getMediaFixtures(t *testing.T) *mediaFixtures {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	cachedFixturesOnce.Do(func() {
		f := &mediaFixtures{}

		h264Raw, err := exec.Command(
			"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
			"-t", "6", "-c:v", "libx264", "-profile:v", "baseline", "-threads", "1",
			"-pix_fmt", "yuv420p", "-x264-params", "keyint=25:min-keyint=25:scenecut=0:repeat-headers=1",
			"-f", "h264", "pipe:1",
		).Output()
		if err != nil {
			cachedFixturesErr = fmt.Errorf("ffmpeg h264 fixture: %w", err)
			return
		}
		f.h264NALs = splitAnnexBNALs(h264Raw)
		for _, nal := range f.h264NALs {
			switch nal[0] & 0x1f {
			case 7:
				if f.h264SPS == nil {
					f.h264SPS = nal
				}
			case 8:
				if f.h264PPS == nil {
					f.h264PPS = nal
				}
			}
		}

		hevcRaw, err := exec.Command(
			"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
			"-t", "6", "-c:v", "libx265", "-threads", "1", "-pix_fmt", "yuv420p",
			"-x265-params", "keyint=25:min-keyint=25:scenecut=0:repeat-headers=1:bframes=0",
			"-f", "hevc", "pipe:1",
		).Output()
		if err != nil {
			cachedFixturesErr = fmt.Errorf("ffmpeg hevc fixture: %w", err)
			return
		}
		f.hevcNALs = splitAnnexBNALs(hevcRaw)
		for _, nal := range f.hevcNALs {
			switch (nal[0] >> 1) & 0x3f {
			case 32:
				if f.hevcVPS == nil {
					f.hevcVPS = nal
				}
			case 33:
				if f.hevcSPS == nil {
					f.hevcSPS = nal
				}
			case 34:
				if f.hevcPPS == nil {
					f.hevcPPS = nal
				}
			}
		}

		aacRaw, err := exec.Command(
			"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", "6", "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-f", "adts", "pipe:1",
		).Output()
		if err != nil {
			cachedFixturesErr = fmt.Errorf("ffmpeg aac fixture: %w", err)
			return
		}
		f.aacFrames = parseADTSFrames(aacRaw)
		cachedFixtures = f
	})

	if cachedFixturesErr != nil {
		t.Fatal(cachedFixturesErr)
	}
	return cachedFixtures
}

func splitAnnexBNALs(data []byte) [][]byte {
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

func parseADTSFrames(adts []byte) [][]byte {
	var frames [][]byte
	for i := 0; i+7 <= len(adts); {
		if adts[i] != 0xff || (adts[i+1]&0xf6) != 0xf0 {
			i++
			continue
		}
		hdrLen := 7
		if adts[i+1]&1 == 0 {
			hdrLen = 9
		}
		frameLen := int(adts[i+3]&0x03)<<11 | int(adts[i+4])<<3 | int(adts[i+5]>>5)
		if frameLen <= hdrLen || i+frameLen > len(adts) {
			break
		}
		frames = append(frames, append([]byte(nil), adts[i+hdrLen:i+frameLen]...))
		i += frameLen
	}
	return frames
}

func wireH264NAL(nal []byte, broken bool) [][]byte {
	typ := nal[0] & 0x1f
	if typ != 1 && typ != 5 {
		return [][]byte{append([]byte(nil), nal...)}
	}
	norm := append([]byte(nil), nal...)
	norm[0] = 0x60 | typ
	if len(norm) < 4 {
		return [][]byte{norm}
	}
	startIdx := 1
	if broken {
		startIdx = 2
	}
	body := norm[startIdx:]
	const step = 700
	var chunks [][]byte
	for off := 0; off < len(body); off += step {
		end := off + step
		if end > len(body) {
			end = len(body)
		}
		chunks = append(chunks, body[off:end])
	}
	if len(chunks) < 2 {
		mid := len(body) / 2
		chunks = [][]byte{body[:mid], body[mid:]}
	}
	out := make([][]byte, 0, len(chunks))
	for i, chunk := range chunks {
		var flags byte
		if i == 0 {
			flags |= 0x80
		}
		if i == len(chunks)-1 {
			flags |= 0x40
		}
		if broken {
			pkt := make([]byte, 0, 3+len(chunk))
			pkt = append(pkt, 0x63, norm[1], flags|((norm[0]>>1)&0x3f))
			pkt = append(pkt, chunk...)
			out = append(out, pkt)
		} else {
			pkt := make([]byte, 0, 2+len(chunk))
			pkt = append(pkt, 0x7c, flags|typ)
			pkt = append(pkt, chunk...)
			out = append(out, pkt)
		}
	}
	return out
}

func wireHEVCNAL(nal []byte) [][]byte {
	const maxBody = 700
	if len(nal) <= maxBody+2 {
		return [][]byte{append([]byte(nil), nal...)}
	}
	nut := (nal[0] >> 1) & 0x3f
	body := nal[2:]
	var chunks [][]byte
	for off := 0; off < len(body); off += maxBody {
		end := off + maxBody
		if end > len(body) {
			end = len(body)
		}
		chunks = append(chunks, body[off:end])
	}
	out := make([][]byte, 0, len(chunks))
	for i, chunk := range chunks {
		var fu byte = nut
		if i == 0 {
			fu |= 0x80
		}
		if i == len(chunks)-1 {
			fu |= 0x40
		}
		pkt := make([]byte, 0, 3+len(chunk))
		pkt = append(pkt, (nal[0]&0x81)|(49<<1), nal[1], fu)
		pkt = append(pkt, chunk...)
		out = append(out, pkt)
	}
	return out
}

func makeAACRTPPayload(rawFrame []byte) []byte {
	auSize := uint16(len(rawFrame))
	p := make([]byte, 4+len(rawFrame))
	p[0] = 0
	p[1] = 16
	binary.BigEndian.PutUint16(p[2:], auSize<<3)
	copy(p[4:], rawFrame)
	return p
}

type hanetMockServer struct {
	addr        string
	ln          net.Listener
	fix         *mediaFixtures
	conns       atomic.Int32
	describeNum atomic.Int32
	playNum     atomic.Int32
	activeConns atomic.Int32
	maxActive   atomic.Int32
	dropOnce    atomic.Bool
	dropAtFrame int
	startSeq    uint16
	startTS     uint32
	framePace   time.Duration
}

func startHanetMockServer(t *testing.T, fix *mediaFixtures) *hanetMockServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := &hanetMockServer{
		addr:      ln.Addr().String(),
		ln:        ln,
		fix:       fix,
		startSeq:  65500,
		startTS:   0xFFFF0000,
		framePace: 2 * time.Millisecond,
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go s.serveConn(conn)
		}
	}()
	return s
}

func (s *hanetMockServer) serveConn(conn net.Conn) {
	cur := s.activeConns.Add(1)
	for {
		prev := s.maxActive.Load()
		if cur <= prev || s.maxActive.CompareAndSwap(prev, cur) {
			break
		}
	}
	defer func() {
		s.activeConns.Add(-1)
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	rd := bufio.NewReader(conn)
	var writeMu sync.Mutex
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stopPump := func() { stopOnce.Do(func() { close(stopCh) }) }
	defer stopPump()

	var route string
	var descCount int32
	var videoPT uint8 = 96
	var videoCh byte = 0
	var audioCh byte = 2
	hasAudio := false
	isHEVC := false
	isBroken := false

	sendResp := func(cseq, extra, body string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		var hdr strings.Builder
		hdr.WriteString("RTSP/1.0 200 OK\r\nCSeq: " + cseq + "\r\n")
		hdr.WriteString(extra)
		if body != "" {
			hdr.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
		}
		hdr.WriteString("\r\n")
		if _, err := conn.Write([]byte(hdr.String() + body)); err != nil {
			return err
		}
		return nil
	}

	sendRTP := func(ch byte, pkt *rtp.Packet) error {
		b, err := pkt.Marshal()
		if err != nil {
			return err
		}
		frame := make([]byte, 4+len(b))
		frame[0] = '$'
		frame[1] = ch
		binary.BigEndian.PutUint16(frame[2:], uint16(len(b)))
		copy(frame[4:], b)
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = conn.Write(frame)
		return err
	}

	pump := func(playCount int32) {
		seqV := s.startSeq
		tsV := s.startTS
		seqA := uint16(100)
		tsA := uint32(1000)
		audioIdx := 0
		framesSent := 0
		dropPacketInjected := false

		for loop := 0; loop < 3; loop++ {
			select {
			case <-stopCh:
				return
			default:
			}

			switch {
			case route == "/ambiguous":
				for i := 0; i < 40; i++ {
					select {
					case <-stopCh:
						return
					default:
					}
					pkt := &rtp.Packet{
						Header: rtp.Header{
							Version: 2, Marker: true, PayloadType: videoPT,
							SequenceNumber: seqV, Timestamp: tsV, SSRC: 0x22334455,
						},
						Payload: []byte{0x7e, 0x01, 0xaa, 0xbb, 0xcc},
					}
					seqV++
					tsV += 3600
					if sendRTP(videoCh, pkt) != nil {
						return
					}
					time.Sleep(s.framePace)
				}
				return

			case route == "/switch_active" && playCount >= 2:
				// Probe saw broken HANET; active PLAY switches to genuine HEVC
				for _, nal := range s.fix.hevcNALs {
					select {
					case <-stopCh:
						return
					default:
					}
					nut := (nal[0] >> 1) & 0x3f
					isVCL := nut <= 1 || (nut >= 16 && nut <= 21)
					for _, pl := range wireHEVCNAL(nal) {
						pkt := &rtp.Packet{
							Header: rtp.Header{
								Version: 2, Marker: isVCL, PayloadType: videoPT,
								SequenceNumber: seqV, Timestamp: tsV, SSRC: 0x33445566,
							},
							Payload: pl,
						}
						seqV++
						if sendRTP(videoCh, pkt) != nil {
							return
						}
					}
					if isVCL {
						tsV += 3600
						time.Sleep(s.framePace)
					}
				}
				return

			case isHEVC:
				for _, nal := range s.fix.hevcNALs {
					select {
					case <-stopCh:
						return
					default:
					}
					nut := (nal[0] >> 1) & 0x3f
					isVCL := nut <= 1 || (nut >= 16 && nut <= 21)
					parts := wireHEVCNAL(nal)
					for i, pl := range parts {
						pkt := &rtp.Packet{
							Header: rtp.Header{
								Version: 2, Marker: isVCL && i == len(parts)-1, PayloadType: videoPT,
								SequenceNumber: seqV, Timestamp: tsV, SSRC: 0x44556677,
							},
							Payload: pl,
						}
						seqV++
						if sendRTP(videoCh, pkt) != nil {
							return
						}
					}
					if isVCL {
						tsV += 3600
						if hasAudio && len(s.fix.aacFrames) > 0 {
							af := s.fix.aacFrames[audioIdx%len(s.fix.aacFrames)]
							audioIdx++
							apkt := &rtp.Packet{
								Header: rtp.Header{
									Version: 2, Marker: true, PayloadType: 97,
									SequenceNumber: seqA, Timestamp: tsA, SSRC: 0x77665544,
								},
								Payload: makeAACRTPPayload(af),
							}
							seqA++
							tsA += 1024
							if sendRTP(audioCh, apkt) != nil {
								return
							}
						}
						time.Sleep(s.framePace)
					}
				}

			default:
				for _, nal := range s.fix.h264NALs {
					select {
					case <-stopCh:
						return
					default:
					}
					typ := nal[0] & 0x1f
					if typ != 1 && typ != 5 && typ != 6 && typ != 7 && typ != 8 && typ != 9 {
						continue
					}
					parts := wireH264NAL(nal, isBroken)
					for i, pl := range parts {
						if route == "/drop_loss" && playCount >= 2 && !dropPacketInjected && framesSent == 30 && typ == 1 && i == len(parts)-1 {
							dropPacketInjected = true
							seqV++ // simulate dropped packet in middle of P-frame
							continue
						}
						pkt := &rtp.Packet{
							Header: rtp.Header{
								Version: 2, Marker: (typ == 1 || typ == 5) && i == len(parts)-1, PayloadType: videoPT,
								SequenceNumber: seqV, Timestamp: tsV, SSRC: 0x11223344,
							},
							Payload: pl,
						}
						seqV++
						if sendRTP(videoCh, pkt) != nil {
							return
						}
					}
					if typ == 1 || typ == 5 {
						framesSent++
						if s.dropAtFrame > 0 && framesSent == s.dropAtFrame && s.dropOnce.CompareAndSwap(true, false) {
							_ = conn.Close()
							return
						}
						tsV += 3600
						if hasAudio && len(s.fix.aacFrames) > 0 {
							for k := 0; k < 2; k++ {
								af := s.fix.aacFrames[audioIdx%len(s.fix.aacFrames)]
								audioIdx++
								apkt := &rtp.Packet{
									Header: rtp.Header{
										Version: 2, Marker: true, PayloadType: 97,
										SequenceNumber: seqA, Timestamp: tsA, SSRC: 0x55667788,
									},
									Payload: makeAACRTPPayload(af),
								}
								seqA++
								tsA += 1024
								if sendRTP(audioCh, apkt) != nil {
									return
								}
							}
						}
						time.Sleep(s.framePace)
					}
				}
			}
		}
	}

	for {
		req, err := tcp.ReadRequest(rd)
		if err != nil {
			return
		}
		cseq := req.Header.Get("CSeq")
		switch req.Method {
		case MethodOptions:
			if sendResp(cseq, "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n", "") != nil {
				return
			}
		case MethodDescribe:
			descCount = s.describeNum.Add(1)
			path := req.URL.Path
			switch {
			case strings.Contains(path, "/broken_pt98"):
				route = "/broken_pt98"
				videoPT = 98
				isBroken = true
			case strings.Contains(path, "/broken"):
				route = "/broken"
				isBroken = true
			case strings.Contains(path, "/drop_loss"):
				route = "/drop_loss"
				isBroken = true
			case strings.Contains(path, "/hevc_fmtp"):
				route = "/hevc_fmtp"
				isHEVC = true
			case strings.Contains(path, "/hevc_nofmtp"):
				route = "/hevc_nofmtp"
				isHEVC = true
			case strings.Contains(path, "/ambiguous"):
				route = "/ambiguous"
			case strings.Contains(path, "/switch_sdp"):
				route = "/switch_sdp"
				isBroken = true
			case strings.Contains(path, "/switch_active"):
				route = "/switch_active"
				isBroken = true
			case strings.Contains(path, "/switch_reconnect"):
				route = "/switch_reconnect"
				isBroken = true
			case strings.Contains(path, "/multi_video"):
				route = "/multi_video"
			default:
				route = "/good"
			}

			var sdpBody string
			ptStr := strconv.Itoa(int(videoPT))
			base := "rtsp://" + s.addr + path
			switch route {
			case "/multi_video":
				sdpBody = "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\n" +
					"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:track1\r\n" +
					"m=video 0 RTP/AVP 98\r\na=rtpmap:98 H265/90000\r\na=control:track2\r\n"
			case "/switch_sdp":
				if descCount >= 2 {
					sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;sprop-parameter-sets=%s,%s\r\na=control:%s/track1\r\n",
						base64.StdEncoding.EncodeToString(s.fix.h264SPS),
						base64.StdEncoding.EncodeToString(s.fix.h264PPS), base)
				} else {
					sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:%s/track1\r\n", base)
				}
			case "/switch_reconnect":
				if descCount >= 3 {
					// On reconnect, camera claims normal H265 on non-rtsp_demo SDP -> must fail codec switch!
					sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=changed\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:%s/track1\r\n", base)
				} else {
					sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:%s/track1\r\n", base)
				}
			case "/hevc_fmtp":
				fmtp := fmt.Sprintf("a=fmtp:%s sprop-vps=%s;sprop-sps=%s;sprop-pps=%s\r\n",
					ptStr,
					base64.StdEncoding.EncodeToString(s.fix.hevcVPS),
					base64.StdEncoding.EncodeToString(s.fix.hevcSPS),
					base64.StdEncoding.EncodeToString(s.fix.hevcPPS))
				sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP %s\r\na=rtpmap:%s H265/90000\r\n%sa=control:%s/track1\r\n"+
					"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/1\r\na=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188\r\na=control:%s/track2\r\n",
					ptStr, ptStr, fmtp, base, base)
			case "/good":
				fmtp := fmt.Sprintf("a=fmtp:%s packetization-mode=1;sprop-parameter-sets=%s,%s\r\n",
					ptStr,
					base64.StdEncoding.EncodeToString(s.fix.h264SPS),
					base64.StdEncoding.EncodeToString(s.fix.h264PPS))
				sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP %s\r\na=rtpmap:%s H264/90000\r\n%sa=control:%s/track1\r\n"+
					"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/1\r\na=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188\r\na=control:%s/track2\r\n",
					ptStr, ptStr, fmtp, base, base)
			default:
				sdpBody = fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\nm=video 0 RTP/AVP %s\r\na=rtpmap:%s H265/90000\r\na=control:%s/track1\r\n"+
					"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/1\r\na=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188\r\na=control:%s/track2\r\n",
					ptStr, ptStr, base, base)
			}
			if sendResp(cseq, "Content-Type: application/sdp\r\n", sdpBody) != nil {
				return
			}
		case MethodSetup:
			tr := req.Header.Get("Transport")
			chStr := core.Between(tr, "interleaved=", "-")
			ch, _ := strconv.Atoi(chStr)
			if strings.Contains(req.URL.Path, "track2") {
				audioCh = byte(ch)
				hasAudio = true
			} else {
				videoCh = byte(ch)
			}
			extra := fmt.Sprintf("Session: test123;timeout=60\r\nTransport: RTP/AVP/TCP;unicast;interleaved=%d-%d\r\n", ch, ch+1)
			if sendResp(cseq, extra, "") != nil {
				return
			}
		case MethodPlay:
			pc := s.playNum.Add(1)
			if sendResp(cseq, "Session: test123\r\n", "") != nil {
				return
			}
			go pump(pc)
		case MethodTeardown:
			_ = sendResp(cseq, "Session: test123\r\n", "")
			return
		default:
			if sendResp(cseq, "", "") != nil {
				return
			}
		}
	}
}

func strictDecodeAnnexB(t *testing.T, codec string, annexB []byte, minFrames int) int {
	t.Helper()
	cmd := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
		"-threads", "1", "-f", codec, "-i", "pipe:0",
		"-map", "0:v:0", "-an", "-progress", "pipe:1", "-nostats", "-f", "null", "-",
	)
	cmd.Stdin = bytes.NewReader(annexB)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.NoError(t, err, "ffmpeg strict decode failed: stderr=%s", stderr.String())
	require.Empty(t, strings.TrimSpace(stderr.String()), "ffmpeg stderr not empty")
	re := regexp.MustCompile(`(?m)^frame=(\d+)\s*$`)
	matches := re.FindAllStringSubmatch(stdout.String(), -1)
	frames := 0
	for _, m := range matches {
		if n, _ := strconv.Atoi(m[1]); n > frames {
			frames = n
		}
	}
	require.GreaterOrEqual(t, frames, minFrames, "decoded too few frames: %d", frames)
	return frames
}

func rtpToAnnexB(pkts []*rtp.Packet, isHEVC bool) []byte {
	var out bytes.Buffer
	startCode := []byte{0, 0, 0, 1}
	var fuBuf []byte
	for _, pkt := range pkts {
		p := pkt.Payload
		if len(p) == 0 {
			continue
		}
		if !isHEVC {
			if p[0]&0x1f == 28 && len(p) >= 2 {
				if p[1]&0x80 != 0 {
					fuBuf = []byte{(p[0] & 0xe0) | (p[1] & 0x1f)}
				}
				fuBuf = append(fuBuf, p[2:]...)
				if p[1]&0x40 != 0 {
					out.Write(startCode)
					out.Write(fuBuf)
					fuBuf = nil
				}
			} else {
				out.Write(startCode)
				out.Write(p)
			}
		} else {
			nut := (p[0] >> 1) & 0x3f
			if nut == 49 && len(p) >= 3 {
				if p[2]&0x80 != 0 {
					fuBuf = []byte{(p[0] & 0x81) | ((p[2] & 0x3f) << 1), p[1]}
				}
				fuBuf = append(fuBuf, p[3:]...)
				if p[2]&0x40 != 0 {
					out.Write(startCode)
					out.Write(fuBuf)
					fuBuf = nil
				}
			} else {
				out.Write(startCode)
				out.Write(p)
			}
		}
	}
	return out.Bytes()
}

func collectClientPackets(t *testing.T, uri string, hanetFix bool, targetVideo int) (*Conn, []*rtp.Packet, []*rtp.Packet) {
	t.Helper()
	client := NewClient(uri)
	client.HanetFix = hanetFix
	client.HanetProbeTimeout = 5 * time.Second
	require.NoError(t, client.Dial())
	require.NoError(t, client.Describe())

	var videoTrack, audioTrack *core.Receiver
	for _, m := range client.Medias {
		if m.Kind == core.KindVideo && videoTrack == nil {
			var err error
			videoTrack, err = client.SetupMediaTrack(m, m.Codecs[0])
			require.NoError(t, err)
		} else if m.Kind == core.KindAudio && audioTrack == nil {
			var err error
			audioTrack, err = client.SetupMediaTrack(m, m.Codecs[0])
			require.NoError(t, err)
		}
	}
	require.NotNil(t, videoTrack)

	var mu sync.Mutex
	var videoPkts, audioPkts []*rtp.Packet
	done := make(chan struct{})
	var doneOnce sync.Once

	vSender := core.NewSender(videoTrack.Media, videoTrack.Codec)
	vSender.Handler = func(p *rtp.Packet) {
		mu.Lock()
		videoPkts = append(videoPkts, p)
		n := len(videoPkts)
		mu.Unlock()
		if n >= targetVideo && p.Marker {
			doneOnce.Do(func() { close(done) })
		}
	}
	vSender.HandleRTP(videoTrack)

	var aSender *core.Sender
	if audioTrack != nil {
		aSender = core.NewSender(audioTrack.Media, audioTrack.Codec)
		aSender.Handler = func(p *rtp.Packet) {
			mu.Lock()
			audioPkts = append(audioPkts, p)
			mu.Unlock()
		}
		aSender.HandleRTP(audioTrack)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Start()
	}()

	select {
	case <-done:
	case err := <-errCh:
		if err != nil {
			t.Fatalf("client.Start exited early: %v", err)
		}
	case <-time.After(15 * time.Second):
		mu.Lock()
		got := len(videoPkts)
		mu.Unlock()
		t.Fatalf("timeout collecting packets (got %d)", got)
	}

	_ = client.Close()
	<-errCh
	vSender.Close()
	vSender.Wait()
	if aSender != nil {
		aSender.Close()
		aSender.Wait()
	}
	_ = client.Stop()

	mu.Lock()
	vOut := append([]*rtp.Packet(nil), videoPkts...)
	aOut := append([]*rtp.Packet(nil), audioPkts...)
	mu.Unlock()
	return client, vOut, aOut
}

func (c *Conn) SetupMediaTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return c.GetTrack(media, codec)
}

func TestHanetIntegrationBrokenOptInAndDynamicPT(t *testing.T) {
	fix := getMediaFixtures(t)
	for _, tc := range []struct {
		name string
		path string
		pt   uint8
	}{
		{"pt96", "/user:hanet;pwd:Secret123/broken", 96},
		{"pt98", "/user:hanet;pwd:Secret123/broken_pt98", 98},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startHanetMockServer(t, fix)
			client, vPkts, aPkts := collectClientPackets(t, "rtsp://"+srv.addr+tc.path, true, 500)

			require.Equal(t, core.CodecH264, client.Medias[0].Codecs[0].Name)
			require.Equal(t, tc.pt, client.Medias[0].Codecs[0].PayloadType)
			require.Contains(t, client.Medias[0].Codecs[0].FmtpLine, "sprop-parameter-sets="+base64.StdEncoding.EncodeToString(fix.h264SPS)+","+base64.StdEncoding.EncodeToString(fix.h264PPS))
			require.Equal(t, uint64(1), client.HanetStats().ConfirmedHANET)
			require.NotEmpty(t, aPkts, "AAC audio packets must be preserved")
			require.Greater(t, len(vPkts), 200)

			// Verify strictly contiguous output sequence numbers across uint16 wrap
			for i := 1; i < len(vPkts); i++ {
				require.Equal(t, uint16(vPkts[i-1].SequenceNumber+1), vPkts[i].SequenceNumber)
			}

			annexB := rtpToAnnexB(vPkts, false)
			frames := strictDecodeAnnexB(t, "h264", annexB, 100)
			t.Logf("%s decoded %d strict H264 frames, audio pkts=%d", tc.name, frames, len(aPkts))
		})
	}
}

func TestHanetIntegrationNoOptInAndPassthrough(t *testing.T) {
	fix := getMediaFixtures(t)

	t.Run("broken_no_opt_in", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/broken")
		c.HanetFix = false
		require.NoError(t, c.Dial())
		defer c.Close()
		require.NoError(t, c.Describe())
		require.Equal(t, core.CodecH265, c.Medias[0].Codecs[0].Name)
		require.Equal(t, int32(1), srv.conns.Load())
	})

	t.Run("normal_h264_opt_in", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c, vPkts, aPkts := collectClientPackets(t, "rtsp://"+srv.addr+"/good", true, 300)
		require.Equal(t, core.CodecH264, c.Medias[0].Codecs[0].Name)
		require.Equal(t, int32(1), srv.conns.Load(), "normal H264 must not open a probe session")
		require.Equal(t, uint64(1), c.HanetStats().Passthrough)
		require.NotEmpty(t, aPkts)
		strictDecodeAnnexB(t, "h264", rtpToAnnexB(vPkts, false), 100)
	})

	t.Run("genuine_hevc_with_fmtp", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c, vPkts, aPkts := collectClientPackets(t, "rtsp://"+srv.addr+"/hevc_fmtp", true, 300)
		require.Equal(t, core.CodecH265, c.Medias[0].Codecs[0].Name)
		require.Equal(t, int32(1), srv.conns.Load(), "HEVC with fmtp must not probe")
		require.Equal(t, uint64(1), c.HanetStats().Passthrough)
		require.NotEmpty(t, aPkts)
		strictDecodeAnnexB(t, "hevc", rtpToAnnexB(vPkts, true), 100)
	})

	t.Run("genuine_hevc_without_fmtp", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c, vPkts, aPkts := collectClientPackets(t, "rtsp://"+srv.addr+"/hevc_nofmtp", true, 300)
		require.Equal(t, core.CodecH265, c.Medias[0].Codecs[0].Name)
		require.Equal(t, uint64(1), c.HanetStats().ConfirmedHEVC)
		require.Equal(t, uint64(1), c.HanetStats().Passthrough)
		require.NotEmpty(t, aPkts)
		strictDecodeAnnexB(t, "hevc", rtpToAnnexB(vPkts, true), 100)
	})
}

func TestHanetIntegrationNegativeAndGuards(t *testing.T) {
	fix := getMediaFixtures(t)

	t.Run("ambiguous_h265_fails_closed", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/user:hanet;pwd:Secret999/ambiguous")
		c.HanetFix = true
		c.HanetProbeTimeout = 400 * time.Millisecond
		require.NoError(t, c.Dial())
		defer c.Close()
		err := c.Describe()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "Secret999")
	})

	t.Run("multi_video_ambiguous", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/multi_video")
		c.HanetFix = true
		require.NoError(t, c.Dial())
		defer c.Close()
		err := c.Describe()
		require.ErrorIs(t, err, hanetfix.ErrAmbiguousSDP)
	})

	t.Run("unsupported_transport_and_scheme", func(t *testing.T) {
		c := NewClient("rtsp://127.0.0.1:554/stream")
		c.HanetFix = true
		c.Transport = "udp"
		require.Error(t, c.Dial())

		c2 := NewClient("rtsps://127.0.0.1:554/stream")
		c2.HanetFix = true
		require.Error(t, c2.Dial())
	})

	t.Run("stale_probe_sdp_changed", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/switch_sdp")
		c.HanetFix = true
		c.HanetProbeTimeout = 3 * time.Second
		require.NoError(t, c.Dial())
		defer c.Close()
		err := c.Describe()
		require.ErrorContains(t, err, "SDP changed")
	})

	t.Run("active_stream_switches_to_hevc", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/switch_active")
		c.HanetFix = true
		c.HanetProbeTimeout = 3 * time.Second
		require.NoError(t, c.Dial())
		require.NoError(t, c.Describe())
		_, err := c.GetTrack(c.Medias[0], c.Medias[0].Codecs[0])
		require.NoError(t, err)
		err = c.Start()
		require.ErrorIs(t, err, hanetfix.ErrCodecMismatch)
		require.Equal(t, uint64(1), c.HanetStats().CodecMismatches)
		_ = c.Stop()
	})

	t.Run("reconnect_codec_switch_rejected", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c := NewClient("rtsp://" + srv.addr + "/switch_reconnect")
		c.HanetFix = true
		c.HanetProbeTimeout = 3 * time.Second
		require.NoError(t, c.Dial())
		defer c.Close()
		require.NoError(t, c.Describe())
		_, err := c.GetTrack(c.Medias[0], c.Medias[0].Codecs[0])
		require.NoError(t, err)
		err = c.Reconnect()
		require.ErrorContains(t, err, "codec changed")
	})

	t.Run("packet_loss_resyncs_cleanly", func(t *testing.T) {
		srv := startHanetMockServer(t, fix)
		c, vPkts, _ := collectClientPackets(t, "rtsp://"+srv.addr+"/drop_loss", true, 450)
		require.GreaterOrEqual(t, c.HanetStats().Resyncs, uint64(1))
		for i := 1; i < len(vPkts); i++ {
			require.Equal(t, uint16(vPkts[i-1].SequenceNumber+1), vPkts[i].SequenceNumber)
		}
		strictDecodeAnnexB(t, "h264", rtpToAnnexB(vPkts, false), 100)
	})
}
