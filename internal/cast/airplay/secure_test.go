package airplay

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// HKDF-SHA512 known answers (IKM = 00 01 … 1f, 32-byte output), computed independently with
// Python's hmac/hashlib.
func TestHKDFKnownAnswers(t *testing.T) {
	ikm := make([]byte, 32)
	for i := range ikm {
		ikm[i] = byte(i)
	}
	for _, c := range []struct{ salt, info, want string }{
		{"Control-Salt", "Control-Write-Encryption-Key", "c3ca130c7033dbe5e7ff7f91d117ead869bac476994c7a48ca170c111136ed96"},
		{"Control-Salt", "Control-Read-Encryption-Key", "c09403ef8aa6c5045cbd8cf9bf3e665b2caed623af2be0e87c8f80f519914d3d"},
		{"Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info", "52890146745a52e57b82b859a7a3679c7f3d40bb295b055a0c8fa8af92a3746d"},
		{"Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info", "faf9f3558a8ed1e45219bd94fb6d27e5b43a1bc861157fc2a0d291d8e3df410a"},
		{"Events-Salt", "Events-Read-Encryption-Key", "77c905af437e45acea18c03d6d5b7791fa9b9da65f3d4d63de8ce7ab3ea3fa5a"},
	} {
		if got := hex.EncodeToString(hkdfKey(ikm, c.salt, c.info)); got != c.want {
			t.Errorf("%s/%s = %s", c.salt, c.info, got)
		}
	}
}

func TestNonces(t *testing.T) {
	if got := labelNonce("PS-Msg05"); !bytes.Equal(got, append(make([]byte, 4), "PS-Msg05"...)) {
		t.Errorf("label nonce % x", got)
	}
	if got := counterNonce(0x0102030405060708); !bytes.Equal(got, mustHex(t, "000000000807060504030201")) {
		t.Errorf("counter nonce % x", got)
	}
}

// pipePair returns two connected TCP connections (net.Pipe has no buffering, which would
// deadlock writes larger than one read).
func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestSecureConnFraming(t *testing.T) {
	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	a, b := pipePair(t)
	sa, err := newSecureConn(a, k1, k2)
	if err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, 3000)
	for i := range msg {
		msg[i] = byte(i * 7)
	}
	go sa.Write(msg)

	// Parse the raw frames independently: 1024 + 1024 + 952 plaintext bytes.
	enc, _ := chacha20poly1305.New(k1)
	var got []byte
	for i, want := range []int{1024, 1024, 952} {
		var hdr [2]byte
		if _, err := io.ReadFull(b, hdr[:]); err != nil {
			t.Fatal(err)
		}
		n := int(binary.LittleEndian.Uint16(hdr[:]))
		if n != want {
			t.Fatalf("frame %d length %d, want %d", i, n, want)
		}
		ct := make([]byte, n+16)
		if _, err := io.ReadFull(b, ct); err != nil {
			t.Fatal(err)
		}
		var nonce [12]byte
		nonce[4] = byte(i)
		pt, err := enc.Open(nil, nonce[:], ct, hdr[:])
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		got = append(got, pt...)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("plaintext differs")
	}

	// And the other direction through secureConn on both ends.
	sb, _ := newSecureConn(b, k2, k1)
	go sb.Write([]byte("hello over HAP"))
	buf := make([]byte, 64)
	sa.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := sa.Read(buf)
	if err != nil || string(buf[:n]) != "hello over HAP" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
}

func TestSecureConnRejects(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	aead, _ := chacha20poly1305.New(key)
	frame := func(n uint16, counter uint64, tamper bool) []byte {
		hdr := binary.LittleEndian.AppendUint16(nil, n)
		ct := aead.Seal(nil, counterNonce(counter), make([]byte, n), hdr)
		if tamper {
			ct[0] ^= 1
		}
		return append(hdr, ct...)
	}
	for name, raw := range map[string][]byte{
		"bad tag":       frame(10, 0, true),
		"wrong counter": frame(10, 1, false),
		"too long":      binary.LittleEndian.AppendUint16(nil, 1025),
		"truncated":     frame(10, 0, false)[:20],
	} {
		a, b := pipePair(t)
		s, _ := newSecureConn(a, key, key)
		b.Write(raw)
		b.Close()
		s.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := s.Read(make([]byte, 16)); err == nil {
			t.Errorf("%s: read succeeded", name)
		} else if name != "truncated" && !errors.Is(err, errSecure) {
			t.Errorf("%s: err %v", name, err)
		}
		// Errors are sticky.
		if _, err := s.Read(make([]byte, 16)); err == nil {
			t.Errorf("%s: second read succeeded", name)
		}
	}
}
