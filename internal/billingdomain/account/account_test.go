package account

import (
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"strings"
	"testing"
)

func chain() *Store {
	s := NewStore()
	s.Put(&Account{ID: "tenant", Name: "Tenant", RateGroup: "base", Postpaid: true})
	s.Put(&Account{ID: "agent1", ParentID: "tenant", RateGroup: "wholesale", Credit: money.MustFromYuan(100)})
	s.Put(&Account{ID: "agent2", ParentID: "agent1", RateGroup: "retail", Credit: money.MustFromYuan(50)})
	s.Put(&Account{ID: "end1", ParentID: "agent2", RateGroup: "retail", Credit: money.MustFromYuan(10)})
	return s
}

func TestChainOrder(t *testing.T) {
	s := chain()
	c, err := s.Chain("end1")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, a := range c {
		got = append(got, a.ID)
	}
	want := "end1,agent2,agent1,tenant"
	if strings.Join(got, ",") != want {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestReserveAllOrNothing(t *testing.T) {
	s := chain()
	// end1 has 10, needs 9: whole chain gets reserved.
	if _, err := s.Reserve("end1", money.MustFromYuan(9)); err != nil {
		t.Fatalf("reserve 9 should pass: %v", err)
	}
	for id, want := range map[string]money.Amount{"end1": money.MustFromYuan(1), "agent2": money.MustFromYuan(41), "agent1": money.MustFromYuan(91), "tenant": 0} {
		if bal, _ := Balance(s, id); bal != want {
			t.Errorf("balance[%s] = %v, want %v", id, bal, want)
		}
	}

	// Now end1 has 1: another reserve of 5 must fail AND roll back agent2/agent1.
	_, err := s.Reserve("end1", money.MustFromYuan(5))
	if err == nil || !strings.Contains(err.Error(), "insufficient") {
		t.Fatalf("expected insufficient-balance error, got %v", err)
	}
	if bal, _ := Balance(s, "agent2"); bal != money.MustFromYuan(41) {
		t.Errorf("rollback failed: agent2 balance = %v, want 41", bal)
	}
	if bal, _ := Balance(s, "agent1"); bal != money.MustFromYuan(91) {
		t.Errorf("rollback failed: agent1 balance = %v, want 91", bal)
	}
}

func TestCommitRefund(t *testing.T) {
	s := chain()
	s.Reserve("end1", money.MustFromYuan(9))
	// Actual cost 4: refund 5.
	if err := s.Commit("tok", "end1", money.MustFromYuan(9), money.MustFromYuan(4)); err != nil {
		t.Fatal(err)
	}
	if bal, _ := Balance(s, "end1"); bal != money.MustFromYuan(6) {
		t.Errorf("end1 = %v, want 6", bal)
	}
}

func TestCycleDetection(t *testing.T) {
	s := NewStore()
	s.Put(&Account{ID: "a", ParentID: "b"})
	s.Put(&Account{ID: "b", ParentID: "a"})
	if _, err := s.Chain("a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}
