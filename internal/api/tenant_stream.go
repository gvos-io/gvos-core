// Phase D wiring: contract event publication + the tenant CDR
// WebSocket stream + webhook delivery-log persistence.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/mediamain/mediastream"
	"github.com/gvos-io/gvos-core/internal/shared/store"
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
// tenant's CDR events in real time (api-roadmap Phase D §4). Data
// frames are cdr.completed projections (same JSON the webhook data
// carries); control frames are {"ctrl":...} objects (resume/dropped).
// Frames for other tenants are filtered before the fan-out, never
// client-side. Protocol: ping/pong both ways, 30s server heartbeat,
// ?since=RFC3339 replay window, dropped-frame control frames.
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
	// Resume cursor: ?since=RFC3339 replays that window (PG history or
	// the memory ring) before attaching to the live feed — a reconnect
	// no longer silently loses the disconnect window.
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		st, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			httpError(w, 400, "since must be RFC3339")
			return
		}
		since = st
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
	// streams every event for this tenant.
	set := s.tenantAccountsCached(id.TenantID)
	ch := make(chan []byte, 64)
	tap := &cdrTap{set: set, ch: ch}
	s.cdrTapsMu.Lock()
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

	// All writes (data, control, ping, pong) serialize here — the
	// reader answers pings from its goroutine while the loop below
	// streams, and RFC6455 forbids interleaved frames.
	var writeMu sync.Mutex
	write := func(op int, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := mediastream.WriteFrame(rw.Writer, op, payload); err != nil {
			return err
		}
		if f := rw.Flush; f != nil {
			f()
		}
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Reader loop: answer client pings (they used to be swallowed —
		// well-behaved clients would time out), honor closes.
		for {
			f, rerr := mediastream.ReadFrame(rw.Reader)
			if rerr != nil {
				return
			}
			switch f.Opcode {
			case mediastream.OpPingValue:
				if werr := write(mediastream.OpPongValue, f.Payload); werr != nil {
					return
				}
			case mediastream.OpCloseValue:
				return
			}
		}
	}()

	// Replay window (bounded): PG history in pages, the ring in memory
	// mode. partial=true tells the client the window exceeded the cap
	// and it should re-since from the earliest frame received.
	replayed, partial := 0, false
	if !since.IsZero() {
		writeFrame := func(c *callcore.CDR) bool {
			b, merr := json.Marshal(contractCDRData(c))
			if merr != nil {
				return true
			}
			return write(mediastream.OpTextValue, b) == nil
		}
		if s.pg != nil {
			cursor := ""
			for page := 0; page < 5; page++ {
				pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				rows, next, qerr := s.pg.QueryCDRs(pctx, store.CDRQuery{
					Cursor: cursor, Limit: 500, Accounts: setKeys(set), From: since,
				})
				cancel()
				if qerr != nil {
					break
				}
				for i := range rows {
					if !writeFrame(&rows[i]) {
						return
					}
					replayed++
				}
				if next == "" {
					break
				}
				cursor = next
				if page == 4 {
					partial = true
				}
			}
		} else {
			s.cdrsMu.Lock()
			ring := append([]*callcore.CDR(nil), s.cdrs...)
			s.cdrsMu.Unlock()
			// ring is oldest→newest; replay in chronological order.
			for _, c := range ring {
				if set[c.AccountID] && c.StartTime.After(since) {
					if !writeFrame(c) {
						return
					}
					replayed++
				}
			}
		}
	}
	if b, merr := json.Marshal(map[string]any{"ctrl": "resume", "replayed": replayed, "partial": partial}); merr == nil {
		_ = write(mediastream.OpTextValue, b)
	}

	// Live loop: stream frames, heartbeat every 30s (LB idle timeouts
	// used to kill silent connections), and surface drops — slow taps
	// lost frames silently before; the loss count now follows the next
	// drained frame or heartbeat as a {"ctrl":"dropped","n":N} frame.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	lastDropped := tap.dropped.Load()
	reportDrops := func() bool {
		if d := tap.dropped.Load() - lastDropped; d > 0 {
			lastDropped = tap.dropped.Load()
			if b, merr := json.Marshal(map[string]any{"ctrl": "dropped", "n": d}); merr == nil {
				return write(mediastream.OpTextValue, b) == nil
			}
		}
		return true
	}
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if err := write(mediastream.OpTextValue, frame); err != nil {
				return
			}
			if !reportDrops() {
				return
			}
		case <-heartbeat.C:
			if err := write(mediastream.OpPingValue, []byte("hb")); err != nil {
				return
			}
			if !reportDrops() {
				return
			}
		case <-done:
			return
		}
	}
}

// cdrTap is one tenant-plane stream subscriber: the account set is
// snapshotted at connect (a mid-stream account creation is picked up
// on reconnect — documented in api-conventions §Webhook).
type cdrTap struct {
	set     map[string]bool
	ch      chan []byte
	dropped atomic.Int64 // frames shed on a full buffer, surfaced to the client
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
				t.dropped.Add(1)
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
