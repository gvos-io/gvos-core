package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	body := []byte(`{"call_id":"c-1"}`)
	hdr := Sign("sec1", time.Now(), body)
	if !Verify("sec1", hdr, body, 10*time.Minute) {
		t.Fatal("round trip failed")
	}
	if Verify("sec2", hdr, body, 10*time.Minute) {
		t.Error("wrong secret must fail")
	}
	if Verify("sec1", hdr, []byte("tampered"), 10*time.Minute) {
		t.Error("tampered body must fail")
	}
	// Replay outside the skew window fails.
	old := Sign("sec1", time.Now().Add(-time.Hour), body)
	if Verify("sec1", old, body, 10*time.Minute) {
		t.Error("stale timestamp must fail")
	}
}

func TestNextRetrySchedule(t *testing.T) {
	// Attempt 1 failed -> retry after Backoff[0]; attempt 5 failed -> dead.
	if d, ok := NextRetry(1); !ok || d != time.Minute {
		t.Errorf("attempt1: %v %v", d, ok)
	}
	if _, ok := NextRetry(5); ok {
		t.Error("attempt 5 must dead-letter")
	}
}

// TestDeliverRetriesThenSucceeds: a flapping endpoint (fails twice,
// then 200) is delivered on attempt 3 — the schedule keeps driving.
func TestDeliverRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	// Contract-scaled backoff is minutes; for the test we shrink it.
	saved := Backoff
	Backoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	defer func() { Backoff = saved }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(500)
			return
		}
		if !Verify("sec9", r.Header.Get("X-GVOS-Signature"), readBody(r), time.Minute) {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDeliverer()
	// The endpoint is a loopback test server, which the deliverer's
	// dial-time SSRF guard refuses by design. This test is about the
	// retry schedule (transport-independent), so it swaps in a plain
	// transport; the guard has its own tests in deliver_guard_test.go.
	d.Client.Transport = nil
	sub := Subscription{ID: "s1", URL: srv.URL, Secret: "sec9", Events: []string{"gvos.cdr.completed"}, Active: true}
	attempts := d.Deliver(context.Background(), sub, "gvos.cdr.completed", 7, map[string]string{"call_id": "c-1"})
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if calls.Load() != 3 {
		t.Errorf("endpoint calls = %d, want 3", calls.Load())
	}
}

func readBody(r *http.Request) []byte {
	b := make([]byte, 4096)
	n, _ := r.Body.Read(b)
	return b[:n]
}
