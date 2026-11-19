// lcr.go: least-cost routing (competitive-analysis §3.1). The control
// plane prices each gateway against its CostRate group per prefix,
// reorders cheapest-first, and encodes the order into the broadcast
// priority field — the edge selection algorithm is untouched (it
// already prefers the lowest priority value).
package gateway

import (
	"sort"

	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
)

// Rater is the rate lookup the LCR needs (the api Server's per-group
// table cache implements it).
type Rater interface {
	// BestRate returns the cheapest price/min for a destination under
	// the group, or nil when unratable.
	BestRate(group, dest string) *money.Amount
}

// LCR reorders the gateways for one destination: cheapest
// CostRate-first, then base Priority, then ID (determinism). Prefix
// matching is longest-first (8613 beats 86). Gateways without a
// matching prefix or without a resolvable cost rate keep their base
// order after the LCR-ordered ones.
func LCR(gws []Gateway, dest string, r Rater) []Gateway {
	type scored struct {
		gw    Gateway
		score int // rank bucket: 0 = LCR-priced, 1 = fallback
		price money.Amount
	}
	out := make([]scored, 0, len(gws))
	for _, g := range gws {
		if !g.Enabled {
			continue
		}
		// Longest matching prefix this gateway serves.
		best := ""
		for _, p := range g.Prefixes {
			if len(p) > len(best) && len(dest) >= len(p) && dest[:len(p)] == p {
				best = p
			}
		}
		if best != "" && g.CostRate != "" && r != nil {
			if price := r.BestRate(g.CostRate, dest); price != nil {
				out = append(out, scored{gw: g, score: 0, price: *price})
				continue
			}
		}
		out = append(out, scored{gw: g, score: 1})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		if out[i].score == 0 && out[i].price != out[j].price {
			return out[i].price < out[j].price
		}
		if out[i].gw.Priority != out[j].gw.Priority {
			return out[i].gw.Priority < out[j].gw.Priority
		}
		return out[i].gw.ID < out[j].gw.ID
	})
	res := make([]Gateway, 0, len(out))
	for _, s := range out {
		res = append(res, s.gw)
	}
	return res
}

// LCRPriority encodes an ordered list into the broadcast priority
// field: index in the LCR order (0 first). The edge's
// "lowest prio wins" selection then reproduces exactly this order.
func LCRPriority(ordered []Gateway, id string) int {
	for i, g := range ordered {
		if g.ID == id {
			return i
		}
	}
	return len(ordered) // unrouted tail
}

