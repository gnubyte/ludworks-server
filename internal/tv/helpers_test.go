package tv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/media"
	"ludworks/internal/media/naming"
)

const userPW = "user password 1234"

// migrationsOnly mounts another module's schema without its routes or background work, so
// fixtures (media_items rows) stay fully under the test's control.
type migrationsOnly struct{ m httpx.Module }

func (w migrationsOnly) Name() string                { return w.m.Name() }
func (w migrationsOnly) Migrations() fs.FS           { return w.m.Migrations() }
func (w migrationsOnly) Routes(*httpx.Router)        {}
func (w migrationsOnly) Start(context.Context) error { return nil }
func (w migrationsOnly) OpenAPI() []byte             { return nil }

// fakeMedia records how the tv module delegates program requests.
type fakeMedia struct {
	mu     sync.Mutex
	calls  []string // "<method> <owner user id> <file id> <extra>"
	ffmpeg string
}

func (f *fakeMedia) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeMedia) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeMedia) ServeStream(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string) error {
	f.record("stream " + owner.UserID + " " + fileID)
	w.Header().Set("Content-Type", "video/mp4")
	_, err := w.Write([]byte("STREAM:" + fileID))
	return err
}

func (f *fakeMedia) ServeTranscode(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, start float64, height int) error {
	f.record(fmt.Sprintf("transcode %s %s %g %d", owner.UserID, fileID, start, height))
	_, err := w.Write([]byte("FMP4"))
	return err
}

func (f *fakeMedia) ServePoster(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID, size string) error {
	f.record("poster " + owner.UserID + " " + fileID + " " + size)
	_, err := w.Write([]byte("JPEG"))
	return err
}

func (f *fakeMedia) ServeSubtitle(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, index int) error {
	f.record(fmt.Sprintf("subtitle %s %s %d", owner.UserID, fileID, index))
	_, err := w.Write([]byte("WEBVTT\n"))
	return err
}

func (f *fakeMedia) FFmpegPath(context.Context) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ffmpeg
}

type env struct {
	t       *testing.T
	e       *authtest.Env
	tv      *Module
	files   *files.Service
	media   *fakeMedia
	admin   *authtest.Client // cookie session with CSRF
	adm     *authtest.Client // bearer session
	adminID string

	clockMu sync.Mutex
	clock   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, false)
}

// newEnvWith is newEnv; with fullMedia the media module also serves its routes (/api/v1/media/…).
func newEnvWith(t *testing.T, fullMedia bool) *env {
	t.Helper()
	var (
		tvm *Module
		fm  *files.Module
	)
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		ml := mail.New(e.Deps)
		fm = files.New(e.Deps)
		med := media.New(e.Deps, fm.Service())
		tvm = New(e.Deps, fm.Service(), med.Service())
		var mm httpx.Module = migrationsOnly{med}
		if fullMedia {
			mm = med
		}
		return []httpx.Module{ml, admin.New(e.Deps, e.Auth, ml), fm, mm, tvm}
	}})
	v := &env{t: t, e: e, tv: tvm, files: fm.Service(), media: &fakeMedia{},
		clock: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	tvm.media = v.media
	tvm.now = v.now
	tvm.ffmpegPath = v.media.FFmpegPath
	v.admin = e.Setup()
	v.adm = e.TokenLogin(authtest.AdminUser, authtest.AdminPassword)
	var me map[string]any
	v.adm.Do("GET", "/api/v1/me", nil).Expect(200).JSON(&me)
	v.adminID = me["id"].(string)
	// The media module's schema may still be a stub: make sure the columns tv reads exist.
	if _, err := e.Deps.DB.SQL.Exec(`CREATE TABLE IF NOT EXISTS media_items (file_id TEXT PRIMARY KEY REFERENCES files (id)
		ON DELETE CASCADE, title TEXT NOT NULL DEFAULT '', sort_title TEXT NOT NULL DEFAULT '', year INTEGER NULL,
		genres TEXT NOT NULL DEFAULT '[]', show_title TEXT NOT NULL DEFAULT '', show_key TEXT NOT NULL DEFAULT '',
		season INTEGER NULL, episode INTEGER NULL, episode_title TEXT NOT NULL DEFAULT '', duration_seconds REAL NULL,
		browser_playable INTEGER NOT NULL DEFAULT 0, poster_path TEXT NULL, subtitles TEXT NOT NULL DEFAULT '[]',
		added_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *env) now() time.Time {
	v.clockMu.Lock()
	defer v.clockMu.Unlock()
	return v.clock
}

// advance moves the fake clock.
func (v *env) advance(d time.Duration) {
	v.clockMu.Lock()
	v.clock = v.clock.Add(d)
	v.clockMu.Unlock()
}

// user creates an account and returns its id and a bearer client.
func (v *env) user(name string) (string, *authtest.Client) {
	v.t.Helper()
	body := map[string]any{"username": name, "email": name + "@example.com", "password": userPW, "role": "user"}
	u := v.admin.Do("POST", "/api/v1/admin/users", body).Expect(201).Map()
	return u["id"].(string), v.e.TokenLogin(name, userPW)
}

// principal builds a session principal for a user id.
func (v *env) principal(userID string) *core.Principal {
	v.t.Helper()
	u, err := v.e.Deps.Users.GetUser(context.Background(), userID)
	if err != nil {
		v.t.Fatal(err)
	}
	return &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Scopes: []string{"*"}, Kind: core.KindSession}
}

// media describes a fixture file's media_items row.
type mediaRow struct {
	Title, ShowTitle, EpisodeTitle string
	Season, Episode                *int
	Duration                       float64 // 0 = NULL (unknown)
	Genres                         []string
	Year                           int
	Playable                       bool
	Category                       string
}

func ip(n int) *int { return &n }

// addFile creates a file in the user's home (dir is created as needed) and, when m is not
// nil, its media_items row. It returns the file id.
func (v *env) addFile(userID, dir, name string, m *mediaRow) string {
	v.t.Helper()
	ctx := context.Background()
	p := v.principal(userID)
	space, err := v.files.HomeSpaceID(ctx, userID)
	if err != nil {
		v.t.Fatal(err)
	}
	return v.addFileIn(p, space, dir, name, m)
}

func (v *env) addFileIn(p *core.Principal, space, dir, name string, m *mediaRow) string {
	v.t.Helper()
	ctx := context.Background()
	if dir != "" {
		if _, err := v.files.EnsureDir(ctx, p, space, dir); err != nil {
			v.t.Fatal(err)
		}
	}
	opts := files.CreateOpts{}
	if m != nil {
		opts.Category = m.Category
	}
	e, err := v.files.CreateFromReader(ctx, p, space, dir, name, strings.NewReader("fake media "+name), opts)
	if err != nil {
		v.t.Fatal(err)
	}
	if m != nil {
		v.setMedia(e.ID, m)
	}
	return e.ID
}

func (v *env) setMedia(fileID string, m *mediaRow) {
	v.t.Helper()
	var dur any
	if m.Duration != 0 {
		dur = m.Duration
	}
	var year any
	if m.Year != 0 {
		year = m.Year
	}
	g, _ := json.Marshal(m.Genres)
	if m.Genres == nil {
		g = []byte("[]")
	}
	key := ""
	if m.ShowTitle != "" {
		key = naming.ShowKey(m.ShowTitle)
	}
	now := db.FormatTime(time.Now())
	_, err := v.e.Deps.DB.SQL.Exec(`INSERT INTO media_items (file_id, title, sort_title, year, genres, show_title, show_key,
		season, episode, episode_title, duration_seconds, browser_playable, added_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET title = excluded.title, duration_seconds = excluded.duration_seconds,
		show_title = excluded.show_title, show_key = excluded.show_key, season = excluded.season, episode = excluded.episode,
		genres = excluded.genres, year = excluded.year`,
		fileID, m.Title, strings.ToLower(m.Title), year, string(g), m.ShowTitle, key, m.Season, m.Episode, m.EpisodeTitle,
		dur, db.Bool(m.Playable), now, now)
	if err != nil {
		v.t.Fatal(err)
	}
	v.tv.cache.invalidateAll()
}

// do sends a JSON request and decodes a 2xx JSON response into out (when not nil).
func do(t *testing.T, c *authtest.Client, method, path string, body any, status int, out any) *authtest.Response {
	t.Helper()
	r := c.Do(method, path, body).Expect(status)
	if out != nil {
		r.JSON(out)
	}
	return r
}

// raw sends a request with a raw body and headers using c's credentials.
func (v *env) raw(c *authtest.Client, method, path string, hdr map[string]string, body []byte) (int, http.Header, []byte) {
	v.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, v.e.URL+path, rd)
	if err != nil {
		v.t.Fatal(err)
	}
	if c != nil {
		if c.Bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.Bearer)
		}
		if c.CSRF != "" && method != "GET" {
			req.Header.Set("X-CSRF-Token", c.CSRF)
		}
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c != nil {
		hc.Jar = c.HTTP.Jar
	}
	resp, err := hc.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}
