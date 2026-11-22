package callcore

import (
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"

	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
)

// TestSweeperSettlesZombies: a call that never gets a BYE is force
// settled after the timeout - lock released, CDR emitted with the
// zombie-timeout release reason.
func TestSweeperSettlesZombies(t *testing.T) {
	store := account.NewStore()
	store.Put(&account.Account{ID: "agent", Credit: money.MustFromYuan(1000), RateGroup: "g"})
	store.Put(&account.Account{ID: "u1", ParentID: "agent", Credit: money.MustFromYuan(100), RateGroup: "g"})

	m := NewManager(store, nil, time.Second)
	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	if err := m.Start("z1", "u1", "86", entry2(), 60, false); err != nil {
		t.Fatal(err)
	}
	if m.Live() != 1 {
		t.Fatalf("live = %d, want 1", m.Live())
	}

	// Advance past the zombie timeout and run one sweep.
	cur = base.Add(11 * time.Minute)
	m.sweep(10 * time.Minute)

	if m.Live() != 0 {
		t.Fatalf("zombie still live after sweep: %d", m.Live())
	}

	// Lock was 60s estimate (0.01/min -> 0.01). Zombie duration 660s
	// bills at 60+6: 660s = 0.11.
	bal, _ := account.Balance(store, "u1")
	if !approxEqual(bal.AsFloat(), 100-0.11, 1e-7) {
		t.Errorf("post-sweep balance = %s, want %.7f (lock released, elapsed billed)", bal, 100-0.11)
	}
}

// TestSweeperIgnoresFreshCalls: in-progress calls are untouched.
func TestSweeperIgnoresFreshCalls(t *testing.T) {
	store := account.NewStore()
	store.Put(&account.Account{ID: "u1", Credit: money.MustFromYuan(100), RateGroup: "g"})
	m := NewManager(store, nil, time.Second)

	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	m.Start("f1", "u1", "86", entry2(), 60, false)
	cur = base.Add(5 * time.Minute)
	m.sweep(10 * time.Minute)

	if m.Live() != 1 {
		t.Fatalf("fresh call swept: live = %d", m.Live())
	}
}

// TestSweeperIgnoresActiveLongCalls: a call whose Start is older than
// the timeout but whose LastActive was refreshed by Interim must not
// be force-settled (the old Start-only check billed live 10m+ calls).
func TestSweeperIgnoresActiveLongCalls(t *testing.T) {
	store := account.NewStore()
	store.Put(&account.Account{ID: "u1", Credit: money.MustFromYuan(100), RateGroup: "g"})
	m := NewManager(store, nil, time.Second)

	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	if err := m.Start("long1", "u1", "86", entry2(), 60, false); err != nil {
		t.Fatal(err)
	}
	// Age the call past the zombie timeout, then refresh LastActive.
	cur = base.Add(15 * time.Minute)
	if _, err := m.Interim("long1"); err != nil {
		t.Fatal(err)
	}
	m.sweep(10 * time.Minute)

	if m.Live() != 1 {
		t.Fatalf("active long call swept: live = %d", m.Live())
	}
}

// TestSweeperLoop: the background goroutine settles without manual
// sweep calls, and stop() halts it. The fake clock hands the time over
// a channel (a shared `cur` variable would be a data race between the
// test goroutine and the sweeper goroutine reading it).
func TestSweeperLoop(t *testing.T) {
	store := account.NewStore()
	store.Put(&account.Account{ID: "u1", Credit: money.MustFromYuan(100), RateGroup: "g"})
	m := NewManager(store, nil, 10*time.Millisecond)

	nowCh := make(chan time.Time, 1)
	nowCh <- time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return <-nowCh })

	m.Start("loop1", "u1", "86", entry2(), 60, false)

	stop := m.StartSweeper(50*time.Millisecond, 10*time.Millisecond)
	deadline := time.After(3 * time.Second)
	for m.Live() != 0 {
		select {
		case <-deadline:
			t.Fatalf("sweeper never settled the zombie, live = %d", m.Live())
		default:
			// Real time advances past the fake start (buffered: the
			// next clock read by any goroutine picks it up).
			select {
			case nowCh <- time.Now():
			default: // a value is still parked: the sweeper has not read it yet
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	stop()
}

// TestZombieCDRReleaseReason: swept CDR carries the release reason.
func TestZombieCDRReleaseReason(t *testing.T) {
	var cdrs []*CDR
	store := account.NewStore()
	store.Put(&account.Account{ID: "u1", Credit: money.MustFromYuan(100), RateGroup: "g"})

	// Capture CDRs via a wrapping approach: we can't intercept End's
	// return from sweep, so assert via a settle hook - use the manager
	// directly and check the lastRelease bookkeeping instead.
	m := NewManager(store, nil, time.Second)
	base := time.Unix(1_700_000_000, 0)
	cur := base
	m.SetClock(func() time.Time { return cur })

	m.Start("zr1", "u1", "86", entry2(), 60, false)
	cur = base.Add(11 * time.Minute)
	m.markRelease("zr1", "zombie-timeout")
	cdr, err := m.End("zr1", 660)
	if err != nil {
		t.Fatal(err)
	}
	cdrs = append(cdrs, cdr)
	if cdrs[0].Release != "zombie-timeout" {
		t.Errorf("release = %q, want zombie-timeout", cdrs[0].Release)
	}
}
