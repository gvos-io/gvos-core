// bench_lock_test.go: microbenchmark of the Start path under the
// shared-ledger wiring — the lock-refactor A/B. Run with:
//
//	go test -bench . -run xxx ./internal/callcore/
package callcore

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

var benchSeq atomic.Int64

func benchSetup(b *testing.B) *Manager {
	b.Helper()
	mr := miniredis.RunT(b)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	store := account.NewStore()
	store.Put(&account.Account{ID: "tenant", RateGroup: "g", Postpaid: true})
	store.Put(&account.Account{ID: "agent", ParentID: "tenant", Credit: 1e9, RateGroup: "g"})
	store.Put(&account.Account{ID: "user-a", ParentID: "agent", Credit: 1e9, RateGroup: "g", CPS: 1000000})
	adm := NewAdmissionR(c)
	m := NewManager(store, nil, time.Second)
	m.SetAdmissionLedger(adm)
	return m
}

func BenchmarkStartEndLedger(b *testing.B) {
	m := benchSetup(b)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := "b-" + strconv.FormatInt(benchSeq.Add(1), 10)
			if err := m.Start(id, "user-a", "86", entry2(), 60, false); err != nil {
				b.Fatal(err)
			}
			if _, err := m.End(id, 60); err != nil {
				b.Fatal(err)
			}
		}
	})
}
