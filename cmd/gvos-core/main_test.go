package main

import (
	"bytes"
	"fmt"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gvos/gvos-core/internal/account"
)

// TestEndToEndCallFlow simulates the exact Kamailio event sequence for
// one call: INVITE (start) -> 200 (answer) -> BYE (end with duration),
// and verifies the billing invariants end to end.
func TestEndToEndCallFlow(t *testing.T) {
	s := NewServer()
	// Deterministic clock.
	base := time.Unix(1_700_000_000, 0)
	cur := base
	s.calls.SetClock(func() time.Time { return cur })

	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// 1. INVITE to a CN mobile number (prefix 8613, 0.05/min, 60+6).
	code, m := post("/v1/call/start", map[string]string{
		"call_id": "call-e2e-1", "from_user": "1000", "to_user": "8613800138000",
	})
	if code != 200 {
		t.Fatalf("start: code=%d body=%v", code, m)
	}
	if m["rate_prefix"] != "8613" {
		t.Errorf("prefix = %v, want 8613", m["rate_prefix"])
	}

	// 2. Answer.
	code, _ = post("/v1/call/answer", map[string]string{"call_id": "call-e2e-1"})
	if code != 200 {
		t.Fatalf("answer: code=%d", code)
	}

	// 3. Call runs 66 seconds -> 66 billed secs, cost 0.055.
	cur = base.Add(66 * time.Second)
	code, cdr := post("/v1/call/end", map[string]any{"call_id": "call-e2e-1", "duration": 66})
	if code != 200 {
		t.Fatalf("end: code=%d body=%v", code, cdr)
	}
	if cdr["BilledSecs"] != float64(66) {
		t.Errorf("billedSecs = %v, want 66", cdr["BilledSecs"])
	}
	if cdr["Cost"] != 0.055 {
		t.Errorf("cost = %v, want 0.055", cdr["Cost"])
	}

	// Balance invariant: start lock 0.05 (60s estimate), final cost 0.055.
	bal, _ := account.Balance(s.store, "1000")
	if !approxEqual(bal, 10-0.055, 1e-7) {
		t.Errorf("balance after call = %.7f, want %.7f", bal, 10-0.055)
	}

	// Stats reflect the CDR.
	code, stats := get(t, srv.URL+"/v1/stats")
	if code != 200 || stats["cdrs"] != float64(1) || stats["live_calls"] != float64(0) {
		t.Errorf("stats = %v", stats)
	}
}

// TestInsufficientBalance403 verifies the PRD acceptance: an account
// that cannot afford the estimate gets a 403 on INVITE.
func TestInsufficientBalance403(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	// Drain the caller: overwrite with a near-empty balance.
	s.store.Put(&account.Account{ID: "1000", ParentID: "agent1", Credit: 0.001, RateGroup: "demo"})

	b, _ := json.Marshal(map[string]string{
		"call_id": "call-x", "from_user": "1000", "to_user": "8613800138000",
	})
	resp, err := http.Post(srv.URL+"/v1/call/start", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("start code = %d, want 403", resp.StatusCode)
	}
}

// TestUnknownDestination404: unratable numbers are rejected cleanly.
func TestUnknownDestination404(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	b, _ := json.Marshal(map[string]string{
		"call_id": "call-y", "from_user": "1000", "to_user": "9999999",
	})
	resp, err := http.Post(srv.URL+"/v1/call/start", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("start code = %d, want 404", resp.StatusCode)
	}
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// approxEqual compares money-ish floats at 1e-7 (VOS precision).
func approxEqual(a, b, tol float64) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}

// TestBlocklistRejectsCall: a blacklisted destination is 403-rejected
// before rating/billing; removing the rule restores the call.
func TestBlocklistRejectsCall(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	call := func(dest string) int {
		b, _ := json.Marshal(map[string]string{
			"call_id": "bl-" + dest, "from_user": "1000", "to_user": dest,
		})
		resp, err := http.Post(srv.URL+"/v1/call/start", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Baseline: the number is routable.
	if code := call("8613800138000"); code != 200 {
		t.Fatalf("baseline call: %d", code)
	}

	// Blacklist the prefix via the admin API.
	b, _ := json.Marshal(map[string]any{"kind": "black", "scope": "dest", "pattern": "86139"})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/blocklist", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("blocklist add: %v code=%d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// Calls to the blacklisted range now 403.
	if code := call("8613912345678"); code != 403 {
		t.Errorf("blacklisted call: %d, want 403", code)
	}
	// Unaffected numbers still work.
	if code := call("8613800138000"); code != 200 {
		t.Errorf("non-matching call: %d, want 200", code)
	}

	// Remove the rule; the range is callable again.
	rb, _ := json.Marshal(map[string]any{"kind": "black", "scope": "dest", "pattern": "86139", "remove": true})
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/admin/blocklist", bytes.NewReader(rb))
	req2.Header.Set("Authorization", "Bearer "+adminToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("blocklist remove: %v code=%d", err, resp2.StatusCode)
	}
	resp2.Body.Close()

	if code := call("8613912345678"); code != 200 {
		t.Errorf("call after rule removal: %d, want 200", code)
	}
}

// TestRiskEngineBlocksBurst: a freq rule set via the admin API blocks
// the burst caller mid-stream while a normal-rate caller continues.
func TestRiskEngineBlocksBurst(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	// Install a 3/min frequency block rule.
	body, _ := json.Marshal(map[string]any{
		"rules": []map[string]any{{"id": "burst", "kind": "freq", "action": "block", "limit": 3}},
	})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/risk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("risk rules set: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	call := func(id string) int {
		b, _ := json.Marshal(map[string]string{
			"call_id": id, "from_user": "1000", "to_user": "8613800138000",
		})
		r, err := http.Post(srv.URL+"/v1/call/start", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	for i := 1; i <= 3; i++ {
		if code := call(fmt.Sprintf("r-%d", i)); code != 200 {
			t.Fatalf("call %d: %d, want 200", i, code)
		}
	}
	// 4th call inside the window: risk-blocked 403.
	if code := call("r-4"); code != 403 {
		t.Errorf("burst call: %d, want 403", code)
	}
	// Rate/billing paths still work for other destinations.
	if code := call("r-5"); code != 403 {
		t.Errorf("window overflow: %d, want 403", code)
	}
}
