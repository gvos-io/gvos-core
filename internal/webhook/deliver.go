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
	"net/http"
	"sync"
	"time"
)

// Subscription is one customer endpoint (PG-backed in the api wiring;
// this face is the worker's view).
type Subscription struct {
	ID     string
	URL    string
	Secret string
	Events []string
	Active bool
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
// schedule. Returns the final attempt count (5 = dead-lettered).
func (d *Deliverer) Deliver(ctx context.Context, sub Subscription, subject string, payload any) int {
	body, err := json.Marshal(payload)
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
	dsp := &Dispatcher{d: d, ctx: ctx, events: make(chan eventJob, 256)}
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
			for _, sub := range subs {
				if !sub.Active || !WantsEvent(sub.Events, job.subject) {
					continue
				}
				go dsp.deliverLogged(sub, job.subject, job.payload)
			}
		}
	}
}

// deliverLogged runs the retry schedule and logs the outcome (the
// PG delivery-log write is the api wiring's PG seam).
func (dsp *Dispatcher) deliverLogged(sub Subscription, subject string, payload any) {
	attempts := dsp.d.Deliver(dsp.ctx, sub, subject, payload)
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
