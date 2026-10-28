package kanban

import (
	"bytes"
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
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
)

const userPW = "kanban user password 1234"

type env struct {
	t     *testing.T
	e     *authtest.Env
	m     *Module
	fs    *files.Service
	admin *authtest.Client // cookie session of the server admin (with CSRF)
}

// newEnv starts a server with auth, mail, admin, files and kanban mounted.
func newEnv(t *testing.T) *env {
	t.Helper()
	v := &env{t: t}
	v.e = authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		ml := mail.New(e.Deps)
		fm := files.New(e.Deps)
		v.fs = fm.Service()
		v.m = New(e.Deps, fm.Service())
		return []httpx.Module{ml, admin.New(e.Deps, e.Auth, ml), fm, v.m}
	}})
	v.admin = v.e.Setup()
	return v
}

// user creates an account and returns its id and a signed-in cookie client.
func (v *env) user(name string) (string, *authtest.Client) {
	v.t.Helper()
	body := map[string]any{"username": name, "email": name + "@example.com", "password": userPW, "role": "user", "display_name": "User " + name}
	u := v.admin.Do("POST", "/api/v1/admin/users", body).Expect(http.StatusCreated).Map()
	c := v.e.NewClient()
	c.Login(name, userPW).Expect(http.StatusOK)
	return u["id"].(string), c
}

// token returns a bearer client holding a personal access token with the given scopes.
func (v *env) token(c *authtest.Client, scopes ...string) *authtest.Client {
	v.t.Helper()
	r := c.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "t", "scopes": scopes}).Expect(http.StatusCreated).Map()
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

func (r *rawResp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// raw sends a request with arbitrary headers and body using c's credentials.
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
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	if c.CSRF != "" && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", c.CSRF)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &rawResp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

/* ------------------------------------------------------------- API helpers */

func (v *env) board(c *authtest.Client, name string, extra ...map[string]any) Board {
	v.t.Helper()
	body := map[string]any{"name": name}
	for _, x := range extra {
		for k, val := range x {
			body[k] = val
		}
	}
	var b Board
	c.Do("POST", "/api/v1/kanban/boards", body).Expect(http.StatusCreated).JSON(&b)
	return b
}

func (v *env) getBoard(c *authtest.Client, id string, query ...string) Board {
	v.t.Helper()
	q := ""
	if len(query) > 0 {
		q = "?" + query[0]
	}
	var b Board
	c.Do("GET", "/api/v1/kanban/boards/"+id+q, nil).Expect(http.StatusOK).JSON(&b)
	return b
}

func (v *env) card(c *authtest.Client, columnID, title string, extra ...map[string]any) Card {
	v.t.Helper()
	body := map[string]any{"title": title}
	for _, x := range extra {
		for k, val := range x {
			body[k] = val
		}
	}
	var out Card
	c.Do("POST", "/api/v1/kanban/columns/"+columnID+"/cards", body).Expect(http.StatusCreated).JSON(&out)
	return out
}

func (v *env) members(c *authtest.Client, boardID string, members map[string]string) Board {
	v.t.Helper()
	list := []map[string]any{}
	for u, r := range members {
		list = append(list, map[string]any{"user_id": u, "role": r})
	}
	var b Board
	c.Do("PUT", "/api/v1/kanban/boards/"+boardID+"/members", map[string]any{"members": list}).Expect(http.StatusOK).JSON(&b)
	return b
}

// columnTitles returns the titles of a column's cards in position order (from a board).
func columnTitles(b Board, columnID string) []string {
	var out []string
	pos := -1
	for _, c := range b.Cards {
		if c.ColumnID != columnID {
			continue
		}
		if c.Position <= pos {
			panic("cards out of order")
		}
		pos = c.Position
		out = append(out, c.Title)
	}
	return out
}

// upload attaches bytes to a card.
func (v *env) upload(c *authtest.Client, cardID, name string, data []byte) *rawResp {
	v.t.Helper()
	return v.raw(c, "POST", "/api/v1/kanban/cards/"+cardID+"/attachments?filename="+url.QueryEscape(name),
		map[string]string{"Content-Type": "application/octet-stream"}, data)
}

// pngBytes renders a w×h PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// eventually polls cond until it holds or fails the test after 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errDetails(t *testing.T, r *authtest.Response) map[string]any {
	t.Helper()
	var e struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	r.JSON(&e)
	return e.Error.Details
}
