package did

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		d    DID
		ok   bool
	}{
		{"user dest", DID{Number: "8621088001234", AccountID: "a1", DestinationType: DestUser, Destination: "2001", Enabled: true}, true},
		{"external dest", DID{Number: "9xxxxxxxxx", AccountID: "a1", DestinationType: DestExternal, Destination: "13800138000", Enabled: true}, true},
		{"agent seam", DID{Number: "95105", AccountID: "a1", DestinationType: DestAgent, Destination: "agent-pool-1", Enabled: true}, true},
		{"missing number", DID{AccountID: "a1", DestinationType: DestUser, Destination: "x"}, false},
		{"missing account", DID{Number: "123", DestinationType: DestUser, Destination: "x"}, false},
		{"bad type", DID{Number: "123", AccountID: "a1", DestinationType: "sip", Destination: "x"}, false},
		{"missing dest", DID{Number: "123", AccountID: "a1", DestinationType: DestUser}, false},
		{"too long number", DID{Number: "123456789012345678901", AccountID: "a1", DestinationType: DestUser, Destination: "x"}, false},
	}
	for _, c := range cases {
		err := Validate(c.d)
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate=%v want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestUpsertResolveDelete(t *testing.T) {
	s := NewStore()
	d := DID{ID: "did-1", Number: "8621088001234", AccountID: "a1", DestinationType: DestUser, Destination: "2001", Enabled: true}
	if err := s.Upsert(d); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Resolve("8621088001234")
	if !ok || got.AccountID != "a1" || got.Destination != "2001" || got.DestinationType != DestUser {
		t.Fatalf("Resolve=%+v ok=%v", got, ok)
	}
	// Replace by number keeps one entry.
	d2 := d
	d2.Destination = "2002"
	d2.ID = "did-2"
	if err := s.Upsert(d2); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 1 {
		t.Fatalf("upsert by number should replace, count=%d", s.Count())
	}
	if got, _ := s.Resolve("8621088001234"); got.Destination != "2002" {
		t.Fatalf("replacement not applied: %+v", got)
	}
	// Disabled DIDs do not resolve.
	d3 := d2
	d3.Enabled = false
	_ = s.Upsert(d3)
	if _, ok := s.Resolve("8621088001234"); ok {
		t.Fatal("disabled DID must not resolve")
	}
	// Delete.
	d4 := d2
	d4.Enabled = true
	_ = s.Upsert(d4)
	if !s.Delete("8621088001234") {
		t.Fatal("Delete should report true")
	}
	if s.Delete("8621088001234") {
		t.Fatal("second Delete should report false")
	}
	if _, ok := s.Resolve("8621088001234"); ok {
		t.Fatal("deleted DID must not resolve")
	}
}

func TestReloadReplaces(t *testing.T) {
	s := NewStore()
	_ = s.Upsert(DID{ID: "a", Number: "111", AccountID: "x", DestinationType: DestUser, Destination: "u"})
	// Snapshot entries round-trip via List(): every field including
	// Enabled is explicit, so Reload never guesses.
	live := DID{ID: "b", Number: "222", AccountID: "y", DestinationType: DestExternal, Destination: "138", Enabled: true}
	off := DID{ID: "c", Number: "333", AccountID: "z", DestinationType: DestUser, Destination: "u3", Enabled: false}
	s.Reload([]DID{live, off})
	if _, ok := s.Resolve("111"); ok {
		t.Fatal("reload must drop stale entries")
	}
	if got, ok := s.Resolve("222"); !ok || got.AccountID != "y" {
		t.Fatalf("reload apply failed: %+v", got)
	}
	if _, ok := s.Resolve("333"); ok {
		t.Fatal("disabled entry must not resolve after reload")
	}
}
