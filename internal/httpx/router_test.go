package httpx

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/settings"
)

// fakeAuth: "Bearer user" / "Bearer admin" / "Bearer files-ro" tokens; cookie lw_session=<name>
// with CSRF token "csrf-<name>".
type fakeAuth struct{}

var principals = map[string]*core.Principal{
	"user":     {UserID: "u1", Username: "user", Role: core.RoleUser, Scopes: []string{"*"}, Kind: core.KindSession},
	"admin":    {UserID: "a1", Username: "admin", Role: core.RoleAdmin, Scopes: []string{"*"}, Kind: core.KindSession},
	"files-ro": {UserID: "u1", Username: "user", Role: core.RoleUser, Scopes: []string{"files:read"}, Kind: core.KindToken},
	"user-adm": {UserID: "u1", Username: "user", Role: core.RoleUser, Scopes: []string{"admin", "*"}, Kind: core.KindToken},
	"adm-tok":  {UserID: "a1", Username: "admin", Role: core.RoleAdmin, Scopes: []string{"files:read"}, Kind: core.KindToken},
}

func (fakeAuth) Authenticate(r *http.Request, allowQuery bool) (AuthResult, error) {
	if a := r.Header.Get("Authorization"); a != "" {
		if strings.HasPrefix(a, "Bearer ") {
			if p := principals[strings.TrimPrefix(a, "Bearer ")]; p != nil {
				return AuthResult{Principal: p}, nil
			}
		}
		return AuthResult{}, nil
	}
	if allowQuery {
		if p := principals[r.URL.Query().Get("access_token")]; p != nil {
			return AuthResult{Principal: p}, nil
		}
	}
	if c, err := r.Cookie("lw_session"); err == nil {
		if c.Value == "disabled" {
			return AuthResult{}, core.Forbidden("account disabled")
		}
		if p := principals[c.Value]; p != nil {
			return AuthResult{Principal: p, ViaCookie: true, CSRFToken: "csrf-" + c.Value}, nil
		}
	}
	return AuthResult{}, nil
}

func newTestRouter(t *testing.T, setupRequired *bool, st *settings.Store) *Router {
	t.Helper()
	rt := NewRouter(Options{
		Auth:           fakeAuth{},
		SetupRequired:  func(context.Context) bool { return setupRequired != nil && *setupRequired },
		Settings:       st,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RateLimit:      RateLimitOptions{Disabled: true},
	})
	ok := func(w http.ResponseWriter, r *http.Request) error {
		p, _ := core.PrincipalFrom(r.Context())
		name := ""
		if p != nil {
			name = p.Username
		}
		return WriteJSON(w, 200, map[string]string{"who": name})
	}
	rt.Handle("GET /api/v1/health", Public(), ok)
	rt.Handle("GET /api/v1/setup/status", Public(), ok)
	rt.Handle("POST /api/v1/setup", Public().WithoutCSRF(), ok)
	rt.Handle("POST /api/v1/public-write", Public(), ok)
	rt.Handle("GET /api/v1/me", Authenticated(), ok)
	rt.Handle("POST /api/v1/me", Authenticated(), ok)
	rt.Handle("GET /api/v1/files", Scope("files:read"), ok)
	rt.Handle("POST /api/v1/files", Scope("files:write"), ok)
	rt.Handle("GET /api/v1/admin/users", AdminOnly(), ok)
	rt.Handle("GET /api/v1/stream", Scope("files:read").WithQueryToken(), ok)
	rt.Handle("POST /api/v1/echo", Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := DecodeJSON(r, &body); err != nil {
			return err
		}
		return WriteJSON(w, 200, body)
	})
	rt.Handle("PUT /api/v1/big", Authenticated().WithMaxBody(10), func(w http.ResponseWriter, r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		return NoContent(w)
	})
	rt.Handle("PUT /api/v1/unlimited", Authenticated().WithMaxBody(-1), func(w http.ResponseWriter, r *http.Request) error {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return err
		}
		return WriteJSON(w, 200, map[string]int64{"n": n})
	})
	rt.Handle("GET /api/v1/boom", Public(), func(w http.ResponseWriter, r *http.Request) error {
		return errors.New("secret internal detail")
	})
	rt.Handle("GET /api/v1/panic", Public(), func(w http.ResponseWriter, r *http.Request) error { panic("kaboom") })
	rt.Handle("GET /api/v1/wrapped", Public(), func(w http.ResponseWriter, r *http.Request) error {
		return errors.Join(errors.New("ctx"), core.Conflict("already exists"))
	})
	rt.Handle("GET /hdhr/{feed_token}/discover.json", Public(), ok)
	rt.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "spa") }))
	return rt
}

type req struct {
	method, path, body string
	hdr                map[string]string
}

func do(rt http.Handler, q req) *httptest.ResponseRecorder {
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	r := httptest.NewRequest(q.method, "http://nas.local"+q.path, body)
	for k, v := range q.hdr {
		if k == "Cookie" {
			r.AddCookie(&http.Cookie{Name: "lw_session", Value: v})
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &b)
	return b.Error.Code
}

func TestAccessRules(t *testing.T) {
	rt := newTestRouter(t, nil, nil)
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	cases := []struct {
		name   string
		q      req
		status int
		code   string
	}{
		{"public anon", req{method: "GET", path: "/api/v1/health"}, 200, ""},
		{"auth anon 401", req{method: "GET", path: "/api/v1/me"}, 401, "unauthorized"},
		{"auth bad token 401", req{method: "GET", path: "/api/v1/me", hdr: bearer("nope")}, 401, "unauthorized"},
		{"auth ok", req{method: "GET", path: "/api/v1/me", hdr: bearer("user")}, 200, ""},
		{"scope ok via *", req{method: "GET", path: "/api/v1/files", hdr: bearer("user")}, 200, ""},
		{"scope ok explicit", req{method: "GET", path: "/api/v1/files", hdr: bearer("files-ro")}, 200, ""},
		{"scope missing 403", req{method: "POST", path: "/api/v1/files", hdr: bearer("files-ro")}, 403, "forbidden"},
		{"admin anon 401", req{method: "GET", path: "/api/v1/admin/users"}, 401, "unauthorized"},
		{"admin as user 403", req{method: "GET", path: "/api/v1/admin/users", hdr: bearer("user")}, 403, "forbidden"},
		{"admin scope on non-admin user 403", req{method: "GET", path: "/api/v1/admin/users", hdr: bearer("user-adm")}, 403, "forbidden"},
		{"admin token w/o admin scope 403", req{method: "GET", path: "/api/v1/admin/users", hdr: bearer("adm-tok")}, 403, "forbidden"},
		{"admin ok", req{method: "GET", path: "/api/v1/admin/users", hdr: bearer("admin")}, 200, ""},
		{"query token refused by default", req{method: "GET", path: "/api/v1/files?access_token=user"}, 401, "unauthorized"},
		{"query token allowed when opted in", req{method: "GET", path: "/api/v1/stream?access_token=user"}, 200, ""},
		{"authenticator core error", req{method: "GET", path: "/api/v1/me", hdr: map[string]string{"Cookie": "disabled"}}, 403, "forbidden"},
		{"unknown api 404 json", req{method: "GET", path: "/api/v1/nope"}, 404, "not_found"},
		{"wrong method 405 json", req{method: "DELETE", path: "/api/v1/me", hdr: bearer("user")}, 405, "method_not_allowed"},
		{"internal error hidden", req{method: "GET", path: "/api/v1/boom"}, 500, "internal"},
		{"panic recovered", req{method: "GET", path: "/api/v1/panic"}, 500, "internal"},
		{"wrapped core error", req{method: "GET", path: "/api/v1/wrapped"}, 409, "conflict"},
		{"spa fallback", req{method: "GET", path: "/files/some/deep/link"}, 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(rt, c.q)
			if w.Code != c.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, c.status, w.Body)
			}
			if c.code != "" && errCode(t, w) != c.code {
				t.Fatalf("code = %q, want %q", errCode(t, w), c.code)
			}
			if c.status == 500 && strings.Contains(w.Body.String(), "secret internal detail") {
				t.Fatal("internal error leaked")
			}
		})
	}
	w := do(rt, req{method: "DELETE", path: "/api/v1/me", hdr: bearer("user")})
	if !strings.Contains(w.Header().Get("Allow"), "GET") {
		t.Fatalf("Allow = %q", w.Header().Get("Allow"))
	}
	if w := do(rt, req{method: "GET", path: "/api/v1/me"}); w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 without WWW-Authenticate")
	}
}

func TestCSRF(t *testing.T) {
	rt := newTestRouter(t, nil, nil)
	cookie := func(extra map[string]string) map[string]string {
		m := map[string]string{"Cookie": "user"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name   string
		q      req
		status int
		who    string
	}{
		{"cookie GET needs no csrf", req{method: "GET", path: "/api/v1/me", hdr: cookie(nil)}, 200, "user"},
		{"cookie GET same origin", req{method: "GET", path: "/api/v1/me", hdr: cookie(map[string]string{"Origin": "http://nas.local"})}, 200, "user"},
		{"cookie GET foreign origin", req{method: "GET", path: "/api/v1/me", hdr: cookie(map[string]string{"Origin": "https://evil.example"})}, 403, ""},
		{"cookie GET null origin", req{method: "GET", path: "/api/v1/me", hdr: cookie(map[string]string{"Origin": "null"})}, 403, ""},
		{"cookie GET foreign origin on public route is anonymous", req{method: "GET", path: "/api/v1/health", hdr: cookie(map[string]string{"Origin": "https://evil.example"})}, 200, ""},
		{"cookie GET cross-site navigation keeps session", req{method: "GET", path: "/api/v1/me", hdr: cookie(map[string]string{"Sec-Fetch-Site": "cross-site"})}, 200, "user"},
		{"cookie NoCSRF route foreign origin is anonymous", req{method: "POST", path: "/api/v1/setup", hdr: cookie(map[string]string{"Origin": "https://evil.example"})}, 200, ""},
		{"cookie POST without token", req{method: "POST", path: "/api/v1/me", hdr: cookie(nil)}, 403, ""},
		{"cookie POST wrong token", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-admin"})}, 403, ""},
		{"cookie POST good token", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user"})}, 200, "user"},
		{"good token same origin", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user", "Origin": "http://nas.local"})}, 200, "user"},
		{"good token foreign origin", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user", "Origin": "https://evil.example"})}, 403, ""},
		{"good token null origin", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user", "Origin": "null"})}, 403, ""},
		{"good token cross-site fetch", req{method: "POST", path: "/api/v1/me", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user", "Sec-Fetch-Site": "cross-site"})}, 403, ""},
		{"bearer exempt from csrf", req{method: "POST", path: "/api/v1/me", hdr: map[string]string{"Authorization": "Bearer user", "Origin": "https://evil.example"}}, 200, "user"},
		{"NoCSRF route", req{method: "POST", path: "/api/v1/setup", hdr: cookie(nil)}, 200, "user"},
		{"public route drops cookie principal without csrf", req{method: "POST", path: "/api/v1/public-write", hdr: cookie(nil)}, 200, ""},
		{"public route keeps principal with csrf", req{method: "POST", path: "/api/v1/public-write", hdr: cookie(map[string]string{"X-CSRF-Token": "csrf-user"})}, 200, "user"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(rt, c.q)
			if w.Code != c.status {
				t.Fatalf("status = %d, want %d; %s", w.Code, c.status, w.Body)
			}
			if c.status == 200 {
				var b map[string]string
				json.Unmarshal(w.Body.Bytes(), &b)
				if b["who"] != c.who {
					t.Fatalf("who = %q, want %q", b["who"], c.who)
				}
			}
		})
	}
}

func TestCSRFOriginWithTLSAndPublicURL(t *testing.T) {
	st := testSettings(t)
	st.Update(context.Background(), "general", json.RawMessage(`{"public_url":"https://files.example.com"}`))
	rt := newTestRouter(t, nil, st)
	post := func(origin string, tls bool, remote string, xfp string) int {
		r := httptest.NewRequest("POST", "http://nas.local/api/v1/me", nil)
		if tls {
			r = httptest.NewRequest("POST", "https://nas.local/api/v1/me", nil)
		}
		if remote != "" {
			r.RemoteAddr = remote
		}
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		r.AddCookie(&http.Cookie{Name: "lw_session", Value: "user"})
		r.Header.Set("X-CSRF-Token", "csrf-user")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w.Code
	}
	if c := post("https://nas.local", true, "", ""); c != 200 {
		t.Fatalf("https same origin: %d", c)
	}
	if c := post("http://nas.local", true, "", ""); c != 403 {
		t.Fatalf("http origin on https request must fail: %d", c)
	}
	if c := post("https://files.example.com", false, "", ""); c != 200 {
		t.Fatalf("public_url origin: %d", c)
	}
	if c := post("http://files.example.com", false, "", ""); c != 403 {
		t.Fatalf("public_url scheme mismatch: %d", c)
	}
	// Trusted proxy says https: an http origin is rejected.
	if c := post("http://nas.local", false, "10.1.2.3:4444", "https"); c != 403 {
		t.Fatalf("trusted proxy https: %d", c)
	}
	// Untrusted peer cannot claim https via header.
	if c := post("http://nas.local", false, "192.0.2.1:4444", "https"); c != 200 {
		t.Fatalf("untrusted X-Forwarded-Proto believed: %d", c)
	}
}

func testSettings(t *testing.T) *settings.Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := settings.New(context.Background(), d, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSetupGate(t *testing.T) {
	required := true
	rt := newTestRouter(t, &required, nil)
	allowed := []req{
		{method: "GET", path: "/api/v1/health"},
		{method: "HEAD", path: "/api/v1/health"},
		{method: "GET", path: "/api/v1/setup/status"},
		{method: "POST", path: "/api/v1/setup"},
		{method: "GET", path: "/some/spa/route"},
	}
	for _, q := range allowed {
		if w := do(rt, q); w.Code == 503 {
			t.Errorf("%s %s gated", q.method, q.path)
		}
	}
	gated := []req{
		{method: "GET", path: "/api/v1/me", hdr: map[string]string{"Authorization": "Bearer admin"}},
		{method: "GET", path: "/api/v1/files"},
		{method: "GET", path: "/api/v1/nonexistent"},
		{method: "POST", path: "/api/v1/setup/status"},
		{method: "GET", path: "/api/v1/setup"},
	}
	for _, q := range gated {
		w := do(rt, q)
		if w.Code != 503 || errCode(t, w) != "setup_required" {
			t.Errorf("%s %s: %d %s", q.method, q.path, w.Code, w.Body)
		}
	}
	required = false
	if w := do(rt, req{method: "GET", path: "/api/v1/me", hdr: map[string]string{"Authorization": "Bearer admin"}}); w.Code != 200 {
		t.Fatalf("after setup: %d", w.Code)
	}
}

func TestCORS(t *testing.T) {
	st := testSettings(t)
	rt := newTestRouter(t, nil, st)
	ext := "chrome-extension://abcdefghijklmnopabcdefghijklmnop"
	preflight := func(origin, headers string) *httptest.ResponseRecorder {
		return do(rt, req{method: "OPTIONS", path: "/api/v1/files", hdr: map[string]string{
			"Origin": origin, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": headers}})
	}
	// Empty allow-list: any extension origin may use Bearer.
	w := preflight(ext, "authorization")
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != ext || w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
	if w := preflight("https://evil.example", "authorization"); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("web origin got CORS")
	}
	if w := preflight(ext, "content-type"); w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("preflight without Authorization must be refused")
	}
	// Actual requests: Bearer gets ACAO, cookie does not.
	w = do(rt, req{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": ext, "Authorization": "Bearer user"}})
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != ext || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Upload-Offset") {
		t.Fatalf("bearer actual: %d %v", w.Code, w.Header())
	}
	w = do(rt, req{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": ext, "Cookie": "user"}})
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("cookie request got CORS")
	}
	// Regression: cookie reads from an extension origin used to be served (200 with the data,
	// just without ACAO). An extension with a host permission skips CORS and could read them.
	if w.Code != 403 || strings.Contains(w.Body.String(), `"who"`) {
		t.Fatalf("cookie GET from an extension origin: %d %s", w.Code, w.Body)
	}
	// A public route still answers, but never as the cookie's user.
	w = do(rt, req{method: "GET", path: "/api/v1/setup/status", hdr: map[string]string{"Origin": ext, "Cookie": "user"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"who":""`) {
		t.Fatalf("public route with extension cookie: %d %s", w.Code, w.Body)
	}
	// Bearer wins over a cookie that happens to be sent along.
	w = do(rt, req{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": ext, "Cookie": "admin", "Authorization": "Bearer user"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"who":"user"`) {
		t.Fatalf("bearer + cookie from extension: %d %s", w.Code, w.Body)
	}
	// With an allow-list only listed origins pass.
	if err := st.Update(context.Background(), "security", json.RawMessage(`{"allowed_extension_origins":["moz-extension://11111111-2222-3333-4444-555555555555"]}`)); err != nil {
		t.Fatal(err)
	}
	if w := preflight(ext, "authorization"); w.Code != 403 {
		t.Fatalf("unlisted extension allowed: %d", w.Code)
	}
	if w := preflight("moz-extension://11111111-2222-3333-4444-555555555555", "Authorization, Content-Type"); w.Code != 204 {
		t.Fatalf("listed extension refused: %d", w.Code)
	}
	// Regression: the allow-list used to be applied only through CORS headers, so an unlisted
	// extension with a host permission (which skips CORS) could still use a Bearer token.
	for _, q := range []req{
		{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": ext, "Authorization": "Bearer user"}},
		{method: "POST", path: "/api/v1/files", hdr: map[string]string{"Origin": ext, "Authorization": "Bearer user"}},
		{method: "GET", path: "/api/v1/setup/status", hdr: map[string]string{"Origin": ext}},
		{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": "CHROME-EXTENSION://ABCDEFGHIJKLMNOPABCDEFGHIJKLMNOP", "Authorization": "Bearer user"}},
		{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": "chrome-extension://not-a-valid-id", "Authorization": "Bearer user"}},
	} {
		w := do(rt, q)
		if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" || strings.Contains(w.Body.String(), `"who"`) {
			t.Fatalf("unlisted extension %s %s %s: %d %v %s", q.hdr["Origin"], q.method, q.path, w.Code, w.Header(), w.Body)
		}
	}
	listed := "moz-extension://11111111-2222-3333-4444-555555555555"
	w = do(rt, req{method: "GET", path: "/api/v1/files", hdr: map[string]string{"Origin": listed, "Authorization": "Bearer user"}})
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != listed {
		t.Fatalf("listed extension bearer GET: %d %v", w.Code, w.Header())
	}
	// Non-API paths never get CORS.
	if w := do(rt, req{method: "GET", path: "/hdhr/x/discover.json", hdr: map[string]string{"Origin": ext, "Authorization": "Bearer user"}}); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS outside /api/v1")
	}
}

func TestBodyLimitsAndDecodeJSON(t *testing.T) {
	rt := newTestRouter(t, nil, nil)
	auth := map[string]string{"Authorization": "Bearer user"}
	withCT := map[string]string{"Authorization": "Bearer user", "Content-Type": "application/json"}
	if w := do(rt, req{method: "PUT", path: "/api/v1/big", body: "01234567890123", hdr: auth}); w.Code != 413 || errCode(t, w) != "payload_too_large" {
		t.Fatalf("over limit: %d %s", w.Code, w.Body)
	}
	if w := do(rt, req{method: "PUT", path: "/api/v1/big", body: "0123", hdr: auth}); w.Code != 204 {
		t.Fatalf("under limit: %d", w.Code)
	}
	big := strings.Repeat("x", 3<<20)
	if w := do(rt, req{method: "PUT", path: "/api/v1/unlimited", body: big, hdr: auth}); w.Code != 200 {
		t.Fatalf("unlimited: %d", w.Code)
	}
	huge := `{"a":"` + strings.Repeat("x", 2<<20) + `"}`
	if w := do(rt, req{method: "POST", path: "/api/v1/echo", body: huge, hdr: withCT}); w.Code != 413 {
		t.Fatalf("default 1MiB: %d", w.Code)
	}
	cases := map[string]int{
		`{"a":1}`:             200,
		`{"a":1,"unknown":2}`: 200,
		`{"a":1} {"b":2}`:     400,
		`{"a":1}xyz`:          400,
		`{"a":`:               400,
		``:                    400,
		`[1]`:                 400,
	}
	for body, want := range cases {
		if w := do(rt, req{method: "POST", path: "/api/v1/echo", body: body, hdr: withCT}); w.Code != want {
			t.Errorf("body %q: %d, want %d (%s)", body, w.Code, want, w.Body)
		}
	}
	if w := do(rt, req{method: "POST", path: "/api/v1/echo", body: `{"a":1}`, hdr: map[string]string{"Authorization": "Bearer user", "Content-Type": "text/plain"}}); w.Code != 415 {
		t.Fatalf("text/plain: %d", w.Code)
	}
}

// brokenBody yields some data and then fails the way net/http's body reader does when the
// client hangs up mid-body.
type brokenBody struct {
	data []byte
	err  error
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// Regression: a request body that fails mid-read (client disconnect, reset, short body,
// idle timeout) surfaced as a 500 "internal error" logged at ERROR.
func TestClientBodyAbortIsAClientError(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rt := NewRouter(Options{Auth: fakeAuth{}, Log: log, RateLimit: RateLimitOptions{Disabled: true}})
	var handlerErr error
	rt.Handle("PUT /api/v1/unlimited", Authenticated().WithMaxBody(-1), func(w http.ResponseWriter, r *http.Request) error {
		_, handlerErr = io.Copy(io.Discard, r.Body)
		return handlerErr
	})
	rt.Handle("PUT /api/v1/limited", Authenticated().WithMaxBody(1<<20), func(w http.ResponseWriter, r *http.Request) error {
		_, handlerErr = io.Copy(io.Discard, r.Body)
		return handlerErr
	})
	rt.Handle("POST /api/v1/echo", Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		var v map[string]any
		return DecodeJSON(r, &v)
	})
	for _, cause := range []error{io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, os.ErrDeadlineExceeded} {
		for _, p := range []string{"/api/v1/unlimited", "/api/v1/limited", "/api/v1/echo"} {
			logs.Reset()
			method := "PUT"
			if p == "/api/v1/echo" {
				method = "POST"
			}
			r := httptest.NewRequest(method, "http://nas.local"+p, &brokenBody{data: []byte(`{"a": "xxxxxxxx`), err: cause})
			r.Header.Set("Authorization", "Bearer user")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, r)
			if w.Code != 400 || errCode(t, w) != "bad_request" || !strings.Contains(w.Body.String(), "ended early") {
				t.Errorf("%s with %v: %d %s", p, cause, w.Code, w.Body)
			}
			if strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "status=400") {
				t.Errorf("%s with %v logged as:\n%s", p, cause, logs.String())
			}
			if p != "/api/v1/echo" && !errors.Is(handlerErr, cause) {
				t.Errorf("cause not preserved for errors.Is: %v", handlerErr)
			}
		}
	}
	// Too large is still 413, and a clean short body is still fine.
	r := httptest.NewRequest("PUT", "http://nas.local/api/v1/limited", strings.NewReader(strings.Repeat("x", 2<<20)))
	r.ContentLength = -1 // unknown length: the limit trips while the handler reads
	r.Header.Set("Authorization", "Bearer user")
	handlerErr = nil
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("over limit: %d %s", w.Code, w.Body)
	}
	var bre *BodyReadError
	var mbe *http.MaxBytesError
	if errors.As(handlerErr, &bre) || !errors.As(handlerErr, &mbe) {
		t.Fatalf("MaxBytesError not passed through as is: %v", handlerErr)
	}
}

func TestHeadersAndLogging(t *testing.T) {
	rt := newTestRouter(t, nil, nil)
	w := do(rt, req{method: "GET", path: "/api/v1/health"})
	h := w.Header()
	for k, v := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"Content-Security-Policy":      DefaultCSP,
	} {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	if h.Get("Strict-Transport-Security") != "" || h.Get("X-Request-Id") == "" {
		t.Error("HSTS on plain HTTP or missing request id")
	}
	r := httptest.NewRequest("GET", "https://nas.local/api/v1/health", nil)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS over TLS")
	}
	if got := redactPath("/hdhr/lwp_SECRET/discover.json", "GET /hdhr/{feed_token}/discover.json"); strings.Contains(got, "SECRET") {
		t.Errorf("token not redacted: %s", got)
	}
	if got := redactPath("/api/v1/files/abc", "GET /api/v1/files/{id}"); got != "/api/v1/files/abc" {
		t.Errorf("redacted unexpectedly: %s", got)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	rt := NewRouter(Options{Auth: fakeAuth{}, RateLimit: RateLimitOptions{AuthRPS: 1, AuthBurst: 5, AnonRPS: 1, AnonBurst: 3}})
	rt.Handle("GET /api/v1/x", Public(), func(w http.ResponseWriter, r *http.Request) error { return NoContent(w) })
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, do(rt, req{method: "GET", path: "/api/v1/x"}).Code)
	}
	if codes[2] != 204 || codes[3] != 429 {
		t.Fatalf("codes = %v", codes)
	}
	w := do(rt, req{method: "GET", path: "/api/v1/x"})
	if w.Header().Get("Retry-After") == "" || errCode(t, w) != "rate_limited" {
		t.Fatal("missing Retry-After")
	}
	// A principal has its own, larger bucket.
	admin := map[string]string{"Authorization": "Bearer admin"}
	codes = codes[:0]
	for i := 0; i < 6; i++ {
		codes = append(codes, do(rt, req{method: "GET", path: "/api/v1/x", hdr: admin}).Code)
	}
	if codes[4] != 204 || codes[5] != 429 {
		t.Fatalf("principal codes = %v", codes)
	}
}

// The defaults leave room for bulk work by a signed-in user while keeping anonymous clients
// on a tighter budget.
func TestRateLimitDefaults(t *testing.T) {
	rt := NewRouter(Options{Auth: fakeAuth{}})
	if rt.authLimiter.rate != AuthRateLimitRPS || rt.authLimiter.burst != AuthRateLimitBurst ||
		rt.anonLimiter.rate != AnonRateLimitRPS || rt.anonLimiter.burst != AnonRateLimitBurst {
		t.Fatalf("limits: auth %v/%v anon %v/%v", rt.authLimiter.rate, rt.authLimiter.burst, rt.anonLimiter.rate, rt.anonLimiter.burst)
	}
	if AuthRateLimitRPS != 200 || AuthRateLimitBurst != 1000 || AnonRateLimitRPS != 30 || AnonRateLimitBurst != 120 {
		t.Fatal("documented limits changed (update ARCHITECTURE.md §2.6 and docs/api/index.md)")
	}
	rt.Handle("GET /api/v1/x", Public(), func(w http.ResponseWriter, r *http.Request) error { return NoContent(w) })
	user := map[string]string{"Authorization": "Bearer user"}
	for i := 0; i < AuthRateLimitBurst; i++ { // e.g. a client syncing a large folder
		if w := do(rt, req{method: "GET", path: "/api/v1/x", hdr: user}); w.Code != 204 {
			t.Fatalf("signed-in request %d throttled", i)
		}
	}
	anon := 0
	for i := 0; i < AnonRateLimitBurst+10; i++ {
		if do(rt, req{method: "GET", path: "/api/v1/x"}).Code == 204 {
			anon++
		}
	}
	if anon < AnonRateLimitBurst || anon > AnonRateLimitBurst+5 {
		t.Fatalf("anonymous requests allowed: %d", anon)
	}
}

// tus chunks and Range requests for content skip the generic limiter (they are bounded by
// their own locks and caps); everything else on the same routes still counts.
func TestRateLimitExemptions(t *testing.T) {
	rt := NewRouter(Options{Auth: fakeAuth{}, RateLimit: RateLimitOptions{AuthRPS: 0.001, AuthBurst: 1, AnonRPS: 0.001, AnonBurst: 1}})
	ok := func(w http.ResponseWriter, r *http.Request) error { return NoContent(w) }
	rt.Handle("PATCH /api/v1/uploads/{id}", Authenticated().WithMaxBody(-1), ok)
	rt.Handle("HEAD /api/v1/uploads/{id}", Authenticated(), ok)
	rt.Handle("GET /api/v1/files/{id}/content", Authenticated().WithQueryToken(), ok)
	rt.Handle("GET /api/v1/media/{file_id}/{asset}", Authenticated().WithQueryToken(), ok)
	rt.Handle("GET /api/v1/tv/channels/{id}/programs/{file_id}/stream", Authenticated().WithQueryToken(), ok)
	rt.Handle("GET /api/v1/files/{id}", Authenticated(), ok)
	user := map[string]string{"Authorization": "Bearer user"}
	rng := map[string]string{"Authorization": "Bearer user", "Range": "bytes=0-99"}
	for i := 0; i < 50; i++ {
		for _, r := range []req{
			{method: "PATCH", path: "/api/v1/uploads/u1", hdr: user},
			{method: "HEAD", path: "/api/v1/uploads/u1", hdr: user},
			{method: "GET", path: "/api/v1/files/f1/content", hdr: rng},
			{method: "GET", path: "/api/v1/media/f1/stream", hdr: rng},
			{method: "GET", path: "/api/v1/tv/channels/c1/programs/f1/stream", hdr: rng},
		} {
			if w := do(rt, r); w.Code != 204 {
				t.Fatalf("exempt %s %s throttled at %d: %d", r.method, r.path, i, w.Code)
			}
		}
	}
	// The user's single token is still there for an ordinary request, then it is gone:
	// whole-file downloads, posters and transcodes are not exempt.
	if w := do(rt, req{method: "GET", path: "/api/v1/files/f1", hdr: user}); w.Code != 204 {
		t.Fatalf("exempt requests consumed the bucket: %d", w.Code)
	}
	for _, r := range []req{
		{method: "GET", path: "/api/v1/files/f1/content", hdr: user},
		{method: "HEAD", path: "/api/v1/files/f1/content", hdr: rng},
		{method: "GET", path: "/api/v1/media/f1/transcode", hdr: rng},
		{method: "GET", path: "/api/v1/media/f1/poster", hdr: rng},
		{method: "GET", path: "/api/v1/files/f1/content", hdr: map[string]string{"Authorization": "Bearer user", "Range": "items=0-1"}},
	} {
		if w := do(rt, r); w.Code != 429 {
			t.Fatalf("%s %s %v not limited: %d", r.method, r.path, r.hdr, w.Code)
		}
	}
	// Exempt routes still require authentication.
	if w := do(rt, req{method: "PATCH", path: "/api/v1/uploads/u1"}); w.Code != 401 {
		t.Fatalf("anonymous tus PATCH: %d", w.Code)
	}
}

func TestRegistrationPanics(t *testing.T) {
	rt := NewRouter(Options{})
	for _, p := range []string{"/api/v1/no-method", "get /x", "GET"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("pattern %q accepted", p)
				}
			}()
			rt.Handle(p, Public(), func(http.ResponseWriter, *http.Request) error { return nil })
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Public+Admin accepted")
			}
		}()
		rt.Handle("GET /x", Access{Public: true, Admin: true}, func(http.ResponseWriter, *http.Request) error { return nil })
	}()
}

func TestClientIP(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	cases := []struct {
		remote, xff, want string
	}{
		{"192.0.2.9:1234", "", "192.0.2.9"},
		{"192.0.2.9:1234", "1.2.3.4", "192.0.2.9"},                 // untrusted peer: XFF ignored
		{"10.0.0.1:1234", "1.2.3.4", "1.2.3.4"},                    // trusted proxy
		{"10.0.0.1:1234", "6.6.6.6, 1.2.3.4, 10.0.0.2", "1.2.3.4"}, // spoofed left entry ignored
		{"10.0.0.1:1234", "10.0.0.3, 10.0.0.2", "10.0.0.3"},        // all trusted: leftmost
		{"10.0.0.1:1234", "garbage", "10.0.0.1"},
		{"[fd00::1]:1234", "2001:db8::5", "2001:db8::5"},
		{"[::ffff:10.0.0.1]:1234", "1.2.3.4", "1.2.3.4"}, // v4-mapped peer
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		ip, _, _ := resolveClient(r, proxies)
		if ip.String() != c.want {
			t.Errorf("%s / %q: got %s want %s", c.remote, c.xff, ip, c.want)
		}
	}
	if RateKeyForIP("2001:db8:1:2:3:4:5:6") != RateKeyForIP("2001:db8:1:2:ffff::1") {
		t.Error("IPv6 /64 not grouped")
	}
}

func TestCursorTamper(t *testing.T) {
	type cur struct {
		T  string `json:"t"`
		ID string `json:"id"`
	}
	s := EncodeCursor(cur{T: "2026-10-26T00:00:00.000Z", ID: "abc"})
	var got cur
	if err := DecodeCursor(s, &got); err != nil || got.ID != "abc" {
		t.Fatal(err)
	}
	b := []byte(s)
	for i := range b {
		mut := append([]byte(nil), b...)
		mut[i] ^= 0x01
		if err := DecodeCursor(string(mut), &got); err == nil && string(mut) != s {
			// Some single-bit flips in base64 padding bits decode identically; ensure payload unchanged.
			var again cur
			DecodeCursor(string(mut), &again)
			if again != (cur{T: "2026-10-26T00:00:00.000Z", ID: "abc"}) {
				t.Fatalf("tampered cursor accepted at byte %d", i)
			}
		}
	}
	if DecodeCursor("", &got) == nil || DecodeCursor("!!!", &got) == nil || DecodeCursor(strings.Repeat("A", 5000), &got) == nil {
		t.Fatal("garbage accepted")
	}
	// Swap in a fresh random key (not a fixed one: with -count>1 a fixed key would already be
	// installed by the previous run, so s would have been signed with it) and restore the
	// original afterwards so the test is repeatable and does not leak into other tests.
	prev := *cursorKey.Load()
	t.Cleanup(func() { SetCursorKey(prev) })
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	SetCursorKey(other)
	if DecodeCursor(s, &got) == nil {
		t.Fatal("cursor from another key accepted")
	}
	var ce *core.Error
	if err := DecodeCursor("abc", &got); !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("err = %v", err)
	}
}

func TestPageAndParseLimit(t *testing.T) {
	b, _ := json.Marshal(NewPage[int](nil, ""))
	if string(b) != `{"items":[],"next_cursor":null}` {
		t.Fatal(string(b))
	}
	r := httptest.NewRequest("GET", "/?limit=500", nil)
	if ParseLimit(r, 50, 200) != 200 {
		t.Fatal("clamp")
	}
	r = httptest.NewRequest("GET", "/?limit=-3", nil)
	if ParseLimit(r, 50, 200) != 50 {
		t.Fatal("negative")
	}
	r = httptest.NewRequest("GET", "/?limit=7", nil)
	if ParseLimit(r, 50, 200) != 7 {
		t.Fatal("value")
	}
}

func TestKeyedLimiterMath(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewKeyedLimiter(2, 4)
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("burst denied at %d", i)
		}
	}
	ok, wait := l.Allow("k")
	if ok || wait < 400*time.Millisecond || wait > 600*time.Millisecond {
		t.Fatalf("ok=%v wait=%v", ok, wait)
	}
	now = now.Add(500 * time.Millisecond) // +1 token
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refill failed")
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("keys not independent")
	}
	now = now.Add(time.Hour)
	l.Allow("fresh")
	if l.Len() != 1 {
		t.Fatalf("idle buckets not swept: %d", l.Len())
	}
}

func TestLockoutMath(t *testing.T) {
	want := map[int]time.Duration{1: 0, 5: 0, 6: 2 * time.Second, 7: 4 * time.Second, 10: 32 * time.Second, 14: 512 * time.Second, 15: 15 * time.Minute, 100: 15 * time.Minute}
	for f, d := range want {
		if got := LockoutDelay(f, 5, time.Second, 15*time.Minute); got != d {
			t.Errorf("failures=%d: %v want %v", f, got, d)
		}
	}
	now := time.Unix(0, 0)
	l := NewLockout()
	l.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if d := l.Failure("alice"); d != 0 {
			t.Fatalf("free failure %d locked %v", i, d)
		}
	}
	if l.Check("alice") != 0 {
		t.Fatal("locked after free failures")
	}
	if d := l.Failure("alice"); d != 2*time.Second || l.Check("alice") != 2*time.Second {
		t.Fatalf("6th failure: %v", d)
	}
	now = now.Add(3 * time.Second)
	if l.Check("alice") != 0 {
		t.Fatal("lock did not expire")
	}
	if d := l.Failure("alice"); d != 4*time.Second {
		t.Fatalf("7th failure: %v", d)
	}
	l.Success("alice")
	if l.Check("alice") != 0 || l.Failures("alice") != 0 {
		t.Fatal("success did not reset")
	}
	for i := 0; i < 6; i++ {
		l.Failure("bob")
	}
	now = now.Add(25 * time.Hour)
	if l.Check("bob") != 0 || l.Failure("bob") != 0 {
		t.Fatal("state not forgotten after Forget window")
	}
}

// Begin decides and reserves atomically: attempts still in flight count as failures, so a
// simultaneous burst gets no more verifications than a sequence (5 free + the one that locks).
func TestLockoutBeginCountsAttemptsInFlight(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLockout()
	l.now = func() time.Time { return now }
	var admitted []*Attempt
	refused := 0
	for i := 0; i < 20; i++ {
		a, d := l.Begin("alice")
		if d > 0 {
			refused++
			if a != nil || d != LockoutRetryInFlight {
				t.Fatalf("refused attempt: %v %v", a, d)
			}
			continue
		}
		admitted = append(admitted, a)
	}
	if len(admitted) != 6 || refused != 14 {
		t.Fatalf("admitted %d, refused %d; want 6 and 14", len(admitted), refused)
	}
	for _, a := range admitted {
		a.Fail()
		a.Fail()    // only the first outcome counts
		a.Release() // no-op after an outcome
	}
	if l.Failures("alice") != 6 || l.Check("alice") != 2*time.Second {
		t.Fatalf("failures %d, lock %v", l.Failures("alice"), l.Check("alice"))
	}
	if _, d := l.Begin("alice"); d != 2*time.Second {
		t.Fatalf("locked key admitted: %v", d)
	}
	// After the lock expires exactly one attempt at a time is verified.
	now = now.Add(3 * time.Second)
	a, d := l.Begin("alice")
	if d != 0 {
		t.Fatalf("not admitted after expiry: %v", d)
	}
	if _, d := l.Begin("alice"); d != LockoutRetryInFlight {
		t.Fatalf("second concurrent attempt after expiry: %v", d)
	}
	a.Release() // e.g. a 2FA code was still missing: neither failure nor success
	if l.Failures("alice") != 6 {
		t.Fatal("release changed the failure count")
	}
	a, _ = l.Begin("alice")
	a.Succeed()
	if l.Failures("alice") != 0 || l.Check("alice") != 0 {
		t.Fatal("success did not reset")
	}
	// Finished attempts leave no state behind; other keys are independent.
	b, _ := l.Begin("bob")
	b.Release()
	if len(l.state) != 0 {
		t.Fatalf("state leaked: %v", l.state)
	}
	// A success while others are in flight keeps their reservations.
	x, _ := l.Begin("carol")
	y, _ := l.Begin("carol")
	x.Succeed()
	if l.state["carol"] == nil || l.state["carol"].inflight != 1 {
		t.Fatal("success dropped another attempt's reservation")
	}
	y.Fail()
	if l.Failures("carol") != 1 {
		t.Fatal("failure after a concurrent success not counted")
	}
}

func TestPrincipalHas(t *testing.T) {
	user := &core.Principal{Role: core.RoleUser, Scopes: []string{"*"}}
	admin := &core.Principal{Role: core.RoleAdmin, Scopes: []string{"*"}}
	tok := &core.Principal{Role: core.RoleAdmin, Scopes: []string{"files:read"}}
	if !user.Has("files:write") || user.Has("admin") || !admin.Has("admin") || tok.Has("admin") || tok.Has("files:write") || !tok.Has("files:read") {
		t.Fatal("scope rules wrong")
	}
	var nilP *core.Principal
	if nilP.Has("x") || nilP.IsAdmin() {
		t.Fatal("nil principal")
	}
}
