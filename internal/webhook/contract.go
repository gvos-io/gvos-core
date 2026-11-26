// Contract envelope + payload projections for the customer-facing
// webhook API (api-roadmap Phase D). Rules of the contract:
//
//   - Every payload is wrapped in an Envelope: {"v":1, "event":...,
//     "occurred_at":..., "tenant":..., "seq":N, "data":{...}}.
//   - "v" is the ENVELOPE version. Within v1: fields are additive
//     only — new fields may appear, existing fields never change
//     type or disappear, enum values never gain new members without
//     a v2. A breaking change ships as v2 with both versions served
//     for at least six months (api-conventions.md §Webhook).
//   - "seq" is monotonic per subscription (gaps = missed events, the
//     customer's signal to reconcile via the CDR API). Redeliveries
//     carry the SAME seq — consumers must be idempotent on
//     (tenant, event, seq).
//   - "data" is a stable PROJECTION of internal state, never the
//     internal struct itself: internal refactors must not break
//     customers.
//
// Delivery rides the Stripe-style HMAC signature (Sign in
// webhook.go): X-GVOS-Signature: t=<unix>,v1=<hex(hmac(secret, t.body))>.
package webhook

import (
	"encoding/json"
	"time"
)

// EnvelopeVersion is the current payload contract version.
const EnvelopeVersion = 1

// Event names customers may subscribe to. Closed enum: adding a
// member requires a contract revision (v1 rules still allow it as
// additive — new event types are new keys, not changes to existing
// payloads — but they are announced in the changelog).
const (
	EventCDRCompleted = "cdr.completed"
	EventCallTeardown = "call.teardown"
)

// Envelope is the top-level webhook body. JSON field order is
// significant to nobody; the signature covers the exact bytes.
type Envelope struct {
	V          int             `json:"v"`
	Event      string          `json:"event"`
	OccurredAt string          `json:"occurred_at"` // RFC3339, UTC
	Tenant     string          `json:"tenant"`
	Seq        int64           `json:"seq"`
	Data       json.RawMessage `json:"data"`
}

// CDRCompletedData is the stable projection of a finished call —
// the fields a billing/CRM integration needs, none of the internals.
// Never rename these JSON keys; additions only (v1 rules).
type CDRCompletedData struct {
	CallID     string `json:"call_id"`
	AccountID  string `json:"account_id"`
	Dest       string `json:"dest"`
	StartTime  string `json:"start_time"`  // RFC3339 UTC
	AnswerTime string `json:"answer_time"` // RFC3339 UTC, "" when never answered
	EndTime    string `json:"end_time"`
	Duration   int    `json:"duration"` // connected seconds
	BilledSecs int    `json:"billed_secs"`
	Cost       string `json:"cost"` // decimal string, account currency
	Currency   string `json:"currency"`
	Release    string `json:"release"` // normal | balance-exhausted | zombie-timeout | unratable
	SIPCode    int    `json:"sip_code"`
	PDDMs      int    `json:"pdd_ms"`
	Recorded   bool   `json:"recorded"`
}

// CallTeardownData is the projection of a forced teardown (balance
// exhaustion / zombie sweep): the billing event a fraud desk needs.
type CallTeardownData struct {
	CallID string `json:"call_id"`
	Reason string `json:"reason"` // balance-exhausted | zombie-timeout | admin
}

// BuildCDRCompleted wraps one CDR row into the contract envelope.
// RFC3339 times, decimal-string cost (the money.Amount JSON form),
// empty AnswerTime when the call was never answered.
func BuildCDRCompleted(tenant string, seq int64, at time.Time, d CDRCompletedData) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	env := Envelope{
		V:          EnvelopeVersion,
		Event:      EventCDRCompleted,
		OccurredAt: at.UTC().Format(time.RFC3339Nano),
		Tenant:     tenant,
		Seq:        seq,
		Data:       raw,
	}
	return json.Marshal(env)
}

// BuildCallTeardown wraps one forced teardown into the contract envelope.
func BuildCallTeardown(tenant string, seq int64, at time.Time, d CallTeardownData) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	env := Envelope{
		V:          EnvelopeVersion,
		Event:      EventCallTeardown,
		OccurredAt: at.UTC().Format(time.RFC3339Nano),
		Tenant:     tenant,
		Seq:        seq,
		Data:       raw,
	}
	return json.Marshal(env)
}
