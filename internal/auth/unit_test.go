package auth

import (
	"context"
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

var cheap = PasswordParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestPasswordHashPHC(t *testing.T) {
	ctx := context.Background()
	h := newHasher(cheap)
	enc, err := h.Hash(ctx, "hunter2-but-longer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") || strings.Count(enc, "$") != 5 {
		t.Fatalf("bad PHC string %q", enc)
	}
	enc2, _ := h.Hash(ctx, "hunter2-but-longer")
	if enc == enc2 {
		t.Fatal("salt not random")
	}
	ok, rehash, err := h.Verify(ctx, enc, "hunter2-but-longer")
	if err != nil || !ok || rehash {
		t.Fatalf("verify: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, enc, "hunter2-but-longeR"); ok {
		t.Fatal("wrong password accepted")
	}
	// Parameter change → valid but needs rehash.
	h2 := newHasher(PasswordParams{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})
	ok, rehash, err = h2.Verify(ctx, enc, "hunter2-but-longer")
	if err != nil || !ok || !rehash {
		t.Fatalf("param change: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestPasswordDefaultParamsFormat(t *testing.T) {
	enc := encodePHC(DefaultPasswordParams, make([]byte, 16), make([]byte, 32))
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatal(enc)
	}
	p, salt, key, err := decodePHC(enc)
	if err != nil || p != DefaultPasswordParams || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestDecodePHCRejects(t *testing.T) {
	salt := "AAAAAAAAAAAAAAAAAAAAAA"
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, s := range []string{
		"",
		"$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=16$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=4194304,t=1,p=1$" + salt + "$" + key, // 4 GiB: refused
		"$argon2id$v=19$m=64,t=99,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=0$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1,x=2$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$!!$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$AAAA$" + key, // salt too short
		"$argon2id$v=19$m=64,t=1,p=1$" + salt,
	} {
		if _, _, _, err := decodePHC(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestBurnDoesFullWork(t *testing.T) {
	h := newHasher(cheap)
	h.Burn(context.Background(), "x")
	if _, _, _, err := decodePHC(h.dummy); err != nil {
		t.Fatalf("dummy hash malformed: %v", err)
	}
}

func TestHashRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Fill the semaphore so acquisition must wait, then observe cancellation.
	for i := 0; i < cap(hashSem); i++ {
		hashSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(hashSem); i++ {
			<-hashSem
		}
	}()
	if _, err := newHasher(cheap).Hash(ctx, "x"); err == nil {
		t.Fatal("expected context error while all hash slots are busy")
	}
}

// RFC 6238 Appendix B (SHA-1), truncated to 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	b32 := base32.StdEncoding.EncodeToString(secret)
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		if got := hotp(secret, tc.unix/30); got != tc.want {
			t.Errorf("t=%d: %s want %s", tc.unix, got, tc.want)
		}
		got, err := GenerateTOTP(strings.ToLower(b32), time.Unix(tc.unix, 0))
		if err != nil || got != tc.want {
			t.Errorf("GenerateTOTP t=%d: %s %v", tc.unix, got, err)
		}
	}
	if _, err := GenerateTOTP("not base32!", time.Now()); err == nil {
		t.Error("bad secret accepted")
	}
}

func TestMatchTOTPWindowAndReplay(t *testing.T) {
	secret := newTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	cur := totpStep(now)
	for _, d := range []int64{-1, 0, 1} {
		code := hotp(secret, cur+d)
		step, ok := matchTOTP(secret, code, now, 0)
		if !ok || step != cur+d {
			t.Fatalf("offset %d: ok=%v step=%d", d, ok, step)
		}
		// Replay: the same or an older step is refused.
		if _, ok := matchTOTP(secret, code, now, cur+d); ok {
			t.Fatalf("offset %d: replay accepted", d)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := matchTOTP(secret, hotp(secret, cur+d), now, 0); ok {
			t.Fatalf("offset %d accepted", d)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12a456", "abcdef"} {
		if _, ok := matchTOTP(secret, bad, now, 0); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestOtpauthURL(t *testing.T) {
	u := otpauthURL("My NAS", "bob", []byte("12345678901234567890"))
	if !strings.HasPrefix(u, "otpauth://totp/My%20NAS:bob?") || !strings.Contains(u, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") ||
		!strings.Contains(u, "issuer=My+NAS") || !strings.Contains(u, "digits=6") {
		t.Fatal(u)
	}
}

func TestRecoveryCodes(t *testing.T) {
	key := []byte("k")
	c := newRecoveryCode()
	if len(c) != 19 || strings.Count(c, "-") != 3 {
		t.Fatal(c)
	}
	h := recoveryCodeHash(key, "u1", c)
	if !constantTimeEqual(h, recoveryCodeHash(key, "u1", strings.ToUpper(strings.ReplaceAll(c, "-", " ")))) {
		t.Fatal("normalisation failed")
	}
	if constantTimeEqual(h, recoveryCodeHash(key, "u2", c)) {
		t.Fatal("hash not bound to user")
	}
}

func TestCredentialFormat(t *testing.T) {
	tok, hash := newCredential(PrefixPAT)
	if len(tok) != 47 || !strings.HasPrefix(tok, "lwp_") || len(hash) != 32 {
		t.Fatal(tok)
	}
	if p, ok := splitCredential(tok); !ok || p != PrefixPAT {
		t.Fatal("own token rejected")
	}
	for _, bad := range []string{"", "lws_", tok + "x", "lwx_" + tok[4:], "lws_" + strings.Repeat("!", 43), tok[:46]} {
		if _, ok := splitCredential(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if patDisplayPrefix(tok) != tok[:8] {
		t.Fatal("prefix")
	}
	if tk, ok := bearerToken("bearer  " + tok); !ok || tk != tok {
		t.Fatal("bearer parse")
	}
	if _, ok := bearerToken("Basic abc"); ok {
		t.Fatal("basic accepted")
	}
}

func TestValidation(t *testing.T) {
	for _, u := range []string{"a", "bob", "Bob.Smith-2", "x_y"} {
		if msg := checkUsername(u); msg != "" {
			t.Errorf("%q: %s", u, msg)
		}
	}
	for _, u := range []string{"", "-bob", "bob@x", "bo b", "bøb", strings.Repeat("a", 65), "a\nb"} {
		if checkUsername(u) == "" {
			t.Errorf("%q accepted", u)
		}
	}
	if e, msg := normalizeEmail("  Bob@Example.com "); msg != "" || e != "Bob@Example.com" {
		t.Errorf("%q %q", e, msg)
	}
	for _, e := range []string{"", "bob", "Bob <bob@x.com>", "bob@x.com\r\nBcc: y@z.com", strings.Repeat("a", 250) + "@x.com"} {
		if _, msg := normalizeEmail(e); msg == "" {
			t.Errorf("%q accepted", e)
		}
	}
	sec := settings.Security{PasswordMinLength: 12}
	for pw, ok := range map[string]bool{
		"short":                       false,
		"twelve chars":                true,
		"            ":                false,
		"alice-the-great":             false, // equals username
		"ALICE@EXAMPLE.COM":           false, // equals email (case-insensitive)
		strings.Repeat("ä", 12):       true,  // characters, not bytes
		strings.Repeat("x", 1025):     false,
		strings.Repeat("x", 1024):     true,
		"pässwörd-pässwörd\xff\xfe12": false, // invalid UTF-8
	} {
		if got := checkPassword(sec.PasswordMinLength, pw, "alice-the-great", "alice@example.com") == ""; got != ok {
			t.Errorf("%q: valid=%v want %v", pw, got, ok)
		}
	}
	if s, msg := cleanDeviceName(strings.Repeat("é", 200)); msg != "" || len([]rune(s)) != maxDeviceNameLen {
		t.Errorf("device name truncation: %q %q", s, msg)
	}
	if _, msg := cleanDeviceName("a\x00b"); msg == "" {
		t.Error("control char accepted")
	}
	if s, msg := normalizeScopes([]string{"files:read", "files:read", "account"}); msg != "" || len(s) != 2 {
		t.Errorf("%v %q", s, msg)
	}
	if _, msg := normalizeScopes([]string{"*"}); msg == "" {
		t.Error("wildcard scope accepted")
	}
	if _, msg := normalizeScopes(nil); msg == "" {
		t.Error("empty scopes accepted")
	}
}

func TestSessionValid(t *testing.T) {
	sec := settings.Security{SessionIdleMinutes: 60, SessionAbsoluteDays: 2}
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	created := now.Add(-24 * time.Hour)
	expires := created.AddDate(0, 0, 30) // stored with an older, longer setting
	if !sessionValid(sec, created, now.Add(-59*time.Minute), expires, now) {
		t.Fatal("fresh session invalid")
	}
	if sessionValid(sec, created, now.Add(-61*time.Minute), expires, now) {
		t.Fatal("idle timeout ignored")
	}
	if sessionValid(sec, now.Add(-49*time.Hour), now, expires, now) {
		t.Fatal("shortened absolute lifetime ignored")
	}
	if sessionValid(sec, created, now, now, now) {
		t.Fatal("stored expiry ignored")
	}
}

// Regression (e2e): a reset requested from http://nas.localhost:port sent no email ("set
// public_url"), although *.localhost names are loopback by definition (RFC 6761).
func TestIsOwnHostLoopbackNames(t *testing.T) {
	m := &Module{d: &core.Deps{}}
	for _, h := range []string{"localhost", "LOCALHOST.", "nas.localhost", "a.b.localhost", "127.0.0.1", "::1"} {
		if !m.isOwnHost(h) {
			t.Errorf("isOwnHost(%q) = false", h)
		}
	}
	for _, h := range []string{"", "evil.example", "localhost.evil.example", "xlocalhost", ".localhost", "localhost-evil.com", "8.8.8.8"} {
		if m.isOwnHost(h) {
			t.Errorf("isOwnHost(%q) = true", h)
		}
	}
}
