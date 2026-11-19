package mediastream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDialWSRoundtrip: a tiny WS server on httptest, dial it with
// DialWS, send a masked frame the server reads with ReadFrame, and
// read an unmasked server frame with Read.
func TestDialWSRoundtrip(t *testing.T) {
	upgraded := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "custom" {
			http.Error(w, "missing header", 400)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, rw, _ := hj.Hijack()
		defer conn.Close()
		resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsClientAccept(key) + "\r\n\r\n"
		rw.WriteString(resp)
		rw.Flush()
		upgraded <- "ok"

		// Read one masked client frame, echo it back as a server frame.
		f, err := ReadFrame(rw.Reader)
		if err != nil {
			return
		}
		WriteFrame(conn, OpBinaryValue, f.Payload)
	}))
	defer srv.Close()

	url := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	c, err := DialWS(url, map[string]string{"X-Test": "custom"}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v := <-upgraded; v != "ok" {
		t.Fatal("server not upgraded")
	}

	if err := c.Write(OpBinaryValue, []byte("ping-echo")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != "ping-echo" {
		t.Fatalf("echo mismatch: %q", f.Payload)
	}
}

// TestDialWSAcceptMismatch: a server with a bad accept key is rejected.
func TestDialWSAcceptMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, rw, _ := hj.Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: bogusvalue\r\n\r\n")
		rw.Flush()
	}))
	defer srv.Close()
	url := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	if _, err := DialWS(url, nil, 3*time.Second); err == nil {
		t.Fatal("bad accept key must be rejected")
	}
}

// TestClientFrameLarge: 16-bit length path for masked frames.
func TestClientFrameLarge(t *testing.T) {
	payload := make([]byte, 300)
	var buf memoryBuffer
	if err := WriteClientFrame(&buf, OpBinaryValue, payload); err != nil {
		t.Fatal(err)
	}
	// Read it back: our ReadFrame unmasks (test via net.Pipe-free path)
	var raw = buf.b
	// parse header manually: opcode + len126
	if raw[1]&0x7F != 126 {
		t.Fatalf("expected 126 length marker, got %d", raw[1]&0x7F)
	}
	_ = json.Marshal
}

type memoryBuffer struct{ b []byte }

func (m *memoryBuffer) Write(p []byte) (int, error) {
	m.b = append(m.b, p...)
	return len(p), nil
}
