package chromecast

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// golden is CastMessage{sender-0 → receiver-0, ns "urn:x", PING} encoded by hand from
// cast_channel.proto (field order, proto2 required enums present even when 0).
var golden = []byte{
	0x08, 0x00, // protocol_version = CASTV2_1_0
	0x12, 0x08, 's', 'e', 'n', 'd', 'e', 'r', '-', '0',
	0x1a, 0x0a, 'r', 'e', 'c', 'e', 'i', 'v', 'e', 'r', '-', '0',
	0x22, 0x05, 'u', 'r', 'n', ':', 'x',
	0x28, 0x00, // payload_type = STRING
	0x32, 0x0f, '{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 'P', 'I', 'N', 'G', '"', '}',
}

func TestMarshalGolden(t *testing.T) {
	m := &Message{SourceID: "sender-0", DestinationID: "receiver-0", Namespace: "urn:x", PayloadUTF8: `{"type":"PING"}`}
	if got := m.Marshal(); !bytes.Equal(got, golden) {
		t.Fatalf("marshal:\n got % x\nwant % x", got, golden)
	}
	back, err := UnmarshalMessage(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMessage(back, m) {
		t.Fatalf("unmarshal: %+v", back)
	}
}

func TestRoundTrip(t *testing.T) {
	long := strings.Repeat("x", 40000) // multi-byte length varint
	for _, m := range []*Message{
		{},
		{SourceID: "s", DestinationID: "d", Namespace: NamespaceMedia, PayloadUTF8: `{"type":"LOAD","requestId":7}`},
		{SourceID: "sender-123", DestinationID: "*", Namespace: NamespaceReceiver, PayloadUTF8: long},
		{SourceID: "a", DestinationID: "b", Namespace: "urn:x-cast:bin", PayloadType: PayloadBinary, PayloadBinary: []byte{0, 1, 2, 0xff}},
		{SourceID: "ünïcode", DestinationID: "receiver-0", Namespace: NamespaceHeartbeat, PayloadUTF8: `{"type":"PONG"}`},
	} {
		got, err := UnmarshalMessage(m.Marshal())
		if err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
		if got.SourceID != m.SourceID || got.DestinationID != m.DestinationID || got.Namespace != m.Namespace ||
			got.PayloadType != m.PayloadType || got.PayloadUTF8 != m.PayloadUTF8 || !bytes.Equal(got.PayloadBinary, m.PayloadBinary) {
			t.Fatalf("round trip: got %+v want %+v", got, m)
		}
	}
}

func TestUnmarshalSkipsUnknownFields(t *testing.T) {
	b := append([]byte(nil), golden...)
	b = append(b, 0x40, 0x01)                              // field 8 (continued) varint
	b = append(b, 0x48, 0x96, 0x01)                        // field 9 (remaining_length) varint 150
	b = append(b, 0x52, 0x02, 'x', 'y')                    // field 10 bytes
	b = append(b, 0x59, 1, 2, 3, 4, 5, 6, 7, 8)            // field 11 fixed64
	b = append(b, 0x65, 1, 2, 3, 4)                        // field 12 fixed32
	b = append(b, 0xc0, 0x3e, 0x05)                        // field 1000 varint
	b = append(b, 0xfa, 0xff, 0xff, 0xff, 0x0f, 0x01, 'z') // field 2^29-1 bytes
	m, err := UnmarshalMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceID != "sender-0" || m.PayloadUTF8 != `{"type":"PING"}` {
		t.Fatalf("known fields lost: %+v", m)
	}
}

func TestUnmarshalRejects(t *testing.T) {
	overlong := append([]byte{0x08}, bytes.Repeat([]byte{0xff}, 10)...)
	overlong = append(overlong, 0x01)
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"truncated key", []byte{0x80}},
		{"truncated value varint", []byte{0x08, 0x80}},
		{"overlong varint", overlong},
		{"10th varint byte too big", append([]byte{0x08}, append(bytes.Repeat([]byte{0x80}, 9), 0x02)...)},
		{"length past end", []byte{0x12, 0x05, 'a'}},
		{"huge length", []byte{0x12, 0xff, 0xff, 0xff, 0xff, 0x0f}},
		{"start group", []byte{0x43}},
		{"end group", []byte{0x44}},
		{"wire type 6", []byte{0x46}},
		{"wire type 7", []byte{0x47}},
		{"field 0 varint", []byte{0x00, 0x00}},
		{"field 0 bytes", []byte{0x02, 0x00}},
		{"field number too big", []byte{0x80, 0x80, 0x80, 0x80, 0x10, 0x00}},
		{"source as varint", []byte{0x10, 0x01}},
		{"protocol version as bytes", []byte{0x0a, 0x00}},
		{"source as fixed32", []byte{0x15, 1, 2, 3, 4}},
		{"payload type 2", []byte{0x28, 0x02}},
		{"truncated fixed32", []byte{0x4d, 1, 2}},
		{"truncated fixed64", []byte{0x49, 1, 2, 3}},
	} {
		if _, err := UnmarshalMessage(tc.b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", tc.name, err)
		}
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	msgs := []*Message{
		{SourceID: "a", DestinationID: "b", Namespace: "n", PayloadUTF8: "1"},
		{SourceID: "c", DestinationID: "d", Namespace: "m", PayloadUTF8: strings.Repeat("y", 1000)},
	}
	for _, m := range msgs {
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range msgs {
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if !sameMessage(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	if _, err := ReadMessage(&buf); err != io.EOF {
		t.Fatalf("end of stream: %v", err)
	}

	frame := func(n uint32, body []byte) io.Reader {
		b := binary.BigEndian.AppendUint32(nil, n)
		return bytes.NewReader(append(b, body...))
	}
	if _, err := ReadMessage(frame(0, nil)); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("zero frame: %v", err)
	}
	if _, err := ReadMessage(frame(MaxFrameSize+1, nil)); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("oversized frame: %v", err)
	}
	if _, err := ReadMessage(frame(1<<31, nil)); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("huge frame: %v", err)
	}
	if _, err := ReadMessage(frame(10, []byte{0x08})); err != io.ErrUnexpectedEOF {
		t.Fatalf("short body: %v", err)
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0})); err != io.ErrUnexpectedEOF {
		t.Fatalf("short header: %v", err)
	}
	if _, err := ReadMessage(frame(2, []byte{0x43, 0x00})); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed body: %v", err)
	}
	big := &Message{PayloadUTF8: strings.Repeat("z", MaxFrameSize)}
	if err := WriteMessage(io.Discard, big); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("writing an oversized frame: %v", err)
	}
}

func TestUvarint(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 32, 1<<63 - 1, 1 << 63, ^uint64(0)} {
		b := binary.AppendUvarint(nil, v)
		got, n := uvarint(b)
		if got != v || n != len(b) {
			t.Fatalf("%d: got %d (%d bytes), want %d bytes", v, got, n, len(b))
		}
		if varintLen(v) != len(b) {
			t.Fatalf("varintLen(%d) = %d, want %d", v, varintLen(v), len(b))
		}
	}
}

func sameMessage(a, b *Message) bool {
	return a.SourceID == b.SourceID && a.DestinationID == b.DestinationID && a.Namespace == b.Namespace &&
		a.PayloadType == b.PayloadType && a.PayloadUTF8 == b.PayloadUTF8 && bytes.Equal(a.PayloadBinary, b.PayloadBinary)
}
