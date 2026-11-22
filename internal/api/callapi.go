// Call-lifecycle core: the INVITE lock, answer acknowledgement, and
// BYE settlement logic shared by both transports — the HTTP surface
// (Kamailio http_connect) and the NATS service endpoints
// (gvos.svc.call.start|answer|end). HTTP wrappers stay thin: parse,
// call core, write response. Status codes are HTTP codes; the NATS
// responder carries them in the reply envelope.
package api

import (
	"context"
	"errors"
	"log"
	"math"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/billing"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

type callStartReq struct {
	CallID string `json:"call_id"`
	From   string `json:"from_user"`
	To     string `json:"to_user"`
}

// callStart is the pre-answer lock: screen, resolve the caller, check
// risk, rate, and reserve. Status: 200 locked, 403 blocked/broke,
// 404 unratable/unknown, 429 limits (Retry-After on the HTTP face).
func (s *Server) callStart(req callStartReq) (int, any) {
	start := time.Now()
	defer func() {
		mRPCSeconds.Observe(time.Since(start).Seconds())
	}()
	// Caller resolution first: the risk engine and billing both need
	// the final account ID.
	accID := req.From
	if accID == "" {
		accID = "1000" // spike default caller
	}
	// Unknown callers resolve by SIP username (Kamailio sends $fU,
	// the auth username). The "fall back to the demo account" spike
	// shortcut is memory-mode only: in PG mode an unknown caller is a
	// hard 404 — billing must never silently land on another
	// account's balance.
	if _, err := s.store.Get(accID); err != nil {
		s.cfgMu.RLock()
		id, ok := s.userIndex[accID]
		s.cfgMu.RUnlock()
		if ok {
			accID = id
		} else if s.pg == nil {
			if _, err2 := s.store.Get("1000"); err2 == nil {
				accID = "1000"
			}
		}
	}
	// AXB privacy resolution: dialing a virtual X number rewrites the
	// destination to the bound B number (billing + routing follow B;
	// the edge rewrites the R-URI from the rewrite_dest instruction).
	actualDest := req.To
	if s.axb != nil {
		if bd, ok := s.axb.Resolve(req.To); ok {
			actualDest = bd.BNumber
		}
	}
	// Number screening AFTER the AXB resolve: the blacklist semantics
	// is "this destination is forbidden" — screening only the dialed
	// X would let a banned B number be reached through any binding.
	// Both the dialed number and the real destination are screened.
	for _, screened := range []string{req.To, actualDest} {
		if ok, why := s.blocklist.CheckScreen(screened, "dest"); !ok {
			return 403, map[string]errorEnvelope{"error": {Code: "FORBIDDEN", Message: "destination blocked: " + why}}
		}
	}

	now := time.Now()
	s.cfgMu.RLock()
	rates := s.rates
	s.cfgMu.RUnlock()
	entry := rates.Lookup(actualDest, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		return 404, map[string]errorEnvelope{"error": {Code: "NOT_FOUND", Message: "no rate for destination " + actualDest}}
	}
	// Fraud engine: frequency/cost/prefix rules against the REAL
	// destination (post-AXB). Daily-cost estimate uses the same 60s
	// lock basis as Start — never a hardcoded stub amount.
	estCost := (&billing.Charger{}).EstimatedCost(entry, 60)
	if h := s.risk.CheckCall(accID, actualDest, estCost); h.Blocked {
		s.emit(events.SubjectRiskBlocked, map[string]string{
			"account_id": accID, "dest": actualDest, "rule_id": h.RuleID, "reason": h.Reason,
		})
		return 403, map[string]errorEnvelope{"error": {Code: "FORBIDDEN", Message: "risk-blocked [" + h.RuleID + "]: " + h.Reason}}
	} else if h.Alerted {
		log.Printf("RISK-ALERT account=%s rule=%s: %s", accID, h.RuleID, h.Reason)
		username := req.From
		if a, err := s.store.Get(accID); err == nil {
			username = a.Username
		}
		s.emit(events.SubjectRiskAlert, map[string]string{
			"account": accID, "username": username, "rule_id": h.RuleID, "reason": h.Reason, "to": actualDest,
		})
	}
	if err := s.calls.Start(req.CallID, accID, actualDest, entry, 60, s.recordEnabled); err != nil {
		// Insufficient balance is a 403 for the caller; account limits
		// are a 429 (retry later); unknown account a 404.
		var limit *callcore.ErrLimit
		if errors.As(err, &limit) {
			return 429, map[string]errorEnvelope{"error": {Code: "RATE_LIMITED", Message: err.Error()}}
		}
		if strings.Contains(err.Error(), "not found") {
			return 404, map[string]errorEnvelope{"error": {Code: "NOT_FOUND", Message: err.Error()}}
		}
		return 403, map[string]errorEnvelope{"error": {Code: "INSUFFICIENT_CREDIT", Message: err.Error()}}
	}
	// Media plane: open the bidirectional stream so AI subscribers
	// can attach even before the first RTP chunk.
	s.media.Open(req.CallID)

	answer := map[string]any{
		"call_id":     req.CallID,
		"rate_prefix": entry.Prefix,
		"price":       entry.Price,
		"lock":        entry.Price,
		// Recording opt-in: Kamailio calls start_recording() when true.
		"record": s.recordEnabled,
	}
	if actualDest != req.To {
		// AXB rewrite instruction: the edge replaces the R-URI user
		// part with the real destination before relaying to the gateway.
		answer["rewrite_dest"] = actualDest
	}
	mCallsStarted.Inc(1)
	return 200, answer
}

// callAnswer switches the call from lock to live.
func (s *Server) callAnswer(callID string) (int, any) {
	// Interim starts on the first tick; the manager has no explicit
	// answer event in the spike, so acknowledge.
	return 200, map[string]string{"call_id": callID, "status": "live"}
}

type callEndReq struct {
	CallID     string `json:"call_id"`
	DurSecs    int    `json:"duration"`    // optional: defaulted from call start
	DurMs      int    `json:"duration_ms"` // µs-clock duration from the edge (sub-second precision; the $TS second-granular field truncated 11% of calls to 0)
	AnswerTsMs int64  `json:"answer_ts"`  // epoch-ms of the 200 answer (0 = unanswered)
	PDDMs      int    `json:"pdd_ms"`     // post-dial delay measured by Kamailio
	SIPCode    int    `json:"sip_code"`
	Release    string `json:"release"` // normal | balance-exhausted | ...
	QoS        string `json:"qos"`     // rtpengine $rtpstat text (U6)
}

// maxInMemoryCDRs caps the recent-CDR ring: the CDR list API is a
// "what just happened" view, PG is the history authority. The cap
// bounds the memory a long-lived replica burns (the U1 drill
// accumulated 19k rows in-memory for nothing).
const maxInMemoryCDRs = 10000

// storeCdr appends into the bounded, lock-guarded ring. Callers run
// on both the HTTP face and the 16 NATS responder workers — the
// lock is what keeps handleCDRList/statsBody racing them safe.
func (s *Server) storeCdr(cdr *callcore.CDR) {
	s.cdrsMu.Lock()
	if len(s.cdrs) >= maxInMemoryCDRs {
		// Drop from the head: copy the newest max-1 forward, then
		// overwrite the last slot. Steady-state cost is one memmove,
		// not a realloc.
		copy(s.cdrs, s.cdrs[1:])
		s.cdrs[len(s.cdrs)-1] = cdr
		s.cdrsMu.Unlock()
		s.tapAndPublish(cdr)
		return
	}
	s.cdrs = append(s.cdrs, cdr)
	s.cdrsMu.Unlock()
	s.tapAndPublish(cdr)
}

// callEnd finalizes billing and returns the CDR. ctx drives the
// persistence block (HTTP request ctx / background for the bus face);
// persistence failures log but never block the billing answer.
func (s *Server) callEnd(ctx context.Context, req callEndReq) (int, any) {
	seconds := req.DurSecs
	if req.DurMs > 0 {
		// Edge-measured µs-clock duration: ceil to whole seconds so
		// billing never undercounts the partial second (the 60+6
		// first step absorbs it anyway; this keeps second-grid
		// semantics while escaping the $TS truncation).
		seconds = (req.DurMs + 999) / 1000
	}
	if seconds == 0 {
		// Kamailio sends the BYE without duration: derive it from the
		// call's start time (SIPp's failed call lasted ~0s, real calls
		// bill their true length).
		if info, ok := s.calls.Info(req.CallID); ok {
			seconds = int(time.Since(info).Seconds())
		}
	}
	if seconds < 0 {
		// Edge clocks can publish a negative duration_ms (NTP step or
		// $TS/$TV mix). Clamp and fall back to wall-clock since Start
		// instead of 400 — a 400 on the CDRR path NAKs and burns the
		// consumer (MaxDeliver×1s) for a non-retryable payload.
		seconds = 0
		if info, ok := s.calls.Info(req.CallID); ok {
			seconds = int(time.Since(info).Seconds())
			if seconds < 0 {
				seconds = 0
			}
		}
	}
	cdr, err := s.calls.End(req.CallID, seconds)
	if err != nil {
		return 404, map[string]errorEnvelope{"error": {Code: "NOT_FOUND", Message: err.Error()}}
	}
	// Subscription package: free allowance first, then discount on the
	// billable remainder. End() already charged list price — refund the
	// gap so Redis/memory authority matches the CDR/ledger amount.
	listCost := cdr.Cost
	pr := s.pkg.Apply(cdr.AccountID, cdr.BilledSecs, cdr.Cost)
	if pr.FreeSecs == 0 && pr.BillableSecs == 0 && cdr.BilledSecs > 0 {
		// Contract event (Phase D): the free allowance and the paid
		// tier both drained — the package is exhausted for this cycle.
		s.emit(events.SubjectPackageExhausted, map[string]string{
			"account_id": cdr.AccountID, "call_id": cdr.CallID,
		})
	}
	if pr.BillableSecs < float64(cdr.BilledSecs) || pr.Discount != 1 {
		if float64(cdr.BilledSecs) > 0 {
			// Ratio math stays float (proportion); the product returns
			// to fixed point, ceil'd (never undercharge).
			ratio := pr.BillableSecs / float64(cdr.BilledSecs) * pr.Discount
			cdr.Cost = money.Amount(int64(math.Ceil(float64(cdr.Cost) * ratio)))
			cdr.FreeSecs = int(pr.FreeSecs)
			if refund := listCost.Sub(cdr.Cost); refund > 0 {
				if err := s.calls.Refund(cdr.AccountID, refund); err != nil {
					log.Printf("package refund failed (call %s): %v", cdr.CallID, err)
				}
			}
		}
	}

	// Fraud engine settlement: reconcile the daily cost accumulation
	// with the actual CDR cost (EstCost is the same 60s lock basis
	// reserved at CheckCall/Start).
	s.risk.SettleCost(cdr.AccountID, cdr.EstCost, cdr.Cost)
	// Media plane: close the call's stream (subscribers drop, final
	// stats reported for the RTT budget audit).
	if st, ok := s.media.Close(req.CallID); ok && st.UplinkChunks+st.DownlinkChunks > 0 {
		log.Printf("MEDIA call=%s up=%d down=%d maxRTT=%dms", req.CallID,
			st.UplinkChunks, st.DownlinkChunks, st.MaxRTTMs)
	}
	if req.PDDMs > 0 {
		cdr.PDDMs = req.PDDMs
	}
	if req.AnswerTsMs > 0 {
		cdr.AnswerTime = time.UnixMilli(req.AnswerTsMs)
	}
	if req.SIPCode > 0 {
		cdr.SIPCode = req.SIPCode
	}
	if req.Release != "" {
		cdr.Release = req.Release
	}
	if req.QoS != "" {
		cdr.Mos, cdr.JitterMs, cdr.PacketLoss = callcore.ParseRTPStat(req.QoS)
	}
	s.storeCdr(cdr)
	// Event bus: the fully-stamped CDR fans out in both modes (the
	// in-memory demo is a valid publisher target for downstream tests).
	s.emit(events.SubjectCDRCompleted, cdr)
	if s.pg != nil && !s.asyncCdr {
		// Fallback direct write (bus off or consumer unavailable).
		// With the CDRW worker active, persistence is event-driven:
		// the consumer owns this write and redelivers on failure.
		if _, err := s.pg.InsertCDR(ctx, cdr); err != nil {
			log.Printf("CDR persist failed (call %s): %v", cdr.CallID, err)
		}
		// Ledger snapshot reads the authority (Redis when wired).
		var bal money.Amount
		if s.bal != nil {
			bal, _ = s.bal.Get(context.Background(), cdr.AccountID)
		} else if b, err := account.Balance(s.store, cdr.AccountID); err == nil {
			bal = b
		}
		if err := s.pg.AppendLedger(ctx, cdr.AccountID, -cdr.Cost, bal, "call-settle", cdr.CallID); err != nil {
			log.Printf("ledger persist failed (call %s): %v", cdr.CallID, err)
		}
	}
	mCallsEnded.Inc(1)
	return 200, cdr
}
