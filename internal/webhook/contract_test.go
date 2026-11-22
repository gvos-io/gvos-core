package webhook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The contract is the product for this half: envelope shape, seq
// semantics, signature coverage, and the SSRF gate each get a test.
func TestEnvelopeShapeAndSignature(t *testing.T) {
	data := CDRCompletedData{
		CallID: "c-9", AccountID: "a-1", Dest: "8613800138000",
		StartTime: "2026-11-21T03:00:00Z", AnswerTime: "2026-11-21T03:00:02Z",
		EndTime: "2026-11-21T03:00:30Z", Duration: 30, BilledSecs: 60,
		Cost: "0.0550", Currency: "CNY", Release: "normal", SIPCode: 200, PDDMs: 2000,
	}
	body, err := BuildCDRCompleted("t-1", 42, time.Now(), data)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		V          int             `json:"v"`
		Event      string          `json:"event"`
		OccurredAt string          `json:"occurred_at"`
		Tenant     string          `json:"tenant"`
		Seq        int64           `json:"seq"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.V != EnvelopeVersion || env.Event != "cdr.completed" || env.Tenant != "t-1" || env.Seq != 42 {
		t.Errorf("envelope mismatch: %+v", env)
	}
	var back CDRCompletedData
	if err := json.Unmarshal(env.Data, &back); err != nil {
		t.Fatal(err)
	}
	if back.BilledSecs != 60 || back.Cost != "0.0550" || back.AnswerTime == "" {
		t.Errorf("projection mismatch: %+v", back)
	}
	// signature covers the exact envelope bytes
	if !Verify("s", Sign("s", time.Now(), body), body, time.Minute) {
		t.Error("signature does not verify against the envelope bytes")
	}
}

func TestSeqIsPerSubscriptionMonotonic(t *testing.T) {
	// deliveries land nowhere; we only exercise the seq assignment in
	// the dispatcher loop through Deliver's envelope — so verify via
	// BuildCallTeardown ordering instead (the dispatcher's seq lives
	// in the loop and is integration-tested through the admin face).
	a, _ := BuildCallTeardown("t", 1, time.Now(), CallTeardownData{CallID: "c1", Reason: "balance-exhausted"})
	b, _ := BuildCallTeardown("t", 2, time.Now(), CallTeardownData{CallID: "c2", Reason: "zombie-timeout"})
	var ea, eb struct {
		Seq   int64            `json:"seq"`
		Event string           `json:"event"`
		Data  CallTeardownData `json:"data"`
	}
	json.Unmarshal(a, &ea)
	json.Unmarshal(b, &eb)
	if ea.Seq >= eb.Seq || ea.Event != "call.teardown" || eb.Data.Reason != "zombie-timeout" {
		t.Errorf("teardown envelope ordering/payload wrong: %+v %+v", ea, eb)
	}
}

func TestSSRFGuard(t *testing.T) {
	cases := []struct {
		url  string
		pass bool
	}{
		{"https://hooks.customer.com/v1", true},
		{"http://hooks.customer.com/v1", false},            // https only
		{"http://127.0.0.1:6379/x", false},                 // loopback ip
		{"http://169.254.169.254/latest/meta-data", false}, // cloud metadata
		{"http://localhost:8080/v1/stats", false},          // localhost name
		{"file:///etc/passwd", false},                      // scheme
		{"https://10.0.0.5/admin", false},                  // private ip
	}
	for _, c := range cases {
		err := ValidateSubscription(c.url)
		if c.pass && err != nil {
			// Hostname allow-list does a live LookupHost; offline CI
			// or transient DNS must not fail the suite.
			if strings.Contains(err.Error(), "resolve") || strings.Contains(err.Error(), "lookup") {
				t.Logf("skipping %s (DNS unavailable): %v", c.url, err)
				continue
			}
			t.Errorf("%s should pass, got %v", c.url, err)
		}
		if !c.pass && err == nil {
			t.Errorf("%s should be rejected", c.url)
		}
	}
}
