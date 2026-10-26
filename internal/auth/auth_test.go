package auth_test

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

const userPW = "user password 1234"

func newEnv(t *testing.T) *authtest.Env {
	return authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module { return []httpx.Module{probe{}} }})
}

// probe registers routes exercising access rules auth itself does not use: admin-only, a
// scoped route and a query-token (streaming-style) route.
type probe struct{}

func (probe) Name() string                { return "probe" }
func (probe) Migrations() fs.FS           { return nil }
func (probe) Start(context.Context) error { return nil }
func (probe) OpenAPI() []byte             { return nil }
func (probe) Routes(r *httpx.Router) {
	ok := func(w http.ResponseWriter, r *http.Request) error {
		p := core.MustPrincipal(r.Context())
		return httpx.WriteJSON(w, 200, map[string]any{"user_id": p.UserID, "kind": p.Kind, "role": p.Role})
	}
	r.Handle("GET /api/v1/probe/admin", httpx.AdminOnly(), ok)
	r.Handle("GET /api/v1/probe/stream", httpx.Scope("files:read").WithQueryToken(), ok)
	r.Handle("GET /api/v1/probe/files", httpx.Scope("files:read"), ok)
}

// createUser adds a regular user directly through the account service.
func createUser(t *testing.T, e *authtest.Env, name string, role core.Role) auth.User {
	t.Helper()
	u, err := e.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: name, Email: name + "@example.com", Password: userPW, Role: string(role)})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func auditActions(t *testing.T, e *authtest.Env, action string) []audit.Entry {
	t.Helper()
	items, _, err := e.Audit.List(context.Background(), audit.Query{Action: action, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestSetupFlow(t *testing.T) {
	e := newEnv(t)
	c := e.NewClient()

	st := c.Do("GET", "/api/v1/setup/status", nil).Expect(200).Map()
	if st["setup_required"] != true || st["server_name"] != "Ludworks" || st["registration_enabled"] != false ||
		st["tls_fingerprint_sha256"] != nil || st["version"] == "" {
		t.Fatalf("status: %v", st)
	}
	c.Do("GET", "/api/v1/me", nil).ExpectError(503, "setup_required")
	c.Do("POST", "/api/v1/auth/login", map[string]any{"username": "x", "password": "y"}).ExpectError(503, "setup_required")

	tokPath := e.Deps.Config.SetupTokenPath()
	if fi, err := os.Stat(tokPath); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("setup token file: %v %v", fi, err)
	}
	good := e.SetupToken()
	body := func(tok string) map[string]any {
		return map[string]any{"setup_token": tok, "username": "root", "password": "a very good password", "email": "root@example.com", "server_name": "Home NAS"}
	}
	c.Do("POST", "/api/v1/setup", body("wrong")).ExpectError(403, "forbidden")
	last := "A"
	if strings.HasSuffix(good, "A") {
		last = "B"
	}
	c.Do("POST", "/api/v1/setup", body(good[:len(good)-1]+last)).ExpectError(403, "forbidden") // one char off
	bad := body(good)
	bad["password"] = "short"
	c.Do("POST", "/api/v1/setup", bad).ExpectError(422, "validation_failed")
	bad = body(good)
	bad["username"] = "no spaces allowed"
	c.Do("POST", "/api/v1/setup", bad).ExpectError(422, "validation_failed")

	// The token survived the rejected attempts.
	r := c.Do("POST", "/api/v1/setup", body(good)).Expect(201)
	var out struct {
		User      auth.User `json:"user"`
		CSRFToken string    `json:"csrf_token"`
	}
	r.JSON(&out)
	if out.User.Role != core.RoleAdmin || out.User.Username != "root" || out.CSRFToken == "" {
		t.Fatalf("setup response: %s", r.Body)
	}
	var ck *http.Cookie
	for _, x := range r.Cookies {
		if x.Name == auth.CookieName {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || !strings.HasPrefix(ck.Value, "lws_") {
		t.Fatalf("cookie: %+v", ck)
	}
	if _, err := os.Stat(tokPath); !os.IsNotExist(err) {
		t.Fatalf("setup token file not deleted: %v", err)
	}
	c.CSRF = out.CSRFToken
	me := c.Do("GET", "/api/v1/me", nil).Expect(200).Map()
	if me["username"] != "root" || me["role"] != "admin" {
		t.Fatal(me)
	}
	st = c.Do("GET", "/api/v1/setup/status", nil).Expect(200).Map()
	if st["setup_required"] != false || st["server_name"] != "Home NAS" {
		t.Fatalf("status after setup: %v", st)
	}
	// Token reuse / second setup.
	e.NewClient().Do("POST", "/api/v1/setup", body(good)).ExpectError(409, "conflict")
	if len(auditActions(t, e, "auth.setup")) != 1 || len(auditActions(t, e, "auth.setup_failed")) != 2 {
		t.Fatal("setup audit entries missing")
	}
	if _, err := auth.CreateSetupTokenCLI(context.Background(), e.Deps); err == nil {
		t.Fatal("CLI created a setup token after setup")
	}
}

func TestSetupTokenReuseAndExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.SetupToken()
	// A restart reuses the unexpired token from the file.
	m2 := auth.New(e.Deps)
	if err := m2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.SetupToken(); got != first {
		t.Fatal("token not reused across restart")
	}
	// After expiry a restart issues a new one; the old one stops working.
	if _, err := e.Deps.DB.SQL.Exec(`UPDATE setup_token SET expires_at = '2000-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	m3 := auth.New(e.Deps)
	if err := m3.Start(ctx); err != nil {
		t.Fatal(err)
	}
	second := e.SetupToken()
	if second == first {
		t.Fatal("expired token reused")
	}
	c := e.NewClient()
	req := map[string]any{"setup_token": first, "username": "root", "password": "a very good password", "email": "r@example.com"}
	c.Do("POST", "/api/v1/setup", req).ExpectError(403, "forbidden")
	// The CLI replaces the token (e.g. file lost).
	cli, err := auth.CreateSetupTokenCLI(ctx, e.Deps)
	if err != nil || cli == second || e.SetupToken() != cli {
		t.Fatalf("cli token: %v", err)
	}
	req["setup_token"] = second
	c.Do("POST", "/api/v1/setup", req).ExpectError(403, "forbidden")
	req["setup_token"] = cli
	c.Do("POST", "/api/v1/setup", req).Expect(201)
}

func TestSetupRateLimited(t *testing.T) {
	e := newEnv(t)
	c := e.NewClient()
	var limited bool
	for i := 0; i < 30 && !limited; i++ {
		r := c.Do("POST", "/api/v1/setup", map[string]any{"setup_token": "nope", "username": "a", "password": "b", "email": "c"})
		if r.Status == 429 {
			limited = r.Header.Get("Retry-After") != ""
		}
	}
	if !limited {
		t.Fatal("setup attempts never rate limited")
	}
}

func TestCookieLoginAndCSRF(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)

	c := e.NewClient()
	r := c.Login("BOB", userPW).Expect(200) // case-insensitive username
	m := r.Map()
	if m["expires_at"] == nil || m["csrf_token"] == "" || m["token"] != nil {
		t.Fatalf("login response: %s", r.Body)
	}
	if !strings.HasPrefix(c.Cookie(), "lws_") {
		t.Fatal("no session cookie")
	}
	c.Do("GET", "/api/v1/me", nil).Expect(200)
	sess := c.Do("GET", "/api/v1/auth/session", nil).Expect(200).Map()
	if sess["csrf_token"] != c.CSRF || len(sess["scopes"].([]any)) != 1 || sess["scopes"].([]any)[0] != "*" {
		t.Fatalf("session: %v", sess)
	}
	// Email login works too.
	e.NewClient().Login("Bob@Example.com", userPW).Expect(200)

	tok := map[string]any{"name": "t", "scopes": []string{"files:read"}}
	csrf := c.CSRF
	c.CSRF = ""
	c.Do("POST", "/api/v1/me/tokens", tok).ExpectError(403, "forbidden")
	c.CSRF = "wrong-" + csrf
	c.Do("POST", "/api/v1/me/tokens", tok).ExpectError(403, "forbidden")
	c.CSRF = csrf
	c.Header.Set("Origin", "https://evil.example")
	c.Do("POST", "/api/v1/me/tokens", tok).ExpectError(403, "forbidden")
	c.Header.Set("Origin", e.URL)
	c.Do("POST", "/api/v1/me/tokens", tok).Expect(201)
	c.Header.Del("Origin")
	c.Header.Set("Sec-Fetch-Site", "cross-site")
	c.Do("PATCH", "/api/v1/me", map[string]any{"display_name": "B"}).ExpectError(403, "forbidden")
	c.Header.Del("Sec-Fetch-Site")
	c.Do("PATCH", "/api/v1/me", map[string]any{"display_name": "B"}).Expect(200)

	// Login CSRF: a cross-site page cannot log a browser into an account.
	x := e.NewClient()
	x.Header.Set("Origin", "https://evil.example")
	x.Login("bob", userPW).ExpectError(403, "forbidden")
	x.Header.Set("Origin", "null")
	x.Login("bob", userPW).ExpectError(403, "forbidden")
	x.Header.Del("Origin")
	x.Header.Set("Sec-Fetch-Site", "cross-site")
	x.Login("bob", userPW).ExpectError(403, "forbidden")
	// token_mode is allowed from anywhere (no cookie; e.g. browser extensions).
	r = x.Do("POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": userPW, "token_mode": true}).Expect(200)
	if len(r.Cookies) != 0 {
		t.Fatal("token_mode login set a cookie")
	}

	// Logout revokes the cookie session.
	old := c.Cookie()
	c.Do("POST", "/api/v1/auth/logout", nil).Expect(204)
	c.Do("GET", "/api/v1/me", nil).ExpectError(401, "unauthorized")
	replay := e.NewClient()
	replay.Header.Set("Cookie", auth.CookieName+"="+old)
	replay.Do("GET", "/api/v1/me", nil).ExpectError(401, "unauthorized")
	if len(auditActions(t, e, "auth.logout")) != 1 || len(auditActions(t, e, "auth.login")) < 3 {
		t.Fatal("login/logout not audited")
	}
}

func TestLoginRotatesSession(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	before := c.Cookie()
	c.Login(authtest.AdminUser, authtest.AdminPassword).Expect(200)
	after := c.Cookie()
	if before == after || after == "" {
		t.Fatal("session not rotated")
	}
	old := e.NewClient()
	old.Header.Set("Cookie", auth.CookieName+"="+before)
	old.Do("GET", "/api/v1/me", nil).Expect(401)
}

func TestTokenModeBearer(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	me := b.Do("GET", "/api/v1/me", nil).Expect(200).Map()
	if me["username"] != "bob" {
		t.Fatal(me)
	}
	// Bearer requests need no CSRF token.
	b.Do("PATCH", "/api/v1/me", map[string]any{"display_name": "Bobby"}).Expect(200)
	// A bearer session token is not accepted as a cookie, and a cookie session not as bearer.
	asCookie := e.NewClient()
	asCookie.Header.Set("Cookie", auth.CookieName+"="+b.Bearer)
	asCookie.Do("GET", "/api/v1/me", nil).Expect(401)
	ck := e.NewClient()
	ck.Login("bob", userPW).Expect(200)
	e.WithBearer(ck.Cookie()).Do("GET", "/api/v1/me", nil).Expect(401)
	// Authorization header wins over the cookie (no fallback).
	ck.Bearer = "lws_" + strings.Repeat("A", 43)
	ck.Do("GET", "/api/v1/me", nil).Expect(401)
	// Query tokens only on routes that allow them.
	e.NewClient().Do("GET", "/api/v1/me?access_token="+b.Bearer, nil).Expect(401)
	e.NewClient().Do("GET", "/api/v1/probe/stream?access_token="+b.Bearer, nil).Expect(200)
	// Malformed Authorization headers are anonymous.
	bad := e.NewClient()
	bad.Header.Set("Authorization", "Basic Ym9iOnB3")
	bad.Do("GET", "/api/v1/me", nil).Expect(401)

	sess := b.Do("GET", "/api/v1/auth/session", nil).Expect(200).Map()
	if sess["csrf_token"] != "" || sess["expires_at"] == nil {
		t.Fatalf("bearer session: %v", sess)
	}
	b.Do("POST", "/api/v1/auth/logout", nil).Expect(204)
	b.Do("GET", "/api/v1/me", nil).Expect(401)
}

func TestPersonalAccessTokens(t *testing.T) {
	e := newEnv(t)
	admin := e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	bob := e.NewClient()
	bob.Login("bob", userPW).Expect(200)

	r := bob.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "backup", "scopes": []string{"files:read", "account"}, "expires_in_days": 30}).Expect(201)
	var created struct {
		auth.TokenInfo
		Token string `json:"token"`
	}
	r.JSON(&created)
	if !strings.HasPrefix(created.Token, "lwp_") || created.Prefix != created.Token[:8] || created.ExpiresAt == nil {
		t.Fatalf("created: %s", r.Body)
	}
	bob.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "x", "scopes": []string{"admin"}}).ExpectError(403, "forbidden")
	bob.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "x", "scopes": []string{"nope"}}).ExpectError(422, "validation_failed")
	bob.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "", "scopes": []string{"files:read"}}).ExpectError(422, "validation_failed")
	bob.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "x", "scopes": []string{"files:read"}, "expires_in_days": 0}).ExpectError(422, "validation_failed")

	pat := e.WithBearer(created.Token)
	pat.Do("GET", "/api/v1/me", nil).Expect(200)          // [auth]
	pat.Do("GET", "/api/v1/probe/files", nil).Expect(200) // files:read
	pat.Do("GET", "/api/v1/probe/stream?x=1", nil).Expect(200)
	pat.Do("GET", "/api/v1/probe/admin", nil).ExpectError(403, "forbidden")
	// The PAT holds "account", so it may mint tokens — but only with scopes it holds.
	pat.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "y", "scopes": []string{"files:write"}}).ExpectError(403, "forbidden")
	r = pat.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "y", "scopes": []string{"files:read"}}).Expect(201)
	var narrow struct {
		Token string `json:"token"`
	}
	r.JSON(&narrow)
	np := e.WithBearer(narrow.Token)
	np.Do("PATCH", "/api/v1/me", map[string]any{"display_name": "x"}).ExpectError(403, "forbidden") // needs account
	np.Do("GET", "/api/v1/me/tokens", nil).ExpectError(403, "forbidden")
	// PATs are never accepted from a cookie.
	ck := e.NewClient()
	ck.Header.Set("Cookie", auth.CookieName+"="+created.Token)
	ck.Do("GET", "/api/v1/me", nil).Expect(401)
	// Query token on an allowed route.
	e.NewClient().Do("GET", "/api/v1/probe/stream?access_token="+narrow.Token, nil).Expect(200)

	// Listing never reveals secrets; last_used_at is tracked.
	list := bob.Do("GET", "/api/v1/me/tokens", nil).Expect(200)
	if strings.Contains(string(list.Body), created.Token) || strings.Contains(string(list.Body), "token_hash") {
		t.Fatal("token secret leaked in list")
	}
	var toks []auth.TokenInfo
	list.JSON(&toks)
	if len(toks) != 2 {
		t.Fatalf("tokens: %s", list.Body)
	}
	for _, tk := range toks {
		if tk.ID == created.ID && tk.LastUsedAt == nil {
			t.Fatal("last_used_at not recorded")
		}
	}
	// Admin PAT with the admin scope reaches admin routes; without it does not.
	r = admin.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "ops", "scopes": []string{"admin"}}).Expect(201)
	var adm struct {
		Token string `json:"token"`
	}
	r.JSON(&adm)
	e.WithBearer(adm.Token).Do("GET", "/api/v1/probe/admin", nil).Expect(200)
	r = admin.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "ro", "scopes": []string{"files:read"}}).Expect(201)
	r.JSON(&adm)
	e.WithBearer(adm.Token).Do("GET", "/api/v1/probe/admin", nil).ExpectError(403, "forbidden")

	// Expiry and revocation.
	if _, err := e.Deps.DB.SQL.Exec(`UPDATE personal_access_tokens SET expires_at = '2001-01-01T00:00:00.000Z' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	pat.Do("GET", "/api/v1/me", nil).Expect(401)
	bob.Do("DELETE", "/api/v1/me/tokens/"+created.ID, nil).Expect(204)
	bob.Do("DELETE", "/api/v1/me/tokens/"+created.ID, nil).ExpectError(404, "not_found")
	admin.Do("DELETE", "/api/v1/me/tokens/"+toks[0].ID, nil).ExpectError(404, "not_found") // someone else's
	if len(auditActions(t, e, "token.create")) != 4 || len(auditActions(t, e, "token.revoke")) != 1 {
		t.Fatal("token audit entries")
	}
	// VerifyToken (path tokens) resolves PATs only.
	p, err := auth.VerifyToken(context.Background(), e.Deps, narrow.Token)
	if err != nil || p == nil || !p.Has("files:read") || p.Has("files:write") {
		t.Fatalf("VerifyToken: %+v %v", p, err)
	}
	if p, _ := auth.VerifyToken(context.Background(), e.Deps, bob.Cookie()); p != nil {
		t.Fatal("VerifyToken accepted a session token")
	}
}

func TestTOTPFlow(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC).Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	e.Auth.SetClock(now)

	bob := e.NewClient()
	bob.Login("bob", userPW).Expect(200)
	bob.Do("POST", "/api/v1/me/totp/enable", map[string]any{"code": "123456"}).ExpectError(409, "conflict") // no setup yet
	setup := bob.Do("POST", "/api/v1/me/totp/setup", map[string]any{}).Expect(200).Map()
	secret := setup["secret"].(string)
	if !strings.HasPrefix(setup["otpauth_url"].(string), "otpauth://totp/Ludworks:bob?") {
		t.Fatal(setup)
	}
	code := func(offset int64) string {
		c, err := auth.GenerateTOTP(secret, time.Unix(clock.Load()+offset, 0))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	wrong := code(0)
	wrong = string(rune('0'+(wrong[0]-'0'+1)%10)) + wrong[1:]
	bob.Do("POST", "/api/v1/me/totp/enable", map[string]any{"code": wrong}).ExpectError(401, "invalid_credentials")
	r := bob.Do("POST", "/api/v1/me/totp/enable", map[string]any{"code": code(0)}).Expect(200)
	var rc struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	r.JSON(&rc)
	if len(rc.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes: %v", rc.RecoveryCodes)
	}
	if me := bob.Do("GET", "/api/v1/me", nil).Expect(200).Map(); me["totp_enabled"] != true {
		t.Fatal(me)
	}
	bob.Do("POST", "/api/v1/me/totp/setup", map[string]any{}).ExpectError(409, "conflict")
	// Secrets are stored encrypted.
	var enc string
	e.Deps.DB.SQL.QueryRow(`SELECT totp_secret_enc FROM users WHERE username = 'bob'`).Scan(&enc)
	if !strings.HasPrefix(enc, "v1.") || strings.Contains(enc, secret) {
		t.Fatalf("totp secret at rest: %q", enc)
	}

	login := func(extra map[string]any) *authtest.Response {
		body := map[string]any{"username": "bob", "password": userPW, "token_mode": true}
		for k, v := range extra {
			body[k] = v
		}
		return e.NewClient().Do("POST", "/api/v1/auth/login", body)
	}
	login(nil).ExpectError(401, "totp_required")
	login(map[string]any{"password": "wrong password!!"}).ExpectError(401, "invalid_credentials")
	login(map[string]any{"totp_code": code(0)}).ExpectError(401, "invalid_credentials")  // replay of the enable code
	login(map[string]any{"totp_code": code(30)}).Expect(200)                             // next step (clock skew)
	login(map[string]any{"totp_code": code(30)}).ExpectError(401, "invalid_credentials") // replay
	clock.Add(90)
	login(map[string]any{"totp_code": code(0)}).Expect(200)
	// Recovery codes are single use and normalised.
	rcode := strings.ToUpper(strings.ReplaceAll(rc.RecoveryCodes[3], "-", " "))
	login(map[string]any{"recovery_code": rcode}).Expect(200)
	login(map[string]any{"recovery_code": rc.RecoveryCodes[3]}).ExpectError(401, "invalid_credentials")
	login(map[string]any{"recovery_code": "0000-0000-0000-0000"}).ExpectError(401, "invalid_credentials")

	clock.Add(60)
	bob.Do("POST", "/api/v1/me/totp/disable", map[string]any{"password": "nope nope nope", "code": code(0)}).ExpectError(401, "invalid_credentials")
	bob.Do("POST", "/api/v1/me/totp/disable", map[string]any{"password": userPW, "code": wrong}).ExpectError(401, "invalid_credentials")
	bob.Do("POST", "/api/v1/me/totp/disable", map[string]any{"password": userPW, "code": code(0)}).Expect(204)
	bob.Do("POST", "/api/v1/me/totp/disable", map[string]any{"password": userPW, "code": code(0)}).ExpectError(409, "conflict")
	login(nil).Expect(200)
	var n int
	e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM recovery_codes`).Scan(&n)
	if n != 0 {
		t.Fatal("recovery codes not deleted")
	}
	for _, a := range []string{"auth.totp_enabled", "auth.totp_disabled"} {
		if len(auditActions(t, e, a)) != 1 {
			t.Fatalf("%s not audited", a)
		}
	}
}

func TestRequireTOTPForAdmins(t *testing.T) {
	e := newEnv(t)
	admin := e.Setup()
	admin.Do("GET", "/api/v1/probe/admin", nil).Expect(200)
	e.Patch("security", map[string]any{"require_totp_for_admins": true})
	// Admin rights are withheld until 2FA is enrolled; the account itself keeps working.
	admin.Do("GET", "/api/v1/probe/admin", nil).ExpectError(403, "forbidden")
	setup := admin.Do("POST", "/api/v1/me/totp/setup", map[string]any{}).Expect(200).Map()
	code, _ := auth.GenerateTOTP(setup["secret"].(string), time.Now())
	admin.Do("POST", "/api/v1/me/totp/enable", map[string]any{"code": code}).Expect(200)
	admin.Do("GET", "/api/v1/probe/admin", nil).Expect(200)
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	c := e.NewClient()
	for i := 0; i < 6; i++ {
		c.Login("bob", "wrong password "+string(rune('a'+i))).ExpectError(401, "invalid_credentials")
	}
	r := c.Login("bob", userPW).ExpectError(429, "rate_limited")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Unknown accounts are locked the same way (no enumeration through lockout behaviour).
	for i := 0; i < 6; i++ {
		c.Login("ghost", "whatever password").ExpectError(401, "invalid_credentials")
	}
	c.Login("ghost", "whatever password").ExpectError(429, "rate_limited")
	// Other accounts are unaffected.
	createUser(t, e, "carol", core.RoleUser)
	c.Login("carol", userPW).Expect(200)
	fails := auditActions(t, e, "auth.login_failed")
	if len(fails) < 12 {
		t.Fatalf("failed logins audited: %d", len(fails))
	}
	var anon bool
	for _, f := range fails {
		if f.ActorUsername == "ghost" && f.ActorID == "" {
			anon = true
		}
	}
	if !anon {
		t.Fatal("unknown-user failure should carry the attempted name and no actor id")
	}

	// With lockout disabled repeated failures are not locked out.
	e.Patch("security", map[string]any{"login_lockout_enabled": false})
	createUser(t, e, "dave", core.RoleUser)
	for i := 0; i < 7; i++ {
		c.Login("dave", "wrong password!!").ExpectError(401, "invalid_credentials")
	}
	c.Login("dave", userPW).Expect(200)
}

func TestDisabledAccountCannotLogin(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	u := createUser(t, e, "bob", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	if _, _, err := e.Auth.UpdateUser(context.Background(), "someone", u.ID, auth.UserPatch{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	b.Do("GET", "/api/v1/me", nil).Expect(401) // sessions revoked on disable
	e.NewClient().Login("bob", "wrong password!!").ExpectError(401, "invalid_credentials")
	e.NewClient().Login("bob", userPW).ExpectError(403, "forbidden")
}

func ptr[T any](v T) *T { return &v }

func TestRegistration(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	reg := func(c *authtest.Client, name, pw string) *authtest.Response {
		return c.Do("POST", "/api/v1/auth/register", map[string]any{"username": name, "email": name + "@example.com", "password": pw})
	}
	reg(e.NewClient(), "eve", userPW).ExpectError(403, "registration_disabled")
	e.Patch("general", map[string]any{"registration_enabled": true, "default_role": "admin", "default_quota_bytes": 1000})
	if st := e.NewClient().Do("GET", "/api/v1/setup/status", nil).Map(); st["registration_enabled"] != true {
		t.Fatal(st)
	}
	c := e.NewClient()
	r := reg(c, "eve", userPW).Expect(201).Map()
	u := r["user"].(map[string]any)
	if r["verification_required"] != false || u["role"] != "user" || u["quota_bytes"] != float64(1000) {
		t.Fatalf("register: %v", r)
	}
	c.Do("GET", "/api/v1/me", nil).Expect(200) // logged in by cookie
	reg(e.NewClient(), "EVE", userPW).ExpectError(409, "conflict")
	reg(e.NewClient(), "frank", "short").ExpectError(422, "validation_failed")
	reg(e.NewClient(), "frank-the-great", "frank-the-great").ExpectError(422, "validation_failed")
	x := e.NewClient()
	x.Header.Set("Origin", "https://evil.example")
	reg(x, "mallory", userPW).ExpectError(403, "forbidden")

	// With email verification required.
	e.Patch("general", map[string]any{"require_email_verification": true})
	c = e.NewClient()
	r = reg(c, "gina", userPW).Expect(201).Map()
	if r["verification_required"] != true || c.Cookie() != "" {
		t.Fatalf("verification register: %v", r)
	}
	msgs := e.Mailer.Wait(t, 1)
	if msgs[0].To[0] != "gina@example.com" || !strings.Contains(msgs[0].Subject, "Confirm") {
		t.Fatalf("mail: %+v", msgs[0])
	}
	tok, origin := authtest.LinkToken(t, msgs[0], "/verify-email")
	if origin != e.URL {
		t.Fatalf("link origin %s, want %s", origin, e.URL)
	}
	c.Login("gina", userPW).ExpectError(403, "forbidden") // unverified
	c.Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": "x"}).ExpectError(410, "gone")
	c.Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": tok}).Expect(204)
	c.Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": tok}).ExpectError(410, "gone")
	if c.Login("gina", userPW).Expect(200).Map()["user"].(map[string]any)["email_verified"] != true {
		t.Fatal("email not verified")
	}
	// Changing the email takes effect once the new address confirms it (through the same
	// /verify-email page); the old address is notified.
	if m := c.Do("PATCH", "/api/v1/me", map[string]any{"email": "gina2@example.com"}).Expect(200).Map(); m["email"] != "gina@example.com" || m["email_change_pending"] != true {
		t.Fatal(m)
	}
	msgs = e.Mailer.Wait(t, 3)
	var confirm *core.MailMessage
	for i := range msgs[1:] {
		if msgs[1+i].To[0] == "gina2@example.com" {
			confirm = &msgs[1+i]
		}
	}
	if confirm == nil {
		t.Fatalf("no confirmation link: %+v", msgs)
	}
	tok, _ = authtest.LinkToken(t, *confirm, "/verify-email")
	c.Do("POST", "/api/v1/auth/verify-email", map[string]any{"token": tok}).Expect(204)
	if me := c.Do("GET", "/api/v1/me", nil).Map(); me["email"] != "gina2@example.com" || me["email_verified"] != true {
		t.Fatal(me)
	}
	// A taken address answers like a free one (no enumeration); see security_test.go.
	c.Do("PATCH", "/api/v1/me", map[string]any{"email": "eve@EXAMPLE.com"}).Expect(200)
	if len(auditActions(t, e, "auth.register")) != 2 {
		t.Fatal("registration not audited")
	}
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	a := e.NewClient()
	a.Login("bob", userPW).Expect(200)
	b := e.TokenLogin("bob", userPW)
	r := e.NewClient()
	r.Login("bob", userPW).Expect(200)
	pat := a.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "p", "scopes": []string{"files:read"}}).Expect(201).Map()["token"].(string)

	a.Do("POST", "/api/v1/me/password", map[string]any{"current_password": "wrong one", "new_password": "a brand new password"}).
		ExpectError(401, "invalid_credentials")
	a.Do("POST", "/api/v1/me/password", map[string]any{"current_password": userPW, "new_password": "short"}).
		ExpectError(422, "validation_failed")
	a.Do("POST", "/api/v1/me/password", map[string]any{"current_password": userPW, "new_password": "a brand new password"}).Expect(204)
	a.Do("GET", "/api/v1/me", nil).Expect(200)
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	r.Do("GET", "/api/v1/me", nil).Expect(401)
	e.WithBearer(pat).Do("GET", "/api/v1/me", nil).Expect(200) // PATs are separate credentials
	e.NewClient().Login("bob", userPW).ExpectError(401, "invalid_credentials")
	e.NewClient().Login("bob", "a brand new password").Expect(200)

	// Re-authentication failures lock out.
	for i := 0; i < 6; i++ {
		a.Do("POST", "/api/v1/me/password", map[string]any{"current_password": "wrong", "new_password": "whatever whatever"})
	}
	a.Do("POST", "/api/v1/me/password", map[string]any{"current_password": "a brand new password", "new_password": "x"}).
		ExpectError(429, "rate_limited")
}

func TestPasswordResetFlow(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	req := func(email string) {
		e.NewClient().Do("POST", "/api/v1/auth/password-reset/request", map[string]any{"email": email}).Expect(202)
	}

	e.Mailer.SetEnabled(false)
	req("bob@example.com")
	e.Mailer.SetEnabled(true)
	req("nobody@example.com")
	e.NewClient().Do("POST", "/api/v1/auth/password-reset/request", map[string]any{"email": "not-an-email"}).ExpectError(422, "validation_failed")
	req("BOB@example.com")
	msgs := e.Mailer.Wait(t, 1)
	time.Sleep(100 * time.Millisecond) // the other requests' jobs have had time to (not) send
	if n := len(e.Mailer.Messages()); n != 1 {
		t.Fatalf("expected exactly one reset email, got %d", n)
	}
	if msgs[0].To[0] != "bob@example.com" || msgs[0].HTML == "" || !strings.Contains(msgs[0].Text, "one hour") {
		t.Fatalf("reset mail: %+v", msgs[0])
	}
	tok, origin := authtest.LinkToken(t, msgs[0], "/reset-password")
	if origin != e.URL {
		t.Fatalf("origin %s", origin)
	}
	c := e.NewClient()
	confirm := func(tk, pw string) *authtest.Response {
		return c.Do("POST", "/api/v1/auth/password-reset/confirm", map[string]any{"token": tk, "new_password": pw})
	}
	confirm("garbage", "a brand new password").ExpectError(410, "gone")
	confirm(tok, "short").ExpectError(422, "validation_failed") // token not consumed
	confirm(tok, "a brand new password").Expect(204)
	confirm(tok, "another new password").ExpectError(410, "gone")
	b.Do("GET", "/api/v1/me", nil).Expect(401) // sessions revoked
	e.NewClient().Login("bob", "a brand new password").Expect(200)
	if len(auditActions(t, e, "auth.password_reset")) != 1 {
		t.Fatal("reset not audited")
	}

	// Expired tokens are gone.
	req("bob@example.com")
	msgs = e.Mailer.Wait(t, 2)
	tok2, _ := authtest.LinkToken(t, msgs[1], "/reset-password")
	e.Deps.DB.SQL.Exec(`UPDATE email_tokens SET expires_at = '2001-01-01T00:00:00.000Z'`)
	confirm(tok2, "a third new password").ExpectError(410, "gone")

	// A forged Host header never ends up in a reset link (another account: bob hit the per-address limit).
	createUser(t, e, "carol", core.RoleUser)
	evil := e.NewClient()
	evil.HTTP.Transport = hostRewriter{host: "evil.example"}
	evil.Do("POST", "/api/v1/auth/password-reset/request", map[string]any{"email": "carol@example.com"}).Expect(202)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.Mailer.Messages()); n != 2 {
		t.Fatalf("reset email sent for an untrusted Host (%d mails)", n)
	}
	// With public_url configured the link always uses it.
	e.Patch("general", map[string]any{"public_url": "https://nas.example.com/"})
	evil.Do("POST", "/api/v1/auth/password-reset/request", map[string]any{"email": "carol@example.com"}).Expect(202)
	msgs = e.Mailer.Wait(t, 3)
	if _, o := authtest.LinkToken(t, msgs[2], "/reset-password"); o != "https://nas.example.com" {
		t.Fatalf("public_url not used: %s", o)
	}
}

type hostRewriter struct{ host string }

func (h hostRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	return http.DefaultTransport.RoundTrip(r)
}

func TestSessionsListAndRevoke(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	a := e.NewClient()
	a.Login("bob", userPW).Expect(200)
	b := e.NewClient()
	b.Do("POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": userPW, "token_mode": true, "device_name": "Pixel 9"}).Expect(200)
	a.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "cli", "scopes": []string{"files:read"}}).Expect(201)

	var list []map[string]any
	a.Do("GET", "/api/v1/me/sessions", nil).Expect(200).JSON(&list)
	if len(list) != 3 {
		t.Fatalf("sessions: %v", list)
	}
	var current, other, pat map[string]any
	for _, s := range list {
		switch {
		case s["current"] == true:
			current = s
		case s["kind"] == "token":
			pat = s
		default:
			other = s
		}
	}
	if current == nil || other == nil || pat == nil || other["device_name"] != "Pixel 9" || pat["device_name"] != "cli" ||
		current["ip"] != "127.0.0.1" || current["expires_at"] == nil {
		t.Fatalf("sessions: %v", list)
	}
	a.Do("DELETE", "/api/v1/me/sessions/"+other["id"].(string), nil).Expect(204)
	a.Do("DELETE", "/api/v1/me/sessions/"+other["id"].(string), nil).ExpectError(404, "not_found")
	a.Do("DELETE", "/api/v1/me/sessions/"+pat["id"].(string), nil).Expect(204)
	a.Do("DELETE", "/api/v1/me/sessions/not-a-valid-id!", nil).ExpectError(404, "not_found")
	a.Do("GET", "/api/v1/me/sessions", nil).Expect(200).JSON(&list)
	if len(list) != 1 {
		t.Fatal(list)
	}
	// Revoking the current session logs out.
	a.Do("DELETE", "/api/v1/me/sessions/"+current["id"].(string), nil).Expect(204)
	a.Do("GET", "/api/v1/me", nil).Expect(401)
}

func TestSessionTimeouts(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	var off atomic.Int64
	e.Auth.SetClock(func() time.Time { return time.Now().Add(time.Duration(off.Load())) })
	e.Patch("security", map[string]any{"session_idle_minutes": 30, "session_absolute_days": 1})
	b := e.TokenLogin("bob", userPW)
	for i := 0; i < 3; i++ { // activity keeps the session alive past the idle timeout
		off.Add(int64(20 * time.Minute))
		b.Do("GET", "/api/v1/me", nil).Expect(200)
	}
	off.Add(int64(31 * time.Minute))
	b.Do("GET", "/api/v1/me", nil).Expect(401)

	off.Store(0)
	b = e.TokenLogin("bob", userPW)
	for i := 0; i < 50; i++ {
		off.Add(int64(29 * time.Minute))
		if r := b.Do("GET", "/api/v1/me", nil); r.Status != 200 {
			if time.Duration(off.Load()) < 24*time.Hour-30*time.Minute {
				t.Fatalf("expired early after %v", time.Duration(off.Load()))
			}
			return
		}
	}
	t.Fatal("absolute lifetime not enforced")
}

func TestPasswordRehashOnLogin(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	hashOf := func() string {
		var h string
		e.Deps.DB.SQL.QueryRow(`SELECT password_hash FROM users WHERE username = 'bob'`).Scan(&h)
		return h
	}
	if !strings.Contains(hashOf(), "m=64,t=1,p=1") {
		t.Fatal(hashOf())
	}
	e.Auth.SetPasswordParams(auth.PasswordParams{Memory: 256, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})
	e.NewClient().Login("bob", userPW).Expect(200)
	if !strings.Contains(hashOf(), "m=256,t=2,p=1") {
		t.Fatalf("not rehashed: %s", hashOf())
	}
	e.NewClient().Login("bob", userPW).Expect(200)
}

func TestUserLookup(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	u := createUser(t, e, "bob", core.RoleUser)
	ctx := context.Background()
	got, err := e.Deps.Users.GetUser(ctx, u.ID)
	if err != nil || got.Username != "bob" || got.Role != core.RoleUser {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := e.Deps.Users.FindUserByEmail(ctx, "BOB@example.COM"); err != nil || got.ID != u.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := e.Deps.Users.GetUser(ctx, "missing-user-id"); err == nil {
		t.Fatal("missing user found")
	}
	ids, err := e.Deps.Users.ListUserIDs(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
}

func TestResetPasswordCLI(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	b := e.TokenLogin("bob", userPW)
	ctx := context.Background()
	if err := auth.ResetPasswordCLI(ctx, e.Deps, "nobody", "a fine new password"); err == nil {
		t.Fatal("unknown user accepted")
	}
	if err := auth.ResetPasswordCLI(ctx, e.Deps, "bob", "short"); err == nil {
		t.Fatal("weak password accepted")
	}
	if err := auth.ResetPasswordCLI(ctx, e.Deps, "Bob", "a fine new password"); err != nil {
		t.Fatal(err)
	}
	b.Do("GET", "/api/v1/me", nil).Expect(401)
	e.NewClient().Login("bob", "a fine new password").Expect(200)
}

func TestCleanupJobRemovesExpired(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	createUser(t, e, "bob", core.RoleUser)
	e.TokenLogin("bob", userPW)
	e.Deps.DB.SQL.Exec(`UPDATE sessions SET expires_at = '2001-01-01T00:00:00.000Z' WHERE kind = 'bearer'`)
	if err := auth.RunCleanup(e.Auth, context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM sessions WHERE kind = 'bearer'`).Scan(&n)
	if n != 0 {
		t.Fatal("expired session not purged")
	}
}
