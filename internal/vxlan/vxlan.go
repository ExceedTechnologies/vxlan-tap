// Package vxlan implements the RFC 7348 VXLAN header.
package vxlan

import "errors"

// HeaderLen is the size of the VXLAN header in bytes.
const HeaderLen = 8

// DefaultPort is the IANA-assigned VXLAN UDP port.
const DefaultPort = 4789

// MaxVNI is the largest valid 24-bit VXLAN Network Identifier.
const MaxVNI = 1<<24 - 1

const flagI = 0x08 // "VNI present" flag

var (
	ErrShort  = errors.New("vxlan: packet shorter than header")
	ErrNoVNI  = errors.New("vxlan: I flag not set")
	ErrBadVNI = errors.New("vxlan: VNI exceeds 24 bits")
)

// Encode writes a VXLAN header carrying vni into dst[:HeaderLen].
func Encode(dst []byte, vni uint32) error {
	if len(dst) < HeaderLen {
		return ErrShort
	}
	if vni > MaxVNI {
		return ErrBadVNI
	}
	dst[0] = flagI
	dst[1], dst[2], dst[3] = 0, 0, 0
	dst[4] = byte(vni >> 16)
	dst[5] = byte(vni >> 8)
	dst[6] = byte(vni)
	dst[7] = 0
	return nil
}

// Decode parses the VXLAN header in b and returns the VNI and the inner
// Ethernet frame. The payload aliases b.
func Decode(b []byte) (vni uint32, payload []byte, err error) {
	if len(b) < HeaderLen {
		return 0, nil, ErrShort
	}
	if b[0]&flagI == 0 {
		return 0, nil, ErrNoVNI
	}
	vni = uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6])
	return vni, b[HeaderLen:], nil
}
