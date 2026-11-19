package billing

import (
	"testing"

	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
	"github.com/fastvoip/gvos-core/internal/billingdomain/rate"
)

// entry builds a rate entry for tests: price/min, initDur+stepDur.
func entry(price float64, initDur, stepDur int) *rate.Entry {
	return &rate.Entry{Prefix: "86", Price: money.MustFromYuan(price), InitDur: initDur, StepDur: stepDur}
}

func TestChargeSchemes(t *testing.T) {
	c := &Charger{}
	cases := []struct {
		name     string
		e        *rate.Entry
		secs     int
		wantSec  int
		wantCost money.Amount
	}{
		// 60+6: 30s talk -> 60s billed, one init minute.
		{"60+6 short", entry(0.06, 60, 6), 30, 60, money.MustFromYuan(0.06)},
		// 60+6: 61s talk -> 60+6=66s billed, 1.1 minutes.
		{"60+6 over", entry(0.06, 60, 6), 61, 66, money.MustFromYuan(0.066)},
		// 60+6: 66s talk -> exactly 66s.
		{"60+6 exact", entry(0.06, 60, 6), 66, 66, money.MustFromYuan(0.066)},
		// 6+6: 7s -> 12s billed.
		{"6+6", entry(0.06, 6, 6), 7, 12, money.MustFromYuan(0.012)},
		// 1+1: 7s -> 7s billed, per-second price = price/60.
		{"1+1", entry(0.06, 1, 1), 7, 7, money.MustFromYuan(0.007)},
		// N+M (30+5): 36s -> init 30 + two 5s steps (31..35, 36..40) = 40s.
		{"30+5", entry(0.06, 30, 5), 36, 40, money.MustFromYuan(0.04)},
	}
	for _, tc := range cases {
		gotSec, gotCost, err := c.ChargeFor(tc.e, tc.secs)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if gotSec != tc.wantSec {
			t.Errorf("%s: billedSecs = %d, want %d", tc.name, gotSec, tc.wantSec)
		}
		if gotCost != tc.wantCost {
			t.Errorf("%s: cost = %s, want %s", tc.name, gotCost, tc.wantCost)
		}
	}
}

func TestZeroAndEdge(t *testing.T) {
	c := &Charger{}
	if sec, cost, _ := c.ChargeFor(entry(0.06, 60, 6), 0); sec != 0 || cost != 0 {
		t.Errorf("zero duration should cost 0, got %d/%s", sec, cost)
	}
	if _, _, err := c.ChargeFor(nil, 10); err == nil {
		t.Error("nil entry should error")
	}
	if _, _, err := c.ChargeFor(entry(0.06, 60, 6), -1); err == nil {
		t.Error("negative duration should error")
	}
}

func TestMinDur(t *testing.T) {
	c := &Charger{}
	// 6+6 steps with MinDur 30: 6s talk lifts to 30s, price is per-minute
	// so 30s at 0.06/min = 0.03.
	e := &rate.Entry{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 6, StepDur: 6, MinDur: 30}
	sec, cost, _ := c.ChargeFor(e, 6)
	if sec != 30 || cost != money.MustFromYuan(0.03) {
		t.Errorf("MinDur: got %d/%s, want 30/0.0300", sec, cost)
	}
}

func TestEstimate(t *testing.T) {
	c := &Charger{}
	if got := c.EstimatedCost(entry(0.06, 60, 6), 60); got != money.MustFromYuan(0.06) {
		t.Errorf("estimate 60s = %s, want 0.0600", got)
	}
	if got := c.EstimatedCost(entry(0.06, 60, 6), 0); got != 0 {
		t.Errorf("estimate 0s = %s, want 0", got)
	}
}

func TestPrecisionCeilAtScale(t *testing.T) {
	c := &Charger{}
	// 1+1 with price 0.1: 7 seconds -> 7/600 = 0.0116666... -> ceil at
	// the 4th decimal (ADR-0003 scale) = 0.0117. The legacy 7-decimal
	// rule maps to ceil-at-scale; never undercharge.
	_, cost, _ := c.ChargeFor(entry(0.1, 1, 1), 7)
	if cost != money.MustFromYuan(0.0117) {
		t.Errorf("precision: cost = %s, want 0.0117 (ceil at 4th decimal)", cost)
	}
}
