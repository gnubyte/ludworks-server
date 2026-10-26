package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Credential prefixes (ARCHITECTURE.md §2.4).
const (
	PrefixSession = "lws_" // session token (cookie value or token_mode bearer)
	PrefixPAT     = "lwp_" // personal access token
)

// tokenBodyLen is the base64url length of 32 random bytes.
const tokenBodyLen = 43

var b64url = base64.RawURLEncoding

// randomToken returns 32 random bytes, base64url without padding.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return b64url.EncodeToString(b)
}

// newCredential returns prefix+random and its storage hash.
func newCredential(prefix string) (string, []byte) {
	tok := prefix + randomToken()
	return tok, hashToken(tok)
}

// hashToken is the at-rest form of a token: SHA-256 (tokens carry 256 bits of entropy, so a
// fast hash is sufficient and keeps per-request authentication cheap).
func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// splitCredential validates the token syntax and returns its prefix. Malformed values are
// rejected before touching the database.
func splitCredential(tok string) (prefix string, ok bool) {
	if len(tok) != len(PrefixSession)+tokenBodyLen {
		return "", false
	}
	prefix, body := tok[:4], tok[4:]
	if prefix != PrefixSession && prefix != PrefixPAT {
		return "", false
	}
	if !isB64URL(body) {
		return "", false
	}
	return prefix, true
}

// isB64URL reports whether s is a plain random token body (base64url alphabet only).
func isB64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// patDisplayPrefix is what lists show for recognition, e.g. "lwp_ab12".
func patDisplayPrefix(tok string) string { return tok[:len(PrefixPAT)+4] }

// bearerToken extracts the credential from an Authorization header value.
func bearerToken(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// validEmailToken reports whether s has the shape of an emailed token (no prefix).
func validEmailToken(s string) bool { return len(s) == tokenBodyLen && isB64URL(s) }
