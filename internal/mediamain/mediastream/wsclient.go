// Client-side WebSocket dialing for the media plane: connects to a
// ws:// endpoint (TLS optional), performs the RFC 6455 handshake, and
// writes masked client frames. Reading reuses ReadFrame.
package mediastream

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// WSConn is a dialed WebSocket connection.
type WSConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// DialWS connects to url (ws:// or wss://) with extra headers and
// performs the upgrade handshake. Host header and Sec-WebSocket-*
// headers are added automatically.
func DialWS(rawurl string, headers map[string]string, timeout time.Duration) (*WSConn, error) {
	useTLS := strings.HasPrefix(rawurl, "wss://")
	rest := strings.TrimPrefix(strings.TrimPrefix(rawurl, "wss://"), "ws://")
	if rest == rawurl {
		return nil, fmt.Errorf("url must be ws:// or wss://")
	}
	hostPart := rest
	path := "/"
	if i := strings.Index(rest, "/"); i >= 0 {
		hostPart = rest[:i]
		path = rest[i:]
	}
	port := "80"
	if useTLS {
		port = "443"
	}
	if strings.Contains(hostPart, ":") {
		port = ""
	} else {
		hostPart = hostPart + ":" + port
	}

	d := net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if useTLS {
		conn, err = tls.DialWithDialer(&d, "tcp", hostPart, &tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = d.Dial("tcp", hostPart)
	}
	if err != nil {
		return nil, err
	}

	keyRaw := make([]byte, 16)
	rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, hostPart)
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	// A User-Agent is mandatory on some gateways (DashScope rejects
	// bare clients with 401).
	b.WriteString("User-Agent: gvos/0.1\r\n\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake read: %w", err)
	}
	if !strings.Contains(line, "101") {
		conn.Close()
		return nil, fmt.Errorf("handshake rejected: %s", strings.TrimSpace(line))
	}
	// Drain headers; validate the accept key.
	accept := ""
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if h == "\r\n" {
			break
		}
		if lower := strings.ToLower(h); strings.HasPrefix(lower, "sec-websocket-accept:") {
			accept = strings.TrimSpace(h[len("Sec-WebSocket-Accept:"):])
		}
	}
	want := wsClientAccept(key)
	if accept != want {
		conn.Close()
		return nil, fmt.Errorf("accept key mismatch: %s != %s", accept, want)
	}
	return &WSConn{conn: conn, br: br}, nil
}

// AcceptKey computes the server Sec-WebSocket-Accept value for a key.
func AcceptKey(key string) string { return wsClientAccept(key) }

func wsClientAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Read reads one frame (server frames are unmasked).
func (c *WSConn) Read() (Frame, error) { return ReadFrame(c.br) }

// Write sends a masked client frame.
func (c *WSConn) Write(opcode int, payload []byte) error {
	return WriteClientFrame(c.conn, opcode, payload)
}

// Close closes the connection.
func (c *WSConn) Close() error { return c.conn.Close() }

// SetReadDeadline passes through to the connection.
func (c *WSConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// WriteClientFrame writes a masked (client-side) frame.
func WriteClientFrame(w io.Writer, opcode int, payload []byte) error {
	n := len(payload)
	h := make([]byte, 0, 14)
	h = append(h, byte(0x80|opcode))
	if n < 126 {
		h = append(h, byte(0x80|n))
	} else if n <= 0xFFFF {
		h = append(h, byte(0x80|126), 0, 0)
		binary.BigEndian.PutUint16(h[len(h)-2:], uint16(n))
	} else {
		h = append(h, byte(0x80|127))
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		h = append(h, ext[:]...)
	}
	var mask [4]byte
	rand.Read(mask[:])
	h = append(h, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.Write(h); err != nil {
		return err
	}
	if n > 0 {
		_, err := w.Write(masked)
		return err
	}
	return nil
}

var _ = http.MethodGet // keep net/http imported for docs parity
