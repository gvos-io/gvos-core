package gateway

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		g    Gateway
		err  string
	}{
		{"ok", Gateway{ID: "gw1", Name: "one", SipAddr: "10.0.0.1:5060", Protocol: "udp"}, ""},
		{"ok-no-proto", Gateway{ID: "gw1", Name: "one", SipAddr: "gw.example:5060"}, ""},
		{"no-id", Gateway{Name: "one", SipAddr: "x"}, "id required"},
		{"no-name", Gateway{ID: "gw1", SipAddr: "x"}, "name required"},
		{"no-addr", Gateway{ID: "gw1", Name: "one"}, "sip_addr"},
		{"bad-proto", Gateway{ID: "gw1", Name: "one", SipAddr: "x", Protocol: "sctp"}, "protocol"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.g)
			if c.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestReloadReplacesAndSkipsInvalid(t *testing.T) {
	l := New()
	if n, _ := l.Reload([]Gateway{
		{ID: "a", Name: "a", SipAddr: "a:5060", Priority: 1, Enabled: true},
		{ID: "b", Name: "b"}, // invalid: no addr, skipped
	}); n != 1 {
		t.Fatalf("expected 1 applied, got %d", n)
	}
	if l.Count() != 1 {
		t.Fatalf("count = %d", l.Count())
	}
	// Full replace: the old entry is gone.
	l.Reload([]Gateway{{ID: "c", Name: "c", SipAddr: "c:5060"}})
	gws := l.Gateways()
	if len(gws) != 1 || gws[0].ID != "c" {
		t.Fatalf("expected [c], got %v", gws)
	}
}

func TestGatewaysCopyIsDeep(t *testing.T) {
	l := New()
	l.Reload([]Gateway{{ID: "a", Name: "a", SipAddr: "a:5060"}})
	g := l.Gateways()[0]
	g.Name = "mutated"
	if l.Gateways()[0].Name != "a" {
		t.Fatal("Gateways() must return a copy")
	}
}
