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
	status    int
	body      []byte
	at        time.Time
	inFlight  bool
	done      chan struct{} // closed when the first request finishes
}

type idemStore struct {
	mu   sync.Mutex
	seen map[string]*idemRecord
}

func newIdemStore() *idemStore { return &idemStore{seen: map[string]*idemRecord{}} }

const idemTTL = 24 * time.Hour

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
		s.pruneIdem(idemTTL)

		s.idem.mu.Lock()
		if rec, ok := s.idem.seen[key]; ok {
			if rec.inFlight {
				done := rec.done
				s.idem.mu.Unlock()
				// Wait for the in-flight request to finish, then replay.
				select {
				case <-done:
				case <-r.Context().Done():
					httpErrorC(w, 499, "CLIENT_CLOSED", "client closed while waiting for idempotent peer")
					return
				case <-time.After(30 * time.Second):
					httpErrorC(w, 409, "IDEMPOTENCY_IN_FLIGHT", "request with this key is still processing")
					return
				}
				s.idem.mu.Lock()
				rec, ok = s.idem.seen[key]
				if !ok || rec.inFlight {
					s.idem.mu.Unlock()
					httpErrorC(w, 409, "IDEMPOTENCY_IN_FLIGHT", "request with this key is still processing")
					return
				}
				status, body := rec.status, append([]byte(nil), rec.body...)
				s.idem.mu.Unlock()
				w.Header().Set("X-Idempotent-Replay", "true")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(body)
				return
			}
			status, body := rec.status, append([]byte(nil), rec.body...)
			s.idem.mu.Unlock()
			w.Header().Set("X-Idempotent-Replay", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		rec := &idemRecord{at: time.Now(), inFlight: true, done: make(chan struct{})}
		s.idem.seen[key] = rec
		s.idem.mu.Unlock()

		cw := &captureWriter{header: http.Header{}, buf: &bytes.Buffer{}, status: 200}
		next(cw, r)

		s.idem.mu.Lock()
		rec.status = cw.status
		rec.body = cw.buf.Bytes()
		rec.at = time.Now()
		rec.inFlight = false
		close(rec.done)
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
		if rec.inFlight {
			continue
		}
		if time.Since(rec.at) > ttl {
			delete(s.idem.seen, k)
		}
	}
}
