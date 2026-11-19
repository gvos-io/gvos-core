// IP-allowlist admin API + snapshot endpoint + broadcast assertions.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lixuanqun/gvos-core/internal/customerdomain/ipallow"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
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
	if code, m := get("/v1/admin/ipallow", true); code != 200 || m["count"] != float64(0) {
		t.Fatalf("initial GET: %d %v", code, m)
	}

	// Auth guard: no token -> 401.
	if code, _ := post("/v1/admin/ipallow", map[string]any{"rules": []any{}}, false); code != 401 {
		t.Errorf("unauthenticated POST: %d, want 401", code)
	}

	// Validation: bad CIDR rejected with 400, state untouched.
	if code, m := post("/v1/admin/ipallow", map[string]any{"rules": []any{
		map[string]any{"id": "bad", "cidr": "10.0.0.0/99", "action": "allow"},
	}}, true); code != 400 {
		t.Errorf("bad CIDR accepted: %d %v", code, m)
	}

	// Valid replace: allow + deny (exception) rules land and broadcast.
	if code, m := post("/v1/admin/ipallow", map[string]any{"rules": []any{
		map[string]any{"id": "office", "cidr": "192.168.1.0/24", "action": "allow"},
		map[string]any{"id": "quarantine", "cidr": "192.168.1.66/32", "action": "deny"},
	}}, true); code != 200 || m["rules"] != float64(2) {
		t.Fatalf("replace: %d %v", code, m)
	}
	if code, m := get("/v1/admin/ipallow", true); code != 200 {
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
	if code, m := post("/v1/admin/ipallow", map[string]any{"remove_id": "quarantine"}, true); code != 200 {
		t.Fatalf("remove: %d %v", code, m)
	}
	if got := s.ipallow.Count(); got != 1 {
		t.Errorf("count after remove = %d, want 1", got)
	}
	// Unknown remove_id -> 404.
	if code, _ := post("/v1/admin/ipallow", map[string]any{"remove_id": "nope"}, true); code != 404 {
		t.Errorf("unknown remove: %d, want 404", code)
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
