package regtable

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/fastvoip/gvos-core/internal/shared/rds"
)

func newRegR(t *testing.T) (*Table, *RegR) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	tb := New()
	r := NewRegR(c)
	tb.SetRegRedis(r)
	return tb, r
}

// TestMirrorSurvivesRestart: the redis hash outlives the process — a
// fresh Table (restart) still answers the fallback lookup.
func TestMirrorSurvivesRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	c, _ := rds.New("redis://" + mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	r := NewRegR(c)

	tb1 := New()
	tb1.SetRegRedis(r)
	tb1.Apply("3000", "reg", time.Now().Add(time.Hour).Unix(), "10.0.0.5", time.Now())

	// "Restart": brand-new table, same redis.
	tb2 := New()
	tb2.SetRegRedis(r)
	e, ok := tb2.LookupUser(context.Background(), "3000")
	if !ok || !e.Online || e.SrcIP != "10.0.0.5" {
		t.Fatalf("fallback lookup after restart: %+v ok=%v", e, ok)
	}
}

// TestExpiryEvaporation: the hash TTL equals the granted expiry —
// FastForward drops dead registrations without any sweeper.
func TestExpiryEvaporation(t *testing.T) {
	mr := miniredis.RunT(t)
	c, _ := rds.New("redis://" + mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	r := NewRegR(c)
	tb := New()
	tb.SetRegRedis(r)

	now := time.Now()
	tb.Apply("3001", "reg", now.Add(2*time.Second).Unix(), "10.0.0.6", now)
	mr.FastForward(3 * time.Second)

	tb2 := New()
	tb2.SetRegRedis(r)
	if _, ok := tb2.LookupUser(context.Background(), "3001"); ok {
		t.Fatal("expired registration must evaporate via TTL")
	}
}

// TestLocalFirstFallback: the local map wins over the mirror (it is
// strictly fresher on this replica).
func TestLocalFirstFallback(t *testing.T) {
	tb, _ := newRegR(t)
	tb.Apply("3002", "reg", time.Now().Add(time.Hour).Unix(), "10.0.0.7", time.Now())
	e, ok := tb.LookupUser(context.Background(), "3002")
	if !ok || e.Events != 1 {
		t.Fatalf("local-first lookup: %+v ok=%v", e, ok)
	}
}
