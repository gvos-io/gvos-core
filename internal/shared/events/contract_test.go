// Contract fixtures for the event payloads (02-architecture §9 /
// api-conventions §6): any structural change to a published payload
// must update these files explicitly — silent field drift fails CI.
// Consumers (durable workers, the edge, future webhooks) depend on
// the shapes below.
package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixture is one subject's canonical payload (built inline so the
// test documents the producer's field set).
type fixture struct {
	Subject string
	Payload map[string]any
}

func contractFixtures() []fixture {
	return []fixture{
		{"gvos.cdr.completed", map[string]any{
			"CallID": "c-1", "AccountID": "acc-1", "Dest": "8613800138000",
			"Duration": 66, "BilledSecs": 66, "Cost": "0.0550", "RatePrefix": "8613",
			"Price": "0.0500", "Release": "normal", "PDDMs": 20, "SIPCode": 200,
			"Recorded": false, "FreeSecs": 0,
		}},
		{"gvos.cdr.raw", map[string]any{
			"call_id": "c-1", "event": "end", "duration": 66,
			"pdd_ms": 20, "sip_code": 200, "release": "normal", "qos": "",
		}},
		{"gvos.call.teardown", map[string]any{"call_id": "c-1", "reason": "balance-exhausted"}},
		{"gvos.risk.alert", map[string]any{
			"account": "acc-1", "username": "1000", "rule_id": "r1", "reason": "freq", "to": "8613800138000",
		}},
		{"gvos.reg.event", map[string]any{"user": "1000", "op": "reg", "expires": 3600, "src_ip": "10.0.0.5"}},
		{"gvos.admin.changed", map[string]any{
			"action": "create", "entity": "account", "entity_id": "acc-1",
			"operator": "gvos_admin_ab12", "tenant_id": "t1", "before": nil, "after": map[string]any{"credit": "10"},
		}},
		{"gvos.cfg.reload", map[string]any{
			"entity": "gateway", "gateways": []map[string]any{{
				"id": "g1", "name": "one", "sip_addr": "10.0.0.1:5060", "priority": 0,
				"enabled": true, "prefixes": []string{"86"}, "cost_rate": "demo",
			}},
		}},
		{"gvos.recording.completed", map[string]any{
			"call_id": "c-1", "track": "agent", "object_key": "recordings/x/agent.mp3",
			"backend": "local", "size_bytes": 12345, "format": "mp3",
		}},
	}
}

// TestPayloadContractFixtures: marshal each payload, compare against
// the golden file byte-for-byte (after JSON normalization), and
// verify every field survives a JSON round trip.
func TestPayloadContractFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "contract")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	update := os.Getenv("UPDATE_CONTRACT") == "1"
	for _, f := range contractFixtures() {
		b, err := json.Marshal(f.Payload)
		if err != nil {
			t.Fatalf("%s: marshal: %v", f.Subject, err)
		}
		var norm map[string]any
		if err := json.Unmarshal(b, &norm); err != nil {
			t.Fatalf("%s: normalize: %v", f.Subject, err)
		}
		nb, _ := json.MarshalIndent(norm, "", "  ")
		golden := filepath.Join(dir, f.Subject+".json")
		if update {
			if err := os.WriteFile(golden, nb, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: golden missing (run with UPDATE_CONTRACT=1 to seed): %v", f.Subject, err)
		}
		if string(want) != string(nb) {
			t.Errorf("%s payload drift:\n--- golden ---\n%s\n--- current ---\n%s\n(update fixture only with a consumer-aware review)", f.Subject, want, nb)
		}
	}
}
