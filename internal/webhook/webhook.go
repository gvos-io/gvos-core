// Package webhook is the outbound event pipeline (api-roadmap Phase D,
// the half that ships: delivery to customer endpoints). NATS subjects
// fan out to subscribed HTTPS URLs with a Stripe-style HMAC signature
// (t=<unix>,v1=hex(hmac-sha256(secret, t + "." + body))), exponential
// backoff retries, and a dead-letter log table for operator triage.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// Sign produces the X-GVOS-Signature header value: t=<unix>,v1=<mac>.
// The MAC covers "<t>.<body>" — a replayed (t, body) pair fails
// verification on the consumer side.
func Sign(secret string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(at.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Verify is the consumer-side counterpart (used by the integration
// test and by customers implementing receivers).
func Verify(secret string, header string, body []byte, maxSkew time.Duration) bool {
	var t int64
	var sig string
	if _, err := fmt.Sscanf(header, "t=%d,v1=%s", &t, &sig); err != nil {
		return false
	}
	if time.Since(time.Unix(t, 0)) > maxSkew {
		return false // replay window exceeded
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), want)
}

// Backoff schedule (retry 0..4), then dead-letter.
var Backoff = []time.Duration{
	1 * time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour,
}

// NextRetry returns the delay after `attempts` failed attempts
// (1-based), or ok=false when the delivery should dead-letter (the
// whole schedule spent: attempt N+1 would exceed the table).
func NextRetry(attempts int) (time.Duration, bool) {
	if attempts < len(Backoff) {
		return Backoff[attempts-1], true
	}
	return 0, false
}

// WantsEvent reports whether a subscription listed the subject
// (subscriptions store subjects as a comma-joined TEXT[] in PG; here
// as a slice).
func WantsEvent(subscribed []string, subject string) bool {
	for _, s := range subscribed {
		if s == subject {
			return true
		}
	}
	return false
}
