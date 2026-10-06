package callstate

import (
	"testing"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/rate"
)

func setup(t *testing.T, teardown TeardownFunc) (*Manager, *account.Store) {
	t.Helper()
	store := account.NewStore()
	store.Put(&account.Account{ID: "agent1", Credit: 100, RateGroup: "g"})
	store.Put(&account.Account{ID: "end1", ParentID: "agent1", Credit: 100, RateGroup: "g"})
	m := NewManager(store, teardown, time.Second)
	return m, store
}

func endToEndEntry() *rate.Entry {
	return &rate.Entry{Prefix: "86", Price: 0.06, InitDur: 60, StepDur: 6}
}

func TestStartEndFullCycle(t *testing.T) {
	var torn []string
	m, store := setup(t, func(id, reason string) { torn = append(torn, id) })

	err := m.Start("call-1", "end1", "8613800138000", endToEndEntry(), 60)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Live() != 1 {
		t.Fatalf("live = %d, want 1", m.Live())
	}

	cdr, err := m.End("call-1", 66) // 60+6 -> 66s billed, 0.066
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Cost != 0.066 {
		t.Errorf("CDR cost = %.7f, want 0.0660000", cdr.Cost)
	}
	if cdr.BilledSecs != 66 {
		t.Errorf("CDR billedSecs = %d, want 66", cdr.BilledSecs)
	}
	// Lock was 60s estimate (0.06), actual 0.066 -> end1 owes 0.066 net:
	// taken 0.06, charged extra 0.006 at settle.
	if bal, _ := account.Balance(store, "end1"); !approxEqual(bal, 100-0.066, 1e-7) {
		t.Errorf("end balance = %.7f, want %.7f", bal, 100-0.066)
	}
	if m.Live() != 0 {
		t.Errorf("live after end = %d, want 0", m.Live())
	}
	if len(torn) != 0 {
		t.Errorf("no teardown expected, got %v", torn)
	}
}

func TestInsufficientBalanceRejectsStart(t *testing.T) {
	m, _ := setup(t, nil)
	// end1 has 100; a call estimated at 0.06 is fine. Drain the parent instead.
	err := m.Start("call-1", "agent1", "86", endToEndEntry(), 60)
	if err != nil {
		t.Fatalf("agent1 should afford: %v", err)
	}
	m.End("call-1", 60)

	// Now an end1 call with a huge estimate must fail.
	err = m.Start("call-2", "end1", "86", endToEndEntry(), 100000)
	if err == nil {
		t.Fatal("expected insufficient balance error on Start")
	}
	if m.Live() != 0 {
		t.Errorf("failed start must not leave a live call, live = %d", m.Live())
	}
}

func TestTeardownOnExhaustion(t *testing.T) {
	var torn []string
	m, store := setup(t, func(id, reason string) {
		torn = append(torn, id+"|"+reason)
	})

	// Deterministic clock: call starts at T0.
	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	m.Start("call-1", "end1", "86", endToEndEntry(), 60)

	// Advance 60s: interim charge 0.06. Both end1 and agent1 had 100,
	// minus the 0.06 start lock = 99.94. Drain 99.9 so end1 is left with
	// 0.04 — less than the 0.06 interim charge itself.
	cur = base.Add(60 * time.Second)
	store.Reserve("end1", 99.9)

	alive, err := m.Interim("call-1")
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Error("call should be torn down when balance can't cover next tick")
	}
	if len(torn) != 1 || torn[0] != "call-1|balance-exhausted" {
		t.Errorf("teardown calls = %v, want [call-1|balance-exhausted]", torn)
	}
}

func TestUnknownCallErrors(t *testing.T) {
	m, _ := setup(t, nil)
	if _, err := m.Interim("nope"); err == nil {
		t.Error("Interim on unknown call should error")
	}
	if _, err := m.End("nope", 10); err == nil {
		t.Error("End on unknown call should error")
	}
}

func approxEqual(a, b, tol float64) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}
