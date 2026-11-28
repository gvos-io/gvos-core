// Per-account pricing tests: memory mode (quote API, group validation
// guards) plus the multi-group billing lifecycle — a dedicated group
// with a different price must actually re-price an account's calls
// (rate_group used to be decorative: every account priced from the
// single demo table).
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvos-io/gvos-core/internal/callcore"
)

func TestRateQuoteMemory(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	rec := doAdmin(t, h, "POST", "/v1/admin/rates/quote", map[string]any{
		"dest": "8613800138000", "seconds": 120,
	})
	if rec.Code != 200 {
		t.Fatalf("quote = %d body=%s", rec.Code, rec.Body.String())
	}
	var q struct {
		Prefix      string `json:"prefix"`
		EstCost     string `json:"est_cost"`
		Group       string `json:"group"`
		PricePerMin string `json:"price_per_min"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if q.Prefix == "" || q.EstCost == "" || q.Group != "demo" || q.PricePerMin == "" {
		t.Fatalf("quote = %+v", q)
	}
	// Unknown destination: 404, not a fake price.
	if rec := doAdmin(t, h, "POST", "/v1/admin/rates/quote", map[string]any{
		"dest": "00000000", "seconds": 60,
	}); rec.Code != 404 {
		t.Fatalf("unknown dest = %d, want 404", rec.Code)
	}
	// 86401 seconds: rejected.
	if rec := doAdmin(t, h, "POST", "/v1/admin/rates/quote", map[string]any{
		"dest": "8613800138000", "seconds": 90000,
	}); rec.Code != 400 {
		t.Fatalf("oversized seconds = %d, want 400", rec.Code)
	}
}

func TestRateGroupMemoryGuards(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	// Memory mode: single implicit demo group; creation is PG-only.
	if rec := doAdmin(t, h, "POST", "/v1/admin/rate-groups", map[string]any{"name": "gold"}); rec.Code != 503 {
		t.Fatalf("memory create = %d, want 503", rec.Code)
	}
	rec := doAdmin(t, h, "GET", "/v1/admin/rate-groups", nil)
	var list struct {
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Mode != "memory" || len(list.Groups) != 1 || list.Groups[0].Name != "demo" {
		t.Fatalf("memory groups = %+v", list)
	}
	// PUT rates with a non-demo group in memory mode: 400.
	if rec := doAdmin(t, h, "PUT", "/v1/admin/rates", map[string]any{
		"prefix": "86138", "price": 0.1, "group": "gold",
	}); rec.Code != 400 {
		t.Fatalf("memory group upsert = %d, want 400", rec.Code)
	}
}

func TestRateGroupPGMultiGroupPricing(t *testing.T) {
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
		// The new group, its rates and the account reference go too.
		_ = s1.pg.TestExecSQL(cctx, `DELETE FROM accounts WHERE username LIKE 'pggrp-%'`)
		_ = s1.pg.TestExecSQL(cctx, `DELETE FROM rates WHERE group_id = (SELECT id FROM rate_groups WHERE name = 'pggrp')`)
		_ = s1.pg.TestExecSQL(cctx, `DELETE FROM rate_groups WHERE name = 'pggrp'`)
		s1.pg.Close()
	})
	srv := httptest.NewServer(MuxFor(s1))

	// Define the group; PUT a rate into it (validated against the
	// group table now).
	if res := adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/rate-groups", `{"name":"pggrp"}`, ""); res.Code != 201 {
		t.Fatalf("group create = %d body=%s", res.Code, res.Body.String())
	}

	// The demo book's cheapest prefix is the shared test destination.
	res := adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/rates?group=demo", "", "")
	var demoBook struct {
		Rates []struct {
			Prefix string `json:"prefix"`
			Price  string `json:"price"`
		} `json:"rates"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &demoBook)
	if len(demoBook.Rates) == 0 {
		t.Fatal("demo book empty")
	}
	demoPrefix := demoBook.Rates[0].Prefix

	body := `{"prefix":"` + demoPrefix + `","price":9.99,"init_dur":60,"step_dur":6,"group":"pggrp"}`
	if res := adminDo(t, srv, "dev-admin-token", "PUT", "/v1/admin/rates", body, ""); res.Code != 200 {
		t.Fatalf("group rate upsert = %d body=%s", res.Code, res.Body.String())
	}
	// Unknown group rejected on upsert.
	if res := adminDo(t, srv, "dev-admin-token", "PUT", "/v1/admin/rates",
		`{"prefix":"86138","price":1,"group":"nope"}`, ""); res.Code != 400 {
		t.Fatalf("unknown-group upsert = %d, want 400", res.Code)
	}

	// Point a fresh account at the group; a call prices from pggrp.
	res = adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/accounts",
		`{"username":"pggrp-u1","credit":100,"rate_group":"pggrp"}`, "")
	var acc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &acc)
	if acc.ID == "" {
		t.Fatalf("account create failed: %s", res.Body.String())
	}
	// Unknown group rejected on create too.
	if res := adminDo(t, srv, "dev-admin-token", "POST", "/v1/admin/accounts",
		`{"username":"pggrp-u2","initial_balance":1,"rate_group":"nope"}`, ""); res.Code != 400 {
		t.Fatalf("unknown-group create = %d, want 400", res.Code)
	}

	if code, _ := s1.callStart(callStartReq{CallID: "pggrp-1", From: "pggrp-u1", To: demoPrefix + "999999"}); code != 200 {
		t.Fatalf("start = %d", code)
	}
	code, cdrAny := s1.callEnd(ctx, callEndReq{CallID: "pggrp-1", DurSecs: 60})
	if code != 200 {
		t.Fatalf("end = %d", code)
	}
	cdr, ok := cdrAny.(*callcore.CDR)
	if !ok {
		t.Fatalf("unexpected cdr type %T", cdrAny)
	}
	if price := cdr.Price.String(); !strings.HasPrefix(price, "9.99") {
		t.Fatalf("price = %s, want the pggrp 9.99 rate (demo was %s) — rate_group still decorative", price, demoBook.Rates[0].Price)
	}

	// Delete guard: the account reference blocks group removal.
	if res := adminDo(t, srv, "dev-admin-token", "DELETE", "/v1/admin/rate-groups/pggrp", "", ""); res.Code != 409 {
		t.Fatalf("delete in-use group = %d, want 409", res.Code)
	}
	// The demo group is protected.
	if res := adminDo(t, srv, "dev-admin-token", "DELETE", "/v1/admin/rate-groups/demo", "", ""); res.Code != 409 {
		t.Fatalf("delete demo = %d, want 409", res.Code)
	}
}
