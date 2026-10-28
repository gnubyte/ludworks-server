package airplay

import (
	"errors"
	"fmt"
)

// TLV8 (HAP pairing messages): one type byte, one length byte and up to 255 value bytes.
// Longer values are split into consecutive items of the same type, every fragment but the
// last exactly 255 bytes long.

// HAP pairing TLV types.
const (
	tlvMethod        = 0x00
	tlvIdentifier    = 0x01
	tlvSalt          = 0x02
	tlvPublicKey     = 0x03
	tlvProof         = 0x04
	tlvEncryptedData = 0x05
	tlvState         = 0x06
	tlvError         = 0x07
	tlvRetryDelay    = 0x08
	tlvSignature     = 0x0A
	tlvFlags         = 0x13
)

// HAP pairing error codes (tlvError).
const (
	tlvErrUnknown        = 0x01
	tlvErrAuthentication = 0x02
	tlvErrBackoff        = 0x03
	tlvErrMaxPeers       = 0x04
	tlvErrMaxTries       = 0x05
	tlvErrUnavailable    = 0x06
	tlvErrBusy           = 0x07
)

var errTLV = errors.New("airplay: malformed TLV8")

type tlvItem struct {
	typ byte
	val []byte
}

// encodeTLV serialises items in order, fragmenting values longer than 255 bytes. An empty
// value is written as a zero-length item.
func encodeTLV(items ...tlvItem) []byte {
	var out []byte
	for _, it := range items {
		v := it.val
		for {
			n := min(len(v), 255)
			out = append(out, it.typ, byte(n))
			out = append(out, v[:n]...)
			v = v[n:]
			if len(v) == 0 {
				break
			}
		}
	}
	return out
}

// decodeTLV parses a flat TLV8 message into a map, joining fragments. A type that appears
// again after a different type (lists, unused by pairing) is rejected.
func decodeTLV(b []byte) (map[byte][]byte, error) {
	out := map[byte][]byte{}
	last, lastLen := -1, 0
	for len(b) > 0 {
		if len(b) < 2 || int(b[1]) > len(b)-2 {
			return nil, fmt.Errorf("%w: truncated item", errTLV)
		}
		t, n := b[0], int(b[1])
		v := b[2 : 2+n]
		b = b[2+n:]
		if int(t) == last && lastLen == 255 {
			out[t] = append(out[t], v...)
		} else if _, dup := out[t]; dup {
			return nil, fmt.Errorf("%w: repeated type 0x%02x", errTLV, t)
		} else {
			out[t] = append([]byte{}, v...)
		}
		last, lastLen = int(t), n
	}
	return out, nil
}
