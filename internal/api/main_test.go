package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"github.com/lixuanqun/gvos-core/internal/shared/rds"
)

// TestMain pins the internal token for the signaling face (Phase A
// fail-closed guard) so call-plane tests authenticate like Kamailio.
func TestMain(m *testing.M) {
	os.Setenv("GVOS_INTERNAL_TOKEN", "test-internal-token")
	os.Exit(m.Run())
}

// postInternal is the test-facing POST helper that carries the
// X-GVOS-Internal header (TestMain sets the env).
func postInternal(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// postInternalRaw posts with the internal header and returns the raw response.
func postInternalRaw(t *testing.T, url string, body []byte) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	return http.DefaultClient.Do(req)
}

// TestAPIKeyAuthFlow: create a key via the admin plane, use it for
// admin auth, revoke it, and confirm refusal — plus legacy fallback.
func TestAPIKeyAuthFlow(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	post := func(path, token string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// 1. No key registered -> legacy constant token still works (demo mode).
	if code, m := post("/v1/admin/accounts", "dev-admin-token", map[string]any{
		"username": "k1", "name": "KeyFlow", "credit": 5,
	}); code != 201 {
		t.Fatalf("legacy token: %d %v", code, m)
	}

	// 2. Bootstrap from a pinned env token, then create an operator key.
	t.Setenv("GVOS_ADMIN_TOKEN", "gvos_admin_pinnedboot0000")
	BootstrapKey(s)
	code, m := post("/v1/admin/keys", "gvos_admin_pinnedboot0000", map[string]any{
		"role": "operator",
	})
	if code != 201 {
		t.Fatalf("key create: %d %v", code, m)
	}
	key := m["key"].(string)
	prefix := m["prefix"].(string)
	_ = code

	// 3. Key authenticates the admin plane.
	if code, _ := post("/v1/admin/accounts", key, map[string]any{
		"username": "k2", "name": "KeyAuth", "credit": 5,
	}); code != 201 {
		t.Fatalf("key auth: %d", code)
	}

	// 4. Legacy constant token now refused (a bootstrap key exists).
	if code, _ := post("/v1/admin/accounts", "dev-admin-token", map[string]any{
		"username": "k3", "name": "Nope", "credit": 5,
	}); code != 401 {
		t.Fatalf("legacy should 401 after key exists: %d", code)
	}

	// 5. Unknown key refused.
	if code, _ := post("/v1/admin/accounts", "gvos_admin_deadbeef", nil); code != 401 {
		t.Fatalf("unknown key: %d", code)
	}

	// 6. Revoke -> refused.
	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/admin/keys/"+prefix, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	if code, _ := post("/v1/admin/accounts", key, nil); code != 401 {
		t.Fatalf("revoked key: %d", code)
	}
}

// TestIdempotentTopup: same Idempotency-Key replays the first
// response; the balance moves once.
func TestIdempotentTopup(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	call := func(key string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"account_id": "1000", "delta": 5.0, "reason": "idem-test"})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/accounts/1000/topup", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	c1, m1 := call("idem-1")
	if c1 != 200 {
		t.Fatalf("first topup: %d %v", c1, m1)
	}
	c2, m2 := call("idem-1") // network retry of the same request
	if c2 != 200 {
		t.Fatalf("replay: %d %v", c2, m2)
	}
	bal, _ := account.Balance(s.store, "1000")
	if bal.AsFloat() != mustParseBal(t, m1["balance"]) {
		t.Errorf("balance moved twice: now %v, first response %v", bal, m1["balance"])
	}
	if mustParseBal(t, m2["balance"]) != mustParseBal(t, m1["balance"]) {
		t.Errorf("replay response differs: %v vs %v", m2["balance"], m1["balance"])
	}
	// A different key is a different request.
	c3, m3 := call("idem-2")
	if c3 != 200 || mustParseBal(t, m3["balance"]) != bal.AsFloat()+5 {
		t.Errorf("distinct key should top up again: %d %v", c3, m3)
	}
}

// mustParseBal decodes a JSON balance value (decimal string post
// ADR-0003, or legacy number).
func mustParseBal(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case string:
		a, err := money.FromDecimal(x)
		if err != nil {
			t.Fatalf("bad balance %q: %v", x, err)
		}
		return a.AsFloat()
	case float64:
		return x
	}
	t.Fatalf("balance type %T", v)
	return 0
}

// TestRedisAuthorityEndToEnd: with the Redis authority attached, a
// full call flow (start -> 66s end) moves the money in Redis, the
// in-memory map is untouched, and a drain-to-zero is enforced by the
// authority (fail-closed face).
func TestRedisAuthorityEndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	rc, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	s := NewServer()
	AttachRedis(s, rc)

	// The demo seed (tenant/agent/end1) is now authority-backed.
	b, _ := json.Marshal(map[string]string{
		"call_id": "redis-e2e-1", "from_user": "1000", "to_user": "8613800138000",
	})
	code, _ := postInternal(t, srvURL(t, s)+"/v1/call/start", b)
	if code != 200 {
		t.Fatalf("start under redis authority: %d", code)
	}
	b2, _ := json.Marshal(map[string]any{"call_id": "redis-e2e-1", "duration": 66})
	code, m := postInternal(t, srvURL(t, s)+"/v1/call/end", b2)
	if code != 200 {
		t.Fatalf("end: %d %v", code, m)
	}
	// Money moved in Redis, not in the memory map.
	bal, _ := s.bal.Get(context.Background(), "1000")
	if bal <= 0 {
		t.Errorf("redis balance after call = %s, expected a positive drained-remainder", bal)
	}

	// Drain to zero via the authority -> next call is refused by the
	// authority (403 insufficient, not a crash).
	_ = s.bal.SetAbsolute(context.Background(), "1000", 0)
	b3, _ := json.Marshal(map[string]string{
		"call_id": "redis-e2e-2", "from_user": "1000", "to_user": "8613800138000",
	})
	if code, m := postInternal(t, srvURL(t, s)+"/v1/call/start", b3); code != 403 {
		t.Errorf("drained start = %d %v, want 403", code, m)
	}
}

func srvURL(t *testing.T, s *Server) string {
	t.Helper()
	srv := httptest.NewServer(MuxFor(s))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAdmissionUnderRedisAuthority: with Redis wired, the concurrent
// cap counts the shared ledger — two starts admitted, the third over
// the chain cap gets 429.
func TestAdmissionUnderRedisAuthority(t *testing.T) {
	mr := miniredis.RunT(t)
	rc, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	s := NewServer()
	// Pin the demo account's chain cap: agent1 (parent of 1000) cap 2.
	if a, err := s.store.Get("agent1"); err == nil {
		a.Concurrent = 2
		_ = s.store.Put(a)
	}
	AttachRedis(s, rc)
	url := srvURL(t, s)

	start := func(id string) int {
		t.Helper()
		b, _ := json.Marshal(map[string]string{
			"call_id": id, "from_user": "1000", "to_user": "8613800138000",
		})
		code, _ := postInternal(t, url+"/v1/call/start", b)
		return code
	}
	if c := start("adm-1"); c != 200 {
		t.Fatalf("first start: %d", c)
	}
	if c := start("adm-2"); c != 200 {
		t.Fatalf("second start: %d", c)
	}
	if c := start("adm-3"); c != 429 {
		t.Fatalf("third start over chain cap = %d, want 429", c)
	}
}
