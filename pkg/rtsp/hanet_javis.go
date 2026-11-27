package rtsp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/hanetfix"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
)

const (
	defaultHanetProbeTimeout = 8 * time.Second
	maxHanetProbePackets     = 4000
	maxHanetProbeBytes       = 8 * 1024 * 1024
	hanetLogRateLimit        = 30 * time.Second
)

// HanetEvent is emitted via Conn.Listen for structured operational logging
// without exposing credentials or raw payload bytes.
type HanetEvent struct {
	Level  string
	Event  string
	Target string
	Detail string
}

type hanetState struct {
	mu            sync.Mutex
	repair        bool
	videoPT       uint8
	evidence      hanetfix.Evidence
	converter     *hanetfix.Converter
	stats         hanetfix.Stats
	lastDropLog   time.Time
	lastResyncLog time.Time
}

func (c *Conn) ensureHanetState() *hanetState {
	if c.hanet == nil {
		c.hanet = &hanetState{}
	}
	return c.hanet
}

// HanetStats returns a point-in-time snapshot of HANET repair counters.
func (c *Conn) HanetStats() hanetfix.StatsSnapshot {
	if c.hanet == nil {
		return hanetfix.StatsSnapshot{}
	}
	return c.hanet.stats.Snapshot()
}

func (c *Conn) hanetValidateConfig() error {
	if c.URL == nil || !strings.EqualFold(c.URL.Scheme, "rtsp") {
		return errors.New("hanetfix: only plain rtsp:// TCP transport is supported")
	}
	if c.Transport != "" && c.Transport != "tcp" {
		return errors.New("hanetfix: only RTSP TCP interleaved transport is supported")
	}
	return nil
}

func (c *Conn) fireHanet(level, event, detail string) {
	c.Fire(HanetEvent{
		Level:  level,
		Event:  event,
		Target: hanetfix.RedactURL(c.uri),
		Detail: detail,
	})
}

func (c *Conn) hanetProcessDescribe(rawSDP []byte) ([]byte, error) {
	st := c.ensureHanetState()
	target := hanetfix.RedactURL(c.uri)

	cand, isCand, err := hanetfix.InspectSDP(string(rawSDP))
	if err != nil {
		st.mu.Lock()
		st.repair = false
		st.mu.Unlock()
		st.stats.ProbeFailures.Add(1)
		c.fireHanet("warn", "ambiguous_sdp", fmt.Sprintf("HANET SDP ambiguous (%s): %v", target, err))
		return nil, err
	}

	if !isCand {
		st.mu.Lock()
		st.repair = false
		st.mu.Unlock()
		st.stats.Passthrough.Add(1)
		c.fireHanet("debug", "passthrough", fmt.Sprintf("HANET opt-in passthrough (%s)", target))
		return rawSDP, nil
	}

	st.stats.Candidate.Add(1)
	c.fireHanet("info", "candidate", fmt.Sprintf("HANET candidate PT=%d (%s)", cand.PayloadType, target))

	class, ev, err := c.hanetProbeCurrentSession(rawSDP, cand)
	if err != nil {
		st.stats.ProbeFailures.Add(1)
		c.fireHanet("warn", "probe_failed", fmt.Sprintf("HANET probe failed (%s): %v", target, err))
		return nil, err
	}

	switch class {
	case hanetfix.ClassConfirmedHEVC:
		st.stats.ConfirmedHEVC.Add(1)
		st.stats.Passthrough.Add(1)
		c.fireHanet("info", "confirmed_hevc", fmt.Sprintf("HANET probe confirmed genuine HEVC passthrough (%s)", target))

		freshSDP, err := c.hanetFreshDescribe()
		if err != nil {
			st.stats.ProbeFailures.Add(1)
			return nil, err
		}

		// Re-check that the camera did not switch to malformed HANET between probe and main session.
		if freshCand, stillCand, checkErr := hanetfix.InspectSDP(string(freshSDP)); checkErr != nil {
			_ = c.Close()
			return nil, checkErr
		} else if stillCand && freshCand.PayloadType != cand.PayloadType {
			_ = c.Close()
			st.stats.CodecMismatches.Add(1)
			return nil, errors.New("hanetfix: SDP payload type changed after HEVC probe")
		}

		st.mu.Lock()
		st.repair = false
		st.mu.Unlock()
		return freshSDP, nil

	case hanetfix.ClassConfirmedHANET:
		st.stats.ConfirmedHANET.Add(1)
		c.fireHanet("info", "confirmed_hanet", fmt.Sprintf(
			"HANET confirmed H264 (%dx%d profile=0x%02x) PT=%d (%s)",
			ev.SPSInfo.Width, ev.SPSInfo.Height, ev.SPSInfo.ProfileIDC, cand.PayloadType, target,
		))

		freshSDP, err := c.hanetFreshDescribe()
		if err != nil {
			st.stats.ProbeFailures.Add(1)
			return nil, err
		}

		freshCand, stillCand, err := hanetfix.InspectSDP(string(freshSDP))
		if err != nil {
			_ = c.Close()
			st.stats.CodecMismatches.Add(1)
			return nil, err
		}
		if !stillCand {
			_ = c.Close()
			st.stats.CodecMismatches.Add(1)
			c.fireHanet("warn", "sdp_changed", fmt.Sprintf("HANET SDP changed after probe (%s)", target))
			return nil, errors.New("hanetfix: SDP changed after probe")
		}

		fixedSDP, err := hanetfix.RewriteSDP(string(freshSDP), freshCand.PayloadType, ev)
		if err != nil {
			_ = c.Close()
			st.stats.ProbeFailures.Add(1)
			return nil, err
		}

		st.mu.Lock()
		st.repair = true
		st.videoPT = freshCand.PayloadType
		st.evidence = ev.Clone()
		if st.converter == nil {
			st.converter = hanetfix.NewConverter(st.evidence, &st.stats)
		} else {
			st.converter.ResetForReconnect(st.evidence)
		}
		st.mu.Unlock()

		return []byte(fixedSDP), nil

	default:
		st.stats.ProbeFailures.Add(1)
		c.fireHanet("warn", "probe_ambiguous", fmt.Sprintf("HANET signature unconfirmed; refusing ambiguous H265 (%s)", target))
		return nil, errors.New("hanetfix: HANET signature unconfirmed; refusing ambiguous H265")
	}
}

func (c *Conn) hanetProbeCurrentSession(rawSDP []byte, cand hanetfix.SDPCandidate) (hanetfix.Classification, hanetfix.Evidence, error) {
	oldMedias := c.Medias
	oldBackchannel := c.Backchannel
	defer func() {
		if c.conn != nil {
			_ = c.Teardown()
			_ = c.conn.Close()
			c.conn = nil
		}
		c.state.Store(StateNone)
		c.session = ""
		c.keepalive = 0
		c.Medias = oldMedias
		c.Backchannel = oldBackchannel
	}()

	medias, err := UnmarshalSDP(rawSDP)
	if err != nil {
		return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe SDP parse failed: %w", err)
	}

	var videoMedia *core.Media
	videoIdx := 0
	for _, m := range medias {
		if m.Kind != core.KindVideo {
			continue
		}
		hasPT := false
		for _, codec := range m.Codecs {
			if codec.PayloadType == cand.PayloadType {
				hasPT = true
				break
			}
		}
		if hasPT && (videoIdx == cand.VideoIndex || (cand.Control != "" && m.ID == cand.Control)) {
			videoMedia = m
			break
		}
		videoIdx++
	}
	if videoMedia == nil {
		return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, errors.New("hanetfix: candidate video track missing in probe SDP")
	}

	c.Medias = medias
	c.Backchannel = false

	videoCh, err := c.SetupMedia(videoMedia)
	if err != nil {
		return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe setup failed: %w", err)
	}

	if err = c.Play(); err != nil {
		return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe play failed: %w", err)
	}

	timeout := c.HanetProbeTimeout
	if timeout <= 0 {
		timeout = defaultHanetProbeTimeout
	}
	deadline := time.Now().Add(timeout)

	var classifier hanetfix.Classifier
	totalBytes := 0

	for pkts := 0; pkts < maxHanetProbePackets && totalBytes < maxHanetProbeBytes && time.Now().Before(deadline); {
		if err = c.conn.SetReadDeadline(deadline); err != nil {
			return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, err
		}

		buf4, err := c.reader.Peek(4)
		if err != nil {
			return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe read failed: %w", err)
		}

		if buf4[0] != '$' {
			switch string(buf4) {
			case "RTSP":
				res, err := readResponse(c.conn, c.reader)
				if err != nil {
					return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe response failed: %w", err)
				}
				if res.StatusCode != 200 {
					return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, fmt.Errorf("hanetfix: probe play status %d", res.StatusCode)
				}
				continue

			case "OPTI", "TEAR", "DESC", "SETU", "PLAY", "PAUS", "RECO", "ANNO", "GET_", "SET_":
				req, err := c.ReadRequest()
				if err != nil {
					return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, err
				}
				if req.Method == MethodOptions {
					if err = c.WriteResponse(&tcp.Response{Request: req}); err != nil {
						return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, err
					}
				}
				continue

			default:
				return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, errors.New("hanetfix: unexpected RTSP stream framing during probe")
			}
		}

		ch := buf4[1]
		size := int(binary.BigEndian.Uint16(buf4[2:]))
		if _, err = c.reader.Discard(4); err != nil {
			return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, err
		}

		data := make([]byte, size)
		if _, err = io.ReadFull(c.reader, data); err != nil {
			return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, err
		}
		totalBytes += size

		if ch != videoCh {
			continue
		}

		var pkt rtp.Packet
		if pkt.Unmarshal(data) != nil || pkt.PayloadType != cand.PayloadType {
			continue
		}

		pkts++
		state := classifier.ObservePacket(&pkt)
		if state == hanetfix.ClassConfirmedHANET || state == hanetfix.ClassConfirmedHEVC {
			return state, classifier.Evidence(), nil
		}
	}

	return hanetfix.ClassAmbiguous, hanetfix.Evidence{}, errors.New("hanetfix: probe timeout before conclusive video classification")
}

func (c *Conn) hanetFreshDescribe() ([]byte, error) {
	if err := c.Dial(); err != nil {
		return nil, err
	}

	req := &tcp.Request{
		Method: MethodDescribe,
		URL:    c.URL,
		Header: map[string][]string{
			"Accept": {"application/sdp"},
		},
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	res, err := c.Do(req)
	if err != nil {
		_ = c.Close()
		return nil, err
	}

	if val := res.Header.Get("Content-Base"); val != "" {
		c.URL, err = urlParse(val)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
	}

	return res.Body, nil
}

func (c *Conn) hanetCheckReconnectCodec(nextMedias []*core.Media) error {
	if c.Medias == nil {
		return nil
	}

	for _, oldMedia := range c.Medias {
		if oldMedia.Kind != core.KindVideo || len(oldMedia.Codecs) == 0 {
			continue
		}
		for _, nextMedia := range nextMedias {
			if nextMedia.Kind != core.KindVideo || len(nextMedia.Codecs) == 0 {
				continue
			}
			oldCodec := oldMedia.Codecs[0]
			nextCodec := nextMedia.Codecs[0]
			if oldCodec.Name != nextCodec.Name {
				if c.hanet != nil {
					c.hanet.stats.CodecMismatches.Add(1)
				}
				c.fireHanet("warn", "codec_changed", fmt.Sprintf(
					"HANET video codec switched %s -> %s (%s)",
					oldCodec.Name, nextCodec.Name, hanetfix.RedactURL(c.uri),
				))
				_ = c.Close()
				return fmt.Errorf("hanetfix: video codec changed (%s -> %s); renegotiate stream", oldCodec.Name, nextCodec.Name)
			}
			oldCodec.PayloadType = nextCodec.PayloadType
			if nextCodec.FmtpLine != "" {
				oldCodec.FmtpLine = nextCodec.FmtpLine
			}
			break
		}
	}
	return nil
}

func (c *Conn) hanetHandleVideoPacket(receiver *core.Receiver, packet *rtp.Packet) (bool, error) {
	st := c.hanet
	if st == nil || receiver == nil || receiver.Media == nil || receiver.Media.Kind != core.KindVideo {
		return false, nil
	}

	st.mu.Lock()
	if !st.repair || packet.PayloadType != st.videoPT || st.converter == nil {
		st.mu.Unlock()
		return false, nil
	}

	prevDrops := st.stats.DroppedPackets.Load()
	prevResyncs := st.stats.Resyncs.Load()

	outPackets, err := st.converter.Process(packet)

	now := time.Now()
	newDrops := st.stats.DroppedPackets.Load()
	newResyncs := st.stats.Resyncs.Load()
	emitDrop := newDrops > prevDrops && (st.lastDropLog.IsZero() || now.Sub(st.lastDropLog) >= hanetLogRateLimit)
	if emitDrop {
		st.lastDropLog = now
	}
	emitResync := newResyncs > prevResyncs && (st.lastResyncLog.IsZero() || now.Sub(st.lastResyncLog) >= hanetLogRateLimit)
	if emitResync {
		st.lastResyncLog = now
	}
	st.mu.Unlock()

	if err != nil {
		c.fireHanet("warn", "runtime_mismatch", fmt.Sprintf("HANET active mismatch (%s): %v", hanetfix.RedactURL(c.uri), err))
		return true, err
	}
	if emitResync {
		c.fireHanet("debug", "resync", fmt.Sprintf("HANET resynced at keyframe boundary (%s), resyncs=%d", hanetfix.RedactURL(c.uri), newResyncs))
	} else if emitDrop {
		c.fireHanet("debug", "drop", fmt.Sprintf("HANET dropped pre-IDR/incomplete packets (%s), dropped=%d", hanetfix.RedactURL(c.uri), newDrops))
	}

	for _, outPkt := range outPackets {
		receiver.WriteRTP(outPkt)
	}
	return true, nil
}
