// P1 PG-mode verification (GVOS_TEST_DSN-gated): the audit-trail and
// ledger read faces against real rows, and the account lifecycle
// persisting across a restart (PATCH survives; soft-delete stays
// deleted; the low_balance threshold column round-trips).
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPGAuditLedgerAccountLifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GVOS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx := context.Background()
	s1, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGServer: %v", err)
	}
	if err := s1.pg.TestResetRuntimeState(ctx); err != nil {
		t.Fatal(err)
	}
	s1.acl.Reset(nil)
	t.Cleanup(func() {
		// TestResetRuntimeState also clears the p1pg-% test accounts.
		_ = s1.pg.TestResetRuntimeState(context.Background())
		s1.pg.Close()
	})
	srv1 := httptest.NewServer(MuxFor(s1))
	defer srv1.Close()

	// Create an account with a low-balance threshold, top it up (both
	// write audit rows; the top-up also writes a ledger row).
	res := adminDo(t, srv1, "dev-admin-token", "POST", "/v1/admin/accounts",
		`{"username":"p1pg-u1","initial_balance":10}`, "")
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("create failed: %s", res.Body.String())
	}
	res = adminDo(t, srv1, "dev-admin-token", "PATCH", "/v1/admin/accounts/"+created.ID,
		`{"max_cps":9,"low_balance":3.5}`, "")
	if res.Code != 200 {
		t.Fatalf("patch = %d body=%s", res.Code, res.Body.String())
	}
	res = adminDo(t, srv1, "dev-admin-token", "POST", "/v1/admin/accounts/"+created.ID+"/topup",
		`{"delta":5,"reason":"pg-verify"}`, "")
	if res.Code != 200 {
		t.Fatalf("topup = %d", res.Code)
	}

	// Ledger: the top-up movement is readable (previously write-only).
	res = adminDo(t, srv1, "dev-admin-token", "GET", "/v1/admin/accounts/"+created.ID+"/ledger", "", "")
	var led struct {
		Items []struct {
			Reason  string `json:"reason"`
			Delta   string `json:"delta"`
			Balance string `json:"balance"`
		} `json:"items"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &led)
	if led.Count == 0 {
		t.Fatalf("ledger empty: %s", res.Body.String())
	}

	// Audit: create + patch + topup all landed.
	res = adminDo(t, srv1, "dev-admin-token", "GET", "/v1/admin/audit?entity=account&limit=10", "", "")
	var aud struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &aud)
	if aud.Count < 3 {
		t.Fatalf("audit rows = %d, want >= 3 (create+patch+topup)", aud.Count)
	}

	// Restart: PATCH survived (max_cps + low_balance round-trip).
	s2, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	acc2, err := s2.store.Get(created.ID)
	if err != nil {
		t.Fatalf("account lost across restart: %v", err)
	}
	if acc2.CPS != 9 || acc2.LowBalance == 0 {
		t.Fatalf("patch did not survive restart: cps=%d low_balance=%s", acc2.CPS, acc2.LowBalance.String())
	}
	s2.pg.Close()

	// Soft delete stays deleted across a restart.
	if res := adminDo(t, srv1, "dev-admin-token", "DELETE", "/v1/admin/accounts/"+created.ID, "", ""); res.Code != 200 {
		t.Fatalf("delete = %d", res.Code)
	}
	s3, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.pg.Close()
	if _, err := s3.store.Get(created.ID); err == nil {
		t.Fatal("soft-deleted account resurrected on restart")
	}
}
