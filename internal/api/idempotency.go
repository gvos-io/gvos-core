// Idempotency for money routes (api-conventions §Phase B):
// Idempotency-Key header + same-key replay returns the first
// response verbatim. In-memory registry; the PG mode later swaps in
// a table with the same shape (TTL 24h).
package api

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

type idemRecord struct {
	status int
	body   []byte
	at     time.Time
}

type idemStore struct {
	mu   sync.Mutex
	seen map[string]*idemRecord
}

func newIdemStore() *idemStore { return &idemStore{seen: map[string]*idemRecord{}} }

// withIdempotency wraps a money-route handler. Requests carrying an
// Idempotency-Key are replayed from the registry on conflict.
func (s *Server) withIdempotency(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		if s.idem == nil {
			next(w, r)
			return
		}
		s.idem.mu.Lock()
		if rec, ok := s.idem.seen[key]; ok {
			s.idem.mu.Unlock()
			// Replay: first status + body, marked idempotent-replay.
			w.Header().Set("X-Idempotent-Replay", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.body)
			return
		}
		s.idem.seen[key] = &idemRecord{at: time.Now()} // reserve the slot
		s.idem.mu.Unlock()

		cw := &captureWriter{header: http.Header{}, buf: &bytes.Buffer{}}
		next(cw, r)

		s.idem.mu.Lock()
		s.idem.seen[key] = &idemRecord{status: cw.status, body: cw.buf.Bytes(), at: time.Now()}
		s.idem.mu.Unlock()

		for k, vs := range cw.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(cw.status)
		_, _ = w.Write(cw.buf.Bytes())
	}
}

// captureWriter records the handler's response for idempotent replay.
type captureWriter struct {
	header http.Header
	buf    *bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }

// pruneIdem drops records older than the TTL (called lazily).
func (s *Server) pruneIdem(ttl time.Duration) {
	if s.idem == nil {
		return
	}
	s.idem.mu.Lock()
	defer s.idem.mu.Unlock()
	for k, rec := range s.idem.seen {
		if time.Since(rec.at) > ttl {
			delete(s.idem.seen, k)
		}
	}
}
