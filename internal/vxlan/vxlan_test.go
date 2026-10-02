package vxlan

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeKnownBytes(t *testing.T) {
	buf := make([]byte, HeaderLen)
	if err := Encode(buf, 0x123456); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x08, 0, 0, 0, 0x12, 0x34, 0x56, 0}
	if !bytes.Equal(buf, want) {
		t.Fatalf("got % x, want % x", buf, want)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, vni := range []uint32{0, 1, 100, 4096, MaxVNI} {
		frame := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 0x08, 0x00}
		pkt := make([]byte, HeaderLen+len(frame))
		if err := Encode(pkt, vni); err != nil {
			t.Fatal(err)
		}
		copy(pkt[HeaderLen:], frame)
		got, payload, err := Decode(pkt)
		if err != nil {
			t.Fatalf("vni %d: %v", vni, err)
		}
		if got != vni || !bytes.Equal(payload, frame) {
			t.Fatalf("vni %d: got vni %d payload % x", vni, got, payload)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		fn   func() error
	}{
		{"encode short", ErrShort, func() error { return Encode(make([]byte, 7), 1) }},
		{"encode big vni", ErrBadVNI, func() error { return Encode(make([]byte, 8), MaxVNI+1) }},
		{"decode short", ErrShort, func() error { _, _, err := Decode(make([]byte, 7)); return err }},
		{"decode no I flag", ErrNoVNI, func() error { _, _, err := Decode(make([]byte, 8)); return err }},
	}
	for _, tt := range tests {
		if err := tt.fn(); !errors.Is(err, tt.err) {
			t.Errorf("%s: got %v, want %v", tt.name, err, tt.err)
		}
	}
}
