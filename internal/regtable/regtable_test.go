package regtable

import (
	"testing"
	"time"
)

func TestApplyAndSnapshot(t *testing.T) {
	tbl := New()
	at := time.Unix(1_700_000_000, 0)

	tbl.Apply("u1", "reg", at.Add(3600).Unix(), "10.0.0.1", at)
	tbl.Apply("u1", "reg", at.Add(3600).Unix(), "10.0.0.2", at.Add(time.Second))

	if tbl.Count() != 1 {
		t.Fatalf("count = %d (dedupe by user)", tbl.Count())
	}
	entries := tbl.Entries()
	if entries[0].Events != 2 || entries[0].SrcIP != "10.0.0.2" || !entries[0].Online {
		t.Fatalf("entry = %+v", entries[0])
	}

	// Expiry flips offline, keeps the event count.
	tbl.Apply("u1", "expire", 0, "", at.Add(time.Minute))
	entries = tbl.Entries()
	if entries[0].Online || entries[0].Events != 3 {
		t.Fatalf("after expire = %+v", entries[0])
	}
	if tbl.Online() != 0 {
		t.Fatalf("online = %d", tbl.Online())
	}
}

func TestApplyUnknownUser(t *testing.T) {
	tbl := New()
	tbl.Apply("", "reg", 0, "x", time.Now())
	if tbl.Count() != 0 {
		t.Fatal("empty user must be ignored")
	}
}
