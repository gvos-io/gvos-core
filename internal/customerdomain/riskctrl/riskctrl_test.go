package riskctrl

import (
	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
	"testing"
	"time"
)

func base(t *testing.T) *Engine {
	t.Helper()
	e := New()
	cur := time.Unix(1_700_000_000, 0)
	e.SetClock(func() time.Time { return cur })
	e.cur = &cur
	return e
}

func TestFreqRuleBlocks(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "r1", Kind: "freq", Action: "block", Limit: 3}})

	for i := 1; i <= 3; i++ {
		if h := e.CheckCall("u1", "861", money.MustFromYuan(0.01)); h.Blocked {
			t.Fatalf("call %d should pass: %+v", i, h)
		}
	}
	h := e.CheckCall("u1", "861", money.MustFromYuan(0.01))
	if !h.Blocked || h.RuleID != "r1" {
		t.Fatalf("4th call in a minute must block: %+v", h)
	}
}

func TestFreqWindowSlides(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "r1", Kind: "freq", Action: "block", Limit: 2}})

	e.CheckCall("u1", "861", money.MustFromYuan(0.01))
	e.CheckCall("u1", "861", money.MustFromYuan(0.01))
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.01)); !h.Blocked {
		t.Fatal("3rd call in window should block")
	}
	// 61 seconds later the window has slid: allowed again.
	*e.cur = e.cur.Add(61 * time.Second)
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.01)); h.Blocked {
		t.Fatalf("call after window slide blocked: %+v", h)
	}
}

func TestCostDayRule(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "c1", Kind: "cost_day", Action: "block", CostLimit: 0.1}})

	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.04)); h.Blocked {
		t.Fatal("first call under the cap")
	}
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.04)); h.Blocked {
		t.Fatal("second call still under (0.08)")
	}
	h := e.CheckCall("u1", "861", money.MustFromYuan(0.04))
	if !h.Blocked || h.RuleID != "c1" {
		t.Fatalf("third call must exceed 0.1: %+v", h)
	}

	// Next day resets.
	*e.cur = e.cur.Add(25 * time.Hour)
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.04)); h.Blocked {
		t.Fatalf("new day blocked: %+v", h)
	}
}

func TestSettleCostReconciles(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "c1", Kind: "cost_day", Action: "block", CostLimit: 0.1}})

	e.CheckCall("u1", "861", money.MustFromYuan(0.04)) // est 0.04
	e.SettleCost("u1", money.MustFromYuan(0.04), money.MustFromYuan(0.02)) // actual 0.02 -> 0.02 spent
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.08)); h.Blocked {
		t.Fatalf("settle should free headroom (0.02+0.08=0.1 at the edge): %+v", h)
	}
}

func TestDestPrefixAdmission(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "p1", Kind: "dest_prefix", Action: "block", Prefixes: []string{"861", "1"}}})

	if h := e.CheckCall("u1", "8613800138000", money.MustFromYuan(0.01)); h.Blocked {
		t.Fatal("admitted prefix blocked")
	}
	if h := e.CheckCall("u1", "9112345678", money.MustFromYuan(0.01)); !h.Blocked {
		t.Fatal("non-admitted destination allowed")
	}
}

func TestAlertActionDoesNotBlock(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "a1", Kind: "freq", Action: "alert", Limit: 1}})

	e.CheckCall("u1", "861", money.MustFromYuan(0.01))
	h := e.CheckCall("u1", "861", money.MustFromYuan(0.01))
	if h.Blocked {
		t.Fatal("alert rules must never block")
	}
	if !h.Alerted || h.RuleID != "a1" {
		t.Fatalf("expected alert hit: %+v", h)
	}
}

func TestRuleAccountScoping(t *testing.T) {
	e := base(t)
	e.SetRules([]Rule{{ID: "s1", Kind: "freq", Action: "block", Limit: 1, Account: "vip"}})

	// The scoped account's first call passes, second blocks.
	if h := e.CheckCall("vip", "861", money.MustFromYuan(0.01)); h.Blocked {
		t.Fatal("first call within limit")
	}
	if h := e.CheckCall("vip", "861", money.MustFromYuan(0.01)); !h.Blocked {
		t.Fatal("vip should hit its own rule on call 2")
	}
	// Unrelated accounts are unaffected (two calls, both pass).
	if h := e.CheckCall("other", "861", money.MustFromYuan(0.01)); h.Blocked {
		t.Fatal("rule must not apply to other accounts")
	}
	if h := e.CheckCall("other", "861", money.MustFromYuan(0.01)); h.Blocked {
		t.Fatal("rule must not apply to other accounts (2nd)")
	}
}

func TestSetRulesLive(t *testing.T) {
	e := base(t)
	if h := e.CheckCall("u1", "861", money.MustFromYuan(100)); h.Blocked {
		t.Fatal("no rules yet")
	}
	e.SetRules([]Rule{{ID: "x", Kind: "cost_day", Action: "block", CostLimit: 1}})
	if h := e.CheckCall("u1", "861", money.MustFromYuan(100)); !h.Blocked {
		t.Fatal("live rule not applied")
	}
}

func BenchmarkCheckCall(b *testing.B) {
	e := New()
	e.SetRules([]Rule{
		{ID: "f", Kind: "freq", Action: "block", Limit: 60},
		{ID: "c", Kind: "cost_day", Action: "block", CostLimit: 100},
		{ID: "p", Kind: "dest_prefix", Action: "block", Prefixes: []string{"861", "1", "44"}},
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.CheckCall("u1", "8613800138000", money.MustFromYuan(0.05))
	}
}
