// storage.go: the object-storage seam for the recording pipeline
// (recording-oss-upgrade-plan §2.2). LocalFS is the POC/POC-grade
// backend; OSS (aliyun-sdk-oss or minio-go S3-compatible) plugs into
// the same interface once credentials exist — the R0-era decision
// that the Go uploader is the only OSS exit stays unchanged.
package recpipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Storage is one object backend.
type Storage interface {
	// Put uploads a local file under objectKey; returns the opaque
	// locator the play endpoint resolves later.
	Put(ctx context.Context, objectKey, localPath string) (locator string, err error)
	// Delete removes the object (retention policy / manual cleanup).
	Delete(ctx context.Context, objectKey string) error
	// Kind names the backend ("local", "oss", ...) — surfaced in stats.
	Kind() string
}

// LocalFS stores objects under a root directory; the locator is the
// absolute file path (the play endpoint streams from it).
type LocalFS struct {
	Root string
}

// Put copies the file into the backend root (os.Rename when same
// filesystem, copy otherwise — always copy: the output dir may be a
// different mount than the archive).
func (l *LocalFS) Put(_ context.Context, objectKey, localPath string) (string, error) {
	dst := filepath.Join(l.Root, filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// Delete removes the object file (missing = fine).
func (l *LocalFS) Delete(_ context.Context, objectKey string) error {
	err := os.Remove(filepath.Join(l.Root, filepath.FromSlash(objectKey)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Kind implements Storage.
func (l *LocalFS) Kind() string { return "local" }

// objectKeyFromPath derives a stable content-ish key: recordings/<ymd>/<hash>.mp3
// (the call/track classification happens in the uploader; this is the
// raw-landing layout used before classification).
func objectKeyFromPath(localPath string, at time.Time) string {
	sum := sha256.Sum256([]byte(localPath))
	key := hex.EncodeToString(sum[:])[:16]
	return fmt.Sprintf("recordings/%s/%s.mp3", at.Format("2006-01"), key)
}

// fileModTime is the metadata timestamp used for the month bucket.
func fileModTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Now()
	}
	return st.ModTime()
}
