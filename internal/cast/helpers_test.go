package cast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
	"ludworks/internal/media"
	"ludworks/internal/media/naming"
	"ludworks/internal/music"
	"ludworks/internal/tv"
)

const userPW = "user password 1234"

func bg() context.Context { return context.Background() }

// migrationsOnly mounts a module's schema without its routes or background work.
type migrationsOnly struct{ m httpx.Module }

func (w migrationsOnly) Name() string                { return w.m.Name() }
func (w migrationsOnly) Migrations() fs.FS           { return w.m.Migrations() }
func (w migrationsOnly) Routes(*httpx.Router)        {}
func (w migrationsOnly) Start(context.Context) error { return nil }
func (w migrationsOnly) OpenAPI() []byte             { return nil }

/* ------------------------------------------------------------ fake devices */

// fakeRenderer is a scriptable renderer.Renderer (+ Addresser).
type fakeRenderer struct {
	mu      sync.Mutex
	st      renderer.Status
	loads   []*renderer.LoadRequest
	cmds    []string
	changed chan struct{}
	closed  bool
	loadErr error
	ctlErr  error
	remote  net.Addr
	local   net.Addr
}

func newFakeRenderer(remote string) *fakeRenderer {
	ra, _ := net.ResolveTCPAddr("tcp", remote)
	la, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:40000")
	return &fakeRenderer{st: renderer.Status{State: renderer.StateIdle, Volume: 0.5}, changed: make(chan struct{}, 1), remote: ra, local: la}
}

func (r *fakeRenderer) notify() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *fakeRenderer) record(c string) {
	r.cmds = append(r.cmds, c)
}

func (r *fakeRenderer) Load(ctx context.Context, req *renderer.LoadRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("closed")
	}
	r.loads = append(r.loads, req)
	r.record("load")
	if r.loadErr != nil {
		r.st = renderer.Status{State: renderer.StateError, Err: r.loadErr, Volume: r.st.Volume}
		r.notify()
		return r.loadErr
	}
	r.st = renderer.Status{State: renderer.StatePlaying, Position: req.StartSeconds, Duration: req.DurationSeconds, Volume: r.st.Volume, Muted: r.st.Muted}
	r.notify()
	return nil
}

func (r *fakeRenderer) set(fn func(st *renderer.Status), cmd string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(cmd)
	if r.ctlErr != nil {
		return r.ctlErr
	}
	fn(&r.st)
	r.notify()
	return nil
}

func (r *fakeRenderer) Play(context.Context) error {
	return r.set(func(st *renderer.Status) { st.State = renderer.StatePlaying }, "play")
}
func (r *fakeRenderer) Pause(context.Context) error {
	return r.set(func(st *renderer.Status) { st.State = renderer.StatePaused }, "pause")
}
func (r *fakeRenderer) Seek(_ context.Context, s float64) error {
	return r.set(func(st *renderer.Status) { st.Position = s }, fmt.Sprintf("seek %g", s))
}
func (r *fakeRenderer) Stop(context.Context) error {
	return r.set(func(st *renderer.Status) { st.State = renderer.StateIdle }, "stop")
}
func (r *fakeRenderer) SetVolume(_ context.Context, v float64) error {
	return r.set(func(st *renderer.Status) { st.Volume = v }, fmt.Sprintf("volume %g", v))
}
func (r *fakeRenderer) SetMuted(_ context.Context, m bool) error {
	return r.set(func(st *renderer.Status) { st.Muted = m }, fmt.Sprintf("muted %v", m))
}
func (r *fakeRenderer) Status() renderer.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}
func (r *fakeRenderer) Changed() <-chan struct{} { return r.changed }
func (r *fakeRenderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.record("close")
		close(r.changed)
	}
	return nil
}
func (r *fakeRenderer) LocalAddr() net.Addr  { return r.local }
func (r *fakeRenderer) RemoteAddr() net.Addr { return r.remote }

// lose simulates a connection lost for good.
func (r *fakeRenderer) lose() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.st.Err = errors.New("lost connection")
		close(r.changed)
	}
}

// update changes the status as the device would report it.
func (r *fakeRenderer) update(fn func(st *renderer.Status)) {
	r.mu.Lock()
	fn(&r.st)
	r.notify()
	r.mu.Unlock()
}

func (r *fakeRenderer) lastLoad() *renderer.LoadRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.loads) == 0 {
		return nil
	}
	return r.loads[len(r.loads)-1]
}

func (r *fakeRenderer) loadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.loads)
}

func (r *fakeRenderer) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// fakeDriver connects fake renderers; with pin set it also implements PIN pairing.
type fakeDriver struct {
	kind string
	caps renderer.Capabilities

	mu         sync.Mutex
	connectErr error
	remote     string
	rends      []*fakeRenderer
	creds      [][]byte
}

func (d *fakeDriver) Kind() string { return d.kind }
func (d *fakeDriver) Describe(*renderer.Device) renderer.Capabilities {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.caps
}
func (d *fakeDriver) Connect(ctx context.Context, dev *renderer.Device, creds []byte) (renderer.Renderer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.creds = append(d.creds, creds)
	if d.connectErr != nil {
		return nil, d.connectErr
	}
	remote := d.remote
	if remote == "" {
		remote = "192.0.2.10:8009"
	}
	r := newFakeRenderer(remote)
	d.rends = append(d.rends, r)
	return r, nil
}

func (d *fakeDriver) last() *fakeRenderer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.rends) == 0 {
		return nil
	}
	return d.rends[len(d.rends)-1]
}

// pairingDriver adds renderer.Pairer to fakeDriver (the AirPlay side).
type pairingDriver struct {
	*fakeDriver
	pin     string
	started int
}

func (d *pairingDriver) StartPairing(ctx context.Context, dev *renderer.Device) (renderer.Pairing, error) {
	d.mu.Lock()
	d.started++
	d.mu.Unlock()
	return &fakePairing{pin: d.pin}, nil
}

type fakePairing struct {
	pin    string
	closed bool
}

func (p *fakePairing) Confirm(ctx context.Context, pin string) ([]byte, error) {
	if pin != p.pin {
		return nil, renderer.ErrWrongPIN
	}
	return []byte("SECRET-PAIRING-CREDS"), nil
}
func (p *fakePairing) Close() error { p.closed = true; return nil }

// fakeBrowser stands in for the mDNS browser.
type fakeBrowser struct {
	mu        sync.Mutex
	insts     []mdns.Instance
	started   bool
	closed    bool
	refreshes int
}

func (b *fakeBrowser) Start(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = true
	return nil
}
func (b *fakeBrowser) Refresh(context.Context) {
	b.mu.Lock()
	b.refreshes++
	b.mu.Unlock()
}
func (b *fakeBrowser) Instances() []mdns.Instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mdns.Instance(nil), b.insts...)
}
func (b *fakeBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

/* ------------------------------------------------------------ fake music */

type playRec struct {
	ID        string
	Seconds   float64
	Completed bool
}

// fakeMusic is the music service (its real implementation is another module's work).
type fakeMusic struct {
	mu     sync.Mutex
	tracks map[string]*music.TrackInfo
	owner  map[string]string // track id → the only user who can read it
	plays  []playRec
	served []string
}

func (f *fakeMusic) check(p *core.Principal, id string) (*music.TrackInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tracks[id]
	if t == nil || p == nil || f.owner[id] != p.UserID {
		return nil, core.NotFound("Track not found")
	}
	c := *t
	return &c, nil
}
func (f *fakeMusic) TrackInfo(ctx context.Context, p *core.Principal, id string) (*music.TrackInfo, error) {
	return f.check(p, id)
}
func (f *fakeMusic) serve(w http.ResponseWriter, p *core.Principal, id, what string) error {
	if _, err := f.check(p, id); err != nil {
		return err
	}
	f.mu.Lock()
	f.served = append(f.served, what+" "+id)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "audio/mpeg")
	_, err := io.WriteString(w, strings.ToUpper(what)+":"+id)
	return err
}
func (f *fakeMusic) ServeStream(w http.ResponseWriter, r *http.Request, p *core.Principal, id string) error {
	return f.serve(w, p, id, "stream")
}
func (f *fakeMusic) ServeTranscode(w http.ResponseWriter, r *http.Request, p *core.Principal, id, format string, kbps int, start float64) error {
	return f.serve(w, p, id, fmt.Sprintf("transcode-%s-%g", format, start))
}
func (f *fakeMusic) ServeArt(w http.ResponseWriter, r *http.Request, p *core.Principal, id, size string) error {
	return f.serve(w, p, id, "art-"+size)
}
func (f *fakeMusic) ArtJPEG(ctx context.Context, p *core.Principal, id, size string) ([]byte, error) {
	if _, err := f.check(p, id); err != nil {
		return nil, err
	}
	return []byte("JPEG"), nil
}
func (f *fakeMusic) RecordPlay(ctx context.Context, p *core.Principal, id string, at time.Time, secs float64, completed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plays = append(f.plays, playRec{ID: id, Seconds: secs, Completed: completed})
	return nil
}
func (f *fakeMusic) recorded() []playRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]playRec(nil), f.plays...)
}

/* ------------------------------------------------------------ environment */

type env struct {
	t       *testing.T
	e       *authtest.Env
	m       *Module
	files   *files.Service
	tv      *tv.Module
	music   *fakeMusic
	cc      *fakeDriver
	ap      *pairingDriver
	br      *fakeBrowser
	admin   *authtest.Client // cookie session (CSRF)
	adm     *authtest.Client // bearer session
	adminID string

	clockMu sync.Mutex
	clock   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv; configure (when not nil) adjusts the module before it starts.
func newEnvWith(t *testing.T, configure func(m *Module)) *env {
	t.Helper()
	var (
		cm  *Module
		fm  *files.Module
		tvm *tv.Module
	)
	v := &env{t: t, music: &fakeMusic{tracks: map[string]*music.TrackInfo{}, owner: map[string]string{}},
		cc:    &fakeDriver{kind: renderer.KindChromecast, caps: renderer.Capabilities{Audio: true, Video: true}},
		br:    &fakeBrowser{},
		clock: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	v.ap = &pairingDriver{fakeDriver: &fakeDriver{kind: renderer.KindAirPlay, caps: renderer.Capabilities{Audio: true,
		Video: true, RequiresPairing: true, PairingSupported: true}}, pin: "1234"}
	v.e = authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		ml := mail.New(e.Deps)
		fm = files.New(e.Deps)
		med := media.New(e.Deps, fm.Service())
		tvm = tv.New(e.Deps, fm.Service(), med.Service())
		cm = New(e.Deps, fm.Service(), med.Service(), tvm, nil)
		cm.music = v.music
		cm.SetDrivers(v.cc, v.ap)
		cm.newBrowser = func(mdns.Options) browser { return v.br }
		cm.now = v.now
		cm.connectWait, cm.loadTimeout, cm.commandWait, cm.replaceWait = 2*time.Second, 2*time.Second, 3*time.Second, 2*time.Second
		if configure != nil {
			configure(cm)
		}
		return []httpx.Module{ml, admin.New(e.Deps, e.Auth, ml), fm, migrationsOnly{med}, tvm, cm}
	}})
	t.Cleanup(cm.Stop)
	v.m, v.files, v.tv = cm, fm.Service(), tvm
	v.admin = v.e.Setup()
	v.adm = v.e.TokenLogin(authtest.AdminUser, authtest.AdminPassword)
	var me map[string]any
	v.adm.Do("GET", "/api/v1/me", nil).Expect(200).JSON(&me)
	v.adminID = me["id"].(string)
	return v
}

func (v *env) now() time.Time {
	v.clockMu.Lock()
	defer v.clockMu.Unlock()
	return v.clock
}

func (v *env) advance(d time.Duration) {
	v.clockMu.Lock()
	v.clock = v.clock.Add(d)
	v.clockMu.Unlock()
}

// enable turns casting on with the given extra settings and waits until it applied.
func (v *env) enable(extra map[string]any) {
	v.t.Helper()
	s := map[string]any{"enabled": true}
	for k, val := range extra {
		s[k] = val
	}
	v.e.Patch("cast", s)
	v.waitFor("casting enabled", func() bool {
		v.m.mu.Lock()
		defer v.m.mu.Unlock()
		return v.m.enabled && v.m.cfg.Enabled
	})
}

func (v *env) waitFor(what string, cond func() bool) {
	v.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			v.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// user creates an account and returns its id and a bearer client.
func (v *env) user(name string) (string, *authtest.Client) {
	v.t.Helper()
	body := map[string]any{"username": name, "email": name + "@example.com", "password": userPW, "role": "user"}
	u := v.admin.Do("POST", "/api/v1/admin/users", body).Expect(201).Map()
	return u["id"].(string), v.e.TokenLogin(name, userPW)
}

// token mints a personal access token with scopes for the user behind c.
func (v *env) token(c *authtest.Client, scopes ...string) *authtest.Client {
	v.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	c.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "t", "scopes": scopes}).Expect(201).JSON(&out)
	return v.e.WithBearer(out.Token)
}

func (v *env) principal(userID string) *core.Principal {
	v.t.Helper()
	u, err := v.e.Deps.Users.GetUser(bg(), userID)
	if err != nil {
		v.t.Fatal(err)
	}
	return &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Scopes: []string{"*"}, Kind: core.KindSession}
}

// mediaRow describes a fixture's media_items row.
type mediaRow struct {
	Title, ShowTitle, EpisodeTitle string
	Season, Episode                int
	Duration                       float64
	Container, Video, Audio        string
	Playable                       bool
}

// addMedia creates a movie file in the user's home with a media row; it returns the id.
func (v *env) addMedia(userID, name string, m mediaRow) string {
	v.t.Helper()
	ctx := bg()
	p := v.principal(userID)
	space, err := v.files.HomeSpaceID(ctx, userID)
	if err != nil {
		v.t.Fatal(err)
	}
	cat := "movie"
	if m.ShowTitle != "" {
		cat = "tv"
	}
	e, err := v.files.CreateFromReader(ctx, p, space, "", name, strings.NewReader("fake media bytes of "+name), files.CreateOpts{Category: cat})
	if err != nil {
		v.t.Fatal(err)
	}
	var season, episode any
	if m.Season > 0 {
		season = m.Season
	}
	if m.Episode > 0 {
		episode = m.Episode
	}
	var dur any
	if m.Duration > 0 {
		dur = m.Duration
	}
	now := db.FormatTime(time.Now())
	_, err = v.e.Deps.DB.SQL.Exec(`INSERT INTO media_items (file_id, title, sort_title, show_title, show_key, season, episode,
		episode_title, duration_seconds, video_codec, audio_codec, container, browser_playable, added_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET title = excluded.title, show_title = excluded.show_title, show_key = excluded.show_key,
		season = excluded.season, episode = excluded.episode, episode_title = excluded.episode_title,
		duration_seconds = excluded.duration_seconds, video_codec = excluded.video_codec, audio_codec = excluded.audio_codec,
		container = excluded.container, browser_playable = excluded.browser_playable`,
		e.ID, m.Title, strings.ToLower(m.Title), m.ShowTitle, showKey(m.ShowTitle), season, episode, m.EpisodeTitle,
		dur, m.Video, m.Audio, m.Container, db.Bool(m.Playable), now, now)
	if err != nil {
		v.t.Fatal(err)
	}
	return e.ID
}

// addTrack registers a fake track readable by userID.
func (v *env) addTrack(userID, id string, info music.TrackInfo) {
	info.ID = id
	v.music.mu.Lock()
	v.music.tracks[id] = &info
	v.music.owner[id] = userID
	v.music.mu.Unlock()
}

// chromecastInstance is a discovered Chromecast at addr.
func chromecastInstance(id, name, addr string) mdns.Instance {
	return mdns.Instance{Service: renderer.ServiceGoogleCast, Name: "Chromecast-" + id, Host: id + ".local", Port: 8009,
		Addrs: []netip.Addr{netip.MustParseAddr(addr)}, TXT: map[string]string{"id": id, "fn": name, "md": "Chromecast", "ca": "4101"},
		Seen: time.Now()}
}

func (v *env) setInstances(insts ...mdns.Instance) {
	v.br.mu.Lock()
	v.br.insts = insts
	v.br.mu.Unlock()
}

// sessionJSON decodes a session response.
type sessionResp struct {
	ID       string        `json:"id"`
	Device   deviceJSON    `json:"device"`
	OwnerID  string        `json:"owner_id"`
	State    string        `json:"state"`
	Error    *sessionError `json:"error"`
	Items    []itemJSON    `json:"items"`
	Index    int           `json:"index"`
	Position float64       `json:"position_seconds"`
	Duration *float64      `json:"duration_seconds"`
	Volume   float64       `json:"volume"`
	Muted    bool          `json:"muted"`
	Repeat   string        `json:"repeat"`
	Shuffle  bool          `json:"shuffle"`
	Version  int64         `json:"version"`
}

func decodeSession(t *testing.T, r *authtest.Response) sessionResp {
	t.Helper()
	var s sessionResp
	r.JSON(&s)
	return s
}

// session fetches a session.
func (v *env) session(c *authtest.Client, id string) sessionResp {
	v.t.Helper()
	return decodeSession(v.t, c.Do("GET", "/api/v1/cast/sessions/"+id, nil).Expect(200))
}

// waitState polls a session until cond holds.
func (v *env) waitSession(c *authtest.Client, id string, what string, cond func(s sessionResp) bool) sessionResp {
	v.t.Helper()
	var s sessionResp
	v.waitFor(what, func() bool {
		s = v.session(c, id)
		return cond(s)
	})
	return s
}

// start creates a session and waits until the device loaded the first item.
func (v *env) start(c *authtest.Client, deviceID string, items ...map[string]any) sessionResp {
	v.t.Helper()
	body := map[string]any{"device_id": deviceID, "items": items}
	s := decodeSession(v.t, c.Do("POST", "/api/v1/cast/sessions", body).Expect(201))
	return v.waitSession(c, s.ID, "first item playing", func(s sessionResp) bool { return s.State == StatePlaying })
}

func item(kind, id string) map[string]any { return map[string]any{"kind": kind, "id": id} }

// ticketURL splits a device URL into its path (with query) relative to the media base.
func ticketPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

// fetchFrom sends a GET through the real router as if from remote (no network).
func (v *env) fetchFrom(remote, path string, hdr map[string]string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remote
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	v.e.Router.ServeHTTP(rec, req)
	return rec
}

// rawGet sends a real HTTP GET to the test server.
func rawGet(t *testing.T, u string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// httpRequest builds a request for serve.
func httpRequest(method, path string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	return req
}

// serve runs req through the real router as if from remote.
func (v *env) serve(remote string, req *http.Request) *httptest.ResponseRecorder {
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	v.e.Router.ServeHTTP(rec, req)
	return rec
}

func mustAddrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func showKey(show string) string {
	if show == "" {
		return ""
	}
	return naming.ShowKey(show)
}

// sess returns the module's session by id.
func (v *env) sess(id string) *session {
	v.t.Helper()
	v.m.mu.Lock()
	defer v.m.mu.Unlock()
	s := v.m.sessions[id]
	if s == nil {
		v.t.Fatalf("no session %s", id)
	}
	return s
}
