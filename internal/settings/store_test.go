package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ludworks/internal/db"
)

func newStore(t *testing.T) (*Store, *db.DB, []byte) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	key, err := LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), d, key)
	if err != nil {
		t.Fatal(err)
	}
	return s, d, key
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.key")
	k1, err := LoadOrCreateKey(p)
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, _ := LoadOrCreateKey(p)
	if !bytes.Equal(k1, k2) {
		t.Fatal("key changed on reload")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 || !KeyFilePermissionsOK(p) {
			t.Fatalf("mode = %v", st.Mode().Perm())
		}
	}
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := LoadOrCreateKey(p); err == nil {
		t.Fatal("expected error for truncated key")
	}
}

func TestCipher(t *testing.T) {
	c, _ := NewCipher(DeriveKey(make([]byte, 32), "x"))
	s, err := c.Seal([]byte("hunter2"), "a")
	if err != nil || !IsSealed(s) {
		t.Fatal(err, s)
	}
	if pt, err := c.Open(s, "a"); err != nil || string(pt) != "hunter2" {
		t.Fatal(err)
	}
	if _, err := c.Open(s, "b"); !errors.Is(err, ErrDecrypt) {
		t.Fatal("aad not enforced")
	}
	tampered := s[:len(s)-2] + "AA"
	if _, err := c.Open(tampered, "a"); !errors.Is(err, ErrDecrypt) {
		t.Fatal("tamper not detected")
	}
	if bytes.Equal(DeriveKey([]byte("k"), "a"), DeriveKey([]byte("k"), "b")) {
		t.Fatal("derived keys must differ per purpose")
	}
}

func TestDefaultsAndTypedGet(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	g, err := s.General(ctx)
	if err != nil || g.ServerName != "Ludworks" || g.RegistrationEnabled || g.DefaultRole != "user" {
		t.Fatalf("%+v %v", g, err)
	}
	sec, _ := s.Security(ctx)
	if sec.SessionIdleMinutes != 10080 || sec.PasswordMinLength != 12 || !sec.LoginLockoutEnabled || sec.AllowedExtensionOrigins == nil {
		t.Fatalf("%+v", sec)
	}
	m, _ := s.SMTP(ctx)
	if m.Port != 587 || m.Security != "starttls" || m.FromName != "Ludworks" {
		t.Fatalf("%+v", m)
	}
	i, _ := s.IMAP(ctx)
	if i.Port != 993 || i.Mailbox != "INBOX" || i.PollMinutes != 5 || !i.MarkSeen {
		t.Fatalf("%+v", i)
	}
	pub := s.Public(ctx)
	for _, ns := range []string{"general", "security", "smtp", "imap"} {
		if _, ok := pub[ns]; !ok {
			t.Fatalf("public missing %s", ns)
		}
	}
	raw, _ := json.Marshal(pub["security"])
	if !strings.Contains(string(raw), `"allowed_extension_origins":[]`) {
		t.Fatalf("security = %s", raw)
	}
}

func TestSecretEncryptedAtRestAndRedacted(t *testing.T) {
	s, d, _ := newStore(t)
	ctx := context.Background()
	err := s.Update(ctx, "smtp", json.RawMessage(`{"enabled":true,"host":"mail.example.com","password":"s3cr3t-P@ss","from_address":"nas@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	d.SQL.QueryRowContext(ctx, `SELECT json FROM settings WHERE ns='smtp'`).Scan(&raw)
	if strings.Contains(raw, "s3cr3t") || !strings.Contains(raw, `"password":"v1.`) {
		t.Fatalf("secret not encrypted at rest: %s", raw)
	}
	m, _ := s.SMTP(ctx)
	if m.Password != "s3cr3t-P@ss" || m.Host != "mail.example.com" {
		t.Fatalf("%+v", m)
	}
	pub, _ := s.PublicNamespace(ctx, "smtp")
	b, _ := json.Marshal(pub)
	if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"password":{"set":true}`) {
		t.Fatalf("public = %s", b)
	}
	// Round-tripping the redacted object keeps the secret; omitting keeps it too.
	full, _ := json.Marshal(pub)
	if err := s.Update(ctx, "smtp", full); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "smtp", json.RawMessage(`{"port":465,"security":"tls"}`)); err != nil {
		t.Fatal(err)
	}
	m, _ = s.SMTP(ctx)
	if m.Password != "s3cr3t-P@ss" || m.Port != 465 {
		t.Fatalf("%+v", m)
	}
	// null clears.
	if err := s.Update(ctx, "smtp", json.RawMessage(`{"password":null}`)); err != nil {
		t.Fatal(err)
	}
	m, _ = s.SMTP(ctx)
	pub, _ = s.PublicNamespace(ctx, "smtp")
	if m.Password != "" || pub["password"].(map[string]any)["set"] != false {
		t.Fatalf("not cleared: %+v", m)
	}
	// A sealed value moved to another field/namespace must not decrypt (aad binding).
	if _, err := s.cipher.Open(extractSealed(t, raw), aadFor("imap", "password")); err == nil {
		t.Fatal("ciphertext usable under another aad")
	}
}

func extractSealed(t *testing.T, raw string) string {
	var m map[string]string
	var anyMap map[string]json.RawMessage
	json.Unmarshal([]byte(raw), &anyMap)
	m = map[string]string{}
	for k, v := range anyMap {
		var s string
		if json.Unmarshal(v, &s) == nil {
			m[k] = s
		}
	}
	return m["password"]
}

func TestValidation(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	cases := []struct {
		ns, patch, field string
	}{
		{"smtp", `{"port":70000}`, "smtp.port"},
		{"smtp", `{"enabled":true}`, "smtp.host"},
		{"smtp", `{"from_name":"x\r\nBcc: victim@example.com"}`, "smtp.from_name"},
		{"smtp", `{"from_address":"Evil <a@b.c>"}`, "smtp.from_address"},
		{"smtp", `{"security":"ssl"}`, "smtp.security"},
		{"smtp", `{"nope":1}`, "smtp.nope"},
		{"smtp", `{"port":"25"}`, "smtp.port"},
		{"smtp", `{"password":123}`, "smtp.password"},
		{"general", `{"server_name":"  "}`, "general.server_name"},
		{"general", `{"public_url":"javascript:alert(1)"}`, "general.public_url"},
		{"general", `{"default_role":"root"}`, "general.default_role"},
		{"security", `{"password_min_length":4}`, "security.password_min_length"},
		{"security", `{"allowed_extension_origins":["https://evil.com"]}`, "security.allowed_extension_origins"},
		{"imap", `{"poll_minutes":0}`, "imap.poll_minutes"},
		{"bogus", `{}`, "bogus"},
		{"general", `[1,2]`, "general"},
	}
	for _, c := range cases {
		err := s.Update(ctx, c.ns, json.RawMessage(c.patch))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s %s: err = %v, want ValidationError", c.ns, c.patch, err)
			continue
		}
		if _, ok := ve.Fields[c.field]; !ok {
			t.Errorf("%s %s: fields = %v, want key %s", c.ns, c.patch, ve.Fields, c.field)
		}
	}
	// Nothing was persisted by the failed updates.
	m, _ := s.SMTP(ctx)
	if m.Port != 587 {
		t.Fatalf("port changed to %d", m.Port)
	}
}

func TestUpdateManyAtomicAndNormalisation(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	var changed []string
	s.OnChange(func(ns string) { changed = append(changed, ns) })
	err := s.UpdateMany(ctx, map[string]json.RawMessage{
		"general":  json.RawMessage(`{"server_name":"Home NAS"}`),
		"security": json.RawMessage(`{"session_idle_minutes":1}`), // invalid
	})
	if err == nil {
		t.Fatal("expected error")
	}
	g, _ := s.General(ctx)
	if g.ServerName != "Ludworks" || len(changed) != 0 {
		t.Fatal("partial update applied")
	}
	err = s.UpdateMany(ctx, map[string]json.RawMessage{
		"general":  json.RawMessage(`{"server_name":"  Home NAS ","public_url":"https://nas.example.com/"}`),
		"security": json.RawMessage(`{"allowed_extension_origins":["Chrome-Extension://abcdefghijklmnopabcdefghijklmnop/","chrome-extension://abcdefghijklmnopabcdefghijklmnop"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	g, _ = s.General(ctx)
	sec, _ := s.Security(ctx)
	if g.ServerName != "Home NAS" || g.PublicURL != "https://nas.example.com" {
		t.Fatalf("%+v", g)
	}
	if len(sec.AllowedExtensionOrigins) != 1 || sec.AllowedExtensionOrigins[0] != "chrome-extension://abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("%+v", sec.AllowedExtensionOrigins)
	}
	if len(changed) != 2 {
		t.Fatalf("changed = %v", changed)
	}
	// null resets a plain field to its default.
	s.Update(ctx, "general", json.RawMessage(`{"server_name":null}`))
	g, _ = s.General(ctx)
	if g.ServerName != "Ludworks" {
		t.Fatalf("reset failed: %+v", g)
	}
}

func TestModuleNamespaceRegistration(t *testing.T) {
	s, _, _ := newStore(t)
	type media struct {
		FFmpeg string `json:"ffmpeg_path"`
		Key    string `json:"tmdb_api_key"`
		Auto   bool   `json:"auto_probe"`
	}
	s.Register("media", media{Auto: true}, []string{"tmdb_api_key"}, Validator(func(m *media) error {
		if m.FFmpeg != "" && !filepath.IsAbs(m.FFmpeg) {
			return FieldErrors{"ffmpeg_path": "must be absolute"}
		}
		return nil
	}))
	ctx := context.Background()
	if err := s.Update(ctx, "media", json.RawMessage(`{"ffmpeg_path":"rel/ffmpeg"}`)); err == nil {
		t.Fatal("validator not applied")
	}
	if err := s.Update(ctx, "media", json.RawMessage(`{"tmdb_api_key":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	var m media
	s.Get(ctx, "media", &m)
	if m.Key != "abc" || !m.Auto {
		t.Fatalf("%+v", m)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	s.Register("media", media{}, nil, nil)
}

func TestReopenReadsPersisted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d, _ := db.Open(ctx, filepath.Join(dir, "s.db"))
	key, _ := LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	s, _ := New(ctx, d, key)
	if err := s.Update(ctx, "imap", json.RawMessage(`{"password":"pw","host":"imap.example.com"}`)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d2, _ := db.Open(ctx, filepath.Join(dir, "s.db"))
	defer d2.Close()
	s2, err := New(ctx, d2, key)
	if err != nil {
		t.Fatal(err)
	}
	i, err := s2.IMAP(ctx)
	if err != nil || i.Password != "pw" || i.Host != "imap.example.com" {
		t.Fatalf("%+v %v", i, err)
	}
	// Wrong key: the secret cannot be decrypted and Get reports an error instead of garbage.
	other := make([]byte, 32)
	other[0] = 1
	s3, _ := New(ctx, d2, other)
	if _, err := s3.IMAP(ctx); err == nil {
		t.Fatal("expected decrypt error with wrong key")
	}
}
