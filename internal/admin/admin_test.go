package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/mail/mailtest"
)

const pw = "user password 1234"

func newEnv(t *testing.T) (*authtest.Env, *authtest.Client) {
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		m := mail.New(e.Deps)
		return []httpx.Module{m, admin.New(e.Deps, e.Auth, m)}
	}})
	return e, e.Setup()
}

func create(t *testing.T, c *authtest.Client, name, role string) map[string]any {
	t.Helper()
	return c.Do("POST", "/api/v1/admin/users", map[string]any{
		"username": name, "email": name + "@example.com", "password": pw, "role": role,
	}).Expect(201).Map()
}

func login(t *testing.T, e *authtest.Env, name string) *authtest.Client {
	t.Helper()
	return e.TokenLogin(name, pw)
}

func TestAccessControl(t *testing.T) {
	e, adm := newEnv(t)
	create(t, adm, "bob", "user")
	e.NewClient().Do("GET", "/api/v1/admin/users", nil).ExpectError(401, "unauthorized")
	login(t, e, "bob").Do("GET", "/api/v1/admin/users", nil).ExpectError(403, "forbidden")
	adm.Do("GET", "/api/v1/admin/users", nil).Expect(200)

	tok := func(scopes ...string) *authtest.Client {
		r := adm.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "t", "scopes": scopes}).Expect(201).Map()
		return e.WithBearer(r["token"].(string))
	}
	tok("files:read").Do("GET", "/api/v1/admin/users", nil).ExpectError(403, "forbidden")
	tok("admin").Do("GET", "/api/v1/admin/settings", nil).Expect(200)
	// Cookie-authenticated admin writes need the CSRF token.
	csrf := adm.CSRF
	adm.CSRF = ""
	adm.Do("POST", "/api/v1/admin/db-backup", nil).ExpectError(403, "forbidden")
	adm.CSRF = csrf
}

func TestUserManagement(t *testing.T) {
	e, adm := newEnv(t)
	e.Patch("general", map[string]any{"default_quota_bytes": 5000})
	bob := create(t, adm, "bob", "user")
	if bob["quota_bytes"] != float64(5000) || bob["display_name"] != "bob" || bob["role"] != "user" || bob["email_verified"] != false {
		t.Fatalf("create: %v", bob)
	}
	r := adm.Do("POST", "/api/v1/admin/users", map[string]any{"username": "carol", "email": "carol@example.com", "password": pw,
		"role": "admin", "quota_bytes": 0, "display_name": "Carol C."}).Expect(201).Map()
	if r["role"] != "admin" || r["quota_bytes"] != float64(0) || r["display_name"] != "Carol C." {
		t.Fatal(r)
	}
	adm.Do("POST", "/api/v1/admin/users", map[string]any{"username": "BOB", "email": "x@example.com", "password": pw, "role": "user"}).
		ExpectError(409, "conflict")
	adm.Do("POST", "/api/v1/admin/users", map[string]any{"username": "bob2", "email": "BOB@example.com", "password": pw, "role": "user"}).
		ExpectError(409, "conflict")
	v := adm.Do("POST", "/api/v1/admin/users", map[string]any{"username": "bad name", "email": "x", "password": "short", "role": "root", "quota_bytes": -1}).
		ExpectError(422, "validation_failed").Map()
	fields := v["error"].(map[string]any)["details"].(map[string]any)["fields"].(map[string]any)
	for _, f := range []string{"username", "email", "password", "role", "quota_bytes"} {
		if fields[f] == nil {
			t.Errorf("no error for %s: %v", f, fields)
		}
	}
	for _, n := range []string{"dave", "erin", "frank"} {
		create(t, adm, n, "user")
	}

	// Listing: sorted by username, filter, pagination.
	var page struct {
		Items      []auth.User `json:"items"`
		NextCursor *string     `json:"next_cursor"`
	}
	adm.Do("GET", "/api/v1/admin/users", nil).Expect(200).JSON(&page)
	var names []string
	for _, u := range page.Items {
		names = append(names, u.Username)
	}
	if strings.Join(names, ",") != "admin,bob,carol,dave,erin,frank" || page.NextCursor != nil {
		t.Fatalf("list: %v", names)
	}
	adm.Do("GET", "/api/v1/admin/users?q=CAROL", nil).Expect(200).JSON(&page)
	if len(page.Items) != 1 || page.Items[0].Username != "carol" {
		t.Fatal(page.Items)
	}
	adm.Do("GET", "/api/v1/admin/users?q=%25", nil).Expect(200).JSON(&page) // LIKE wildcards are literal
	if len(page.Items) != 0 {
		t.Fatal("wildcard not escaped")
	}
	var all []string
	cursor := ""
	for i := 0; i < 10; i++ {
		adm.Do("GET", "/api/v1/admin/users?limit=4&cursor="+cursor, nil).Expect(200).JSON(&page)
		for _, u := range page.Items {
			all = append(all, u.Username)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if strings.Join(all, ",") != "admin,bob,carol,dave,erin,frank" {
		t.Fatalf("paged: %v", all)
	}
	adm.Do("GET", "/api/v1/admin/users?cursor=forged", nil).ExpectError(400, "bad_request")

	// Detail with usage (0 until the files table exists).
	id := bob["id"].(string)
	d := adm.Do("GET", "/api/v1/admin/users/"+id, nil).Expect(200).Map()
	if d["usage_bytes"] != float64(0) || d["username"] != "bob" {
		t.Fatal(d)
	}
	db := e.Deps.DB.SQL
	if _, err := db.Exec(`CREATE TABLE files (id TEXT PRIMARY KEY, owner_id TEXT, size INTEGER, is_dir INTEGER, trashed_at TEXT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO files VALUES ('f1', ?, 100, 0, NULL), ('f2', ?, 50, 0, NULL), ('f3', ?, 1000, 0, '2026-01-01T00:00:00.000Z'),
		('d1', ?, 4096, 1, NULL), ('f4', 'other', 7, 0, NULL)`, id, id, id, id)
	if d := adm.Do("GET", "/api/v1/admin/users/"+id, nil).Expect(200).Map(); d["usage_bytes"] != float64(150) {
		t.Fatalf("usage: %v", d["usage_bytes"])
	}
	adm.Do("GET", "/api/v1/admin/users/doesnotexist1", nil).ExpectError(404, "not_found")
	adm.Do("GET", "/api/v1/admin/users/bad!", nil).ExpectError(404, "not_found")

	// Patch.
	u := adm.Do("PATCH", "/api/v1/admin/users/"+id, map[string]any{"quota_bytes": 10, "display_name": "Bobby", "email": "bobby@example.com"}).Expect(200).Map()
	if u["quota_bytes"] != float64(10) || u["display_name"] != "Bobby" || u["email"] != "bobby@example.com" || u["email_verified"] != false {
		t.Fatal(u)
	}
	u = adm.Do("PATCH", "/api/v1/admin/users/"+id, map[string]any{"email_verified": true}).Expect(200).Map()
	if u["email_verified"] != true {
		t.Fatal(u)
	}
	adm.Do("PATCH", "/api/v1/admin/users/"+id, map[string]any{"email": "carol@example.com"}).ExpectError(409, "conflict")
	adm.Do("PATCH", "/api/v1/admin/users/"+id, map[string]any{"role": "god"}).ExpectError(422, "validation_failed")
	adm.Do("PATCH", "/api/v1/admin/users/doesnotexist1", map[string]any{"quota_bytes": 1}).ExpectError(404, "not_found")
	// Promotion takes effect immediately for existing sessions.
	b := login(t, e, "bob")
	b.Do("GET", "/api/v1/admin/users", nil).Expect(403)
	adm.Do("PATCH", "/api/v1/admin/users/"+id, map[string]any{"role": "admin"}).Expect(200)
	b.Do("GET", "/api/v1/admin/users", nil).Expect(200)

	entries := auditEntries(t, adm, "admin.user.update")
	if len(entries) != 3 {
		t.Fatalf("update audit entries: %d", len(entries))
	}
	if len(auditEntries(t, adm, "admin.user.create")) != 5 {
		t.Fatal("create not audited")
	}
}

func TestLastAdminProtection(t *testing.T) {
	e, adm := newEnv(t)
	me := adm.Do("GET", "/api/v1/me", nil).Map()
	myID := me["id"].(string)
	adm.Do("PATCH", "/api/v1/admin/users/"+myID, map[string]any{"role": "user"}).ExpectError(409, "conflict")
	adm.Do("PATCH", "/api/v1/admin/users/"+myID, map[string]any{"disabled": true}).ExpectError(409, "conflict")
	adm.Do("DELETE", "/api/v1/admin/users/"+myID, nil).ExpectError(409, "conflict")
	if r := adm.Do("GET", "/api/v1/me", nil).Map(); r["role"] != "admin" || r["disabled"] != false {
		t.Fatal("refused change was applied")
	}

	other := create(t, adm, "root2", "admin")
	oid := other["id"].(string)
	// A disabled admin does not count as an active one.
	adm.Do("PATCH", "/api/v1/admin/users/"+oid, map[string]any{"disabled": true}).Expect(200)
	adm.Do("PATCH", "/api/v1/admin/users/"+myID, map[string]any{"role": "user"}).ExpectError(409, "conflict")
	adm.Do("PATCH", "/api/v1/admin/users/"+oid, map[string]any{"disabled": false}).Expect(200)
	// With a second active admin, self-demotion is allowed (but never self-disable).
	adm.Do("PATCH", "/api/v1/admin/users/"+myID, map[string]any{"disabled": true}).ExpectError(409, "conflict")
	adm.Do("PATCH", "/api/v1/admin/users/"+myID, map[string]any{"role": "user"}).Expect(200)
	adm.Do("GET", "/api/v1/admin/users", nil).ExpectError(403, "forbidden")
	r2 := login(t, e, "root2")
	r2.Do("PATCH", "/api/v1/admin/users/"+oid, map[string]any{"role": "user"}).ExpectError(409, "conflict")
	r2.Do("DELETE", "/api/v1/admin/users/"+myID, nil).Expect(204)
	adm.Do("GET", "/api/v1/me", nil).Expect(401)
}

func TestDisableRevokesEverything(t *testing.T) {
	e, adm := newEnv(t)
	u := create(t, adm, "bob", "user")
	b := login(t, e, "bob")
	pat := b.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "p", "scopes": []string{"files:read"}}).Expect(201).Map()["token"].(string)
	adm.Do("PATCH", "/api/v1/admin/users/"+u["id"].(string), map[string]any{"disabled": true}).Expect(200)
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	e.WithBearer(pat).Do("GET", "/api/v1/me", nil).Expect(401)
	e.NewClient().Login("bob", pw).ExpectError(403, "forbidden")
	adm.Do("PATCH", "/api/v1/admin/users/"+u["id"].(string), map[string]any{"disabled": false}).Expect(200)
	e.NewClient().Login("bob", pw).Expect(200)
}

type recorder struct {
	mu  sync.Mutex
	got map[string][]string
}

func (r *recorder) sub(b *events.Bus, topic string) {
	b.Subscribe(topic, func(p any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got[topic] = append(r.got[topic], p.(string))
	})
}

func (r *recorder) wait(t *testing.T, topic string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		v := append([]string(nil), r.got[topic]...)
		r.mu.Unlock()
		if len(v) >= n || time.Now().After(deadline) {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeleteUser(t *testing.T) {
	e, adm := newEnv(t)
	rec := &recorder{got: map[string][]string{}}
	rec.sub(e.Deps.Events, events.UserDeleted)
	rec.sub(e.Deps.Events, admin.TopicUserDeleteFiles)

	bob := create(t, adm, "bob", "user")
	carol := create(t, adm, "carol", "user")
	b := login(t, e, "bob")
	adm.Do("DELETE", "/api/v1/admin/users/"+bob["id"].(string)+"?delete_files=true", nil).Expect(204)
	adm.Do("DELETE", "/api/v1/admin/users/"+carol["id"].(string), nil).Expect(204)
	adm.Do("DELETE", "/api/v1/admin/users/"+carol["id"].(string), nil).ExpectError(404, "not_found")
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	e.NewClient().Login("bob", pw).ExpectError(401, "invalid_credentials")
	adm.Do("GET", "/api/v1/admin/users/"+bob["id"].(string), nil).Expect(404)
	var n int
	e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = ?`, bob["id"]).Scan(&n)
	if n != 0 {
		t.Fatal("sessions survived deletion")
	}
	if got := rec.wait(t, events.UserDeleted, 2); len(got) != 2 || got[0] != bob["id"] || got[1] != carol["id"] {
		t.Fatalf("user.deleted events: %v", got)
	}
	if got := rec.wait(t, admin.TopicUserDeleteFiles, 1); len(got) != 1 || got[0] != bob["id"] {
		t.Fatalf("delete_files events: %v", got)
	}
	del := auditEntries(t, adm, "admin.user.delete")
	if len(del) != 2 {
		t.Fatalf("delete audit: %v", del)
	}
	for _, d := range del {
		meta := d["meta"].(map[string]any)
		if (meta["username"] == "bob") != (meta["delete_files"] == true) || d["target_id"] == nil {
			t.Fatalf("delete audit: %v", del)
		}
	}
}

func TestPasswordSessionsTOTP(t *testing.T) {
	e, adm := newEnv(t)
	u := create(t, adm, "bob", "user")
	id := u["id"].(string)
	b := login(t, e, "bob")
	pat := b.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "p", "scopes": []string{"files:read"}}).Expect(201).Map()["token"].(string)

	adm.Do("POST", "/api/v1/admin/users/"+id+"/password", map[string]any{"new_password": "short"}).ExpectError(422, "validation_failed")
	adm.Do("POST", "/api/v1/admin/users/"+id+"/password", map[string]any{"new_password": "an admin chosen password"}).Expect(204)
	adm.Do("POST", "/api/v1/admin/users/doesnotexist1/password", map[string]any{"new_password": "an admin chosen password"}).Expect(404)
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	e.WithBearer(pat).Do("GET", "/api/v1/me", nil).Expect(200)
	b = e.TokenLogin("bob", "an admin chosen password")

	adm.Do("POST", "/api/v1/admin/users/"+id+"/revoke-sessions", map[string]any{}).Expect(204)
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	e.WithBearer(pat).Do("GET", "/api/v1/me", nil).Expect(401)
	adm.Do("POST", "/api/v1/admin/users/doesnotexist1/revoke-sessions", nil).Expect(404)

	// Lost authenticator: the admin removes 2FA.
	b = e.TokenLogin("bob", "an admin chosen password")
	s := b.Do("POST", "/api/v1/me/totp/setup", nil).Expect(200).Map()
	code, _ := auth.GenerateTOTP(s["secret"].(string), time.Now())
	b.Do("POST", "/api/v1/me/totp/enable", map[string]any{"code": code}).Expect(200)
	e.NewClient().Do("POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": "an admin chosen password"}).ExpectError(401, "totp_required")
	adm.Do("DELETE", "/api/v1/admin/users/"+id+"/totp", nil).Expect(204)
	if d := adm.Do("GET", "/api/v1/admin/users/"+id, nil).Map(); d["totp_enabled"] != false {
		t.Fatal(d)
	}
	e.NewClient().Login("bob", "an admin chosen password").Expect(200)
	for _, a := range []string{"admin.user.password", "admin.user.revoke_sessions", "admin.user.totp_removed"} {
		if len(auditEntries(t, adm, a)) != 1 {
			t.Errorf("%s not audited", a)
		}
	}
}

func TestSettings(t *testing.T) {
	e, adm := newEnv(t)
	s := adm.Do("GET", "/api/v1/admin/settings", nil).Expect(200).Map()
	for _, ns := range []string{"general", "security", "smtp", "imap"} {
		if s[ns] == nil {
			t.Fatalf("namespace %s missing: %v", ns, s)
		}
	}
	if pwv := s["smtp"].(map[string]any)["password"].(map[string]any); pwv["set"] != false {
		t.Fatal(pwv)
	}
	const secret = "hunter2-very-secret"
	r := adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{
		"smtp":    map[string]any{"enabled": true, "host": "mail.example.com", "from_address": "nas@example.com", "username": "nas", "password": secret, "port": 2525},
		"general": map[string]any{"server_name": "Attic"},
	}).Expect(200)
	if strings.Contains(string(r.Body), secret) {
		t.Fatal("secret echoed")
	}
	m := r.Map()
	smtp := m["smtp"].(map[string]any)
	if smtp["password"].(map[string]any)["set"] != true || smtp["port"] != float64(2525) || m["general"].(map[string]any)["server_name"] != "Attic" {
		t.Fatal(m)
	}
	var raw string
	e.Deps.DB.SQL.QueryRow(`SELECT json FROM settings WHERE ns = 'smtp'`).Scan(&raw)
	if strings.Contains(raw, secret) || !strings.Contains(raw, `"password":"v1.`) {
		t.Fatalf("secret stored in clear: %s", raw)
	}
	if got, _ := e.Deps.Settings.SMTP(context.Background()); got.Password != secret {
		t.Fatal("secret not decryptable")
	}
	// Round-tripping the redacted form keeps the secret; null clears it.
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp": smtp}).Expect(200)
	if got, _ := e.Deps.Settings.SMTP(context.Background()); got.Password != secret {
		t.Fatal("redacted round trip lost the secret")
	}
	m = adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp": map[string]any{"password": nil}}).Expect(200).Map()
	if m["smtp"].(map[string]any)["password"].(map[string]any)["set"] != false {
		t.Fatal("secret not cleared")
	}
	// Validation is atomic across namespaces.
	v := adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{
		"general": map[string]any{"server_name": "Changed"},
		"smtp":    map[string]any{"port": 0},
	}).ExpectError(422, "validation_failed").Map()
	if v["error"].(map[string]any)["details"].(map[string]any)["fields"].(map[string]any)["smtp.port"] == nil {
		t.Fatal(v)
	}
	if g, _ := e.Deps.Settings.General(context.Background()); g.ServerName != "Attic" {
		t.Fatal("partial update applied")
	}
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"nope": map[string]any{}}).ExpectError(422, "validation_failed")
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"general": map[string]any{"bogus": 1}}).ExpectError(422, "validation_failed")
	adm.Do("PATCH", "/api/v1/admin/settings", []byte(`[1]`)).ExpectError(400, "bad_request")
	// Registration toggle is visible publicly.
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"general": map[string]any{"registration_enabled": true}}).Expect(200)
	if st := e.NewClient().Do("GET", "/api/v1/setup/status", nil).Map(); st["registration_enabled"] != true {
		t.Fatal(st)
	}

	entries := auditEntries(t, adm, "admin.settings.update")
	if len(entries) != 4 {
		t.Fatalf("settings audit entries: %d", len(entries))
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "mail.example.com") {
		t.Fatal("audit log contains setting values")
	}
	// Entries in the same millisecond have no defined order: find the first PATCH by content.
	found := false
	for _, en := range entries {
		ch := en["meta"].(map[string]any)["changes"].(map[string]any)
		if g, ok := ch["general"].([]any); ok && len(g) == 1 && g[0] == "server_name" && len(ch["smtp"].([]any)) == 6 {
			found = true
		}
	}
	if !found {
		t.Fatalf("first settings change not audited with field names: %v", entries)
	}
}

func TestSMTPAndIMAPTest(t *testing.T) {
	e, adm := newEnv(t)
	adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "ops@example.com"}).ExpectError(422, "validation_failed")
	srv := mailtest.Start(t, mailtest.Options{})
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp": map[string]any{
		"host": srv.Host, "port": srv.Port, "security": "none", "from_address": "nas@example.com"}}).Expect(200)
	adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "bad\r\nBcc: x@y.z"}).ExpectError(422, "validation_failed")
	adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "ops@"}).ExpectError(422, "validation_failed")
	adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "Ops <ops@example.com>"}).ExpectError(422, "validation_failed")
	adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "ops@example.com"}).Expect(204)
	msgs := srv.Messages()
	if len(msgs) != 1 || msgs[0].To[0] != "ops@example.com" || !strings.Contains(msgs[0].Data, "Subject: Ludworks test email") {
		t.Fatalf("test mail: %+v", msgs)
	}
	bad := mailtest.Start(t, mailtest.Options{RejectRcpt: true})
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp": map[string]any{"port": bad.Port}}).Expect(200)
	r := adm.Do("POST", "/api/v1/admin/settings/smtp/test", map[string]any{"to": "ops@example.com"}).ExpectError(502, "smtp_failed")
	if !strings.Contains(string(r.Body), "550") {
		t.Fatalf("error should carry the server's reason: %s", r.Body)
	}
	if n := len(auditEntries(t, adm, "admin.settings.smtp_test")); n != 3 {
		t.Fatalf("smtp test audit entries: %d", n)
	}

	// IMAP: unconfigured is a validation error naming the missing fields.
	const imapTest = "/api/v1/admin/settings/imap/test"
	r = adm.Do("POST", imapTest, map[string]any{}).ExpectError(422, "validation_failed")
	if !strings.Contains(string(r.Body), "imap.host") || !strings.Contains(string(r.Body), "imap.username") {
		t.Fatalf("imap validation should name the missing fields: %s", r.Body)
	}
	// Configured against a loopback fake server: lists mailboxes and counts unseen mail.
	imapSrv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: "imap-pass-123", Mailboxes: []string{"Archive"}})
	imapSrv.Add("INBOX", []byte("Subject: one\r\n\r\nhello\r\n"))
	imapSrv.Add("INBOX", []byte("Subject: two\r\n\r\nworld\r\n"))
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{
		"host": imapSrv.Host, "port": imapSrv.Port, "security": "none", "username": "inbox", "password": "imap-pass-123"}}).Expect(200)
	var res struct {
		OK        bool     `json:"ok"`
		Mailboxes []string `json:"mailboxes"`
		Unseen    int      `json:"unseen"`
	}
	adm.Do("POST", imapTest, map[string]any{}).Expect(200).JSON(&res)
	if !res.OK || res.Unseen != 2 || !strings.Contains(strings.Join(res.Mailboxes, ","), "Archive") {
		t.Fatalf("imap test result: %+v", res)
	}
	// Wrong password: 502 imap_failed, and the error never echoes the secret.
	adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"imap": map[string]any{"password": "wrong-pass-999"}}).Expect(200)
	r = adm.Do("POST", imapTest, map[string]any{}).ExpectError(502, "imap_failed")
	if strings.Contains(string(r.Body), "wrong-pass-999") {
		t.Fatalf("imap error leaks the password: %s", r.Body)
	}
	if n := len(auditEntries(t, adm, "admin.settings.imap_test")); n != 3 {
		t.Fatalf("imap test audit entries: %d", n)
	}
	_ = e
}

func auditEntries(t *testing.T, c *authtest.Client, action string) []map[string]any {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	c.Do("GET", "/api/v1/admin/audit?limit=200&action="+action, nil).Expect(200).JSON(&page)
	return page.Items
}

func TestAuditLog(t *testing.T) {
	e, adm := newEnv(t)
	create(t, adm, "bob", "user")
	e.NewClient().Login("ghost", "nope nope nope").Expect(401)
	login(t, e, "bob")
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	adm.Do("GET", "/api/v1/admin/audit", nil).Expect(200).JSON(&page)
	bobLogin := false
	for _, it := range page.Items {
		bobLogin = bobLogin || (it["action"] == "auth.login" && it["actor_username"] == "bob")
	}
	if len(page.Items) != 4 || !bobLogin {
		t.Fatalf("audit: %v", page.Items)
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1]["time"].(string) < page.Items[i]["time"].(string) {
			t.Fatal("not newest first")
		}
	}
	failed := auditEntries(t, adm, "auth.login_failed")
	if len(failed) != 1 || failed[0]["actor_id"] != nil || failed[0]["actor_username"] != "ghost" || failed[0]["ip"] != "127.0.0.1" {
		t.Fatalf("failed login entry: %v", failed)
	}
	me := adm.Do("GET", "/api/v1/me", nil).Map()
	adm.Do("GET", "/api/v1/admin/audit?actor_id="+me["id"].(string), nil).Expect(200).JSON(&page)
	for _, it := range page.Items {
		if it["actor_id"] != me["id"] {
			t.Fatal("actor filter ignored")
		}
	}
	adm.Do("GET", "/api/v1/admin/audit?action=auth.*", nil).Expect(200).JSON(&page)
	for _, it := range page.Items {
		if !strings.HasPrefix(it["action"].(string), "auth.") {
			t.Fatal("prefix filter ignored")
		}
	}
	// Pagination.
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 20; i++ {
		adm.Do("GET", "/api/v1/admin/audit?limit=2&cursor="+cursor, nil).Expect(200).JSON(&page)
		for _, it := range page.Items {
			if seen[it["id"].(string)] {
				t.Fatal("duplicate across pages")
			}
			seen[it["id"].(string)] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != 4 { // setup, user create, failed login, login
		t.Fatalf("paged %d entries", len(seen))
	}
	adm.Do("GET", "/api/v1/admin/audit?actor_id=bad!", nil).ExpectError(422, "validation_failed")
	adm.Do("GET", "/api/v1/admin/audit?cursor=xyz", nil).ExpectError(400, "bad_request")
}

func TestDBBackup(t *testing.T) {
	e, adm := newEnv(t)
	var out struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	adm.Do("POST", "/api/v1/admin/db-backup", map[string]any{}).Expect(200).JSON(&out)
	if filepath.Dir(out.Path) != e.Deps.Config.BackupsDir() || !strings.HasPrefix(filepath.Base(out.Path), "ludworks-") {
		t.Fatalf("path %s", out.Path)
	}
	st, err := os.Stat(out.Path)
	if err != nil || st.Size() != out.Size || out.Size == 0 {
		t.Fatalf("backup file: %v %v", st, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	hdr := make([]byte, 16)
	f, _ := os.Open(out.Path)
	f.Read(hdr)
	f.Close()
	if string(hdr) != "SQLite format 3\x00" {
		t.Fatal("not an SQLite database")
	}
	// Repeated backups in the same second get distinct names; only 10 are kept.
	for i := 0; i < 11; i++ {
		adm.Do("POST", "/api/v1/admin/db-backup", nil).Expect(200)
	}
	ents, _ := os.ReadDir(e.Deps.Config.BackupsDir())
	if len(ents) != 10 {
		t.Fatalf("%d backups kept", len(ents))
	}
	if len(auditEntries(t, adm, "admin.db_backup")) != 12 {
		t.Fatal("backup not audited")
	}
	e.TokenLogin(authtest.AdminUser, authtest.AdminPassword).Do("POST", "/api/v1/admin/db-backup", nil).Expect(http.StatusOK) // bearer: no CSRF
}

func TestUserDeletedWhileRequestUsesCore(t *testing.T) {
	// core.Users stays consistent with admin operations.
	e, adm := newEnv(t)
	u := create(t, adm, "bob", "user")
	if got, err := e.Deps.Users.GetUser(context.Background(), u["id"].(string)); err != nil || got.Username != "bob" {
		t.Fatal(err)
	}
	adm.Do("DELETE", "/api/v1/admin/users/"+u["id"].(string), nil).Expect(204)
	if _, err := e.Deps.Users.GetUser(context.Background(), u["id"].(string)); err == nil {
		t.Fatal("deleted user still found")
	} else if ce, ok := err.(*core.Error); !ok || ce.Status != 404 {
		t.Fatal(err)
	}
}
