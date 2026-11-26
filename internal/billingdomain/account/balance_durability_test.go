package account

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// newRedisWithServer is newRedisStore plus the server handle, for the
// tests that lose or inspect authority data.
func newRedisWithServer(t *testing.T) (*miniredis.Miniredis, *Store, *BalanceR) {
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
	return mr, s, b
}

// The authority holding no balance for an account is a platform fault
// (data lost, seed never landed): it is reported apart from "broke", so
// the caller answers 503 instead of hanging up on a funded customer.
func TestRedisReserveWithoutBalanceIsNotSeeded(t *testing.T) {
	mr, s, b := newRedisWithServer(t)
	ctx := context.Background()
	s.Put(&Account{ID: "late", ParentID: "agent1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	// "late" was never seeded: the authority has no key for it.

	_, err := b.Reserve(ctx, "late", money.MustFromYuan(0.06))
	if !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("reserve on a missing balance: err = %v, want ErrNotSeeded", err)
	}
	if mr.Exists(balKey("late")) {
		t.Error("a refused reserve must not create a balance")
	}
}

// A settlement refund must never materialise a balance. With the key
// gone, creating it pins a wrong value that the SETNX re-seed then
// refuses to overwrite (audit: a 9.94 balance became 0.06 after the
// authority lost its data).
func TestRedisRefundNeverCreatesBalance(t *testing.T) {
	mr, _, b := newRedisWithServer(t)
	ctx := context.Background()

	mr.FlushAll() // the authority restarts empty (no persistence)
	if err := b.Commit(ctx, "tok", "end1", money.MustFromYuan(1), money.MustFromYuan(0.4)); !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("commit on a lost balance: err = %v, want ErrNotSeeded", err)
	}
	if mr.Exists(balKey("end1")) {
		t.Fatal("a refund created a balance key; the re-seed could no longer restore the snapshot")
	}

	// The boot seed then restores the last snapshot untouched.
	if err := b.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if bal, _ := b.Get(ctx, "end1"); bal != money.MustFromYuan(10) {
		t.Errorf("end1 after re-seed = %s, want 10.0000", bal)
	}
}

// The in-memory authority shares the credit-line rule.
func TestMemoryMaxDebtIsCreditLine(t *testing.T) {
	s := NewStore()
	s.Put(&Account{ID: "a", Credit: money.MustFromYuan(1), MaxDebt: money.MustFromYuan(0.5)})
	if _, err := s.Reserve("a", money.MustFromYuan(1.5)); err != nil {
		t.Fatalf("reserve up to credit + line: %v", err)
	}
	if bal, _ := Balance(s, "a"); bal != money.MustFromYuan(-0.5) {
		t.Errorf("a = %s, want -0.5000 (credit line spent)", bal)
	}
	if _, err := s.Reserve("a", money.MustFromYuan(0.01)); err == nil {
		t.Error("reserve beyond the credit line must fail")
	}

	s.Put(&Account{ID: "b", Credit: money.MustFromYuan(0.05)})
	if _, err := s.Reserve("b", money.MustFromYuan(0.06)); err == nil {
		t.Error("reserve beyond the balance must fail without a credit line")
	}
}
