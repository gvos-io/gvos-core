// MockServer is a UDP SIP endpoint for loopback tests — it answers
// INVITEs per a caller-supplied script and records every message it
// sees, so dialer-level tests assert CANCEL/BYE/3PCC sequences at
// packet level. Test infrastructure, exported for the originate
// tests; never wired into product code.
package sipuac

import (
	"net"
	"strings"
	"sync"
)

// MockServer answers INVITEs per a scripted sequence.
type MockServer struct {
	conn     *net.UDPConn
	mu       sync.Mutex
	invites  []string // received raw INVITEs
	all      []string // every message (INVITE/ACK/BYE/CANCEL)
	script   func(raw string) []string
	nextPort int
}

// NewMockServer binds a random UDP port and starts answering.
func NewMockServer(script func(raw string) []string) (*MockServer, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		return nil, err
	}
	m := &MockServer{conn: conn, script: script}
	go m.loop()
	return m, nil
}

// Addr is the mock's "host:port" (the client's RemoteAddr).
func (m *MockServer) Addr() string { return m.conn.LocalAddr().String() }

// Close releases the socket.
func (m *MockServer) Close() error { return m.conn.Close() }

// NextClientPort returns a free UDP port for a client socket (avoids
// cross-test port collisions on the fixed default).
func (m *MockServer) NextClientPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextPort++
	return 5090 + m.nextPort
}

// Seen counts received messages starting with prefix.
func (m *MockServer) Seen(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, raw := range m.all {
		if strings.HasPrefix(raw, prefix) {
			n++
		}
	}
	return n
}

// Invites returns the received raw INVITEs.
func (m *MockServer) Invites() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.invites...)
}

func (m *MockServer) loop() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		raw := strings.ReplaceAll(string(buf[:n]), "\r\n", "\n")
		m.mu.Lock()
		m.all = append(m.all, raw)
		if strings.HasPrefix(raw, "INVITE ") {
			m.invites = append(m.invites, raw)
		}
		responds := m.script(raw)
		m.mu.Unlock()
		// Echo the request's real Via branch — the client dispatches
		// responses by it (a mismatch drops the response silently).
		branch := headerValue(raw, "Via", "branch=")
		for _, r := range responds {
			_, _ = m.conn.WriteToUDP([]byte(strings.ReplaceAll(strings.ReplaceAll(r, "\n", "\r\n"), "branch=z9hG4bK", "branch="+branch)), addr)
		}
	}
}
