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
	"github.com/gvos-io/gvos-core/internal/customerdomain/did"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

type callStartReq struct {
	CallID string `json:"call_id"`
	From   string `json:"from_user"`
	To     string `json:"to_user"`
	// FromTag is the SIP From-tag. With call_id it names the dialog:
	// a second dialog under a live Call-ID is refused instead of riding
	// it unbilled. Edges that predate it send none.
	FromTag string `json:"from_tag"`
	// Direction: "inbound" marks a carrier INVITE for a DID number
	// (trunk-admitted, no Digest credentials). The billing account is
	// then the DID owner (callee-billed) instead of the From user.
	// Absent/any other value = the historical outbound path.
	Direction string `json:"direction"`
}

// callStart is the pre-answer lock: screen, resolve the caller, check
// risk, rate, and reserve. Status: 200 locked, 403 blocked/broke,
// 404 unratable/unknown, 429 limits (Retry-After on the HTTP face).
//
// Inbound (direction="inbound"): a carrier INVITE for a DID. The
// billing account is the DID OWNER (callee-billed, pipecat O1
// resolved 2026-11) — $fU is the carrier's MSISDN and must never
// resolve to a billing account. The DID's destination pre-resolves
// the routing: "user" terminates locally, "external" dispatches out
// via the answer's gw_id, "media_agent" is the reserved AI seam and
// answers 501 (billing never locks a call the platform cannot route).
func (s *Server) callStart(req callStartReq) (int, any) {
	start := time.Now()
	defer func() {
		mRPCSeconds.Observe(time.Since(start).Seconds())
	}()
	var inbound did.DID
	if req.Direction == "inbound" {
		bd, ok := s.dids.Resolve(req.To)
		if !ok {
			return 404, map[string]errorEnvelope{"error": {Code: "NOT_FOUND", Message: "no DID registered for " + req.To}}
		}
		inbound = bd
	}
	// Caller resolution first: the risk engine and billing both need
	// the final account ID. Inbound skips it entirely: the DID owner
	// below IS the billing account.
	accID := req.From
	if accID == "" {
		accID = "1000" // spike default caller
	}
	if req.Direction != "inbound" {
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
	}
	if req.Direction == "inbound" {
		// Callee-billed: the DID owner's account is the lock target.
		// An owner that no longer exists is a 404, never a silent
		// fallback — the number must be deregistered first.
		if _, err := s.store.Get(inbound.AccountID); err != nil {
			return 404, map[string]errorEnvelope{"error": {Code: "NOT_FOUND", Message: "DID " + req.To + " owner account missing: " + inbound.AccountID}}
		}
		accID = inbound.AccountID
		if inbound.DestinationType == did.DestAgent {
			// Reserved AI voice-agent seam: refuse to bill what we
			// cannot route (501, not 403 — the callee is not at fault).
			return 501, map[string]errorEnvelope{"error": {Code: "NOT_IMPLEMENTED", Message: "DID " + req.To + " destination media_agent is not implemented yet"}}
		}
	}
	// AXB privacy resolution: dialing a virtual X number rewrites the
	// destination to the bound B number (billing + routing follow B;
	// the edge rewrites the R-URI from the rewrite_dest instruction).
	// Inbound DID calls skip AXB: the DID binding IS the translation.
	actualDest := req.To
	if req.Direction != "inbound" {
		if s.axb != nil {
			if bd, ok := s.axb.Resolve(req.To); ok {
				actualDest = bd.BNumber
			}
		}
	}
	// Number screening AFTER the AXB resolve: the blacklist semantics
	// is "this destination is forbidden" — screening only the dialed
	// X would let a banned B number be reached through any binding.
	// Both the dialed number and the real destination are screened.
	// Inbound: the caller ID is screened as src (the carrier's caller
	// is the untrusted party), the DID as dest.
	screenScope := "dest"
	if req.Direction == "inbound" {
		screenScope = "src"
	}
	for _, screened := range []string{req.To, actualDest} {
		if ok, why := s.blocklist.CheckScreen(screened, screenScope); !ok {
			return 403, map[string]errorEnvelope{"error": {Code: "FORBIDDEN", Message: "number blocked: " + why}}
		}
	}
	if req.Direction == "inbound" && inbound.DestinationType == did.DestExternal {
		// Outbound screening of the DID's real destination still
		// applies (an external second leg must pass dest rules).
		if ok, why := s.blocklist.CheckScreen(inbound.Destination, "dest"); !ok {
			return 403, map[string]errorEnvelope{"error": {Code: "FORBIDDEN", Message: "DID second-leg blocked: " + why}}
		}
	}

	// Rate lookup: inbound rates by the DID prefix (the callee's own
	// tariff — a dedicated inbound rate group is just a different
	// group on the owner account), outbound as before on the dialed
	// destination.
	now := time.Now()
	s.cfgMu.RLock()
	rates := s.rates
	s.cfgMu.RUnlock()
	rateDest := actualDest
	if req.Direction == "inbound" {
		rateDest = inbound.Number // rate the DID leg, not the carrier's caller
	}
	entry := rates.Lookup(rateDest, int(now.Weekday()), now.Hour()*60+now.Minute())
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
		username := req.From
		if a, gerr := s.store.Get(accID); gerr == nil {
			username = a.Username
		}
		s.publishRiskBlocked(accID, username, actualDest, h.RuleID, h.Reason)
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
	if err := s.calls.StartDialogOpts(req.CallID, accID, req.FromTag, actualDest, entry, 60, s.recordEnabled, callcore.CallOpts{
		Direction: req.Direction, // callcore normalizes "inbound" -> "in"
	}); err != nil {
		// Insufficient balance is a 403 for the caller; account limits
		// are a 429 (retry later); unknown account a 404.
		var limit *callcore.ErrLimit
		switch {
		case errors.As(err, &limit):
			return 429, map[string]errorEnvelope{"error": {Code: "RATE_LIMITED", Message: err.Error()}}
		case errors.Is(err, callcore.ErrCallIDInUse):
			// Another dialog is live under this Call-ID: the edge must
			// not let a second call ride it unbilled.
			return 409, map[string]errorEnvelope{"error": {Code: "CALL_ID_INUSE", Message: err.Error()}}
		case errors.Is(err, account.ErrUnavailable), errors.Is(err, account.ErrNotSeeded),
			errors.Is(err, callcore.ErrAdmissionUnavailable):
			// A platform fault (authority down, or holding no balance for
			// the account). 503, not 403: the caller is not broke and the
			// answer must not tell the edge that it is.
			return 503, map[string]errorEnvelope{"error": {Code: "AUTHORITY_UNAVAILABLE", Message: "billing authority unavailable, retry shortly"}}
		case strings.Contains(err.Error(), "not found"):
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
	if req.Direction == "inbound" {
		// Routing instruction for the edge (pipecat §7 item 2): the
		// control plane pre-resolved the DID's destination.
		answer["direction"] = "inbound"
		answer["destination_type"] = string(inbound.DestinationType)
		switch inbound.DestinationType {
		case did.DestUser:
			// lookup("location") target: the edge rewrites the R-URI
			// user part to the registered username.
			answer["rewrite_dest"] = inbound.Destination
		case did.DestExternal:
			// Second leg out via the carrier: the destination rides
			// rewrite_dest AND the gateway is pre-picked (per-dest LCR,
			// replacing the global best-index approximation).
			answer["rewrite_dest"] = inbound.Destination
			if gw, ok := s.gws.PickForDest(inbound.Destination); ok {
				answer["gw_id"] = gw.ID
			}
			// No gateway serves this prefix: the edge falls back to
			// global GW_DISPATCH, which 503s when the table is empty.
		case did.DestAgent:
			// unreachable: refused with 501 above
		}
	} else if actualDest != req.To {
		// AXB rewrite instruction: the edge replaces the R-URI user
		// part with the real destination before relaying to the gateway.
		answer["rewrite_dest"] = actualDest
	}
	mCallsStarted.Inc(1)
	return 200, answer
}

// callAnswer records the 200 OK: the billing clock starts here, and
// Interim only charges answered calls (a ringing or failed INVITE
// accrues nothing). A call this replica does not own still gets its
// answer parked on the shared meta for the owner; the edge gets a 200
// either way — it has nothing to retry.
func (s *Server) callAnswer(callID string) (int, any) {
	_ = s.calls.Answer(callID)
	return 200, map[string]string{"call_id": callID, "status": "live"}
}

// answeredSeconds is the fallback duration for an end event without a
// usable one: the wall clock since the answer, when the call is known
// to have been answered (the edge's answer timestamp, else the locally
// recorded one); 0 otherwise. Must run before End (it reads the call).
func (s *Server) answeredSeconds(callID string, answerTsMs int64) int {
	var at time.Time
	if answerTsMs > 0 {
		at = time.UnixMilli(answerTsMs)
	} else if t, ok := s.calls.AnswerTime(callID); ok {
		at = t
	}
	if at.IsZero() {
		return 0
	}
	if d := int(time.Since(at).Seconds()); d > 0 {
		return d
	}
	return 0
}

type callEndReq struct {
	CallID     string `json:"call_id"`
	DurSecs    int    `json:"duration"`    // optional: defaulted from call start
	DurMs      int    `json:"duration_ms"` // µs-clock duration from the edge (sub-second precision; the $TS second-granular field truncated 11% of calls to 0)
	AnswerTsMs int64  `json:"answer_ts"`   // epoch-ms of the 200 answer (0 = unanswered)
	PDDMs      int    `json:"pdd_ms"`      // post-dial delay measured by Kamailio
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
	if seconds <= 0 {
		// No usable duration arrived (a negative duration_ms from an
		// NTP step or a $TS/$TV mix lands here too). Only a call known
		// to be ANSWERED may be timed off the wall clock, since its
		// answer. An unanswered call bills nothing: deriving since the
		// INVITE — the old rule — charged ring time on calls that never
		// connected, and a 400 here would only NAK the consumer for a
		// payload that will never get better.
		seconds = s.answeredSeconds(req.CallID, req.AnswerTsMs)
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
	if pr.ExhaustedNow {
		// The allowance crossed its cap on this call. (The old
		// FreeSecs==0 && BillableSecs==0 test fired on unrelated
		// no-charge shapes; ExhaustedNow is the crossing itself.)
		s.emit(events.SubjectPackageExhausted, map[string]string{
			"account_id": cdr.AccountID, "call_id": cdr.CallID,
		})
		if pkg, ok := s.pkg.PackageOf(cdr.AccountID); ok {
			s.publishPackageExhausted(cdr.AccountID, pkg.ID, pkg.FreeMinutes)
		}
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
	// Low-balance watch (best-effort, deduped): settlement is where
	// balances dip below thresholds.
	s.publishLowBalance(cdr.AccountID)
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
		if err := s.persistSettlement(ctx, cdr); err != nil {
			log.Printf("settlement persist failed (call %s): %v", cdr.CallID, err)
		}
	}
	mCallsEnded.Inc(1)
	return 200, cdr
}
