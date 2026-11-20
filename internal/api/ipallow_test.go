// IP-allowlist admin API + snapshot endpoint + broadcast assertions.
package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fastvoip/gvos-core/internal/customerdomain/ipallow"
	"github.com/fastvoip/gvos-core/internal/shared/events"
)

func TestIPAllowAdminAPI(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)

	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	post := func(path string, body any, auth bool) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if auth {
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	get := func(path string, auth bool) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// Empty list = default-deny: no rules loaded, engine rejects.
	if code, m := get("/v1/admin/ipallow/rules", true); code != 200 || m["count"] != float64(0) {
		t.Fatalf("initial GET: %d %v", code, m)
	}

	// Auth guard: no token -> 401.
	if code, _ := post("/v1/admin/ipallow/rules", map[string]any{"rules": []any{}}, false); code != 401 {
		t.Errorf("unauthenticated POST: %d, want 401", code)
	}

	// Validation: bad CIDR rejected with 400, state untouched.
	if code, m := post("/v1/admin/ipallow/rules", map[string]any{"rules": []any{
		map[string]any{"id": "bad", "cidr": "10.0.0.0/99", "action": "allow"},
	}}, true); code != 400 {
		t.Errorf("bad CIDR accepted: %d %v", code, m)
	}

	// Valid replace: allow + deny (exception) rules land and broadcast.
	if code, m := post("/v1/admin/ipallow/rules", map[string]any{"rules": []any{
		map[string]any{"id": "office", "cidr": "192.168.1.0/24", "action": "allow"},
		map[string]any{"id": "quarantine", "cidr": "192.168.1.66/32", "action": "deny"},
	}}, true); code != 200 || m["rules"] != float64(2) {
		t.Fatalf("replace: %d %v", code, m)
	}
	if code, m := get("/v1/admin/ipallow/rules", true); code != 200 {
		t.Fatal(code)
	} else {
		rules := m["rules"].([]any)
		if len(rules) != 2 {
			t.Fatalf("GET rules = %v", rules)
		}
	}
	// Engine state follows the admin write (the handler is the source).
	if got := s.ipallow.Count(); got != 2 {
		t.Errorf("in-memory count = %d, want 2", got)
	}
	flush(s)
	subjects, bodies := cap.snapshot()
	broadcasts := 0
	for i, name := range subjects {
		if name == events.SubjectCfgReload && bodies[i]["entity"] == "ip_allowlist" {
			broadcasts++
			rules, ok := bodies[i]["rules"].([]any)
			if !ok || len(rules) != 2 {
				t.Errorf("broadcast payload rules = %v", bodies[i]["rules"])
			}
		}
	}
	if broadcasts != 1 {
		t.Errorf("ip_allowlist broadcasts = %d, want 1", broadcasts)
	}

	// remove_id removes exactly one rule.
	if code, m := post("/v1/admin/ipallow/rules", map[string]any{"remove_id": "quarantine"}, true); code != 200 {
		t.Fatalf("remove: %d %v", code, m)
	}
	if got := s.ipallow.Count(); got != 1 {
		t.Errorf("count after remove = %d, want 1", got)
	}
	// Unknown remove_id -> 404.
	if code, _ := post("/v1/admin/ipallow/rules", map[string]any{"remove_id": "nope"}, true); code != 404 {
		t.Errorf("unknown remove: %d, want 404", code)
	}
}

// TestIPAllowDeleteRoute: the resource DELETE path must behave exactly
// like the remove_id form — memory updated, one broadcast emitted, and
// the persisted shape must be the plain rules array ([]ipallow.Rule),
// because NewPGServer restores the setting by unmarshalling into that
// type (a broadcast-shaped map survives PG but fails the restore, and
// the empty result default-denies every SIP source after a restart).
// PG is nil here, so the persistence contract is pinned by marshalling
// what this handler would hand PutSetting.
func TestIPAllowDeleteRoute(t *testing.T) {
	s := NewServer()
	s.ipallow.Reload([]ipallow.Rule{
		{ID: "office", CIDR: "192.168.1.0/24", Action: "allow"},
		{ID: "quarantine", CIDR: "192.168.1.66/32", Action: "deny"},
		{ID: "vpn", CIDR: "10.8.0.0/16", Action: "allow"},
	})
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()
	defer flush(s)

	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/admin/ipallow/rules/quarantine", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("DELETE: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	if got := s.ipallow.Count(); got != 2 {
		t.Fatalf("count after DELETE = %d, want 2", got)
	}
	// The deny exception is gone: 192.168.1.66 falls back to the
	// covering allow rule ("office"), which is the point of deleting it.
	if allowed, matched := s.ipallow.Check(net.ParseIP("192.168.1.66")); !allowed || matched != "office" {
		t.Errorf("after deleting the deny rule: allowed=%v matched=%q, want true/office", allowed, matched)
	}
	if allowed, _ := s.ipallow.Check(net.ParseIP("10.8.0.1")); !allowed {
		t.Error("unrelated allow rule lost by DELETE")
	}

	// Exactly one broadcast, carrying the full kept set (Kamailio
	// IPALLOW_APPLY and the 60s rebroadcaster both rely on it).
	subjects, bodies := cap.snapshot()
	broadcasts := 0
	for i, name := range subjects {
		if name == events.SubjectCfgReload && bodies[i]["entity"] == "ip_allowlist" {
			broadcasts++
			rules, ok := bodies[i]["rules"].([]any)
			if !ok || len(rules) != 2 {
				t.Errorf("broadcast payload rules = %v", bodies[i]["rules"])
			}
		}
	}
	if broadcasts != 1 {
		t.Errorf("ip_allowlist broadcasts = %d, want 1", broadcasts)
	}

	// Persistence contract: what this handler persists must round-trip
	// through the restore type. The handler keeps kept as
	// []ipallow.Rule — assert that shape directly (PG is nil in-memory;
	// this is the value PutSetting would marshal).
	var restored []ipallow.Rule
	if b, err := json.Marshal(s.ipallow.Rules()); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatalf("persisted shape does not unmarshal into []ipallow.Rule: %v", err)
	} else if len(restored) != 2 {
		t.Fatalf("restored rules = %d, want 2", len(restored))
	}

	// Unknown id -> 404, nothing broadcast.
	beforeSubjects, _ := cap.snapshot()
	before := len(beforeSubjects)
	req2, _ := http.NewRequest("DELETE", srv.URL+"/v1/admin/ipallow/rules/nope", nil)
	req2.Header.Set("Authorization", "Bearer "+adminToken)
	resp2, err := srv.Client().Do(req2)
	if err != nil || resp2.StatusCode != 404 {
		t.Fatalf("DELETE unknown: %v %d", err, resp2.StatusCode)
	}
	resp2.Body.Close()
	if afterSubjects, _ := cap.snapshot(); len(afterSubjects) != before {
		t.Errorf("unknown DELETE broadcast (%d -> %d)", before, len(afterSubjects))
	}
}

// TestIPAllowEmptyGuard: emptying a non-empty allowlist needs
// confirm_empty:true on all three write paths — POST replace,
// POST remove_id, and the resource DELETE. Without it a slip of the
// hand is a full-network SIP outage in seconds (default-deny is
// broadcast to the edge immediately). Confirmed empties succeed;
// empty->empty and non-empty writes are untouched.
func TestIPAllowEmptyGuard(t *testing.T) {
	s := NewServer()
	s.ipallow.Reload([]ipallow.Rule{
		{ID: "last", CIDR: "192.168.1.0/24", Action: "allow"},
	})
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()
	defer flush(s)

	post := func(body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/ipallow/rules", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	delete := func(id string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("DELETE", srv.URL+"/v1/admin/ipallow/rules/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// 1. Full-set replace to empty without confirm -> 409, state kept.
	if code, m := post(`{"rules":[]}`); code != 409 {
		t.Fatalf("replace-to-empty unconfirmed: %d %v, want 409", code, m)
	}
	if got := s.ipallow.Count(); got != 1 {
		t.Fatalf("state after rejected replace: %d rules, want 1", got)
	}

	// 2. remove_id that would empty the set -> 409.
	if code, m := post(`{"remove_id":"last"}`); code != 409 {
		t.Fatalf("remove-last unconfirmed: %d %v, want 409", code, m)
	}
	if got := s.ipallow.Count(); got != 1 {
		t.Fatalf("state after rejected remove: %d rules, want 1", got)
	}

	// 3. DELETE the last rule -> 409 (no confirm field on this route:
	// the message points at the POST form).
	if code, m := delete("last"); code != 409 {
		t.Fatalf("DELETE last unconfirmed: %d %v, want 409", code, m)
	}
	if got := s.ipallow.Count(); got != 1 {
		t.Fatalf("state after rejected DELETE: %d rules, want 1", got)
	}

	// No broadcast from any rejected write.
	subjects, bodies := cap.snapshot()
	for i, name := range subjects {
		if name == events.SubjectCfgReload && bodies[i]["entity"] == "ip_allowlist" {
			t.Errorf("rejected write still broadcast: %v", bodies[i])
		}
	}

	// 4. Confirmed empty replace succeeds and lands.
	if code, m := post(`{"rules":[],"confirm_empty":true}`); code != 200 {
		t.Fatalf("confirmed empty replace: %d %v, want 200", code, m)
	}
	if got := s.ipallow.Count(); got != 0 {
		t.Fatalf("count after confirmed empty: %d, want 0", got)
	}
	subjects, bodies = cap.snapshot()
	broadcast := 0
	for i, name := range subjects {
		if name == events.SubjectCfgReload && bodies[i]["entity"] == "ip_allowlist" {
			broadcast++
		}
	}
	if broadcast != 1 {
		t.Errorf("confirmed empty broadcasts = %d, want 1", broadcast)
	}

	// 5. Empty->empty replace is a no-op, not a guard trip (already
	// empty: nothing to lose, no confirmation needed).
	if code, m := post(`{"rules":[]}`); code != 200 {
		t.Fatalf("empty->empty replace: %d %v, want 200", code, m)
	}

	// 6. Non-empty replace never trips the guard.
	if code, m := post(`{"rules":[{"id":"a","cidr":"10.0.0.0/8","action":"allow"}]}`); code != 200 {
		t.Fatalf("non-empty replace: %d %v, want 200", code, m)
	}
	if code, m := post(`{"remove_id":"a"}`); code != 409 {
		t.Fatalf("remove-last again unconfirmed: %d %v, want 409", code, m)
	}
	if code, m := post(`{"remove_id":"a","confirm_empty":true}`); code != 200 {
		t.Fatalf("confirmed remove-last: %d %v, want 200", code, m)
	}
	if got := s.ipallow.Count(); got != 0 {
		t.Fatalf("final count = %d, want 0", got)
	}
}

func TestIPAllowSnapshotEndpoint(t *testing.T) {
	s := NewServer()
	// Configure one allow rule directly (engine API — the admin API is
	// covered above).
	s.ipallow.Reload([]ipallow.Rule{
		{ID: "a", CIDR: "10.0.0.0/8", Action: "allow"},
	})

	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	// Pinned token (TestMain), no header -> 401 (never open).
	if code, body := get(t, srv.URL+"/v1/internal/ipallow-snapshot"); code != 401 {
		t.Errorf("no-header snapshot: %d %v, want 401", code, body)
	}

	t.Setenv("GVOS_INTERNAL_TOKEN", "secret-internal")
	// Wrong token -> 401.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/internal/ipallow-snapshot", nil)
	req.Header.Set("X-GVOS-Internal", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("wrong token: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// Right token -> versioned snapshot.
	req2, _ := http.NewRequest("GET", srv.URL+"/v1/internal/ipallow-snapshot", nil)
	req2.Header.Set("X-GVOS-Internal", "secret-internal")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("snapshot: %v %d", err, resp2.StatusCode)
	}
	var snap map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&snap)
	resp2.Body.Close()
	if snap["count"] != float64(1) {
		t.Errorf("snapshot count = %v", snap["count"])
	}
	ver, _ := snap["version"].(string)
	if len(ver) != 16 { // fnv64 hex
		t.Errorf("snapshot version = %q, want 16-hex", ver)
	}

	// Non-GET -> 405.
	req3, _ := http.NewRequest("POST", srv.URL+"/v1/internal/ipallow-snapshot", nil)
	req3.Header.Set("X-GVOS-Internal", "secret-internal")
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3 != nil && resp3.StatusCode != 405 {
		t.Errorf("POST snapshot: %d, want 405", resp3.StatusCode)
	}
	if resp3 != nil {
		resp3.Body.Close()
	}
}
