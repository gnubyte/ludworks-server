package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/media"
)

const userPW = "user password 1234"

type env struct {
	t     *testing.T
	e     *authtest.Env
	fs    *files.Service
	mm    *media.Module
	svc   *media.Service
	admin *authtest.Client // cookie session with CSRF
	adm   *authtest.Client // bearer session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var (
		fm *files.Module
		mm *media.Module
	)
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		m := mail.New(e.Deps)
		fm = files.New(e.Deps)
		mm = media.New(e.Deps, fm.Service())
		return []httpx.Module{m, admin.New(e.Deps, e.Auth, m), fm, mm}
	}})
	v := &env{t: t, e: e, fs: fm.Service(), mm: mm, svc: mm.Service()}
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

// upload stores content via the simple upload endpoint (creating parent folders).
func (v *env) upload(c *authtest.Client, spaceID, dir, name string, content []byte) map[string]any {
	v.t.Helper()
	if dir != "" {
		v.mkdirAll(c, spaceID, dir)
	}
	q := url.Values{"space_id": {spaceID}, "path": {dir}, "name": {name}}
	r := v.raw(c, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": "application/octet-stream"}, content)
	r.expect(v.t, 201)
	return r.json(v.t)
}

func (v *env) mkdirAll(c *authtest.Client, spaceID, dir string) {
	v.t.Helper()
	parent := ""
	for _, seg := range splitPath(dir) {
		r := c.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": spaceID, "path": parent, "name": seg})
		if r.Status != 201 && r.Status != 409 {
			v.t.Fatalf("mkdir %s/%s: %d", parent, seg, r.Status)
		}
		if parent == "" {
			parent = seg
		} else {
			parent += "/" + seg
		}
	}
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// media fetches GET /media/{id} for c.
func (v *env) media(c *authtest.Client, id string) map[string]any {
	v.t.Helper()
	return c.Do("GET", "/api/v1/media/"+id, nil).Expect(200).Map()
}

func info(m map[string]any) map[string]any { return m["media"].(map[string]any) }

// waitProbed waits until an item's probe finished.
func (v *env) waitProbed(c *authtest.Client, id string) map[string]any {
	v.t.Helper()
	var m map[string]any
	eventually(v.t, "probe of "+id, func() bool {
		r := c.Do("GET", "/api/v1/media/"+id, nil)
		if r.Status != 200 {
			return false
		}
		m = r.Map()
		return info(m)["probe_status"] != "pending"
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
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pngImage encodes a w×h PNG.
func pngImage(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
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

func titles(page map[string]any) []string {
	var out []string
	for _, it := range items(page) {
		out = append(out, info(it)["title"].(string))
	}
	return out
}
