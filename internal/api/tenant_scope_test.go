package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
	"github.com/gvos-io/gvos-core/internal/shared/store"
)

// Tenant-plane scoping, with real keys. The phase-C tests add keys
// under made-up hashes, so every lookup there misses and the scoping
// assertions never run.

func issueKey(s *Server, role acl.Role, tenantID string) string {
	plaintext, prefix, hash := acl.GenerateKey(role)
	s.acl.Add(acl.Key{ID: prefix, TenantID: tenantID, Role: role, Prefix: prefix, Hash: hash})
	return plaintext
}

func tenantGet(t *testing.T, s *Server, key, path string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	MuxFor(s).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ids extracts obj["id"] from a []any of row maps.
func ids(v any) map[string]bool {
	out := map[string]bool{}
	rows, _ := v.([]any)
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out[id] = true
			}
		}
	}
	return out
}

// A tenant's scope is its whole sub-tree. It used to stop at the
// direct children, so a tenant never saw the calls of its agents' own
// customers.
func TestTenantScopeIsTheWholeSubtree(t *testing.T) {
	s := NewServer()
	for _, a := range []*account.Account{
		{ID: "T", Postpaid: true},
		{ID: "A", ParentID: "T", Credit: money.MustFromYuan(1)},
		{ID: "U1", ParentID: "A", Credit: money.MustFromYuan(1)},
		{ID: "U2", ParentID: "T", Credit: money.MustFromYuan(1)},
	} {
		s.store.Put(a)
	}
	tenantKey := issueKey(s, acl.RoleTenant, "T")
	agentKey := issueKey(s, acl.RoleAgent, "A")
	u2Key := issueKey(s, acl.RoleTenant, "U2")

	code, me := tenantGet(t, s, tenantKey, "/v1/tenant/me", nil)
	if code != 200 {
		t.Fatalf("tenant /me: code = %d", code)
	}
	if n, _ := me["child_accounts"].(float64); int(n) != 3 {
		t.Errorf("child_accounts = %v, want 3 (A, U1, U2)", me["child_accounts"])
	}
	code, accs := tenantGet(t, s, tenantKey, "/v1/tenant/accounts", nil)
	if got := ids(accs["accounts"]); code != 200 || len(got) != 3 || !got["A"] || !got["U1"] || !got["U2"] {
		t.Errorf("tenant accounts: code = %d ids = %v, want A, U1, U2", code, got)
	}
	code, accs = tenantGet(t, s, agentKey, "/v1/tenant/accounts", nil)
	if got := ids(accs["accounts"]); code != 200 || len(got) != 1 || !got["U1"] {
		t.Errorf("agent accounts: code = %d ids = %v, want U1 only", code, got)
	}

	s.storeCdr(&callcore.CDR{CallID: "u1-call", AccountID: "U1"})
	s.storeCdr(&callcore.CDR{CallID: "u2-call", AccountID: "U2"})
	s.storeCdr(&callcore.CDR{CallID: "other-call", AccountID: "1000"})
	for _, tc := range []struct {
		name, key string
		want      map[string]bool
	}{
		{"tenant", tenantKey, map[string]bool{"u1-call": true, "u2-call": true}},
		{"agent", agentKey, map[string]bool{"u1-call": true}},
		{"U2", u2Key, map[string]bool{"u2-call": true}},
	} {
		code, out := tenantGet(t, s, tc.key, "/v1/tenant/cdrs", nil)
		got := map[string]bool{}
		if rows, ok := out["cdrs"].([]any); ok {
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					got[m["call_id"].(string)] = true
				}
			}
		}
		if code != 200 || len(got) != len(tc.want) {
			t.Errorf("%s cdrs: code = %d rows = %v, want %v", tc.name, code, got, tc.want)
		}
		for id := range got {
			if !tc.want[id] {
				t.Errorf("%s saw %s, outside its scope", tc.name, id)
			}
		}
	}
}

// The CDR cursor advances by rows scanned, not rows returned: a tenant
// with few calls among many used to get the same page back forever.
func TestTenantCDRPaginationTerminates(t *testing.T) {
	s := NewServer()
	s.store.Put(&account.Account{ID: "T", Postpaid: true})
	s.store.Put(&account.Account{ID: "U", ParentID: "T", Credit: money.MustFromYuan(1)})
	key := issueKey(s, acl.RoleTenant, "T")
	s.storeCdr(&callcore.CDR{CallID: "mine-a", AccountID: "U"})
	s.storeCdr(&callcore.CDR{CallID: "o1", AccountID: "1000"})
	s.storeCdr(&callcore.CDR{CallID: "mine-b", AccountID: "U"})
	s.storeCdr(&callcore.CDR{CallID: "o2", AccountID: "1000"})
	s.storeCdr(&callcore.CDR{CallID: "o3", AccountID: "1000"})

	seen := map[string]int{}
	path := "/v1/tenant/cdrs?limit=1"
	pages := 0
	for {
		code, out := tenantGet(t, s, key, path, nil)
		if code != 200 {
			t.Fatalf("page %d: code = %d", pages, code)
		}
		rows, _ := out["cdrs"].([]any)
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen[r.(map[string]any)["call_id"].(string)]++
		}
		hasMore, _ := out["has_more"].(bool)
		pages++
		if !hasMore || pages > 4 {
			break
		}
		path = "/v1/tenant/cdrs?limit=1&cursor=" + jsonNumber(out["cursor"])
	}
	if pages > 3 {
		t.Fatalf("pagination did not terminate: %d pages", pages)
	}
	if seen["mine-a"] != 1 || seen["mine-b"] != 1 || len(seen) != 2 {
		t.Errorf("rows over all pages = %v (pages %d), want mine-a and mine-b exactly once", seen, pages)
	}
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type fakeWebhookStore struct{ rows []store.WebhookRow }

func (f *fakeWebhookStore) CreateWebhook(context.Context, string, string, []string) (string, error) {
	return "", nil
}
func (f *fakeWebhookStore) CreateWebhookTenant(context.Context, string, string, []string, string) (string, error) {
	return "", nil
}
func (f *fakeWebhookStore) ListWebhooks(context.Context) ([]store.WebhookRow, error) {
	return f.rows, nil
}
func (f *fakeWebhookStore) DeleteWebhook(context.Context, string) (int, error) { return 0, nil }

// A tenant lists only its own webhook subscriptions. Operator-created
// ones (no tenant) used to be listed to every tenant, endpoints
// included.
func TestTenantWebhookListHidesOperatorSubscriptions(t *testing.T) {
	s := NewServer()
	s.whStore = &fakeWebhookStore{rows: []store.WebhookRow{
		{ID: "ops", URL: "https://ops.example/h", Active: true},
		{ID: "a", URL: "https://a.example/h", Active: true, TenantID: "tenant-a"},
		{ID: "b", URL: "https://b.example/h", Active: true, TenantID: "tenant-b"},
	}}
	key := issueKey(s, acl.RoleTenant, "tenant-a")
	code, out := tenantGet(t, s, key, "/v1/tenant/webhooks", nil)
	got := ids(out["webhooks"])
	if code != 200 || len(got) != 1 || !got["a"] {
		t.Errorf("tenant-a webhooks: code = %d ids = %v, want [a] only", code, got)
	}
}

// A platform key uses the tenant plane only when the trusted backend
// names the tenant; the named tenant becomes the key's scope.
func TestAdminKeyActsForTenantOnlyWhenNamed(t *testing.T) {
	s := NewServer()
	s.store.Put(&account.Account{ID: "T", Postpaid: true})
	admin := issueKey(s, acl.RoleAdmin, "")

	if code, _ := tenantGet(t, s, admin, "/v1/tenant/me", nil); code != 403 {
		t.Fatalf("admin key without the tenant header: code = %d, want 403", code)
	}
	code, me := tenantGet(t, s, admin, "/v1/tenant/me", map[string]string{TenantActHeader: "T"})
	if code != 200 {
		t.Fatalf("admin key acting for T: code = %d, want 200", code)
	}
	if acc, _ := me["account"].(map[string]any); acc == nil || acc["id"] != "T" {
		t.Errorf("acting scope = %v, want account T", me["account"])
	}
	// A tenant key cannot widen its own scope with the header.
	s.store.Put(&account.Account{ID: "other", Postpaid: true})
	tenant := issueKey(s, acl.RoleTenant, "other")
	code, me = tenantGet(t, s, tenant, "/v1/tenant/me", map[string]string{TenantActHeader: "T"})
	if code != 200 {
		t.Fatalf("tenant key: code = %d", code)
	}
	if acc, _ := me["account"].(map[string]any); acc == nil || acc["id"] != "other" {
		t.Errorf("tenant key's scope = %v, want other (the header is ignored for tenant keys)", me["account"])
	}
}
