package cast

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

var (
	errDisabled       = core.NewError(http.StatusConflict, codeDisabled, "Casting is turned off; an administrator can enable it in the settings")
	errDeviceNotFound = core.NotFound("Cast device not found")
	errNoSession      = core.NotFound("Cast session not found")
	errSessionLimit   = core.NewError(http.StatusConflict, "session_limit", fmt.Sprintf("You can cast to at most %d devices at once; stop one first", maxSessionsPerUser))
)

/* ------------------------------------------------------------------ devices */

// devicesResponse is GET /cast/devices.
type devicesResponse struct {
	Enabled bool         `json:"enabled"`
	Devices []deviceJSON `json:"devices"`
}

func (m *Module) devicesJSON(ctx context.Context, p *core.Principal) devicesResponse {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	out := devicesResponse{Enabled: enabled, Devices: []deviceJSON{}}
	if !enabled {
		return out
	}
	paired := m.pairedIDs(ctx)
	for _, e := range m.devices() {
		out.Devices = append(out.Devices, m.deviceJSONFor(e, p, paired))
	}
	slices.SortFunc(out.Devices, func(a, b deviceJSON) int {
		if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// GET /cast/devices
func (m *Module) listDevices(w http.ResponseWriter, r *http.Request) error {
	return httpx.WriteJSON(w, http.StatusOK, m.devicesJSON(r.Context(), core.MustPrincipal(r.Context())))
}

// POST /cast/devices/refresh: browse now (and check the manual devices), at most refreshWait.
func (m *Module) refreshDevices(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	m.mu.Lock()
	enabled, wantBrowser, br := m.enabled, m.cfg.Discovery, m.browser
	m.mu.Unlock()
	// One refresh at a time, at most every refreshEvery server-wide: a refresh sends mDNS queries
	// and connects to every manual device, which a client must not be able to turn into a flood.
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	fresh := m.now().Sub(m.lastRefresh) < refreshEvery
	m.mu.Unlock()
	if enabled && !fresh {
		if wantBrowser && br == nil {
			m.startBrowser()
			m.mu.Lock()
			br = m.browser
			m.mu.Unlock()
		}
		rctx, cancel := context.WithTimeout(ctx, refreshWait)
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.probeManual(rctx)
		}()
		if br != nil {
			br.Refresh(rctx)
		}
		<-done
		cancel()
		m.mu.Lock()
		m.lastRefresh = m.now()
		m.mu.Unlock()
	}
	return httpx.WriteJSON(w, http.StatusOK, m.devicesJSON(ctx, core.MustPrincipal(ctx)))
}

// deviceFromPath resolves the {id} device (404 while casting is off).
func (m *Module) deviceFromPath(r *http.Request) (*deviceEntry, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, errDeviceNotFound
	}
	e := m.device(id)
	if e == nil {
		return nil, errDeviceNotFound
	}
	return e, nil
}

/* ------------------------------------------------------------------ sessions */

// createRequest is the POST /cast/sessions body.
type createRequest struct {
	DeviceID        string    `json:"device_id"`
	Items           []itemRef `json:"items"`
	Index           *int      `json:"index"`
	PositionSeconds *float64  `json:"position_seconds"`
	Repeat          *string   `json:"repeat"`
	Shuffle         *bool     `json:"shuffle"`
	Volume          *float64  `json:"volume"`
}

func (c *createRequest) validate() error {
	fe := map[string]string{}
	if !httpx.ValidID(c.DeviceID) {
		fe["device_id"] = "is required"
	}
	if err := checkRefs("items", c.Items, 1); err != nil {
		return err
	}
	if c.Index != nil && (*c.Index < 0 || *c.Index >= len(c.Items)) {
		fe["index"] = "must be the index of one of the items"
	}
	if c.PositionSeconds != nil && !validSeconds(*c.PositionSeconds) {
		fe["position_seconds"] = "must be a number of seconds of 0 or more"
	}
	if c.Repeat != nil && *c.Repeat != RepeatOff && *c.Repeat != RepeatAll && *c.Repeat != RepeatOne {
		fe["repeat"] = "must be off, all or one"
	}
	if c.Volume != nil && (math.IsNaN(*c.Volume) || *c.Volume < 0 || *c.Volume > 1) {
		fe["volume"] = "must be between 0 and 1"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	return nil
}

// checkCapabilities refuses items the device cannot play (422 unsupported_media).
func checkCapabilities(caps renderer.Capabilities, items []*castItem) error {
	for _, it := range items {
		if it.Video && !caps.Video {
			return core.NewError(http.StatusUnprocessableEntity, codeUnsupported, "This device cannot play video (it is a speaker)")
		}
		if !it.Video && !caps.Audio {
			return core.NewError(http.StatusUnprocessableEntity, codeUnsupported, "This device cannot play audio")
		}
	}
	return nil
}

// connectError turns a failed connection into the API error.
func connectError(err error) error {
	switch {
	case errors.Is(err, renderer.ErrPairingRequired):
		return core.NewError(http.StatusUnprocessableEntity, codePairingRequired, "Pair this device first: it shows a PIN when you start pairing")
	case errors.Is(err, renderer.ErrPairingUnsupported):
		return errPairingUnsupported
	case errors.Is(err, renderer.ErrUnsupported):
		return core.NewError(http.StatusUnprocessableEntity, codeUnsupported, "This device cannot be used for casting")
	case errors.Is(err, context.Canceled):
		return err
	}
	return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "The device cannot be reached")
}

// POST /cast/sessions
func (m *Module) createSession(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req createRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if err := req.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	enabled, st := m.enabled, m.cfg
	m.mu.Unlock()
	if !enabled {
		return errDisabled
	}
	e := m.device(req.DeviceID)
	if e == nil {
		return errDeviceNotFound
	}
	caps, drv := m.capabilities(&e.dev)
	if drv == nil {
		return errDeviceNotFound
	}
	items, err := m.resolveItems(ctx, p, req.Items)
	if err != nil {
		return err
	}
	if err := checkCapabilities(caps, items); err != nil {
		return err
	}
	addrs := resolveAddrs(ctx, e)
	if _, err := m.mediaBase(ctx, st, addrs, netip.Addr{}); err != nil {
		return err
	}

	unlock := m.lockDevice(e.dev.ID)
	defer unlock()
	m.mu.Lock()
	old := m.byDevice[e.dev.ID]
	n := 0
	for _, s := range m.byDevice {
		if s.owner.UserID == p.UserID && s != old {
			n++
		}
	}
	m.mu.Unlock()
	if n >= maxSessionsPerUser {
		return errSessionLimit
	}
	if old != nil {
		old.end(codeReplaced, "Someone started casting to this device")
		waitSessions([]*session{old}, m.replaceWait)
	}

	creds := m.loadCreds(ctx, e.dev.ID)
	cctx, cancel := context.WithTimeout(ctx, m.connectWait)
	rend, err := drv.Connect(cctx, &e.dev, creds)
	cancel()
	if e.dev.Manual {
		// A device that answered (even to refuse) is reachable.
		m.setReachable(e.dev.ID, err == nil || errors.Is(err, renderer.ErrPairingRequired) ||
			errors.Is(err, renderer.ErrPairingUnsupported) || errors.Is(err, renderer.ErrUnsupported))
	}
	if err != nil {
		m.log.Info("cast: connecting to device failed", "device", e.dev.ID, "kind", e.dev.Kind, "err", err)
		return connectError(err)
	}
	local := netip.Addr{}
	if ad, ok := rend.(renderer.Addresser); ok {
		local = addrIP(ad.LocalAddr())
	}
	base, err := m.mediaBase(ctx, st, addrs, local)
	if err != nil {
		_ = rend.Close()
		return err
	}
	if req.Volume != nil {
		vctx, vcancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rend.SetVolume(vctx, *req.Volume); err != nil {
			m.log.Debug("cast: setting the initial volume failed", "device", e.dev.ID, "err", err)
		}
		vcancel()
	}

	s := m.newSession(p, e, drv, caps, creds, rend, base, items, req)
	m.mu.Lock()
	n = 0
	for _, o := range m.byDevice {
		if o.owner.UserID == p.UserID {
			n++
		}
	}
	if n >= maxSessionsPerUser || !m.enabled {
		m.mu.Unlock()
		s.cancel()
		_ = rend.Close()
		if !m.enabled {
			return errDisabled
		}
		return errSessionLimit
	}
	m.sessions[s.id] = s
	m.byDevice[e.dev.ID] = s
	m.byTicket[s.ticketHash] = s
	m.mu.Unlock()
	go s.run()
	m.log.Info("cast: session started", "session", s.id, "device", e.dev.ID, "kind", e.dev.Kind, "user", p.UserID, "items", len(items))
	return httpx.WriteJSON(w, http.StatusCreated, s.snapshot(creds != nil))
}

// newSession builds (does not start) a session on a connected renderer.
func (m *Module) newSession(p *core.Principal, e *deviceEntry, drv renderer.Driver, caps renderer.Capabilities, creds []byte,
	rend renderer.Renderer, base string, items []*castItem, req createRequest) *session {
	now := m.now()
	ctx, cancel := context.WithCancel(m.runCtx)
	ticket, hash := newTicket()
	index := 0
	if req.Index != nil {
		index = *req.Index
	}
	repeat := RepeatOff
	if req.Repeat != nil {
		repeat = *req.Repeat
	}
	s := &session{
		m: m, id: core.NewID(), owner: owner{UserID: p.UserID, Username: p.Username, Role: p.Role, Scopes: slices.Clone(p.Scopes)},
		entry: e, drv: drv, caps: caps, creds: creds, created: now, ticket: ticket, ticketHash: hash, base: base,
		ctx: ctx, cancel: cancel, cmds: make(chan *command, sessionCommandQueue), done: make(chan struct{}),
		rend: rend, lastCheck: now,
		state: StateLoading, q: newQueue(items, index, repeat, req.Shuffle != nil && *req.Shuffle, nil),
		changed: make(chan struct{}), updated: now, activity: now, version: 1,
	}
	if req.PositionSeconds != nil {
		s.start = *req.PositionSeconds
	}
	s.reindexLocked()
	s.addAllowedLocked(e.addrs...)
	s.learnAddrs(rend)
	st := rend.Status()
	s.volume, s.muted = st.Volume, st.Muted
	if req.Volume != nil {
		s.volume = *req.Volume
	}
	return s
}

// ownSession resolves the {id} session of the caller (owner only; 404 otherwise).
func (m *Module) ownSession(r *http.Request) (*session, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, errNoSession
	}
	p := core.MustPrincipal(r.Context())
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil || s.owner.UserID != p.UserID {
		return nil, errNoSession
	}
	return s, nil
}

func (m *Module) writeSession(w http.ResponseWriter, r *http.Request, status int, s *session) error {
	return httpx.WriteJSON(w, status, s.snapshot(m.isPaired(r.Context(), s.entry.dev.ID)))
}

// GET /cast/sessions: the caller's live sessions.
func (m *Module) listSessions(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	m.mu.Lock()
	var ss []*session
	for _, s := range m.byDevice {
		if s.owner.UserID == p.UserID {
			ss = append(ss, s)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(ss, func(a, b *session) int { return a.created.Compare(b.created) })
	paired := m.pairedIDs(r.Context())
	out := make([]sessionJSON, 0, len(ss))
	for _, s := range ss {
		s.touch()
		out = append(out, s.snapshot(paired[s.entry.dev.ID]))
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /cast/sessions/{id}?wait_version=&timeout= (long poll)
func (m *Module) getSession(w http.ResponseWriter, r *http.Request) error {
	s, err := m.ownSession(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if v := q.Get("wait_version"); v != "" {
		want, err := strconv.ParseInt(v, 10, 64)
		if err != nil || want < 0 {
			return core.BadRequest("wait_version: must be a version number")
		}
		timeout := 20 * time.Second
		if t := q.Get("timeout"); t != "" {
			n, err := strconv.Atoi(t)
			if err != nil || n < 1 || n > 30 {
				return core.BadRequest("timeout: must be 1 to 30 seconds")
			}
			timeout = time.Duration(n) * time.Second
		}
		s.touch()
		s.waitVersion(r.Context(), want, timeout)
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
	}
	s.touch()
	return m.writeSession(w, r, http.StatusOK, s)
}

// POST /cast/sessions/{id}/control
func (m *Module) controlSession(w http.ResponseWriter, r *http.Request) error {
	s, err := m.ownSession(r)
	if err != nil {
		return err
	}
	var req controlRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if err := req.validate(); err != nil {
		return err
	}
	if !s.live() {
		return errSessionEnded
	}
	s.touch()
	if err := s.do(r.Context(), func() error { return s.control(req) }); err != nil {
		return err
	}
	return m.writeSession(w, r, http.StatusOK, s)
}

// itemsRequest is the PUT/POST /cast/sessions/{id}/items body.
type itemsRequest struct {
	Items           []itemRef `json:"items"`
	Index           *int      `json:"index"`
	PositionSeconds *float64  `json:"position_seconds"`
	Position        *int      `json:"position"`
}

// queueItems validates and resolves an items request for session s.
func (m *Module) queueItems(r *http.Request, s *session, req *itemsRequest) ([]*castItem, error) {
	if err := checkRefs("items", req.Items, 1); err != nil {
		return nil, err
	}
	if !s.live() {
		return nil, errSessionEnded
	}
	items, err := m.resolveItems(r.Context(), core.MustPrincipal(r.Context()), req.Items)
	if err != nil {
		return nil, err
	}
	if err := checkCapabilities(s.caps, items); err != nil {
		return nil, err
	}
	return items, nil
}

// PUT /cast/sessions/{id}/items: replace the queue.
func (m *Module) replaceItems(w http.ResponseWriter, r *http.Request) error {
	s, err := m.ownSession(r)
	if err != nil {
		return err
	}
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	fe := map[string]string{}
	if req.Index != nil && (*req.Index < 0 || *req.Index >= len(req.Items)) {
		fe["index"] = "must be the index of one of the items"
	}
	if req.PositionSeconds != nil && !validSeconds(*req.PositionSeconds) {
		fe["position_seconds"] = "must be a number of seconds of 0 or more"
	}
	if len(fe) > 0 && len(req.Items) > 0 {
		return core.Validation(fe)
	}
	items, err := m.queueItems(r, s, &req)
	if err != nil {
		return err
	}
	index, pos := 0, 0.0
	if req.Index != nil {
		index = *req.Index
	}
	if req.PositionSeconds != nil {
		pos = *req.PositionSeconds
	}
	s.touch()
	if err := s.do(r.Context(), func() error { return s.replace(items, index, pos) }); err != nil {
		return err
	}
	return m.writeSession(w, r, http.StatusOK, s)
}

// POST /cast/sessions/{id}/items: insert (default append).
func (m *Module) insertItems(w http.ResponseWriter, r *http.Request) error {
	s, err := m.ownSession(r)
	if err != nil {
		return err
	}
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.Position != nil && *req.Position < 0 {
		return core.Validation(map[string]string{"position": "must be 0 or more"})
	}
	items, err := m.queueItems(r, s, &req)
	if err != nil {
		return err
	}
	at := -1
	if req.Position != nil {
		at = *req.Position
	}
	s.touch()
	if err := s.do(r.Context(), func() error { return s.insert(items, at) }); err != nil {
		return err
	}
	return m.writeSession(w, r, http.StatusOK, s)
}

// DELETE /cast/sessions/{id}: the owner, or an admin for any session.
func (m *Module) deleteSession(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errNoSession
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil || (s.owner.UserID != p.UserID && !isAdmin(p)) {
		return errNoSession
	}
	if !s.live() {
		return httpx.NoContent(w)
	}
	if s.owner.UserID == p.UserID {
		s.end("", "")
	} else {
		s.end("admin_stopped", "An administrator stopped this session")
		m.d.Audit.Record(ctx, "cast.session.stop", "cast_session", s.id, map[string]any{"owner_id": s.owner.UserID, "device_id": s.entry.dev.ID})
	}
	// Answer once the device was told to stop, so a client can resume locally right away.
	waitSessions([]*session{s}, 8*time.Second)
	return httpx.NoContent(w)
}
