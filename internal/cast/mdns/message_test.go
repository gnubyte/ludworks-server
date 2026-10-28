package mdns

import (
	"bytes"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// nameBuf is a message whose name area starts at offset 12 (after a zeroed header).
func nameBuf(b ...byte) []byte { return append(make([]byte, headerLen), b...) }

func label(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func TestReadName(t *testing.T) {
	long63 := strings.Repeat("a", 63)
	// A chain of n backward pointers ending at the name "x" at offset 12: offset 15 points to
	// 12, 17 to 15, 19 to 17, … Reading the last pointer follows n+1 pointers.
	chain := func(n int) ([]byte, int) {
		b := nameBuf(1, 'x', 0, 0xc0, 12)
		for i := 1; i <= n; i++ {
			prev := 15 + 2*(i-1)
			b = append(b, 0xc0, byte(prev))
		}
		return b, len(b) - 2
	}
	chain63, start63 := chain(63)
	chain64, start64 := chain(64)
	var exact255 []byte // 3×63 + 61 byte labels: 3·64 + 62 + 1 (root) = 255
	for i := 0; i < 3; i++ {
		exact255 = append(exact255, label(long63)...)
	}
	exact255 = append(exact255, label(strings.Repeat("b", 61))...)
	tooLong := append(append([]byte{}, exact255...), label("c")...)

	tests := []struct {
		name    string
		buf     []byte
		off     int
		want    []string
		wantEnd int
		err     error
	}{
		{"plain", nameBuf(append(append(label("abc"), label("local")...), 0)...), 12, []string{"abc", "local"}, 23, nil},
		{"root only", nameBuf(0), 12, nil, 13, nil},
		{"backward pointer", nameBuf(append(append(label("local"), 0), append(label("tv"), 0xc0, 12)...)...), 19,
			[]string{"tv", "local"}, 24, nil},
		{"pointer to pointer", nameBuf(1, 'a', 0, 0xc0, 12, 0xc0, 15), 17, []string{"a"}, 19, nil},
		{"forward pointer", nameBuf(0xc0, 14, 1, 'a', 0), 12, nil, 0, errPointer},
		{"self pointer", nameBuf(0xc0, 12), 12, nil, 0, errPointer},
		{"two-node loop from second", nameBuf(0xc0, 14, 0xc0, 12), 14, nil, 0, errPointer},
		{"two-node loop from first", nameBuf(0xc0, 14, 0xc0, 12), 12, nil, 0, errPointer},
		{"pointer into own segment", nameBuf(1, 'a', 0xc0, 12), 12, nil, 0, errPointer},
		{"64 jumps", chain63, start63, []string{"x"}, start63 + 2, nil},
		{"65 jumps", chain64, start64, nil, 0, errPointer},
		{"label type 0x40", nameBuf(0x41, 'a', 0), 12, nil, 0, errLabelType},
		{"label type 0x80", nameBuf(0x81, 'a', 0), 12, nil, 0, errLabelType},
		{"name of 255 bytes", nameBuf(append(append([]byte{}, exact255...), 0)...), 12,
			[]string{long63, long63, long63, strings.Repeat("b", 61)}, 12 + 255, nil},
		{"name of 257 bytes", nameBuf(append(tooLong, 0)...), 12, nil, 0, errNameLong},
		{"label past end", nameBuf(5, 'a', 'b'), 12, nil, 0, errShort},
		{"no root", nameBuf(1, 'a'), 12, nil, 0, errShort},
		{"half a pointer", nameBuf(0xc0), 12, nil, 0, errShort},
		{"offset past end", nameBuf(), 12, nil, 0, errShort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, end, err := readName(tc.buf, tc.off)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, tc.want) || end != tc.wantEnd {
				t.Fatalf("got %q end %d, want %q end %d", got, end, tc.want, tc.wantEnd)
			}
		})
	}
}

func TestParseRealisticResponses(t *testing.T) {
	for _, compress := range []bool{false, true} {
		m, err := parseMessage(chromecastResponse(compress))
		if err != nil || m == nil {
			t.Fatalf("compress=%v: %v", compress, err)
		}
		if m.flags&flagQR == 0 || len(m.questions) != 0 || len(m.records) != 4 {
			t.Fatalf("compress=%v: flags %x, %d questions, %d records", compress, m.flags, len(m.questions), len(m.records))
		}
		ptr, txt, srv, a := m.records[0], m.records[1], m.records[2], m.records[3]
		if ptr.rtype != typePTR || !labelsEqual(ptr.name, svcCast) || !labelsEqual(ptr.target, ccInstance) || ptr.ttl != 120 || ptr.flush {
			t.Fatalf("ptr %+v", ptr)
		}
		if txt.rtype != typeTXT || !txt.flush || txt.ttl != 4500 || len(txt.txt) != 12 || string(txt.txt[6]) != "fn=Living Room TV" {
			t.Fatalf("txt %+v", txt)
		}
		if srv.rtype != typeSRV || srv.port != 8009 || !labelsEqual(srv.target, ccHost) || !srv.flush {
			t.Fatalf("srv %+v", srv)
		}
		if a.rtype != typeA || a.addr != netip.MustParseAddr("192.168.1.20") || !labelsEqual(a.name, ccHost) {
			t.Fatalf("a %+v", a)
		}
	}
	m, err := parseMessage(appleResponse(true))
	if err != nil || len(m.records) != 9 {
		t.Fatalf("apple: %v, %d records", err, len(m.records))
	}
	if r := m.records[1]; r.rtype != typePTR || r.target[0] != "AABBCCDDEEFF@Living Room" {
		t.Fatalf("raop ptr %+v", r)
	}
	if r := m.records[7]; r.rtype != typeAAAA || r.addr != netip.MustParseAddr("fe80::1") {
		t.Fatalf("aaaa %+v", r)
	}
}

func TestParseMessageErrors(t *testing.T) {
	good := chromecastResponse(true)
	countLie := append([]byte(nil), good...)
	countLie[7] = 200 // ANCOUNT 200 cannot fit
	truncated := good[:len(good)-3]

	tests := []struct {
		name     string
		pkt      []byte
		err      error
		nilMsg   bool
		nRecords int
	}{
		{"empty", nil, errShort, true, 0},
		{"short header", make([]byte, 11), errShort, true, 0},
		{"too large", make([]byte, maxPacket+1), errTooLarge, true, 0},
		{"max size", append(make([]byte, headerLen), make([]byte, maxPacket-headerLen)...), nil, false, 0},
		{"count lie", countLie, errCounts, true, 0},
		{"truncated rdata", truncated, errShort, false, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseMessage(tc.pkt)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if (m == nil) != tc.nilMsg {
				t.Fatalf("message nil = %v", m == nil)
			}
			if m != nil && len(m.records) != tc.nRecords {
				t.Fatalf("%d records, want %d", len(m.records), tc.nRecords)
			}
		})
	}
}

// Malformed records are skipped individually; the records around them survive.
func TestParseSkipsBadRecords(t *testing.T) {
	host := L("h.local")
	none := func() {}
	p := newResponse(false).
		rr(secAnswer, host, typeA, classIN, 120, none).         // A without data: skipped
		addr(secAnswer, host, "10.0.0.1", 120, false).          // kept
		rr(secAnswer, host, typeAAAA, classIN, 120, none).      // AAAA without data: skipped
		srv(secAnswer, ccInstance, host, 0, 120, false).        // port 0: skipped
		rr(secAnswer, svcCast, typePTR, classIN, 120, none).    // PTR without data: skipped
		rr(secAnswer, ccInstance, typeTXT, classIN, 120, none). // TXT without strings: kept
		rr(secAnswer, host, 47, classIN, 120, none).            // NSEC: ignored
		rr(secAnswer, host, typeA, 3, 120, none).               // class CH: ignored
		srv(secAnswer, ccInstance, host, 8009, 120, false).     // kept
		bytes()
	m, err := parseMessage(p)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []uint16
	for _, r := range m.records {
		kinds = append(kinds, r.rtype)
	}
	if want := []uint16{typeA, typeTXT, typeSRV}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kept %v, want %v", kinds, want)
	}
	if m.records[0].addr != netip.MustParseAddr("10.0.0.1") || len(m.records[1].txt) != 0 || m.records[2].port != 8009 {
		t.Fatalf("records %+v", m.records)
	}

	// replaceRData swaps the (empty) rdata of a one-record message for raw bytes.
	replaceRData := func(pkt []byte, rdlength uint16, data ...byte) []byte {
		pkt = append([]byte(nil), pkt[:len(pkt)-2]...)
		pkt = append(pkt, byte(rdlength>>8), byte(rdlength))
		return append(pkt, data...)
	}
	// A TXT string that overruns its rdata (length byte 9, two bytes of data).
	txt := newResponse(false).rr(secAnswer, ccInstance, typeTXT, classIN, 120, none).bytes()
	if m, err := parseMessage(replaceRData(txt, 3, 9, 'a', 'b')); err != nil || len(m.records) != 0 {
		t.Fatalf("overrunning TXT: %v, %+v", err, m)
	}
	// A PTR whose target runs past its rdata (rdlength 2, the name needs 5 bytes).
	ptr := newResponse(false).rr(secAnswer, svcCast, typePTR, classIN, 120, none).bytes()
	if m, err := parseMessage(replaceRData(ptr, 2, 3, 'a', 'b', 'c', 0)); err != nil || len(m.records) != 0 {
		t.Fatalf("overrunning PTR: %v, %+v", err, m)
	}
	// rdlength beyond the end of the message breaks the framing.
	if m, err := parseMessage(replaceRData(ptr, 40, 0)); !errors.Is(err, errShort) || m == nil || len(m.records) != 0 {
		t.Fatalf("rdlength past end: %v, %+v", err, m)
	}
}

func TestParseRecordCap(t *testing.T) {
	p := newResponse(true)
	for i := 0; i < 300; i++ {
		p.addr(secAdditional, L("h.local"), "10.0.0.1", 120, false)
	}
	pkt := p.bytes()
	if len(pkt) > maxPacket {
		t.Fatalf("fixture too large: %d", len(pkt))
	}
	m, err := parseMessage(pkt)
	if err != nil || len(m.records) != maxRecords {
		t.Fatalf("%v, %d records", err, len(m.records))
	}
}

func TestParseFlags(t *testing.T) {
	p := newResponse(false).
		addr(secAnswer, L("h.local"), "10.0.0.1", 0, true).
		ptr(secAnswer, svcCast, ccInstance, 0).
		bytes()
	m, err := parseMessage(p)
	if err != nil || len(m.records) != 2 {
		t.Fatal(err)
	}
	if r := m.records[0]; r.ttl != 0 || !r.flush || r.class != classIN {
		t.Fatalf("goodbye with flush: %+v", r)
	}
	if r := m.records[1]; r.ttl != 0 || r.flush {
		t.Fatalf("ptr goodbye: %+v", r)
	}
}

func TestBuildQuery(t *testing.T) {
	q := buildQuery([]question{
		{name: svcCast, qtype: typePTR, qclass: classIN, qu: true},
		{name: svcAirPlay, qtype: typePTR, qclass: classIN},
		{name: L("Living-Room.local"), qtype: typeAAAA, qclass: classIN},
	})
	if !bytes.Equal(q[:headerLen], []byte{0, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("header % x", q[:headerLen])
	}
	wantStart := append(label("_googlecast"), label("_tcp")...)
	wantStart = append(append(wantStart, label("local")...), 0, 0, typePTR, 0x80, classIN)
	if !bytes.Equal(q[headerLen:headerLen+len(wantStart)], wantStart) {
		t.Fatalf("first question % x", q[headerLen:headerLen+len(wantStart)])
	}
	m, err := parseMessage(q)
	if err != nil || len(m.questions) != 3 || m.flags != 0 {
		t.Fatalf("round trip: %v %+v", err, m)
	}
	if !m.questions[0].qu || m.questions[1].qu || m.questions[2].qu || m.questions[0].qclass != classIN {
		t.Fatalf("QU bits: %+v", m.questions)
	}
	if !labelsEqual(m.questions[2].name, L("living-room.local")) || m.questions[2].qtype != typeAAAA {
		t.Fatalf("question %+v", m.questions[2])
	}
}

func TestTXTMap(t *testing.T) {
	parts := func(s ...string) [][]byte {
		out := make([][]byte, len(s))
		for i, x := range s {
			out[i] = []byte(x)
		}
		return out
	}
	many := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		many = append(many, "k"+strings.Repeat("x", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26))+"=v")
	}
	big := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		big = append(big, string(rune('a'+i%26))+string(rune('a'+i/26))+"="+strings.Repeat("z", 250))
	}
	tests := []struct {
		name string
		in   [][]byte
		want map[string]string
		n    int // when want is nil: expected size
	}{
		{"empty TXT", parts(""), map[string]string{}, 0},
		{"no strings", nil, map[string]string{}, 0},
		{"key only", parts("da"), map[string]string{"da": ""}, 0},
		{"empty value", parts("rm="), map[string]string{"rm": ""}, 0},
		{"equals in value", parts("a=b=c"), map[string]string{"a": "b=c"}, 0},
		{"duplicate first wins", parts("ID=one", "id=two"), map[string]string{"id": "one"}, 0},
		{"key lower-cased", parts("FN=Living Room"), map[string]string{"fn": "Living Room"}, 0},
		{"empty key skipped", parts("=x", "a=1"), map[string]string{"a": "1"}, 0},
		{"bad key skipped", parts("k\x01=1", "k\xff=2", "ok=3"), map[string]string{"ok": "3"}, 0},
		{"non UTF-8 value", parts("v=a\xffb"), map[string]string{"v": "a�b"}, 0},
		{"control chars", parts("v=a\x00b\nc"), map[string]string{"v": "a�b�c"}, 0},
		{"binary value growing past 255", parts("pk=" + strings.Repeat("\xff", 200)), nil, 1},
		{"pair cap", parts(many...), nil, maxTXTPairs},
		{"byte cap", parts(big...), nil, maxTXTBytes / 252},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := txtMap(tc.in)
			if tc.want != nil {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
				return
			}
			if len(got) != tc.n {
				t.Fatalf("%d pairs, want %d", len(got), tc.n)
			}
			total := 0
			for k, v := range got {
				total += len(k) + len(v)
				if len(v) > maxTXTValue || !utf8.ValidString(v) {
					t.Fatalf("value of %q: %d bytes, valid %v", k, len(v), utf8.ValidString(v))
				}
			}
			if total > maxTXTBytes {
				t.Fatalf("%d bytes kept", total)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Living Room TV", "Living Room TV"},
		{"Küche 📺", "Küche 📺"},
		{"a\x00b", "a�b"},
		{"a\u0085b", "a�b"}, // C1 control
		{"\xc3", "�"},
		{strings.Repeat("é", 200), strings.Repeat("é", 127)}, // cut on a rune boundary (254 bytes)
	}
	for _, tc := range tests {
		if got := sanitize(tc.in, 255); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNames(t *testing.T) {
	if got := presentation([]string{"Mr. TV", `a\b`, "x y", "Local"}); got != `mr\.\032tv.a\\b.x\032y.local` {
		t.Errorf("presentation = %q", got)
	}
	if got := presentation([]string{"\x00\xff"}); got != `\000\255` {
		t.Errorf("presentation = %q", got)
	}
	if !labelsEqual(L("_GoogleCast._TCP.Local"), svcCast) || labelsEqual(L("_googlecast._udp.local"), svcCast) ||
		labelsEqual(L("a.local"), L("a.b.local")) {
		t.Error("labelsEqual")
	}
	// ASCII-only folding: "É" and "é" differ.
	if labelsEqual([]string{"É"}, []string{"é"}) {
		t.Error("non-ASCII folded")
	}
	for in, want := range map[string][]string{
		"_googlecast._tcp":      {"_googlecast", "_tcp"},
		"_raop._tcp.":           {"_raop", "_tcp"},
		"":                      nil,
		"a..b":                  nil,
		strings.Repeat("a", 64): nil,
	} {
		if got := splitName(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitName(%q) = %q, want %q", in, got, want)
		}
	}
	if !isLocal(L("h.local")) || !isLocal(L("h.LOCAL")) || isLocal(L("local")) || isLocal(L("h.example.com")) {
		t.Error("isLocal")
	}
}
