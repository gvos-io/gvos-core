// Minimal RFC 6455 WebSocket server-side framing - just enough for
// the AI media plane (text handshake headers are handled by the
// endpoint; this file implements frame encode/decode for binary
// payloads, which is all the media stream needs).
//
// Deliberately dependency-free: the media-plane protocol carries
// length-prefixed binary audio frames and small JSON control frames;
// no fragmentation extensions, no compression.
package mediastream

import (
	"encoding/binary"
	"errors"
	"io"
)

// Opcode constants (RFC 6455 §5.2). Exported for endpoint integrations.
const (
	OpContinuationValue = 0x0
	OpTextValue         = 0x1
	OpBinaryValue       = 0x2
	OpCloseValue        = 0x8
	OpPingValue         = 0x9
	OpPongValue         = 0xA
)

// Frame is one decoded WebSocket frame.
type Frame struct {
	FIN     bool
	Opcode  int
	Payload []byte
}

// ReadFrame reads one frame from the reader. Control frames are
// returned as-is; the caller (endpoint loop) answers pings and honors
// closes.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	fin := hdr[0]&0x80 != 0
	opcode := int(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > 16<<20 { // 16MB sanity cap
		return Frame{}, errors.New("frame too large")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return Frame{}, err
		}
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return Frame{FIN: fin, Opcode: opcode, Payload: payload}, nil
}

// WriteFrame writes one server frame (server frames are unmasked).
func WriteFrame(w io.Writer, opcode int, payload []byte) error {
	var hdr []byte
	fin := byte(0x80) | byte(opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = []byte{fin, byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{fin, 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0] = fin
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if n > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}
