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
	"strings"
	"time"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/callcore"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
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
	// Number screening happens before rating: a blacklisted (or
	// non-whitelisted) destination never reaches billing.
	if ok, why := s.blocklist.CheckScreen(req.To, "dest"); !ok {
		return 403, map[string]string{"error": "destination blocked: " + why}
	}
	// Caller resolution first: the risk engine and billing both need
	// the final account ID.
	accID := req.From
	if accID == "" {
		accID = "1000" // spike default caller
	}
	// Spike compatibility: unknown callers resolve by SIP username
	// (Kamailio sends $fU which is the auth username), then fall back
	// to the demo account. Production rejects with 401/404 after auth.
	if _, err := s.store.Get(accID); err != nil {
		if id, ok := s.userIndex[accID]; ok {
			accID = id
		} else if _, err2 := s.store.Get("1000"); err2 == nil {
			accID = "1000"
		}
	}
	// Fraud engine: frequency/cost/prefix rules, block or alert.
	// Estimated cost for the cost-cap check uses the 60s lock basis.
	if h := s.risk.CheckCall(accID, req.To, 0.05); h.Blocked {
		return 403, map[string]string{"error": "risk-blocked [" + h.RuleID + "]: " + h.Reason}
	} else if h.Alerted {
		log.Printf("RISK-ALERT account=%s rule=%s: %s", accID, h.RuleID, h.Reason)
		// username rides the payload: the Kamailio edge keys its
		// banned htable on $fU (SIP username), not the account ID.
		username := req.From
		if a, err := s.store.Get(accID); err == nil {
			username = a.Username
		}
		s.emit(events.SubjectRiskAlert, map[string]string{
			"account": accID, "username": username, "rule_id": h.RuleID, "reason": h.Reason, "to": req.To,
		})
	}

	now := time.Now()
	entry := s.rates.Lookup(req.To, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		return 404, map[string]string{"error": "no rate for destination " + req.To}
	}
	if err := s.calls.Start(req.CallID, accID, req.To, entry, 60, s.recordEnabled); err != nil {
		// Insufficient balance is a 403 for the caller; account limits
		// are a 429 (retry later); unknown account a 404.
		var limit *callcore.ErrLimit
		if errors.As(err, &limit) {
			return 429, map[string]string{"error": err.Error()}
		}
		if strings.Contains(err.Error(), "not found") {
			return 404, map[string]string{"error": err.Error()}
		}
		return 403, map[string]string{"error": err.Error()}
	}
	// Media plane: open the bidirectional stream so AI subscribers
	// can attach even before the first RTP chunk.
	s.media.Open(req.CallID)

	return 200, map[string]any{
		"call_id":     req.CallID,
		"rate_prefix": entry.Prefix,
		"price":       entry.Price,
		"lock":        entry.Price,
		// Recording opt-in: Kamailio calls start_recording() when true.
		"record": s.recordEnabled,
	}
}

// callAnswer switches the call from lock to live.
func (s *Server) callAnswer(callID string) (int, any) {
	// Interim starts on the first tick; the manager has no explicit
	// answer event in the spike, so acknowledge.
	return 200, map[string]string{"call_id": callID, "status": "live"}
}

type callEndReq struct {
	CallID  string `json:"call_id"`
	DurSecs int    `json:"duration"` // optional: defaulted from call start
	PDDMs   int    `json:"pdd_ms"`   // post-dial delay measured by Kamailio
	SIPCode int    `json:"sip_code"`
	Release string `json:"release"` // normal | balance-exhausted | ...
}

// callEnd finalizes billing and returns the CDR. ctx drives the
// persistence block (HTTP request ctx / background for the bus face);
// persistence failures log but never block the billing answer.
func (s *Server) callEnd(ctx context.Context, req callEndReq) (int, any) {
	seconds := req.DurSecs
	if seconds == 0 {
		// Kamailio sends the BYE without duration: derive it from the
		// call's start time (SIPp's failed call lasted ~0s, real calls
		// bill their true length).
		if info, ok := s.calls.Info(req.CallID); ok {
			seconds = int(time.Since(info).Seconds())
		}
	}
	cdr, err := s.calls.End(req.CallID, seconds)
	if err != nil {
		return 404, map[string]string{"error": err.Error()}
	}
	// Subscription package: free allowance first, then discount on the
	// billable remainder. The CDR carries the effective cost; free
	// seconds are recorded in the record for the bill view.
	if pr := s.pkg.Apply(cdr.AccountID, cdr.BilledSecs, cdr.Cost); pr.BillableSecs < float64(cdr.BilledSecs) || pr.Discount != 1 {
		if float64(cdr.BilledSecs) > 0 {
			ratio := pr.BillableSecs / float64(cdr.BilledSecs)
			cdr.Cost = roundUp7f(cdr.Cost * ratio * pr.Discount)
			cdr.FreeSecs = int(pr.FreeSecs)
		}
	}

	// Fraud engine settlement: reconcile the daily cost accumulation
	// with the actual CDR cost (estimate was 0.05 basis at start).
	s.risk.SettleCost(cdr.AccountID, 0.05, cdr.Cost)
	// Media plane: close the call's stream (subscribers drop, final
	// stats reported for the RTT budget audit).
	if st, ok := s.media.Close(req.CallID); ok && st.UplinkChunks+st.DownlinkChunks > 0 {
		log.Printf("MEDIA call=%s up=%d down=%d maxRTT=%dms", req.CallID,
			st.UplinkChunks, st.DownlinkChunks, st.MaxRTTMs)
	}
	if req.PDDMs > 0 {
		cdr.PDDMs = req.PDDMs
	}
	if req.SIPCode > 0 {
		cdr.SIPCode = req.SIPCode
	}
	if req.Release != "" {
		cdr.Release = req.Release
	}
	s.cdrs = append(s.cdrs, cdr)
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
		if bal, err := account.Balance(s.store, cdr.AccountID); err == nil {
			if err := s.pg.AppendLedger(ctx, cdr.AccountID, -cdr.Cost, bal, "call-settle", cdr.CallID); err != nil {
				log.Printf("ledger persist failed (call %s): %v", cdr.CallID, err)
			}
		}
	}
	return 200, cdr
}
