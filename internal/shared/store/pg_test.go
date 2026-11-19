package store

// Integration tests against a live PostgreSQL. Skipped unless
// GVOs_TEST_DSN is set, so `go test ./...` stays hermetic:
//
//   docker compose -f deploy/docker-compose.yml up -d postgres
//   docker exec -i deploy-postgres-1 psql -U gvos \
//     < migrations/002_seed.sql
//   GVOs_TEST_DSN="postgres://gvos:gvos_dev@127.0.0.1:5432/gvos?sslmode=disable" \
//     go test ./internal/store/ -v

import (
	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
	"context"
	"os"
	"testing"
	"time"

	"github.com/fastvoip/gvos-core/internal/billingdomain/account"
	"github.com/fastvoip/gvos-core/internal/customerdomain/blocklist"
	"github.com/fastvoip/gvos-core/internal/callcore"
)

func testDSN() string {
	return os.Getenv("GVOs_TEST_DSN") // accept either case for convenience
}

func TestLoadAccounts(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		dsn = os.Getenv("GVOS_TEST_DSN")
	}
	if dsn == "" {
		t.Skip("GVOs_TEST_DSN not set; skipping live-DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()

	acc := account.NewStore()
	n, err := pg.LoadAccounts(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("expected >=3 seeded accounts, got %d", n)
	}

	// The demo user chain loads with parents intact.
	chain, err := acc.Chain("00000000-0000-0000-0000-0000000000a3")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain depth = %d, want 3", len(chain))
	}
	if !chain[2].Postpaid {
		t.Error("tenant root should be postpaid")
	}
	if chain[0].Credit != 10 {
		t.Errorf("user credit = %v, want 10", chain[0].Credit)
	}
}

func TestLoadRatesAndLookup(t *testing.T) {
	dsn := os.Getenv("GVOs_TEST_DSN")
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()

	tbl, err := pg.LoadRates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}

	// Longest prefix.
	e := tbl.Lookup("8613800138000", 1, 600)
	if e == nil || e.Prefix != "8613" || e.Price != money.MustFromYuan(0.05) {
		t.Fatalf("CN mobile lookup = %+v", e)
	}
	// Night window on US: Monday 23:00.
	e = tbl.Lookup("12125550123", 1, 23*60)
	if e == nil || e.Price != money.MustFromYuan(0.015) {
		t.Fatalf("US night lookup = %+v", e)
	}
	// US daytime: base.
	e = tbl.Lookup("12125550123", 1, 600)
	if e == nil || e.Price != money.MustFromYuan(0.03) {
		t.Fatalf("US day lookup = %+v", e)
	}
}

func TestInsertCDRAndLedger(t *testing.T) {
	dsn := os.Getenv("GVOS_TEST_DSN")
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()

	cdr := &callcore.CDR{
		CallID:     "test-cdr-1",
		AccountID:  "00000000-0000-0000-0000-0000000000a3",
		Dest:       "8613800138000",
		StartTime:  time.Now(),
		Duration:   66,
		BilledSecs: 66,
		Cost:       money.MustFromYuan(0.055),
		RatePrefix: "8613",
		Price:      money.MustFromYuan(0.05),
		Release:    "normal",
	}
	inserted, err := pg.InsertCDR(ctx, cdr)
	if err != nil || !inserted {
		t.Fatalf("insert: inserted=%v err=%v", inserted, err)
	}
	// Idempotent under redelivery: the same CDR again finds the unique
	// index (uq_cdr_call) and reports no new row.
	if again, err := pg.InsertCDR(ctx, cdr); err != nil || again {
		t.Fatalf("redelivery insert: again=%v err=%v (want false, nil)", again, err)
	}
	if err := pg.AppendLedger(ctx, cdr.AccountID, -cdr.Cost, money.MustFromYuan(9.945), "call-settle", cdr.CallID); err != nil {
		t.Fatal(err)
	}
	if err := pg.AppendAudit(ctx, "00000000-0000-0000-0000-000000000001", "tester",
		"create", "cdr", cdr.CallID, nil, map[string]any{"cost": cdr.Cost}); err != nil {
		t.Fatal(err)
	}

	// Verify the CDR row landed with the exact money precision.
	var cost float64
	err = pg.pool.QueryRow(ctx,
		`SELECT cost FROM cdr WHERE call_id = $1`, cdr.CallID).Scan(&cost)
	if err != nil {
		t.Fatalf("read back cdr: %v", err)
	}
	if cost != 0.055 {
		t.Errorf("cdr cost roundtrip = %v, want 0.055", cost)
	}
}

// TestNumberListWriteThrough: the admin add/remove path persists to
// number_lists and the next LoadBlocklist sees the same ruleset —
// the durability contract behind the op-payload broadcast.
func TestNumberListWriteThrough(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		dsn = os.Getenv("GVOS_TEST_DSN")
	}
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()

	entry := blocklist.Entry{Kind: "black", Scope: "dest", Pattern: "86139000", IsRegex: false}
	if err := pg.UpsertNumberList(ctx, entry); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := pg.UpsertNumberList(ctx, blocklist.Entry{Kind: "white", Scope: "src", Pattern: "95xx$", IsRegex: true}); err != nil {
		t.Fatalf("upsert regex: %v", err)
	}

	lst := blocklist.New()
	n, err := pg.LoadBlocklist(ctx, lst)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n < 2 {
		t.Fatalf("loaded %d rules, want >=2", n)
	}
	if ok, why := lst.CheckScreen("8613900012345", "dest"); ok {
		t.Errorf("persisted black rule not active: %s", why)
	}

	removed, err := pg.RemoveNumberList(ctx, "", "black", "dest", "86139000")
	if err != nil || removed != 1 {
		t.Fatalf("remove: n=%d err=%v", removed, err)
	}
	if removed2, _ := pg.RemoveNumberList(ctx, "", "black", "dest", "86139000"); removed2 != 0 {
		t.Errorf("second remove = %d, want 0", removed2)
	}
	// Cleanup the regex rule so repeat runs stay tidy.
	if _, err := pg.RemoveNumberList(ctx, "", "white", "src", "95xx$"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
