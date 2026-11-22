// Stress: 10-round architecture load against leaf-only billing +
// cross-replica settle. Run with:
//
//	go test -count=1 -timeout 10m -run TestArchStress10Rounds ./internal/callcore/
package callcore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

const (
	stressRounds   = 10
	stressCalls    = 400 // per round (≈40 CPS × 10s)
	stressWorkers  = 64
	stressCrossPct = 50 // % of calls ended on peer (cross-replica)
)

type roundStats struct {
		ok, fail, doubleCharge, parentDebit int64
	latMs                                   []float64
}

func (r *roundStats) record(ms float64, ok bool) {
	r.latMs = append(r.latMs, ms)
	if ok {
		r.ok++
	} else {
		r.fail++
	}
}

func (r *roundStats) pct(p float64) float64 {
	if len(r.latMs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), r.latMs...)
	sort.Float64s(sorted)
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func TestArchStress10Rounds(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	store := account.NewStore()
	// Hierarchy: tenant (postpaid) → agent (prepaid high) → leaf (prepaid).
	// Mode A leaf-only: only leaf credit must move; agent/tenant stay put.
	tenantCredit := money.MustFromYuan(1_000_000)
	agentCredit := money.MustFromYuan(1_000_000)
	leafCredit := money.MustFromYuan(1_000_000)
	store.Put(&account.Account{ID: "tenant", Credit: tenantCredit, RateGroup: "g", Postpaid: true})
	store.Put(&account.Account{ID: "agent", ParentID: "tenant", Credit: agentCredit, RateGroup: "g"})
	store.Put(&account.Account{ID: "leaf", ParentID: "agent", Credit: leafCredit, RateGroup: "g", CPS: 1_000_000, Concurrent: 1_000_000})

	bal := account.NewBalanceR(store, c)
	if err := bal.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta := NewCallMetaStore(c)
	adm := NewAdmissionR(c)

	owner := NewManager(store, nil, time.Second)
	owner.SetBalanceAuthority(bal)
	owner.SetCallMetaStore(meta)
	owner.SetAdmissionLedger(adm)

	peer := NewManager(store, nil, time.Second)
	peer.SetBalanceAuthority(bal)
	peer.SetCallMetaStore(meta)
	peer.SetAdmissionLedger(adm)

	entry := &rate.Entry{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 60, StepDur: 6}
	costPerCall := money.MustFromYuan(0.06) // 60s billed

	t.Logf("arch-stress: %d rounds × %d calls (cross-replica end=%d%%, workers=%d)",
		stressRounds, stressCalls, stressCrossPct, stressWorkers)

	var totalOk, totalFail, totalDbl, totalParentDebit int64
	var cumOk int64
	var allLat []float64

	for round := 1; round <= stressRounds; round++ {
		st := &roundStats{}
		var mu sync.Mutex
		jobs := make(chan int, stressCalls)
		var wg sync.WaitGroup
		var seq atomic.Int64

		for w := 0; w < stressWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range jobs {
					id := fmt.Sprintf("r%d-%d-%d", round, n, seq.Add(1))
					t0 := time.Now()
					ok := true
					if err := owner.Start(id, "leaf", "8613800138000", entry, 60, false); err != nil {
						ok = false
					} else {
						cross := (n % 100) < stressCrossPct
						var endErr error
						if cross {
							_, endErr = peer.End(id, 60)
							// Owner late End must fail (settle lock) — not a call failure.
							if _, err2 := owner.End(id, 60); err2 == nil {
								mu.Lock()
								st.doubleCharge++
								mu.Unlock()
							}
						} else {
							_, endErr = owner.End(id, 60)
							// Peer End after local settle must miss.
							if _, err2 := peer.End(id, 60); err2 == nil {
								mu.Lock()
								st.doubleCharge++
								mu.Unlock()
							}
						}
						if endErr != nil {
							ok = false
						}
					}
					ms := float64(time.Since(t0).Microseconds()) / 1000.0
					mu.Lock()
					st.record(ms, ok)
					mu.Unlock()
				}
			}()
		}
		for i := 0; i < stressCalls; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()

		// Leaf-only integrity: agent + tenant credit unchanged in Redis.
		leafBal, _ := bal.Get(context.Background(), "leaf")
		agentBal, _ := bal.Get(context.Background(), "agent")
		tenantBal, _ := bal.Get(context.Background(), "tenant")
		if agentBal != agentCredit {
			st.parentDebit++
			t.Errorf("round %d: agent balance moved %s → %s (leaf-only violation)", round, agentCredit, agentBal)
		}
		if tenantBal != tenantCredit {
			st.parentDebit++
			t.Errorf("round %d: tenant balance moved %s → %s (leaf-only violation)", round, tenantCredit, tenantBal)
		}
		cumOk += st.ok
		wantLeaf := leafCredit.Sub(money.Amount(int64(costPerCall) * cumOk))
		leafDrift := leafBal != wantLeaf
		if leafDrift {
			t.Errorf("round %d: leaf bal=%s want=%s (cumOk=%d) delta=%s — over/under charge",
				round, leafBal, wantLeaf, cumOk, leafBal.Sub(wantLeaf))
		}

		t.Logf("round=%-2d calls=%d ok=%d fail=%d dbl=%d parentDebit=%d leafOK=%v p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms",
			round, stressCalls, st.ok, st.fail, st.doubleCharge, st.parentDebit, !leafDrift,
			st.pct(0.50), st.pct(0.95), st.pct(0.99), st.pct(1.0))

		totalOk += st.ok
		totalFail += st.fail
		totalDbl += st.doubleCharge
		totalParentDebit += st.parentDebit
		allLat = append(allLat, st.latMs...)

		if st.fail > 0 {
			t.Errorf("round %d: %d call failures", round, st.fail)
		}
		if st.doubleCharge > 0 {
			t.Errorf("round %d: %d double-settle wins (settle lock broken)", round, st.doubleCharge)
		}
	}

	sort.Float64s(allLat)
	pick := func(p float64) float64 {
		if len(allLat) == 0 {
			return 0
		}
		return allLat[int(float64(len(allLat)-1)*p)]
	}
	finalLeaf, _ := bal.Get(context.Background(), "leaf")
	wantFinal := leafCredit.Sub(money.Amount(int64(costPerCall) * totalOk))
	t.Logf("TOTAL ok=%d fail=%d dbl=%d parentDebit=%d leaf=%s want=%s | latency p50=%.2f p95=%.2f p99=%.2f max=%.2f ms",
		totalOk, totalFail, totalDbl, totalParentDebit, finalLeaf, wantFinal,
		pick(0.50), pick(0.95), pick(0.99), pick(1.0))

	if totalFail > 0 || totalDbl > 0 || totalParentDebit > 0 {
		t.Fatalf("arch stress failed: fail=%d dbl=%d parentDebit=%d", totalFail, totalDbl, totalParentDebit)
	}
	if finalLeaf != wantFinal {
		t.Fatalf("final leaf balance drift: got %s want %s", finalLeaf, wantFinal)
	}
}
