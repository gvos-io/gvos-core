// Package recpipe is the recording pipeline (recording-oss-upgrade-plan
// R2): it watches the rtpengine-recording daemon's OUTPUT directory,
// parses each finished call's metadata, classifies tracks (agent/user
// legs), uploads artifacts through a Storage backend, indexes them in
// PostgreSQL, and emits gvos.recording.completed events.
//
// Pipeline position: rtpengine-recording daemon (DUX→MP3) writes
// /output → recpipe (this package) → Storage + PG + NATS → the play
// API serves presigned/stream URLs from the recordings table.
package recpipe

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Track classification of one recording file.
type Track string

const (
	TrackAgent Track = "agent"
	TrackUser  Track = "user"
)

// Recording is one finished artifact the uploader works on.
type Recording struct {
	CallID    string
	Track     Track
	LocalPath string // absolute path in the output dir
	SizeBytes int64
}

// outputFilenames look like <call-id>-<ssrc|mix>.mp3 (the daemon's
// --output-pattern=%c-%t). The call-id may contain %-escapes
// (%40 = '@') — they are normalized back.
var outputNameRe = regexp.MustCompile(`^(?P<call>.+)-(?P<tag>[0-9a-f]{8,}|mix)\.(mp3|wav)$`)

// ParseOutputName splits a daemon output filename into its call-id and
// track hint. The SSRC→leg classification (agent/user) is NOT
// decidable from the name alone — the caller uses the DUX metadata
// (A-leg from-tag / c= IP) per recording-oss-upgrade-plan §1.3; this
// returns TrackAgent for the first-seen SSRC and TrackUser otherwise
// via the caller-provided classifier.
func ParseOutputName(name string) (callID string, ok bool) {
	m := outputNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	call := m[1]
	// Un-escape rtpengine's filename encoding (%40 -> '@' etc).
	if strings.Contains(call, "%") {
		call = unescape(call)
	}
	return call, true
}

// unescape decodes %XX hex escapes (rtpengine normalizes special
// characters in filenames to %XX).
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// MetaEntry is one stream line from the DUX metadata (each SSRC's
// SDP block tells the legs apart: the A-leg's c= IP is the agent's
// registration network, the B-leg's goes through the gateway).
type MetaEntry struct {
	CallID   string
	PcapPath string
	SDP      string
	Started  string
}

// ParseMetaExtractsPCAP pulls the pcap reference (first line) of a
// metadata file — the uploader cross-references output files against
// it to bind tracks to calls.
func ParseMetaExtractsPCAP(content string) (pcapPath string, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ".pcap") {
			return line, true
		}
	}
	return "", false
}

// ClassifyTrack assigns the agent/user split: rtpengine's A-leg
// (caller side, SIPp agent in the POC) recorded first; the classifier
// falls back to file order within a call when metadata labels are
// absent (known POC limitation, recording-plan §R0).
func ClassifyTrack(rec Recording, seenForCall map[string]int) Track {
	n := seenForCall[rec.CallID]
	seenForCall[rec.CallID] = n + 1
	if n == 0 {
		return TrackAgent
	}
	return TrackUser
}

// ObjectKey is the storage key layout: recordings/<yyyy-mm>/<call>/<track>.<fmt>
func ObjectKey(callID string, track Track, format string, at string) string {
	month := at
	if len(month) >= 7 {
		month = month[:7]
	}
	return fmt.Sprintf("recordings/%s/%s/%s.%s", month, callID, track, format)
}

// LocalPathJoin guards against path traversal in the play endpoint
// (call-id comes from the request; it must stay inside the output
// root).
func LocalPathJoin(root, callID, format string) (string, error) {
	clean := filepath.Clean(callID)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, `\`) {
		return "", fmt.Errorf("bad call-id path")
	}
	return filepath.Join(root, clean+"-"+format), nil
}
