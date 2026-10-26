package auth_test

// Regression tests for security findings: concurrent lockout bypass, account enumeration
// through registration and email changes, browser state left behind on sign-out, and the
// setup token in the service log.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
)

// burst sends n requests at once while every password-hashing slot is held: requests the
// brute-force defences admit block inside the password check, refused ones answer at once.
// It returns how many responses had each status code.
func burst(t *testing.T, n int, mk func() *http.Request) map[int]int {
	t.Helper()
	release := auth.HoldHashSlots()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.DefaultClient.Do(mk())
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	got := map[int]int{}
	received := 0
	timeout := time.After(1500 * time.Millisecond)
wait:
	for received < n {
		select {
		case c := <-codes:
			got[c]++
			received++
		case <-timeout:
			break wait
		}
	}
	release()
	released = true
	for ; received < n; received++ {
		got[<-codes]++
	}
	return got
}

func jsonRequest(t *testing.T, method, url string, body any, hdr map[string]string) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

// The exponential lockout must hold for a simultaneous burst too: at most the free failures
// plus the one that engages the lock may reach password verification.
func TestLoginLockoutHoldsUnderConcurrentBurst(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	got := burst(t, 20, func() *http.Request {
		return jsonRequest(t, "POST", e.URL+"/api/v1/auth/login", map[string]any{"username": "bob", "password": "wrong password guess"}, nil)
	})
	if got[401] > 6 || got[401]+got[429] != 20 {
		t.Fatalf("burst of 20 wrong passwords: %v (want at most 6 verified, the rest 429)", got)
	}
	// The account is now locked for everyone, including the right password.
	e.NewClient().Login("bob", userPW).ExpectError(429, "rate_limited")
}

// Re-authentication (password change, TOTP) has the same guarantee per user.
func TestReauthLockoutHoldsUnderConcurrentBurst(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	got := burst(t, 20, func() *http.Request {
		return jsonRequest(t, "POST", e.URL+"/api/v1/me/password",
			map[string]any{"current_password": "wrong password guess", "new_password": "another good password"},
			map[string]string{"Authorization": "Bearer " + b.Bearer})
	})
	if got[401] > 6 || got[401]+got[429] != 20 {
		t.Fatalf("burst of 20 wrong current passwords: %v (want at most 6 verified, the rest 429)", got)
	}
	b.Do("POST", "/api/v1/me/password", map[string]any{"current_password": userPW, "new_password": "another good password"}).
		ExpectError(429, "rate_limited")
}

// Signing out of a browser clears the private file bytes and thumbnails it cached.
func TestBrowserSignOutClearsCache(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	c := e.NewClient()
	c.Login("bob", userPW).Expect(200)
	if got := c.Do("POST", "/api/v1/auth/logout", nil).Expect(204).Header.Get("Clear-Site-Data"); got != `"cache"` {
		t.Fatalf("cookie logout Clear-Site-Data = %q", got)
	}
	// Bearer clients have no browser cache to clear.
	b := e.TokenLogin("bob", userPW)
	if got := b.Do("POST", "/api/v1/auth/logout", nil).Expect(204).Header.Get("Clear-Site-Data"); got != "" {
		t.Fatalf("bearer logout Clear-Site-Data = %q", got)
	}
	// Revoking the current browser session from the session list is a sign-out as well;
	// revoking another session leaves this browser alone.
	c = e.NewClient()
	c.Login("bob", userPW).Expect(200)
	other := e.NewClient()
	other.Login("bob", userPW).Expect(200)
	var list []map[string]any
	c.Do("GET", "/api/v1/me/sessions", nil).Expect(200).JSON(&list)
	var current, second string
	for _, s := range list {
		if s["current"] == true {
			current = s["id"].(string)
		} else if s["kind"] == "session" {
			second = s["id"].(string)
		}
	}
	if got := c.Do("DELETE", "/api/v1/me/sessions/"+second, nil).Expect(204).Header.Get("Clear-Site-Data"); got != "" {
		t.Fatalf("revoking another session: Clear-Site-Data = %q", got)
	}
	if got := c.Do("DELETE", "/api/v1/me/sessions/"+current, nil).Expect(204).Header.Get("Clear-Site-Data"); got != `"cache"` {
		t.Fatalf("revoking the current session: Clear-Site-Data = %q", got)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countUsersNamed(t *testing.T, e *authtest.Env, name string) int {
	t.Helper()
	var n int
	if err := e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM users WHERE username = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With email verification on, registering with an address that already has an account
// answers exactly like a fresh registration and tells the address owner instead.
func TestRegisterDoesNotRevealTakenEmail(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	e.Patch("general", map[string]any{"registration_enabled": true, "require_email_verification": true})
	reg := func(name, email string) *authtest.Response {
		return e.NewClient().Do("POST", "/api/v1/auth/register", map[string]any{"username": name, "email": email, "password": userPW})
	}
	fresh := reg("newbie", "newbie@example.com").Expect(201).Map()
	decoy := reg("other", "BOB@example.com").Expect(201).Map()
	if strings.Join(sortedKeys(fresh), ",") != strings.Join(sortedKeys(decoy), ",") || decoy["verification_required"] != true {
		t.Fatalf("responses differ: %v vs %v", fresh, decoy)
	}
	fu, du := fresh["user"].(map[string]any), decoy["user"].(map[string]any)
	if strings.Join(sortedKeys(fu), ",") != strings.Join(sortedKeys(du), ",") ||
		du["username"] != "other" || du["email"] != "BOB@example.com" || du["email_verified"] != false || du["role"] != "user" {
		t.Fatalf("user objects differ: %v vs %v", fu, du)
	}
	if countUsersNamed(t, e, "other") != 0 {
		t.Fatal("an account was created for a taken email address")
	}
	msgs := e.Mailer.Wait(t, 2)
	var notice *core.MailMessage
	for i := range msgs {
		if msgs[i].To[0] == "bob@example.com" {
			notice = &msgs[i]
		}
	}
	if notice == nil || strings.Contains(notice.Text, "?token=") || !strings.Contains(notice.Text, "password") {
		t.Fatalf("no notice to the address owner: %+v", msgs)
	}
	if len(auditActions(t, e, "auth.register_conflict")) != 1 {
		t.Fatal("conflicting registration not audited")
	}
	// The decoy did not reserve the username.
	reg("other", "other@example.com").Expect(201)
	// Usernames are public by nature on a server with open registration.
	reg("bob", "fresh@example.com").ExpectError(409, "conflict")
	// Without verification the new account would be signed in at once, so a taken address
	// cannot be hidden; it is still reported.
	e.Patch("general", map[string]any{"require_email_verification": false})
	reg("third", "bob@example.com").ExpectError(409, "conflict")
}

// PATCH /me with a new email answers the same whether or not the address is taken; the
// change only happens once the new address confirms it.
func TestEmailChangeDoesNotRevealTakenAddress(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	createUser(t, e, "carol", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	taken := b.Do("PATCH", "/api/v1/me", map[string]any{"email": "carol@example.com"}).Expect(200)
	free := b.Do("PATCH", "/api/v1/me", map[string]any{"email": "bob.new@example.com"}).Expect(200)
	if !bytes.Equal(taken.Body, free.Body) {
		t.Fatalf("taken and free addresses answer differently:\n%s\n%s", taken.Body, free.Body)
	}
	if m := free.Map(); m["email"] != "bob@example.com" || m["email_change_pending"] != true {
		t.Fatalf("email changed before confirmation: %v", m)
	}
	// Bob's current address is told about both requests; only the free new address gets a
	// confirmation link, and carol gets nothing.
	msgs := e.Mailer.Wait(t, 3)
	time.Sleep(100 * time.Millisecond)
	msgs = e.Mailer.Messages()
	var link *core.MailMessage
	notices := 0
	for i, m := range msgs {
		switch m.To[0] {
		case "bob@example.com":
			notices++
		case "bob.new@example.com":
			link = &msgs[i]
		default:
			t.Fatalf("unexpected mail to %s", m.To[0])
		}
	}
	if notices != 2 || link == nil {
		t.Fatalf("mails: %+v", msgs)
	}
	tok, _ := authtest.LinkToken(t, *link, "/verify-email")
	e.NewClient().Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": tok}).Expect(204)
	if me := b.Do("GET", "/api/v1/me", nil).Expect(200).Map(); me["email"] != "bob.new@example.com" || me["email_verified"] != true {
		t.Fatalf("after confirmation: %v", me)
	}
	if len(auditActions(t, e, "account.email_change_requested")) != 2 || len(auditActions(t, e, "account.email_changed")) != 1 {
		t.Fatal("email change not audited")
	}
	// An address taken between request and confirmation fails like any stale link.
	b.Do("PATCH", "/api/v1/me", map[string]any{"email": "dave@example.com"}).Expect(200)
	msgs = e.Mailer.Wait(t, len(msgs)+2)
	var daveLink *core.MailMessage
	for i := range msgs {
		if msgs[i].To[0] == "dave@example.com" {
			daveLink = &msgs[i]
		}
	}
	if daveLink == nil {
		t.Fatal("no confirmation link for dave@")
	}
	createUser(t, e, "dave", core.RoleUser)
	tok, _ = authtest.LinkToken(t, *daveLink, "/verify-email")
	e.NewClient().Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": tok}).ExpectError(410, "gone")
	if me := b.Do("GET", "/api/v1/me", nil).Map(); me["email"] != "bob.new@example.com" {
		t.Fatalf("email changed to a taken address: %v", me)
	}
	// Email change requests are rate limited per user.
	limited := false
	for i := 0; i < 10 && !limited; i++ {
		limited = b.Do("PATCH", "/api/v1/me", map[string]any{"email": "spam" + string(rune('a'+i)) + "@example.com"}).Status == 429
	}
	if !limited {
		t.Fatal("email change requests not rate limited")
	}
	// Other fields are not affected by that limit.
	b.Do("PATCH", "/api/v1/me", map[string]any{"display_name": "Bobby"}).Expect(200)
}

// Without SMTP a confirmation link cannot be delivered, so the change applies at once.
func TestEmailChangeWithoutMail(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	createUser(t, e, "carol", core.RoleUser)
	e.Mailer.SetEnabled(false)
	b := e.TokenLogin("bob", userPW)
	m := b.Do("PATCH", "/api/v1/me", map[string]any{"email": "bob2@example.com"}).Expect(200).Map()
	if m["email"] != "bob2@example.com" || m["email_verified"] != false {
		t.Fatalf("immediate change: %v", m)
	}
	if _, ok := m["email_change_pending"]; ok {
		t.Fatalf("pending flag on an immediate change: %v", m)
	}
	b.Do("PATCH", "/api/v1/me", map[string]any{"email": "carol@example.com"}).ExpectError(409, "conflict")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The setup token lives in the 0600 file; the log only says where, so log readers
// (journald, syslog, shippers) cannot claim the server.
func TestSetupTokenNotLogged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var logs lockedBuffer
	d := *e.Deps
	d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	m := auth.New(&d)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	path := e.Deps.Config.SetupTokenPath()
	tok := e.SetupToken()
	if strings.Contains(logs.String(), tok) || !strings.Contains(logs.String(), path) {
		t.Fatalf("start-up log leaks the token or lacks the file path:\n%s", logs.String())
	}
	// A rotation after expiry does not log the new token either.
	expire := func() {
		if _, err := e.Deps.DB.SQL.Exec(`UPDATE setup_token SET expires_at = '2000-01-01T00:00:00.000Z'`); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	if err := auth.RunSetupTokenRotation(m, ctx); err != nil {
		t.Fatal(err)
	}
	tok2 := e.SetupToken()
	if tok2 == tok || strings.Contains(logs.String(), tok2) {
		t.Fatal("rotated token logged (or not rotated)")
	}
	// When the file cannot be written the log is the only copy: then the token is logged.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	expire()
	if err := auth.RunSetupTokenRotation(m, ctx); err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`setup_token=([A-Za-z0-9_-]{20,})`).FindStringSubmatch(logs.String())
	if match == nil {
		t.Fatalf("fallback token not logged:\n%s", logs.String())
	}
	e.NewClient().Do("POST", "/api/v1/setup", map[string]any{
		"setup_token": match[1], "username": "root", "password": "a very good password", "email": "r@example.com",
	}).Expect(201)
}
