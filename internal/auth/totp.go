package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP per RFC 6238: HMAC-SHA1, 6 digits, 30 s steps, ±1 step of clock skew accepted.
const (
	totpDigits    = 6
	totpPeriod    = 30
	totpSkew      = 1
	totpSecretLen = 20 // 160 bits, the RFC 4226 recommendation for SHA-1
)

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() []byte {
	b := make([]byte, totpSecretLen)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return b
}

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// hotp computes the RFC 4226 value for counter.
func hotp(secret []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// GenerateTOTP returns the code for a base32 secret at time t (clients, tests).
func GenerateTOTP(secretBase32 string, t time.Time) (string, error) {
	secret, err := totpB32.DecodeString(strings.ToUpper(strings.TrimRight(strings.ReplaceAll(secretBase32, " ", ""), "=")))
	if err != nil || len(secret) == 0 {
		return "", errors.New("auth: invalid TOTP secret")
	}
	return hotp(secret, totpStep(t)), nil
}

// matchTOTP checks code against the steps around now and returns the matching step. Only
// steps strictly after lastStep are accepted (a code can never be used twice). All candidate
// steps are compared so timing does not reveal which one matched.
func matchTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	cur := totpStep(now)
	var found int64
	ok := 0
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		eq := subtle.ConstantTimeCompare([]byte(hotp(secret, s)), []byte(code))
		if eq == 1 && s > lastStep && ok == 0 {
			found, ok = s, 1
		}
	}
	return found, ok == 1
}

// otpauthURL builds the key URI understood by authenticator apps.
func otpauthURL(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", totpB32.EncodeToString(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Recovery codes: 10 codes of 64 random bits, shown as xxxx-xxxx-xxxx-xxxx (hex).
const recoveryCodeCount = 10

func newRecoveryCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	h := hex.EncodeToString(b)
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

// normalizeRecoveryCode strips separators and case so users can type codes loosely.
func normalizeRecoveryCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// recoveryCodeHash is HMAC-SHA256 under a server-derived key, bound to the user id. The
// codes have 64 bits of entropy, so a keyed fast hash is sufficient; the key means a stolen
// database alone does not allow offline guessing.
func recoveryCodeHash(key []byte, userID, code string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(userID))
	m.Write([]byte{0})
	m.Write([]byte(normalizeRecoveryCode(code)))
	return m.Sum(nil)
}
