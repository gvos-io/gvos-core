// uploader.go: the output-directory scanner and the upload/index/emit
// loop (recording-oss-upgrade-plan §3). One pass per tick: list
// finished MP3s in the output dir, classify tracks per call, upload
// through the Storage backend, index in PG (recordings table, 009),
// emit gvos.recording.completed, then remove the local artifact
// (at-least-once: a crash between upload and cleanup re-uploads —
// idempotent by object key + UNIQUE(call_id,track,format)).
package recpipe

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/shared/events"
)

// PGERecordIndex is the PG-facing index seam (satisfied by store.PG
// in the api wiring; nil in memory mode — the scan still uploads and
// cleans, just without indexing).
type PGERecordIndex interface {
	UpsertRecording(ctx context.Context, callID, track, objectKey, locator, format string, sizeBytes int64) error
}

// Emitter is the NATS publish seam (Server.emit's shape).
type Emitter func(subject string, payload any)

// Uploader owns the pipeline loop.
type Uploader struct {
	OutputDir string
	Store     Storage
	PG        PGERecordIndex
	Emit      Emitter // nil = silent

	// seenForCall carries the file-order track classification
	// (agent first, user second) across passes.
	seenForCall map[string]int
}

// NewUploader wires the loop.
func NewUploader(outputDir string, store Storage, pg PGERecordIndex, emit Emitter) *Uploader {
	return &Uploader{OutputDir: outputDir, Store: store, PG: pg, Emit: emit, seenForCall: map[string]int{}}
}

// ScanOnce processes everything currently sitting in the output dir.
// Returns the number of artifacts uploaded.
func (u *Uploader) ScanOnce(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(u.OutputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // output dir not mounted yet
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp3") {
			continue
		}
		uploaded, err := u.processOne(ctx, e.Name())
		if err != nil {
			log.Printf("recpipe %s: %v (retry next tick)", e.Name(), err)
			continue
		}
		if uploaded {
			n++
		}
	}
	return n, nil
}

// processOne handles a single artifact end-to-end; uploaded=false
// means "left in place" (unknown naming / still being written).
func (u *Uploader) processOne(ctx context.Context, name string) (bool, error) {
	callID, ok := ParseOutputName(name)
	if !ok {
		// Unknown naming: leave the file for a human (don't delete).
		log.Printf("recpipe: unrecognized output name %q, left in place", name)
		return false, nil
	}
	local := filepath.Join(u.OutputDir, name)
	st, err := os.Stat(local)
	if err != nil {
		return false, err
	}
	if time.Since(st.ModTime()) < 2*time.Second {
		return false, nil // still being written; wait for the next tick
	}

	rec := Recording{CallID: callID, LocalPath: local, SizeBytes: st.Size()}
	// Peek track without advancing: failed uploads must not flip
	// agent/user on the next retry.
	n := u.seenForCall[callID]
	if n == 0 {
		rec.Track = TrackAgent
	} else {
		rec.Track = TrackUser
	}
	objectKey := ObjectKey(callID, rec.Track, "mp3", st.ModTime().Format(time.RFC3339))

	locator, err := u.Store.Put(ctx, objectKey, local)
	if err != nil {
		return false, fmt.Errorf("upload: %w", err)
	}
	if u.PG != nil {
		if err := u.PG.UpsertRecording(ctx, callID, string(rec.Track), objectKey, locator, "mp3", st.Size()); err != nil {
			return false, fmt.Errorf("index: %w", err)
		}
	}
	// Upload + index done: remove the local artifact (the retry loop
	// is the at-least-once guarantee; PG's UNIQUE collapses replays).
	if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("cleanup: %w", err)
	}
	u.seenForCall[callID] = n + 1
	if u.Emit != nil {
		u.Emit(events.SubjectRecordingCompleted, map[string]any{
			"call_id": callID, "track": rec.Track,
			"object_key": objectKey, "backend": u.Store.Kind(),
			"size_bytes": st.Size(), "format": "mp3",
		})
	}
	log.Printf("recpipe: uploaded %s (%s track, %d bytes, %s)", name, rec.Track, st.Size(), u.Store.Kind())
	return true, nil
}

// Run scans on a ticker until stop is closed (the api layer starts
// this next to the other async workers). Overlapping ticks are
// skipped so a slow ScanOnce cannot pile up concurrent ReadDir/upload
// passes against a large output directory.
func (u *Uploader) Run(ctx context.Context, interval time.Duration) (stop func()) {
	t := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		busy := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if busy {
					continue
				}
				busy = true
				if _, err := u.ScanOnce(ctx); err != nil {
					log.Printf("recpipe scan: %v", err)
				}
				busy = false
			}
		}
	}()
	return func() { t.Stop(); <-done }
}
