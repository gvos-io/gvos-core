package ipallow

import (
	"net"
	"testing"
)

func check(t *testing.T, l *List, ip string) (bool, string) {
	t.Helper()
	return l.Check(net.ParseIP(ip))
}

// TestDefaultDeny: an empty list and an unmatched source both reject.
func TestDefaultDeny(t *testing.T) {
	l := New()
	if ok, _ := check(t, l, "10.0.0.1"); ok {
		t.Error("empty list must reject (default-deny)")
	}
}

// TestAllowAndDeny: allow CIDR passes, outside rejects; an overlapping
// deny beats an allow (decision order).
func TestAllowAndDeny(t *testing.T) {
	l := New()
	_, err := l.Reload([]Rule{
		{ID: "office", CIDR: "192.168.1.0/24", Action: Allow},
		{ID: "badhost", CIDR: "192.168.1.66/32", Action: Deny},
		{ID: "v6lab", CIDR: "2001:db8::/32", Action: Allow},
	})
	if err != nil || l.Count() != 3 {
		t.Fatalf("reload: %v count=%d", err, l.Count())
	}
	cases := []struct {
		ip   string
		want bool
		rule string
	}{
		{"192.168.1.10", true, "office"},
		{"192.168.1.66", false, "badhost"},      // deny wins over containing allow
		{"192.168.2.10", false, ""},             // outside every rule
		{"2001:db8::1", true, "v6lab"},          // IPv6 allow
		{"2001:db9::1", false, ""},              // adjacent v6 block
		{"::ffff:192.168.1.10", true, "office"}, // v4-mapped sources follow the v4 rule
	}
	for _, c := range cases {
		got, rule := check(t, l, c.ip)
		if got != c.want || rule != c.rule {
			t.Errorf("Check(%s) = (%v, %q), want (%v, %q)", c.ip, got, rule, c.want, c.rule)
		}
	}
}

// TestReloadSkipsInvalid: bad CIDR/action entries are dropped by
// Reload (the API layer validates first; this is the broadcast path's
// safety net).
func TestReloadSkipsInvalid(t *testing.T) {
	l := New()
	_, err := l.Reload([]Rule{
		{ID: "ok", CIDR: "10.0.0.0/8", Action: Allow},
		{ID: "bad-cidr", CIDR: "10.0.0.0/99", Action: Allow},
		{ID: "bad-action", CIDR: "10.1.0.0/16", Action: "maybe"},
		{ID: "", CIDR: "10.2.0.0/16", Action: Allow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Count() != 1 {
		t.Fatalf("count = %d, want 1 (invalid entries skipped)", l.Count())
	}
}

// TestRulesRoundtrip: enumeration returns the entries in insertion
// order (the GET API and snapshot endpoint rely on this).
func TestRulesRoundtrip(t *testing.T) {
	in := []Rule{
		{ID: "a", CIDR: "10.0.0.0/8", Action: Allow},
		{ID: "b", CIDR: "172.16.0.0/12", Action: Allow, Note: "dc"},
	}
	l := New()
	l.Reload(in)
	out := l.Rules()
	if len(out) != 2 || out[0].ID != "a" || out[1].Note != "dc" {
		t.Errorf("Rules() = %v, want roundtrip of %v", out, in)
	}
	// The copy must be detached: mutating it must not corrupt the list.
	out[0].ID = "mutated"
	if l.Rules()[0].ID != "a" {
		t.Error("Rules() returned a shared slice, not a copy")
	}
}

// TestIPv4IPv6Isolation: an IPv4 source never matches an IPv6 rule
// and vice versa (net.IPNet.Contains semantics).
func TestIPv4IPv6Isolation(t *testing.T) {
	l := New()
	l.Reload([]Rule{{ID: "v6", CIDR: "::/0", Action: Allow}})
	if ok, _ := check(t, l, "1.2.3.4"); ok {
		t.Error("IPv4 source matched an IPv6-only ::/0 rule")
	}
}
