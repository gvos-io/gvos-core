package gateway

import (
	"testing"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
)

type fakeRater struct {
	groups map[string]map[string]money.Amount
}

func (f fakeRater) BestRate(group, dest string) *money.Amount {
	for prefix, price := range f.groups[group] {
		if len(dest) >= len(prefix) && dest[:len(prefix)] == prefix {
			return &price
		}
	}
	return nil
}

func TestLCRCheapestFirst(t *testing.T) {
	gws := []Gateway{
		{ID: "g-expensive", SipAddr: "10.0.0.1:5060", Priority: 1, Enabled: true,
			Prefixes: []string{"86"}, CostRate: "grpA"},
		{ID: "g-cheap", SipAddr: "10.0.0.2:5060", Priority: 9, Enabled: true,
			Prefixes: []string{"86"}, CostRate: "grpB"},
		{ID: "g-off", SipAddr: "10.0.0.3:5060", Priority: 0, Enabled: false,
			Prefixes: []string{"86"}, CostRate: "grpA"},
	}
	r := fakeRater{groups: map[string]map[string]money.Amount{
		"grpA": {"86": money.MustFromYuan(0.10)},
		"grpB": {"86": money.MustFromYuan(0.05)},
	}}
	// g-cheap costs less despite Priority 9; g-off is out entirely.
	got := LCR(gws, "8613800138000", r)
	if len(got) != 2 || got[0].ID != "g-cheap" || got[1].ID != "g-expensive" {
		t.Fatalf("LCR order = %v", ids(got))
	}
	if p := LCRPriority(got, "g-cheap"); p != 0 {
		t.Errorf("g-cheap prio = %d, want 0", p)
	}
}

func TestLCRPrefixScoping(t *testing.T) {
	// 8613-matching gateway wins for 8613...; a cheaper 86-only one
	// still loses to a 8613 gateway when both serve the destination
	// (both matched: longest-prefix preference is per-gateway, then
	// price decides across gateways — but g-intl does not serve 86 at
	// all so it is fallback-ordered).
	gws := []Gateway{
		{ID: "g-cn", SipAddr: "a:5060", Priority: 1, Enabled: true,
			Prefixes: []string{"86"}, CostRate: "grpA"},
		{ID: "g-cn13", SipAddr: "b:5060", Priority: 5, Enabled: true,
			Prefixes: []string{"8613"}, CostRate: "grpB"},
		{ID: "g-intl", SipAddr: "c:5060", Priority: 0, Enabled: true,
			Prefixes: []string{"1"}, CostRate: "grpC"},
	}
	r := fakeRater{groups: map[string]map[string]money.Amount{
		"grpA": {"86": money.MustFromYuan(0.04)}, // cheaper but shorter prefix
		"grpB": {"8613": money.MustFromYuan(0.06)},
		"grpC": {"1": money.MustFromYuan(0.01)},
	}}
	got := LCR(gws, "8613800138000", r)
	// Both 86 and 8613 gateways match; price decides: 0.04 first.
	if got[0].ID != "g-cn" || got[1].ID != "g-cn13" {
		t.Fatalf("order = %v", ids(got))
	}
	// International gateway never matches 86 (its prefix is 1) —
	// fallback bucket, sorted by base priority (0).
	if got[2].ID != "g-intl" {
		t.Fatalf("intl should be the fallback tail: %v", ids(got))
	}
}

func TestLCRRateChangeImmediateEffect(t *testing.T) {
	gws := []Gateway{
		{ID: "a", SipAddr: "a:5060", Priority: 0, Enabled: true, Prefixes: []string{"86"}, CostRate: "g1"},
		{ID: "b", SipAddr: "b:5060", Priority: 1, Enabled: true, Prefixes: []string{"86"}, CostRate: "g2"},
	}
	prices := map[string]map[string]money.Amount{
		"g1": {"86": money.MustFromYuan(0.03)},
		"g2": {"86": money.MustFromYuan(0.08)},
	}
	r := fakeRater{groups: prices}
	if got := LCR(gws, "86", r); got[0].ID != "a" {
		t.Fatalf("initial order %v", ids(got))
	}
	// Rate change flips the order (the control-plane recompute is the
	// only step; the broadcast carries it).
	prices["g2"]["86"] = money.MustFromYuan(0.01)
	if got := LCR(gws, "86", r); got[0].ID != "b" {
		t.Fatalf("after rate change order %v, want b first", ids(got))
	}
}

func ids(g []Gateway) []string {
	out := make([]string, 0, len(g))
	for _, x := range g {
		out = append(out, x.ID)
	}
	return out
}
