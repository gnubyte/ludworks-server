package airplay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

// testdata/srp_vectors.json is written by testdata/gen_srp_vectors.py, an independent
// Python (stdlib) SRP-6a implementation that checks itself against RFC 5054 Appendix B and
// the 3072-bit prime's defining formula before writing. Its first HAP vector has the RFC
// 5054 inputs and equals the SRP test vectors of Apple's HomeKit ADK; the others cover
// leading zero bytes in the salt, B, S and A.
type srpVectors struct {
	RFC5054 map[string]string   `json:"rfc5054"`
	HAP     []map[string]string `json:"hap"`
}

func loadSRPVectors(t *testing.T) *srpVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/srp_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f srpVectors
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.HAP) < 5 || f.RFC5054["S"] == "" {
		t.Fatal("incomplete vector file")
	}
	return &f
}

func hexInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		t.Fatalf("bad hex %q", s)
	}
	return n
}

// rfc5054N1024 is the 1024-bit group of RFC 5054 Appendix A (generator 2).
const rfc5054N1024 = "EEAF0AB9ADB38DD69C33F80AFA8FC5E86072618775FF3C0B9EA2314C9C256576D674DF7496EA81D3383B4813D692C6E0" +
	"E0D5D8E250B98BE48E495C1D6089DAD15DC7D7B46154D6B6CE8EF4AD69B15D4982559B297BCF1885C529F566660E57EC68" +
	"EDBC3C05726CC02FD4CBF4976EAA9AFD5138FE8376435B9FC61D2FC0EB06E3"

// TestSRPRFC5054 checks the generic SRP-6a arithmetic with the 1024-bit group and SHA-1.
func TestSRPRFC5054(t *testing.T) {
	v := loadSRPVectors(t).RFC5054
	grp := newSRPGroup(rfc5054N1024, 2, sha1.New)
	salt := mustHex(t, v["salt"])
	if got := grp.k(); got.Cmp(hexInt(t, v["k"])) != 0 {
		t.Errorf("k = %x", got)
	}
	x := grp.x(salt, []byte(v["user"]), []byte(v["password"]))
	if x.Cmp(hexInt(t, v["x"])) != 0 {
		t.Errorf("x = %x", x)
	}
	if verifier := new(big.Int).Exp(grp.g, x, grp.N); verifier.Cmp(hexInt(t, v["v"])) != 0 {
		t.Errorf("v = %x", verifier)
	}
	c := newSRPClient(grp, []byte(v["user"]), []byte(v["password"]), mustHex(t, v["a"]))
	if c.A.Cmp(hexInt(t, v["A"])) != 0 {
		t.Errorf("A = %x", c.A)
	}
	B := hexInt(t, v["B"])
	if u := grp.u(c.A, B); u.Cmp(hexInt(t, v["u"])) != 0 {
		t.Errorf("u = %x", u)
	}
	if err := c.compute(salt, B.Bytes()); err != nil {
		t.Fatal(err)
	}
	if c.S.Cmp(hexInt(t, v["S"])) != 0 {
		t.Errorf("premaster secret = %x", c.S)
	}
}

// TestSRPHAPVectors checks the HAP configuration (3072-bit group, g=5, SHA-512) including
// the session key and both proofs.
func TestSRPHAPVectors(t *testing.T) {
	for _, v := range loadSRPVectors(t).HAP {
		t.Run(v["name"], func(t *testing.T) {
			c := newSRPClient(hapSRP, []byte(v["user"]), []byte(v["password"]), mustHex(t, v["a"]))
			if got := hex.EncodeToString(c.publicA()); got != v["A"] {
				t.Fatalf("A = %s", got)
			}
			if err := c.compute(mustHex(t, v["salt"]), mustHex(t, v["B"])); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(hapSRP.pad(c.S)); got != v["S"] {
				t.Errorf("S = %s", got)
			}
			if got := hex.EncodeToString(c.K); got != v["K"] {
				t.Errorf("K = %s", got)
			}
			if got := hex.EncodeToString(c.M1); got != v["M1"] {
				t.Errorf("M1 = %s", got)
			}
			m2 := mustHex(t, v["M2"])
			if !c.verifyM2(m2) {
				t.Error("server proof M2 rejected")
			}
			m2[0] ^= 1
			if c.verifyM2(m2) {
				t.Error("damaged M2 accepted")
			}
		})
	}
}

func TestSRPGroupAndChecks(t *testing.T) {
	if hapSRP.N.BitLen() != 3072 || hapSRP.size != 384 || hapSRP.g.Int64() != 5 {
		t.Fatalf("HAP group %d bits", hapSRP.N.BitLen())
	}
	c := newSRPClient(hapSRP, []byte("Pair-Setup"), []byte("1234"), bytes.Repeat([]byte{1}, 32))
	for name, b := range map[string][]byte{
		"zero":      {0},
		"N":         hapSRP.N.Bytes(),
		"empty":     nil,
		"too large": make([]byte, 385),
	} {
		if err := c.compute(make([]byte, 16), b); err == nil {
			t.Errorf("B = %s accepted", name)
		}
	}
	if c.verifyM2(make([]byte, 64)) {
		t.Error("M2 accepted before compute")
	}
}

// tinyFirst yields tiny private values (A = 5^n has many leading zero bytes) for its first
// reads, then random ones.
type tinyFirst struct{ n int }

func (r *tinyFirst) Read(p []byte) (int, error) {
	if r.n > 0 {
		r.n--
		clear(p)
		p[len(p)-1] = byte(r.n + 2)
		return len(p), nil
	}
	return rand.Read(p)
}

func TestSRPRandomClientAvoidsLeadingZeros(t *testing.T) {
	c, err := newRandomSRPClient(hapSRP, []byte("Pair-Setup"), []byte("3939"), &tinyFirst{n: 3})
	if err != nil {
		t.Fatal(err)
	}
	if a := c.publicA(); len(a) != 384 || a[0] == 0 || c.a.BitLen() < 64 {
		t.Fatalf("A % x… from a %d-bit private value", a[:2], c.a.BitLen())
	}
	if _, err := newRandomSRPClient(hapSRP, []byte("Pair-Setup"), []byte("3939"), &tinyFirst{n: 1000}); err == nil {
		t.Error("a generator that only yields bad values was accepted")
	}
}
