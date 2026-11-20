// cdr_ring_test.go: the bounded recent-CDR ring — cap enforcement,
// newest-retention, and the concurrent append/read race (run under
// -race).
package api

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gvos-io/gvos-core/internal/callcore"
)

func TestCDRRingCapAndRecency(t *testing.T) {
	s := NewServer()
	for i := 0; i < maxInMemoryCDRs+100; i++ {
		s.storeCdr(&callcore.CDR{CallID: fmt.Sprintf("c-%d", i)})
	}
	if got := len(s.cdrs); got != maxInMemoryCDRs {
		t.Fatalf("ring len = %d, want cap %d", got, maxInMemoryCDRs)
	}
	// The newest CDRs must be the survivors.
	if s.cdrs[len(s.cdrs)-1].CallID != "c-10099" {
		t.Fatalf("newest = %s, want c-10099", s.cdrs[len(s.cdrs)-1].CallID)
	}
	if s.cdrs[0].CallID != "c-100" {
		t.Fatalf("oldest = %s, want c-100 (first 100 dropped)", s.cdrs[0].CallID)
	}
}

// Concurrent storeCdr (the 16 NATS responders) against statsBody and
// handleCDRList reads — the race the cdrsMu lock closes.
func TestCDRRingConcurrentAccess(t *testing.T) {
	s := NewServer()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.storeCdr(&callcore.CDR{CallID: fmt.Sprintf("w%d-%d", w, i)})
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = s.statsBody()
		}
	}()
	wg.Wait()
	if got := len(s.cdrs); got != 1600 {
		t.Fatalf("ring len = %d, want 1600", got)
	}
}
