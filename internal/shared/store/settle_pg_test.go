package store

// Integration tests against a live PostgreSQL (same gate as pg_test.go:
// GVOs_TEST_DSN / GVOS_TEST_DSN). They pin the money contract at the
// SQL boundary, where the 2026-11 audit found every CDR and ledger row
// stored x10^4 and a rate upserted through the API coming back x10^4.

import (
	"context"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/callcore"
)

func liveDB(t *testing.T) (*PG, context.Context) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("GVOs_TEST_DSN not set; skipping live-DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg, ctx
}

func settleCDR(callID, accountID string, cost money.Amount) *callcore.CDR {
	return &callcore.CDR{
		CallID: callID, AccountID: accountID, Dest: "8613800138000",
		StartTime: time.Now(), Duration: 66, BilledSecs: 66,
		Cost: cost, RatePrefix: "8613", Price: money.MustFromYuan(0.05), Release: "normal",
	}
}

func pgCredit(t *testing.T, pg *PG, ctx context.Context, id string) money.Amount {
	t.Helper()
	acc := account.NewStore()
	if _, err := pg.LoadAccounts(ctx, acc); err != nil {
		t.Fatal(err)
	}
	bal, err := account.Balance(acc, id)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

// The settle transaction takes the cost off accounts.credit exactly
// once, and a redelivery changes nothing.
func TestSettleDeductsCreditOnce(t *testing.T) {
	pg, ctx := liveDB(t)
	const tenant = "00000000-0000-0000-0000-000000000001"
	const parent = "00000000-0000-0000-0000-0000000000a2"
	id, err := pg.CreateAccountWithHA1(ctx, tenant, parent, "audit", "settle-"+time.Now().Format("150405.000000000"),
		"00000000000000000000000000000000", "demo", false, money.MustFromYuan(10))
	if err != nil {
		t.Fatal(err)
	}
	cdr := settleCDR("settle-"+id[:8], id, money.MustFromYuan(0.066))
	cost := cdr.Cost
	bal := money.MustFromYuan(10).Sub(cost)

	inserted, err := pg.InsertCDRWithSettle(ctx, cdr, bal, true)
	if err != nil || !inserted {
		t.Fatalf("first delivery: inserted=%v err=%v", inserted, err)
	}
	if got := pgCredit(t, pg, ctx, id); got != bal {
		t.Errorf("accounts.credit after settle = %s, want %s", got, bal)
	}

	// Redelivery of the same CDR: no second deduction, no second row.
	if inserted, err := pg.InsertCDRWithSettle(ctx, cdr, bal, true); err != nil || inserted {
		t.Fatalf("redelivery: inserted=%v err=%v, want false/nil", inserted, err)
	}
	if got := pgCredit(t, pg, ctx, id); got != bal {
		t.Errorf("accounts.credit after redelivery = %s, want %s (charged twice?)", got, bal)
	}
	var rows int
	var delta string
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int, min(delta)::text FROM balance_ledger WHERE call_id = $1`, cdr.CallID).Scan(&rows, &delta); err != nil {
		t.Fatal(err)
	}
	// NUMERIC(18,7) prints 7 decimals ("0.0660000"); compare as money.
	if rows != 1 || !sameYuan(t, delta, -cost) {
		t.Errorf("ledger rows = %d delta = %s, want 1 / %s (yuan, not x10^4)", rows, delta, (-cost).String())
	}
	var cdrCost string
	if err := pg.pool.QueryRow(ctx, `SELECT cost::text FROM cdr WHERE call_id = $1`, cdr.CallID).Scan(&cdrCost); err != nil {
		t.Fatal(err)
	}
	if !sameYuan(t, cdrCost, cost) {
		t.Errorf("cdr.cost = %s, want %s", cdrCost, cost)
	}
}

// sameYuan compares a NUMERIC column's text with an Amount.
func sameYuan(t *testing.T, text string, want money.Amount) bool {
	t.Helper()
	got, err := money.FromDecimal(text)
	if err != nil {
		t.Errorf("column value %q is not a decimal: %v", text, err)
		return false
	}
	return got == want
}

// The mirror never drives a prepaid balance below zero (the schema's
// CHECK forbids it and a violation would roll the CDR back with it).
func TestSettleKeepsPrepaidCreditNonNegative(t *testing.T) {
	pg, ctx := liveDB(t)
	const tenant = "00000000-0000-0000-0000-000000000001"
	const parent = "00000000-0000-0000-0000-0000000000a2"
	id, err := pg.CreateAccountWithHA1(ctx, tenant, parent, "audit", "clamp-"+time.Now().Format("150405.000000000"),
		"00000000000000000000000000000000", "demo", false, money.MustFromYuan(0.05))
	if err != nil {
		t.Fatal(err)
	}
	cdr := settleCDR("clamp-"+id[:8], id, money.MustFromYuan(0.066)) // paid partly out of a credit line
	if inserted, err := pg.InsertCDRWithSettle(ctx, cdr, money.MustFromYuan(-0.016), true); err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if got := pgCredit(t, pg, ctx, id); got != 0 {
		t.Errorf("accounts.credit = %s, want 0.0000", got)
	}
}

// A rate written through the store must load back as the same yuan
// value: UpsertRate used to hand pgx the scaled int64.
func TestUpsertRateRoundTripsAsYuan(t *testing.T) {
	pg, ctx := liveDB(t)
	price := money.MustFromYuan(0.06)
	prefix := "99966" + time.Now().Format("150405")[:4]
	if err := pg.UpsertRate(ctx, demoRateGroupForTest(t, pg), prefix, price, 60, 6, 0); err != nil {
		t.Fatal(err)
	}
	tbl, err := pg.LoadRates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	e := tbl.Lookup(prefix+"123456", 1, 600)
	if e == nil || e.Price != price {
		t.Fatalf("rate %s loaded back as %+v, want price %s (yuan)", prefix, e, price)
	}
}

func demoRateGroupForTest(t *testing.T, pg *PG) string {
	t.Helper()
	var id string
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text FROM rate_groups WHERE name = 'demo'`).Scan(&id); err != nil {
		t.Fatalf("demo rate group: %v (seed missing?)", err)
	}
	return id
}

// The shipped schema covers a fixed window of months; the ensure step
// must create the rest, idempotently.
func TestEnsureCDRPartitions(t *testing.T) {
	pg, ctx := liveDB(t)
	for pass := 0; pass < 2; pass++ {
		if err := pg.EnsureCDRPartitions(ctx, 3); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
	}
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 3; i++ {
		m := first.AddDate(0, i, 0)
		name := m.Format("cdr_2006_01")
		var n int
		if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("partition %s: found %d, want 1", name, n)
		}
	}
}
