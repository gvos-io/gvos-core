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
