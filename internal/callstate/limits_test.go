package callstate

import (
	"errors"
	"testing"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/rate"
)

func limitSetup(t *testing.T) (*Manager, *account.Store) {
	t.Helper()
	store := account.NewStore()
	store.Put(&account.Account{ID: "tenant", RateGroup: "g", Postpaid: true})
	store.Put(&account.Account{ID: "agent", ParentID: "tenant", Credit: 1e6, RateGroup: "g", Concurrent: 2})
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e3, RateGroup: "g"})
	store.Put(&account.Account{ID: "user-b", ParentID: "agent", Credit: 1e3, RateGroup: "g", CPS: 1})
	m := NewManager(store, nil, time.Second)
	return m, store
}

func entry2() *rate.Entry {
	return &rate.Entry{Prefix: "86", Price: 0.01, InitDur: 60, StepDur: 6}
}

// Concurrent cap on the account itself.
func TestConcurrentLimitOwn(t *testing.T) {
	m, store := limitSetup(t)
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e3, Concurrent: 2})

	for i := 1; i <= 2; i++ {
		if err := m.Start("c"+string(rune('0'+i)), "user-a", "86", entry2(), 60); err != nil {
			t.Fatalf("call %d should pass: %v", i, err)
		}
	}
	err := m.Start("c3", "user-a", "86", entry2(), 60)
	var limit *ErrLimit
	if !errors.As(err, &limit) || limit.Kind != "concurrent" {
		t.Fatalf("expected concurrent limit error, got %v", err)
	}

	// End one call -> the next passes again.
	m.End("c1", 60)
	if err := m.Start("c3", "user-a", "86", entry2(), 60); err != nil {
		t.Fatalf("start after end should pass: %v", err)
	}
}

// Agent cap counts the whole sub-tree: two users under one agent with
// agent cap 2 -> third call from either user is rejected.
func TestConcurrentLimitAncestorSubtree(t *testing.T) {
	m, _ := limitSetup(t) // agent has Concurrent: 2

	if err := m.Start("a1", "user-a", "86", entry2(), 60); err != nil {
		t.Fatalf("call 1 (user-a): %v", err)
	}
	if err := m.Start("b1", "user-b", "86", entry2(), 60); err != nil {
		t.Fatalf("call 2 (user-b): %v", err)
	}
	err := m.Start("a2", "user-a", "86", entry2(), 60)
	var limit *ErrLimit
	if !errors.As(err, &limit) || limit.Kind != "concurrent" || limit.AccountID != "agent" {
		t.Fatalf("expected agent sub-tree limit, got %+v", err)
	}
}

// CPS cap: user-b allows 1 start per second.
func TestCPSLimit(t *testing.T) {
	m, _ := limitSetup(t)

	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	if err := m.Start("p1", "user-b", "86", entry2(), 60); err != nil {
		t.Fatalf("first call in second: %v", err)
	}
	err := m.Start("p2", "user-b", "86", entry2(), 60)
	var limit *ErrLimit
	if !errors.As(err, &limit) || limit.Kind != "cps" {
		t.Fatalf("expected cps limit, got %v", err)
	}

	// Next second passes.
	cur = base.Add(time.Second)
	if err := m.Start("p2", "user-b", "86", entry2(), 60); err != nil {
		t.Fatalf("call in next second: %v", err)
	}
}

// Unlimited accounts (cap 0) never reject; a user directly under the
// postpaid tenant root escapes the capped agent sub-tree.
func TestNoLimitWhenZero(t *testing.T) {
	m, store := limitSetup(t)
	store.Put(&account.Account{ID: "user-c", ParentID: "tenant", Credit: 1e9, RateGroup: "g"})

	for i := 0; i < 50; i++ {
		if err := m.Start("n"+string(rune('a'+i)), "user-c", "86", entry2(), 60); err != nil {
			t.Fatalf("call %d on unlimited account: %v", i, err)
		}
	}
}
