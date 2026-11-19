// Package events is gv-core's NATS bus, covering three interaction
// styles on one transport:
//
//   - fire-and-forget events (Publish): CDRs, teardown notices, risk
//     alerts, admin change feed — strictly best-effort, a slow or
//     absent broker never blocks a billing answer (same contract as
//     CDR persistence: log loudly, carry on);
//   - broadcast (Subscribe): config-change fan-out so every gv-core
//     replica rebuilds its snapshot instantly;
//   - request/reply (Respond/RequestAsync): async service calls such
//     as teardown dispatch;
//   - work queue (NewDurableConsumer): JetStream durable pull
//     consumers with competing workers and at-least-once delivery —
//     the Webhook dispatcher runs here.
//
// Transport: JetStream stream GVOS (subjects gvos.>) when the server
// has JetStream enabled, core pub/sub otherwise. Subject scheme is
// gvos.<domain>.<event> so consumers can wildcard per domain.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Subjects published by gv-core. Payloads are plain JSON documents
// (CDR, teardown reason, audit diff) — no envelope: each payload is
// already self-describing and versioned through the stream itself.
const (
	SubjectCDRCompleted = "gvos.cdr.completed" // payload: callcore.CDR
	SubjectCDRRaw       = "gvos.cdr.raw"       // payload: callEndReq (Kamailio-measured; CDRR consumer finalizes billing)
	SubjectCallTeardown = "gvos.call.teardown" // payload: {call_id, reason}
	SubjectRiskAlert    = "gvos.risk.alert"    // payload: {account, username, rule_id, reason, to}
	SubjectRegEvent     = "gvos.reg.event"     // payload: {user, expires, src_ip, op} (Kamailio usrloc hooks)
	SubjectAdminChanged = "gvos.admin.changed" // payload: {action, entity, entity_id, before, after}
)

// Interaction subjects (beyond fire-and-forget events):
//
//   - gvos.cfg.reload is a broadcast (core pub/sub): every gv-core
//     replica applies the payload (rate rebuild, rule op, account
//     create, credit update, toggles) — instant propagation.
//   - gvos.call.teardown doubles as the teardown-executor queue: a
//     durable TEARDOWN consumer dispatches CANCEL/BYE (at-least-once,
//     persists while the executor is down).
//   - gvos.svc.* are request/reply service calls (call lifecycle
//     mirroring the HTTP face, stats snapshot).
const (
	SubjectCfgReload = "gvos.cfg.reload" // payload entity: rate|blocklist|account|credit|recording|risk|ip_allowlist|package|gateway
)

// Edge-alias subjects: the same events mirrored under dot-free names.
// The Kamailio nats module's event_route lexer token
// ([a-zA-Z][0-9a-zA-Z-]*(":"[...])+) cannot express dotted subjects,
// so the signaling edge subscribes to these aliases instead. EmitEdge
// (used by emitEdge below) dual-publishes so both audiences see one
// message; the aliases stay outside streamSubjects (the dotted
// originals are the persisted, canonical stream records).
const (
	SubjectEdgeCfgReload = "gvos-edge-cfg"   // alias of gvos.cfg.reload
	SubjectEdgeRiskAlert = "gvos-edge-risk"  // alias of gvos.risk.alert
	SubjectEdgeTeardown  = "gvos-edge-tear"  // alias of gvos.call.teardown
)

// Service request/reply subjects (outside the stream domains — see
// streamSubjects): the call lifecycle mirrors the HTTP face and stats
// serves the ops snapshot.
const (
	SubjectCallStart  = "gvos.svc.call.start"  // req: callStartReq  rep: {code, body}
	SubjectCallAnswer = "gvos.svc.call.answer" // req: {call_id}     rep: {code, body}
	SubjectCallEnd    = "gvos.svc.call.end"    // req: callEndReq    rep: {code, body}
	SubjectStats      = "gvos.svc.stats"       // req: (ignored)     rep: /v1/stats map
)

const (
	streamName    = "GVOS"
	queueSize     = 1024 // burst buffer; drops beyond this, never stalls
	flushTimeout  = 5 * time.Second
	publishBudget = 5 * time.Second // per-message JetStream ack budget
	fetchTimeout  = 1 * time.Second // idle poll so workers notice Stop
	retryDelay    = 1 * time.Second // NAK redelivery delay (at-least-once)
	maxDeliveries = 5               // poison-pill cap before JS stops redelivering
)

// streamSubjects are the domains the GVOS stream persists. The service
// domain (gvos.svc.>) is deliberately EXCLUDED: a request carrying a
// reply subject that lands in a stream gets the server's PubAck on the
// inbox, racing (and usually beating) the responder's actual answer.
// Events and broadcasts are persisted; requests stay pure core NATS.
var streamSubjects = []string{
	"gvos.cdr.>",
	"gvos.call.>",
	"gvos.risk.>",
	"gvos.admin.>",
	"gvos.cfg.>",
	"gvos.reg.>",
}

// Poster is the transport seam (second interface in the repo, after
// mediastream.Subscriber): real wiring uses NATS, tests inject a stub.
type Poster interface {
	Post(subject string, data []byte) error
	Close() error
}

// queued is one in-flight event between Publish and the pump goroutine.
type queued struct {
	subject string
	data    []byte
}

// Publisher serializes events onto NATS through a bounded in-memory
// queue. All failures are logged, never propagated: the billing path
// treats the bus as observability, not authority.
//
// nc/js carry the interaction mechanisms (broadcast Subscribe, req/
// reply, durable pull consumers); both are nil for stub transports,
// where those features silently no-op.
type Publisher struct {
	mu     sync.Mutex
	closed bool
	ch     chan queued
	done   chan struct{}

	poster Poster
	nc     *nats.Conn            // nil for stub transports
	js     nats.JetStreamContext // nil in core mode / stubs
	mode   string                // "jetstream" | "core" (surface in /v1/stats)
}

// New connects to NATS and returns a publisher. Connection is
// resilient: if the broker is down at boot the client keeps retrying
// (RetryOnFailedConnect + MaxReconnects(-1)), so the control plane
// starts even before NATS does — publishes just log failures until
// the broker appears. JetStream is preferred but optional.
func New(url string) (*Publisher, error) {
	nc, err := nats.Connect(url,
		nats.Name("gvos-core"),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, err
	}
	var (
		poster Poster = &corePoster{nc: nc}
		mode          = "core"
		js     nats.JetStreamContext
	)
	if jsx, err := nc.JetStream(); err == nil {
		if err := ensureStream(jsx); err == nil {
			poster = &jsPoster{js: jsx}
			mode = "jetstream"
			js = jsx
		} else {
			log.Printf("NATS JetStream stream unavailable, using core pub/sub: %v", err)
		}
	} else {
		log.Printf("NATS JetStream unavailable, using core pub/sub: %v", err)
	}
	log.Printf("NATS event bus ready mode=%s url=%s", mode, url)
	p := &Publisher{
		ch:     make(chan queued, queueSize),
		done:   make(chan struct{}),
		poster: poster,
		nc:     nc,
		js:     js,
		mode:   mode,
	}
	go p.pump()
	return p, nil
}

// NewWithPoster builds a publisher on a custom transport (test seam).
// Interaction mechanisms (Subscribe/Respond/RequestAsync/consumers)
// are unavailable here: nc/js are nil and the methods no-op.
func NewWithPoster(p Poster) *Publisher {
	pub := &Publisher{
		ch:     make(chan queued, queueSize),
		done:   make(chan struct{}),
		poster: p,
		mode:   "core",
	}
	go pub.pump()
	return pub
}

// ensureStream creates the GVOS stream idempotently and converges its
// subject list: builds before the svc-exclusion created gvos.> (which
// swallowed req/reply subjects), so an existing stream whose subjects
// differ is updated in place. Already-exists counts as success so
// restarts and replicas converge.
func ensureStream(js nats.JetStreamContext) error {
	info, err := js.StreamInfo(streamName)
	if err == nil {
		if !sameSubjects(info.Config.Subjects, streamSubjects) {
			cfg := info.Config
			cfg.Subjects = streamSubjects
			_, err = js.UpdateStream(&cfg)
			if err == nil {
				log.Printf("NATS stream %s subjects converged to %v", streamName, streamSubjects)
			}
			return err
		}
		return nil
	} else if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: streamSubjects,
		Storage:  nats.FileStorage,
	})
	// Already-exists counts as success: restarts and replicas converge.
	if err != nil && (errors.Is(err, nats.ErrStreamNameAlreadyInUse) ||
		strings.Contains(err.Error(), "already in use")) {
		return nil
	}
	return err
}

// sameSubjects compares subject sets order-independently.
func sameSubjects(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(a))
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}

// Publish enqueues payload as JSON on subject. Non-blocking: a full
// queue drops the event loudly (media-pump semantics) instead of
// stalling the billing answer.
func (p *Publisher) Publish(subject string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("NATS marshal failed (%s): %v", subject, err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.ch <- queued{subject: subject, data: data}:
	default:
		log.Printf("NATS queue full, dropped event %s", subject)
	}
}

// pump drains the queue onto the transport. One goroutine per
// publisher: ordering per subject is preserved, slow JetStream acks
// are absorbed by the queue, not by callers.
func (p *Publisher) pump() {
	defer close(p.done)
	for q := range p.ch {
		if err := p.poster.Post(q.subject, q.data); err != nil {
			log.Printf("NATS publish failed (%s): %v", q.subject, err)
		}
	}
}

// Mode reports the active transport for /v1/stats.
func (p *Publisher) Mode() string { return p.mode }

// Close flushes the queue (bounded by flushTimeout) and releases the
// transport. Publish after Close is a silent no-op, so shutdown order
// needs no coordination.
func (p *Publisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.ch)
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(flushTimeout):
		log.Printf("NATS flush timeout on close; %d events may be lost", len(p.ch))
	}
	if p.nc != nil {
		p.nc.Close()
	}
}

// corePoster publishes on the plain NATS connection (client-side
// buffering; delivery is at-most-once — acceptable for events that
// also land in PostgreSQL).
type corePoster struct{ nc *nats.Conn }

func (p *corePoster) Post(subject string, data []byte) error {
	return p.nc.Publish(subject, data)
}

func (p *corePoster) Close() error { return nil }

// jsPoster publishes with a JetStream ack (at-least-once), bounded by
// publishBudget so a stalled broker degrades to drop-on-timeout.
type jsPoster struct{ js nats.JetStreamContext }

func (p *jsPoster) Post(subject string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), publishBudget)
	defer cancel()
	_, err := p.js.PublishMsg(&nats.Msg{Subject: subject, Data: data}, nats.Context(ctx))
	return err
}

func (p *jsPoster) Close() error { return nil }

// --- interaction mechanisms (broadcast / request-reply / MQ) ---
// All require a real connection (nc != nil); on stub transports they
// log and no-op so callers never branch on transport availability.

// Subscribe receives broadcasts on subject (core pub/sub fan-out:
// every subscriber gets its own copy). The handler runs on the NATS
// dispatch goroutine — keep it quick or hand off internally.
func (p *Publisher) Subscribe(subject string, fn func(data []byte)) error {
	if p.nc == nil {
		return errors.New("nats: subscribe requires a live connection")
	}
	_, err := p.nc.Subscribe(subject, func(m *nats.Msg) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("NATS handler panic (%s): %v", subject, r)
			}
		}()
		fn(m.Data)
	})
	return err
}

// Respond serves request/reply on subject: fn turns a request payload
// into the reply bytes; the reply goes back over the requester's
// _INBOX. Used by service seams (teardown dispatch today).
func (p *Publisher) Respond(subject string, fn func(req []byte) []byte) error {
	if p.nc == nil {
		return errors.New("nats: respond requires a live connection")
	}
	_, err := p.nc.Subscribe(subject, func(m *nats.Msg) {
		var reply []byte
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("NATS responder panic (%s): %v", subject, r)
					reply = []byte(`{"error":"internal panic"}`)
				}
			}()
			reply = fn(m.Data)
		}()
		if m.Reply != "" && reply != nil {
			if err := m.Respond(reply); err != nil {
				log.Printf("NATS reply failed (%s): %v", subject, err)
			}
		}
	})
	return err
}

// RequestAsync sends a request and delivers the first reply (or a
// timeout error) to onReply on its own goroutine — the caller keeps
// running without waiting. Request subjects must live outside the
// stream domains (gvos.svc.*): a stream-covered request would collect
// the server's PubAck on the inbox instead of the responder's answer.
// Returns false when no transport is available (stub mode): callers
// treat that as "dispatch skipped".
func (p *Publisher) RequestAsync(subject string, payload any, timeout time.Duration, onReply func(reply []byte, err error)) bool {
	if p.nc == nil {
		return false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("NATS request marshal failed (%s): %v", subject, err)
		return false
	}
	inbox := nats.NewInbox()
	ch := make(chan *nats.Msg, 1)
	sub, err := p.nc.ChanSubscribe(inbox, ch)
	if err != nil {
		log.Printf("NATS request subscribe failed (%s): %v", subject, err)
		return false
	}
	if err := p.nc.PublishRequest(subject, inbox, data); err != nil {
		_ = sub.Unsubscribe()
		log.Printf("NATS request publish failed (%s): %v", subject, err)
		return false
	}
	go func() {
		defer sub.Unsubscribe()
		select {
		case m := <-ch:
			onReply(m.Data, nil)
		case <-time.After(timeout):
			onReply(nil, errors.New("nats request timeout after "+timeout.String()))
		}
	}()
	return true
}

// Handler processes one consumed message. Returning nil acks (the
// work is done); returning an error NAKs with retryDelay — JetStream
// redelivers to this or a competing consumer (at-least-once MQ).
type Handler func(data []byte) error

// puller is the consumer transport seam: real wiring wraps the
// JetStream pull subscription (Fetch protocol — pull subscriptions
// do not support NextMsg), tests feed deterministic messages.
type puller interface {
	Fetch(batch int, opts ...nats.PullOpt) ([]*nats.Msg, error)
}

// Consumer drains a JetStream durable pull consumer with N competing
// workers. The durable name is the queue identity: replicas sharing
// it load-balance messages (work-queue semantics) while each keeps
// its own delivery position across restarts.
type Consumer struct {
	name    string
	puller  puller
	handler Handler
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewDurableConsumer creates a durable pull consumer on filter and
// starts workers. MaxDeliver caps redelivery of poison messages (the
// stream keeps them for operations; delivery just stops).
func (p *Publisher) NewDurableConsumer(name, filter string, handler Handler, workers int) (*Consumer, error) {
	if p.js == nil {
		return nil, errors.New("nats: durable consumer requires JetStream mode")
	}
	sub, err := p.js.PullSubscribe(filter, name, nats.MaxDeliver(maxDeliveries))
	if err != nil {
		return nil, err
	}
	c := &Consumer{
		name:    name,
		puller:  sub,
		handler: handler,
		done:    make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		c.wg.Add(1)
		go c.work()
	}
	log.Printf("NATS consumer %s on %s workers=%d", name, filter, workers)
	return c, nil
}

// work pulls messages until Stop. Idle polls use fetchTimeout so the
// worker notices shutdown within a second; a lost pull (broker
// restart) logs and retries rather than killing the worker. Batch of
// one: per-message ack/nak decisions stay exact regardless of batch
// semantics.
func (c *Consumer) work() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		default:
		}
		msgs, err := c.puller.Fetch(1, nats.MaxWait(fetchTimeout))
		switch {
		case err == nil:
			for _, msg := range msgs {
				if herr := c.handler(msg.Data); herr != nil {
					log.Printf("NATS consumer %s handler failed (redelivery in %s): %v", c.name, retryDelay, herr)
					if nakErr := msg.NakWithDelay(retryDelay); !ackNoise(nakErr) {
						log.Printf("NATS consumer %s nak failed: %v", c.name, nakErr)
					}
					continue
				}
				if ackErr := msg.Ack(); !ackNoise(ackErr) {
					log.Printf("NATS consumer %s ack failed: %v", c.name, ackErr)
				}
			}
		case errors.Is(err, nats.ErrTimeout):
			// Idle poll: loop back and re-check done.
		default:
			select {
			case <-c.done:
				return
			case <-time.After(retryDelay):
			}
			log.Printf("NATS consumer %s pull failed, retrying: %v", c.name, err)
		}
	}
}

// Stop waits (bounded) for in-flight handlers to finish and the
// workers to exit. Unacked in-flight messages are redelivered by
// JetStream after AckWait, so a stop mid-processing stays correct.
func (c *Consumer) Stop() {
	close(c.done)
	c.wg.Wait()
}

// ackNoise reports ack/nak errors that only occur on messages not
// bound to a JetStream subscription (stub pullers in tests): they
// never happen on the real transport, so they are not log-worthy.
func ackNoise(err error) bool {
	return err == nil || errors.Is(err, nats.ErrMsgNotBound) || errors.Is(err, nats.ErrMsgNoReply)
}
