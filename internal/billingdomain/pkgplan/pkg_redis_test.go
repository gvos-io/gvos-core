package pkgplan

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func newPkgR(t *testing.T) (*Engine, *PkgR) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e := New()
	cur := time.Unix(1_700_000_000, 0)
	e.SetClock(func() time.Time { return cur })
	r := NewPkgR(c)
	e.SetPkgRedis(r)
	return e, r
}

// TestSharedFreeAllowance: two replicas (here: two engines sharing the
// face) consume one free pool — the third call has no headroom left.
func TestSharedFreeAllowance(t *testing.T) {
	e, r := newPkgR(t)
	e.Define(Package{ID: "p1", Cycle: Daily, FreeMinutes: 1, Discount: 1})
	e.Assign("u1", "p1")

	// 30s free: 30 left in the shared pool.
	pr1 := e.Apply("u1", 30, money.MustFromYuan(0.06))
	if pr1.FreeSecs != 30 {
		t.Fatalf("first call free = %v, want 30", pr1.FreeSecs)
	}
	// A second replica's engine reads the shared headroom.
	e2 := New()
	cur := time.Unix(1_700_000_000, 0)
	e2.SetClock(func() time.Time { return cur })
	e2.SetPkgRedis(r)
	e2.Define(Package{ID: "p1", Cycle: Daily, FreeMinutes: 1, Discount: 1})
	e2.Assign("u1", "p1")
	pr2 := e2.Apply("u1", 30, money.MustFromYuan(0.06))
	if pr2.FreeSecs != 30 {
		t.Fatalf("second replica free = %v, want 30 (shared headroom 60-30)", pr2.FreeSecs)
	}
	// The pool is exhausted: the next call is fully billable.
	if got := e2.Apply("u1", 30, money.MustFromYuan(0.06)).FreeSecs; got != 0 {
		t.Fatalf("pool exhausted, free = %v want 0", got)
	}
}

// TestCycleRolloverNewKey: the next day's cycle is a new key (fresh
// pool) while the old key evaporates via TTL.
func TestCycleRolloverNewKey(t *testing.T) {
	e, r := newPkgR(t)
	e.Define(Package{ID: "p1", Cycle: Daily, FreeMinutes: 1, Discount: 1})
	e.Assign("u1", "p1")
	e.Apply("u1", 60, money.MustFromYuan(0.06)) // pool fully used

	// Fast-forward past midnight: shared used resets because the
	// cycleKey changed (new key = zero).
	next := time.Unix(1_700_000_000, 0).Add(26 * time.Hour)
	e.SetClock(func() time.Time { return next })
	if pr := e.Apply("u1", 60, money.MustFromYuan(0.06)); pr.FreeSecs != 60 {
		t.Fatalf("new cycle should grant the full pool: free = %v", pr.FreeSecs)
	}
	used, _ := r.GetUsed(context.Background(), "u1", e.cycleKeyOf(Daily))
	if used != 60 {
		t.Errorf("shared used under the new key = %v, want 60", used)
	}
}

// TestFailOpenLocalFallback: with ModeOpen, redis down -> local
// counters still grant free minutes.
func TestFailOpenLocalFallback(t *testing.T) {
	e, r := newPkgR(t)
	_ = r
	mr2 := miniredis.RunT(t)
	c2, _ := rds.New("redis://" + mr2.Addr())
	mr2.Close()
	e.SetMode(ModeOpen)
	e.SetPkgRedis(NewPkgR(c2)) // unreachable — open fallback
	e.Define(Package{ID: "p1", Cycle: Daily, FreeMinutes: 1, Discount: 1})
	e.Assign("u1", "p1")
	if pr := e.Apply("u1", 60, money.MustFromYuan(0.06)); pr.FreeSecs != 60 {
		t.Fatalf("fail-open should still apply the local package: %+v", pr)
	}
}

// TestListPriceOnRedisFailure: default ModeListPrice charges list
// (zero free) when Redis claim fails — never oversell free minutes.
func TestListPriceOnRedisFailure(t *testing.T) {
	e, _ := newPkgR(t)
	mr2 := miniredis.RunT(t)
	c2, _ := rds.New("redis://" + mr2.Addr())
	mr2.Close()
	e.SetMode(ModeListPrice)
	e.SetPkgRedis(NewPkgR(c2))
	e.Define(Package{ID: "p1", Cycle: Daily, FreeMinutes: 1, Discount: 1})
	e.Assign("u1", "p1")
	pr := e.Apply("u1", 60, money.MustFromYuan(0.06))
	if pr.FreeSecs != 0 || pr.BillableSecs != 60 {
		t.Fatalf("list_price on redis failure: %+v", pr)
	}
}
