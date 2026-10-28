package airplay

import (
	"bytes"
	"errors"
	"testing"
)

func TestTLV8Encode(t *testing.T) {
	long := bytes.Repeat([]byte{0xAB}, 300)
	got := encodeTLV(tlvItem{tlvState, []byte{1}}, tlvItem{tlvPublicKey, long}, tlvItem{tlvMethod, nil})
	want := append([]byte{0x06, 1, 1, 0x03, 255}, long[:255]...)
	want = append(want, 0x03, 45)
	want = append(want, long[255:]...)
	want = append(want, 0x00, 0)
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x", got)
	}
	// Exactly 255 bytes is one fragment; 510 two full ones.
	if got := encodeTLV(tlvItem{1, make([]byte, 255)}); len(got) != 257 {
		t.Errorf("255-byte value: %d bytes", len(got))
	}
	if got := encodeTLV(tlvItem{1, make([]byte, 510)}); len(got) != 514 || got[257] != 1 || got[258] != 255 {
		t.Errorf("510-byte value: %d bytes", len(got))
	}
}

func TestTLV8Decode(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 384) // an SRP public key: 255 + 129
	m, err := decodeTLV(encodeTLV(tlvItem{tlvState, []byte{2}}, tlvItem{tlvSalt, []byte("saltsaltsaltsalt")},
		tlvItem{tlvPublicKey, key}, tlvItem{tlvFlags, nil}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m[tlvState], []byte{2}) || string(m[tlvSalt]) != "saltsaltsaltsalt" || !bytes.Equal(m[tlvPublicKey], key) {
		t.Errorf("decoded %v", m)
	}
	if v, ok := m[tlvFlags]; !ok || len(v) != 0 {
		t.Errorf("empty item: %v %v", v, ok)
	}

	for name, in := range map[string][]byte{
		"truncated header": {0x06},
		"truncated value":  {0x06, 4, 1, 2},
		"repeated type":    {0x06, 1, 1, 0x02, 1, 0, 0x06, 1, 2},
		"short fragment":   {0x06, 1, 1, 0x06, 1, 2}, // a 1-byte item cannot continue
	} {
		if _, err := decodeTLV(in); !errors.Is(err, errTLV) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if m, err := decodeTLV(nil); err != nil || len(m) != 0 {
		t.Errorf("empty input: %v %v", m, err)
	}
}

func FuzzTLV8(f *testing.F) {
	f.Add(encodeTLV(tlvItem{tlvState, []byte{2}}, tlvItem{tlvPublicKey, bytes.Repeat([]byte{7}, 384)}))
	f.Add([]byte{0x06, 1, 1, 0x06, 1, 2})
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := decodeTLV(b)
		if err != nil {
			return
		}
		total := 0
		var items []tlvItem
		for typ, v := range m {
			total += len(v)
			items = append(items, tlvItem{typ, v})
		}
		if total > len(b) {
			t.Fatalf("decoded %d value bytes from %d input bytes", total, len(b))
		}
		again, err := decodeTLV(encodeTLV(items...))
		if err != nil || len(again) != len(m) {
			t.Fatalf("re-encoding failed: %v", err)
		}
		for typ, v := range m {
			if !bytes.Equal(again[typ], v) {
				t.Fatalf("type %d changed", typ)
			}
		}
	})
}
