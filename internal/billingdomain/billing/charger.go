package billing

import (
	"fmt"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
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
//
// All money is money.Amount (ADR-0003 fixed point): per-second prices
// round UP at the 4th decimal (the legacy 8th-decimal-up rule at the
// new scale), keeping the VOS "never undercharge" contract.
func (c *Charger) ChargeFor(e *rate.Entry, seconds int) (billedSecs int, cost money.Amount, err error) {
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
	// Price*initDur/60), remainder per second at Price/60 (ceil'd).
	initPrice := e.InitRate
	if initPrice == 0 {
		initPrice = e.Price.MulFrac(int64(initDur), 60)
	}

	if billedSecs <= initDur {
		return billedSecs, initPrice, nil
	}
	incSecs := int64(billedSecs - initDur)
	incPrice := e.Price.MulFrac(incSecs, 60)
	return billedSecs, initPrice.Add(incPrice), nil
}

// EstimatedCost is the pre-answer lock amount: what a call of the given
// duration would cost, used before the call starts (PRD: lock estimate
// then reconcile at BYE).
func (c *Charger) EstimatedCost(e *rate.Entry, expectedSecs int) money.Amount {
	if e == nil || expectedSecs <= 0 {
		return 0
	}
	_, cost, err := c.ChargeFor(e, expectedSecs)
	if err != nil {
		return 0
	}
	return cost
}
