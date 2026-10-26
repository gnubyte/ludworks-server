package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"ludworks/internal/config"
)

// KeySize is the size of the server master secret in <data>/secret.key.
const KeySize = 32

// LoadOrCreateKey reads the 32-byte server secret, creating it (mode 0600, O_EXCL) on first
// start. An existing file of the wrong size is an error: silently regenerating it would make
// every stored secret undecryptable.
func LoadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != KeySize {
			return nil, fmt.Errorf("secret key %s: expected %d bytes, found %d", path, KeySize, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read secret key: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) { // lost a race with another process: use theirs
			return LoadOrCreateKey(path)
		}
		return nil, fmt.Errorf("create secret key: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

// CheckKeyFile reports why the key file is not private (nil if it is). On POSIX systems it
// must not be accessible to group/others. On Windows, where modes do not apply, its owner and
// every permission entry must belong to SYSTEM, Administrators, TrustedInstaller, the Ludworks
// service account or the account running the server, and it must not be a link: a key planted
// by another user (e.g. in a data directory they created first) must never be adopted.
func CheckKeyFile(path string) error {
	if runtime.GOOS == "windows" {
		return config.CheckPrivate(path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secret key file %s is accessible to other users (mode %v); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	return nil
}

// KeyFilePermissionsOK reports whether CheckKeyFile accepts the key file.
func KeyFilePermissionsOK(path string) bool { return CheckKeyFile(path) == nil }

// DeriveKey derives an independent 32-byte key for a purpose label from the master secret
// (HMAC-SHA256(master, "ludworks:"+purpose)), so that e.g. the cursor MAC key and the
// settings encryption key never share key material directly.
func DeriveKey(master []byte, purpose string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("ludworks:" + purpose))
	return m.Sum(nil)
}

// Cipher is AES-256-GCM authenticated encryption for small secrets at rest. Sealed values
// are "v1.<b64url nonce>.<b64url ciphertext+tag>". The aad binds a ciphertext to its use
// (e.g. "settings:smtp.password", "totp:<user id>") so values cannot be swapped around.
type Cipher struct{ aead cipher.AEAD }

// NewCipher builds a Cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("cipher: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce.
func (c *Cipher) Seal(plaintext []byte, aad string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nil, nonce, plaintext, []byte(aad))
	enc := base64.RawURLEncoding
	return "v1." + enc.EncodeToString(nonce) + "." + enc.EncodeToString(ct), nil
}

// ErrDecrypt is returned for malformed or tampered ciphertexts (or a wrong key / aad).
var ErrDecrypt = errors.New("cipher: decryption failed")

// Open decrypts a value produced by Seal with the same aad.
func (c *Cipher) Open(sealed, aad string) ([]byte, error) {
	parts := strings.Split(sealed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, ErrDecrypt
	}
	enc := base64.RawURLEncoding
	nonce, err1 := enc.DecodeString(parts[1])
	ct, err2 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(nonce) != c.aead.NonceSize() {
		return nil, ErrDecrypt
	}
	pt, err := c.aead.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// IsSealed reports whether s looks like a Seal output.
func IsSealed(s string) bool { return strings.HasPrefix(s, "v1.") && strings.Count(s, ".") == 2 }
