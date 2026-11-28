// Tenant administration tests (PG-gated — the table is PG-owned;
// memory mode answers 503 on writes and shows the demo tenant read-
// only) plus the read-only settings face.
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestTenantAdminMemoryGuards(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	if rec := doAdmin(t, h, "POST", "/v1/admin/tenants", map[string]any{"name": "t2"}); rec.Code != 503 {
		t.Fatalf("memory create = %d, want 503", rec.Code)
	}
	rec := doAdmin(t, h, "GET", "/v1/admin/tenants", nil)
	var list struct {
		Count int    `json:"count"`
		Mode  string `json:"mode"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Count != 1 || list.Mode != "memory" {
		t.Fatalf("memory tenants = %+v", list)
	}
	if rec := doAdmin(t, h, "GET", "/v1/admin/settings", nil); rec.Code != 503 {
		t.Fatalf("memory settings = %d, want 503", rec.Code)
	}
}

func TestTenantAdminPGAndSettings(t *testing.T) {
	dsn := pgDSN(t)
	if dsn == "" {
		return
	}
	ctx := context.Background()
	s1, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.pg.TestResetRuntimeState(ctx); err != nil {
		t.Fatal(err)
	}
	s1.acl.Reset(nil)
	t.Cleanup(func() {
		cctx := context.Background()
		_ = s1.pg.TestResetRuntimeState(cctx)
		_ = s1.pg.TestExecSQL(cctx, `DELETE FROM tenants WHERE name LIKE 'pgten-%'`)
		s1.pg.Close()
	})
	srv := httptest.NewServer(MuxFor(s1))

	// Create (validation: currency length).
	if res := adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/tenants",
		`{"name":"pgten-a","currency":"USDTXX"}`, ""); res.Code != 400 {
		t.Fatalf("bad currency = %d, want 400", res.Code)
	}
	res := adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/tenants",
		`{"name":"pgten-a","currency":"USD"}`, "")
	if res.Code != 201 {
		t.Fatalf("create = %d body=%s", res.Code, res.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &created)

	// Duplicate name → 409.
	if res := adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/tenants",
		`{"name":"pgten-a"}`, ""); res.Code != 409 {
		t.Fatalf("duplicate = %d, want 409", res.Code)
	}

	// PATCH.
	if res := adminDo(t, srv, "dev-admin-token", "PATCH", "/v1/admin/tenants/"+created.ID,
		`{"name":"pgten-a2","currency":"EUR"}`, ""); res.Code != 200 {
		t.Fatalf("patch = %d", res.Code)
	}

	// Delete guard: no accounts yet → succeeds; demo tenant (has
	// accounts) → 409.
	if res := adminDo(t, srv, "dev-admin-token", "DELETE", "/v1/admin/tenants/"+created.ID, "", ""); res.Code != 200 {
		t.Fatalf("delete empty tenant = %d", res.Code)
	}
	if res := adminDo(t, srv, "dev-admin-token", "DELETE", "/v1/admin/tenants/00000000-0000-0000-0000-000000000001", "", ""); res.Code != 409 {
		t.Fatalf("delete demo tenant = %d, want 409", res.Code)
	}

	// Settings face: the six runtime keys, read-only.
	res = adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/settings", "", "")
	var settings struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Mode     string                     `json:"mode"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &settings)
	if settings.Mode != "readonly" {
		t.Fatalf("settings mode = %s", settings.Mode)
	}
	for _, k := range []string{"gateways", "dids", "recording", "packages", "risk_rules", "ip_allowlist"} {
		if _, ok := settings.Settings[k]; !ok {
			t.Fatalf("settings missing key %s", k)
		}
	}
}
