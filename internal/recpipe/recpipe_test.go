package recpipe

import (
	"path/filepath"
	"testing"
)

func TestParseOutputName(t *testing.T) {
	cases := []struct {
		in   string
		call string
		ok   bool
	}{
		{"1-1%40172.22.0.9-eadce8fa0994b150.mp3", "1-1@172.22.0.9", true},
		{"manual-poc-9-e49ddab98320ab75.mp3", "manual-poc-9", true},
		{"callid-mix.mp3", "callid", true},
		{"nope.txt", "", false},
		{"noextension", "", false},
	}
	for _, c := range cases {
		call, ok := ParseOutputName(c.in)
		if ok != c.ok || (ok && call != c.call) {
			t.Errorf("ParseOutputName(%q) = %q %v, want %q %v", c.in, call, ok, c.call, c.ok)
		}
	}
}

func TestParseMetaExtractsPCAP(t *testing.T) {
	content := "/recording/pcaps/manual-poc-9-x.pcap\n\nTimestamp terminated ms: 1\ncall start time: x\n"
	p, ok := ParseMetaExtractsPCAP(content)
	if !ok || p != "/recording/pcaps/manual-poc-9-x.pcap" {
		t.Errorf("pcap = %q %v", p, ok)
	}
	if _, ok := ParseMetaExtractsPCAP("no pcap here"); ok {
		t.Error("empty metadata must miss")
	}
}

func TestClassifyTrackOrder(t *testing.T) {
	seen := map[string]int{}
	r1 := Recording{CallID: "c1", LocalPath: "a.mp3"}
	r2 := Recording{CallID: "c1", LocalPath: "b.mp3"}
	if ClassifyTrack(r1, seen) != TrackAgent {
		t.Error("first file must be agent")
	}
	if ClassifyTrack(r2, seen) != TrackUser {
		t.Error("second file must be user")
	}
	if ClassifyTrack(Recording{CallID: "c2"}, seen) != TrackAgent {
		t.Error("new call restarts classification")
	}
}

func TestObjectKey(t *testing.T) {
	k := ObjectKey("call-1", TrackAgent, "mp3", "2026-11-20T15:00:00Z")
	if k != "recordings/2026-11/call-1/agent.mp3" {
		t.Errorf("key = %q", k)
	}
}

func TestLocalPathJoinTraversal(t *testing.T) {
	if _, err := LocalPathJoin("/out", "../../etc/passwd", "mp3"); err == nil {
		t.Error("traversal must be rejected")
	}
	if _, err := LocalPathJoin("/out", "/abs/path", "mp3"); err == nil {
		t.Error("absolute call-id must be rejected")
	}
	p, err := LocalPathJoin("/out", "call-1", "mp3")
	want := filepath.Join("/out", "call-1-mp3")
	if err != nil || p != want {
		t.Errorf("join = %q err=%v, want %q", p, err, want)
	}
}
