package chromecast

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire format of the Cast v2 channel: every message is a 4-byte big-endian length followed
// by a CastMessage protobuf (cast_channel.proto). The handful of fields the sender needs are
// encoded and decoded by hand; everything else is skipped. Frames come from an untrusted
// device on the LAN, so sizes are capped and every length is checked before it is used.

// MaxFrameSize caps one framed CastMessage (receivers send at most 64 KiB; larger frames are
// treated as a broken or hostile connection).
const MaxFrameSize = 64 << 10

// Namespaces and well-known ids of the Cast v2 protocol.
const (
	NamespaceConnection = "urn:x-cast:com.google.cast.tp.connection"
	NamespaceHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	NamespaceReceiver   = "urn:x-cast:com.google.cast.receiver"
	NamespaceMedia      = "urn:x-cast:com.google.cast.media"

	// ReceiverID is the platform receiver every sender talks to first.
	ReceiverID = "receiver-0"
	// DefaultMediaReceiverAppID is Google's Default Media Receiver application.
	DefaultMediaReceiverAppID = "CC1AD845"
)

// Payload types (CastMessage.payload_type).
const (
	PayloadString = 0
	PayloadBinary = 1
)

// Message is one CastMessage. ProtocolVersion is always CASTV2_1_0 (0) on the wire.
type Message struct {
	SourceID      string
	DestinationID string
	Namespace     string
	PayloadType   int // PayloadString | PayloadBinary
	PayloadUTF8   string
	PayloadBinary []byte
}

// Protobuf field numbers and wire types of CastMessage.
const (
	fieldProtocolVersion = 1
	fieldSourceID        = 2
	fieldDestinationID   = 3
	fieldNamespace       = 4
	fieldPayloadType     = 5
	fieldPayloadUTF8     = 6
	fieldPayloadBinary   = 7

	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// Codec errors. Decoding never panics: malformed input yields one of these (wrapped).
var (
	ErrFrameSize   = errors.New("chromecast: frame size out of range")
	ErrMalformed   = errors.New("chromecast: malformed message")
	errVarint      = fmt.Errorf("%w: bad varint", ErrMalformed)
	errTruncated   = fmt.Errorf("%w: truncated field", ErrMalformed)
	errWireType    = fmt.Errorf("%w: unsupported wire type", ErrMalformed)
	errFieldNumber = fmt.Errorf("%w: invalid field number", ErrMalformed)
)

// Marshal encodes m as a CastMessage protobuf (without the frame header).
func (m *Message) Marshal() []byte {
	n := 2 + 2 + // protocol_version, payload_type (required in proto2: always present)
		bytesFieldLen(len(m.SourceID)) + bytesFieldLen(len(m.DestinationID)) + bytesFieldLen(len(m.Namespace))
	if m.PayloadType == PayloadBinary {
		n += bytesFieldLen(len(m.PayloadBinary))
	} else {
		n += bytesFieldLen(len(m.PayloadUTF8))
	}
	b := make([]byte, 0, n)
	b = appendVarintField(b, fieldProtocolVersion, 0)
	b = appendBytesField(b, fieldSourceID, []byte(m.SourceID))
	b = appendBytesField(b, fieldDestinationID, []byte(m.DestinationID))
	b = appendBytesField(b, fieldNamespace, []byte(m.Namespace))
	if m.PayloadType == PayloadBinary {
		b = appendVarintField(b, fieldPayloadType, PayloadBinary)
		b = appendBytesField(b, fieldPayloadBinary, m.PayloadBinary)
	} else {
		b = appendVarintField(b, fieldPayloadType, PayloadString)
		b = appendBytesField(b, fieldPayloadUTF8, []byte(m.PayloadUTF8))
	}
	return b
}

// UnmarshalMessage decodes a CastMessage protobuf. Unknown fields are skipped; groups, invalid
// wire types, field number 0, known fields with the wrong wire type, overlong varints and
// lengths that run past the end of b are errors.
func UnmarshalMessage(b []byte) (*Message, error) {
	m := &Message{}
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return nil, errVarint
		}
		b = b[n:]
		field, wire := key>>3, key&7
		if field == 0 || field > 1<<29-1 {
			return nil, errFieldNumber
		}
		switch wire {
		case wireVarint:
			v, n := uvarint(b)
			if n <= 0 {
				return nil, errVarint
			}
			b = b[n:]
			switch field {
			case fieldProtocolVersion:
				// Only CASTV2_1_0 exists in practice; any value is accepted and ignored.
			case fieldPayloadType:
				if v > PayloadBinary {
					return nil, fmt.Errorf("%w: payload type %d", ErrMalformed, v)
				}
				m.PayloadType = int(v)
			case fieldSourceID, fieldDestinationID, fieldNamespace, fieldPayloadUTF8, fieldPayloadBinary:
				return nil, fmt.Errorf("%w: field %d is not a varint", ErrMalformed, field)
			}
		case wireBytes:
			l, n := uvarint(b)
			if n <= 0 {
				return nil, errVarint
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nil, errTruncated
			}
			v := b[:l]
			b = b[l:]
			switch field {
			case fieldSourceID:
				m.SourceID = string(v)
			case fieldDestinationID:
				m.DestinationID = string(v)
			case fieldNamespace:
				m.Namespace = string(v)
			case fieldPayloadUTF8:
				m.PayloadUTF8 = string(v)
			case fieldPayloadBinary:
				m.PayloadBinary = append([]byte(nil), v...)
			case fieldProtocolVersion, fieldPayloadType:
				return nil, fmt.Errorf("%w: field %d is not length-delimited", ErrMalformed, field)
			}
		case wireFixed64, wireFixed32:
			size := 8
			if wire == wireFixed32 {
				size = 4
			}
			if field <= fieldPayloadBinary {
				return nil, fmt.Errorf("%w: field %d has a fixed-size wire type", ErrMalformed, field)
			}
			if len(b) < size {
				return nil, errTruncated
			}
			b = b[size:]
		default: // 3/4 = groups (deprecated, never used by Cast), 6/7 = invalid
			return nil, errWireType
		}
	}
	return m, nil
}

// WriteMessage writes m as one frame. The frame is assembled first so it goes out in a
// single Write (callers serialise writers).
func WriteMessage(w io.Writer, m *Message) error {
	body := m.Marshal()
	if len(body) == 0 || len(body) > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameSize, len(body))
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	_, err := w.Write(frame)
	return err
}

// ReadMessage reads one frame from r. A length of 0 or above MaxFrameSize is ErrFrameSize
// (the stream cannot be resynchronised after that, so callers drop the connection).
func ReadMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameSize, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return UnmarshalMessage(body)
}

// uvarint decodes a base-128 varint of at most 10 bytes. n is the number of bytes read, or
// ≤ 0 when b is truncated or the value overflows 64 bits.
func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < binary.MaxVarintLen64; i++ {
		c := b[i]
		if i == binary.MaxVarintLen64-1 && c > 1 {
			return 0, -1 // the 10th byte may only carry the 64th bit
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func appendVarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func appendVarintField(b []byte, field int, v uint64) []byte {
	return appendVarint(appendVarint(b, uint64(field)<<3|wireVarint), v)
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	b = appendVarint(b, uint64(field)<<3|wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

// bytesFieldLen is the encoded size of a length-delimited field with a one-byte key.
func bytesFieldLen(n int) int { return 1 + varintLen(uint64(n)) + n }

func varintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
