package mediastream

import (
	"bytes"
	"testing"
)

func TestFrameRoundtrip(t *testing.T) {
	cases := [][]byte{
		[]byte("hello"),                // small
		bytes.Repeat([]byte("x"), 200), // 16-bit length
		{},                             // empty
		bytes.Repeat([]byte("y"), 3),   // tiny binary
	}
	for _, payload := range cases {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, OpBinaryValue, payload); err != nil {
			t.Fatal(err)
		}
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read %d bytes: %v", len(payload), err)
		}
		if !f.FIN || f.Opcode != OpBinaryValue {
			t.Errorf("header: fin=%v op=%d", f.FIN, f.Opcode)
		}
		if !bytes.Equal(f.Payload, payload) {
			t.Errorf("payload mismatch: %d vs %d bytes", len(f.Payload), len(payload))
		}
	}
}

func TestMaskedClientFrame(t *testing.T) {
	// Client frames are masked per RFC 6455; simulate one.
	payload := []byte("masked-audio")
	mask := [4]byte{0xA7, 0x31, 0x5C, 0x90}

	var buf bytes.Buffer
	buf.WriteByte(0x80 | OpBinaryValue) // FIN + binary
	n := len(payload)
	buf.WriteByte(0x80 | byte(n)) // MASK + len (<126)
	buf.Write(mask[:])
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	buf.Write(masked)

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("unmask failed: %q", f.Payload)
	}
}

func TestPingControlFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x80 | OpPingValue)
	buf.WriteByte(4)
	buf.WriteString("ping")

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != OpPingValue || string(f.Payload) != "ping" {
		t.Errorf("ping frame: %+v", f)
	}
}

func TestHugeFrameRejected(t *testing.T) {
	// Claim a 17MB length -> rejected before allocating.
	var buf bytes.Buffer
	buf.WriteByte(0x80 | OpBinaryValue)
	buf.WriteByte(0x80 | 127) // masked + 64-bit length
	var len8 [8]byte
	binary := make([]byte, 8)
	binary[0] = 0x01 // 16MB+1
	buf.Write(len8[:0])
	buf.Write([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("oversized frame must be rejected")
	}
	_ = binary
}
