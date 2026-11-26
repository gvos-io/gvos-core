package rate

import (
	"testing"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
)

// Regression tests for the 2026-11 audit.

// A prefix with only a night-window variant used to charge the night
// price at noon: out of every window the FIRST defined variant won,
// whichever that was.
func TestWindowOnlyPrefixFallsBackToCheapest(t *testing.T) {
	night := money.MustFromYuan(0.01)
	day := money.MustFromYuan(0.03)
	tbl := NewTable([]Rate{
		{Prefix: "1", Price: night, InitDur: 60, StepDur: 6,
			Effective: []Interval{{Start: "23:00", End: "06:00"}}},
		{Prefix: "1", Price: day, InitDur: 60, StepDur: 6,
			Effective: []Interval{{Start: "06:00", End: "23:00"}}},
	})
	if e := tbl.Lookup("12125550123", 1, 12*60); e == nil || e.Price != day {
		t.Errorf("daytime lookup = %+v, want price %s", e, day)
	}
	if e := tbl.Lookup("12125550123", 1, 23*60+30); e == nil || e.Price != night {
		t.Errorf("night lookup = %+v, want price %s", e, night)
	}
}

// The documented behaviour: a base variant still wins over windowed
// ones outside their windows.
func TestWindowedVariantStillBeatsBaseInsideWindow(t *testing.T) {
	base := money.MustFromYuan(0.03)
	night := money.MustFromYuan(0.015)
	tbl := NewTable([]Rate{
		{Prefix: "1", Price: base, InitDur: 60, StepDur: 6},
		{Prefix: "1", Price: night, InitDur: 60, StepDur: 6,
			Effective: []Interval{{Start: "23:00", End: "06:00"}}},
	})
	if e := tbl.Lookup("12125550123", 1, 23*60+30); e == nil || e.Price != night {
		t.Errorf("night lookup = %+v, want price %s", e, night)
	}
	if e := tbl.Lookup("12125550123", 1, 12*60); e == nil || e.Price != base {
		t.Errorf("day lookup = %+v, want price %s", e, base)
	}
}
