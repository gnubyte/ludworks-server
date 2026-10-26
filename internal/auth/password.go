package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are Argon2id parameters. Memory is in KiB.
type PasswordParams struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultPasswordParams are the contract parameters (ARCHITECTURE.md §2.3):
// m=64 MiB, t=3, p=2, 16-byte salt, 32-byte key.
var DefaultPasswordParams = PasswordParams{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// MaxPasswordBytes is the longest accepted password (bounds hashing cost and request size).
const MaxPasswordBytes = 1024

// hashSem bounds concurrent Argon2id computations process-wide: each one holds Memory KiB, so
// an unbounded number of parallel logins could exhaust RAM.
var hashSem = make(chan struct{}, 4)

func acquireHashSlot(ctx context.Context) error {
	select {
	case hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseHashSlot() { <-hashSem }

// hasher hashes and verifies passwords in PHC string format
// ($argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>, unpadded standard base64).
type hasher struct {
	params PasswordParams

	dummyOnce sync.Once
	dummy     string
}

func newHasher(p PasswordParams) *hasher { return &hasher{params: p} }

var b64 = base64.RawStdEncoding

// Hash returns the PHC encoding of password under the current parameters.
func (h *hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := acquireHashSlot(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)
	releaseHashSlot()
	return encodePHC(h.params, salt, key), nil
}

func encodePHC(p PasswordParams, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

var errBadHash = errors.New("auth: malformed password hash")

// decodePHC parses an argon2id PHC string. Parameters are bounded so that a corrupted or
// tampered row cannot make verification allocate unbounded memory or CPU.
func decodePHC(s string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, errBadHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return PasswordParams{}, nil, nil, errBadHash
	}
	var p PasswordParams
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return PasswordParams{}, nil, nil, errBadHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return PasswordParams{}, nil, nil, errBadHash
		}
		switch k {
		case "m":
			p.Memory = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > 255 {
				return PasswordParams{}, nil, nil, errBadHash
			}
			p.Threads = uint8(n)
		default:
			return PasswordParams{}, nil, nil, errBadHash
		}
	}
	salt, err1 := b64.DecodeString(parts[4])
	key, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return PasswordParams{}, nil, nil, errBadHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	if p.Threads < 1 || p.Threads > 16 || p.Time < 1 || p.Time > 16 ||
		p.Memory < 8*uint32(p.Threads) || p.Memory > 1<<20 ||
		len(salt) < 8 || len(salt) > 64 || len(key) < 16 || len(key) > 64 {
		return PasswordParams{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// Verify checks password against a PHC hash in constant time. needsRehash reports that the
// hash was made with parameters other than the current ones (upgrade it after a successful
// login).
func (h *hasher) Verify(ctx context.Context, encoded, password string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if err := acquireHashSlot(ctx); err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	releaseHashSlot()
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// Burn verifies password against a fixed dummy hash so that logins for unknown accounts take
// as long as real ones (no username enumeration through timing). It always returns false.
func (h *hasher) Burn(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		pw := make([]byte, 16)
		_, _ = rand.Read(pw)
		d, err := h.Hash(context.Background(), string(pw))
		if err != nil {
			// Unreachable in practice (crypto/rand failure); fall back to a well-formed hash
			// with the right parameters so Verify still does the full amount of work.
			d = encodePHC(h.params, make([]byte, h.params.SaltLen), make([]byte, h.params.KeyLen))
		}
		h.dummy = d
	})
	_, _, _ = h.Verify(ctx, h.dummy, password)
}
