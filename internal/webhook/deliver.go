// deliver.go: the delivery worker — NATS events fan out to active
// subscriptions; failures retry on the backoff schedule in-process
// (a restart re-drives from the delivery log's dead rows is left to
// the operator; at-least-once is the guarantee, PG logs the trail).
package webhook

import (
	"bytes"
	"fmt"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Subscription is one customer endpoint (PG-backed in the api wiring;
// this face is the worker's view). Tenant rides the contract envelope.
type Subscription struct {
	ID     string
	URL    string
	Secret string
	Events []string
	Active bool
	Tenant string
}

// SSRF guard: subscription URLs are customer input, and the deliverer
// POSTs to them from INSIDE the cluster — an unrestricted URL turns
// every webhook into an internal-network prober (the classic
// webhook-as-SSRF hole, aimed at 169.254.169.254 metadata or
// localhost:6379). Rules: https only, no literal-IP hosts in private
// or loopback ranges, no localhost names, no names resolving to
// private addresses. DNS rebinding between this check and the dial is
// the documented residual, covered by the egress policy note in the
// operations manual.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("unparseable url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("host %q not allowed", lower)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLoopback(ip) {
			return fmt.Errorf("ip %q not allowed (private/loopback)", host)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && isPrivateOrLoopback(ip) {
			return fmt.Errorf("host %q resolves to private %q", host, a)
		}
	}
	return nil
}

func isPrivateOrLoopback(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified() || ip.Equal(net.IPv4bcast)
}

// ValidateSubscription is the admin-plane gate: create/update paths
// call this before persisting (see admin_webhooks.go).
func ValidateSubscription(raw string) error {
	return validateWebhookURL(raw)
}

// Deliverer posts signed payloads to subscriptions.
type Deliverer struct {
	Client *http.Client
}

// NewDeliverer wires a deliverer with sane timeouts.
func NewDeliverer() *Deliverer {
	return &Deliverer{Client: &http.Client{Timeout: 15 * time.Second}}
}

// DeliverResult is one delivery attempt's outcome.
type DeliverResult struct {
	OK      bool
	Status  int
	Err     string
	Attempt int
}

// deliverOnce posts one signed payload (no retries).
func (d *Deliverer) deliverOnce(ctx context.Context, sub Subscription, subject string, body []byte) DeliverResult {
	req, err := http.NewRequestWithContext(ctx, "POST", sub.URL, bytes.NewReader(body))
	if err != nil {
		return DeliverResult{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GVOS-Event", subject)
	req.Header.Set("X-GVOS-Version", strconv.Itoa(EnvelopeVersion))
	req.Header.Set("X-GVOS-Signature", Sign(sub.Secret, time.Now(), body))
	resp, err := d.Client.Do(req)
	if err != nil {
		return DeliverResult{Err: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return DeliverResult{OK: true, Status: resp.StatusCode}
	}
	return DeliverResult{Status: resp.StatusCode, Err: fmt.Sprintf("endpoint returned %d", resp.StatusCode)}
}

// Deliver drives one (subscription, event) with the full retry
// schedule. payload is the CONTRACT data (not the envelope — the
// envelope is built here so seq/timestamp are assigned at delivery
// time, not publish time). Returns the final attempt count
// (5 = dead-lettered).
func (d *Deliverer) Deliver(ctx context.Context, sub Subscription, subject string, seq int64, payload any) int {
	at := time.Now()
	envelope := Envelope{
		V:          EnvelopeVersion,
		Event:      subject,
		OccurredAt: at.UTC().Format(time.RFC3339Nano),
		Tenant:     sub.Tenant,
		Seq:        seq,
	}
	if subject == EventCDRCompleted {
		if d0, err := json.Marshal(payload); err == nil {
			envelope.Data = json.RawMessage(d0)
		}
	} else {
		// Non-contract events (e.g. internal probes) pass through as
		// raw data; contract events get their typed projections.
		if d0, err := json.Marshal(payload); err == nil {
			envelope.Data = json.RawMessage(d0)
		}
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("webhook %s marshal: %v", sub.URL, err)
		return 0
	}
	for attempt := 1; attempt <= len(Backoff); attempt++ {
		res := d.deliverOnce(ctx, sub, subject, body)
		if res.OK {
			return attempt
		}
		delay, more := NextRetry(attempt)
		if !more {
			break
		}
		log.Printf("webhook %s attempt %d failed (%s); retry in %v", sub.URL, attempt, res.Err, delay)
		select {
		case <-ctx.Done():
			return attempt
		case <-time.After(delay):
		}
	}
	return len(Backoff) // dead-lettered
}

// Dispatcher holds the active subscription set and fans out events.
type Dispatcher struct {
	mu    sync.RWMutex
	subs  []Subscription
	seq   map[string]int64 // per-subscription contract seq (Envelope.Seq)
	d     *Deliverer
	ctx   context.Context
	events chan eventJob
}

type eventJob struct {
	subject string
	payload any
}

// NewDispatcher wires the fan-out loop (buffered jobs; delivery runs
// in its own goroutine per event so the NATS consumer never blocks).
func NewDispatcher(ctx context.Context, d *Deliverer) *Dispatcher {
	dsp := &Dispatcher{d: d, ctx: ctx, seq: map[string]int64{}, events: make(chan eventJob, 256)}
	go dsp.loop()
	return dsp
}

// SetSubscriptions atomically replaces the active set (admin CRUD
// path refreshes this).
func (dsp *Dispatcher) SetSubscriptions(subs []Subscription) {
	dsp.mu.Lock()
	dsp.subs = subs
	dsp.mu.Unlock()
}

// Publish queues one event for fan-out (non-blocking; a full queue
// drops — observability beats blocking the billing path).
func (dsp *Dispatcher) Publish(subject string, payload any) {
	select {
	case dsp.events <- eventJob{subject, payload}:
	default:
		log.Printf("webhook queue full, event %s dropped (observability tier)", subject)
	}
}

func (dsp *Dispatcher) loop() {
	for {
		select {
		case <-dsp.ctx.Done():
			return
		case job := <-dsp.events:
			dsp.mu.RLock()
			subs := dsp.subs
			dsp.mu.RUnlock()
			for i := range subs {
				sub := subs[i]
				if !sub.Active || !WantsEvent(sub.Events, job.subject) {
					continue
				}
				// Per-subscription monotonic seq (contract: gaps = missed
				// events; redeliveries repeat the same seq). Assigned here
				// under the loop so ordering follows event arrival.
				dsp.mu.Lock()
				dsp.seq[sub.ID]++
				seq := dsp.seq[sub.ID]
				dsp.mu.Unlock()
				go dsp.deliverLogged(sub, job.subject, seq, job.payload)
			}
		}
	}
}

// deliverLogged runs the retry schedule and logs the outcome (the
// PG delivery-log write is the api wiring's PG seam).
func (dsp *Dispatcher) deliverLogged(sub Subscription, subject string, seq int64, payload any) {
	attempts := dsp.d.Deliver(dsp.ctx, sub, subject, seq, payload)
	if attempts >= len(Backoff) {
		log.Printf("webhook %s DEAD-LETTERED after %d attempts (%s)", sub.URL, attempts, subject)
	}
}

// SubscriptionCount reports the active set size (health/stats).
func (dsp *Dispatcher) SubscriptionCount() []Subscription {
	dsp.mu.RLock()
	defer dsp.mu.RUnlock()
	return dsp.subs
}

// PublishEnvelope queues one CONTRACT event: event is the contract
// name (cdr.completed / call.teardown), tenant the owning tenant, and
// data the typed projection struct. The envelope (v/seq/occurred_at)
// is built at delivery time per subscription.
func (dsp *Dispatcher) PublishEnvelope(event, tenant string, data any) {
	dsp.mu.RLock()
	subs := dsp.subs
	dsp.mu.RUnlock()
	for i := range subs {
		sub := subs[i]
		if !sub.Active || !WantsEvent(sub.Events, "gvos."+event) {
			continue
		}
		dsp.mu.Lock()
		dsp.seq[sub.ID]++
		seq := dsp.seq[sub.ID]
		dsp.mu.Unlock()
		go dsp.deliverEnvelope(sub, event, tenant, seq, data)
	}
}

// deliverEnvelope builds the contract envelope with the subscription's
// secret and drives the retry schedule.
func (dsp *Dispatcher) deliverEnvelope(sub Subscription, event, tenant string, seq int64, data any) {
	at := time.Now()
	env := Envelope{V: EnvelopeVersion, Event: event,
		OccurredAt: at.UTC().Format(time.RFC3339Nano), Tenant: tenant, Seq: seq}
	if raw, err := json.Marshal(data); err == nil {
		env.Data = raw
	}
	body, err := json.Marshal(env)
	if err != nil {
		log.Printf("webhook %s envelope marshal: %v", sub.URL, err)
		return
	}
	for attempt := 1; attempt <= len(Backoff); attempt++ {
		res := dsp.d.deliverOnce(dsp.ctx, sub, event, body)
		if res.OK {
			return
		}
		delay, more := NextRetry(attempt)
		if !more {
			log.Printf("webhook %s DEAD-LETTERED after %d attempts (%s)", sub.URL, attempt, event)
			return
		}
		select {
		case <-dsp.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}