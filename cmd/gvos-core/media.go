// AI media-plane endpoint: /v1/stream/{call_id} upgrades to
// WebSocket, subscribes the peer to both directions of the call's
// media stream, forwards injected uplink audio, and answers pings.
//
// Wire protocol (all binary frames, length-prefixed by WS itself):
//
//	client->server: raw audio bytes  -> Inject (TTS speaking to caller)
//	server->client: 1-byte header + audio:
//	    header[0] == 0 -> uplink chunk   (caller audio, ASR input)
//	    header[0] == 1 -> downlink chunk (network audio, what caller hears)
//	client->server text frame "echo" -> RTT sample against the budget
package main

import (
	"bufio"
	"encoding/binary"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gvos/gvos-core/internal/mediastream"
)

// handleMediaStream upgrades and runs the media loop.
func (s *Server) handleMediaStream(w http.ResponseWriter, r *http.Request) {
	callID := strings.TrimPrefix(r.URL.Path, "/v1/stream/")
	if callID == "" {
		httpError(w, 400, "call_id required in path")
		return
	}
	if s.media == nil {
		httpError(w, 503, "media plane unavailable")
		return
	}

	// Handshake (RFC 6455): require the upgrade headers.
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		httpError(w, 400, "websocket upgrade required")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		httpError(w, 500, "hijack unsupported")
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer conn.Close()

	accept := wsAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		return
	}
	_ = rw.Flush()

	s.runMediaLoop(conn, rw.Reader, callID)
}

func wsAccept(key string) string { return mediastream.AcceptKey(key) }

// runMediaLoop is the per-connection media pump.
func (s *Server) runMediaLoop(conn net.Conn, reader *bufio.Reader, callID string) {
	stream := s.media.Open(callID)
	defer func() {
		// Last-writer-closes: the stream stays open for other peers;
		// it is closed when the call ends (event path).
		conn.Close()
	}()

	// Fan-out pump: chunks from both directions push to the socket.
	// ready gates the caller until the pump is subscribed so no early
	// chunks race the subscription.
	done := make(chan struct{})
	ready := make(chan struct{})
	go s.mediaPump(conn, stream, done, ready)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		return
	}

	// Reader loop: injected audio + control frames.
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		f, err := mediastream.ReadFrame(reader)
		if err != nil {
			return
		}
		switch f.Opcode {
		case mediastream.OpPingValue:
			_ = mediastream.WriteFrame(conn, mediastream.OpPongValue, f.Payload)
		case mediastream.OpCloseValue:
			_ = mediastream.WriteFrame(conn, mediastream.OpCloseValue, nil)
			return
		case mediastream.OpTextValue:
			if string(f.Payload) == "echo" {
				rtt, within := stream.Echo()
				var hdr [9]byte
				hdr[0] = 2 // RTT telemetry frame
				binary.BigEndian.PutUint64(hdr[1:], uint64(rtt.Milliseconds()))
				if !within {
					hdr[0] = 3 // over-budget telemetry
				}
				_ = mediastream.WriteFrame(conn, mediastream.OpBinaryValue, hdr[:])
			}
		case mediastream.OpBinaryValue:
			if len(f.Payload) > 0 {
				stream.Inject(f.Payload)
			}
		}
	}
}

// mediaPump forwards stream chunks to the socket with direction tags.
func (s *Server) mediaPump(conn net.Conn, stream *mediastream.Stream, done, ready chan struct{}) {
	type tagged struct {
		dir  byte
		data []byte
	}
	out := make(chan tagged, 256)

	unsubUp := stream.Subscribe(mediastream.Uplink, mediastream.SubscriberFunc(
		func(c mediastream.Chunk) {
			select {
			case out <- tagged{0, c.Data}:
			default: // slow consumer: drop rather than block the media path
			}
		}))
	defer unsubUp()
	unsubDown := stream.Subscribe(mediastream.Downlink, mediastream.SubscriberFunc(
		func(c mediastream.Chunk) {
			select {
			case out <- tagged{1, c.Data}:
			default:
			}
		}))
	defer unsubDown()
	close(ready)

	for {
		select {
		case <-done:
			return
		case tg := <-out:
			buf := make([]byte, 1+len(tg.data))
			buf[0] = tg.dir
			copy(buf[1:], tg.data)
			if err := mediastream.WriteFrame(conn, mediastream.OpBinaryValue, buf); err != nil {
				log.Printf("MEDIA-PUMP write error: %v", err)
				return
			}
		}
	}
}
