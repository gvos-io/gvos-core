package api

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fastvoip/gvos-core/internal/mediamain/mediastream"
)

// TestMediaStreamEndToEnd: a raw TCP client upgrades to WebSocket,
// receives both directions of a live call's audio, injects TTS audio
// (visible to uplink subscribers), and reads the echo RTT telemetry.
func TestMediaStreamEndToEnd(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	// The call goes live (billing path creates it implicitly - the
	// media plane keys by call id).
	if code, m := postStart(t, srv, "media-e2e-1", "8613800138000"); code != 200 {
		t.Fatalf("call start: %d %v", code, m)
	}

	// WebSocket client handshake.
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	key := make([]byte, 16)
	rand.Read(key)
	keyB64 := base64.StdEncoding.EncodeToString(key)
	req := "GET /v1/stream/media-e2e-1 HTTP/1.1\r\n" +
		"Host: t\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + keyB64 + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	// Read the 101 response line (headers validation: accept key).
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !bytes.Contains([]byte(line), []byte("101")) {
		t.Fatalf("handshake: %q err=%v", line, err)
	}
	var acceptLine string
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if h == "\r\n" {
			break
		}
		if bytes.HasPrefix(bytes.ToLower([]byte(h)), []byte("sec-websocket-accept:")) {
			acceptLine = h
		}
	}
	want := wsAccept(keyB64)
	got := bytes.TrimSpace(bytes.SplitN([]byte(acceptLine), []byte(": "), 2)[1])
	if string(got) != want {
		t.Fatalf("accept key: %s want %s", got, want)
	}

	// Telephony side pushes both directions. Small settle wait: the
	// server-side pump goroutine subscribes right after the handshake
	// flush; this pushes after subscription is in place.
	stream := s.media.Get("media-e2e-1")
	if stream == nil {
		t.Fatal("stream not open")
	}
	time.Sleep(150 * time.Millisecond)
	stream.Push(mediastream.Downlink, []byte("net-audio"))

	// Client should receive the tagged downlink chunk (header 0x01).
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := mediastream.ReadFrame(br)
	if err != nil {
		t.Fatalf("recv downlink: %v", err)
	}
	if f.Opcode != mediastream.OpBinaryValue || len(f.Payload) < 1 || f.Payload[0] != 1 ||
		string(f.Payload[1:]) != "net-audio" {
		t.Fatalf("downlink frame: %+v %q", f, f.Payload)
	}

	// Client injects TTS audio (masked binary frame).
	if err := writeClientFrame(conn, mediastream.OpBinaryValue, []byte("tts-audio")); err != nil {
		t.Fatal(err)
	}
	// It lands on the uplink - a second local subscriber sees it.
	var upGot []byte
	recv := make(chan []byte, 1)
	stream.Subscribe(mediastream.Uplink, mediastream.SubscriberFunc(func(c mediastream.Chunk) {
		select {
		case recv <- c.Data:
		default:
		}
	}))
	// The injected chunk was fanned out before we subscribed; verify
	// through stats instead: uplink chunk count advanced.
	for i := 0; i < 50 && stream.Stats().UplinkChunks == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if stream.Stats().UplinkChunks == 0 {
		t.Fatal("injected audio never reached the uplink")
	}

	// The injected TTS audio echoes back to us first (we subscribe to
	// uplink too): consume it before requesting the RTT telemetry.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	echoChunk, err := mediastream.ReadFrame(br)
	if err != nil {
		t.Fatalf("recv injected echo: %v", err)
	}
	if echoChunk.Opcode != mediastream.OpBinaryValue || len(echoChunk.Payload) < 1 ||
		echoChunk.Payload[0] != 0 || string(echoChunk.Payload[1:]) != "tts-audio" {
		t.Fatalf("injected echo frame: %+v %q", echoChunk, echoChunk.Payload)
	}

	// Echo telemetry: text frame "echo" -> binary RTT frame.
	if err := writeClientFrame(conn, mediastream.OpTextValue, []byte("echo")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f2, err := mediastream.ReadFrame(br)
	if err != nil {
		t.Fatalf("recv rtt: %v", err)
	}
	if f2.Opcode != mediastream.OpBinaryValue || len(f2.Payload) != 9 || f2.Payload[0] > 3 {
		t.Fatalf("rtt frame: %+v %q", f2, f2.Payload)
	}
	rttMs := int64(binary.BigEndian.Uint64(f2.Payload[1:]))
	if rttMs < 0 || rttMs > 5000 {
		t.Errorf("rtt out of sane range: %d", rttMs)
	}
	_ = upGot
}

// TestMediaStreamClosesWithCall: the call end event tears the stream.
func TestMediaStreamClosesWithCall(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	postStart(t, srv, "media-e2e-2", "8613800138000")
	if s.media.Get("media-e2e-2") == nil {
		t.Fatal("stream not open")
	}
	postEnd(t, srv, "media-e2e-2", 2)
	if s.media.Get("media-e2e-2") != nil {
		t.Fatal("stream must close with the call")
	}
}

// writeClientFrame sends a masked client frame.
func writeClientFrame(conn net.Conn, opcode int, payload []byte) error {
	var hdr []byte
	n := len(payload)
	hdr = append(hdr, byte(0x80|opcode), byte(0x80|n))
	var mask [4]byte
	for i := range mask {
		mask[i] = byte(rand.Intn(256))
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := conn.Write(append(hdr, masked...))
	return err
}

func postStart(t *testing.T, srv *httptest.Server, callID, dest string) (int, map[string]any) {
	t.Helper()
	return postJSONpath(t, srv, "/v1/call/start", map[string]string{
		"call_id": callID, "from_user": "1000", "to_user": dest,
	})
}

func postEnd(t *testing.T, srv *httptest.Server, callID string, dur int) (int, map[string]any) {
	t.Helper()
	return postJSONpath(t, srv, "/v1/call/end", map[string]any{
		"call_id": callID, "duration": dur,
	})
}

func postJSONpath(t *testing.T, srv *httptest.Server, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}
