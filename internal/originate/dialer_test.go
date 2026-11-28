// Dialer orchestration tests (loopback): single-leg, the two-leg
// RFC 3725 flow-I sequence, and cancel — all against a mock SIP
// server over real UDP.
package originate

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/sipuac"
)

func newMock(t *testing.T, script func(raw string) []string) *sipuac.MockServer {
	t.Helper()
	m, err := sipuac.NewMockServer(script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func newClient(t *testing.T, m *sipuac.MockServer) *sipuac.Client {
	t.Helper()
	c, err := sipuac.NewClient(sipuac.Options{
		RemoteAddr: m.Addr(), LocalPort: 0, Realm: "gvos.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func ok200(callID, toTag, sdp string) string {
	return "SIP/2.0 200 OK\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK\n" +
		"From: <sip:a@x>;tag=local\n" +
		"To: <sip:b@x>;tag=" + toTag + "\n" +
		"Call-ID: " + callID + "\n" +
		"CSeq: 1 INVITE\n" +
		"Content-Type: application/sdp\n" +
		"Content-Length: " + strconv.Itoa(len(sdp)) + "\n\n" + sdp
}

func fail487(callID string) string {
	return "SIP/2.0 487 Request Terminated\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK\n" +
		"From: <sip:a@x>;tag=local\n" +
		"To: <sip:b@x>;tag=peer\n" +
		"Call-ID: " + callID + "\n" +
		"CSeq: 1 INVITE\n\n"
}

func instr(callID, caller, callee, agent string) []byte {
	b, _ := json.Marshal(instruction{
		Op: "dial", CallID: callID, Caller: caller, Callee: callee,
		ToAgent: agent, AccountID: "acc", MaxDurationS: 600,
	})
	return b
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) // parallel package runs contend for CPU
	for time.Now().Before(deadline) && !cond() {
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition never met: %s", what)
	}
}

func TestDialerSingleLeg(t *testing.T) {
	m := newMock(t, func(raw string) []string {
		if strings.HasPrefix(raw, "INVITE ") {
			return []string{ok200("solo-1", "peer", "v=0")}
		}
		return nil
	})
	d := &dialer{c: newClient(t, m), live: map[string]*liveCall{}}
	d.handle(instr("solo-1", "13800001234", "", "agent-bot"))

	waitUntil(t, func() bool { return m.Seen("ACK ") > 0 }, "ACK after 200")
	if n := m.Seen("INVITE "); n != 1 {
		t.Fatalf("invites = %d, want 1", n)
	}
	// Single leg dials the `caller` (the billed, dialed party).
	invites := m.Invites()
	inv := strings.Join(invites, "\n")
	if !strings.Contains(inv, "INVITE sip:13800001234@") {
		t.Fatalf("single leg dialed the wrong party:\n%s", inv)
	}
}

func TestDialerTwoLegs3PCC(t *testing.T) {
	var offerSeen bool
	m := newMock(t, func(raw string) []string {
		if !strings.HasPrefix(raw, "INVITE ") {
			return nil
		}
		// Leg A (no SDP) answers with an offer; leg B (carrying it)
		// answers with the answer body.
		if strings.Contains(raw, "v=0") {
			offerSeen = true
			return []string{ok200("two-1-b", "peerb", "v=0\r\nm=audio 6000 RTP/AVP 0")}
		}
		return []string{ok200("two-1", "peera", "v=0\r\nm=audio 5000 RTP/AVP 0")}
	})
	d := &dialer{c: newClient(t, m), live: map[string]*liveCall{}}
	d.handle(instr("two-1", "13800001234", "13900005678", ""))

	waitUntil(t, func() bool { return m.Seen("ACK ") >= 2 }, "both ACKs")
	if n := m.Seen("INVITE "); n != 2 {
		t.Fatalf("invites = %d, want 2 (both legs)", n)
	}
	if !offerSeen {
		t.Fatal("leg B never carried leg A's offer (flow I broken)")
	}
	// Teardown: BYE both legs.
	d.hangup("two-1")
	waitUntil(t, func() bool { return m.Seen("BYE ") >= 2 }, "both BYEs")
}

func TestDialerCancelMidRing(t *testing.T) {
	m := newMock(t, func(raw string) []string {
		if strings.HasPrefix(raw, "CANCEL ") {
			return []string{fail487("cancel-1")}
		}
		return nil // ring forever
	})
	d := &dialer{c: newClient(t, m), live: map[string]*liveCall{}}
	go d.handle(instr("cancel-1", "13800001234", "13900005678", ""))
	waitUntil(t, func() bool { return m.Seen("INVITE ") > 0 }, "INVITE out")
	d.handle([]byte(`{"op":"cancel","call_id":"cancel-1"}`))
	waitUntil(t, func() bool { return m.Seen("CANCEL ") > 0 }, "CANCEL out")
}
