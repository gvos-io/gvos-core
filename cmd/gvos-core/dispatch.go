// Dispatch components that ride the NATS bus: async persistence
// workers (CDR writer, audit writer), the teardown executor, the
// webhook work-queue consumer, config-reload broadcast subscriber,
// and the call/stats service endpoints. Each is optional at runtime
// (bus off / URL unset / core-mode degradation) and none of them may
// block or fail the billing path — the bus is observability and async
// plumbing, not authority.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lixuanqun/gvos-core/internal/account"
	"github.com/lixuanqun/gvos-core/internal/blocklist"
	"github.com/lixuanqun/gvos-core/internal/callstate"
	"github.com/lixuanqun/gvos-core/internal/events"
	"github.com/lixuanqun/gvos-core/internal/gateway"
	"github.com/lixuanqun/gvos-core/internal/ipallow"
	"github.com/lixuanqun/gvos-core/internal/pkgplan"
	"github.com/lixuanqun/gvos-core/internal/riskctrl"
)

// startAsyncWorkers launches the durable write/exec workers. Each
// flips its Server flag so the sync paths stand down (asyncCdr /
// asyncAudit) and records itself for /v1/stats. Consumers compete
// across replicas (shared durable names) and NAK-redeliver failures,
// so a crashed worker's backlog drains on restart.
func startAsyncWorkers(s *Server) {
	// Persistence workers need PG: memory mode publishes events but
	// keeps no durable state, so the handlers must not run there.
	if s.pg == nil {
		log.Printf("async persistence workers skipped (memory mode: CDR/audit are bus-events only)")
	} else {
		// CDR persistence: at-least-once writes with exactly-once effects
		// (uq_cdr_call collapses redeliveries).
		if c, err := s.events.NewDurableConsumer("CDRW", events.SubjectCDRCompleted,
			func(data []byte) error { return s.cdrPersistHandler(data) }, 2); err != nil {
			log.Printf("CDR writer disabled (sync fallback stays): %v", err)
		} else {
			s.asyncCdr = true
			s.workers = append(s.workers, c)
			s.workerNames = append(s.workerNames, "cdrw")
		}

		// Audit trail: the admin.changed feed IS the audit record.
		if c, err := s.events.NewDurableConsumer("AUDIT", events.SubjectAdminChanged,
			func(data []byte) error { return s.auditWriterHandler(data) }, 1); err != nil {
			log.Printf("audit writer disabled (sync fallback stays): %v", err)
		} else {
			s.asyncAudit = true
			s.workers = append(s.workers, c)
			s.workerNames = append(s.workerNames, "audit")
		}
	}

	// Teardown executor: consumes the teardown event itself. If the
	// executor is down, instructions persist in the stream and drain
	// on restart — no req/reply timeout to lose.
	if c, err := s.events.NewDurableConsumer("TEARDOWN", events.SubjectCallTeardown,
		func(data []byte) error { return s.teardownDispatchHandler(data) }, 1); err != nil {
		// Core mode (no JetStream): broadcast fallback — acceptable in
		// the degraded single-node shape; duplicate CANCELs are
		// harmless against unknown calls.
		if err2 := s.events.Subscribe(events.SubjectCallTeardown, func(data []byte) {
			_ = s.teardownDispatchHandler(data)
		}); err2 != nil {
			log.Printf("teardown executor disabled (direct-call fallback stays): %v / %v", err, err2)
			return
		}
		s.workerNames = append(s.workerNames, "teardown(broadcast)")
		log.Printf("teardown executor running in broadcast mode (no JetStream)")
	} else {
		s.workers = append(s.workers, c)
		s.workerNames = append(s.workerNames, "teardown")
	}
}

// teardownTarget is the Kamailio signaling-plane API the teardown
// executor drives: JSON-RPC (jsonrpcs module, ctl/http transports).
// The URL is the environment seam (GVOS_KAMAILIO_RPC, e.g.
// "http://kamailio:5060/RPC"); empty = executor logs only.
var teardownTarget = os.Getenv("GVOS_KAMAILIO_RPC")

// dispatchTeardown is the teardown executor: terminates the call on
// the Kamailio node that holds the dialog. Primary path is the bus —
// the teardown event itself IS the instruction (every Kamailio node
// subscribes; the one holding the dialog terminates it, others
// no-op), and the TEARDOWN durable consumer persists retries while
// the executor is down. This function is the offline seam: a direct
// JSON-RPC dlg.terminate_dlg for the no-bus / NATS-down shape, so the
// <1s forced-release budget has a second road to the edge.
func dispatchTeardown(callID, reason string) {
	if teardownTarget == "" {
		log.Printf("TEARDOWN dispatch call=%s reason=%s (no GVOS_KAMAILIO_RPC: bus path only)", callID, reason)
		return
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "dlg.terminate_dlg",
		"params": map[string]any{"callid": callID}, "id": 1,
	})
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, teardownTarget, bytes.NewReader(body))
	if err != nil {
		log.Printf("TEARDOWN rpc build failed for %s: %v", callID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// Bus path remains: the durable consumer redelivers, and the
		// Kamailio subscription may already have handled it.
		log.Printf("TEARDOWN rpc failed for %s (bus path unaffected): %v", callID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("TEARDOWN rpc rejected for %s: status %d", callID, resp.StatusCode)
	}
}

func (s *Server) teardownDispatchHandler(data []byte) error {
	var p struct {
		CallID string `json:"call_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.CallID == "" {
		return fmt.Errorf("bad teardown payload: %w", err)
	}
	dispatchTeardown(p.CallID, p.Reason)
	return nil
}

// cdrPersistHandler is the CDRW worker body: parse the CDR event and
// persist it plus the settlement ledger row. Idempotent via
// uq_cdr_call — a redelivery finds inserted=false and skips the
// ledger, so at-least-once delivery never double-writes. The ledger
// balance snapshot is taken at consume time: the delta is exact, the
// balance column is a best-effort view of a live number.
func (s *Server) cdrPersistHandler(data []byte) error {
	var cdr callstate.CDR
	if err := json.Unmarshal(data, &cdr); err != nil {
		return fmt.Errorf("bad CDR payload: %w", err)
	}
	ctx, cancel := contextWithTimeoutDuration(context.Background(), 10*time.Second)
	defer cancel()
	inserted, err := s.pg.InsertCDR(ctx, &cdr)
	if err != nil {
		return err
	}
	if !inserted {
		return nil // duplicate delivery: row and ledger already stored
	}
	if bal, err := account.Balance(s.store, cdr.AccountID); err == nil {
		if err := s.pg.AppendLedger(ctx, cdr.AccountID, -cdr.Cost, bal, "call-settle", cdr.CallID); err != nil {
			return err
		}
	}
	return nil
}

// auditWriterHandler is the AUDIT worker body: the admin.changed feed
// carries the exact AppendAudit fields (rollup events have no
// before/after — they arrive as nulls).
func (s *Server) auditWriterHandler(data []byte) error {
	var p struct {
		Action   string          `json:"action"`
		Entity   string          `json:"entity"`
		EntityID string          `json:"entity_id"`
		Before   json.RawMessage `json:"before"`
		After    json.RawMessage `json:"after"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("bad audit payload: %w", err)
	}
	if p.Action == "" || p.Entity == "" {
		return errors.New("audit payload missing action/entity")
	}
	ctx, cancel := contextWithTimeoutDuration(context.Background(), 10*time.Second)
	defer cancel()
	var before, after any = p.Before, p.After
	if len(p.Before) == 0 || string(p.Before) == "null" {
		before = nil
	}
	if len(p.After) == 0 || string(p.After) == "null" {
		after = nil
	}
	return s.pg.AppendAudit(ctx, demoTenant, "admin", p.Action, p.Entity, p.EntityID, before, after)
}

// startWebhookDispatcher consumes gvos.cdr.completed from the shared
// WEBHOOKS durable: competing replicas load-balance deliveries, a
// failed POST NAKs with a 1s redelivery delay (at-least-once), and
// MaxDeliver=5 quarantines poison payloads for operations.
func startWebhookDispatcher(s *Server) *events.Consumer {
	consumer, err := s.events.NewDurableConsumer("WEBHOOKS", events.SubjectCDRCompleted,
		func(data []byte) error { return postWebhook(s.webhookURL, data) }, 2)
	if err != nil {
		log.Printf("webhook dispatcher disabled: %v", err)
		return nil
	}
	return consumer
}

// postWebhook delivers one event. 2xx = done; anything else (network,
// timeout, 5xx, 4xx) is a redelivery-worthy failure.
func postWebhook(url string, payload []byte) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GVOS-Event", events.SubjectCDRCompleted)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &webhookStatusError{code: resp.StatusCode}
	}
	return nil
}

type webhookStatusError struct{ code int }

func (e *webhookStatusError) Error() string {
	return "webhook endpoint returned status " + http.StatusText(e.code)
}

// gatewayApply applies one decoded cfg.reload gateway payload (the
// full-array set; idempotent). Shared by the bus subscriber and tests.
func (s *Server) gatewayApply(m map[string]any) error {
	raw, err := json.Marshal(m["gateways"])
	if err != nil {
		return err
	}
	var gws []gateway.Gateway
	if err := json.Unmarshal(raw, &gws); err != nil {
		return err
	}
	n, err := s.gws.Reload(gws)
	if err == nil {
		log.Printf("CFG-RELOAD gateway set applied via bus (%d entries)", n)
	}
	return err
}

// payloadOf decodes a broadcast body to a generic map (subscriber
// entity dispatch).
func payloadOf(data []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// startRegConsumer mirrors the signaling nodes' registration events
// (gvos.reg.event) into the reg table and feeds the risk engine's
// reg_freq rules. An alert hit rides the existing risk.alert subject
// so the Kamailio banned htable (and any webhook) sees it exactly
// like a call-path alert. Plain subscribe: every replica maintains a
// full mirror; the table is observability, not work distribution.
func startRegConsumer(s *Server) {
	if err := s.events.Subscribe(events.SubjectRegEvent, func(data []byte) {
		var ev struct {
			User    string `json:"user"`
			Op      string `json:"op"` // "reg" | "expire"
			Expires int64  `json:"expires"`
			SrcIP   string `json:"src_ip"`
		}
		if err := json.Unmarshal(data, &ev); err != nil || ev.User == "" {
			log.Printf("REG bad payload: %v", err)
			return
		}
		s.regs.Apply(ev.User, ev.Op, ev.Expires, ev.SrcIP, time.Now())
		if ev.Op == "reg" {
			if h := s.risk.CheckReg(ev.User); h.Alerted {
				log.Printf("RISK-ALERT reg user=%s rule=%s: %s", ev.User, h.RuleID, h.Reason)
				s.emit(events.SubjectRiskAlert, map[string]string{
					"user": ev.User, "username": ev.User, "rule_id": h.RuleID, "reason": h.Reason, "src_ip": ev.SrcIP,
				})
			}
		}
	}); err != nil {
		log.Printf("registration mirror disabled: %v", err)
	}
}

// cdrRawHandler is the CDRR consumer body: finalizes billing from
// the signaling edge's raw call-end events (gvos.cdr.raw — Kamailio
// nats_publish instead of the /v1/call/end HTTP call). Payload is the
// callEndReq shape; callEnd is the same pure function the HTTP face
// uses, so the two transports cannot diverge.
func cdrRawHandler(s *Server) func(data []byte) error {
	return func(data []byte) error {
		var req callEndReq
		if err := json.Unmarshal(data, &req); err != nil || req.CallID == "" {
			return fmt.Errorf("bad cdr.raw payload: %w", err)
		}
		code, body := s.callEnd(context.Background(), req)
		if code >= 400 {
			return fmt.Errorf("callEnd rejected call=%s: %v", req.CallID, body)
		}
		return nil
	}
}

// startCdrRawConsumer runs the cdr.raw finalizer: durable
// (at-least-once) in JetStream mode — uq_cdr_call collapses
// redeliveries; plain subscribe in core mode (degraded single-node
// shape, same as the teardown fallback).
func startCdrRawConsumer(s *Server) {
	handler := cdrRawHandler(s)
	if c, err := s.events.NewDurableConsumer("CDRR", events.SubjectCDRRaw, handler, 1); err != nil {
		if err2 := s.events.Subscribe(events.SubjectCDRRaw, func(data []byte) {
			_ = handler(data)
		}); err2 != nil {
			log.Printf("cdr.raw consumer disabled: %v / %v", err, err2)
			return
		}
		log.Printf("cdr.raw consumer running in broadcast mode (no JetStream)")
		return
	} else {
		s.workers = append(s.workers, c)
	}
	s.workerNames = append(s.workerNames, "cdrr")
}

// startCfgRebroadcaster re-publishes the edge-facing snapshots
// (gateway, ip_allowlist, blocklist) every 60s. The Kamailio nats
// subscriptions are core NATS (at-most-once): a node that was down or
// reconnecting when a change landed never sees that message, and the
// nats module has no JetStream replay. A periodic full-array replay
// heals the gap — at most one minute of drift on the edge. All
// payloads are full-array and idempotent, so replicas and the edge
// can apply them any number of times.
func startCfgRebroadcaster(s *Server) {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.gws.Gateways()})
			// ip_allowlist: pre-joined CIDR lists (ipAllowPayload) —
			// the edge cannot rebuild them from the rules array.
			s.emit(events.SubjectCfgReload, ipAllowPayload(s.ipallow.Rules()))
			// Blocklist: full replay from the live compiled set (the
			// per-op broadcasts carry single entries only). add-ops are
			// idempotent on every consumer.
			for _, e := range s.blocklist.Snapshot() {
				// Lowercase keys: the Kamailio BL_APPLY route parses
				// kind/scope/pattern (the Go struct field names would
				// serialize capitalized and never match).
				s.emit(events.SubjectCfgReload, map[string]any{
					"entity": "blocklist", "op": "add",
					"entry": map[string]any{
						"kind": e.Kind, "scope": e.Scope, "pattern": e.Pattern,
					},
				})
			}
		}
	}()
}

// startCallSvc exposes the call lifecycle over NATS request/reply —
// the same core logic as the HTTP face (callStart/callAnswer/callEnd),
// reply envelope {code, body} carries the HTTP status so bus clients
// see identical semantics. Kamailio stays on HTTP for the lock (sync
// gate); this is the endpoint for bus-native clients and the landing
// spot if signaling ever moves to nats_publish + a bridge.
func startCallSvc(s *Server) {
	callReply := func(code int, body any) []byte {
		b, _ := json.Marshal(map[string]any{"code": code, "body": body})
		return b
	}
	for subject, serve := range map[string]func(req []byte) []byte{
		events.SubjectCallStart: func(req []byte) []byte {
			var r callStartReq
			if err := json.Unmarshal(req, &r); err != nil || r.CallID == "" || r.To == "" {
				return callReply(400, map[string]string{"error": "call_id and to_user required"})
			}
			code, body := s.callStart(r)
			return callReply(code, body)
		},
		events.SubjectCallAnswer: func(req []byte) []byte {
			var r struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(req, &r); err != nil || r.CallID == "" {
				return callReply(400, map[string]string{"error": "call_id required"})
			}
			code, body := s.callAnswer(r.CallID)
			return callReply(code, body)
		},
		events.SubjectCallEnd: func(req []byte) []byte {
			var r callEndReq
			if err := json.Unmarshal(req, &r); err != nil || r.CallID == "" {
				return callReply(400, map[string]string{"error": "call_id required"})
			}
			code, body := s.callEnd(context.Background(), r)
			return callReply(code, body)
		},
	} {
		if err := s.events.Respond(subject, serve); err != nil {
			log.Printf("call service %s disabled: %v", subject, err)
		}
	}
	log.Printf("NATS call service ready (gvos.svc.call.start|answer|end)")
}

// startStatsSvc serves /v1/stats-equivalent snapshots over req/reply
// for ops tooling that lives on the bus.
func startStatsSvc(s *Server) {
	if err := s.events.Respond(events.SubjectStats, func(req []byte) []byte {
		b, _ := json.Marshal(s.statsBody())
		return b
	}); err != nil {
		log.Printf("stats service disabled: %v", err)
	}
}

// startCfgReloadSubscriber turns the broadcast channel into snapshot
// rebuilds: on gvos.cfg.reload every replica (including the sender —
// rebuilds are idempotent and cheap) reloads from PostgreSQL, which is
// the authoritative source. Memory-mode peers can only honor rate
// rebuilds from their own list; blocklist reloads require PG.
func startCfgReloadSubscriber(s *Server) {
	if err := s.events.Subscribe(events.SubjectCfgReload, func(data []byte) {
		var r struct {
			Entity string `json:"entity"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			log.Printf("CFG-RELOAD bad payload: %v", err)
			return
		}
		switch r.Entity {
		case "rate":
			s.rebuildRates()
			log.Printf("CFG-RELOAD rate snapshot rebuilt via bus")
		case "gateway":
			if err := s.gatewayApply(payloadOf(data)); err != nil {
				log.Printf("CFG-RELOAD gateway apply failed: %v", err)
			}
		case "blocklist":
			// Operation payload: peers apply the rule change directly
			// (reloading from PG here would revert memory-only state).
			var op struct {
				Op    string `json:"op"`
				Entry struct {
					TenantID string `json:"tenant_id"`
					Kind     string `json:"kind"`
					Scope    string `json:"scope"`
					Pattern  string `json:"pattern"`
					IsRegex  bool   `json:"is_regex"`
				} `json:"entry"`
			}
			if err := json.Unmarshal(data, &op); err != nil || op.Entry.Pattern == "" {
				log.Printf("CFG-RELOAD blocklist bad payload: %v", err)
				return
			}
			entry := blocklist.Entry{
				TenantID: op.Entry.TenantID, Kind: op.Entry.Kind, Scope: op.Entry.Scope,
				Pattern: op.Entry.Pattern, IsRegex: op.Entry.IsRegex,
			}
			if op.Op == "remove" {
				n := s.blocklist.Remove(op.Entry.TenantID, op.Entry.Kind, op.Entry.Scope, op.Entry.Pattern)
				log.Printf("CFG-RELOAD blocklist rule removed via bus (%d)", n)
			} else {
				s.blocklist.Add(entry)
				log.Printf("CFG-RELOAD blocklist rule added via bus (%s %s %s)", op.Entry.Kind, op.Entry.Scope, op.Entry.Pattern)
			}
		case "account":
			// Peer replicas learn the new account immediately (HA1 stays
			// in PG: the payload carries billing fields only) so caller
			// resolution does not fall back to the demo account.
			var op struct {
				Account struct {
					ID        string  `json:"id"`
					Username  string  `json:"username"`
					Name      string  `json:"name"`
					ParentID  string  `json:"parent_id"`
					RateGroup string  `json:"rate_group"`
					Postpaid  bool    `json:"postpaid"`
					Credit    float64 `json:"credit"`
				} `json:"account"`
			}
			if err := json.Unmarshal(data, &op); err != nil || op.Account.ID == "" {
				log.Printf("CFG-RELOAD account bad payload: %v", err)
				return
			}
			a := &account.Account{
				ID: op.Account.ID, Username: op.Account.Username, Name: op.Account.Name,
				ParentID: op.Account.ParentID, RateGroup: op.Account.RateGroup,
				Postpaid: op.Account.Postpaid, Credit: op.Account.Credit,
			}
			if err := s.store.Put(a); err != nil {
				log.Printf("CFG-RELOAD account apply failed (%s): %v", op.Account.ID, err)
				return
			}
			if s.userIndex != nil {
				s.userIndex[op.Account.Username] = op.Account.ID
			}
			log.Printf("CFG-RELOAD account applied via bus (%s %s)", op.Account.ID, op.Account.Username)
		case "credit":
			// Absolute balance (idempotent on replay), not the delta.
			var op struct {
				AccountID string  `json:"account_id"`
				Credit    float64 `json:"credit"`
			}
			if err := json.Unmarshal(data, &op); err != nil || op.AccountID == "" {
				log.Printf("CFG-RELOAD credit bad payload: %v", err)
				return
			}
			if a, err := s.store.Get(op.AccountID); err != nil {
				log.Printf("CFG-RELOAD credit skipped (%s unknown on this replica): %v", op.AccountID, err)
			} else {
				a.Credit = op.Credit
				_ = s.store.Put(a)
				log.Printf("CFG-RELOAD credit applied via bus (%s -> %.7f)", op.AccountID, op.Credit)
			}
		case "recording":
			var op struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.Unmarshal(data, &op); err != nil {
				log.Printf("CFG-RELOAD recording bad payload: %v", err)
				return
			}
			s.recordEnabled = op.Enabled
			log.Printf("CFG-RELOAD recording toggle applied via bus (enabled=%v)", op.Enabled)
		case "risk":
			// Full-rule-set replace, same semantics as the admin API.
			var op struct {
				Rules []riskctrl.Rule `json:"rules"`
			}
			if err := json.Unmarshal(data, &op); err != nil {
				log.Printf("CFG-RELOAD risk bad payload: %v", err)
				return
			}
			s.risk.SetRules(op.Rules)
			log.Printf("CFG-RELOAD risk rules applied via bus (%d rules)", len(op.Rules))
		case "ip_allowlist":
			// Full-set replace, same semantics as the admin API. The
			// Kamailio edge picks the change up via the snapshot
			// endpoint's version bump on its next poll.
			var op struct {
				Rules []ipallow.Rule `json:"rules"`
			}
			if err := json.Unmarshal(data, &op); err != nil {
				log.Printf("CFG-RELOAD ip_allowlist bad payload: %v", err)
				return
			}
			s.ipallow.Reload(op.Rules)
			log.Printf("CFG-RELOAD IP allowlist applied via bus (%d rules)", len(op.Rules))
		case "package":
			var op struct {
				Op        string          `json:"op"`
				Package   pkgplan.Package `json:"package"`
				Account   string          `json:"account"`
				PackageID string          `json:"package_id"`
			}
			if err := json.Unmarshal(data, &op); err != nil {
				log.Printf("CFG-RELOAD package bad payload: %v", err)
				return
			}
			switch op.Op {
			case "define":
				s.pkg.Define(op.Package)
				log.Printf("CFG-RELOAD package defined via bus (%s)", op.Package.ID)
			case "assign":
				if err := s.pkg.Assign(op.Account, op.PackageID); err != nil {
					// Define-then-assign ordering is preserved by the
					// publisher's queue; a missing definition means a
					// genuinely broken upstream sequence.
					log.Printf("CFG-RELOAD package assign failed (%s -> %s): %v", op.Account, op.PackageID, err)
					return
				}
				log.Printf("CFG-RELOAD package assigned via bus (%s -> %s)", op.Account, op.PackageID)
			default:
				log.Printf("CFG-RELOAD package unknown op %q", op.Op)
			}
		default:
			log.Printf("CFG-RELOAD unknown entity %q ignored", r.Entity)
		}
	}); err != nil {
		log.Printf("cfg-reload subscriber disabled: %v", err)
	}
}

// handleIPAllowSnapshot serves the allowlist to the Kamailio edge on
// GET /v1/internal/ipallow-snapshot. Auth is a lightweight shared
// secret (X-GVOS-Internal header, GVOS_INTERNAL_TOKEN env) — this is
// the machine-to-machine control plane, not the admin API; it must
// stay token-free of the bearer admin surface so the SIP edge can
// poll it with a plain http_connect. Version is a content hash: the
// poller skips the full body unless the version changed.
func (s *Server) handleIPAllowSnapshot(w http.ResponseWriter, r *http.Request) {
	want := os.Getenv("GVOS_INTERNAL_TOKEN")
	if want == "" {
		// No token configured: internal endpoint disabled rather than
		// open — the SIP edge falls back to its last-known ruleset.
		httpError(w, 503, "internal snapshot disabled (GVOS_INTERNAL_TOKEN unset)")
		return
	}
	if r.Header.Get("X-GVOS-Internal") != want {
		httpError(w, 401, "bad internal token")
		return
	}
	if r.Method != http.MethodGet {
		httpError(w, 405, "GET required")
		return
	}
	rules := s.ipallow.Rules()
	// Stable version: hash of the (id, cidr, action) set.
	h := fnv.New64a()
	// Pre-joined CIDR lists: the Kamailio poller feeds them straight
	// into is_in_subnet (its jansson path notation cannot index arrays
	// with dynamic variables, so the edge cannot rebuild the lists
	// itself — hand it the finished strings).
	var allows, denies string
	for _, ru := range rules {
		fmt.Fprintf(h, "%s|%s|%s\n", ru.ID, ru.CIDR, ru.Action)
		if ru.Action == ipallow.Deny {
			denies += ru.CIDR + ","
		} else {
			allows += ru.CIDR + ","
		}
	}
	writeJSON(w, 200, map[string]any{
		"version":    fmt.Sprintf("%016x", h.Sum64()),
		"count":      len(rules),
		"rules":      rules,
		"allow_cidrs": allows,
		"deny_cidrs":  denies,
	})
}
