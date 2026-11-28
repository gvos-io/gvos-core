// Package sipuac is the minimal SIP user-agent client the origination
// dialer runs on (M11 F1 edge side, design option B: an outboard UAC
// consumes gvos.call.originate and issues the INVITEs). Scope is
// deliberately narrow: UDP client transactions (INVITE/ACK/BYE/
// CANCEL), the retransmission timers RFC 3261 mandates for unreliable
// transport, one-dialog-per-call bookkeeping, and in-dialog
// re-INVITE for the RFC 3725 flow-I 3PCC handoff. Server behavior is
// Kamailio's problem; authentication is out of scope (trunk INVITEs
// are CIDR-gated at the edge — see did-inbound-design.md).
package sipuac

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Options configures one client.
type Options struct {
	// LocalHost: the host/IP this UAC announces in Via/Contact.
	LocalHost string
	// LocalPort: the UDP port bound for SIP traffic.
	LocalPort int
	// RemoteAddr: the edge (Kamailio) "host:port" every request goes to.
	RemoteAddr string
	// Realm: the SIP domain announced in From/To (usually the edge's).
	Realm string
}

// Client is one UDP SIP UAC. Safe for concurrent use.
type Client struct {
	mu   sync.Mutex
	opts Options
	conn *net.UDPConn
	done chan struct{}
	// txns: in-flight INVITE transactions keyed by branch. Responses
	// for an unknown branch are dropped (stray 200 retransmits after
	// a timeout are the common case).
	txns map[string]*inviteTxn
	// dialogs: established dialogs keyed by call-id (remote tag
	// learned from the 2xx) for in-dialog requests.
	dialogs map[string]*Dialog
}

// Dialog is one established client-side dialog (remote tag + remote
// target learned from the 2xx Contact, falling back to the remote
// address).
type Dialog struct {
	CallID       string
	RemoteTag    string
	LocalTag     string
	LocalCSeq    uint32
	RemoteCSeq   uint32
	RemoteTarget string
	RouteSet     string
}

// Response is one SIP response worth surfacing to the caller.
type Response struct {
	Code   int
	Reason string
	ToTag  string
	Body   string // SDP payload when present
	Entire string
}

// inviteTxn is one in-flight INVITE client transaction.
type inviteTxn struct {
	callID   string
	branch   string
	cseq     uint32
	raw      string
	fromTag  string
	respCh   chan Response
	acked    bool
	canceled bool
}

// NewClient binds the UDP socket and starts the response pump.
func NewClient(opts Options) (*Client, error) {
	if opts.LocalHost == "" {
		opts.LocalHost = "127.0.0.1"
	}
	if opts.LocalPort == 0 {
		opts.LocalPort = 5070
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: opts.LocalPort})
	if err != nil {
		return nil, fmt.Errorf("sipuac: bind udp %d: %w", opts.LocalPort, err)
	}
	// Port 0 = kernel-assigned: the Via/Contact announce the BOUND port
	// (parallel tests handing out fixed ports used to collide).
	if opts.LocalPort == 0 {
		opts.LocalPort = conn.LocalAddr().(*net.UDPAddr).Port
	}
	c := &Client{
		opts:    opts,
		conn:    conn,
		done:    make(chan struct{}),
		txns:    map[string]*inviteTxn{},
		dialogs: map[string]*Dialog{},
	}
	go c.pump()
	return c, nil
}

// Close stops the pump and releases the socket.
func (c *Client) Close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	_ = c.conn.Close()
}

// pump reads responses and dispatches them to in-flight transactions.
func (c *Client) pump() {
	buf := make([]byte, 65535)
	for {
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
				continue
			}
		}
		c.dispatch(strings.ReplaceAll(string(buf[:n]), "\r\n", "\n"))
		select {
		case <-c.done:
			return
		default:
		}
	}
}

// dispatch routes one response by its Via branch.
func (c *Client) dispatch(raw string) {
	branch := headerValue(raw, "Via", "branch=")
	if branch == "" {
		return
	}
	c.mu.Lock()
	txn, ok := c.txns[branch]
	c.mu.Unlock()
	if !ok {
		return
	}
	resp := parseResponse(raw)
	if resp == nil {
		return
	}
	// Final responses (>=200) retire the transaction (the caller
	// decides retransmission policy on the ACK side).
	if resp.Code >= 200 {
		c.mu.Lock()
		delete(c.txns, branch)
		c.mu.Unlock()
	}
	select {
	case txn.respCh <- *resp:
	default: // caller is slow: drop the earlier provisional
	}
}

// Invite issues one INVITE and runs its transaction (timer A
// retransmissions on a 500ms doubling schedule, timer B gives up at
// 32s). The handle lets the caller CANCEL, ACK with an answer (3PCC)
// or BYE the established dialog.
func (c *Client) Invite(callID, fromUser, toUser string, sdp string, maxForwards int) (*InviteHandle, error) {
	fromTag := randToken(8)
	branch := "z9hG4bK" + randToken(10)
	cseq := uint32(1)
	raw := c.buildRequest("INVITE", callID, fromUser, toUser, fromTag, branch, cseq, "", sdp, maxForwards)
	txn := &inviteTxn{
		callID: callID, branch: branch, cseq: cseq, raw: raw,
		fromTag: fromTag, respCh: make(chan Response, 8),
	}
	c.mu.Lock()
	c.txns[branch] = txn
	c.mu.Unlock()

	remote, rerr := net.ResolveUDPAddr("udp", c.opts.RemoteAddr)
	if rerr != nil {
		return nil, rerr
	}
	if _, err := c.conn.WriteToUDP([]byte(raw), remote); err != nil {
		return nil, err
	}
	go c.retransmit(txn, remote)
	return &InviteHandle{c: c, txn: txn, toUser: toUser, fromUser: fromUser}, nil
}

// retransmit runs timer A/B for one INVITE transaction.
func (c *Client) retransmit(txn *inviteTxn, remote *net.UDPAddr) {
	interval := 500 * time.Millisecond
	deadline := time.After(32 * time.Second)
	for {
		select {
		case <-time.After(interval):
		case <-deadline:
			c.mu.Lock()
			delete(c.txns, txn.branch)
			c.mu.Unlock()
			select {
			case txn.respCh <- Response{Code: 504, Reason: "timer B — no response"}:
			default:
			}
			return
		case <-c.done:
			return
		}
		// Stop retransmitting once a final response arrived (the txn
		// left the map) or the caller ACKed.
		c.mu.Lock()
		_, live := c.txns[txn.branch]
		c.mu.Unlock()
		if !live {
			return
		}
		_, _ = c.conn.WriteToUDP([]byte(txn.raw), remote)
		if interval < 4*time.Second {
			interval *= 2
		}
	}
}

// InviteHandle is one live INVITE transaction.
type InviteHandle struct {
	c        *Client
	txn      *inviteTxn
	fromUser string
	toUser   string
}

// Response returns the next response for the transaction (provisionals
// included; the caller loops until a final one).
func (h *InviteHandle) Response() (Response, error) {
	r := <-h.txn.respCh
	return r, r.Err()
}

// Err reports whether a response is terminal failure (no response at
// all / transport timeout).
func (r Response) Err() error {
	if r.Code == 0 {
		return fmt.Errorf("sipuac: empty response")
	}
	return nil
}

// Cancel aborts a pending INVITE (before any final response): sends
// CANCEL on the same branch. The 487 from the server completes the
// transaction.
func (h *InviteHandle) Cancel() error {
	raw := h.c.buildRequest("CANCEL", h.txn.callID, h.fromUser, h.toUser,
		h.txn.fromTag, h.txn.branch, h.txn.cseq, "", "", 0)
	remote, err := net.ResolveUDPAddr("udp", h.c.opts.RemoteAddr)
	if err != nil {
		return err
	}
	_, err = h.c.conn.WriteToUDP([]byte(raw), remote)
	return err
}

// Ack completes a 2xx on an INVITE, carrying answerSDP when the 200
// offered (RFC 3725 flow I shuttles it from the other leg). Registers
// the dialog for in-dialog requests.
func (h *InviteHandle) Ack(resp Response, answerSDP string) error {
	raw := h.c.buildRequest("ACK", h.txn.callID, h.fromUser, h.toUser,
		h.txn.fromTag, h.txn.branch, h.txn.cseq, resp.ToTag, answerSDP, 0)
	remote, err := net.ResolveUDPAddr("udp", h.c.opts.RemoteAddr)
	if err != nil {
		return err
	}
	if _, err := h.c.conn.WriteToUDP([]byte(raw), remote); err != nil {
		return err
	}
	if resp.Code >= 200 && resp.Code < 300 {
		h.c.mu.Lock()
		h.c.dialogs[h.txn.callID] = &Dialog{
			CallID: h.txn.callID, RemoteTag: resp.ToTag, LocalTag: h.txn.fromTag,
			LocalCSeq: h.txn.cseq, RemoteTarget: h.c.opts.RemoteAddr,
		}
		h.c.mu.Unlock()
	}
	return nil
}

// Bye terminates an established dialog.
func (c *Client) Bye(callID string) error {
	c.mu.Lock()
	d, ok := c.dialogs[callID]
	if ok {
		delete(c.dialogs, callID)
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("sipuac: no dialog for %s", callID)
	}
	d.LocalCSeq++
	raw := c.buildInDialog("BYE", d, d.LocalCSeq)
	remote, err := net.ResolveUDPAddr("udp", c.opts.RemoteAddr)
	if err != nil {
		return err
	}
	_, err = c.conn.WriteToUDP([]byte(raw), remote)
	return err
}

// ReInvite re-INVITEs an established dialog with new SDP (the 3PCC
// handoff: leg A's ACK carries leg B's answer — no separate
// re-INVITE needed for flow I; ReInvite exists for hold/resume and
// future flows).
func (c *Client) ReInvite(callID string, sdp string) error {
	c.mu.Lock()
	d, ok := c.dialogs[callID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("sipuac: no dialog for %s", callID)
	}
	d.LocalCSeq++
	raw := c.buildInDialog("INVITE", d, d.LocalCSeq)
	if sdp != "" {
		raw += sdp + "\r\n"
	}
	remote, err := net.ResolveUDPAddr("udp", c.opts.RemoteAddr)
	if err != nil {
		return err
	}
	_, err = c.conn.WriteToUDP([]byte(raw), remote)
	return err
}

// buildRequest renders one out-of-dialog (or CANCEL) request.
func (c *Client) buildRequest(method, callID, fromUser, toUser, fromTag, branch string, cseq uint32, toTag, sdp string, maxForwards int) string {
	var b strings.Builder
	b.WriteString(method + " sip:" + toUser + "@" + c.opts.Realm + " SIP/2.0\r\n")
	if method != "ACK" && method != "CANCEL" {
		if maxForwards > 0 {
			b.WriteString(fmt.Sprintf("Max-Forwards: %d\r\n", maxForwards))
		}
	}
	b.WriteString("Via: SIP/2.0/UDP " + c.opts.LocalHost + ":" + itoa(c.opts.LocalPort) + ";branch=" + branch + "\r\n")
	b.WriteString("From: <sip:" + fromUser + "@" + c.opts.Realm + ">;tag=" + fromTag + "\r\n")
	to := "sip:" + toUser + "@" + c.opts.Realm
	if toTag != "" {
		to += ";tag=" + toTag
	}
	b.WriteString("To: <" + to + ">\r\n")
	b.WriteString("Call-ID: " + callID + "\r\n")
	b.WriteString("CSeq: " + itoa(int(cseq)) + " " + method + "\r\n")
	if method == "INVITE" {
		b.WriteString("Contact: <sip:" + fromUser + "@" + c.opts.LocalHost + ":" + itoa(c.opts.LocalPort) + ">\r\n")
	}
	b.WriteString("Content-Type: application/sdp\r\n")
	if sdp == "" {
		b.WriteString("Content-Length: 0\r\n\r\n")
	} else {
		b.WriteString("Content-Length: " + itoa(len(sdp)) + "\r\n\r\n")
		b.WriteString(sdp + "\r\n")
	}
	return b.String()
}

// buildInDialog renders one in-dialog request (BYE / re-INVITE) with
// the learned remote tag.
func (c *Client) buildInDialog(method string, d *Dialog, cseq uint32) string {
	var b strings.Builder
	b.WriteString(method + " sip:" + d.RemoteTarget + " SIP/2.0\r\n")
	b.WriteString("Via: SIP/2.0/UDP " + c.opts.LocalHost + ":" + itoa(c.opts.LocalPort) + ";branch=z9hG4bK" + randToken(10) + "\r\n")
	b.WriteString("From: <sip:" + d.CallID + "@" + c.opts.Realm + ">;tag=" + d.LocalTag + "\r\n")
	b.WriteString("To: <sip:" + d.CallID + "@" + c.opts.Realm + ">;tag=" + d.RemoteTag + "\r\n")
	b.WriteString("Call-ID: " + d.CallID + "\r\n")
	b.WriteString("CSeq: " + itoa(int(cseq)) + " " + method + "\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")
	return b.String()
}

// parseResponse extracts what the dialer needs from one response.
func parseResponse(raw string) *Response {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "SIP/2.0 ") {
		return nil
	}
	fields := strings.SplitN(strings.TrimPrefix(lines[0], "SIP/2.0 "), " ", 2)
	code := 0
	fmt.Sscanf(fields[0], "%d", &code)
	reason := ""
	if len(fields) > 1 {
		reason = strings.TrimSpace(fields[1])
	}
	r := &Response{Code: code, Reason: reason, Entire: raw}
	if to := headerValue(raw, "To", "tag="); to != "" {
		r.ToTag = to
	}
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		r.Body = raw[i+4:]
	} else if i := strings.Index(raw, "\n\n"); i >= 0 {
		r.Body = raw[i+2:]
	}
	return r
}

// headerValue returns the parameter value from the first named header
// (e.g. branch= from Via).
func headerValue(raw, header, param string) string {
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(strings.ToLower(line), strings.ToLower(header)+":") {
			continue
		}
		if i := strings.Index(line, param); i >= 0 {
			v := line[i+len(param):]
			if j := strings.IndexAny(v, "; \r\n"); j >= 0 {
				v = v[:j]
			}
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
