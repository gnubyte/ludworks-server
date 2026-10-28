// Package cast implements casting (API.md §14): discovery of Chromecast and AirPlay devices
// (mDNS and admin-configured manual devices), cast sessions with a server-side queue, the
// media tickets devices fetch media with, and the optional plain-HTTP cast listener. Device
// protocols live in subpackages behind the internal/cast/renderer contract
// (internal/cast/chromecast, internal/cast/airplay); discovery in internal/cast/mdns.
//
// Casting is off until an administrator sets cast.enabled. While it is off the module opens
// no sockets and keeps only one idle goroutine; turning it on starts discovery and (with
// cast.media_port) the cast listener, turning it off ends every session and closes them.
//
// A session belongs to the user who started it and plays on one device. Its goroutine owns
// the device connection: it loads queue items, mirrors the device's state, advances the
// queue, follows TV schedules and records the owner's progress as if they played locally.
package cast

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"ludworks/internal/cast/airplay"
	"ludworks/internal/cast/chromecast"
	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/media"
	"ludworks/internal/music"
	"ludworks/internal/settings"
	"ludworks/internal/tv"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// The services the module uses, as interfaces so tests can substitute them. Every method
// applies the given principal's permissions.
type (
	filesService interface {
		LocalPath(ctx context.Context, p *core.Principal, id string) (string, error)
	}
	mediaService interface {
		PlaybackInfo(ctx context.Context, p *core.Principal, fileID string) (*media.PlaybackInfo, error)
		RecordProgress(ctx context.Context, p *core.Principal, fileID string, positionSeconds float64, completed *bool) error
		PosterJPEG(ctx context.Context, p *core.Principal, fileID, size string) ([]byte, error)
		ServeStream(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string) error
		ServeTranscode(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, startSeconds float64, height int) error
		ServePoster(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID, size string) error
		ServeSubtitle(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, index int) error
		FFmpegPath(ctx context.Context) string
	}
	musicService interface {
		TrackInfo(ctx context.Context, p *core.Principal, id string) (*music.TrackInfo, error)
		ServeStream(w http.ResponseWriter, r *http.Request, p *core.Principal, id string) error
		ServeTranscode(w http.ResponseWriter, r *http.Request, p *core.Principal, id, format string, bitrateKbps int, startSeconds float64) error
		ServeArt(w http.ResponseWriter, r *http.Request, p *core.Principal, id, size string) error
		ArtJPEG(ctx context.Context, p *core.Principal, id, size string) ([]byte, error)
		RecordPlay(ctx context.Context, p *core.Principal, id string, playedAt time.Time, secondsPlayed float64, completed bool) error
	}
	tvService interface {
		CastChannel(ctx context.Context, viewer *core.Principal, id string) (*tv.CastChannel, error)
		CastProgramAt(ctx context.Context, viewer *core.Principal, id string, at time.Time) (*tv.CastProgram, error)
		CastProgramByIndex(ctx context.Context, viewer *core.Principal, id string, index int64, offset float64) (*tv.CastProgram, error)
		CastSavePosition(ctx context.Context, viewer *core.Principal, id string, index int64, offset float64) error
		ServeCastProgram(w http.ResponseWriter, r *http.Request, viewer *core.Principal, id, fileID string, a tv.CastAsset) error
	}
	// browser is the mDNS browser (mdns.Browser; tests substitute a fake).
	browser interface {
		Start(ctx context.Context) error
		Refresh(ctx context.Context)
		Instances() []mdns.Instance
		Close() error
	}
)

// Discovery timing.
const (
	refreshWait       = 3 * time.Second // POST /cast/devices/refresh
	refreshEvery      = 5 * time.Second // refreshes closer together share the previous results
	browserRetryAfter = 5 * time.Minute // a browser that could not start is retried
	sweepEvery        = 30 * time.Second
)

// Module is the cast module.
type Module struct {
	d     *core.Deps
	log   *slog.Logger
	files filesService
	media mediaService
	tv    tvService
	music musicService

	drivers    map[string]renderer.Driver
	newBrowser func(mdns.Options) browser
	now        func() time.Time

	// Timing (tests shorten these).
	idleTimeout, ticketGrace, endedKeep                time.Duration
	connectWait, loadTimeout, commandWait, replaceWait time.Duration
	progressEvery                                      time.Duration

	runCtx   context.Context
	stopRun  context.CancelFunc
	applyCh  chan struct{}
	loopDone chan struct{}

	mu            sync.Mutex
	started       bool
	enabled       bool
	cfg           Settings
	manual        []*deviceEntry
	browser       browser
	browserFailed time.Time
	lastRefresh   time.Time
	refreshMu     sync.Mutex // one refresh at a time
	listener      *castListener
	listenerErr   error
	unreachable   map[string]bool
	sessions      map[string]*session   // live and recently ended sessions by id
	byDevice      map[string]*session   // live sessions by device id
	byTicket      map[[32]byte]*session // sessions by ticket hash (until the ticket expires)
	pairings      map[string]*pendingPairing
	devLocks      map[string]*sync.Mutex
}

// New constructs the module, registers the "cast" settings namespace and the device drivers.
func New(d *core.Deps, f *files.Service, m *media.Service, t *tv.Module, mu *music.Service) *Module {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mod := &Module{
		d: d, log: log.With("module", "cast"), now: core.Now,
		idleTimeout: defaultIdleTimeout, ticketGrace: defaultTicketGrace, endedKeep: defaultEndedKeep,
		connectWait: defaultConnectWait, loadTimeout: defaultLoadTimeout, commandWait: defaultCommandWait,
		replaceWait: defaultReplaceWait, progressEvery: defaultProgressEvery,
		applyCh: make(chan struct{}, 1), loopDone: make(chan struct{}),
		unreachable: map[string]bool{}, sessions: map[string]*session{}, byDevice: map[string]*session{},
		byTicket: map[[32]byte]*session{}, pairings: map[string]*pendingPairing{}, devLocks: map[string]*sync.Mutex{},
	}
	// Typed nils must not become non-nil interfaces.
	if f != nil {
		mod.files = f
	}
	if m != nil {
		mod.media = m
	}
	if t != nil {
		mod.tv = t
	}
	if mu != nil {
		mod.music = mu
	}
	var reserved map[int]bool
	if d.Config != nil {
		reserved = listenPorts(d.Config.Listen, d.Config.TLS.RedirectListen)
	}
	d.Settings.Register("cast", DefaultSettings, nil, settings.Validator(func(s *Settings) error {
		return validateSettings(s, reserved)
	}))
	name := "Ludworks"
	if g, err := d.Settings.General(context.Background()); err == nil && strings.TrimSpace(g.ServerName) != "" {
		name = g.ServerName
	}
	mod.SetDrivers(
		chromecast.New(chromecast.Options{Log: mod.log}),
		airplay.New(airplay.Options{Log: mod.log, Name: name}),
	)
	mod.newBrowser = func(o mdns.Options) browser { return mdns.New(o) }
	return mod
}

// SetDrivers replaces the device drivers (one per kind). Call before Start.
func (m *Module) SetDrivers(drivers ...renderer.Driver) {
	m.drivers = map[string]renderer.Driver{}
	for _, d := range drivers {
		m.drivers[d.Kind()] = d
	}
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "cast" }

// Migrations implements httpx.Module.
func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// OpenAPI implements httpx.Module.
func (m *Module) OpenAPI() []byte { return openAPI }

// Routes implements httpx.Module.
func (m *Module) Routes(r *httpx.Router) {
	c := httpx.Scope("cast")
	r.Handle("GET /api/v1/cast/devices", c, m.listDevices)
	r.Handle("POST /api/v1/cast/devices/refresh", c, m.refreshDevices)
	r.Handle("POST /api/v1/cast/devices/{id}/pair", c, m.startPairing)
	r.Handle("POST /api/v1/cast/devices/{id}/pair/confirm", c, m.confirmPairing)
	r.Handle("DELETE /api/v1/cast/devices/{id}/pairing", httpx.AdminOnly(), m.forgetPairing)
	r.Handle("GET /api/v1/cast/sessions", c, m.listSessions)
	r.Handle("POST /api/v1/cast/sessions", c, m.createSession)
	r.Handle("GET /api/v1/cast/sessions/{id}", c, m.getSession)
	r.Handle("POST /api/v1/cast/sessions/{id}/control", c, m.controlSession)
	r.Handle("PUT /api/v1/cast/sessions/{id}/items", c, m.replaceItems)
	r.Handle("POST /api/v1/cast/sessions/{id}/items", c, m.insertItems)
	r.Handle("DELETE /api/v1/cast/sessions/{id}", c, m.deleteSession)
	// What cast devices fetch: public, authorised by the ticket in the path (no cookies or
	// tokens). The same handler is all the cast listener serves.
	r.Handle("GET "+mediaRoute, httpx.Public(), m.serveMainMedia)
}

// Start implements httpx.Module: it applies the settings now and whenever they change, and
// ends a deleted user's sessions. Everything stops when ctx ends.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("cast: already started")
	}
	m.started = true
	m.runCtx, m.stopRun = context.WithCancel(ctx)
	m.mu.Unlock()
	m.d.Settings.OnChange(func(ns string) {
		if ns == "cast" {
			m.kick()
		}
	})
	m.d.Events.Subscribe(events.UserDeleted, func(payload any) {
		if id, ok := payload.(string); ok {
			m.endUserSessions(id, codeAccessRevoked, "The owner's account was deleted")
		}
	})
	go m.loop()
	m.kick()
	return nil
}

// Stop ends every session, closes discovery and the cast listener, and waits for the module's
// goroutine (tests; the app stops the module by cancelling Start's context).
func (m *Module) Stop() {
	m.mu.Lock()
	stop := m.stopRun
	m.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-m.loopDone
}

// kick schedules applying the settings.
func (m *Module) kick() {
	select {
	case m.applyCh <- struct{}{}:
	default:
	}
}

// loop applies settings changes and sweeps expired state until the module stops.
func (m *Module) loop() {
	defer close(m.loopDone)
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-m.runCtx.Done():
			m.shutdown()
			return
		case <-m.applyCh:
			m.apply(m.runCtx)
		case <-t.C:
			m.sweep()
		}
	}
}

// apply makes the running state match the settings.
func (m *Module) apply(ctx context.Context) {
	st := m.settings(ctx)
	if !st.Enabled {
		m.disable(codeDisabled, "Casting was turned off by an administrator")
		m.mu.Lock()
		m.cfg = st
		m.mu.Unlock()
		return
	}
	manual := make([]*deviceEntry, 0, len(st.ManualDevices))
	for _, d := range st.ManualDevices {
		manual = append(manual, manualEntry(d))
	}
	m.mu.Lock()
	if !m.enabled {
		m.log.Info("cast: casting enabled")
	}
	m.enabled, m.manual, m.cfg = true, manual, st
	br, ln := m.browser, m.listener
	m.mu.Unlock()

	switch {
	case st.Discovery && br == nil:
		m.startBrowser()
	case !st.Discovery && br != nil:
		m.mu.Lock()
		m.browser = nil
		m.mu.Unlock()
		_ = br.Close()
	}

	if ln != nil && ln.port != st.MediaPort {
		m.mu.Lock()
		m.listener = nil
		m.mu.Unlock()
		ln.close()
		ln = nil
	}
	if ln == nil && st.MediaPort > 0 {
		l, err := m.startListener(m.runCtx, st.MediaPort)
		if err != nil {
			m.log.Error("cast: cannot open the media listener port", "port", st.MediaPort, "err", err)
		}
		m.mu.Lock()
		m.listener, m.listenerErr = l, err
		m.mu.Unlock()
	}
}

// disable ends every session with code and closes discovery and the listener.
func (m *Module) disable(code, msg string) {
	m.mu.Lock()
	wasEnabled := m.enabled
	m.enabled = false
	br, ln := m.browser, m.listener
	m.browser, m.listener, m.manual = nil, nil, nil
	m.unreachable = map[string]bool{}
	var live []*session
	for _, s := range m.byDevice {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.end(code, msg)
	}
	waitSessions(live, 6*time.Second)
	if br != nil {
		_ = br.Close()
	}
	if ln != nil {
		ln.close()
	}
	m.sweepPairings(true)
	if wasEnabled {
		m.log.Info("cast: casting disabled")
	}
}

// shutdown runs when the module stops.
func (m *Module) shutdown() {
	m.disable("", "")
}

// waitSessions waits (bounded) for session loops to exit.
func waitSessions(ss []*session, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for _, s := range ss {
		select {
		case <-s.done:
		case <-t.C:
			return
		}
	}
}

// startBrowser starts mDNS discovery (failures are logged and retried later).
func (m *Module) startBrowser() {
	b := m.newBrowser(mdns.Options{
		Services: []string{renderer.ServiceGoogleCast, renderer.ServiceAirPlay, renderer.ServiceRAOP},
		Log:      m.log.With("component", "mdns"),
	})
	if err := b.Start(m.runCtx); err != nil {
		m.log.Warn("cast: device discovery is unavailable (no multicast-capable network interface?)", "err", err)
		m.mu.Lock()
		m.browserFailed = m.now()
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	stillWanted := m.enabled && m.cfg.Discovery && m.browser == nil
	if stillWanted {
		m.browser = b
	}
	m.mu.Unlock()
	if !stillWanted {
		_ = b.Close()
	}
}

// sweep forgets ended sessions and expired pairings, and retries discovery that failed.
func (m *Module) sweep() {
	now := m.now()
	m.mu.Lock()
	for id, s := range m.sessions {
		s.mu.Lock()
		ended := s.endedAt
		s.mu.Unlock()
		if ended.IsZero() {
			continue
		}
		if now.Sub(ended) > m.ticketGrace {
			delete(m.byTicket, s.ticketHash)
		}
		if now.Sub(ended) > max(m.endedKeep, m.ticketGrace) {
			delete(m.sessions, id)
		}
	}
	retry := m.enabled && m.cfg.Discovery && m.browser == nil && now.Sub(m.browserFailed) > browserRetryAfter
	m.mu.Unlock()
	m.sweepPairings(false)
	if retry {
		m.startBrowser()
	}
}

/* ------------------------------------------------------------ registry */

// sessionOnDevice is the live session on a device, if any.
func (m *Module) sessionOnDevice(deviceID string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byDevice[deviceID]
}

// sessionEnded unregisters a finished session from its device (the session stays readable
// for endedKeep and its ticket for ticketGrace).
func (m *Module) sessionEnded(s *session) {
	m.mu.Lock()
	if m.byDevice[s.entry.dev.ID] == s {
		delete(m.byDevice, s.entry.dev.ID)
	}
	m.mu.Unlock()
}

// endUserSessions ends every live session of a user.
func (m *Module) endUserSessions(userID, code, msg string) {
	m.mu.Lock()
	var ss []*session
	for _, s := range m.byDevice {
		if s.owner.UserID == userID {
			ss = append(ss, s)
		}
	}
	m.mu.Unlock()
	for _, s := range ss {
		s.end(code, msg)
	}
}

// lockDevice serialises session creation per device.
func (m *Module) lockDevice(id string) func() {
	m.mu.Lock()
	l := m.devLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.devLocks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// userPrincipal is the principal of a session owner with their current role, or nil when the
// account is disabled or gone. A failed lookup keeps the session going with the role it had.
func (m *Module) userPrincipal(ctx context.Context, o owner) *core.Principal {
	if m.d.Users == nil {
		return nil
	}
	u, err := m.d.Users.GetUser(ctx, o.UserID)
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		m.log.Warn("cast: looking up a session owner failed", "user", o.UserID, "err", err)
		return &core.Principal{UserID: o.UserID, Username: o.Username, Role: o.Role, Scopes: o.Scopes, Kind: core.KindSession}
	}
	if u == nil || u.Disabled {
		return nil
	}
	return &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Scopes: o.Scopes, Kind: core.KindSession}
}

/* ------------------------------------------------------------ media URLs */

var errCastUnavailable = core.NewError(http.StatusServiceUnavailable, codeUnavailable,
	"Cast devices cannot reach this server: an administrator must set cast.media_base_url, cast.media_port or general.public_url")

// mediaBase picks the URL base devices fetch media from (API.md §14 Reachability):
// cast.media_base_url, else the cast listener on this server's address toward the device
// (local: the device connection's local address when known), else general.public_url.
func (m *Module) mediaBase(ctx context.Context, st Settings, addrs []netip.Addr, local netip.Addr) (string, error) {
	if st.MediaBaseURL != "" {
		return st.MediaBaseURL, nil
	}
	m.mu.Lock()
	ln := m.listener
	m.mu.Unlock()
	if st.MediaPort > 0 && ln != nil && ln.port == st.MediaPort {
		ip := local
		if !usableHost(ip) {
			ip = localAddrToward(addrs)
		}
		if usableHost(ip) {
			return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(st.MediaPort)), nil
		}
	}
	if g, err := m.d.Settings.General(ctx); err == nil && g.PublicURL != "" {
		return strings.TrimRight(g.PublicURL, "/"), nil
	}
	return "", errCastUnavailable
}

// usableHost reports whether an address can be put in a URL for a device (link-local IPv6
// would need a zone, which devices do not take).
func usableHost(a netip.Addr) bool {
	return a.IsValid() && !a.IsUnspecified() && !(a.Is6() && a.IsLinkLocalUnicast())
}

// localAddrToward returns this host's address on the route to the first reachable device
// address (a connected UDP socket sends nothing, but the kernel picks the source address).
func localAddrToward(addrs []netip.Addr) netip.Addr {
	for _, a := range addrs {
		if !usableHost(a) {
			continue
		}
		c, err := net.Dial("udp", netip.AddrPortFrom(a, 9).String())
		if err != nil {
			continue
		}
		local := addrIP(c.LocalAddr())
		c.Close()
		if usableHost(local) {
			return local
		}
	}
	return netip.Addr{}
}
