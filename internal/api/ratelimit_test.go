package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterBurstAndRefill(t *testing.T) {
	rl := newRateLimiter(5, 2) // 5 burst, 2/s refill
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !rl.allow("k", now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("burst %d rejected", i)
		}
	}
	if rl.allow("k", now.Add(5*time.Millisecond)) {
		t.Fatal("6th request within burst must be rejected")
	}
	// Refill: 1s later 2 more tokens.
	if !rl.allow("k", now.Add(time.Second)) {
		t.Fatal("refill after 1s failed")
	}
	if !rl.allow("k", now.Add(time.Second+time.Millisecond)) {
		t.Fatal("second refilled token rejected")
	}
	// Separate key has its own bucket.
	if !rl.allow("other", now) {
		t.Fatal("independent bucket contaminated")
	}
}

func TestWithRateLimit429AndRetryAfter(t *testing.T) {
	t.Setenv("GVOS_RATE_BURST", "3")
	t.Setenv("GVOS_RATE_RPS", "1")
	s := NewServer()
	h := s.MuxForTest()
	probe := func() int {
		req := httptest.NewRequest("GET", "/v1/admin/rates", nil)
		req.RemoteAddr = "10.9.9.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if c := probe(); c == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i)
		}
	}
	if c := probe(); c != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", c)
	}
	// Retry-After present and positive.
	req := httptest.NewRequest("GET", "/v1/admin/rates", nil)
	req.RemoteAddr = "10.9.9.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

func TestWithRateLimitDisabled(t *testing.T) {
	t.Setenv("GVOS_RATE_BURST", "0")
	s := NewServer()
	h := s.MuxForTest()
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("GET", "/v1/admin/rates", nil)
		req.RemoteAddr = "10.9.9.8:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("limiter active while disabled (request %d)", i)
		}
	}
}

// The signaling plane must never be token-bucketed: its natural rate
// IS the CPS admission gate and a limiter here would shadow the
// billing gate's own 429s (regression: the limiter used to wrap the
// whole mux, clamping /v1/call/* to ~20 rps per IP).
func TestRateLimitSkipsSignalingPlane(t *testing.T) {
	t.Setenv("GVOS_RATE_BURST", "1")
	t.Setenv("GVOS_RATE_RPS", "1")
	s := NewServer()
	h := s.MuxForTest()
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest("GET", "/v1/stats", nil)
		req.RemoteAddr = "10.9.9.7:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("signaling plane request %d was rate-limited", i)
		}
	}
	// Control: the admin plane on the same mux does limit.
	req := httptest.NewRequest("GET", "/v1/admin/rates", nil)
	req.RemoteAddr = "10.9.9.7:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// First admin hit may pass (fresh bucket); exhaust it.
	for i := 0; i < 3 && rec.Code != http.StatusTooManyRequests; i++ {
		req = httptest.NewRequest("GET", "/v1/admin/rates", nil)
		req.RemoteAddr = "10.9.9.7:1234"
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("admin plane never limited (last code %d)", rec.Code)
	}
}

// Keying: two callers with different bearer tokens get independent
// buckets; a caller without a token is keyed by IP.
func TestClientKeyBearerSeparation(t *testing.T) {
	mk := func(tok string) *http.Request {
		req := httptest.NewRequest("GET", "/v1/admin/rates", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return req
	}
	if clientKey(mk("gvos_admin_aaaa")) == clientKey(mk("gvos_admin_bbbb")) {
		t.Fatal("different bearer tokens share one bucket")
	}
	if clientKey(mk("gvos_admin_aaaa")) != clientKey(mk("gvos_admin_aaaa")) {
		t.Fatal("same bearer token must map to one stable bucket")
	}
	if clientKey(mk("")) != clientKey(mk("")) {
		t.Fatal("tokenless callers must share the IP bucket")
	}
}
