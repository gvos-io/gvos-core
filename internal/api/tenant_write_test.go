// M10 tenant write-plane tests (memory mode): the transfer semantics
// (tenant funds its subtree downward, money is conserved), the scope
// guards, and the agent read-only rule.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

func doKey(t *testing.T, h http.Handler, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
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

func TestTenantWriteTransferAndProvision(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	tenantKey := issueKey(s, acl.RoleTenant, "agent1")
	agentKey := issueKey(s, acl.RoleAgent, "agent1")

	bal := func(id string) money.Amount {
		b, _ := account.Balance(s.store, id)
		return b
	}
	before1000 := bal("1000")

	// Provision a child with an initial allocation from agent1.
	rec := doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts", map[string]any{
		"username": "child-x", "name": "Child X", "initial_credit": 2,
	})
	if rec.Code != 201 {
		t.Fatalf("tenant create = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if bal(created.ID) != money.MustFromYuan(2) {
		t.Fatalf("child credit = %s, want 2", bal(created.ID).String())
	}
	if bal("agent1") != money.MustFromYuan(1000-2) {
		t.Fatalf("tenant balance not debited: %s", bal("agent1").String())
	}

	// Transfer more to the child.
	rec = doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts/"+created.ID+"/topup", map[string]any{"delta": 1})
	if rec.Code != 200 {
		t.Fatalf("transfer = %d body=%s", rec.Code, rec.Body.String())
	}
	if bal(created.ID) != money.MustFromYuan(3) || bal("agent1") != money.MustFromYuan(1000-3) {
		t.Fatalf("balances after transfer: child=%s agent1=%s", bal(created.ID).String(), bal("agent1").String())
	}
	// The demo SIP user (grandchild under agent1) is in scope too.
	if rec = doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts/1000/topup", map[string]any{"delta": 1}); rec.Code != 200 {
		t.Fatalf("grandchild transfer = %d", rec.Code)
	}
	if bal("1000") != before1000.Add(money.MustFromYuan(1)) {
		t.Fatalf("grandchild balance = %s", bal("1000").String())
	}

	// Overdraft: transfer more than agent1 holds → INSUFFICIENT_CREDIT.
	if rec = doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts/"+created.ID+"/topup", map[string]any{"delta": 1e9}); rec.Code != 403 {
		t.Fatalf("overdraft = %d, want 403", rec.Code)
	}

	// Self-root topup: out of scope (root is funded from above).
	if rec = doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts/agent1/topup", map[string]any{"delta": 1}); rec.Code != 404 {
		t.Fatalf("self-root topup = %d, want 404", rec.Code)
	}

	// Agent role is read-only on the write plane.
	if rec := doKey(t, h, agentKey, "POST", "/v1/tenant/accounts", map[string]any{"username": "nope"}); rec.Code != 403 {
		t.Fatalf("agent create = %d, want 403", rec.Code)
	}
	if rec := doKey(t, h, agentKey, "POST", "/v1/tenant/accounts/1000/topup", map[string]any{"delta": 1}); rec.Code != 403 {
		t.Fatalf("agent topup = %d, want 403", rec.Code)
	}

	// Out-of-subtree escape: a tenant rooted elsewhere gets a bare 404.
	otherKey := issueKey(s, acl.RoleTenant, "no-such-root")
	if rec := doKey(t, h, otherKey, "POST", "/v1/tenant/accounts/"+created.ID+"/topup", map[string]any{"delta": 1}); rec.Code != 404 {
		t.Fatalf("foreign-scope topup = %d, want 404", rec.Code)
	}

	// Negative delta rejected (clawback is an admin action).
	if rec := doKey(t, h, tenantKey, "POST", "/v1/tenant/accounts/"+created.ID+"/topup", map[string]any{"delta": -1}); rec.Code != 400 {
		t.Fatalf("negative delta = %d, want 400", rec.Code)
	}
}
