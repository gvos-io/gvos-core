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
	"log"
	"net/http"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/blocklist"
	"github.com/gvos/gvos-core/internal/callstate"
	"github.com/gvos/gvos-core/internal/events"
	"github.com/gvos/gvos-core/internal/pkgplan"
	"github.com/gvos/gvos-core/internal/riskctrl"
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

// dispatchTeardown is the teardown executor seam: sends CANCEL/BYE
// via Kamailio in production; the spike logs. Invoked by the TEARDOWN
// worker (bus on) or directly from the billing callback (no bus).
func dispatchTeardown(callID, reason string) {
	// Production executor slot: send CANCEL/BYE via Kamailio here.
	log.Printf("TEARDOWN dispatch call=%s reason=%s", callID, reason)
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
