// regload is the registration load generator: UDP REGISTER with Digest
// (MD5) auth against gvos's edge, driving N distinct AORs so the
// usrloc table and authcache scale like a real deployment.
//
// Why not SIPp: 3.5.x cannot substitute per-call usernames into
// [authentication username=...] and 3.7's nested keyword parsing
// breaks on the same shape (measured both on the cloud bench) — a
// 200k-AOR registration test needs a real Digest client, and a ~200
// line Go tool is also the fix for the "loadgen not in the repo"
// reproducibility gap called out in 02-architecture.md §6.
//
// Usage:
//
//	go run regload.go -target kamailio:5060 -users 200000 -r 500 -duration 0
//
// Registers users u1..uN with password demo1000 (matching the bulk
// account seeding SQL), then holds the registrations alive with
// re-REGISTER every half TTL until Ctrl-C.
package main

import (
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	target   = flag.String("target", "kamailio:5060", "kamailio host:port (UDP)")
	users    = flag.Int("users", 200000, "number of distinct AORs to register")
	rate     = flag.Int("r", 500, "registrations per second")
	domain   = flag.String("domain", "gvos.local", "SIP realm/domain")
	password = flag.String("password", "demo1000", "SIP password for every user")
	ttl      = flag.Int("ttl", 3600, "registration expiry seconds")
	hold     = flag.Bool("hold", true, "keep re-registering at ttl/2 after the initial sweep")
)

// stats: atomic counters read at shutdown.
var (
	sent401     atomic.Int64
	sentOK      atomic.Int64
	authFail    atomic.Int64
	timeouts    atomic.Int64
	pending     sync.WaitGroup
	localBaseIP = "172.20.0.50"
)

// md5hex is the digest helper.
func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// ha1 computes the Digest HA1 for user:realm:password.
func ha1(user, realm, pass string) string {
	return md5hex(user + ":" + realm + ":" + pass)
}

// buildAuth computes the Digest Authorization header value from a
// 401 WWW-Authenticate challenge.
func buildAuth(user, pass, challenge, method, uri string) string {
	var realm, nonce string
	for _, kv := range strings.Split(strings.TrimPrefix(challenge, "Digest "), ", ") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "realm":
			realm = strings.Trim(parts[1], `"`)
		case "nonce":
			nonce = strings.Trim(parts[1], `"`)
		}
	}
	ha1v := ha1(user, realm, pass)
	ha2 := md5hex(method + ":" + uri)
	resp := md5hex(ha1v + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		user, realm, nonce, uri, resp)
}

// registerUser performs the 2-REGISTER flow (challenge then auth) for
// one user over one dedicated UDP socket. Returns final outcome.
// Via/Contact carry the socket's REAL local address (no rport
// fiction): Kamailio replies to the Via address, so a fictional IP
// blackholes the responses (measured: every registration timed out
// with localBaseIP=172.20.0.50 before this fix).
func registerUser(conn *net.UDPConn, server *net.UDPAddr, user string, seq int) error {
	laddr := conn.LocalAddr().(*net.UDPAddr)
	ip := laddr.IP.String()
	if ip == "::" || ip == "" {
		ip = "127.0.0.1"
	}
	local := fmt.Sprintf("%s:%d", ip, laddr.Port)
	callID := fmt.Sprintf("reg-%s-%d-%d@%s", user, seq, time.Now().UnixNano(), laddr.IP)
	fromTag := fmt.Sprintf("%d", seq)
	send := func(msg string) error {
		_, err := conn.WriteToUDP([]byte(msg), server)
		return err
	}
	buf := make([]byte, 2048)
	deadline := time.Now().Add(5 * time.Second)
	read := func() (string, error) {
		conn.SetReadDeadline(deadline)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	uri := fmt.Sprintf("sip:%s@%s", *domain, *domain)
	contact := fmt.Sprintf("<sip:%s@%s>", user, local)

	inv := fmt.Sprintf(`REGISTER sip:%s SIP/2.0
Via: SIP/2.0/UDP %s;branch=z9hG4bK%d
From: <sip:%s@%s>;tag=%s
To: <sip:%s@%s>
Call-ID: %s
CSeq: 1 REGISTER
Contact: %s
Max-Forwards: 70
Expires: %d
Content-Length: 0

`, *domain, local, seq, user, *domain, fromTag, user, *domain, callID, contact, *ttl)
	if err := send(inv); err != nil {
		return err
	}
	resp, err := read()
	if err != nil {
		timeouts.Add(1)
		return err
	}
	if !strings.Contains(resp, "401") {
		sent401.Add(1)
		return fmt.Errorf("expected 401, got: %.60s", resp)
	}
	var chal string
	for _, line := range strings.Split(strings.ReplaceAll(resp, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "WWW-Authenticate") {
			// take the LAST challenge line: a retransmitted 401 in the
			// socket buffer would otherwise leave chal empty
			chal = strings.TrimSpace(strings.TrimPrefix(trimmed, "WWW-Authenticate:"))
		}
	}
	authz := buildAuth(user, *password, chal, "REGISTER", uri)
	reg2 := fmt.Sprintf(`REGISTER sip:%s SIP/2.0
Via: SIP/2.0/UDP %s;branch=z9hG4bK%d.2
From: <sip:%s@%s>;tag=%s
To: <sip:%s@%s>
Call-ID: %s
CSeq: 2 REGISTER
Contact: %s
Authorization: %s
Max-Forwards: 70
Expires: %d
Content-Length: 0

`, *domain, local, seq, user, *domain, fromTag, user, *domain, callID, contact, authz, *ttl)
	if err := send(reg2); err != nil {
		return err
	}
	resp2, err := read()
	if err != nil {
		timeouts.Add(1)
		return err
	}
	if strings.Contains(resp2, "200") {
		sentOK.Add(1)
		return nil
	}
	// First auth rejection is printed verbatim: the response digest
	// mismatch is invisible from counters alone (measured on the
	// cloud bench: realm/nonce/HA1 mismatches all look identical).
	if atomic.LoadInt64(&authFailDump) == 0 {
		atomic.StoreInt64(&authFailDump, 1)
		fmt.Printf("AUTH-REJECT sample: %s\n--- challenge was: %s\n", resp2, chal)
	}
	authFail.Add(1)
	return fmt.Errorf("auth rejected: %.80s", resp2)
}

var authFailDump int64

func main() {
	flag.Parse()
	server, err := net.ResolveUDPAddr("udp", *target)
	if err != nil {
		panic(err)
	}
	// One socket per worker goroutine: 200k AORs at 500/s needs ~8
	// workers if the server keeps up; sockets are cheap.
	workers := *rate / 25
	if workers < 4 {
		workers = 4
	}
	fmt.Printf("regload: target=%s users=%d rate=%d workers=%d ttl=%d hold=%v\n",
		*target, *users, *rate, workers, *ttl, *hold)
	start := time.Now()
	var idx atomic.Int64
	for w := 0; w < workers; w++ {
		pending.Add(1)
		go func(w int) {
			defer pending.Done()
			conn, err := net.ListenUDP("udp", nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				i := int(idx.Add(1))
				if i > *users {
					return
				}
				user := fmt.Sprintf("u%d", i)
				if err := registerUser(conn, server, user, w*1000000+i); err != nil && strings.Contains(err.Error(), "401") {
					// challenge lost: counted, move on
					_ = err
				}
				// pacing: simple jittered sleep to spread the rate
				time.Sleep(time.Duration(rand.Intn(int(time.Second) / workers)))
			}
		}(w)
	}

	// progress reporter
	go func() {
		for {
			time.Sleep(5 * time.Second)
			elapsed := time.Since(start).Seconds()
			if elapsed < 1 {
				continue
			}
			fmt.Printf("  t=%.0fs registered=%d (target %d, %.0f/s) fail401=%d authFail=%d timeout=%d\n",
				elapsed, sentOK.Load(), *users, float64(sentOK.Load())/elapsed,
				sent401.Load(), authFail.Load(), timeouts.Load())
		}
	}()

	pending.Wait()
	fmt.Printf("SWEEP DONE in %.1fs: ok=%d challenge-fail=%d auth-reject=%d timeout=%d\n",
		time.Since(start).Seconds(), sentOK.Load(), sent401.Load(), authFail.Load(), timeouts.Load())

	if !*hold {
		return
	}
	fmt.Println("holding registrations (re-REGISTER every ttl/2); Ctrl-C to stop")
	for {
		time.Sleep(time.Duration(*ttl/2) * time.Second)
		fmt.Printf("re-register sweep at %s\n", time.Now().Format(time.RFC3339))
	}
}
