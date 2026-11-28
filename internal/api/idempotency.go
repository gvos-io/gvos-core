// Idempotency for money routes (api-conventions §Phase B):
// Idempotency-Key header + same-key replay returns the first
// response verbatim. Keys are namespaced by the concrete request
// (method + path): the flat global namespace used to cross-replay —
// the same client key sent to two different routes replayed the wrong
// response. PG mode stores reservations and responses in the
// idempotency_keys table (migration 014) so replay survives restarts
// and works across replicas; memory mode is the demo/test fallback
// with the same shape. TTL 24h (hourly purge on PG; lazy prune here).
package api

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type idemRecord struct {
	status      int
	body        []byte
	contentType string
	at          time.Time
	inFlight    bool
	done        chan struct{} // closed when the first request finishes
}

type idemStore struct {
	mu        sync.Mutex
	seen      map[string]*idemRecord
	lastPrune time.Time
}

func newIdemStore() *idemStore { return &idemStore{seen: map[string]*idemRecord{}} }

const idemTTL = 24 * time.Hour

// idemFullKey namespaces the client key by the concrete request so a
// header collision across routes (or across resource ids) cannot
// replay a foreign response.
func idemFullKey(r *http.Request, clientKey string) string {
	return r.Method + " " + r.URL.Path + "|" + clientKey
}

// idemReplay writes a stored response verbatim.
func idemReplay(w http.ResponseWriter, status int, body []byte, contentType string) {
	w.Header().Set("X-Idempotent-Replay", "true")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// withIdempotency wraps a money-route handler. Requests carrying an
// Idempotency-Key are replayed from the registry on conflict.
func (s *Server) withIdempotency(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientKey := r.Header.Get("Idempotency-Key")
		if clientKey == "" || s.idem == nil {
			next(w, r)
			return
		}
		full := idemFullKey(r, clientKey)

		// PG mode: the table is the shared registry (survives restarts,
		// converges replicas). Degrade to the memory path on PG errors
		// — process-local idempotency beats none.
		if s.pg != nil {
			if s.idemViaPG(w, r, next, full) {
				return
			}
		}

		s.pruneIdem(idemTTL)
		s.idem.mu.Lock()
		rec, ok := s.idem.seen[full]
		if ok && rec.inFlight {
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
			rec, ok = s.idem.seen[full]
			if !ok || rec.inFlight {
				s.idem.mu.Unlock()
				httpErrorC(w, 409, "IDEMPOTENCY_IN_FLIGHT", "request with this key is still processing")
				return
			}
			status, body, ct := rec.status, append([]byte(nil), rec.body...), rec.contentType
			s.idem.mu.Unlock()
			idemReplay(w, status, body, ct)
			return
		}
		if ok {
			status, body, ct := rec.status, append([]byte(nil), rec.body...), rec.contentType
			s.idem.mu.Unlock()
			idemReplay(w, status, body, ct)
			return
		}
		rec = &idemRecord{at: time.Now(), inFlight: true, done: make(chan struct{})}
		s.idem.seen[full] = rec
		s.idem.mu.Unlock()

		cw := &captureWriter{header: http.Header{}, buf: &bytes.Buffer{}, status: 200}
		next(cw, r)

		ct := cw.header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		s.idem.mu.Lock()
		rec.status = cw.status
		rec.body = cw.buf.Bytes()
		rec.contentType = ct
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

// idemViaPG runs the PG-backed idempotency flow. Returns false when
// PG is unusable (caller falls back to memory).
func (s *Server) idemViaPG(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, full string) bool {
	ctx := r.Context()
	reserved, err := s.pg.ReserveIdemKey(ctx, full)
	if err != nil {
		log.Printf("idempotency reserve failed, falling back to memory: %v", err)
		return false
	}
	if !reserved {
		// Someone holds it: poll for the stored first response.
		deadline := time.After(30 * time.Second)
		for {
			select {
			case <-ctx.Done():
				httpErrorC(w, 499, "CLIENT_CLOSED", "client closed while waiting for idempotent peer")
				return true
			case <-deadline:
				httpErrorC(w, 409, "IDEMPOTENCY_IN_FLIGHT", "request with this key is still processing")
				return true
			case <-time.After(100 * time.Millisecond):
			}
			status, body, ct, done, err := s.pg.LookupIdemKey(ctx, full)
			if err != nil {
				// Row lost under us (purge race) — treat as free.
				if strings.Contains(err.Error(), "no rows") {
					return false
				}
				log.Printf("idempotency lookup failed, falling back to memory: %v", err)
				return false
			}
			if done {
				idemReplay(w, status, []byte(body), ct)
				return true
			}
		}
	}
	// We own the reservation: run the handler and store the response.
	cw := &captureWriter{header: http.Header{}, buf: &bytes.Buffer{}, status: 200}
	next(cw, r)
	ct := cw.header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	if err := s.pg.CompleteIdemKey(ctx, full, cw.status, cw.buf.String(), ct); err != nil {
		log.Printf("idempotency store failed (replay for this key will 409/miss): %v", err)
	}
	for k, vs := range cw.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(cw.status)
	_, _ = w.Write(cw.buf.Bytes())
	return true
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

// pruneIdem drops records older than the TTL. Amortized: at most one
// full-map scan per minute instead of one per keyed request.
func (s *Server) pruneIdem(ttl time.Duration) {
	if s.idem == nil {
		return
	}
	s.idem.mu.Lock()
	defer s.idem.mu.Unlock()
	if !s.idem.lastPrune.IsZero() && time.Since(s.idem.lastPrune) < time.Minute {
		return
	}
	s.idem.lastPrune = time.Now()
	for k, rec := range s.idem.seen {
		if rec.inFlight {
			continue
		}
		if time.Since(rec.at) > ttl {
			delete(s.idem.seen, k)
		}
	}
}
