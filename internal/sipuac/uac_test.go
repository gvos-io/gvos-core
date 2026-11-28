// Loopback tests: a mock SIP server answers over real UDP so the
// transaction, dialog and 3PCC plumbing are exercised at packet level
// (not mocked at the API boundary).
package sipuac

import (
	"strings"
	"testing"
	"time"
)

// newMockServer wraps the exported MockServer for this file's tests.
func newMockServer(t *testing.T, script func(raw string) []string) *MockServer {
	t.Helper()
	m, err := NewMockServer(script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func newTestClient(t *testing.T, m *MockServer) *Client {
	t.Helper()
	c, err := NewClient(Options{
		RemoteAddr: m.Addr(), LocalPort: 0, Realm: "gvos.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func ok200(callID string, toTag string, sdp string) string {
	return "SIP/2.0 200 OK\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK\n" +
		"From: <sip:a@x>;tag=local\n" +
		"To: <sip:b@x>;tag=" + toTag + "\n" +
		"Call-ID: " + callID + "\n" +
		"CSeq: 1 INVITE\n" +
		"Content-Type: application/sdp\n" +
		"Content-Length: " + itoa(len(sdp)) + "\n\n" + sdp
}

func ringing180(callID string, toTag string) string {
	return "SIP/2.0 180 Ringing\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK\n" +
		"From: <sip:a@x>;tag=local\n" +
		"To: <sip:b@x>;tag=" + toTag + "\n" +
		"Call-ID: " + callID + "\n" +
		"CSeq: 1 INVITE\n\n"
}

func fail487(callID string) string {
	return "SIP/2.0 487 Request Terminated\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK\n" +
		"From: <sip:a@x>;tag=local\n" +
		"To: <sip:b@x>;tag=peer\n" +
		"Call-ID: " + callID + "\n" +
		"CSeq: 1 INVITE\n\n"
}

// Happy path: provisional, then final 200 with SDP; ACK registered a
// dialog; BYE retires it.
func TestInviteAnswerAckBye(t *testing.T) {
	const callID = "lifecycle-1"
	m := newMockServer(t, func(raw string) []string {
		return []string{ringing180(callID, "peer"), ok200(callID, "peer", "v=0\r\nm=audio 4000 RTP/AVP 0")}
	})
	c := newTestClient(t, m)

	h, err := c.Invite(callID, "13800001234", "13900005678", "", 70)
	if err != nil {
		t.Fatal(err)
	}
	// Provisional first.
	r1, err := h.Response()
	if err != nil || r1.Code != 180 {
		t.Fatalf("first response = %+v err=%v, want 180", r1, err)
	}
	// Final 200 with an offer.
	r2, err := h.Response()
	if err != nil || r2.Code != 200 {
		t.Fatalf("final response = %+v err=%v, want 200", r2, err)
	}
	if !strings.Contains(r2.Body, "m=audio 4000") {
		t.Fatalf("200 without SDP body: %+v", r2)
	}
	if r2.ToTag != "peer" {
		t.Fatalf("ToTag = %q, want peer", r2.ToTag)
	}
	if err := h.Ack(r2, "v=0\r\nm=audio 5000 RTP/AVP 0"); err != nil {
		t.Fatal(err)
	}
	// UDP is asynchronous on the receive side — poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.Seen("ACK ") == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Seen("ACK ") != 1 {
		t.Fatalf("acks seen = %d, want 1", m.Seen("ACK "))
	}
	if err := c.Bye(callID); err != nil {
		t.Fatalf("bye: %v", err)
	}
	byeDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(byeDeadline) && m.Seen("BYE ") == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Seen("BYE ") != 1 {
		t.Fatalf("byes seen = %d, want 1", m.Seen("BYE "))
	}
}

// Cancel: the CANCEL goes out on the same branch and the 487 final
// completes the transaction (no dialog registered).
func TestInviteCancel(t *testing.T) {
	const callID = "cancel-1"
	m := newMockServer(t, func(raw string) []string {
		if strings.HasPrefix(raw, "CANCEL ") {
			return []string{fail487(callID)}
		}
		return nil // ring forever
	})
	c := newTestClient(t, m)

	h, err := c.Invite(callID, "a", "b", "", 70)
	if err != nil {
		t.Fatal(err)
	}
	// Give the INVITE a moment, then cancel.
	time.Sleep(50 * time.Millisecond)
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	r, err := h.Response()
	if err != nil || r.Code != 487 {
		t.Fatalf("response = %+v err=%v, want 487", r, err)
	}
	if m.Seen("CANCEL ") != 1 {
		t.Fatal("CANCEL never reached the mock")
	}
}

// Retransmission: a silent server sees the INVITE repeated on the
// timer A schedule.
func TestInviteRetransmits(t *testing.T) {
	const callID = "rtx-1"
	m := newMockServer(t, func(raw string) []string { return nil }) // silent
	c := newTestClient(t, m)

	if _, err := c.Invite(callID, "a", "b", "", 70); err != nil {
		t.Fatal(err)
	}
	// Timer A fires at 500ms; wait ~1.3s → at least 2 sends.
	time.Sleep(1300 * time.Millisecond)
	if n := m.Seen("INVITE "); n < 2 {
		t.Fatalf("mock saw %d invites, want >= 2 (retransmission broken)", n)
	}
}
