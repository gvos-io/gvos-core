package billing

import (
	"fmt"
	"math"

	"github.com/lixuanqun/gvos-core/internal/rate"
)

// Charger applies a rate.Entry to call durations. It implements the
// classic interval schemes: 60+6, 6+6, 1+1, 1+1, N+M (per-minute first
// step then per-second increments), and flat per-second.
type Charger struct{}

// ChargeFor returns the billed seconds and cost for a connected duration.
// The invariant: cost always prices exactly billedSecs, decomposed as
// one first step (InitDur seconds) plus ceil'd incremental steps
// (StepDur seconds each). MinDur lifts billedSecs before pricing.
// zero-rated calls still produce a step-charged duration so CDRs stay
// consistent.
func (c *Charger) ChargeFor(e *rate.Entry, seconds int) (billedSecs int, cost float64, err error) {
	if e == nil {
		return 0, 0, fmt.Errorf("no rate entry")
	}
	if seconds < 0 {
		return 0, 0, fmt.Errorf("negative duration %d", seconds)
	}
	if seconds == 0 {
		return 0, 0, nil
	}

	initDur := e.InitDur
	stepDur := e.StepDur
	if initDur <= 0 {
		initDur = 60 // default first step = one minute
	}
	if stepDur <= 0 {
		stepDur = 1
	}

	// Billed seconds: talk duration mapped onto the step grid, then
	// lifted to MinDur.
	if seconds <= initDur {
		billedSecs = initDur
	} else {
		rest := seconds - initDur
		steps := (rest + stepDur - 1) / stepDur // ceil
		billedSecs = initDur + steps*stepDur
	}
	if e.MinDur > billedSecs {
		billedSecs = e.MinDur
	}

	// Price the full billed duration: first step at InitRate (or
	// price*initDur/60), remainder per second at Price/60.
	initPrice := e.InitRate
	if initPrice == 0 {
		initPrice = e.Price * float64(initDur) / 60
	}
	initPrice = roundUp7(initPrice)

	if billedSecs <= initDur {
		return billedSecs, initPrice, nil
	}
	incSecs := billedSecs - initDur
	incPrice := roundUp7(e.Price * float64(incSecs) / 60)
	return billedSecs, roundUp7(initPrice + incPrice), nil
}

// EstimatedCost is the pre-answer lock amount: what a call of the given
// duration would cost, used before the call starts (PRD: lock estimate
// then reconcile at BYE).
func (c *Charger) EstimatedCost(e *rate.Entry, expectedSecs int) float64 {
	if e == nil || expectedSecs <= 0 {
		return 0
	}
	_, cost, err := c.ChargeFor(e, expectedSecs)
	if err != nil {
		return 0
	}
	return cost
}

// roundUp7 mirrors VOS precision: 7 decimals, 8th rounds up.
func roundUp7(p float64) float64 {
	if p <= 0 {
		return p
	}
	scaled := p * 1e7
	whole := math.Floor(scaled)
	if scaled-whole > 1e-9 {
		whole++
	}
	return whole / 1e7
}
