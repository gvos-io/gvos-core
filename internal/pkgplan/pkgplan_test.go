package pkgplan

import (
	"testing"
	"time"
)

func setup(t *testing.T) *Engine {
	t.Helper()
	e := New()
	cur := time.Unix(1_700_000_000, 0)
	e.SetClock(func() time.Time { return cur })
	e.cur = &cur
	return e
}

func TestFreeMinutesFirst(t *testing.T) {
	e := setup(t)
	e.Define(Package{ID: "p1", Name: "月套餐", Cycle: Monthly, FreeMinutes: 10, Discount: 1})
	if err := e.Assign("u1", "p1"); err != nil {
		t.Fatal(err)
	}

	// First 10 minutes of billed seconds are free.
	r := e.Apply("u1", 300, 0.3) // 5 min billed
	if r.FreeSecs != 300 || r.BillableSecs != 0 {
		t.Fatalf("first call: %+v", r)
	}
	// Next call: 6 min billed, 5 free left -> 1 min billable.
	r = e.Apply("u1", 360, 0.36)
	if r.FreeSecs != 300 || r.BillableSecs != 60 {
		t.Fatalf("second call: %+v", r)
	}
}

func TestDailyCycleResets(t *testing.T) {
	e := setup(t)
	e.Define(Package{ID: "day", Cycle: Daily, FreeMinutes: 5})
	e.Assign("u1", "day")

	e.Apply("u1", 300, 0.25) // uses all 5 free minutes
	r := e.Apply("u1", 60, 0.05)
	if r.FreeSecs != 0 {
		t.Fatalf("allowance leaked: %+v", r)
	}
	// Next day: allowance back.
	*e.cur = e.cur.Add(26 * time.Hour)
	r = e.Apply("u1", 60, 0.05)
	if r.FreeSecs != 60 || r.BillableSecs != 0 {
		t.Fatalf("after rollover: %+v", r)
	}
}

func TestMonthlyCycleResets(t *testing.T) {
	e := setup(t)
	e.Define(Package{ID: "m", Cycle: Monthly, FreeMinutes: 5})
	e.Assign("u1", "m")
	e.Apply("u1", 300, 0.25)

	// Same month: no reset.
	*e.cur = e.cur.Add(5 * 24 * time.Hour)
	r := e.Apply("u1", 60, 0.05)
	if r.FreeSecs != 0 {
		t.Fatalf("monthly allowance reset early: %+v", r)
	}
	// Next month: reset.
	*e.cur = e.cur.Add(40 * 24 * time.Hour)
	r = e.Apply("u1", 60, 0.05)
	if r.FreeSecs != 60 {
		t.Fatalf("monthly rollover missed: %+v", r)
	}
}

func TestDiscount(t *testing.T) {
	e := setup(t)
	e.Define(Package{ID: "vip", Discount: 0.8, FreeMinutes: 0})
	e.Assign("u1", "vip")
	r := e.Apply("u1", 60, 0.1)
	if r.Discount != 0.8 || r.BillableSecs != 60 {
		t.Fatalf("discount: %+v", r)
	}
}

func TestMinSpendShortfall(t *testing.T) {
	e := setup(t)
	e.Define(Package{ID: "min", Cycle: Monthly, MinSpend: 10, FreeMinutes: 0})
	e.Assign("u1", "min")

	if got := e.MinSpendShortfall("u1"); got != 10 {
		t.Fatalf("fresh cycle shortfall = %v, want 10", got)
	}
	e.Apply("u1", 600, 4) // spend 4
	if got := e.MinSpendShortfall("u1"); got != 6 {
		t.Fatalf("shortfall after spend: %v, want 6", got)
	}
	e.Apply("u1", 1200, 8) // spend 12 total
	if got := e.MinSpendShortfall("u1"); got != 0 {
		t.Fatalf("met minimum: %v, want 0", got)
	}
}

func TestNoPackagePassthrough(t *testing.T) {
	e := setup(t)
	r := e.Apply("ghost", 60, 0.1)
	if r.FreeSecs != 0 || r.BillableSecs != 60 || r.Discount != 1 {
		t.Fatalf("passthrough: %+v", r)
	}
}

func TestAssignUnknownPackage(t *testing.T) {
	e := setup(t)
	if err := e.Assign("u1", "nope"); err == nil {
		t.Fatal("unknown package must error")
	}
}
