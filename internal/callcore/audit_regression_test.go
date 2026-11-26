package callcore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// Regression tests for the 2026-11 billing audit. Each one pins a
// behaviour that was wrong in production shape: unanswered calls
// accruing forever, Call-ID rides, settle locks tripping reused
// Call-IDs, postpaid and credit-line callers hung up, authority outages
// tearing down every live call, ancestor CPS ignored.

var regBase = time.Unix(1_700_000_000, 0)

// regEntry is 0.06/min, 60+6.
func regEntry() *rate.Entry {
	return &rate.Entry{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 60, StepDur: 6}
}

// regRig is a manager on a fake clock with its teardowns recorded.
type regRig struct {
	cur time.Time
	m   *Manager

	mu sync.Mutex
	td []string
}

func newRegRig(store *account.Store, interval time.Duration) *regRig {
	r := &regRig{cur: regBase}
	r.m = NewManager(store, func(id, reason string) {
		r.mu.Lock()
		r.td = append(r.td, id+"|"+reason)
		r.mu.Unlock()
	}, interval)
	r.m.SetClock(func() time.Time { return r.cur })
	return r
}

func (r *regRig) teardowns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.td...)
}

func (r *regRig) advance(d time.Duration) { r.cur = r.cur.Add(d) }

// balance reads the live authority (Redis when the rig has one, else
// the in-memory store).
func (r *regRig) balance(t *testing.T, id string) money.Amount {
	t.Helper()
	if r.m.bal != nil {
		bal, err := r.m.bal.GetBalance(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return bal
	}
	return regBalance(t, r.m.store, id)
}

func regStore(t *testing.T, accs ...*account.Account) *account.Store {
	t.Helper()
	s := account.NewStore()
	for _, a := range accs {
		if err := s.Put(a); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func regBalance(t *testing.T, s *account.Store, id string) money.Amount {
	t.Helper()
	bal, err := account.Balance(s, id)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

// A failed INVITE gets no end event from the edge. It must hold only
// its pre-answer lock, lose that at the ring timeout, and cost nothing.
// It used to accrue from the INVITE time until the balance ran out —
// Interim kept refreshing the activity clock, so the zombie sweeper
// could never reclaim it.
func TestUnansweredCallNeverAccrues(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	if err := m.Start("na", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	// Three hours of interim passes with the sweeper on its 30s cadence.
	for i := 0; i < 1080; i++ {
		r.advance(10 * time.Second)
		m.driveInterim()
		if i%3 == 2 {
			m.sweep(10 * time.Minute)
		}
		if i == 23 && m.Live() != 1 { // 4 minutes: inside the ring timeout
			t.Fatalf("ringing call vanished before the ring timeout: live = %d", m.Live())
		}
	}
	if m.Live() != 0 {
		t.Fatalf("unanswered call still live after 3h: live = %d", m.Live())
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(10) {
		t.Errorf("balance after an unanswered call = %s, want 10.0000 (nothing charged)", bal)
	}
	if td := r.teardowns(); len(td) != 0 {
		t.Errorf("teardowns = %v, want none (nothing to hang up)", td)
	}
}

// The pre-answer lock is held exactly until the ring timeout.
func TestUnansweredCallHoldsLockUntilRingTimeout(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	m.SetRingTimeout(2 * time.Minute)
	if err := m.Start("na", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(9.94) {
		t.Fatalf("balance at INVITE = %s, want 9.9400 (lock held)", bal)
	}
	r.advance(90 * time.Second)
	m.sweep(10 * time.Minute)
	if m.Live() != 1 || regBalance(t, store, "u1") != money.MustFromYuan(9.94) {
		t.Fatalf("inside the ring timeout: live = %d balance = %s, want 1 and 9.9400", m.Live(), regBalance(t, store, "u1"))
	}
	r.advance(60 * time.Second)
	m.sweep(10 * time.Minute)
	if m.Live() != 0 || regBalance(t, store, "u1") != money.MustFromYuan(10) {
		t.Fatalf("past the ring timeout: live = %d balance = %s, want 0 and 10.0000", m.Live(), regBalance(t, store, "u1"))
	}
}

// An unanswered call's CDR carries no cost and says why.
func TestUnansweredEndBillsNothing(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	r.m.Start("na", "u1", "8613800138000", regEntry(), 60, false)
	cdr, err := r.m.End("na", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Cost != 0 || cdr.BilledSecs != 0 || cdr.Release != ReleaseNoAnswer {
		t.Errorf("CDR = cost %s billed %d release %q, want 0 / 0 / %q", cdr.Cost, cdr.BilledSecs, cdr.Release, ReleaseNoAnswer)
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(10) {
		t.Errorf("balance = %s, want 10.0000 (lock released)", bal)
	}
}

// Talk time counts from the answer, not the INVITE.
func TestAnsweredCallBillsFromAnswer(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	m.Start("a1", "u1", "8613800138000", regEntry(), 60, false)
	r.advance(50 * time.Second) // rings for 50s
	if err := m.Answer("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Answer("a1"); err != nil { // retransmitted 200 OK
		t.Fatalf("second answer: %v", err)
	}
	r.advance(20 * time.Second) // 20s of talk, 70s since the INVITE
	alive, err := m.Interim("a1")
	if err != nil || !alive {
		t.Fatalf("interim: alive=%v err=%v", alive, err)
	}
	// 20s of talk is inside the first step (0.06); from the INVITE it
	// would already be 0.072.
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(10-0.06-0.06) {
		t.Errorf("balance = %s, want 9.8800 (lock 0.06 + one first step)", bal)
	}
}

// The answer may land on a replica that does not own the call (HTTP
// tier behind an LB without Call-ID stickiness). It is parked on the
// shared meta and the owner adopts it.
func TestUnansweredCallAdoptsPeerAnswer(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	owner := newRegRig(store, 10*time.Second)
	owner.m.SetCallMetaStore(NewCallMetaStore(c))
	peer := newRegRig(store, 10*time.Second)
	peer.m.SetCallMetaStore(NewCallMetaStore(c))

	if err := owner.m.Start("p1", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	peer.advance(time.Second) // the 200 OK reaches the peer 1s after the INVITE
	if err := peer.m.Answer("p1"); !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("peer answer: err = %v, want ErrUnknownCall (not its call)", err)
	}

	owner.advance(20 * time.Second)
	owner.m.Interim("p1") // below the lookup delay: still not adopted
	if _, ok := owner.m.AnswerTime("p1"); ok {
		t.Fatal("answer adopted before the lookup delay")
	}
	owner.advance(15 * time.Second)
	owner.m.Interim("p1")
	at, ok := owner.m.AnswerTime("p1")
	if !ok {
		t.Fatal("owner never adopted the answer parked by the peer")
	}
	if want := regBase.Add(time.Second); !at.Equal(want) {
		t.Errorf("answer time = %v, want %v", at, want)
	}
}

// A second dialog under a live Call-ID is refused; the same dialog
// again is a harmless retransmission. Accepting the second dialog let
// a caller ride an existing Call-ID unbilled and untracked.
func TestCallIDGuard(t *testing.T) {
	store := regStore(t,
		&account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"},
		&account.Account{ID: "u2", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	if err := m.StartDialog("dup", "u1", "tag-a", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(9.94) {
		t.Fatalf("balance after first start = %s, want 9.9400", bal)
	}

	if err := m.StartDialog("dup", "u1", "tag-a", "8613800138000", regEntry(), 60, false); err != nil {
		t.Errorf("retransmission of the same dialog: err = %v, want nil", err)
	}
	if err := m.StartDialog("dup", "u1", "tag-b", "8613800138000", regEntry(), 60, false); !errors.Is(err, ErrCallIDInUse) {
		t.Errorf("second dialog under a live Call-ID: err = %v, want ErrCallIDInUse", err)
	}
	if err := m.StartDialog("dup", "u2", "tag-a", "8613800138000", regEntry(), 60, false); !errors.Is(err, ErrCallIDInUse) {
		t.Errorf("another account under a live Call-ID: err = %v, want ErrCallIDInUse", err)
	}
	// Edges that predate the tag send none: same account stays idempotent.
	if err := m.Start("dup", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Errorf("tagless repeat of the same account: err = %v, want nil", err)
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(9.94) {
		t.Errorf("balance after the repeats = %s, want 9.9400 (one lock only)", bal)
	}
	if bal := regBalance(t, store, "u2"); bal != money.MustFromYuan(10) {
		t.Errorf("u2 balance = %s, want 10.0000 (its start was refused)", bal)
	}
	if m.Live() != 1 {
		t.Errorf("live = %d, want 1", m.Live())
	}

	// Once the call is over the Call-ID is free again.
	if _, err := m.End("dup", 60); err != nil {
		t.Fatal(err)
	}
	if err := m.StartDialog("dup", "u2", "tag-c", "8613800138000", regEntry(), 60, false); err != nil {
		t.Errorf("new call on a released Call-ID: err = %v, want nil", err)
	}
}

// newSharedRig wires two managers (an owner and a peer) onto one
// authority + meta, sharing the owner's clock so both see one timeline.
// The local admission path writes the meta off the hot path; waitMeta
// blocks until it has landed.
func newSharedRig(t *testing.T, store *account.Store) (owner, peer *regRig, waitMeta func(callID string)) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	bal := account.NewBalanceR(store, c)
	if err := bal.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	meta := NewCallMetaStore(c)
	owner, peer = newRegRig(store, 10*time.Second), newRegRig(store, 10*time.Second)
	for _, r := range []*regRig{owner, peer} {
		r.m.SetBalanceAuthority(bal)
		r.m.SetCallMetaStore(meta)
	}
	peer.m.SetClock(func() time.Time { return owner.cur })
	waitMeta = func(callID string) {
		t.Helper()
		for i := 0; i < 500 && !mr.Exists(callMetaKey(callID)); i++ {
			time.Sleep(2 * time.Millisecond)
		}
		if !mr.Exists(callMetaKey(callID)) {
			t.Fatalf("call meta for %s never landed", callID)
		}
	}
	return owner, peer, waitMeta
}

// Gear that reuses Call-IDs for consecutive calls: the second call must
// settle normally. Its End used to be refused as "already settled"
// inside the 2-minute lock TTL, leaving the lock and accruals hanging.
func TestCallIDReuseAfterEndSettlesNewCall(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	owner, _, _ := newSharedRig(t, store)
	m := owner.m
	m.Start("x", "u1", "8613800138000", regEntry(), 60, false)
	owner.advance(60 * time.Second)
	if _, err := m.End("x", 60); err != nil {
		t.Fatal(err)
	}
	if bal := owner.balance(t, "u1"); bal != money.MustFromYuan(9.94) {
		t.Fatalf("balance after call 1 = %s, want 9.9400", bal)
	}

	owner.advance(30 * time.Second) // a new call, same Call-ID, 30s later
	start2 := owner.cur
	if err := m.Start("x", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	m.Answer("x")
	for i := 1; i <= 7; i++ { // 70s of talk, interim every 10s
		owner.cur = start2.Add(time.Duration(i) * 10 * time.Second)
		m.Interim("x")
	}
	cdr, err := m.End("x", 70)
	if err != nil {
		t.Fatalf("End of the second call: %v (its money would be left hanging)", err)
	}
	if cdr.Cost != money.MustFromYuan(0.072) {
		t.Errorf("call 2 cost = %s, want 0.0720", cdr.Cost)
	}
	if bal := owner.balance(t, "u1"); bal != money.MustFromYuan(10-0.06-0.072) {
		t.Errorf("balance after call 2 = %s, want 9.8680", bal)
	}
	if m.Live() != 0 {
		t.Errorf("live = %d, want 0", m.Live())
	}
}

// A postpaid account invoices: it is never cut off for a low balance.
// It used to be hung up after exactly one minute when it held zero
// prepaid balance.
func TestPostpaidZeroBalanceStaysUp(t *testing.T) {
	store := regStore(t, &account.Account{ID: "p1", Credit: 0, MaxDebt: money.MustFromYuan(1000), Postpaid: true, RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	if err := m.Start("pp", "p1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	m.Answer("pp")
	for i := 0; i < 90; i++ { // 15 minutes of talk
		r.advance(10 * time.Second)
		if alive, err := m.Interim("pp"); err != nil || !alive {
			t.Fatalf("postpaid call at +%d0s: alive=%v err=%v teardowns=%v", i+1, alive, err, r.teardowns())
		}
	}
	if td := r.teardowns(); len(td) != 0 {
		t.Errorf("teardowns = %v, want none", td)
	}
}

// A prepaid account spends its credit line (MaxDebt) before it is hung
// up. The line used to be subtracted instead of added, so the call was
// cut off while the balance was still positive.
func TestCreditLineIsSpentBeforeTeardown(t *testing.T) {
	store := regStore(t, &account.Account{ID: "d1", Credit: money.MustFromYuan(0.20), MaxDebt: money.MustFromYuan(0.10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	if err := m.Start("cl", "d1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	m.Answer("cl")
	aliveAt := func(sec int) bool {
		r.advance(10 * time.Second)
		alive, err := m.Interim("cl")
		if err != nil {
			t.Fatalf("interim at +%ds: %v", sec, err)
		}
		return alive
	}
	for sec := 10; sec <= 200; sec += 10 {
		if !aliveAt(sec) {
			t.Fatalf("torn down at +%ds with balance %s (credit line not honoured)", sec, regBalance(t, store, "d1"))
		}
	}
	if bal := regBalance(t, store, "d1"); bal >= 0 {
		t.Fatalf("balance = %s at +200s, want the credit line in use (negative)", bal)
	}
	// 0.06/min drains 0.20 + 0.10 in ~5 minutes of talk.
	alive := true
	for sec := 210; sec <= 600 && alive; sec += 10 {
		alive = aliveAt(sec)
	}
	if alive {
		t.Fatal("call still up at +600s: the credit line was never exhausted")
	}
	td := r.teardowns()
	if len(td) != 1 || td[0] != "cl|balance-exhausted" {
		t.Errorf("teardowns = %v, want [cl|balance-exhausted]", td)
	}
}

// flakyAuthority is a balance authority that can be switched off.
type flakyAuthority struct {
	mu    sync.Mutex
	down  bool
	store *account.Store
}

func (f *flakyAuthority) failing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down
}

func (f *flakyAuthority) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func (f *flakyAuthority) ReserveCtx(ctx context.Context, id string, est money.Amount) (string, error) {
	if f.failing() {
		return "", account.ErrUnavailable
	}
	return f.store.Reserve(id, est)
}

func (f *flakyAuthority) CommitCtx(ctx context.Context, token, id string, est, actual money.Amount) error {
	if f.failing() {
		return account.ErrUnavailable
	}
	return f.store.Commit(token, id, est, actual)
}

func (f *flakyAuthority) GetBalance(ctx context.Context, id string) (money.Amount, error) {
	if f.failing() {
		return 0, account.ErrUnavailable
	}
	return account.Balance(f.store, id)
}

// An authority outage is not a broke caller: live calls stay up and
// End settles the full cost. The outage used to force-release every
// live call as "balance-exhausted".
func TestAuthorityOutageKeepsCallsUp(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	r := newRegRig(store, 10*time.Second)
	m := r.m
	auth := &flakyAuthority{store: store}
	m.SetBalanceAuthority(auth)
	if err := m.Start("o1", "u1", "8613800138000", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	m.Answer("o1")

	auth.setDown(true)
	for sec := 20; sec <= 300; sec += 10 {
		r.advance(10 * time.Second)
		if alive, err := m.Interim("o1"); err != nil || !alive {
			t.Fatalf("during the outage at +%ds: alive=%v err=%v teardowns=%v", sec, alive, err, r.teardowns())
		}
	}
	if td := r.teardowns(); len(td) != 0 {
		t.Errorf("teardowns during the outage = %v, want none", td)
	}

	// A new call is still refused while the authority is down.
	if err := m.Start("o2", "u1", "8613800138000", regEntry(), 60, false); !errors.Is(err, account.ErrUnavailable) {
		t.Errorf("start during the outage: err = %v, want ErrUnavailable", err)
	}

	auth.setDown(false)
	cdr, err := m.End("o1", 300)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Release != "normal" {
		t.Errorf("release = %q, want normal", cdr.Release)
	}
	// 300s of talk at 60+6: 0.06 + 240/60*0.06 = 0.30, settled in full.
	if cdr.Cost != money.MustFromYuan(0.30) {
		t.Errorf("cost = %s, want 0.3000 (the whole call)", cdr.Cost)
	}
	if bal := regBalance(t, store, "u1"); bal != money.MustFromYuan(10-0.30) {
		t.Errorf("balance = %s, want 9.7000", bal)
	}
}

// An ancestor's CPS cap bounds the starts of its whole sub-tree, on
// both admission paths. It used to be ignored: only the calling
// account's own cap was checked.
func TestAncestorCPSEnforcedLocal(t *testing.T) {
	newTree := func(cps int) *account.Store {
		return regStore(t,
			&account.Account{ID: "tenant", Postpaid: true},
			&account.Account{ID: "agent", ParentID: "tenant", CPS: cps},
			&account.Account{ID: "u1", ParentID: "agent", Credit: money.MustFromYuan(1000)},
			&account.Account{ID: "u2", ParentID: "agent", Credit: money.MustFromYuan(1000)})
	}
	r := newRegRig(newTree(1), 10*time.Second)
	if err := r.m.Start("c1", "u1", "86", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	err := r.m.Start("c2", "u2", "86", regEntry(), 60, false)
	var limit *ErrLimit
	if !errors.As(err, &limit) || limit.Kind != "cps" || limit.AccountID != "agent" {
		t.Fatalf("second start in the same second under agent CPS=1: err = %v, want a cps ErrLimit for agent", err)
	}
	r.advance(time.Second)
	if err := r.m.Start("c3", "u2", "86", regEntry(), 60, false); err != nil {
		t.Errorf("next second: err = %v, want nil", err)
	}

	r2 := newRegRig(newTree(0), 10*time.Second)
	for i := 0; i < 5; i++ {
		if err := r2.m.Start("n"+string(rune('a'+i)), "u1", "86", regEntry(), 60, false); err != nil {
			t.Fatalf("uncapped agent: err = %v", err)
		}
	}
}

func TestAncestorCPSEnforcedOnLedger(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store := regStore(t,
		&account.Account{ID: "tenant", Postpaid: true},
		&account.Account{ID: "agent", ParentID: "tenant", CPS: 1},
		&account.Account{ID: "u1", ParentID: "agent", Credit: money.MustFromYuan(1000)},
		&account.Account{ID: "u2", ParentID: "agent", Credit: money.MustFromYuan(1000)})
	r := newRegRig(store, 10*time.Second)
	r.m.SetAdmissionLedger(NewAdmissionR(c))

	if err := r.m.Start("c1", "u1", "86", regEntry(), 60, false); err != nil {
		t.Fatal(err)
	}
	err = r.m.Start("c2", "u2", "86", regEntry(), 60, false)
	var limit *ErrLimit
	if !errors.As(err, &limit) || limit.Kind != "cps" || limit.AccountID != "agent" {
		t.Fatalf("second start in the same second under agent CPS=1: err = %v, want a cps ErrLimit for agent", err)
	}
	if r.m.Live() != 1 {
		t.Errorf("live = %d, want 1 (the refused start left no ghost)", r.m.Live())
	}
	r.advance(time.Second)
	if err := r.m.Start("c3", "u2", "86", regEntry(), 60, false); err != nil {
		t.Errorf("next second: err = %v, want nil (the refused start's window rolled back)", err)
	}
}

// A refused start gives its CPS windows back, or a burst of refuses
// locks the account out of the next legitimate call.
func TestLedgerReserveFailureReleasesAncestorCPS(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store := regStore(t,
		&account.Account{ID: "agent", CPS: 1},
		&account.Account{ID: "u1", ParentID: "agent", Credit: 0}) // broke: the reserve fails
	r := newRegRig(store, 10*time.Second)
	r.m.SetAdmissionLedger(NewAdmissionR(c))
	if err := r.m.Start("c1", "u1", "86", regEntry(), 60, false); err == nil {
		t.Fatal("reserve on zero balance must fail")
	}
	store.Put(&account.Account{ID: "u1", ParentID: "agent", Credit: money.MustFromYuan(100)})
	// Same second: the refused start must not occupy the agent's window.
	if err := r.m.Start("c2", "u1", "86", regEntry(), 60, false); err != nil {
		t.Fatalf("start right after a refused one in the same second: %v", err)
	}
}

// A peer settling an unanswered call from the meta charges nothing; a
// parked answer lets it time the call since that answer. It used to
// derive the duration since the INVITE.
func TestRemoteSettleOfUnansweredCall(t *testing.T) {
	store := regStore(t, &account.Account{ID: "u1", Credit: money.MustFromYuan(10), RateGroup: "g"})
	owner, peer, waitMeta := newSharedRig(t, store)

	owner.m.Start("q1", "u1", "8613800138000", regEntry(), 60, false)
	waitMeta("q1")
	owner.advance(100 * time.Second)
	cdr, err := peer.m.End("q1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Duration != 0 || cdr.Cost != 0 {
		t.Errorf("unanswered remote settle = duration %d cost %s, want 0 / 0 (it used to charge the 100s since the INVITE)",
			cdr.Duration, cdr.Cost)
	}
	if bal := peer.balance(t, "u1"); bal != money.MustFromYuan(10) {
		t.Errorf("balance = %s, want 10.0000 (only the lock, released)", bal)
	}

	owner.advance(time.Minute)
	owner.m.Start("q2", "u1", "8613800138000", regEntry(), 60, false)
	waitMeta("q2")
	owner.advance(10 * time.Second) // answered 10s after this INVITE
	if err := peer.m.Answer("q2"); !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("peer answer: err = %v, want ErrUnknownCall (it only parks the answer)", err)
	}
	owner.advance(60 * time.Second)
	cdr, err = peer.m.End("q2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Duration != 60 || cdr.Cost != money.MustFromYuan(0.06) {
		t.Errorf("answered remote settle = duration %d cost %s, want 60s since the answer / 0.0600", cdr.Duration, cdr.Cost)
	}
}
