// start_redis_test.go: Manager.Start on the shared-ledger path —
// the lock-refactor regression suite. Covers the reserve-failure
// rollback (ledger must not keep a registration whose balance lock
// failed) and the atomic oversell guarantee at the Manager level.
package callcore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// ledgerSetup wires a Manager onto a miniredis-backed AdmissionR.
func ledgerSetup(t *testing.T) (*Manager, *account.Store, *AdmissionR) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store := account.NewStore()
	store.Put(&account.Account{ID: "tenant", RateGroup: "g", Postpaid: true})
	store.Put(&account.Account{ID: "agent", ParentID: "tenant", Credit: 1e6, RateGroup: "g"})
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e3, RateGroup: "g"})
	adm := NewAdmissionR(c)
	m := NewManager(store, nil, time.Second)
	m.SetAdmissionLedger(adm)
	return m, store, adm
}

// A reserve failure after admission must roll the ledger
// registration back: LiveConcurrent drops to zero, so the next start
// is not blocked by a phantom call.
func TestStartReserveFailureRollsBackLedger(t *testing.T) {
	m, store, adm := ledgerSetup(t)
	ctx := context.Background()

	// Drain the account after setup: reserve must fail, admission
	// must have registered, and the rollback must clear it.
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 0, RateGroup: "g"})
	if err := m.Start("c1", "user-a", "86", entry2(), 60, false); err == nil {
		t.Fatal("reserve on zero balance must fail")
	}
	if n, _ := adm.LiveConcurrent(ctx, "user-a"); n != 0 {
		t.Fatalf("ledger live = %d after rollback, want 0", n)
	}
	if n, _ := adm.LiveConcurrent(ctx, "agent"); n != 0 {
		t.Fatalf("ancestor ledger live = %d after rollback, want 0", n)
	}
	// The map never saw the call either.
	if m.Live() != 0 {
		t.Fatalf("manager live = %d, want 0", m.Live())
	}

	// And the account is startable again once funded (no phantom).
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e3, RateGroup: "g"})
	if err := m.Start("c2", "user-a", "86", entry2(), 60, false); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if n, _ := adm.LiveConcurrent(ctx, "user-a"); n != 1 {
		t.Fatalf("ledger live = %d, want 1", n)
	}
}

// End must remove the registration from every chain key (the EndChain
// round trip now runs outside the map lock; the effect is the same).
func TestEndClearsLedgerChain(t *testing.T) {
	m, _, adm := ledgerSetup(t)
	ctx := context.Background()

	if err := m.Start("c1", "user-a", "86", entry2(), 60, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"user-a", "agent"} {
		if n, _ := adm.LiveConcurrent(ctx, id); n != 1 {
			t.Fatalf("%s live = %d before end, want 1", id, n)
		}
	}
	if _, err := m.End("c1", 60); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"user-a", "agent"} {
		if n, _ := adm.LiveConcurrent(ctx, id); n != 0 {
			t.Fatalf("%s live = %d after end, want 0", id, n)
		}
	}
}

// Manager-level oversell regression: goroutines racing the cap edge
// through the full Start path (admit + reserve + map insert).
func TestStartLedgerOversell(t *testing.T) {
	m, store, adm := ledgerSetup(t)
	ctx := context.Background()
	store.Put(&account.Account{ID: "agent", ParentID: "tenant", Credit: 1e9, RateGroup: "g", Concurrent: 5})
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e9, RateGroup: "g", Concurrent: 5})

	const racers = 32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Start(fmt.Sprintf("r-%d", i), "user-a", "86", entry2(), 60, false)
		}(i)
	}
	wg.Wait()
	if m.Live() != 5 {
		t.Fatalf("manager live = %d, want exactly 5", m.Live())
	}
	if n, _ := adm.LiveConcurrent(ctx, "user-a"); n != 5 {
		t.Fatalf("ledger live = %d, want 5", n)
	}
	if n, _ := adm.LiveConcurrent(ctx, "agent"); n != 5 {
		t.Fatalf("ancestor ledger live = %d, want 5", n)
	}
}

// balanceRefusal is a BalanceAuthority stub that always refuses: the
// rollback path must trigger for the Redis authority shape too, not
// just the memory store.
type balanceRefusal struct{}

func (balanceRefusal) ReserveCtx(ctx context.Context, id string, est money.Amount) (string, error) {
	return "", errors.New("authority down")
}
func (balanceRefusal) CommitCtx(ctx context.Context, token, id string, est, actual money.Amount) error {
	return nil
}
func (balanceRefusal) GetBalance(ctx context.Context, id string) (money.Amount, error) {
	return 0, errors.New("authority down")
}

func TestStartAuthorityFailureRollsBackLedger(t *testing.T) {
	m, store, adm := ledgerSetup(t)
	m.SetBalanceAuthority(balanceRefusal{})
	ctx := context.Background()
	_ = store

	if err := m.Start("c1", "user-a", "86", entry2(), 60, false); err == nil {
		t.Fatal("authority failure must refuse the call")
	}
	if n, _ := adm.LiveConcurrent(ctx, "user-a"); n != 0 {
		t.Fatalf("ledger live = %d, want 0 (rollback ran)", n)
	}
}
