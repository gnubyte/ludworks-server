package mail_test

import (
	"strings"
	"testing"

	"ludworks/internal/admin"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/mail/mailtest"
)

// TestIMAPTestEndpoint drives POST /admin/settings/imap/test through the admin module.
func TestIMAPTestEndpoint(t *testing.T) {
	var mm *mail.Module
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		mm = mail.New(e.Deps)
		return []httpx.Module{mm, admin.New(e.Deps, e.Auth, mm)}
	}})
	adm := e.Setup()
	const path = "/api/v1/admin/settings/imap/test"
	adm.Do("POST", path, map[string]any{}).ExpectError(422, "validation_failed")

	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{STARTTLS: true, AuthPlain: true, User: "inbox", Pass: "pw-123456",
		Mailboxes: []string{"Archive"}})
	srv.Add("INBOX", []byte("Subject: hi\r\n\r\nhello\r\n"))
	mm.SetIMAPRootCAs(srv.Roots)
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{
		"host": srv.Host, "port": srv.Port, "security": "starttls", "username": "inbox", "password": "pw-123456"}}).Expect(200)
	var res struct {
		OK        bool     `json:"ok"`
		Mailboxes []string `json:"mailboxes"`
		Unseen    int      `json:"unseen"`
	}
	adm.Do("POST", path, map[string]any{}).Expect(200).JSON(&res)
	if !res.OK || res.Unseen != 1 || strings.Join(res.Mailboxes, ",") != "Archive,INBOX" {
		t.Fatalf("result %+v", res)
	}

	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{"password": "wrong-password"}}).Expect(200)
	r := adm.Do("POST", path, map[string]any{}).ExpectError(502, "imap_failed")
	if !strings.Contains(string(r.Body), "AUTHENTICATIONFAILED") || strings.Contains(string(r.Body), "wrong-password") {
		t.Fatalf("error body %s", r.Body)
	}

	// A plain server cannot satisfy starttls; 'none' to it is fine because it is loopback.
	plain := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: "pw-123456"})
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{"port": plain.Port, "password": "pw-123456"}}).Expect(200)
	r = adm.Do("POST", path, map[string]any{}).ExpectError(502, "imap_failed")
	if !strings.Contains(string(r.Body), "does not offer STARTTLS") {
		t.Fatalf("starttls body %s", r.Body)
	}
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{"security": "none"}}).Expect(200)
	adm.Do("POST", path, map[string]any{}).Expect(200)

	// Admins only.
	if _, err := e.Auth.CreateUser(t.Context(), auth.CreateUserInput{Username: "bob", Email: "bob@example.com",
		Password: authtest.AdminPassword, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	e.TokenLogin("bob", authtest.AdminPassword).Do("POST", path, map[string]any{}).ExpectError(403, "forbidden")
}
