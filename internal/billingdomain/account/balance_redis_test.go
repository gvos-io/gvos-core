package account

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func newRedisStore(t *testing.T) (*Store, *BalanceR) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s := NewStore()
	s.Put(&Account{ID: "tenant", Credit: money.MustFromYuan(100), RateGroup: "g"})
	s.Put(&Account{ID: "agent1", ParentID: "tenant", Credit: money.MustFromYuan(100), RateGroup: "g"})
	s.Put(&Account{ID: "end1", ParentID: "agent1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	b := NewBalanceR(s, c)
	if err := b.Seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s, b
}

func TestRedisReserveAllOrNothing(t *testing.T) {
	_, b := newRedisStore(t)
	ctx := context.Background()

	// Whole-chain reserve of 9: end1/agent1/tenant each drop 9.
	if _, err := b.Reserve(ctx, "end1", money.MustFromYuan(9)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	for id, want := range map[string]float64{"end1": 1, "agent1": 91, "tenant": 91} {
		if bal, _ := b.Get(ctx, id); bal != money.MustFromYuan(want) {
			t.Errorf("bal[%s] = %s, want %v", id, bal, want)
		}
	}

	// end1 now has 1: reserve 5 fails and the partial debits roll back.
	if _, err := b.Reserve(ctx, "end1", money.MustFromYuan(5)); err == nil {
		t.Fatal("expected insufficient")
	}
	for id, want := range map[string]float64{"agent1": 91, "tenant": 91} {
		if bal, _ := b.Get(ctx, id); bal != money.MustFromYuan(want) {
			t.Errorf("rollback bal[%s] = %s, want %v (partial debit not rolled back)", id, bal, want)
		}
	}
}

func TestRedisCommitRefund(t *testing.T) {
	_, b := newRedisStore(t)
	ctx := context.Background()
	b.Reserve(ctx, "end1", money.MustFromYuan(9))
	// Actual 4: refund 5 across the chain.
	if err := b.Commit(ctx, "tok", "end1", money.MustFromYuan(9), money.MustFromYuan(4)); err != nil {
		t.Fatal(err)
	}
	if bal, _ := b.Get(ctx, "end1"); bal != money.MustFromYuan(6) {
		t.Errorf("end1 = %s, want 6.0000 (est 9, actual 4)", bal)
	}
}

func TestRedisMaxDebtFloor(t *testing.T) {
	s, b := newRedisStore(t)
	ctx := context.Background()
	// end1: 1 yuan left, MaxDebt 0.5 -> can reserve 1.4 (floor -0.5... wait:
	// floor = credit-maxdebt = 0.5). Reserve 0.5 ok, 0.6 fails.
	s.Put(&Account{ID: "end2", ParentID: "agent1", Credit: money.MustFromYuan(1), MaxDebt: money.MustFromYuan(0.5), RateGroup: "g"})
	if err := b.SetAbsolute(ctx, "end2", money.MustFromYuan(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, "end2", money.MustFromYuan(0.5)); err != nil {
		t.Fatalf("reserve at floor edge: %v", err)
	}
	if _, err := b.Reserve(ctx, "end2", money.MustFromYuan(0.6)); err == nil {
		t.Error("reserve beyond maxDebt floor must fail")
	}
}

func TestRedisSeedDoesNotClobber(t *testing.T) {
	_, b := newRedisStore(t)
	ctx := context.Background()
	// Another replica moved the money; a restarting replica must not
	// reset it from its stale config.
	b.SetAbsolute(ctx, "end1", money.MustFromYuan(3))
	if err := b.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if bal, _ := b.Get(ctx, "end1"); bal != money.MustFromYuan(3) {
		t.Errorf("seed clobbered live balance: %s, want 3.0000", bal)
	}
}
