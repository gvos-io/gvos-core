package recpipe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFSPutDelete(t *testing.T) {
	root := t.TempDir()
	l := &LocalFS{Root: root}
	src := filepath.Join(t.TempDir(), "agent.mp3")
	os.WriteFile(src, []byte("mp3-bytes"), 0o644)

	loc, err := l.Put(context.Background(), "recordings/2026-11/c1/agent.mp3", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loc); err != nil {
		t.Fatalf("locator file missing: %v", err)
	}
	if err := l.Delete(context.Background(), "recordings/2026-11/c1/agent.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loc); !os.IsNotExist(err) {
		t.Error("delete did not remove")
	}
	if err := l.Delete(context.Background(), "recordings/none.mp3"); err != nil {
		t.Error("delete of missing object must be a no-op")
	}
}
