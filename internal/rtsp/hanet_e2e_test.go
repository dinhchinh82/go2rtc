package rtsp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type e2eFixtures struct {
	h264NALs  [][]byte
	h264SPS   []byte
	h264PPS   []byte
	aacFrames [][]byte
}

var (
	e2eFixOnce sync.Once
	e2eFix     *e2eFixtures
	e2eFixErr  error
)

func getE2EFixtures(t *testing.T) *e2eFixtures {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	e2eFixOnce.Do(func() {
		f := &e2eFixtures{}
		raw, err := exec.Command(
			"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
			"-t", "7", "-c:v", "libx264", "-profile:v", "baseline", "-threads", "1",
			"-pix_fmt", "yuv420p", "-x264-params", "keyint=25:min-keyint=25:scenecut=0:repeat-headers=1",
			"-f", "h264", "pipe:1",
		).Output()
		if err != nil {
			e2eFixErr = err
			return
		}
		f.h264NALs = splitAnnexBE2E(raw)
		for _, nal := range f.h264NALs {
			if nal[0]&0x1f == 7 && f.h264SPS == nil {
				f.h264SPS = nal
			} else if nal[0]&0x1f == 8 && f.h264PPS == nil {
				f.h264PPS = nal
			}
		}

		aacRaw, err := exec.Command(
			"ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", "7", "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-f", "adts", "pipe:1",
		).Output()
		if err != nil {
			e2eFixErr = err
			return
		}
		f.aacFrames = parseADTSE2E(aacRaw)
		e2eFix = f
	})
	if e2eFixErr != nil {
		t.Fatal(e2eFixErr)
	}
	return e2eFix
}

func splitAnnexBE2E(data []byte) [][]byte {
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

func parseADTSE2E(adts []byte) [][]byte {
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

func wireBrokenH264E2E(nal []byte) [][]byte {
	typ := nal[0] & 0x1f
	if typ != 1 && typ != 5 {
		return [][]byte{append([]byte(nil), nal...)}
	}
	norm := append([]byte(nil), nal...)
	norm[0] = 0x60 | typ
	if len(norm) < 4 {
		return [][]byte{norm}
	}
	body := norm[2:]
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
		pkt := make([]byte, 0, 3+len(chunk))
		pkt = append(pkt, 0x63, norm[1], flags|((norm[0]>>1)&0x3f))
		pkt = append(pkt, chunk...)
		out = append(out, pkt)
	}
	return out
}

type e2eMockCamera struct {
	addr        string
	ln          net.Listener
	fix         *e2eFixtures
	activeConns atomic.Int32
	maxActive   atomic.Int32
	totalConns  atomic.Int32
	dropNextAt  atomic.Int32
	seenPaths   sync.Map
}

func startE2EMockCamera(t *testing.T, fix *e2eFixtures) *e2eMockCamera {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	cam := &e2eMockCamera{addr: ln.Addr().String(), ln: ln, fix: fix}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cam.totalConns.Add(1)
			go cam.serve(conn)
		}
	}()
	return cam
}

func (cam *e2eMockCamera) serve(conn net.Conn) {
	cur := cam.activeConns.Add(1)
	for {
		prev := cam.maxActive.Load()
		if cur <= prev || cam.maxActive.CompareAndSwap(prev, cur) {
			break
		}
	}
	defer func() {
		cam.activeConns.Add(-1)
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	rd := bufio.NewReader(conn)
	var writeMu sync.Mutex
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	defer stopOnce.Do(func() { close(stopCh) })

	var videoCh byte = 0
	var audioCh byte = 2
	var hasAudio bool

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
		_, err := conn.Write([]byte(hdr.String() + body))
		return err
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

	pump := func() {
		seqV := uint16(65500)
		tsV := uint32(0xFFFF1000)
		seqA := uint16(100)
		tsA := uint32(1000)
		aIdx := 0
		frames := 0

		for loop := 0; loop < 6; loop++ {
			for _, nal := range cam.fix.h264NALs {
				select {
				case <-stopCh:
					return
				default:
				}
				typ := nal[0] & 0x1f
				if typ != 1 && typ != 5 && typ != 6 && typ != 7 && typ != 8 && typ != 9 {
					continue
				}
				parts := wireBrokenH264E2E(nal)
				for i, pl := range parts {
					pkt := &rtp.Packet{
						Header: rtp.Header{
							Version: 2, Marker: (typ == 1 || typ == 5) && i == len(parts)-1, PayloadType: 96,
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
					frames++
					if target := cam.dropNextAt.Load(); target > 0 && frames == int(target) && cam.dropNextAt.CompareAndSwap(target, 0) {
						_ = conn.Close()
						return
					}
					tsV += 3600
					if hasAudio && len(cam.fix.aacFrames) > 0 {
						for k := 0; k < 2; k++ {
							af := cam.fix.aacFrames[aIdx%len(cam.fix.aacFrames)]
							aIdx++
							pl := make([]byte, 4+len(af))
							pl[1] = 16
							binary.BigEndian.PutUint16(pl[2:], uint16(len(af))<<3)
							copy(pl[4:], af)
							apkt := &rtp.Packet{
								Header: rtp.Header{
									Version: 2, Marker: true, PayloadType: 97,
									SequenceNumber: seqA, Timestamp: tsA, SSRC: 0x55667788,
								},
								Payload: pl,
							}
							seqA++
							tsA += 1024
							if sendRTP(audioCh, apkt) != nil {
								return
							}
						}
					}
					time.Sleep(8 * time.Millisecond)
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
		case rtsp.MethodOptions:
			if sendResp(cseq, "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n", "") != nil {
				return
			}
		case rtsp.MethodDescribe:
			cam.seenPaths.Store(req.URL.Path, true)
			base := "rtsp://" + cam.addr + req.URL.Path
			sdpBody := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=rtsp_demo\r\nt=0 0\r\n"+
				"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:%s/track1\r\n"+
				"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/48000/1\r\n"+
				"a=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1188\r\na=control:%s/track2\r\n",
				base, base)
			if sendResp(cseq, "Content-Type: application/sdp\r\n", sdpBody) != nil {
				return
			}
		case rtsp.MethodSetup:
			tr := req.Header.Get("Transport")
			ch, _ := strconv.Atoi(core.Between(tr, "interleaved=", "-"))
			if strings.Contains(req.URL.Path, "track2") {
				audioCh = byte(ch)
				hasAudio = true
			} else {
				videoCh = byte(ch)
			}
			extra := fmt.Sprintf("Session: e2e1;timeout=60\r\nTransport: RTP/AVP/TCP;unicast;interleaved=%d-%d\r\n", ch, ch+1)
			if sendResp(cseq, extra, "") != nil {
				return
			}
		case rtsp.MethodPlay:
			if sendResp(cseq, "Session: e2e1\r\n", "") != nil {
				return
			}
			go pump()
		case rtsp.MethodTeardown:
			_ = sendResp(cseq, "Session: e2e1\r\n", "")
			return
		default:
			if sendResp(cseq, "", "") != nil {
				return
			}
		}
	}
}

func setupInternalRTSPTestServer(t *testing.T) string {
	t.Helper()
	defaultMedias = ParseQuery(url.Values{"video": {""}, "audio": {""}})
	streams.HandleFunc("rtsp", rtspHandler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c := rtsp.NewServer(conn)
			go tcpHandler(c)
		}
	}()
	return "rtsp://" + ln.Addr().String()
}

func TestHanetURLParsingAndGuards(t *testing.T) {
	fix := getE2EFixtures(t)
	cam := startE2EMockCamera(t, fix)

	// Valid #hanet=1 + HANET ;pwd: path preserved
	prod, err := rtspHandler("rtsp://" + cam.addr + "/user:hanet;pwd:SecretCam1#timeout=5#hanet=1")
	require.NoError(t, err)
	conn := prod.(*rtsp.Conn)
	require.True(t, conn.HanetFix)
	require.False(t, conn.Backchannel)
	require.Equal(t, 5, conn.Timeout)
	_, ok := cam.seenPaths.Load("/user:hanet;pwd:SecretCam1")
	require.True(t, ok, "path containing ;pwd:... must be sent intact without fragment")
	_ = prod.Stop()

	// Reject UDP and RTSPS with #hanet=1 without leaking password
	_, err = rtspHandler("rtsp://192.0.2.10:554/user:hanet;pwd:SecretCam2#transport=udp#hanet=1")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SecretCam2")

	_, err = rtspHandler("rtsps://192.0.2.10:554/user:hanet;pwd:SecretCam3#hanet=1")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SecretCam3")
}

func runStrictFFmpegRTSP(t *testing.T, rtspURL string, frames int) {
	t.Helper()
	cmd := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
		"-rtsp_transport", "tcp", "-threads", "1", "-i", rtspURL,
		"-map", "0:v:0", "-map", "0:a:0", "-frames:v", strconv.Itoa(frames),
		"-progress", "pipe:1", "-nostats", "-f", "null", "-",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.NoError(t, err, "ffmpeg RTSP decode failed: stderr=%s", stderr.String())
	require.Empty(t, strings.TrimSpace(stderr.String()), "ffmpeg stderr not empty")
	re := regexp.MustCompile(`(?m)^frame=(\d+)\s*$`)
	matches := re.FindAllStringSubmatch(stdout.String(), -1)
	decoded := 0
	for _, m := range matches {
		if n, _ := strconv.Atoi(m[1]); n > decoded {
			decoded = n
		}
	}
	require.GreaterOrEqual(t, decoded, frames)
}

func waitNoActiveConns(t *testing.T, cam *e2eMockCamera) {
	t.Helper()
	require.Eventually(t, func() bool {
		return cam.activeConns.Load() == 0
	}, 5*time.Second, 20*time.Millisecond, "mock camera still has active connections")
}

func TestHanetE2EStreamPipeline(t *testing.T) {
	fix := getE2EFixtures(t)
	cam := startE2EMockCamera(t, fix)
	srvBase := setupInternalRTSPTestServer(t)

	streamName := "hanet_e2e_test"
	srcURL := "rtsp://" + cam.addr + "/user:hanet;pwd:TestSecret/broken#hanet=1"
	stream, err := streams.New(streamName, srcURL)
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete(streamName) })

	rtspOutURL := srvBase + "/" + streamName

	t.Run("rtsp_ffprobe_and_strict_decode", func(t *testing.T) {
		out, err := exec.Command(
			"ffprobe", "-v", "error", "-rtsp_transport", "tcp",
			"-show_entries", "stream=codec_name,profile,width,height,sample_rate,channels",
			"-of", "json", rtspOutURL,
		).Output()
		require.NoError(t, err)

		type ffprobeStream struct {
			CodecName  string `json:"codec_name"`
			Profile    string `json:"profile"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		}
		var probe struct {
			Streams []ffprobeStream `json:"streams"`
		}
		require.NoError(t, json.Unmarshal(out, &probe))
		require.Len(t, probe.Streams, 2)

		var h264Stream, aacStream *ffprobeStream
		for i := range probe.Streams {
			switch probe.Streams[i].CodecName {
			case "h264":
				h264Stream = &probe.Streams[i]
			case "aac":
				aacStream = &probe.Streams[i]
			}
		}
		require.NotNil(t, h264Stream, "expected h264 video stream in ffprobe output: %s", string(out))
		require.NotNil(t, aacStream, "expected aac audio stream in ffprobe output: %s", string(out))
		require.Equal(t, 320, h264Stream.Width)
		require.Equal(t, 240, h264Stream.Height)
		require.Equal(t, "48000", aacStream.SampleRate)
		require.Equal(t, 1, aacStream.Channels)

		runStrictFFmpegRTSP(t, rtspOutURL, 100)
	})

	t.Run("concurrent_clients_share_single_upstream", func(t *testing.T) {
		waitNoActiveConns(t, cam)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			runStrictFFmpegRTSP(t, rtspOutURL, 160)
		}()

		require.Eventually(t, func() bool {
			return cam.activeConns.Load() == 1
		}, 10*time.Second, 20*time.Millisecond, "first client did not establish active upstream")
		// Give the active session a moment so probe teardown cannot overlap.
		time.Sleep(150 * time.Millisecond)
		connsWhileStreaming := cam.totalConns.Load()

		wg.Add(1)
		go func() {
			defer wg.Done()
			runStrictFFmpegRTSP(t, rtspOutURL, 100)
		}()
		wg.Wait()
		require.Equal(t, connsWhileStreaming, cam.totalConns.Load(), "second client must reuse existing upstream without dialing")
	})

	t.Run("mp4_mse_consumer_strict_decode", func(t *testing.T) {
		waitNoActiveConns(t, cam)
		cons := mp4.NewConsumer(nil)
		require.NoError(t, stream.AddConsumer(cons))

		tmpFile := filepath.Join(t.TempDir(), "out.mp4")
		f, err := os.Create(tmpFile)
		require.NoError(t, err)

		var writeErr error
		writeDone := make(chan struct{})
		go func() {
			_, writeErr = cons.WriteTo(f)
			close(writeDone)
		}()

		// Collect enough fMP4 moof fragments for >=100 video frames + audio regardless of runner speed.
		require.Eventually(t, func() bool {
			raw, readErr := os.ReadFile(tmpFile)
			return readErr == nil && bytes.Count(raw, []byte("moof")) >= 420
		}, 15*time.Second, 100*time.Millisecond)

		stream.RemoveConsumer(cons)
		select {
		case <-writeDone:
		case <-time.After(3 * time.Second):
			t.Fatal("mp4 consumer did not finish")
		}
		_ = f.Close()
		_ = writeErr

		raw, err := os.ReadFile(tmpFile)
		require.NoError(t, err)
		require.True(t, bytes.Contains(raw, []byte("ftyp")))
		require.True(t, bytes.Contains(raw, []byte("moov")))
		require.True(t, bytes.Contains(raw, []byte("moof")))
		require.True(t, bytes.Contains(raw, []byte("mdat")))

		cmd := exec.Command(
			"ffmpeg", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
			"-threads", "1", "-i", tmpFile, "-map", "0:v:0", "-map", "0:a:0",
			"-progress", "pipe:1", "-nostats", "-f", "null", "-",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		require.NoError(t, cmd.Run(), "ffmpeg mp4 decode failed: stderr=%s", stderr.String())
		require.Empty(t, strings.TrimSpace(stderr.String()))

		re := regexp.MustCompile(`(?m)^frame=(\d+)\s*$`)
		matches := re.FindAllStringSubmatch(stdout.String(), -1)
		decoded := 0
		for _, m := range matches {
			if n, _ := strconv.Atoi(m[1]); n > decoded {
				decoded = n
			}
		}
		require.GreaterOrEqual(t, decoded, 100, "mp4 decoded too few frames: %d", decoded)
	})

	t.Run("webrtc_h264_repack_pipeline", func(t *testing.T) {
		waitNoActiveConns(t, cam)
		cons := &mockWebRTCH264Consumer{
			medias: []*core.Media{{
				Kind: core.KindVideo, Direction: core.DirectionSendonly,
				Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
			}},
		}
		require.NoError(t, stream.AddConsumer(cons))
		require.Eventually(t, func() bool {
			return cons.frames.Load() >= 100 && cons.keyframes.Load() >= 2
		}, 12*time.Second, 30*time.Millisecond)
		stream.RemoveConsumer(cons)
		require.Greater(t, cons.rtpOut.Load(), int32(100))
		require.Equal(t, base64.StdEncoding.EncodeToString(fix.h264SPS)+","+base64.StdEncoding.EncodeToString(fix.h264PPS),
			core.Between(cons.fmtp, "sprop-parameter-sets=", ";"))
	})

	t.Run("upstream_disconnect_and_reconnect", func(t *testing.T) {
		waitNoActiveConns(t, cam)
		before := cam.totalConns.Load()
		cons := &mockWebRTCH264Consumer{
			medias: []*core.Media{{
				Kind: core.KindVideo, Direction: core.DirectionSendonly,
				Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
			}},
		}
		cam.dropNextAt.Store(25)
		require.NoError(t, stream.AddConsumer(cons))
		require.Eventually(t, func() bool {
			return cam.totalConns.Load() > before+2 && cons.frames.Load() >= 80
		}, 15*time.Second, 30*time.Millisecond, "stream must reconnect and resume delivering frames")
		stream.RemoveConsumer(cons)

		// Fresh strict FFmpeg decode after reconnect
		runStrictFFmpegRTSP(t, rtspOutURL, 110)
	})
}

type mockWebRTCH264Consumer struct {
	core.Connection
	medias    []*core.Media
	fmtp      string
	frames    atomic.Int32
	keyframes atomic.Int32
	rtpOut    atomic.Int32
}

func (m *mockWebRTCH264Consumer) GetMedias() []*core.Media { return m.medias }

func (m *mockWebRTCH264Consumer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	m.fmtp = track.Codec.FmtpLine
	sender := core.NewSender(media, codec)
	pay := h264.RTPPay(1200, func(packet *rtp.Packet) {
		m.rtpOut.Add(1)
	})
	sender.Handler = h264.RTPDepay(track.Codec, func(packet *rtp.Packet) {
		m.frames.Add(1)
		if h264.IsKeyframe(packet.Payload) {
			m.keyframes.Add(1)
		}
		pay(packet)
	})
	sender.HandleRTP(track)
	m.Senders = append(m.Senders, sender)
	return nil
}
