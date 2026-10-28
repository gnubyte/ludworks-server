package airplay

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pythonPlist is plistlib.dumps(sample, fmt=FMT_BINARY) from Python 3.14 (an independent
// encoder; it shares objects and uses a 2-byte offset table).
const pythonPlist = "62706c6973743030de0102030405060708090a0b0c0d0e0f1011121328292a2b34153536375f1010436f6e74656e742d4c6f636174696f6e5f101653746172742d506f736974696f6e2d5365636f6e6473536269675464617461546d616e79536e6567526e6f526f6b5672616e67657354726174655a73747265616d54797065557469746c6553753136547768656e5f103e687474703a2f2f3139322e3136382e312e323a31383138392f6170692f76312f636173742f6d656469612f746b742f6d656469612f6d312f73747265616d2340290000000000001300000001000000004f1014000102030405060708090a0b0c0d0e0f10111213af10141415161718191a1b1c1d1e1f20212223242526271000100110021003100410051006100710081009100a100b100c100d100e100f101010111012101313fffffffffffffffe0809a22c31d22d2e2f30586475726174696f6e55737461727423409c240000000000230000000000000000d22d2e3233234018000000000000234014000000000000233ff00000000000006700430061006600e90020d83cdfb511ffff3341c84864e000000000080025003800510055005a005f0063006600690070007500800086008a008f00d000d900e200f901100112011401160118011a011c011e01200122012401260128012a012c012e013001320134013601380141014201430146014b0154015a0163016c0171017a0183018c019b019e00000000000002010000000000000038000000000000000000000000000001a7"

func pythonSample() map[string]any {
	data := make([]byte, 20)
	many := make([]any, 20)
	for i := range 20 {
		data[i] = byte(i)
		many[i] = int64(i)
	}
	return map[string]any{
		"Content-Location":       "http://192.168.1.2:18189/api/v1/cast/media/tkt/media/m1/stream",
		"Start-Position-Seconds": 12.5,
		"rate":                   1.0,
		"streamType":             int64(1),
		"big":                    int64(4294967296),
		"neg":                    int64(-2),
		"u16":                    int64(65535),
		"ok":                     true,
		"no":                     false,
		"data":                   data,
		"title":                  "Café 🎵",
		"when":                   time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		"ranges": []any{
			map[string]any{"start": 0.0, "duration": 1801.0},
			map[string]any{"start": 5.0, "duration": 6.0},
		},
		"many": many,
	}
}

func equalPlist(a, b any) bool {
	ta, oka := a.(time.Time)
	tb, okb := b.(time.Time)
	if oka && okb { // dates are float seconds: allow rounding
		d := ta.Sub(tb)
		return d < time.Microsecond && d > -time.Microsecond
	}
	ma, oka := a.(map[string]any)
	mb, okb := b.(map[string]any)
	if oka && okb {
		if len(ma) != len(mb) {
			return false
		}
		for k, v := range ma {
			if !equalPlist(v, mb[k]) {
				return false
			}
		}
		return true
	}
	sa, oka := a.([]any)
	sb, okb := b.([]any)
	if oka && okb {
		if len(sa) != len(sb) {
			return false
		}
		for i := range sa {
			if !equalPlist(sa[i], sb[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestPlistDecodesPythonEncoding(t *testing.T) {
	got, err := decodePlist(mustHex(t, pythonPlist))
	if err != nil {
		t.Fatal(err)
	}
	if !equalPlist(got, pythonSample()) {
		t.Fatalf("decoded %#v", got)
	}
}

func TestPlistRoundTrip(t *testing.T) {
	values := []any{
		pythonSample(),
		map[string]any{},
		[]any{},
		map[string]any{"a": nil, "f32": float32(0.5), "maxi": int64(math.MaxInt64), "mini": int64(math.MinInt64),
			"u": uint64(math.MaxUint64), "uOK": uint64(7), "int": 300, "s": strings.Repeat("x", 300),
			"utf16": strings.Repeat("ü", 20), "date": plistEpoch.Add(-time.Hour)},
		strings.Repeat("ünïcødé", 3),
		[]byte{},
	}
	for i, v := range values {
		b, err := encodePlist(v)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		got, err := decodePlist(b)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		want := normalisePlist(v)
		if !equalPlist(got, want) {
			t.Errorf("%d: got %#v\nwant %#v", i, got, want)
		}
	}
	// Many objects need 2-byte references.
	big := make([]any, 600)
	for i := range big {
		big[i] = int64(i)
	}
	b, err := encodePlist(big)
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-32+7] != 2 {
		t.Errorf("ref size %d with 601 objects", b[len(b)-32+7])
	}
	if got, err := decodePlist(b); err != nil || !equalPlist(got, big) {
		t.Errorf("600-element array: %v", err)
	}
	if _, err := encodePlist(map[string]any{"c": make(chan int)}); err == nil {
		t.Error("encoded a channel")
	}
}

// normalisePlist maps a Go value to what decoding its encoding yields.
func normalisePlist(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case float32:
		return float64(x)
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x)
		}
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = normalisePlist(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalisePlist(e)
		}
		return out
	}
	return v
}

// plistDoc assembles a document from raw objects (offset size 1, ref size 1).
func plistDoc(top int, objs ...[]byte) []byte {
	b := []byte("bplist00")
	var offs []byte
	for _, o := range objs {
		offs = append(offs, byte(len(b)))
		b = append(b, o...)
	}
	table := len(b)
	b = append(b, offs...)
	var tr [32]byte
	tr[6], tr[7] = 1, 1
	binary.BigEndian.PutUint64(tr[8:], uint64(len(objs)))
	binary.BigEndian.PutUint64(tr[16:], uint64(top))
	binary.BigEndian.PutUint64(tr[24:], uint64(table))
	return append(b, tr[:]...)
}

func TestPlistHostile(t *testing.T) {
	cases := map[string][]byte{
		"empty":            nil,
		"no header":        []byte("bplist01" + strings.Repeat("\x00", 40)),
		"self reference":   plistDoc(0, []byte{0xA1, 0}),                  // array containing itself
		"cycle":            plistDoc(0, []byte{0xA1, 1}, []byte{0xA1, 0}), // a → b → a
		"ref out of range": plistDoc(0, []byte{0xA1, 9}),
		"string too long":  plistDoc(0, []byte{0x5F, 0x10, 0xFF, 'a'}),
		"bad count int":    plistDoc(0, []byte{0x5F, 0x23, 0, 0}),
		"key not string":   plistDoc(0, []byte{0xD1, 1, 1}, []byte{0x10, 1}),
		"unknown marker":   plistDoc(0, []byte{0x70}),
		"uid":              plistDoc(0, []byte{0x80, 1}),
		"truncated int":    plistDoc(0, []byte{0x13, 1, 2}),
		"nan date":         plistDoc(0, append([]byte{0x33}, binary.BigEndian.AppendUint64(nil, math.Float64bits(math.NaN()))...)),
		"huge int128":      plistDoc(0, append([]byte{0x14, 1}, make([]byte, 15)...)),
	}
	// A shared array referenced twice per level expands to 2^30 values (within the depth
	// limit).
	var objs [][]byte
	for i := range 30 {
		objs = append(objs, []byte{0xA2, byte(i + 1), byte(i + 1)})
	}
	objs = append(objs, []byte{0x09})
	cases["exponential"] = plistDoc(0, objs...)
	// A trailer pointing outside the document.
	bad := plistDoc(0, []byte{0x09})
	binary.BigEndian.PutUint64(bad[len(bad)-8:], 1<<40)
	cases["table offset"] = bad
	bad = plistDoc(0, []byte{0x09})
	binary.BigEndian.PutUint64(bad[len(bad)-24:], 1<<20)
	cases["object count"] = bad
	cases["oversized"] = append(plistDoc(0, []byte{0x09}), make([]byte, maxPlistBytes)...)

	for name, b := range cases {
		start := time.Now()
		if _, err := decodePlist(b); !errors.Is(err, errPlist) {
			t.Errorf("%s: err %v", name, err)
		}
		if el := time.Since(start); el > time.Second {
			t.Errorf("%s: took %v", name, el)
		}
	}
	// A legitimate shared object (two references to one string) decodes.
	got, err := decodePlist(plistDoc(0, []byte{0xA2, 1, 1}, []byte{0x51, 'x'}))
	if err != nil || !reflect.DeepEqual(got, []any{"x", "x"}) {
		t.Errorf("shared object: %v %v", got, err)
	}
}

func FuzzPlist(f *testing.F) {
	f.Add(mustHex(f, pythonPlist))
	enc, _ := encodePlist(map[string]any{"duration": 1801.5, "position": 18.0, "rate": 1.0, "readyToPlay": true,
		"loadedTimeRanges": []any{map[string]any{"start": 0.0, "duration": 30.0}}})
	f.Add(enc)
	f.Add(plistDoc(0, []byte{0xA1, 0}))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := decodePlist(b)
		if err != nil {
			return
		}
		// Whatever decodes re-encodes and decodes to the same value (NaN aside).
		out, err := encodePlist(v)
		if err != nil {
			t.Fatalf("decoded value does not encode: %v", err)
		}
		again, err := decodePlist(out)
		if err != nil {
			t.Fatalf("re-encoded document does not decode: %v", err)
		}
		if !hasNaN(v) && !equalPlist(again, normalisePlist(v)) {
			t.Fatalf("round trip changed %#v to %#v", v, again)
		}
	})
}

func hasNaN(v any) bool {
	switch x := v.(type) {
	case float64:
		return math.IsNaN(x)
	case map[string]any:
		for _, e := range x {
			if hasNaN(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasNaN(e) {
				return true
			}
		}
	}
	return false
}
