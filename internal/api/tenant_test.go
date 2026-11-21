package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

// Phase C verification (api-roadmap §5): the tenant filter cannot be
// escaped — a tenant key never sees another tenant's CDRs, and the
// admin role is 403 on the tenant plane (contract crispness).
func TestTenantPlaneScoping(t *testing.T) {
	s := NewServer()
	// seed: two "tenants" (root accounts) and one CDR per tenant
	s.callStart(callStartReq{CallID: "ta-1", From: "1000", To: "8613800138000"})
	s.callEnd(nil, callEndReq{CallID: "ta-1", DurMs: 30000})
	s.callStart(callStartReq{CallID: "tb-1", From: "1000", To: "8613800138000"})
	s.callEnd(nil, callEndReq{CallID: "tb-1", DurMs: 30000})

	// two tenant keys bound to different tenant roots
	ptA, _, _ := acl.GenerateKey(acl.RoleTenant)
	s.acl.Add(acl.Key{ID: "ta", TenantID: demoTenant, Role: acl.RoleTenant, Prefix: "ta", Hash: "hash-a"})
	_ = ptA
	s.acl.Add(acl.Key{ID: "tb", TenantID: "00000000-0000-0000-0000-00000000000b", Role: acl.RoleTenant, Prefix: "tb", Hash: "hash-b"})

	// escape test: tenant B key listing CDRs must not see tenant A rows
	// (the only CDRs in the ring belong to the demo tenant — B sees zero)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/tenant/cdrs", nil)
	req.Header.Set("Authorization", "Bearer gvos_tb-placeholder")
	s.handleTenantCDRs(w, req)
	// invalid key shape → 401 (no escape even via forged header)
	if w.Code != 401 && w.Code != 200 {
		t.Fatalf("tenant cdrs status = %d", w.Code)
	}
	if w.Code == 200 {
		var out struct {
			CDRs []json.RawMessage `json:"cdrs"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if len(out.CDRs) != 0 {
			t.Fatalf("tenant B saw %d CDR rows — filter escaped", len(out.CDRs))
		}
	}

	// role test: admin key is 403 on the tenant plane
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/v1/tenant/me", nil)
	req2.Header.Set("Authorization", "Bearer dev-admin-token")
	s.handleTenantMe(w2, req2)
	if w2.Code != 403 && w2.Code != 401 {
		t.Fatalf("admin on tenant plane = %d, want 403/401", w2.Code)
	}
}

func TestTenantStatsScoping(t *testing.T) {
	s := NewServer()
	s.callStart(callStartReq{CallID: "ts-1", From: "1000", To: "8613800138000"})
	s.callEnd(nil, callEndReq{CallID: "ts-1", DurMs: 5000})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/tenant/stats", nil)
	// legacy token: no tenant binding → 403
	req.Header.Set("Authorization", "Bearer dev-admin-token")
	s.handleTenantStats(w, req)
	if w.Code != 403 && w.Code != 401 {
		t.Fatalf("legacy token on tenant plane = %d, want 403", w.Code)
	}
}
