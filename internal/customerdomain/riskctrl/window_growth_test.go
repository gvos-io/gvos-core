package riskctrl

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// The engine's local freq window is a shadow record on the Redis
// path: it used to grow without bound — 30k calls left 30k timestamps
// in memory, forever.
func TestLocalFreqWindowIsBounded(t *testing.T) {
	const calls = 3000
	est := money.MustFromYuan(0.06)

	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for _, tc := range []struct {
		name  string
		wire  func(e *Engine)
		rules []Rule
	}{
		{"no rules, no Redis", nil, nil},
		{"freq rule, no Redis", nil, []Rule{{ID: "f", Kind: "freq", Action: "alert", Limit: 1 << 30}}},
		{"freq rule + Redis face", func(e *Engine) { e.SetRiskRedis(NewRiskR(c)) },
			[]Rule{{ID: "f", Kind: "freq", Action: "alert", Limit: 1 << 30}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New()
			if tc.wire != nil {
				tc.wire(e)
			}
			if tc.rules != nil {
				e.SetRules(tc.rules)
			}
			// A fake clock that advances a second per call: the window
			// ages for real instead of every entry sharing one instant.
			t0 := time.Now()
			nc := 0
			e.SetClock(func() time.Time { return t0.Add(time.Duration(nc) * time.Second) })
			for i := 0; i < calls; i++ {
				nc = i
				e.CheckCall("u1", "86", est)
			}
			n := 0
			e.mu.Lock()
			n = len(e.calls["u1"])
			e.mu.Unlock()
			if n > 120 { // a 60s window cannot hold more than a burst
				t.Errorf("local window holds %d entries after %d calls; it grows without bound", n, calls)
			}
		})
	}
}

// dayCost is a per-day map: stale days are dropped (they used to live
// for the process lifetime).
func TestDayCostMapsArePruned(t *testing.T) {
	e := New()
	est := money.MustFromYuan(0.06)
	e.CheckCall("u1", "86", est)

	e.mu.Lock()
	today := dayKey(e.now())
	now := e.now()
	for d := -35; d < 0; d += 5 { // a month of stale days
		e.dayCost[dayKey(now.AddDate(0, 0, d))] = map[string]money.Amount{"u1": money.MustFromYuan(1)}
	}
	e.pruneDaysLocked(e.now())
	stale := 0
	for d := range e.dayCost {
		if d < today-86400 {
			stale++
		}
	}
	e.mu.Unlock()
	if stale != 0 {
		t.Errorf("%d stale dayCost maps survived the prune", stale)
	}
}
