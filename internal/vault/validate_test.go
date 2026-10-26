package vault

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"3f8a2c4e-9b1d-4e7a-8c2f-5d6e7f8a9b0c":  true,
		"00000000-0000-0000-0000-000000000000":  true, // any version (VAULT-CRYPTO §8.3)
		"3F8A2C4E-9B1D-4E7A-8C2F-5D6E7F8A9B0C":  false,
		"3f8a2c4e9b1d4e7a8c2f5d6e7f8a9b0c":      false,
		"3f8a2c4e-9b1d-4e7a-8c2f-5d6e7f8a9b0":   false,
		"3f8a2c4e-9b1d-4e7a-8c2f-5d6e7f8a9b0c ": false,
		"../../etc/passwd":                      false,
		"":                                      false,
	} {
		if got := validID(id); got != want {
			t.Errorf("validID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestCheckSealed(t *testing.T) {
	enc := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	ok := "v1." + enc(12) + "." + enc(48)
	cases := []struct {
		s      string
		max    int
		ct     int
		wantOK bool
	}{
		{ok, 100, 48, true},
		{ok, 100, 0, true},
		{ok, 50, 0, false}, // too long
		{"v1." + enc(12) + "." + enc(47), 100, 48, false}, // wrong exact size
		{"v1." + enc(12) + "." + enc(15), 100, 0, false},  // shorter than the tag
		{"v1." + enc(12) + "." + enc(16), 100, 0, true},   // empty plaintext
		{"v1." + enc(11) + "." + enc(48), 100, 0, false},  // nonce size
		{"v1." + enc(16) + "." + enc(48), 100, 0, false},  // nonce size
		{"v2." + enc(12) + "." + enc(48), 100, 0, false},  // version
		{"V1." + enc(12) + "." + enc(48), 100, 0, false},  // version is case-sensitive
		{"v1." + enc(12) + "." + enc(48) + ".", 100, 0, false},
		{"v1." + enc(12), 100, 0, false},
		{"v1." + enc(12) + "." + strings.TrimRight(enc(47), "="), 100, 0, false}, // unpadded
		{"v1." + enc(12) + "." + "AAAA AAAA AAAA AAAA AAAA AAAA", 100, 0, false}, // whitespace
		{"v1." + enc(12) + "." + strings.Repeat("-", 24), 100, 0, false},         // URL alphabet
		{"v1." + enc(12) + ".AAB=", 100, 0, false},                               // non-canonical padding bits
		{"", 100, 0, false},
	}
	for i, c := range cases {
		msg := checkSealed(c.s, c.max, c.ct)
		if (msg == "") != c.wantOK {
			t.Errorf("case %d %q: got %q, want ok=%v", i, c.s, msg, c.wantOK)
		}
	}
}

func TestKDFValidate(t *testing.T) {
	salt := base64.StdEncoding.EncodeToString(make([]byte, 16))
	wk := "v1." + base64.StdEncoding.EncodeToString(make([]byte, 12)) + "." + base64.StdEncoding.EncodeToString(make([]byte, 48))
	good := kdfInput{KDF: "pbkdf2-sha256", KDFIterations: 600000, KDFSalt: salt, WrappedKey: wk}
	if f := good.validate(false); f != nil {
		t.Fatalf("good input rejected: %v", f)
	}
	if f := good.validate(true); f["previous_wrapped_key"] == "" {
		t.Fatalf("missing previous_wrapped_key accepted: %v", f)
	}
	hi := good
	hi.KDFIterations = MaxIterations
	if f := hi.validate(false); f != nil {
		t.Fatalf("max iterations rejected: %v", f)
	}
	long := good
	long.KDFSalt = base64.StdEncoding.EncodeToString(make([]byte, 64))
	if f := long.validate(false); f != nil {
		t.Fatalf("64-byte salt rejected: %v", f)
	}
}
