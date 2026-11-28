// Phase D wiring: contract event publication + the tenant CDR
// WebSocket stream + webhook delivery-log persistence.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/mediamain/mediastream"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

// contractCDRData projects an internal CDR row into the customer
// contract shape (webhook.CDRCompletedData). Internal refactors stop
// here — the wire contract never sees internal field names.
func contractCDRData(c *callcore.CDR) webhook.CDRCompletedData {
	ans := ""
	if !c.AnswerTime.IsZero() {
		ans = c.AnswerTime.UTC().Format(time.RFC3339)
	}
	return webhook.CDRCompletedData{
		CallID:     c.CallID,
		AccountID:  c.AccountID,
		Dest:       c.Dest,
		StartTime:  c.StartTime.UTC().Format(time.RFC3339),
		AnswerTime: ans,
		EndTime:    c.StartTime.UTC().Add(time.Duration(c.Duration) * time.Second).Format(time.RFC3339),
		Duration:   c.Duration,
		BilledSecs: c.BilledSecs,
		Cost:       c.Cost.String(),
		Currency:   "CNY",
		Release:    c.Release,
		SIPCode:    c.SIPCode,
		PDDMs:      c.PDDMs,
		Recorded:   c.Recorded,
	}
}

// handleTenantCDRStream upgrades to WebSocket and streams the
// tenant's CDR events in real time (api-roadmap Phase D §4). Every
// frame is one contract envelope (cdr.completed). Frames for other
// tenants are filtered before the fan-out, never client-side.
func (s *Server) handleTenantCDRStream(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		httpError(w, 400, "websocket upgrade required")
		return
	}
	hj, ok2 := w.(http.Hijacker)
	if !ok2 {
		httpError(w, 500, "hijack unsupported")
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer conn.Close()

	accept := wsAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		return
	}
	if f := rw.Flush; f != nil {
		f()
	}

	// Register a per-tenant CDR tap. The CDR ring append is the hook:
	// storeCdr is called by both faces, so a channel fed from there
	// streams every contract envelope for this tenant.
	set := s.tenantAccounts(id.TenantID)
	ch := make(chan []byte, 64)
	s.cdrTapsMu.Lock()
	tap := &cdrTap{set: set, ch: ch}
	s.cdrTaps = append(s.cdrTaps, tap)
	s.cdrTapsMu.Unlock()
	defer func() {
		s.cdrTapsMu.Lock()
		for i, t := range s.cdrTaps {
			if t == tap {
				s.cdrTaps = append(s.cdrTaps[:i], s.cdrTaps[i+1:]...)
				break
			}
		}
		s.cdrTapsMu.Unlock()
	}()

	// Reader loop drains client pings/control frames until close.
	go func() {
		for {
			if _, err := mediastream.ReadFrame(rw.Reader); err != nil {
				return
			}
		}
	}()

	for frame := range ch {
		if err := mediastream.WriteFrame(rw.Writer, mediastream.OpTextValue, frame); err != nil {
			return
		}
		if f := rw.Flush; f != nil {
			f()
		}
	}
}

// cdrTap is one tenant-plane stream subscriber: the account set is
// snapshotted at connect (a mid-stream account creation is picked up
// on reconnect — documented in api-conventions §Webhook).
type cdrTap struct {
	set map[string]bool
	ch  chan []byte
}

// fanCDRTap delivers one contract envelope to every matching tap.
// Called from storeCdr (both HTTP and bus faces) — non-blocking, the
// observability tier never blocks billing (slow taps drop frames).
func (s *Server) fanCDRTap(env []byte, accountID string) {
	s.cdrTapsMu.Lock()
	defer s.cdrTapsMu.Unlock()
	for _, t := range s.cdrTaps {
		if t.set[accountID] {
			select {
			case t.ch <- env:
			default:
			}
		}
	}
}

// tapAndPublish is the single post-persist seam: fan the contract
// envelope to tenant CDR streams and to the webhook dispatcher
// (which wraps it into the per-subscription envelopes). Non-blocking
// everywhere — the billing path never waits on observability.
func (s *Server) tapAndPublish(cdr *callcore.CDR) {
	data := contractCDRData(cdr)
	if b, err := json.Marshal(data); err == nil {
		s.fanCDRTap(b, cdr.AccountID)
	}
	if s.whDispatch != nil {
		// Scoped: a tenant-owned subscription receives this call only
		// when its tenant is the caller's account or one of its
		// ancestors. Unscoped, every tenant on the platform was mailed
		// every call.
		s.whDispatch.PublishEnvelopeScoped(webhook.EventCDRCompleted, demoTenant, s.callOwners(cdr.AccountID), data)
	}
}
