package blocklist

import "testing"

func TestPrefixBlacklist(t *testing.T) {
	l := New()
	l.Reload([]Entry{
		{Kind: "black", Scope: "dest", Pattern: "86139"},
	})
	if ok, why := l.CheckScreen("8613912345678", "dest"); ok {
		t.Errorf("blacklisted dest allowed (%v)", why)
	}
	if ok, _ := l.CheckScreen("8613812345678", "dest"); !ok {
		t.Error("non-matching dest rejected")
	}
	// src rules do not affect dest screening.
	if ok, _ := l.CheckScreen("8613912345678", "src"); !ok {
		t.Error("dest rule leaked into src scope")
	}
}

func TestRegexRule(t *testing.T) {
	l := New()
	l.Reload([]Entry{
		{Kind: "black", Scope: "dest", Pattern: `^8613[0-9]{9}$`, IsRegex: true},
	})
	if ok, _ := l.CheckScreen("8613812345678", "dest"); ok {
		t.Error("regex black match allowed")
	}
	if ok, _ := l.CheckScreen("1234", "dest"); !ok {
		t.Error("non-match rejected")
	}
}

func TestWhitelistDefaultDeny(t *testing.T) {
	l := New()
	l.Reload([]Entry{
		{Kind: "white", Scope: "dest", Pattern: "8613"},
	})
	if ok, _ := l.CheckScreen("8613800138000", "dest"); !ok {
		t.Error("whitelisted number rejected")
	}
	if ok, why := l.CheckScreen("10600000", "dest"); ok {
		t.Errorf("non-whitelisted number allowed (%v)", why)
	}
}

func TestInvalidRegexSkipped(t *testing.T) {
	l := New()
	l.Reload([]Entry{
		{Kind: "black", Scope: "dest", Pattern: "([", IsRegex: true},
		{Kind: "black", Scope: "dest", Pattern: "86139"},
	})
	if l.Count() != 1 {
		t.Errorf("invalid regex row kept: count=%d", l.Count())
	}
}

func TestLiveAddRemove(t *testing.T) {
	l := New()
	l.Add(Entry{Kind: "black", Scope: "dest", Pattern: "999"})
	if ok, _ := l.CheckScreen("999123", "dest"); ok {
		t.Error("live-added rule not enforced")
	}
	if n := l.Remove("", "black", "dest", "999"); n != 1 {
		t.Fatalf("removed=%d, want 1", n)
	}
	if ok, _ := l.CheckScreen("999123", "dest"); !ok {
		t.Error("removed rule still enforced")
	}
}

func TestEmptyListAllowsAll(t *testing.T) {
	l := New()
	if ok, _ := l.CheckScreen("8613800138000", "dest"); !ok {
		t.Error("empty list must allow")
	}
}

func BenchmarkCheckScreen(b *testing.B) {
	l := New()
	var es []Entry
	for i := 0; i < 100000; i++ {
		es = append(es, Entry{Kind: "black", Scope: "dest", Pattern: "999" + string(rune('0'+i%10))})
	}
	l.Reload(es)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.CheckScreen("8613800138000", "dest")
	}
}
