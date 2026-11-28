// P1 regression tests (memory mode): account PATCH/DELETE lifecycle,
// webhook PATCH + tenant-scoped DELETE, package unassign, rate delete,
// directory pagination, and the PG-only guards on audit/ledger.
package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

func TestAccountPatchAndDeleteLifecycle(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	// Parent + child: the delete guard needs a child.
	rec := doAdmin(t, h, "POST", "/v1/admin/accounts", map[string]any{"username": "p1-parent", "initial_balance": "10"})
	var parent struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &parent)
	rec = doAdmin(t, h, "POST", "/v1/admin/accounts", map[string]any{"username": "p1-child", "parent_id": parent.ID})
	if rec.Code != 201 {
		t.Fatalf("child create = %d body=%s", rec.Code, rec.Body.String())
	}

	// PATCH the parent (memory path): knobs move, credit untouched.
	rec = doAdmin(t, h, "PATCH", "/v1/admin/accounts/"+parent.ID, map[string]any{
		"name": "Renamed", "max_cps": 7, "low_balance": 2.5,
	})
	if rec.Code != 200 {
		t.Fatalf("patch = %d body=%s", rec.Code, rec.Body.String())
	}
	var patched struct {
		Account struct {
			Name       string `json:"name"`
			CPS        int    `json:"cps"`
			Credit     string `json:"credit"` // money.Amount is a decimal string on the wire
			LowBalance string `json:"low_balance"`
		} `json:"account"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	lb, _ := strconv.ParseFloat(patched.Account.LowBalance, 64)
	if patched.Account.Name != "Renamed" || patched.Account.CPS != 7 || lb != 2.5 {
		t.Fatalf("patched account = %+v", patched.Account)
	}

	// Delete guard: children block with 409.
	rec = doAdmin(t, h, "DELETE", "/v1/admin/accounts/"+parent.ID, nil)
	if rec.Code != 409 {
		t.Fatalf("delete with children = %d, want 409", rec.Code)
	}
	// Child deletes fine; parent then deletes.
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/accounts/p1-child", nil); rec.Code != 200 {
		t.Fatalf("child delete = %d", rec.Code)
	}
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/accounts/"+parent.ID, nil); rec.Code != 200 {
		t.Fatalf("parent delete = %d", rec.Code)
	}
	if rec := doAdmin(t, h, "GET", "/v1/admin/accounts/"+parent.ID, nil); rec.Code != 404 {
		t.Fatalf("GET after delete = %d, want 404", rec.Code)
	}
	if rec := doAdmin(t, h, "PATCH", "/v1/admin/accounts/"+parent.ID, map[string]any{"name": "x"}); rec.Code != 404 {
		t.Fatalf("PATCH after delete = %d, want 404", rec.Code)
	}
}

func TestAuditLedgerPGOnlyGuard(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	// Memory mode keeps no trail/journal — a silent empty list would
	// lie to the operator, so these are explicit 503s.
	if rec := doAdmin(t, h, "GET", "/v1/admin/audit", nil); rec.Code != 503 {
		t.Fatalf("audit in memory mode = %d, want 503", rec.Code)
	}
	if rec := doAdmin(t, h, "GET", "/v1/admin/accounts/1000/ledger", nil); rec.Code != 503 {
		t.Fatalf("ledger in memory mode = %d, want 503", rec.Code)
	}
}

func TestWebhookPatchMemory(t *testing.T) {
	s := NewServer()
	StartWebhookFanout(s)
	h := MuxFor(s)
	rec := doAdmin(t, h, "POST", "/v1/admin/webhooks", map[string]any{
		"url": "https://8.8.8.8/hook", "events": []string{"gvos.call.teardown"},
	})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = doAdmin(t, h, "PATCH", "/v1/admin/webhooks/"+created.ID, map[string]any{
		"url": "https://8.8.4.4/hook2", "events": []string{"gvos.cdr.completed", "gvos.risk.blocked"}, "active": false,
	})
	if rec.Code != 200 {
		t.Fatalf("patch = %d body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, sub := range s.whDispatch.SubscriptionCount() {
		if sub.ID == created.ID {
			found = true
			if sub.URL != "https://8.8.4.4/hook2" || sub.Active ||
				len(sub.Events) != 2 || sub.Events[1] != "gvos.risk.blocked" {
				t.Fatalf("patched subscription = %+v", sub)
			}
		}
	}
	if !found {
		t.Fatal("patched subscription missing from dispatcher")
	}
	// Secret immutability: the PATCH body has no secret knob by design.
	if rec := doAdmin(t, h, "PATCH", "/v1/admin/webhooks/wh-nonexistent", map[string]any{"active": true}); rec.Code != 404 {
		t.Fatalf("patch unknown = %d, want 404", rec.Code)
	}
}

func TestTenantWebhookDeleteScoped(t *testing.T) {
	s := NewServer()
	StartWebhookFanout(s)
	h := MuxFor(s)

	// Operator-created endpoint: invisible and undeletable by tenants.
	rec := doAdmin(t, h, "POST", "/v1/admin/webhooks", map[string]any{
		"url": "https://8.8.8.8/ops", "events": []string{"gvos.cdr.completed"},
	})
	var ops struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ops)

	key := issueKey(s, acl.RoleTenant, "tenant-a")
	tenantDo := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Tenant creates its own, then deletes it.
	rec2 := tenantDo("POST", "/v1/tenant/webhooks", map[string]any{
		"url": "https://8.8.8.8/tenant-a", "events": []string{"gvos.cdr.completed"},
	})
	if rec2.Code != 201 {
		t.Fatalf("tenant create = %d body=%s", rec2.Code, rec2.Body.String())
	}
	var mine struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &mine)

	if rec := tenantDo("DELETE", "/v1/tenant/webhooks/"+mine.ID, nil); rec.Code != 200 {
		t.Fatalf("tenant delete own = %d", rec.Code)
	}
	// The operator endpoint: 404 (never leak existence across scopes).
	if rec := tenantDo("DELETE", "/v1/tenant/webhooks/"+ops.ID, nil); rec.Code != 404 {
		t.Fatalf("tenant delete operator webhook = %d, want 404", rec.Code)
	}
	for _, sub := range s.whDispatch.SubscriptionCount() {
		if sub.ID == mine.ID {
			t.Fatal("deleted tenant webhook still in dispatcher")
		}
	}
}

func TestPackageUnassignAndRateDelete(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	rec := doAdmin(t, h, "POST", "/v1/admin/accounts", map[string]any{"username": "pkg-u", "initial_balance": "10"})
	var acc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &acc)

	// Define + assign.
	rec = doAdmin(t, h, "PUT", "/v1/admin/packages", map[string]any{
		"id": "pkg-x", "cycle": "monthly", "free_minutes": 100, "assign_account": acc.ID,
	})
	if rec.Code != 200 {
		t.Fatalf("define+assign = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, spend, ok := s.pkg.Usage(acc.ID); !ok {
		t.Fatal("assign did not land")
	} else {
		_ = spend
	}
	// Unassign.
	rec = doAdmin(t, h, "PUT", "/v1/admin/packages", map[string]any{
		"id": "pkg-x", "unassign_account": acc.ID,
	})
	if rec.Code != 200 {
		t.Fatalf("unassign = %d", rec.Code)
	}
	if _, _, ok := s.pkg.Usage(acc.ID); ok {
		t.Fatal("unassign did not remove the binding")
	}

	// Rate delete (memory path): upsert then delete.
	doAdmin(t, h, "PUT", "/v1/admin/rates", map[string]any{
		"prefix": "86188888", "price": 0.5, "init_dur": 60, "step_dur": 6,
	})
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/rates/86188888", nil); rec.Code != 200 {
		t.Fatalf("rate delete = %d", rec.Code)
	}
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/rates/86188888", nil); rec.Code != 404 {
		t.Fatalf("rate delete twice = %d, want 404", rec.Code)
	}
	var list struct {
		Rates []struct {
			Prefix string `json:"prefix"`
		} `json:"rates"`
	}
	rec = doAdmin(t, h, "GET", "/v1/admin/rates", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	for _, rr := range list.Rates {
		if rr.Prefix == "86188888" {
			t.Fatal("deleted rate still listed")
		}
	}
}

func TestRegistrationsPagination(t *testing.T) {
	s := NewServer()
	for i := 0; i < 5; i++ {
		s.regs.Apply("user"+string(rune('a'+i)), "reg", 1700000000+int64(i)*60, "10.0.0."+string(rune('1'+i)), time.Unix(1700000000+int64(i)*60, 0))
	}
	h := MuxFor(s)
	rec := doAdmin(t, h, "GET", "/v1/admin/registrations?limit=2", nil)
	var p1 struct {
		Count      int    `json:"count"`
		Total      int    `json:"total"`
		NextCursor string `json:"next_cursor"`
		Items      []struct {
			User string `json:"user"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p1); err != nil {
		t.Fatal(err)
	}
	if p1.Count != 2 || p1.Total != 5 || p1.NextCursor == "" {
		t.Fatalf("page1 = %+v", p1)
	}
	rec = doAdmin(t, h, "GET", "/v1/admin/registrations?limit=2&cursor="+p1.NextCursor, nil)
	var p2 struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p2)
	if p2.Count != 2 {
		t.Fatalf("page2 count = %d", p2.Count)
	}
	// Newest-first ordering: page1 holds the two most recent users
	// (users are a..e with rising LastReg; 1700000000+i*60 — e,d newest).
	if p1.Items[0].User != "usere" || p1.Items[1].User != "userd" {
		t.Fatalf("page1 order = %s,%s (want usere,userd)", p1.Items[0].User, p1.Items[1].User)
	}
}
