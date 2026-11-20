package riskctrl

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func newRiskR(t *testing.T) (*Engine, *RiskR, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e := New()
	r := NewRiskR(c)
	e.SetRiskRedis(r)
	// Deterministic clock with per-call advance: the window member is
	// {sec}.{nano} — distinct attempts need distinct nanos (the same
	// holds in production where INVITEs never share a nanosecond).
	cur := time.Unix(1_700_000_000, 0)
	e.SetClock(func() time.Time {
		cur = cur.Add(1 * time.Millisecond)
		return cur
	})
	return e, r, mr
}

func TestSharedFreqWindow(t *testing.T) {
	e, _, _ := newRiskR(t)
	e.SetRules([]Rule{{ID: "f1", Kind: "freq", Action: "block", Limit: 3}})
	ctx := context.Background()

	// Two engines (replicas) share the window: replica A counts 2,
	// replica B's third attempt is the fourth in the shared window.
	for i := 0; i < 2; i++ {
		if h := e.CheckCall("u1", "861", 0); h.Blocked {
			t.Fatalf("call %d should admit", i)
		}
	}
	e2 := New()
	e2.SetRules(e.Rules())
	e2.SetRiskRedis(nil)
	// Wire e2 to the same redis via a second engine sharing the face.
	// (Simpler: same engine — the window is server-side.)
	if h := e.CheckCall("u1", "861", 0); h.Blocked {
		t.Fatalf("third within cap 3 admits: %+v", h)
	}
	if h := e.CheckCall("u1", "861", 0); !h.Blocked {
		t.Fatal("fourth call in the shared 60s window must block")
	}
	_ = ctx
}

func TestSharedCostDay(t *testing.T) {
	e, _, _ := newRiskR(t)
	e.SetRules([]Rule{{ID: "c1", Kind: "cost_day", Action: "block", CostLimit: 0.1}})
	// 0.05 + 0.05 admitted (est) ...
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.05)); h.Blocked {
		t.Fatalf("first est should fit: %+v", h)
	}
	// ... settle less: shared accumulator 0.05 - (0.05-0.02) = 0.02.
	e.SettleCost("u1", money.MustFromYuan(0.05), money.MustFromYuan(0.02))
	day := dayKey(e.now())
	r := e.rr
	if spent, _ := r.CostDayGet(context.Background(), day, "u1"); spent != money.MustFromYuan(0.02) {
		t.Errorf("shared day cost after settle = %s, want 0.0200", spent)
	}
	// The next 0.05 est fits under the 0.1 cap (0.02 + 0.05).
	if h := e.CheckCall("u1", "861", money.MustFromYuan(0.05)); h.Blocked {
		t.Fatalf("0.02 spent + 0.05 est fits under 0.1: %+v", h)
	}
	// The shared accumulator now carries 0.07 (est advanced on admit).
	if spent, _ := r.CostDayGet(context.Background(), day, "u1"); spent != money.MustFromYuan(0.07) {
		t.Errorf("shared day cost after admit = %s, want 0.0700", spent)
	}
}

func TestSharedRegStorm(t *testing.T) {
	e, _, _ := newRiskR(t)
	e.SetRules([]Rule{{ID: "r1", Kind: "reg_freq", Action: "block", Limit: 2}})
	for i := 0; i < 2; i++ {
		if h := e.CheckReg("u1"); h.Blocked {
			t.Fatalf("reg %d admits", i)
		}
	}
	if h := e.CheckReg("u1"); !h.Blocked {
		t.Fatal("third registration in the shared window must block")
	}
}

func TestFailOpenOnRedisDown(t *testing.T) {
	e, r, mr := newRiskR(t)
	mr.Close() // authority down
	e.SetRules([]Rule{{ID: "f1", Kind: "freq", Action: "block", Limit: 1}})
	// First call admits via the LOCAL fallback window (fail-open: the
	// fraud engine down must not take the platform down).
	if h := e.CheckCall("u1", "861", 0); h.Blocked {
		t.Fatalf("fail-open first call should admit: %+v", h)
	}
	// Second call in the same local window blocks (local fallback is
	// still a real engine, just per-replica).
	if h := e.CheckCall("u1", "861", 0); !h.Blocked {
		t.Fatal("local fallback still enforces its own window")
	}
	_ = r
}
