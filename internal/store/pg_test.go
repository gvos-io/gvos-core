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
	"context"
	"os"
	"testing"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/callstate"
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
	if e == nil || e.Prefix != "8613" || e.Price != 0.05 {
		t.Fatalf("CN mobile lookup = %+v", e)
	}
	// Night window on US: Monday 23:00.
	e = tbl.Lookup("12125550123", 1, 23*60)
	if e == nil || e.Price != 0.015 {
		t.Fatalf("US night lookup = %+v", e)
	}
	// US daytime: base.
	e = tbl.Lookup("12125550123", 1, 600)
	if e == nil || e.Price != 0.03 {
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

	cdr := &callstate.CDR{
		CallID:     "test-cdr-1",
		AccountID:  "00000000-0000-0000-0000-0000000000a3",
		Dest:       "8613800138000",
		StartTime:  time.Now(),
		Duration:   66,
		BilledSecs: 66,
		Cost:       0.055,
		RatePrefix: "8613",
		Price:      0.05,
		Release:    "normal",
	}
	if err := pg.InsertCDR(ctx, cdr); err != nil {
		t.Fatal(err)
	}
	if err := pg.AppendLedger(ctx, cdr.AccountID, -cdr.Cost, 9.945, "call-settle", cdr.CallID); err != nil {
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
