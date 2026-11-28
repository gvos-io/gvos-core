// Restore/Sweep coverage: boot load from PG (migration 014) and the
// hourly dead-row sweep. Restore must be replace-not-merge, skip
// ref-less rows, and advance the serial past restored ids so new
// allocations cannot collide with restored ones.
package axb

import (
	"testing"
	"time"
)

func TestRestoreReplaceSkipReflessAndSerial(t *testing.T) {
	s := NewStore()
	future := time.Now().Add(time.Hour)
	s.Restore([]Binding{
		{ID: "axb-3", XNumber: "9000000001", ANumber: "13800000001", BNumber: "13900000001", ExpireAt: future, Active: true},
		{ID: "", XNumber: "9000000002", ANumber: "13800000002", BNumber: "13900000002", ExpireAt: future, Active: true}, // pre-014 row
	})
	if got := s.Count(); got != 1 {
		t.Fatalf("count after restore = %d, want 1 (ref-less rows skipped)", got)
	}
	if _, ok := s.Resolve("9000000002"); ok {
		t.Fatal("ref-less binding was restored")
	}
	if _, ok := s.Resolve("9000000001"); !ok {
		t.Fatal("ref binding not resolvable after restore")
	}
	if bd, ok := s.Resolve("9000000001"); ok && bd.ID != "axb-3" {
		t.Fatalf("restored id = %q, want axb-3", bd.ID)
	}

	// Replace, not merge: a second Restore with fewer bindings drops
	// the earlier ones (peer cfg.reload apply semantics).
	s.Restore([]Binding{
		{ID: "axb-7", XNumber: "9000000009", ANumber: "13800000009", BNumber: "13900000009", ExpireAt: future, Active: true},
	})
	if got := s.Count(); got != 1 {
		t.Fatalf("count after second restore = %d, want 1 (replace)", got)
	}
	if _, ok := s.Resolve("9000000001"); ok {
		t.Fatal("restore did not replace the previous set")
	}

	// New binds must not collide with restored ids (serial advanced).
	bd, err := s.Bind("13800000003", "13900000003", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if bd.ID == "axb-7" || bd.ID == "axb-3" {
		t.Fatalf("new bind id %q collides with a restored id", bd.ID)
	}
}

func TestSweepDropsDeadBindings(t *testing.T) {
	s := NewStore()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	s.Restore([]Binding{
		{ID: "axb-1", XNumber: "9000000011", ANumber: "13800000011", BNumber: "13900000011", ExpireAt: past, Active: true},   // expired
		{ID: "axb-2", XNumber: "9000000012", ANumber: "13800000012", BNumber: "13900000012", ExpireAt: future, Active: true}, // live
	})
	// Restore skips nothing here (both have refs) but expired stays
	// invisible via Resolve/List; Sweep physically drops it.
	if s.Sweep() != 1 {
		t.Fatalf("sweep removed != 1")
	}
	if s.Sweep() != 0 {
		t.Fatal("sweep not idempotent")
	}
	if got := s.Count(); got != 1 {
		t.Fatalf("count after sweep = %d, want 1", got)
	}
}
