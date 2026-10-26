// Package authtest builds an in-process server (real database, settings, audit log, auth
// module and router) for end-to-end tests of auth and the modules built on it.
package authtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/config"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
	"ludworks/internal/jobs"
	"ludworks/internal/settings"
)

// FastParams makes password hashing cheap in tests.
var FastParams = auth.PasswordParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// Default admin credentials used by Setup.
const (
	AdminUser     = "admin"
	AdminEmail    = "admin@example.com"
	AdminPassword = "correct horse battery staple"
)

// Env is a running test server.
type Env struct {
	T      testing.TB
	Deps   *core.Deps
	Auth   *auth.Module
	Audit  *audit.Log
	Mailer *FakeMailer
	Router *httpx.Router
	Server *httptest.Server
	URL    string
}

// Options configure New.
type Options struct {
	// Modules returns extra modules to mount (constructed after auth, migrated and started).
	Modules func(e *Env) []httpx.Module
	// RateLimit enables the router's generic API limiter (off by default to keep tests
	// deterministic).
	RateLimit bool
	// Log receives server logs (default: discarded).
	Log *slog.Logger
}

// New starts a fresh server with an empty database.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	ctx := context.Background()
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg := &config.Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", TLS: config.TLSConfig{Mode: config.TLSOff}}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	st, err := settings.New(ctx, d, key)
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.New(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	fm := &FakeMailer{}
	deps := &core.Deps{
		DB: d, Config: cfg, Settings: st, Log: log, Jobs: jobs.New(log, 4), Events: events.New(log),
		Mailer: fm, Audit: al, Info: &core.ServerInfo{StartedAt: core.Now(), TLSMode: config.TLSOff},
	}
	e := &Env{T: t, Deps: deps, Audit: al, Mailer: fm}
	e.Auth = auth.New(deps)
	e.Auth.SetPasswordParams(FastParams)
	deps.Users = e.Auth.Users()
	mods := []httpx.Module{e.Auth}
	if o.Modules != nil {
		mods = append(mods, o.Modules(e)...)
	}
	for _, m := range mods {
		if err := d.Migrate(ctx, m.Name(), m.Migrations()); err != nil {
			t.Fatal(err)
		}
	}
	e.Router = httpx.NewRouter(httpx.Options{
		Log: log, Auth: e.Auth.Authenticator(), SetupRequired: e.Auth.SetupRequired, Settings: st,
		RateLimit: httpx.RateLimitOptions{Disabled: !o.RateLimit},
	})
	for _, m := range mods {
		m.Routes(e.Router)
	}
	deps.Jobs.Start()
	for _, m := range mods {
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.Server = httptest.NewServer(e.Router)
	e.URL = e.Server.URL
	t.Cleanup(func() {
		e.Server.Close()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = deps.Jobs.Stop(c)
		_ = deps.Events.Close(c)
		_ = d.Close()
	})
	return e
}

// SetupToken reads the token the server wrote on first start.
func (e *Env) SetupToken() string {
	e.T.Helper()
	b, err := os.ReadFile(e.Deps.Config.SetupTokenPath())
	if err != nil {
		e.T.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Setup claims the server as AdminUser and returns the logged-in cookie client.
func (e *Env) Setup() *Client {
	e.T.Helper()
	c := e.NewClient()
	r := c.Do("POST", "/api/v1/setup", map[string]any{
		"setup_token": e.SetupToken(), "username": AdminUser, "password": AdminPassword, "email": AdminEmail,
	})
	r.Expect(http.StatusCreated)
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	r.JSON(&out)
	c.CSRF = out.CSRFToken
	return c
}

// Patch updates settings directly (bypassing HTTP).
func (e *Env) Patch(ns string, v map[string]any) {
	e.T.Helper()
	b, _ := json.Marshal(v)
	if err := e.Deps.Settings.Update(context.Background(), ns, b); err != nil {
		e.T.Fatal(err)
	}
}

// Client is an HTTP client with a cookie jar and optional CSRF/Bearer credentials.
type Client struct {
	e      *Env
	HTTP   *http.Client
	CSRF   string
	Bearer string
	Header http.Header // extra headers on every request
}

// NewClient returns a client with an empty cookie jar.
func (e *Env) NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{e: e, HTTP: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Header: http.Header{}}
}

// Login performs a cookie-mode login and stores the CSRF token.
func (c *Client) Login(username, password string) *Response {
	c.e.T.Helper()
	r := c.Do("POST", "/api/v1/auth/login", map[string]any{"username": username, "password": password})
	if r.Status == http.StatusOK {
		var out struct {
			CSRFToken string `json:"csrf_token"`
		}
		r.JSON(&out)
		c.CSRF = out.CSRFToken
	}
	return r
}

// TokenLogin performs a token_mode login and returns a client using the bearer token.
func (e *Env) TokenLogin(username, password string) *Client {
	e.T.Helper()
	c := e.NewClient()
	r := c.Do("POST", "/api/v1/auth/login", map[string]any{"username": username, "password": password, "token_mode": true})
	r.Expect(http.StatusOK)
	var out struct {
		Token string `json:"token"`
	}
	r.JSON(&out)
	if !strings.HasPrefix(out.Token, "lws_") {
		e.T.Fatalf("token_mode login returned %q", out.Token)
	}
	c.Bearer = out.Token
	return c
}

// WithBearer returns a cookie-less client using token.
func (e *Env) WithBearer(token string) *Client {
	c := e.NewClient()
	c.Bearer = token
	return c
}

// Cookie returns the current lw_session cookie value ("" if none).
func (c *Client) Cookie() string {
	u, _ := url.Parse(c.e.URL)
	for _, ck := range c.HTTP.Jar.Cookies(u) {
		if ck.Name == auth.CookieName {
			return ck.Value
		}
	}
	return ""
}

// Do sends a JSON request (body nil = no body). The CSRF header is added on unsafe methods
// when the client has one; set c.CSRF = "" to omit it.
func (c *Client) Do(method, path string, body any) *Response {
	c.e.T.Helper()
	var rd io.Reader
	if body != nil {
		if raw, ok := body.([]byte); ok {
			rd = bytes.NewReader(raw)
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				c.e.T.Fatal(err)
			}
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, c.e.URL+path, rd)
	if err != nil {
		c.e.T.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.CSRF != "" && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", c.CSRF)
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	for k, v := range c.Header {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.e.T.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &Response{t: c.e.T, Status: resp.StatusCode, Header: resp.Header, Body: b, Cookies: resp.Cookies(), Method: method, Path: path}
}

// Response is a buffered HTTP response.
type Response struct {
	t       testing.TB
	Status  int
	Header  http.Header
	Body    []byte
	Cookies []*http.Cookie
	Method  string
	Path    string
}

// Expect fails the test unless the status matches; it returns r for chaining.
func (r *Response) Expect(status int) *Response {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("%s %s: status %d, want %d; body: %s", r.Method, r.Path, r.Status, status, r.Body)
	}
	return r
}

// ExpectError checks status and error code.
func (r *Response) ExpectError(status int, code string) *Response {
	r.t.Helper()
	r.Expect(status)
	if got := r.ErrCode(); got != code {
		r.t.Fatalf("%s %s: error code %q, want %q; body: %s", r.Method, r.Path, got, code, r.Body)
	}
	return r
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("%s %s: decode %s: %v", r.Method, r.Path, r.Body, err)
	}
}

// Map decodes the body as an object.
func (r *Response) Map() map[string]any {
	r.t.Helper()
	var m map[string]any
	r.JSON(&m)
	return m
}

// ErrCode returns error.code from an error body.
func (r *Response) ErrCode() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

// FakeMailer records messages instead of sending them.
type FakeMailer struct {
	mu       sync.Mutex
	disabled bool
	msgs     []core.MailMessage
}

// SetEnabled toggles Enabled (default true).
func (f *FakeMailer) SetEnabled(on bool) { f.mu.Lock(); f.disabled = !on; f.mu.Unlock() }

func (f *FakeMailer) Enabled(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.disabled
}

func (f *FakeMailer) Send(_ context.Context, m core.MailMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return nil
}

// Messages returns a copy of the recorded messages.
func (f *FakeMailer) Messages() []core.MailMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.MailMessage(nil), f.msgs...)
}

// Wait polls until at least n messages were sent (mail is sent from background jobs).
func (f *FakeMailer) Wait(t testing.TB, n int) []core.MailMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := f.Messages(); len(m) >= n {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d emails (have %d)", n, len(f.Messages()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LinkToken extracts the token query value and the origin of the first <path>?token= link
// in a message's text body.
func LinkToken(t testing.TB, m core.MailMessage, path string) (token, origin string) {
	t.Helper()
	i := strings.Index(m.Text, path+"?token=")
	start := strings.LastIndex(m.Text[:max(i, 0)], "http")
	if i < 0 || start < 0 {
		t.Fatalf("no %s link in %q", path, m.Text)
	}
	end := len(m.Text)
	if j := strings.IndexAny(m.Text[i:], " \r\n"); j >= 0 {
		end = i + j
	}
	u, err := url.Parse(m.Text[start:end])
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token"), u.Scheme + "://" + u.Host
}
