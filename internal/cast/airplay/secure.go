package airplay

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// The HAP-encrypted connection that follows pair-verify (or transient pair-setup): every
// block is a 2-byte little-endian plaintext length (at most 1024), the ChaCha20-Poly1305
// ciphertext and its 16-byte tag, sealed with the length bytes as additional data and a
// per-direction 64-bit little-endian block counter as nonce (four zero bytes first).

const maxSecureBlock = 1024

var errSecure = errors.New("airplay: encrypted channel failure")

// hkdfKey derives a 32-byte key with HKDF-SHA512.
func hkdfKey(secret []byte, salt, info string) []byte {
	k, err := hkdf.Key(sha512.New, secret, []byte(salt), info, 32)
	if err != nil {
		panic(err) // only for impossible lengths
	}
	return k
}

// counterNonce is the 12-byte nonce for block number n.
func counterNonce(n uint64) []byte {
	var nonce [chacha20poly1305.NonceSize]byte
	binary.LittleEndian.PutUint64(nonce[4:], n)
	return nonce[:]
}

// labelNonce is a pairing message nonce such as "PS-Msg05", zero-padded on the left.
func labelNonce(label string) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	copy(nonce[len(nonce)-len(label):], label)
	return nonce
}

// secureConn encrypts a net.Conn with HAP framing. Deadlines and addresses pass through.
type secureConn struct {
	net.Conn
	enc, dec cipher.AEAD

	wmu    sync.Mutex
	wcount uint64

	rmu    sync.Mutex
	rcount uint64
	rbuf   []byte // decrypted bytes not yet returned
	rblock []byte
	rerr   error
}

// newSecureConn wraps c; writeKey seals what we send, readKey opens what we receive.
func newSecureConn(c net.Conn, writeKey, readKey []byte) (*secureConn, error) {
	enc, err := chacha20poly1305.New(writeKey)
	if err != nil {
		return nil, err
	}
	dec, err := chacha20poly1305.New(readKey)
	if err != nil {
		return nil, err
	}
	return &secureConn{Conn: c, enc: enc, dec: dec, rblock: make([]byte, 0, maxSecureBlock+16)}, nil
}

func (s *secureConn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	written := 0
	out := make([]byte, 0, 2+min(len(p), maxSecureBlock)+16)
	for len(p) > 0 {
		n := min(len(p), maxSecureBlock)
		var hdr [2]byte
		binary.LittleEndian.PutUint16(hdr[:], uint16(n))
		out = s.enc.Seal(append(out[:0], hdr[:]...), counterNonce(s.wcount), p[:n], hdr[:])
		s.wcount++
		if _, err := s.Conn.Write(out); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *secureConn) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.rbuf) == 0 {
		if s.rerr != nil {
			return 0, s.rerr
		}
		if err := s.readBlock(); err != nil {
			s.rerr = err
			return 0, err
		}
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

func (s *secureConn) readBlock() error {
	var hdr [2]byte
	if _, err := io.ReadFull(s.Conn, hdr[:]); err != nil {
		return err
	}
	n := int(binary.LittleEndian.Uint16(hdr[:]))
	if n > maxSecureBlock {
		return fmt.Errorf("%w: block of %d bytes", errSecure, n)
	}
	blk := s.rblock[:n+16]
	if _, err := io.ReadFull(s.Conn, blk); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	pt, err := s.dec.Open(blk[:0], counterNonce(s.rcount), blk, hdr[:])
	if err != nil {
		return fmt.Errorf("%w: authentication failed", errSecure)
	}
	s.rcount++
	s.rbuf = pt
	return nil
}
