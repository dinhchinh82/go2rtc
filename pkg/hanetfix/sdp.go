package hanetfix

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SDPCandidate describes a video media track in an SDP that matches the
// prefilter signature for the malformed HANET H.265/H.264 stream.
type SDPCandidate struct {
	PayloadType uint8
	Control     string
	VideoIndex  int
}

// ErrAmbiguousSDP indicates that an rtsp_demo SDP contains more than one
// candidate H.265 video track/payload type without fmtp.
var ErrAmbiguousSDP = errors.New("hanetfix: ambiguous SDP with multiple candidate H265 video tracks")

var (
	rePathSecret  = regexp.MustCompile(`(?i)(pwd:)([^;/\?#]+)`)
	reQuerySecret = regexp.MustCompile(`(?i)([?&](?:pwd|pass|password|token)=)([^&#\s]+)`)
)

// InspectSDP checks whether rawSDP matches the narrow HANET candidate
// prefilter: session name `s=rtsp_demo` and a single `m=video` payload type
// mapped to `H265/90000` with no `a=fmtp:<pt>` attribute for that payload type.
func InspectSDP(rawSDP string) (SDPCandidate, bool, error) {
	lines := strings.Split(strings.ReplaceAll(rawSDP, "\r", ""), "\n")

	hasDemo := false
	inSession := true

	type videoSection struct {
		index   int
		formats map[string]struct{}
		rtpmap  map[uint8]string
		fmtp    map[uint8]string
		control string
	}

	var videos []videoSection
	var current *videoSection
	videoIdx := -1

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "m=") {
			inSession = false
			if strings.HasPrefix(line, "m=video ") {
				videoIdx++
				vs := videoSection{
					index:   videoIdx,
					formats: make(map[string]struct{}),
					rtpmap:  make(map[uint8]string),
					fmtp:    make(map[uint8]string),
				}
				fields := strings.Fields(line)
				if len(fields) >= 4 {
					for _, f := range fields[3:] {
						vs.formats[f] = struct{}{}
					}
				}
				videos = append(videos, vs)
				current = &videos[len(videos)-1]
			} else {
				current = nil
			}
			continue
		}

		if inSession {
			if line == "s=rtsp_demo" {
				hasDemo = true
			}
			continue
		}

		if current == nil {
			continue
		}

		switch {
		case strings.HasPrefix(line, "a=rtpmap:"):
			rest := strings.TrimPrefix(line, "a=rtpmap:")
			ptStr, codec, ok := strings.Cut(rest, " ")
			if !ok {
				continue
			}
			ptStr = strings.TrimSpace(ptStr)
			codec = strings.TrimSpace(codec)
			if _, listed := current.formats[ptStr]; !listed {
				continue
			}
			pt64, err := strconv.ParseUint(ptStr, 10, 8)
			if err != nil {
				continue
			}
			current.rtpmap[uint8(pt64)] = codec

		case strings.HasPrefix(line, "a=fmtp:"):
			rest := strings.TrimPrefix(line, "a=fmtp:")
			ptStr, val, ok := strings.Cut(rest, " ")
			if !ok {
				continue
			}
			pt64, err := strconv.ParseUint(strings.TrimSpace(ptStr), 10, 8)
			if err != nil {
				continue
			}
			if val = strings.TrimSpace(val); val != "" {
				current.fmtp[uint8(pt64)] = val
			}

		case strings.HasPrefix(line, "a=control:"):
			current.control = strings.TrimSpace(strings.TrimPrefix(line, "a=control:"))
		}
	}

	if !hasDemo {
		return SDPCandidate{}, false, nil
	}

	var candidates []SDPCandidate
	for _, vs := range videos {
		for pt, codec := range vs.rtpmap {
			if !strings.EqualFold(codec, "H265/90000") {
				continue
			}
			if _, hasFmtp := vs.fmtp[pt]; hasFmtp {
				continue
			}
			candidates = append(candidates, SDPCandidate{
				PayloadType: pt,
				Control:     vs.control,
				VideoIndex:  vs.index,
			})
		}
	}

	if len(candidates) > 1 {
		return SDPCandidate{}, false, ErrAmbiguousSDP
	}
	if len(candidates) == 1 {
		return candidates[0], true, nil
	}
	return SDPCandidate{}, false, nil
}

// Candidate is a convenience wrapper around InspectSDP returning the payload
// type and whether rawSDP is a single unambiguous HANET candidate.
func Candidate(rawSDP string) (uint8, bool) {
	cand, ok, err := InspectSDP(rawSDP)
	if err != nil || !ok {
		return 0, false
	}
	return cand.PayloadType, true
}

// RewriteSDP converts the matched H265/90000 video track in rawSDP to
// H264/90000 with fmtp derived from the source's own confirmed SPS and PPS.
// Unrelated lines (audio, control, session headers) are preserved verbatim.
func RewriteSDP(rawSDP string, pt uint8, ev Evidence) (string, error) {
	fmtp := ev.Fmtp()
	if fmtp == "" {
		return "", ErrUnconfirmed
	}

	sep := "\n"
	if strings.Contains(rawSDP, "\r\n") {
		sep = "\r\n"
	}

	lines := strings.Split(rawSDP, sep)
	inVideo := false
	replaced := 0
	ptPrefix := strconv.Itoa(int(pt))

	for i, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "m=") {
			inVideo = strings.HasPrefix(trimmed, "m=video ")
			continue
		}
		if !inVideo {
			continue
		}

		if strings.HasPrefix(trimmed, "a=fmtp:") {
			rest := strings.TrimPrefix(trimmed, "a=fmtp:")
			if linePT, _, ok := strings.Cut(rest, " "); ok && strings.TrimSpace(linePT) == ptPrefix {
				return "", fmt.Errorf("hanetfix: unexpected existing fmtp for PT %d", pt)
			}
			continue
		}

		if strings.HasPrefix(trimmed, "a=rtpmap:") {
			rest := strings.TrimPrefix(trimmed, "a=rtpmap:")
			linePT, codec, ok := strings.Cut(rest, " ")
			if !ok || strings.TrimSpace(linePT) != ptPrefix {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(codec), "H265/90000") {
				lines[i] = fmt.Sprintf("a=rtpmap:%d H264/90000%sa=fmtp:%d %s", pt, sep, pt, fmtp)
				replaced++
			}
		}
	}

	if replaced != 1 {
		return "", fmt.Errorf("hanetfix: unexpected SDP rewrite count (%d) for PT %d", replaced, pt)
	}

	return strings.Join(lines, sep), nil
}

// RedactURL masks credentials in RTSP URLs, including standard userinfo,
// HANET path credentials (`pwd:...`), and sensitive query parameters.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	masked := raw
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			user := u.User.Username()
			if idx := strings.Index(masked, "://"); idx >= 0 {
				rest := masked[idx+3:]
				if at := strings.IndexByte(rest, '@'); at >= 0 {
					masked = masked[:idx+3] + user + ":****" + rest[at:]
				}
			}
		}
	}
	masked = rePathSecret.ReplaceAllString(masked, "${1}****")
	masked = reQuerySecret.ReplaceAllString(masked, "${1}****")
	return masked
}
