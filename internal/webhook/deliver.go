// deliver.go: the delivery worker — NATS events fan out to active
// subscriptions; failures retry on the backoff schedule in-process
// (a restart re-drives from the delivery log's dead rows is left to
// the operator; at-least-once is the guarantee, PG logs the trail).
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
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
	if ip := hostIP(host); ip != nil {
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
		if ip := hostIP(a); ip != nil && isPrivateOrLoopback(ip) {
			return fmt.Errorf("host %q resolves to private %q", host, a)
		}
	}
	return nil
}

// isPrivateOrLoopback reports the addresses a webhook must never reach
// from inside the cluster: loopback, RFC1918 / fc00::/7, link-local
// (169.254.0.0/16 carries the cloud metadata service), carrier-grade
// NAT (100.64.0.0/10 — where Alibaba Cloud's metadata service lives),
// multicast, unspecified and broadcast.
func isPrivateOrLoopback(ip net.IP) bool {
	if ip == nil {
		return true
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return isForbiddenAddr(a.Unmap())
}

func isForbiddenAddr(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1]&0xc0 == 64 { // 100.64.0.0/10
			return true
		}
	}
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// hostIP parses a resolved address string into an IP for
// classification. netip is used first because it understands zoned
// IPv6 literals ("fe80::1%eth0"), which net.ParseIP rejects — a
// link-local address written that way used to slip past the check.
func hostIP(s string) net.IP {
	if a, err := netip.ParseAddr(s); err == nil {
		return net.IP(a.AsSlice())
	}
	return net.ParseIP(s)
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

// NewDeliverer wires a deliverer with sane timeouts and an SSRF-safe
// transport: https only, every redirect hop re-validated, and — the
// part the creation-time check cannot give — the address actually
// dialed is checked at dial time. A host that resolves public when the
// subscription is created and private when the delivery dials (DNS
// rebinding) is refused.
//
// Exception: with an egress proxy configured through the environment
// (HTTPS_PROXY / ALL_PROXY) the proxy resolves the subscriber's host
// itself, so the dial-time check would only end up judging the proxy;
// creation- and redirect-time validation still apply.
func NewDeliverer() *Deliverer {
	d := &Deliverer{}
	d.Client = &http.Client{
		Timeout:   15 * time.Second,
		Transport: newSSRFSafeTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			if err := validateWebhookURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
	return d
}

func newSSRFSafeTransport() http.RoundTripper {
	if egressProxyConfigured() {
		return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}
	}
	return &http.Transport{DialContext: safeDialContext, TLSHandshakeTimeout: 10 * time.Second}
}

func egressProxyConfigured() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// safeDialContext is the dial-time half of the SSRF guard: it resolves
// the host itself, refuses the connection when any resolved address is
// private, loopback or link-local, and dials the validated address —
// never the name, which would be resolved a second time.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("webhook dial %q: %w", addr, err)
	}
	var targets []string
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLoopback(ip) {
			return nil, fmt.Errorf("webhook address %q is private (dial-time check)", host)
		}
		targets = []string{addr}
	} else {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("webhook resolve %q: %w", host, err)
		}
		for _, a := range addrs {
			if isPrivateOrLoopback(a.IP) {
				return nil, fmt.Errorf("webhook host %q resolves to private %q (dial-time check)", host, a.IP)
			}
			targets = append(targets, net.JoinHostPort(a.IP.String(), port))
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("webhook host %q has no address", host)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var lastErr error
	for _, t := range targets {
		conn, err := dialer.DialContext(ctx, network, t)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("webhook dial %q: %w", addr, lastErr)
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
	mu     sync.RWMutex
	subs   []Subscription
	seq    map[string]int64 // per-subscription contract seq (Envelope.Seq)
	d      *Deliverer
	ctx    context.Context
	events chan eventJob
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
// drops — observability beats blocking the billing path). It is not
// scoped: every active subscriber of the subject sees it.
func (dsp *Dispatcher) Publish(subject string, payload any) {
	dsp.PublishScoped(subject, nil, payload)
}

// PublishScoped is Publish for an event that belongs to specific
// tenant identities (owners: the account it concerns and its
// ancestors). A tenant-owned subscription receives it only when one of
// its owners is the tenant the subscription is bound to; an empty,
// non-nil owners list reaches platform subscriptions alone. nil means
// unscoped.
func (dsp *Dispatcher) PublishScoped(subject string, owners []string, payload any) {
	select {
	case dsp.events <- eventJob{subject: subject, owners: owners, payload: payload}:
	default:
		log.Printf("webhook queue full, event %s dropped (observability tier)", subject)
	}
}

// visibleTo applies the scoping rule of PublishScoped.
func visibleTo(sub Subscription, owners []string) bool {
	if owners == nil {
		return true
	}
	if sub.Tenant == "" { // platform (operator-created) subscription
		return true
	}
	for _, o := range owners {
		if o == sub.Tenant {
			return true
		}
	}
	return false
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
			env, isEnv := job.payload.(envelopeJob)
			for i := range subs {
				sub := subs[i]
				if !visibleTo(sub, job.owners) {
					continue
				}
				want := job.subject
				if isEnv {
					want = "gvos." + env.Event
				}
				if !sub.Active || !WantsEvent(sub.Events, want) {
					continue
				}
				// Per-subscription monotonic seq (contract: gaps = missed
				// events; redeliveries repeat the same seq). Assigned here
				// under the loop so ordering follows event arrival.
				dsp.mu.Lock()
				dsp.seq[sub.ID]++
				seq := dsp.seq[sub.ID]
				dsp.mu.Unlock()
				if isEnv {
					go dsp.deliverEnvelope(sub, env.Event, envelopeTenant(env, sub), seq, env.Data)
				} else {
					go dsp.deliverLogged(sub, job.subject, seq, job.payload)
				}
			}
		}
	}
}

// envelopeTenant names the tenant in the contract envelope: the
// receiving subscription's own when it is tenant-owned, else the
// event's primary tenant.
func envelopeTenant(env envelopeJob, sub Subscription) string {
	if sub.Tenant != "" {
		return sub.Tenant
	}
	return env.Tenant
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
// name (cdr.completed / call.teardown), tenant the primary tenant, and
// data the typed projection struct. It is unscoped (every subscriber
// of the event sees it) — the API layer uses PublishEnvelopeScoped for
// customer-facing events. The envelope (v/seq/occurred_at) is built at
// delivery time per subscription. Delivery is funneled through the
// same buffered events channel as Publish so fan-out has backpressure
// instead of unbounded goroutines.
func (dsp *Dispatcher) PublishEnvelope(event, tenant string, data any) {
	dsp.PublishEnvelopeScoped(event, tenant, nil, data)
}

// PublishEnvelopeScoped is PublishEnvelope with tenant scoping (see
// PublishScoped): a tenant-owned subscription receives the event only
// when its tenant is among owners.
func (dsp *Dispatcher) PublishEnvelopeScoped(event, tenant string, owners []string, data any) {
	// Contract subscribers match on "gvos.<event>"; reuse the Publish
	// queue with a synthetic subject so loop applies the same caps.
	dsp.PublishScoped("gvos."+event, owners, envelopeJob{Event: event, Tenant: tenant, Data: data})
}

// eventJob is one queued fan-out. owners carries the scoping rule of
// PublishScoped. The loop recognizes envelopeJob payloads so contract
// events build the typed envelope instead of posting the raw payload.
type eventJob struct {
	subject string
	owners  []string
	payload any
}

type envelopeJob struct {
	Event  string
	Tenant string
	Data   any
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
