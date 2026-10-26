package vault

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// KDF parameters accepted by the server (VAULT-CRYPTO.md §1, §8.3).
const (
	KDFName       = "pbkdf2-sha256"
	MinIterations = 600_000
	MaxIterations = 10_000_000
	MinSaltBytes  = 16
	MaxSaltBytes  = 64

	nonceBytes      = 12
	tagBytes        = 16
	vaultKeyBytes   = 32
	wrappedKeyBytes = vaultKeyBytes + tagBytes
	maxWrappedLen   = 128      // "v1." + 16 + "." + 64 with room to spare
	keyCheckBytes   = tagBytes // key_check seals the empty string: the GCM tag only
)

// idPattern is the lowercase hyphenated UUID that clients bind into the AAD (§8.3).
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validID(s string) bool { return len(s) == 36 && idPattern.MatchString(s) }

var strictB64 = base64.StdEncoding.Strict()

// decodeB64 decodes canonical standard base64 with padding; nil when invalid.
func decodeB64(s string) []byte {
	if s == "" || len(s)%4 != 0 {
		return nil
	}
	b, err := strictB64.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// checkSealed validates the shape of a sealed string "v1.<b64 nonce>.<b64 ciphertext||tag>"
// without any key: version v1, a 12-byte nonce, a ciphertext of at least the 16-byte tag
// (exactly ctLen bytes when ctLen > 0), canonical base64 and at most maxLen characters.
// It returns "" when valid, else a message for the API.
func checkSealed(s string, maxLen, ctLen int) string {
	if s == "" {
		return "is required"
	}
	if len(s) > maxLen {
		return fmt.Sprintf("must be at most %d characters", maxLen)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "must be a sealed string v1.<nonce>.<ciphertext>"
	}
	if n := decodeB64(parts[1]); len(n) != nonceBytes {
		return "must carry a 12-byte base64 nonce"
	}
	ct := decodeB64(parts[2])
	switch {
	case len(ct) < tagBytes:
		return "must carry base64 ciphertext including the 16-byte tag"
	case ctLen > 0 && len(ct) != ctLen:
		return fmt.Sprintf("must seal exactly %d bytes", ctLen-tagBytes)
	}
	return ""
}

// kdfInput is the body of POST /vault/init, PUT /vault/key and (embedded) POST /vault/rekey.
type kdfInput struct {
	KDF                string `json:"kdf"`
	KDFIterations      int64  `json:"kdf_iterations"`
	KDFSalt            string `json:"kdf_salt"`
	WrappedKey         string `json:"wrapped_key"`
	PreviousWrappedKey string `json:"previous_wrapped_key"`
	// KeyCheck = Seal(vault_key, "", AAD "ludworks/vault/check/v1"): optional at init (older
	// clients do not send it), required by rekey.
	KeyCheck string `json:"key_check"`
	// KeyID is the vault key the client believes is current (key change, rekey).
	KeyID string `json:"key_id"`
	// AllowKDFDowngrade must be set to store fewer iterations than the vault currently uses
	// (key change, rekey): a client must never silently weaken a work factor chosen elsewhere.
	AllowKDFDowngrade bool `json:"allow_kdf_downgrade"`
}

// checkKeyCheck validates a key_check value ("" = absent, allowed unless required).
func checkKeyCheck(f map[string]string, s string, required bool) {
	if s == "" && !required {
		return
	}
	if msg := checkSealed(s, maxWrappedLen, keyCheckBytes); msg != "" {
		f["key_check"] = msg
	}
}

// validate returns field errors (nil when valid). withPrevious requires previous_wrapped_key.
func (in *kdfInput) validate(withPrevious bool) map[string]string {
	f := map[string]string{}
	if in.KDF != KDFName {
		f["kdf"] = "must be " + KDFName
	}
	if in.KDFIterations < MinIterations || in.KDFIterations > MaxIterations {
		f["kdf_iterations"] = fmt.Sprintf("must be between %d and %d", MinIterations, MaxIterations)
	}
	if salt := decodeB64(in.KDFSalt); len(salt) < MinSaltBytes || len(salt) > MaxSaltBytes {
		f["kdf_salt"] = fmt.Sprintf("must be %d-%d bytes of standard base64", MinSaltBytes, MaxSaltBytes)
	}
	if msg := checkSealed(in.WrappedKey, maxWrappedLen, wrappedKeyBytes); msg != "" {
		f["wrapped_key"] = msg
	}
	if withPrevious {
		if msg := checkSealed(in.PreviousWrappedKey, maxWrappedLen, wrappedKeyBytes); msg != "" {
			f["previous_wrapped_key"] = msg
		}
	}
	checkKeyCheck(f, in.KeyCheck, false)
	if in.KeyID != "" && !validID(in.KeyID) {
		f["key_id"] = "must be a lowercase UUID"
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

// clip shortens untrusted strings echoed back in batch error reports.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
