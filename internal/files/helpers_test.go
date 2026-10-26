package files_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
)

const userPW = "user password 1234"

type env struct {
	t     *testing.T
	e     *authtest.Env
	fm    *files.Module
	svc   *files.Service
	admin *authtest.Client // cookie session with CSRF
	adm   *authtest.Client // bearer session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvLog(t, nil)
}

// newEnvLog is newEnv with the server log sent to log (nil discards it).
func newEnvLog(t *testing.T, log *slog.Logger) *env {
	t.Helper()
	var fm *files.Module
	e := authtest.New(t, authtest.Options{Log: log, Modules: func(e *authtest.Env) []httpx.Module {
		m := mail.New(e.Deps)
		fm = files.New(e.Deps)
		return []httpx.Module{m, admin.New(e.Deps, e.Auth, m), fm}
	}})
	v := &env{t: t, e: e, fm: fm, svc: fm.Service()}
	t.Cleanup(v.svc.CloseRoot) // runs before the server/DB cleanup registered by authtest.New
	v.admin = e.Setup()
	v.adm = e.TokenLogin(authtest.AdminUser, authtest.AdminPassword)
	v.svc.SetFreeSpace(func(string) (int64, error) { return 1 << 50, nil })
	return v
}

// user creates an account and returns its id and a bearer client.
func (v *env) user(name string, quota int64) (string, *authtest.Client) {
	v.t.Helper()
	body := map[string]any{"username": name, "email": name + "@example.com", "password": userPW, "role": "user"}
	if quota > 0 {
		body["quota_bytes"] = quota
	}
	u := v.admin.Do("POST", "/api/v1/admin/users", body).Expect(201).Map()
	return u["id"].(string), v.e.TokenLogin(name, userPW)
}

// rawResp is a buffered raw response.
type rawResp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *rawResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
	return m
}

// raw sends a request with arbitrary headers/body using c's credentials.
func (v *env) raw(c *authtest.Client, method, path string, hdr map[string]string, body io.Reader) *rawResp {
	v.t.Helper()
	req, err := http.NewRequest(method, v.e.URL+path, body)
	if err != nil {
		v.t.Fatal(err)
	}
	if b, ok := body.(*bytes.Reader); ok {
		req.ContentLength = int64(b.Len())
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	if c != nil {
		if c.Bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.Bearer)
		}
		if c.CSRF != "" && method != http.MethodGet && method != http.MethodHead {
			req.Header.Set("X-CSRF-Token", c.CSRF)
		}
	}
	httpc := http.DefaultClient
	if c != nil {
		httpc = c.HTTP
	}
	resp, err := httpc.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &rawResp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

func (r *rawResp) expect(t *testing.T, status int) *rawResp {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
	return r
}

// home returns the caller's home space id.
func (v *env) home(c *authtest.Client) string {
	v.t.Helper()
	var spaces []map[string]any
	c.Do("GET", "/api/v1/spaces", nil).Expect(200).JSON(&spaces)
	if len(spaces) == 0 || spaces[0]["kind"] != "home" {
		v.t.Fatalf("no home space first: %v", spaces)
	}
	return spaces[0]["id"].(string)
}

// upload stores content via the simple upload endpoint.
func (v *env) upload(c *authtest.Client, spaceID, dir, name string, content []byte, extra ...string) map[string]any {
	v.t.Helper()
	q := url.Values{"space_id": {spaceID}, "path": {dir}, "name": {name}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	r := v.raw(c, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": "application/octet-stream"}, bytes.NewReader(content))
	r.expect(v.t, 201)
	return r.json(v.t)
}

func (v *env) mkdir(c *authtest.Client, spaceID, parent, name string) map[string]any {
	v.t.Helper()
	return c.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": spaceID, "path": parent, "name": name}).Expect(201).Map()
}

func (v *env) list(c *authtest.Client, spaceID, dir string, extra ...string) map[string]any {
	v.t.Helper()
	q := url.Values{"space_id": {spaceID}, "path": {dir}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return c.Do("GET", "/api/v1/files?"+q.Encode(), nil).Expect(200).Map()
}

func names(items any) []string {
	var out []string
	for _, it := range items.([]any) {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func tusMeta(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+" "+b64(kv[i+1]))
	}
	return strings.Join(parts, ",")
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (v *env) dataPath(parts ...string) string {
	return filepath.Join(append([]string{v.e.Deps.Config.DataDir}, parts...)...)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func ctx() context.Context { return context.Background() }

func errCode(r *rawResp) string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

var _ = fmt.Sprintf

// principal builds the core.Principal of a client's session (for direct service calls).
func principal(t *testing.T, v *env, c *authtest.Client) *core.Principal {
	t.Helper()
	me := c.Do("GET", "/api/v1/me", nil).Expect(200).Map()
	return &core.Principal{UserID: me["id"].(string), Username: me["username"].(string), Role: core.Role(me["role"].(string)),
		Scopes: []string{core.ScopeAll}, Kind: core.KindSession}
}
