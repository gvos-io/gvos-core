package recpipe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// s3Stub is a minimal S3-compatible endpoint: PUT/GET/HEAD object +
// HEAD bucket (minio-go's MakeBucket/BucketExists). It validates the
// MinIO backend's protocol behavior without a real server — the
// compose minio service (same API) is the live counterpart.
func s3Stub(t *testing.T) (*httptest.Server, *sync.Map) {
	objects := &sync.Map{} // key -> bytes
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path[1:]
		switch r.Method {
		case "PUT":
			body, _ := io.ReadAll(r.Body)
			// minio-go sends aws-chunked payloads
			// ("<hex>;chunk-signature=<sig>\r\n<data>\r\n" frames);
			// decode to the raw object bytes.
			objects.Store(key, decodeAWSChunked(body))
			w.WriteHeader(200)
		case "GET":
			if v, ok := objects.Load(key); ok {
				w.Write(v.([]byte))
				return
			}
			w.WriteHeader(404)
		case "HEAD":
			if _, ok := objects.Load(key); ok {
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(404)
		case "DELETE":
			objects.Delete(key)
			w.WriteHeader(204)
		}
	})
	return httptest.NewServer(mux), objects
}

func TestMinIOBackendRoundTrip(t *testing.T) {
	srv, objects := s3Stub(t)
	defer srv.Close()

	// secure=false: the stub serves plain HTTP.
	m, err := NewMinIO(srv.URL[len("http://"):], "ak", "sk", "recordings", false)
	if err != nil {
		t.Fatalf("NewMinIO: %v", err)
	}
	src := filepath.Join(t.TempDir(), "agent.mp3")
	os.WriteFile(src, []byte("recorded-audio-bytes"), 0o644)

	key := "recordings/2026-11/call-9/agent.mp3"
	loc, err := m.Put(context.Background(), key, src)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if loc != fmt.Sprintf("s3://recordings/%s", key) {
		t.Errorf("locator = %q", loc)
	}
	// The stub sees the bucket-prefixed path (PUT /<bucket>/<key>).
	if v, ok := objects.Load("recordings/" + key); !ok {
		objects.Range(func(k, vv any) bool {
			t.Errorf("  stub has %q (len=%d)", k, len(vv.([]byte)))
			return true
		})
		t.Fatal("object missing")
	} else if string(v.([]byte)) != "recorded-audio-bytes" {
		t.Errorf("bytes mismatch: got %q", string(v.([]byte))[:30])
	}
	if err := m.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := objects.Load("recordings/" + key); ok {
		t.Error("delete did not remove")
	}
}

// decodeAWSChunked strips the aws-chunked framing from a request body.
func decodeAWSChunked(body []byte) []byte {
	var out []byte
	i := 0
	for i < len(body) {
		j := i
		for j < len(body) && body[j] != '\r' {
			j++
		}
		if j+1 >= len(body) {
			break
		}
		hexLen := body[i:j]
		if semi := indexByte(hexLen, ';'); semi >= 0 {
			hexLen = hexLen[:semi]
		}
		n := parseHex(hexLen)
		if n <= 0 {
			break
		}
		i = j + 2 // skip CRLF
		if i+n > len(body) {
			out = append(out, body[i:]...)
			break
		}
		out = append(out, body[i:i+n]...)
		i += n + 2 // data + CRLF
		if n == 0 {
			break
		}
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func parseHex(b []byte) int {
	n := 0
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			n = n*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n*16 + int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			n = n*16 + int(c-'A'+10)
		default:
			return -1
		}
	}
	return n
}
