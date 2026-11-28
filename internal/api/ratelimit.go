// ratelimit.go: per-caller token bucket for the admin/tenant planes
// (pentest selfaudit leftover 1: "admin API 无速率限制" — brute-force
// 20/2s with no decay was possible on the login-ish endpoints).
// The signaling face is excluded on purpose: it is Kamailio-internal
// traffic whose natural rate IS the CPS admission path, and limiting
// it here would shadow the billing gate's own 429s.
//
// Keying: a hash of the presented bearer token when one is given
// (each key/token = its own bucket — parsed from the header directly,
// because the acl identity is only injected inside the handlers,
// after this middleware has already run), else the client IP.
// Unauthenticated callers share a hostile-world bucket per IP, so a
// distributed guess attack still costs one bucket per source.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rlBucket is one token bucket. refill is tokens per second; burst
// is the bucket depth (the maximum instantaneous burst).
type rlBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rlBucket
	refill  float64
	burst   float64

	lastSweep time.Time
}

// newRateLimiter: burst requests instantly, then sustained requests
// per second, sweeping idle buckets every 10 minutes so the map does
// not grow with source IPs.
func newRateLimiter(burst int, perSec float64) *rateLimiter {
	return &rateLimiter{
		buckets:   map[string]*rlBucket{},
		refill:    perSec,
		burst:     float64(burst),
		lastSweep: time.Now(),
	}
}

// allow reports whether one request passes.
func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &rlBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rl.refill
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep drops buckets idle past the horizon (caller holds no lock
// interplay: runs inside allow's sweep check window).
func (rl *rateLimiter) sweep(now time.Time) {
	horizon := 10 * time.Minute
	for k, b := range rl.buckets {
		if now.Sub(b.last) > horizon {
			delete(rl.buckets, k)
		}
	}
	rl.lastSweep = now
}

// rateLimitedPath reports whether the path belongs to an operator
// plane (admin/tenant). Only those are token-bucketed; signaling,
// internal, media, recording and health paths pass through untouched.
func rateLimitedPath(p string) bool {
	return strings.HasPrefix(p, "/v1/admin/") || strings.HasPrefix(p, "/v1/tenant/")
}

// withRateLimit wraps the whole mux but enforces only on the
// admin/tenant planes (rateLimitedPath). Limit defaults: 100 burst,
// 20 req/s sustained per key/IP (GVOS_RATE_BURST / GVOS_RATE_RPS
// override; 0/0 disables — bench and dev). 429 carries Retry-After so
// well-behaved clients back off instead of retrying harder (the
// opposite behavior would feed the storm it limits).
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	burst := envInt("GVOS_RATE_BURST", 100)
	rps := envFloat("GVOS_RATE_RPS", 20)
	if burst <= 0 || rps <= 0 {
		return next
	}
	rl := newRateLimiter(burst, rps)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rateLimitedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := clientKey(r)
		now := time.Now()
		rl.mu.Lock()
		doSweep := now.Sub(rl.lastSweep) > 10*time.Minute
		rl.mu.Unlock()
		if doSweep {
			rl.mu.Lock()
			rl.sweep(now)
			rl.mu.Unlock()
		}
		if !rl.allow(key, now) {
			retry := int(math.Ceil(1 / rps))
			w.Header().Set("Retry-After", itoa(retry))
			httpErrorC(w, 429, "RATE_LIMITED", "too many requests; retry after "+itoa(retry)+"s")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey: hash of the bearer token when one is presented (each
// key/token = its own bucket; the header is read directly because the
// acl identity is injected into the request only inside the handlers,
// after this middleware has run), else the source IP.
func clientKey(r *http.Request) string {
	const prefix = "Bearer "
	if auth := r.Header.Get("Authorization"); len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		if tok := strings.TrimSpace(auth[len(prefix):]); tok != "" {
			sum := sha256.Sum256([]byte(tok))
			return "key:" + hex.EncodeToString(sum[:6])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v != 0 {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
