package httpx

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"

	"ludworks/internal/core"
)

// Cursors are opaque to clients: base64url(JSON payload || HMAC-SHA256 tag[:16]). The MAC
// stops clients from forging positions (e.g. crafting a cursor that changes a filter or
// skips an ownership predicate that was baked into it).

const (
	cursorTagLen = 16
	maxCursorLen = 2048
)

var cursorKey atomic.Pointer[[]byte]

func init() {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	cursorKey.Store(&k) // replaced at startup by SetCursorKey; random until then (tests)
}

// SetCursorKey sets the MAC key (the app derives it from the server secret so cursors survive
// restarts). Call once at startup.
func SetCursorKey(key []byte) {
	k := append([]byte(nil), key...)
	cursorKey.Store(&k)
}

func cursorMAC(payload []byte) []byte {
	m := hmac.New(sha256.New, *cursorKey.Load())
	m.Write([]byte("cursor:v1:"))
	m.Write(payload)
	return m.Sum(nil)[:cursorTagLen]
}

// EncodeCursor serialises v (typically a small struct of sort keys) into a signed cursor.
// It panics if v cannot be marshalled, which is a programming error.
func EncodeCursor(v any) string {
	payload, err := json.Marshal(v)
	if err != nil {
		panic("httpx: EncodeCursor: " + err.Error())
	}
	buf := append(payload, cursorMAC(payload)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// ErrInvalidCursor is returned for malformed, tampered or foreign cursors.
var ErrInvalidCursor = core.BadRequest("invalid cursor")

// DecodeCursor verifies and decodes a cursor produced by EncodeCursor into dst. An empty
// string is not a cursor; check for "" (first page) before calling.
func DecodeCursor(s string, dst any) error {
	if s == "" || len(s) > maxCursorLen {
		return ErrInvalidCursor
	}
	buf, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(buf) <= cursorTagLen {
		return ErrInvalidCursor
	}
	payload, tag := buf[:len(buf)-cursorTagLen], buf[len(buf)-cursorTagLen:]
	if !hmac.Equal(tag, cursorMAC(payload)) {
		return ErrInvalidCursor
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return ErrInvalidCursor
	}
	return nil
}
