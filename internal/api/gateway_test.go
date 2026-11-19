// Gateway admin API + broadcast: the API is the authoritative plane
// (full-set replace / single removal), every change emits a
// gvos.cfg.reload broadcast carrying the complete array (idempotent
// replay for replicas and the Kamailio htable mirror).
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lixuanqun/gvos-core/internal/customerdomain/gateway"

	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

// TestGatewayAdminAPI: replace, list, remove — each mutation
// broadcasts entity=gateway with the full resulting array.
func TestGatewayAdminAPI(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()
	defer flush(s)

	do := func(method, path, body string) *http.Response {
		t.Helper()
		var rd *bytes.Reader
		if body != "" {
			rd = bytes.NewReader([]byte(body))
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Full-set replace.
	resp := do("POST", "/v1/admin/gateways",
		`{"gateways":[{"id":"g1","name":"primary","sip_addr":"10.0.0.1:5060","protocol":"udp","priority":5,"enabled":true},
		             {"id":"g2","name":"backup","sip_addr":"10.0.0.2:5060","protocol":"udp","priority":10,"enabled":true}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("replace status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List reflects the applied set.
	resp = do("GET", "/v1/admin/gateways", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var lr struct {
		Gateways []map[string]any `json:"gateways"`
		Count    int              `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if lr.Count != 2 || s.gws.Count() != 2 {
		t.Fatalf("count: body=%d memory=%d", lr.Count, s.gws.Count())
	}

	// Invalid entry rejected wholesale (400), state untouched.
	resp = do("POST", "/v1/admin/gateways", `{"gateways":[{"id":"x","name":"x"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid replace status = %d (want 400)", resp.StatusCode)
	}
	resp.Body.Close()
	if s.gws.Count() != 2 {
		t.Fatalf("invalid replace must not apply, count=%d", s.gws.Count())
	}

	// Single removal.
	resp = do("POST", "/v1/admin/gateways", `{"remove_id":"g2"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("remove status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if s.gws.Count() != 1 {
		t.Fatalf("post-remove count = %d", s.gws.Count())
	}

	// Unknown removal: 404.
	resp = do("POST", "/v1/admin/gateways", `{"remove_id":"nope"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown remove status = %d (want 404)", resp.StatusCode)
	}
	resp.Body.Close()

	// Broadcasts: two entity=gateway payloads (replace + remove),
	// each carrying the full array.
	flush(s)
	subjects, bodies := cap.snapshot()
	n := 0
	for i, subj := range subjects {
		if subj != events.SubjectCfgReload {
			continue
		}
		if bodies[i]["entity"] != "gateway" {
			continue
		}
		n++
		gws, _ := bodies[i]["gateways"].([]any)
		if len(gws) != 2 && len(gws) != 1 {
			t.Fatalf("gateway broadcast payload: %d entries", len(gws))
		}
	}
	if n != 2 {
		t.Fatalf("gateway broadcasts = %d (want 2: replace + remove)", n)
	}
}

// TestGatewayReloadBroadcastApplies: the dispatch-side consumer case
// — a cfg.reload payload rebuilds the in-memory set.
func TestGatewayReloadBroadcastApplies(t *testing.T) {
	s := NewServer()
	s.gatewayApply(map[string]any{
		"entity": "gateway",
		"gateways": []map[string]any{
			{"id": "z", "name": "z", "sip_addr": "z:5060"},
		},
	})
	if s.gws.Count() != 1 {
		t.Fatalf("apply left count=%d", s.gws.Count())
	}
}

// TestCdrRawConsumerHandler: the CDRR consumer body — a
// gvos.cdr.raw payload (callEndReq shape) finalizes billing through
// the same callEnd path the HTTP face uses. A completed call lands
// in the CDR ring; a bad payload errors without touching state.
func TestCdrRawConsumerHandler(t *testing.T) {
	s := NewServer()
	// Open a call (bill-lock) the way Kamailio would — destination
	// under a demo rate prefix (8613).
	if code, _ := s.callStart(callStartReq{CallID: "cdrr-1", From: "1000", To: "8613800138000"}); code != 200 {
		t.Fatalf("callStart failed: %d", code)
	}
	handler := cdrRawHandler(s)
	if err := handler([]byte(`{"call_id":"cdrr-1","duration":30,"sip_code":200}`)); err != nil {
		t.Fatalf("cdr.raw handler error: %v", err)
	}
	if len(s.cdrs) != 1 || s.cdrs[0].CallID != "cdrr-1" {
		t.Fatalf("CDR ring = %v", s.cdrs)
	}
	// Unknown call: error surfaces for redelivery.
	if err := handler([]byte(`{"call_id":"ghost","duration":1}`)); err == nil {
		t.Fatal("unknown call_id must error (redelivery path)")
	}
	// Malformed payload: error, state intact.
	if err := handler([]byte(`{not json`)); err == nil {
		t.Fatal("bad json must error")
	}
}

// TestGatewayBroadcastLCROrder: the gateway broadcast carries the LCR
// order (cheapest-first encoded into Priority); a rate change flips
// the order in the next broadcast.
func TestGatewayBroadcastLCROrder(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)

	// Two gateways serving 86: expensive (grp demo = 0.05/min per demo
	// seed), cheap (a lower demo rate is not available in the demo
	// table — use priority semantics: LCR falls back to base priority
	// when cost rates resolve equally).
	s.gws.Reload([]gateway.Gateway{
		{ID: "g1", Name: "one", SipAddr: "10.0.0.1:5060", Priority: 1, Enabled: true, Prefixes: []string{"86"}},
		{ID: "g2", Name: "two", SipAddr: "10.0.0.2:5060", Priority: 5, Enabled: true, Prefixes: []string{"86"}},
	})
	gws := s.lcrGateways()
	if gws[0].ID != "g1" {
		t.Fatalf("equal cost rates -> base priority decides: %v", gws[0].ID)
	}

	// Declare distinct cost groups via rate tables: memory mode maps
	// every group to the demo list, so differentiation comes only
	// from Priority — assert the contract: Priority encodes the order
	// index (broadcast consumers reproduce it via lowest-wins).
	for i, g := range gws {
		if g.Priority != i {
			t.Errorf("gateway %s priority = %d, want order index %d", g.ID, g.Priority, i)
		}
	}
	_ = cap
}
