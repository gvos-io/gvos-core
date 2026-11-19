package riskctrl

import (
	"testing"
	"time"
)

// TestCheckRegWindow: reg_freq rules fire on the trailing-minute
// window, and every event counts toward it (hit or not).
func TestCheckRegWindow(t *testing.T) {
	e := New()
	e.SetRules([]Rule{{ID: "r1", Kind: "reg_freq", Action: "alert", Limit: 3}})

	base := time.Unix(1_700_000_000, 0)
	cur := base
	e.SetClock(func() time.Time { return cur })

	for i := 0; i < 3; i++ {
		if h := e.CheckReg("u1"); h.Alerted {
			t.Fatalf("hit at event %d with limit 3", i+1)
		}
		cur = cur.Add(5 * time.Second)
	}
	// 4th within the minute: over the limit.
	if h := e.CheckReg("u1"); !h.Alerted {
		t.Fatal("expected alert on 4th registration in window")
	}

	// Window slides: 80s later the burst events (0-20s marks) have all
	// aged out of the trailing minute.
	cur = base.Add(80 * time.Second)
	if h := e.CheckReg("u1"); h.Alerted {
		t.Fatal("window must have slid past the burst")
	}
}

// TestCheckRegAccountScope: scoped rules only see their account. The
// limit is inclusive of the current attempt (same semantics as the
// call-path freq rules: Limit=1 alerts on the second registration).
func TestCheckRegAccountScope(t *testing.T) {
	e := New()
	e.SetRules([]Rule{{ID: "r1", Kind: "reg_freq", Action: "block", Limit: 1, Account: "vip"}})
	if h := e.CheckReg("other"); h.Blocked {
		t.Fatal("scoped rule must not fire for other users")
	}
	if h := e.CheckReg("vip"); h.Blocked {
		t.Fatal("first registration is within limit (inclusive counting)")
	}
	if h := e.CheckReg("vip"); !h.Blocked {
		t.Fatal("second registration exceeds the scoped limit")
	}
}
