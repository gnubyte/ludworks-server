package airplay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strings"
	"testing"
)

// airportModulusHex is the AirPort Express modulus as OwnTone (src/outputs/raop.c) writes it;
// the base64 constant in the driver was also checked against the private key shairport-sync
// ships (n = p·q).
const airportModulusHex = "e7d744f2a2e2788b6c1f55a08eb70544a8fa7945aa8be6c62ce5f51cbdd4dc6842fe3d1083dd2edec1bfd4252dc02e6f" +
	"398bdf0e6148ea84855e2e442da6d62664f674a1f304929ade4f6893ef2df6e711a8c77a0d91c9d980822e50d12922afea40ea9f0e" +
	"14c0f76938c5f3882fc0323dd9fe55155f51bb5921c201629fd73352d5e2efaabf9ba048d7b813a2b6767f6c3ccf1eb4ce673d037b0d" +
	"2ea30c5fffeb06f8d08adde409571a9c689fef10728855dd8cfb9a8bef5c8943ef3b5faa15dde698beddf3599603eb3e6f61372bb628" +
	"f6559f599a78bf500687aa7f4976c0562d412956f8989e18a6355bd81597825e0fc875343ec782117625cdbf98447b"

func TestAirportKey(t *testing.T) {
	k := airportKey()
	if k.E != 65537 || k.N.BitLen() != 2048 || hex.EncodeToString(k.N.Bytes()) != airportModulusHex {
		t.Fatalf("AirPort key: e=%d, %d bits, n=%x…", k.E, k.N.BitLen(), k.N.Bytes()[:8])
	}
}

func TestSDP(t *testing.T) {
	got := string(buildSDP(3413821438, net.ParseIP("192.168.1.10"), net.ParseIP("192.168.1.20"), "", ""))
	want := "v=0\r\n" +
		"o=iTunes 3413821438 0 IN IP4 192.168.1.10\r\n" +
		"s=iTunes\r\n" +
		"c=IN IP4 192.168.1.20\r\n" +
		"t=0 0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 AppleLossless\r\n" +
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"
	if got != want {
		t.Errorf("SDP:\n%s\nwant:\n%s", got, want)
	}
	got = string(buildSDP(1, net.ParseIP("fe80::1"), net.ParseIP("fe80::2"), "KEY", "IV"))
	if !strings.Contains(got, "o=iTunes 1 0 IN IP6 fe80::1\r\n") || !strings.Contains(got, "c=IN IP6 fe80::2\r\n") ||
		!strings.HasSuffix(got, "a=rsaaeskey:KEY\r\na=aesiv:IV\r\n") {
		t.Errorf("IPv6/encrypted SDP:\n%s", got)
	}
}

func TestStreamCipher(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := newStreamCipher()
	if err != nil {
		t.Fatal(err)
	}
	keyB64, ivB64, err := sc.sdpAttrs(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keyB64, "=") || strings.Contains(ivB64, "=") || len(keyB64) != 342 || len(ivB64) != 22 {
		t.Errorf("SDP values must be unpadded base64: %d %d chars", len(keyB64), len(ivB64))
	}
	wrapped, _ := base64.RawStdEncoding.DecodeString(keyB64)
	key, err := rsa.DecryptOAEP(sha1.New(), nil, priv, wrapped, nil)
	if err != nil {
		t.Fatalf("receiver cannot unwrap: %v", err)
	}
	iv, _ := base64.RawStdEncoding.DecodeString(ivB64)
	if !bytes.Equal(key, sc.key) || !bytes.Equal(iv, sc.iv) {
		t.Fatal("unwrapped key or IV differ")
	}

	// Payload: whole blocks AES-128-CBC from the IV, the 4-byte tail in the clear; every
	// payload starts from the IV again.
	payload := make([]byte, alacFrameBytes)
	rand.Read(payload)
	orig := bytes.Clone(payload)
	sc.seal(payload)
	block, _ := aes.NewCipher(key)
	dec := bytes.Clone(payload)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(dec[:1408], dec[:1408])
	if !bytes.Equal(dec, orig) || !bytes.Equal(payload[1408:], orig[1408:]) || bytes.Equal(payload[:16], orig[:16]) {
		t.Fatal("payload encryption differs from AES-CBC over whole blocks")
	}
	again := bytes.Clone(orig)
	sc.seal(again)
	if !bytes.Equal(again, payload) {
		t.Error("payloads are not encrypted independently")
	}
	short := []byte{1, 2, 3}
	sc.seal(short)
	if !bytes.Equal(short, []byte{1, 2, 3}) {
		t.Error("a payload shorter than a block changed")
	}
}

func TestDMAPAndParameters(t *testing.T) {
	got := dmapMetadata("Song", "Artist", "")
	want := "mlit\x00\x00\x00\x1a" + "minm\x00\x00\x00\x04Song" + "asar\x00\x00\x00\x06Artist"
	if string(got) != want {
		t.Errorf("DMAP % x", got)
	}
	if got := dmapMetadata("", "", "Album"); string(got) != "mlit\x00\x00\x00\x0dasal\x00\x00\x00\x05Album" {
		t.Errorf("DMAP % x", got)
	}
	if got := clip("ab€cd", 4); got != "ab" {
		t.Errorf("clip split a rune: %q", got)
	}
	for _, c := range []struct {
		level float64
		muted bool
		want  float64
	}{{1, false, 0}, {0.5, false, -15}, {0.25, false, -22.5}, {0, false, -144}, {0.7, true, -144}, {2, false, 0}} {
		if got := volumeDB(c.level, c.muted); got != c.want {
			t.Errorf("volumeDB(%v,%v) = %v", c.level, c.muted, got)
		}
	}
	tp := parseTransport("RTP/AVP/UDP;unicast;mode=record;server_port=6000;Control_Port=6001;timing_port=6002")
	if tp["server_port"] != "6000" || tp["control_port"] != "6001" || tp["timing_port"] != "6002" {
		t.Errorf("transport %v", tp)
	}
	for in, ok := range map[string]bool{"6000": true, "0": false, "65536": false, "x": false, "": false} {
		if _, got := parsePort(in); got != ok {
			t.Errorf("parsePort(%q) = %v", in, got)
		}
	}
}
