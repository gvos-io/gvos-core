// Package originate is the outboard UAC dialer (M11 F1 edge side,
// design option B): consumes gvos.call.originate and issues the SIP
// signaling the edge (Kamailio) routes and bills through the normal
// faces. The dialer is pure SIP signaling — it never talks to core's
// HTTP plane, and billing/settlement ride the INVITEs the edge sees.
//
// Two flows:
//   - single leg (to_agent set, AI outbound): INVITE the dialed party;
//     media routes to the media agent at the edge (destination type
//     media_agent, did-inbound-design.md).
//   - two legs (RFC 3725 flow I): INVITE caller (no SDP) → 200
//     (offer) → INVITE callee (offer) → 200 (answer) → ACK callee →
//     ACK caller (answer) — media flows caller↔callee anchored at
//     rtpengine; the controller shuttles opaque SDP blobs only.
package originate

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gvos-io/gvos-core/internal/shared/events"
	"github.com/gvos-io/gvos-core/internal/sipuac"
)

// Options configures one dialer.
type Options struct {
	NATSURL      string // GVOS_ORIG_NATS_URL
	SIPAddr      string // edge (Kamailio) host:port — GVOS_ORIG_SIP_ADDR
	SIPLocalHost string // announced Via/Contact host — GVOS_ORIG_SIP_HOST
	SIPLocalPort int    // GVOS_ORIG_SIP_PORT
	Realm        string // SIP domain — GVOS_ORIG_REALM
}

// dialer tracks live calls keyed by the origination resource id
// (= SIP Call-ID on both legs; leg B suffixed? No — the callee INVITE
// uses the same Call-ID plus a distinct tag is enough at the edge;
// Kamailio keys dialogs by Call-ID+tags).
type dialer struct {
	opts Options
	c    *sipuac.Client

	mu   sync.Mutex
	live map[string]*liveCall
}

type liveCall struct {
	callID   string
	maxDur   time.Duration
	legA     *sipuac.InviteHandle
	legB     *sipuac.InviteHandle
	answered bool
	deadline *time.Timer
}

// Start wires the NATS subscriptions and returns a stop func.
func Start(opts Options) (stop func(), err error) {
	if opts.SIPAddr == "" {
		return nil, fmt.Errorf("originate: GVOS_ORIG_SIP_ADDR required")
	}
	c, err := sipuac.NewClient(sipuac.Options{
		LocalHost: opts.SIPLocalHost, LocalPort: opts.SIPLocalPort,
		RemoteAddr: opts.SIPAddr, Realm: opts.Realm,
	})
	if err != nil {
		return nil, err
	}
	d := &dialer{opts: opts, c: c, live: map[string]*liveCall{}}

	nc, err := events.New(opts.NATSURL)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("originate: nats connect: %w", err)
	}
	// Durable queue: multiple dialer instances share the workload and
	// a down dialer holds instructions in the stream instead of
	// dropping them.
	cons, err := nc.NewDurableConsumer("originate", events.SubjectCallOriginate, func(data []byte) error {
		d.handle(data)
		return nil
	}, 4)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("originate: consumer: %w", err)
	}
	// Core teardown (admin DELETE on an answered call) must reach the
	// legs even when the edge event is missed — broadcast fan-out.
	_ = nc.Subscribe(events.SubjectCallTeardown, func(data []byte) {
		var t struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(data, &t) == nil && t.CallID != "" {
			d.hangup(t.CallID)
		}
	})
	log.Printf("originate dialer up: sip=%s nats=%s", opts.SIPAddr, opts.NATSURL)
	return func() {
		cons.Stop()
		c.Close()
		nc.Close()
	}, nil
}

// instruction is one gvos.call.originate payload.
type instruction struct {
	Op           string `json:"op"`
	CallID       string `json:"call_id"`
	Caller       string `json:"caller"`
	Callee       string `json:"callee"`
	ToAgent      string `json:"to_agent"`
	AccountID    string `json:"account_id"`
	MaxDurationS int    `json:"max_duration_s"`
}

func (d *dialer) handle(msg []byte) {
	var in instruction
	if err := json.Unmarshal(msg, &in); err != nil || in.CallID == "" {
		log.Printf("originate: bad payload: %v", err)
		return
	}
	if in.Op == "cancel" {
		d.hangup(in.CallID)
		return
	}
	if in.ToAgent != "" && in.Callee == "" {
		go d.singleLeg(in)
		return
	}
	go d.twoLegs(in)
}

// singleLeg dials one party (AI outbound): INVITE → 200 → ACK. The
// edge hooks the normal start/answer faces; max_duration tears down.
func (d *dialer) singleLeg(in instruction) {
	h, err := d.c.Invite(in.CallID, in.AccountID, in.Caller, "", 70)
	if err != nil {
		log.Printf("originate %s: invite: %v", in.CallID, err)
		return
	}
	d.mu.Lock()
	lc := &liveCall{callID: in.CallID, maxDur: time.Duration(in.MaxDurationS) * time.Second, legA: h}
	d.live[in.CallID] = lc
	d.mu.Unlock()
	if lc.maxDur > 0 {
		lc.deadline = time.AfterFunc(lc.maxDur, func() { d.hangup(in.CallID) })
	}
	for {
		r, err := h.Response()
		if err != nil {
			d.hangup(in.CallID)
			return
		}
		if r.Code >= 200 && r.Code < 300 {
			_ = h.Ack(r, "")
			d.mu.Lock()
			lc.answered = true
			d.mu.Unlock()
			return // dialog established; teardown rides the edge
		}
		if r.Code >= 400 {
			d.hangup(in.CallID)
			return
		}
	}
}

// twoLegs runs RFC 3725 flow I.
func (d *dialer) twoLegs(in instruction) {
	// Leg A: INVITE with no SDP — the answer carries the offer.
	ha, err := d.c.Invite(in.CallID, in.AccountID, in.Caller, "", 70)
	if err != nil {
		log.Printf("originate %s: invite a: %v", in.CallID, err)
		return
	}
	d.mu.Lock()
	lc := &liveCall{callID: in.CallID, maxDur: time.Duration(in.MaxDurationS) * time.Second, legA: ha}
	d.live[in.CallID] = lc
	d.mu.Unlock()
	if lc.maxDur > 0 {
		lc.deadline = time.AfterFunc(lc.maxDur, func() { d.hangup(in.CallID) })
	}
	var offer string
	for {
		r, rerr := ha.Response()
		if rerr != nil {
			d.hangup(in.CallID)
			return
		}
		if r.Code >= 200 && r.Code < 300 {
			offer = r.Body
			break // A answered; ACK deferred until B's answer (flow I)
		}
		if r.Code >= 400 {
			log.Printf("originate %s: leg a failed: %d %s", in.CallID, r.Code, r.Reason)
			d.hangup(in.CallID)
			return
		}
		// provisional: keep waiting
	}

	// Leg B: INVITE with A's offer as the offer.
	hb, err := d.c.Invite(in.CallID+"-b", in.AccountID, in.Callee, offer, 70)
	if err != nil {
		log.Printf("originate %s: invite b: %v", in.CallID, err)
		d.hangup(in.CallID)
		return
	}
	d.mu.Lock()
	lc.legB = hb
	d.mu.Unlock()
	for {
		r, rerr := hb.Response()
		if rerr != nil {
			d.hangup(in.CallID)
			return
		}
		if r.Code >= 200 && r.Code < 300 {
			// Answer received: ACK B (no SDP — B's 200 answered our
			// offer), then ACK A carrying B's answer (flow I steps 5/6).
			_ = hb.Ack(r, "")
			_ = ha.Ack(sipuac.Response{Code: 200, ToTag: r.ToTag}, r.Body)
			d.mu.Lock()
			lc.answered = true
			d.mu.Unlock()
			return // bridged; teardown rides the edge / max duration
		}
		if r.Code >= 400 {
			log.Printf("originate %s: leg b failed: %d %s — hanging up a", in.CallID, r.Code, r.Reason)
			d.hangup(in.CallID)
			return
		}
	}
}

// hangup cancels pending INVITEs and BYEs established legs.
func (d *dialer) hangup(callID string) {
	d.mu.Lock()
	lc, ok := d.live[callID]
	if ok {
		delete(d.live, callID)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	if lc.deadline != nil {
		lc.deadline.Stop()
	}
	// CANCEL pending, BYE established — whichever applies per leg.
	if lc.legA != nil {
		if lc.answered {
			_ = d.c.Bye(callID)
		} else {
			_ = lc.legA.Cancel()
		}
	}
	if lc.legB != nil {
		_ = d.c.Bye(callID + "-b")
	}
}
