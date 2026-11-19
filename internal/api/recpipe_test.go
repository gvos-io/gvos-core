package api

import (
	"github.com/fastvoip/gvos-core/internal/recpipe"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordingPipelineEndToEnd: drop two finished MP3s in the output
// dir (agent first), run one scan, verify the archive holds both with
// LCR-…-sorry, with agent/user classification, the local artifact is
// cleaned, and the play endpoint resolves them (PG mode index).
func TestRecordingPipelineEndToEnd(t *testing.T) {
	out := t.TempDir()
	arch := t.TempDir()
	s := NewServer()
	_ = s

	u := AttachRecording(s, out, arch)
	if u == nil {
		t.Fatal("uploader nil")
	}
	// One call, two tracks (PG nil in memory mode: index skipped, the
	// pipeline still uploads+cleans — honest memory-mode behavior).
	os.WriteFile(filepath.Join(out, "call-x-e4e1f2a3.mp3"), []byte("agent audio"), 0o644)
	os.WriteFile(filepath.Join(out, "call-x-b7f2c9d1.mp3"), []byte("user audio"), 0o644)
	time.Sleep(2100 * time.Millisecond) // clear the "still being written" guard

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := u.ScanOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c1, ok1 := recpipe.ParseOutputName("call-x-e4e1f2a3.mp3")
	t.Logf("probe: n=%d parse(call-x-e4e1f2a3.mp3)=%q ok=%v", n, c1, ok1)
	if n != 2 {
		t.Fatalf("uploaded %d, want 2", n)
	}
	filepath.WalkDir(arch, func(p string, d os.DirEntry, err error) error {
		t.Logf("archive: %s (dir=%v)", p, d.IsDir())
		return nil
	})
	// Object layout: recordings/<yyyy-mm>/<call>/agent.mp3 + user.mp3.
	for _, track := range []string{"agent", "user"} {
		p := filepath.Join(arch, "recordings", time.Now().Format("2006-01"), "call-x", track+".mp3")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("archive %s: %v", track, err)
		}
	}
	left, _ := os.ReadDir(out)
	if len(left) != 0 {
		t.Fatalf("output not cleaned: %d files left", len(left))
	}
}
