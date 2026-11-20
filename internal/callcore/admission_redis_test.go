package callcore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func newAdm(t *testing.T) *AdmissionR {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return NewAdmissionR(c)
}

func TestAdmissionLedgerLifecycle(t *testing.T) {
	a := newAdm(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 3; i++ {
		if err := a.Start(ctx, fmtCall(i), "end1", now); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := a.LiveConcurrent(ctx, "end1"); n != 3 {
		t.Fatalf("live = %d, want 3", n)
	}
	if err := a.End(ctx, fmtCall(1), "end1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.LiveConcurrent(ctx, "end1"); n != 2 {
		t.Fatalf("after end live = %d, want 2", n)
	}
}

func fmtCall(i int) string { return "call-" + string(rune('a'+i)) }

func TestAdmitConcurrentChain(t *testing.T) {
	a := newAdm(t)
	ctx := context.Background()
	now := time.Now()

	// agent1's sub-tree cap 2: two live under end1, mirrored onto the
	// chain keys (Manager does this via StartChain).
	chain := []string{"end1", "agent1"}
	for i := 0; i < 2; i++ {
		if err := a.StartChain(ctx, fmtCall(i), chain, now); err != nil {
			t.Fatal(err)
		}
	}
	caps := map[string]int{"agent1": 2, "end1": 10}
	if err := a.AdmitConcurrent(ctx, chain, "end1", caps); err == nil {
		t.Fatal("sub-tree at cap must refuse the third")
	}
	// Own-account cap under the tree: end1 cap 10 admits.
	caps = map[string]int{"agent1": 5, "end1": 10}
	if err := a.AdmitConcurrent(ctx, chain, "end1", caps); err != nil {
		t.Fatalf("under caps should admit: %v", err)
	}
}

func TestAdmitCPSWindow(t *testing.T) {
	a := newAdm(t)
	ctx := context.Background()
	sec := time.Now().Unix()
	for i := 1; i <= 3; i++ {
		if err := a.AdmitCPS(ctx, "end1", sec, 3); err != nil {
			t.Fatalf("within cap: %v", err)
		}
	}
	if err := a.AdmitCPS(ctx, "end1", sec, 3); err == nil {
		t.Fatal("4th start in the same second must be refused")
	}
	// Next second: fresh window.
	if err := a.AdmitCPS(ctx, "end1", sec+1, 3); err != nil {
		t.Fatalf("fresh window: %v", err)
	}
}

func TestZombiesAndSelfClean(t *testing.T) {
	mr := miniredis.RunT(t)
	c, _ := rds.New("redis://" + mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	a := NewAdmissionR(c)
	ctx := context.Background()

	old := time.Now().Add(-ZombieTTL - time.Minute)
	if err := a.Start(ctx, "z-1", "end1", old); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "fresh-1", "end1", time.Now()); err != nil {
		t.Fatal(err)
	}
	z, err := a.Zombies(ctx, "end1")
	if err != nil || len(z) != 1 || z[0] != "z-1" {
		t.Fatalf("zombies = %v err=%v, want [z-1]", z, err)
	}
	if err := a.Remove(ctx, "end1", "z-1"); err != nil {
		t.Fatal(err)
	}
	if z, _ = a.Zombies(ctx, "end1"); len(z) != 0 {
		t.Fatalf("after remove zombies = %v", z)
	}
	// Ledger TTL self-clean: fast-forward, the whole set evaporates.
	mr.FastForward(ZombieTTL + time.Minute)
	if n, _ := a.LiveConcurrent(ctx, "end1"); n != 0 {
		t.Fatalf("TTL should have evicted the ledger, live = %d", n)
	}
}

// TestManagerUsesAdmissionLedger: with the ledger wired, Start admits
// against Redis counts (not the local map) — the ×N oversell closes.
func TestManagerUsesAdmissionLedger(t *testing.T) {
	mr := miniredis.RunT(t)
	rc, _ := rds.New("redis://" + mr.Addr())
	t.Cleanup(func() { _ = rc.Close() })

	store := account.NewStore()
	store.Put(&account.Account{ID: "agent1", Credit: money.MustFromYuan(100), RateGroup: "g", Concurrent: 2})
	store.Put(&account.Account{ID: "end1", ParentID: "agent1", Credit: money.MustFromYuan(100), RateGroup: "g"})
	m := NewManager(store, func(string, string) {}, time.Second)
	m.SetAdmissionLedger(NewAdmissionR(rc))

	e := testEntry()
	ctx := context.Background()
	// Two calls admitted (chain cap 2 via the ledger).
	if err := m.Start("c1", "end1", "86", e, 60, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("c2", "end1", "86", e, 60, false); err != nil {
		t.Fatal(err)
	}
	// Third: the ledger's chain count refuses (agent1 cap 2).
	if err := m.Start("c3", "end1", "86", e, 60, false); err == nil {
		t.Fatal("chain concurrent cap must be enforced by the ledger")
	}
	// End one: ledger frees headroom.
	if _, err := m.End("c1", 66); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("c3", "end1", "86", e, 60, false); err != nil {
		t.Fatalf("after end, headroom should exist: %v", err)
	}
	_ = ctx
}

func testEntry() *rate.Entry {
	return &rate.Entry{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 60, StepDur: 6}
}

func TestAdmitCallSemantics(t *testing.T) {
	a := newAdm(t)
	ctx := context.Background()
	now := time.Now()

	// Chain end1 -> agent1 (own first, ancestor second).
	chain := []string{"end1", "agent1"}

	// 1) Admitted: registers on every chain key.
	if err := a.AdmitCall(ctx, chain, map[string]int{"agent1": 2, "end1": 5}, "c1", now, 0); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	for _, id := range chain {
		if n, _ := a.LiveConcurrent(ctx, id); n != 1 {
			t.Fatalf("%s live = %d, want 1", id, n)
		}
	}
	// ZADD re-registration of the same call-id must not double-count.
	if err := a.AdmitCall(ctx, chain, nil, "c1", now, 0); err != nil {
		t.Fatalf("re-admit same id: %v", err)
	}
	if n, _ := a.LiveConcurrent(ctx, "end1"); n != 1 {
		t.Fatalf("after re-admit live = %d, want 1", n)
	}

	// 2) Own cap refuses and registers nothing.
	if err := a.AdmitCall(ctx, chain, map[string]int{"end1": 1}, "c2", now, 0); err == nil {
		t.Fatal("own cap must refuse")
	}
	if n, _ := a.LiveConcurrent(ctx, "end1"); n != 1 {
		t.Fatalf("refused call must not register: live = %d, want 1", n)
	}

	// 3) Ancestor sub-tree cap refuses; the OWN key counts the reject
	// candidate (live+1 check), the whole registration never happens.
	if err := a.AdmitCall(ctx, chain, map[string]int{"agent1": 1, "end1": 5}, "c3", now, 0); err == nil {
		t.Fatal("sub-tree cap must refuse")
	}
	for _, id := range chain {
		if n, _ := a.LiveConcurrent(ctx, id); n != 1 {
			t.Fatalf("refused call must not register (%s live = %d)", id, n)
		}
	}

	// 4) CPS: the reject still burns a slot in the second's window.
	if err := a.AdmitCall(ctx, chain, nil, "c4", now, 1); err != nil {
		t.Fatalf("cps first admit: %v", err)
	}
	if err := a.AdmitCall(ctx, chain, nil, "c5", now, 1); err == nil {
		t.Fatal("cps over cap must refuse")
	}
	// c5's failed attempt consumed the slot: the next second admits.
	if err := a.AdmitCall(ctx, chain, nil, "c6", now.Add(time.Second), 1); err != nil {
		t.Fatalf("fresh window: %v", err)
	}

	// 5) Concurrent-reject must NOT burn the CPS slot (check order).
	now2 := now.Add(2 * time.Second)
	if err := a.AdmitCall(ctx, chain, map[string]int{"end1": 1}, "c7", now2, 1); err == nil {
		t.Fatal("cap reject")
	}
	// Same second, still under CPS cap — proves the reject did not INCR.
	if err := a.AdmitCall(ctx, chain, nil, "c8", now2, 1); err != nil {
		t.Fatalf("cap reject must not consume the CPS slot: %v", err)
	}
}

func TestAdmitCallAtomicOversell(t *testing.T) {
	// The TOCTOU regression: N goroutines race the cap edge; the atomic
	// script must admit exactly cap and register no more.
	a := newAdm(t)
	ctx := context.Background()
	now := time.Now()
	chain := []string{"end1", "agent1"}
	const cap = 5
	const racers = 32

	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.AdmitCall(ctx, chain, map[string]int{"end1": cap, "agent1": cap}, fmt.Sprintf("race-%d", i), now, 0); err == nil {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := admitted.Load(); got != cap {
		t.Fatalf("admitted %d, want exactly %d", got, cap)
	}
	if n, _ := a.LiveConcurrent(ctx, "end1"); int(n) != cap {
		t.Fatalf("ledger live = %d, want %d", n, cap)
	}
	if n, _ := a.LiveConcurrent(ctx, "agent1"); int(n) != cap {
		t.Fatalf("ancestor ledger live = %d, want %d", n, cap)
	}
}
