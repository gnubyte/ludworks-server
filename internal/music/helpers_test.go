package music_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/media"
	"ludworks/internal/music"
	mt "ludworks/internal/music/musictest"
)

const userPW = "user password 1234"

type env struct {
	t     *testing.T
	e     *authtest.Env
	fs    *files.Service
	media *media.Service
	mu    *music.Module
	svc   *music.Service
	admin *authtest.Client // cookie session with CSRF
	adm   *authtest.Client // bearer session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var (
		fm *files.Module
		mm *media.Module
		mu *music.Module
	)
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		m := mail.New(e.Deps)
		fm = files.New(e.Deps)
		mm = media.New(e.Deps, fm.Service())
		mu = music.New(e.Deps, fm.Service(), mm.Service())
		return []httpx.Module{m, admin.New(e.Deps, e.Auth, m), fm, mm, mu}
	}})
	v := &env{t: t, e: e, fs: fm.Service(), media: mm.Service(), mu: mu, svc: mu.Service()}
	v.admin = e.Setup()
	v.adm = e.TokenLogin(authtest.AdminUser, authtest.AdminPassword)
	return v
}

// user creates an account and returns its id and a bearer client.
func (v *env) user(name string) (string, *authtest.Client) {
	v.t.Helper()
	body := map[string]any{"username": name, "email": name + "@example.com", "password": userPW, "role": "user"}
	u := v.admin.Do("POST", "/api/v1/admin/users", body).Expect(201).Map()
	return u["id"].(string), v.e.TokenLogin(name, userPW)
}

// token creates a personal access token with scopes for c.
func (v *env) token(c *authtest.Client, scopes ...string) *authtest.Client {
	v.t.Helper()
	r := c.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "t", "scopes": scopes}).Expect(201).Map()
	return v.e.WithBearer(r["token"].(string))
}

type rawResp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *rawResp) expect(t *testing.T, status int) *rawResp {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
	return r
}

func (r *rawResp) code() string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func (r *rawResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
	return m
}

// raw sends a request with arbitrary headers/body using c's credentials (nil = anonymous).
func (v *env) raw(c *authtest.Client, method, path string, hdr map[string]string, body []byte) *rawResp {
	v.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, v.e.URL+path, rd)
	if err != nil {
		v.t.Fatal(err)
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	httpc := http.DefaultClient
	if c != nil {
		if c.Bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.Bearer)
		}
		if c.CSRF != "" && method != http.MethodGet && method != http.MethodHead {
			req.Header.Set("X-CSRF-Token", c.CSRF)
		}
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

// home returns the caller's home space id.
func (v *env) home(c *authtest.Client) string {
	v.t.Helper()
	var spaces []map[string]any
	c.Do("GET", "/api/v1/spaces", nil).Expect(200).JSON(&spaces)
	return spaces[0]["id"].(string)
}

// library creates a library space with members ({"user_id","access"}) and returns its id.
func (v *env) library(name string, members ...map[string]any) string {
	v.t.Helper()
	if members == nil {
		members = []map[string]any{}
	}
	r := v.admin.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": name, "members": members}).Expect(201).Map()
	return r["id"].(string)
}

// upload stores content via the simple upload endpoint (creating parent folders).
func (v *env) upload(c *authtest.Client, spaceID, dir, name string, content []byte) string {
	v.t.Helper()
	return v.uploadQ(c, spaceID, dir, name, content, nil)
}

func (v *env) uploadQ(c *authtest.Client, spaceID, dir, name string, content []byte, extra url.Values) string {
	v.t.Helper()
	if dir != "" {
		v.mkdirAll(c, spaceID, dir)
	}
	q := url.Values{"space_id": {spaceID}, "path": {dir}, "name": {name}}
	for k, vals := range extra {
		q[k] = vals
	}
	r := v.raw(c, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": "application/octet-stream"}, content)
	r.expect(v.t, 201)
	return r.json(v.t)["id"].(string)
}

func (v *env) mkdirAll(c *authtest.Client, spaceID, dir string) {
	v.t.Helper()
	parent := ""
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" {
			continue
		}
		r := c.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": spaceID, "path": parent, "name": seg})
		if r.Status != 201 && r.Status != 409 {
			v.t.Fatalf("mkdir %s/%s: %d %s", parent, seg, r.Status, r.Body)
		}
		if parent == "" {
			parent = seg
		} else {
			parent += "/" + seg
		}
	}
}

// track fetches GET /music/tracks/{id}.
func (v *env) track(c *authtest.Client, id string) map[string]any {
	v.t.Helper()
	return c.Do("GET", "/api/v1/music/tracks/"+id, nil).Expect(200).Map()
}

// waitProbed waits until a track's tags were read.
func (v *env) waitProbed(c *authtest.Client, id string) map[string]any {
	v.t.Helper()
	var m map[string]any
	eventually(v.t, "probe of "+id, func() bool {
		r := c.Do("GET", "/api/v1/music/tracks/"+id, nil)
		if r.Status != 200 {
			return false
		}
		m = r.Map()
		return m["probe_status"] != "pending"
	})
	return m
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ctx() context.Context { return context.Background() }

// principalOf builds the core.Principal of a client's session.
func (v *env) principalOf(c *authtest.Client) *core.Principal {
	v.t.Helper()
	me := c.Do("GET", "/api/v1/me", nil).Expect(200).Map()
	return &core.Principal{UserID: me["id"].(string), Username: me["username"].(string), Role: core.Role(me["role"].(string)),
		Scopes: []string{core.ScopeAll}, Kind: core.KindSession}
}

func items(page map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// anyJSON decodes any JSON value (arrays included).
func anyJSON(r *authtest.Response) any {
	var v any
	r.JSON(&v)
	return v
}

func list(v any) []map[string]any {
	var out []map[string]any
	for _, it := range v.([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func field(objs []map[string]any, key string) []string {
	var out []string
	for _, o := range objs {
		out = append(out, toString(o[key]))
	}
	return out
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "<nil>"
	case float64:
		b, _ := json.Marshal(x)
		return string(b)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// taggedMP3 builds an MP3 with an ID3v2.4 tag of the given text frames ("TIT2", "Title", …).
func taggedMP3(seconds float64, frames ...string) []byte {
	var fs []mt.Frame
	for i := 0; i+1 < len(frames); i += 2 {
		fs = append(fs, mt.Text(frames[i], mt.UTF8, frames[i+1]))
	}
	return mt.MP3Xing(mt.ID3v2(mt.ID3Options{Version: 4}, fs...), seconds, nil)
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// timeNow is the current time as an RFC 3339 string.
func timeNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
