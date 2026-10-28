package cast

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/tv"
)

// Session states (API.md §14 CastSession.state).
const (
	StateConnecting = "connecting"
	StateLoading    = "loading"
	StateBuffering  = "buffering"
	StatePlaying    = "playing"
	StatePaused     = "paused"
	StateIdle       = "idle"
	StateStopped    = "stopped"
	StateError      = "error"
)

// Error codes of CastSession.error (several are also HTTP error codes of POST /sessions).
const (
	codeReplaced           = "replaced"            // another session took the device
	codeIdle               = "idle"                // no client request or playback for 30 min
	codeAccessRevoked      = "access_revoked"      // the owner's account was disabled or deleted
	codeDisabled           = "cast_disabled"       // an administrator turned casting off
	codeDeviceUnavailable  = "device_unavailable"  // the device cannot be reached
	codePlaybackFailed     = "playback_failed"     // the device could not play the item
	codeUnsupported        = "unsupported_media"   // the device cannot play this kind of item
	codePairingRequired    = "pairing_required"    // pair the device first
	codePairingUnsupported = "pairing_unsupported" // the device wants a pairing/password we cannot do
	codeInterrupted        = "interrupted"         // stopped or taken over on the device itself
	codeUnavailable        = "cast_unavailable"    // no media URL the device could reach
)

// Session timing (fields of Module so tests can shorten them).
const (
	defaultIdleTimeout   = 30 * time.Minute // API.md §14
	defaultTicketGrace   = 60 * time.Second // tickets outlive their session by this
	defaultEndedKeep     = 2 * time.Minute  // ended sessions stay readable (final state)
	defaultConnectWait   = 8 * time.Second  // connecting to a device
	defaultLoadTimeout   = 45 * time.Second // a device accepting a LOAD (may launch an app first)
	defaultCommandWait   = 50 * time.Second // an HTTP request waiting for the session loop
	defaultReplaceWait   = 5 * time.Second  // a replaced session closing its device connection
	defaultProgressEvery = 15 * time.Second // progress / position saves while playing
	ownerCheckEvery      = time.Minute      // re-checking the owner's account while nothing fetches
	maxFailures          = 3                // consecutive unplayable items before giving up
	maxSessionsPerUser   = 3
	liveResyncAfter      = 5 * time.Second // a live channel paused longer re-joins the broadcast
	programOverrun       = 5 * time.Second // a live program still playing this long after its slot ends is replaced
	positionJump         = 2 * time.Second // a reported position this far from the expected one is a visible change
	sessionCommandQueue  = 8
	trackPlayMinRecorded = 1.0 // seconds; shorter plays are not even sent to the music service
)

// sessionError is CastSession.error.
type sessionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// owner is the user a session plays for and the scopes of the credential that created it.
type owner struct {
	UserID, Username string
	Role             core.Role // at creation (each check uses the current one)
	Scopes           []string
}

// session is one cast session. A goroutine (run) owns the device connection: commands from
// HTTP handlers, device status changes and a 1 s ticker are serialised through it, so the
// renderer and the per-item bookkeeping are only touched there. mu guards what handlers
// read (state, queue, position, version).
type session struct {
	m       *Module
	id      string
	owner   owner
	entry   *deviceEntry // device snapshot at creation
	drv     renderer.Driver
	caps    renderer.Capabilities
	creds   []byte
	created time.Time

	// ticket is the media capability the device's URLs carry; the registry indexes sessions
	// by its SHA-256 (ticketHash) and compares in constant time. The plaintext lives only
	// here, to build those URLs, and is never logged or returned to clients.
	ticket     string
	ticketHash [32]byte
	base       string // media URL base (scheme://host[:port], no trailing slash)

	ctx    context.Context
	cancel context.CancelFunc
	cmds   chan *command
	done   chan struct{}
	prev   *session // the replaced session (its loop must exit before ours connects)

	// Loop-owned.
	rend      renderer.Renderer
	cur       *loaded // nil when nothing is loaded on the device
	carry     *loaded // a seek reloading the same item: its play bookkeeping carries over
	pending   func()  // work queued by a command to run after the command answered
	failures  int
	lastSave  time.Time
	lastCheck time.Time
	start     float64 // where the first item starts

	mu         sync.Mutex
	state      string
	err        *sessionError
	q          *queue
	itemKeys   map[string]int
	pos        float64
	posAt      time.Time
	dur        float64
	volume     float64
	muted      bool
	version    int64
	changed    chan struct{}
	updated    time.Time
	activity   time.Time
	endedAt    time.Time
	endCode    string
	endMessage string
	allowed    []netip.Addr
}

type command struct {
	fn   func() error
	done chan error
}

var (
	errSessionEnded = core.Conflict("This cast session has ended")
	errBusyDevice   = core.NewError(http.StatusGatewayTimeout, codeDeviceUnavailable, "The device did not respond in time")
	errNotSeekable  = core.Conflict("A live channel cannot be seeked")
	errNoNextItem   = core.Conflict("There is no next item (repeat is off)")
	errNothingToSet = core.Conflict("Nothing is playing")
)

/* ------------------------------------------------------------ bookkeeping */

// bumpLocked records a visible change: a new version wakes the long polls.
func (s *session) bumpLocked() {
	s.version++
	s.updated = s.m.now()
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *session) setState(state string, e *sessionError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == state && sameError(s.err, e) {
		return
	}
	s.state, s.err = state, e
	s.bumpLocked()
}

func sameError(a, b *sessionError) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// touch records client activity (it keeps the session from ending as idle).
func (s *session) touch() {
	s.mu.Lock()
	s.activity = s.m.now()
	s.mu.Unlock()
}

// live reports whether the session has not ended.
func (s *session) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endedAt.IsZero()
}

// positionAt is the playback position at t (extrapolated while playing).
func (s *session) positionAtLocked(t time.Time) float64 {
	p := s.pos
	if s.state == StatePlaying && !s.posAt.IsZero() && t.After(s.posAt) {
		p += t.Sub(s.posAt).Seconds()
	}
	if s.dur > 0 && p > s.dur {
		p = s.dur
	}
	return max(0, p)
}

// setPosition records a position report; a jump against the expected position is a visible
// change.
func (s *session) setPositionLocked(p, dur float64, now time.Time) {
	expected := s.positionAtLocked(now)
	s.pos, s.posAt = p, now
	changed := false
	if dur != s.dur {
		s.dur, changed = dur, true
	}
	if math.Abs(expected-p) > positionJump.Seconds() {
		changed = true
	}
	if changed {
		s.bumpLocked()
	}
}

/* ------------------------------------------------------------------ JSON */

// sessionJSON is the CastSession JSON (API.md §14).
type sessionJSON struct {
	ID              string        `json:"id"`
	Device          deviceJSON    `json:"device"`
	OwnerID         string        `json:"owner_id"`
	State           string        `json:"state"`
	Error           *sessionError `json:"error"`
	Items           []itemJSON    `json:"items"`
	Index           int           `json:"index"`
	PositionSeconds float64       `json:"position_seconds"`
	DurationSeconds *float64      `json:"duration_seconds"`
	Volume          float64       `json:"volume"`
	Muted           bool          `json:"muted"`
	Repeat          string        `json:"repeat"`
	Shuffle         bool          `json:"shuffle"`
	Version         int64         `json:"version"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
}

// snapshot renders the session; position_seconds is the position at the time of the call.
func (s *session) snapshot(paired bool) sessionJSON {
	caps := s.caps
	ep := primaryEndpoint(&s.entry.dev)
	dev := deviceJSON{
		ID: s.entry.dev.ID, Name: s.entry.dev.Name, Kind: s.entry.dev.Kind, Model: s.entry.dev.Model, Host: ep.Host,
		Port: ep.Port, Capabilities: []string{}, Source: "discovered", RequiresPairing: caps.RequiresPairing, Paired: paired,
		Available: true,
	}
	if caps.Audio {
		dev.Capabilities = append(dev.Capabilities, "audio")
	}
	if caps.Video {
		dev.Capabilities = append(dev.Capabilities, "video")
	}
	if s.entry.dev.Manual {
		dev.Source = "manual"
	}
	now := s.m.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endedAt.IsZero() {
		id := s.id
		dev.Busy, dev.SessionID = true, &id
	}
	out := sessionJSON{
		ID: s.id, Device: dev, OwnerID: s.owner.UserID, State: s.state, Error: s.err,
		Items: make([]itemJSON, 0, len(s.q.items)), Index: s.q.current(), PositionSeconds: s.positionAtLocked(now),
		Volume: s.volume, Muted: s.muted, Repeat: s.q.repeat, Shuffle: s.q.shuffle, Version: s.version,
		CreatedAt: db.FormatTime(s.created), UpdatedAt: db.FormatTime(s.updated),
	}
	for _, it := range s.q.items {
		out.Items = append(out.Items, it.json())
	}
	if s.dur > 0 {
		d := s.dur
		out.DurationSeconds = &d
	}
	return out
}

// waitVersion blocks until the version differs from v, the session ends, timeout passes or
// ctx is done (long poll).
func (s *session) waitVersion(ctx context.Context, v int64, timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		s.mu.Lock()
		ch, cur := s.changed, s.version
		s.mu.Unlock()
		if cur != v {
			return
		}
		select {
		case <-ch:
		case <-t.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

/* ------------------------------------------------------------------ tickets */

// hasItem reports whether kind/id is in the queue (tickets are only valid for those).
func (s *session) hasItem(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.itemKeys[kind+"/"+id] > 0
}

func (s *session) reindexLocked() {
	s.itemKeys = make(map[string]int, len(s.q.items))
	for _, it := range s.q.items {
		s.itemKeys[it.key()]++
	}
}

// addressAllowed reports whether a request from ip may use the ticket.
func (s *session) addressAllowed(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.allowed, ip)
}

func (s *session) addAllowedLocked(addrs ...netip.Addr) {
	for _, a := range addrs {
		a = a.Unmap().WithZone("")
		if a.IsValid() && !slices.Contains(s.allowed, a) && len(s.allowed) < 32 {
			s.allowed = append(s.allowed, a)
		}
	}
}

// learnAddrs adds the renderer connection's remote address to the ticket's allowed sources.
func (s *session) learnAddrs(r renderer.Renderer) {
	ad, ok := r.(renderer.Addresser)
	if !ok {
		return
	}
	if a := addrIP(ad.RemoteAddr()); a.IsValid() {
		s.mu.Lock()
		s.addAllowedLocked(a)
		s.mu.Unlock()
	}
}

// addrIP extracts the IP of a net.Addr.
func addrIP(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(v.IP)
		return ip.Unmap()
	case *net.UDPAddr:
		ip, _ := netip.AddrFromSlice(v.IP)
		return ip.Unmap()
	}
	if a == nil {
		return netip.Addr{}
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

func hashTicket(t string) [32]byte { return sha256.Sum256([]byte(t)) }

/* ------------------------------------------------------------------ owner */

// ownerPrincipal is the session owner's principal with their current role, or nil when the
// account was disabled or deleted (the session must then end).
func (s *session) ownerPrincipal(ctx context.Context) *core.Principal {
	return s.m.userPrincipal(ctx, s.owner)
}

/* ------------------------------------------------------------------ commands */

// do runs fn on the session loop and returns its error. It fails when the session ended,
// ctx is done, or the loop does not get to it in time (a device that hangs).
func (s *session) do(ctx context.Context, fn func() error) error {
	c := &command{fn: fn, done: make(chan error, 1)}
	t := time.NewTimer(s.m.commandWait)
	defer t.Stop()
	select {
	case s.cmds <- c:
	case <-s.done:
		return errSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errBusyDevice
	}
	select {
	case err := <-c.done:
		return err
	case <-s.done:
		select {
		case err := <-c.done:
			return err
		default:
		}
		return errSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errBusyDevice
	}
}

// end asks the session to finish (code "" = stopped by its owner).
func (s *session) end(code, message string) {
	s.mu.Lock()
	if s.endedAt.IsZero() && s.endCode == "" && code != "" {
		s.endCode, s.endMessage = code, message
	}
	s.mu.Unlock()
	s.cancel()
}

/* ------------------------------------------------------------------ loop */

// run is the session loop. The renderer was connected by the creator.
func (s *session) run() {
	defer close(s.done)
	defer s.finish()
	// A bug (or a driver failing on hostile device input) must end this session, not the server.
	defer func() {
		if p := recover(); p != nil {
			s.m.log.Error("cast: session failed", "session", s.id, "panic", p, "stack", string(debug.Stack()))
			s.mu.Lock()
			if s.endCode == "" {
				s.endCode, s.endMessage = codePlaybackFailed, "The session failed"
			}
			s.mu.Unlock()
		}
	}()
	s.schedule(func() { s.loadCurrent(s.start, nil) })
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		s.runPending()
		var changed <-chan struct{}
		if s.rend != nil {
			changed = s.rend.Changed()
		}
		select {
		case <-s.ctx.Done():
			return
		case c := <-s.cmds:
			c.done <- c.fn()
		case _, ok := <-changed:
			if ok {
				s.onStatus()
			} else {
				s.onLost()
			}
		case <-tick.C:
			s.onTick()
		}
	}
}

func (s *session) schedule(fn func()) {
	prev := s.pending
	if prev == nil {
		s.pending = fn
		return
	}
	s.pending = func() { prev(); fn() }
}

func (s *session) runPending() {
	for s.pending != nil && s.ctx.Err() == nil {
		f := s.pending
		s.pending = nil
		f()
	}
}

// finish runs when the loop ends: progress is recorded, the device stopped and released,
// and the final state published.
func (s *session) finish() {
	s.recordCurrent(false)
	if s.rend != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
		if s.cur != nil {
			_ = s.rend.Stop(ctx)
		}
		cancel()
		_ = s.rend.Close()
		s.rend = nil
	}
	s.cur = nil
	s.mu.Lock()
	var e *sessionError
	if s.endCode != "" {
		e = &sessionError{Code: s.endCode, Message: s.endMessage}
	}
	s.state, s.err = StateStopped, e
	s.endedAt = s.m.now()
	s.bumpLocked()
	s.mu.Unlock()
	s.m.sessionEnded(s)
}

// onStatus mirrors the renderer's status into the session.
func (s *session) onStatus() {
	if s.rend == nil {
		return
	}
	st := s.rend.Status()
	now := s.m.now()
	s.mu.Lock()
	if st.Volume != s.volume || st.Muted != s.muted {
		s.volume, s.muted = st.Volume, st.Muted
		s.bumpLocked()
	}
	s.mu.Unlock()
	if s.cur == nil {
		return // nothing of ours is loaded (stale reports of a failed or replaced load)
	}
	switch st.State {
	case renderer.StateEnded:
		s.onEnded()
		return
	case renderer.StateError:
		if errors.Is(st.Err, renderer.ErrInterrupted) {
			s.end(codeInterrupted, "Playback was stopped or taken over on the device")
			return
		}
		s.onFailure(codeFor(st.Err, codePlaybackFailed), "The device could not play this item")
		return
	case renderer.StateIdle:
		if errors.Is(st.Err, renderer.ErrInterrupted) {
			s.end(codeInterrupted, "Playback was stopped or taken over on the device")
			return
		}
	}
	state := sessionState(st.State)
	l := s.cur
	s.updatePlayed(state, now)
	dur := l.duration
	if dur <= 0 && !l.live && l.base == 0 {
		dur = st.Duration
	}
	s.mu.Lock()
	if state == StatePaused && s.state != StatePaused && l.live {
		l.pausedAt = now
	}
	enteredPause := state == StatePaused && s.state != StatePaused
	if s.state != state || s.err != nil {
		s.state, s.err = state, nil
		s.bumpLocked()
	}
	s.setPositionLocked(l.base+st.Position, dur, now)
	if state == StatePlaying {
		s.activity = now
	}
	s.mu.Unlock()
	if state == StatePlaying {
		s.failures = 0
	}
	if enteredPause {
		s.recordCurrent(false) // a paused item is where the owner resumes later
	}
}

func sessionState(rs string) string {
	switch rs {
	case renderer.StateLoading:
		return StateLoading
	case renderer.StateBuffering:
		return StateBuffering
	case renderer.StatePlaying:
		return StatePlaying
	case renderer.StatePaused:
		return StatePaused
	}
	return StateIdle
}

// codeFor maps a renderer error to a session error code.
func codeFor(err error, fallback string) string {
	switch {
	case errors.Is(err, renderer.ErrUnsupported):
		return codeUnsupported
	case errors.Is(err, renderer.ErrPairingRequired):
		return codePairingRequired
	case errors.Is(err, renderer.ErrPairingUnsupported):
		return codePairingUnsupported
	}
	return fallback
}

// onTick runs every second: position and played time, periodic saves, the live schedule's
// safety net, the owner check and the idle timeout.
func (s *session) onTick() {
	now := s.m.now()
	if s.rend != nil && s.cur != nil {
		st := s.rend.Status()
		switch st.State {
		case renderer.StatePlaying, renderer.StatePaused, renderer.StateBuffering:
			state := sessionState(st.State)
			s.updatePlayed(state, now)
			s.mu.Lock()
			if s.state == state {
				dur := s.cur.duration
				if dur <= 0 && !s.cur.live && s.cur.base == 0 {
					dur = st.Duration
				}
				s.setPositionLocked(s.cur.base+st.Position, dur, now)
			}
			if state == StatePlaying {
				s.activity = now
			}
			s.mu.Unlock()
		}
		// A live program that overruns its slot (the file is longer than the scheduled
		// duration, or the device never reports the end) gives way to the next program.
		if l := s.cur; l.live && l.program != nil && now.After(l.program.End.Add(programOverrun)) {
			s.onEnded()
		}
	}
	s.mu.Lock()
	state, idle := s.state, now.Sub(s.activity)
	s.mu.Unlock()
	if state == StatePlaying && now.Sub(s.lastSave) >= s.m.progressEvery {
		s.recordCurrent(false)
	}
	if now.Sub(s.lastCheck) >= ownerCheckEvery {
		s.lastCheck = now
		if s.ownerPrincipal(s.ctx) == nil && s.ctx.Err() == nil {
			s.end(codeAccessRevoked, "The owner's account is no longer active")
			return
		}
	}
	switch state {
	case StatePlaying, StateBuffering, StateLoading, StateConnecting:
	default:
		if idle >= s.m.idleTimeout {
			s.end(codeIdle, "The session ended after 30 minutes without activity")
		}
	}
}

// updatePlayed advances the played-seconds counter of the current item while it plays.
func (s *session) updatePlayed(state string, now time.Time) {
	l := s.cur
	if l == nil {
		return
	}
	if state == StatePlaying {
		if !l.playingAt.IsZero() {
			if d := now.Sub(l.playingAt).Seconds(); d > 0 && d < 5 {
				l.played += d
			}
		}
		l.playingAt = now
	} else {
		l.playingAt = time.Time{}
	}
}

// onEnded handles the current item finishing on its own: progress, then the next item
// (channels: the next program) or idle.
func (s *session) onEnded() {
	l := s.cur
	if l == nil {
		return
	}
	s.mu.Lock()
	if l.duration > 0 {
		s.pos, s.posAt = l.duration, s.m.now()
	}
	s.mu.Unlock()
	s.recordCurrent(true)
	s.cur = nil
	if l.item.Kind == KindChannel {
		pr := l.program
		s.schedule(func() { s.loadCurrent(0, pr) })
		return
	}
	s.mu.Lock()
	ok := s.q.advance()
	s.bumpLocked()
	s.mu.Unlock()
	if ok {
		s.schedule(func() { s.loadCurrent(0, nil) })
		return
	}
	s.mu.Lock()
	s.state, s.err = StateIdle, nil
	s.bumpLocked()
	s.mu.Unlock()
}

// onFailure handles an item the device could not play: skip to the next one (a few times in
// a row at most), else stop in the error state.
func (s *session) onFailure(code, msg string) {
	l := s.cur
	s.recordCurrent(false)
	s.cur = nil
	s.failures++
	fatal := code == codePairingRequired || code == codePairingUnsupported
	if !fatal && s.failures < maxFailures {
		if l != nil && l.item.Kind == KindChannel {
			pr := l.program
			s.schedule(func() { s.loadCurrent(0, pr) })
			return
		}
		s.mu.Lock()
		ok := false
		switch {
		case s.q.pos+1 < len(s.q.order):
			s.q.pos++
			ok = true
		case s.q.repeat == RepeatAll && len(s.q.order) > 1:
			s.q.pos = 0
			ok = true
		}
		if ok {
			s.bumpLocked()
		}
		s.mu.Unlock()
		if ok {
			s.schedule(func() { s.loadCurrent(0, nil) })
			return
		}
	}
	s.setState(StateError, &sessionError{Code: code, Message: msg})
}

// onLost handles a renderer that lost its device connection for good: reconnect once and
// resume where playback was.
func (s *session) onLost() {
	var cause error
	if s.rend != nil {
		cause = s.rend.Status().Err
		_ = s.rend.Close()
		s.rend = nil
	}
	s.m.log.Info("cast: device connection lost", "session", s.id, "device", s.entry.dev.ID, "err", cause)
	resume := s.cur
	var at float64
	s.mu.Lock()
	at = s.positionAtLocked(s.m.now())
	s.mu.Unlock()
	if err := s.reconnect(); err != nil {
		s.recordCurrent(false)
		s.cur = nil
		s.setState(StateError, &sessionError{Code: codeDeviceUnavailable, Message: "Lost the connection to the device"})
		return
	}
	if resume != nil {
		s.cur = nil
		var pr *tv.CastProgram
		if resume.item.Kind == KindChannel {
			pr = resume.program
		}
		s.schedule(func() { s.resumeAt(resume, at, pr) })
	} else {
		s.setState(StateIdle, nil)
	}
}

// resumeAt reloads an item after a reconnect at position at.
func (s *session) resumeAt(l *loaded, at float64, pr *tv.CastProgram) {
	if l.item.Kind == KindChannel && l.live {
		s.loadCurrent(0, nil) // re-join the broadcast
		return
	}
	if l.item.Kind == KindChannel && pr != nil {
		s.loadProgramAt(l.item, pr.Index, at)
		return
	}
	s.loadCurrent(at, nil)
}

// reconnect opens a new renderer on the device (once).
func (s *session) reconnect() error {
	s.setState(StateConnecting, nil)
	ctx, cancel := context.WithTimeout(s.ctx, s.m.connectWait)
	defer cancel()
	r, err := s.drv.Connect(ctx, &s.entry.dev, s.creds)
	if err != nil {
		return err
	}
	s.rend = r
	s.learnAddrs(r)
	s.syncVolume()
	return nil
}

// ensureRenderer reconnects a session whose device connection was lost (a client asking for
// playback again).
func (s *session) ensureRenderer() error {
	if s.rend != nil {
		return nil
	}
	if err := s.reconnect(); err != nil {
		s.setState(StateError, &sessionError{Code: codeDeviceUnavailable, Message: "Lost the connection to the device"})
		return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "The device cannot be reached")
	}
	return nil
}

func (s *session) syncVolume() {
	if s.rend == nil {
		return
	}
	st := s.rend.Status()
	s.mu.Lock()
	if st.Volume != s.volume || st.Muted != s.muted {
		s.volume, s.muted = st.Volume, st.Muted
		s.bumpLocked()
	}
	s.mu.Unlock()
}

/* ------------------------------------------------------------------ loading */

// loadCurrent loads the queue's current item from start seconds. For a channel item, after
// (when not nil) is the program that just ended or failed: the following one plays.
func (s *session) loadCurrent(start float64, after *tv.CastProgram) {
	if s.ctx.Err() != nil {
		return
	}
	if s.rend == nil {
		if err := s.ensureRenderer(); err != nil {
			return
		}
	}
	owner := s.ownerPrincipal(s.ctx)
	if owner == nil {
		s.end(codeAccessRevoked, "The owner's account is no longer active")
		return
	}
	s.mu.Lock()
	it := s.q.item()
	s.mu.Unlock()
	var pr *tv.CastProgram
	if it.Kind == KindChannel {
		var err error
		pr, err = s.nextProgram(owner, it, after)
		if err != nil || pr == nil {
			s.cur = &loaded{item: it}
			s.onFailure(codePlaybackFailed, "Nothing is on air on this channel")
			return
		}
		start = pr.OffsetSeconds
	}
	s.load(owner, it, start, pr)
}

// loadProgramAt loads program index of an on-demand channel item at offset seconds.
func (s *session) loadProgramAt(it *castItem, index int64, offset float64) {
	owner := s.ownerPrincipal(s.ctx)
	if owner == nil {
		s.end(codeAccessRevoked, "The owner's account is no longer active")
		return
	}
	pr, err := s.m.tv.CastProgramByIndex(s.ctx, owner, it.ID, index, offset)
	if err != nil || pr == nil {
		s.cur = &loaded{item: it}
		s.onFailure(codePlaybackFailed, "Nothing is on air on this channel")
		return
	}
	s.load(owner, it, pr.OffsetSeconds, pr)
}

// nextProgram picks the program a channel item plays: the one on air (on-demand: at the
// owner's saved position), or the one after a program that ended.
func (s *session) nextProgram(owner *core.Principal, it *castItem, after *tv.CastProgram) (*tv.CastProgram, error) {
	tvm := s.m.tv
	if tvm == nil {
		return nil, core.NotFound("Channel not found")
	}
	if after == nil {
		return tvm.CastProgramAt(s.ctx, owner, it.ID, s.m.now())
	}
	ch, err := tvm.CastChannel(s.ctx, owner, it.ID)
	if err != nil {
		return nil, err
	}
	if ch.OnDemand {
		next := after.Index + 1
		if err := tvm.CastSavePosition(s.ctx, owner, it.ID, next, 0); err != nil {
			s.m.log.Debug("cast: saving channel position failed", "session", s.id, "err", err)
		}
		return tvm.CastProgramByIndex(s.ctx, owner, it.ID, next, 0)
	}
	// Live: never start a program before the previous one's slot ended (the device may finish
	// a little early); a late device joins the next program at its live offset.
	at := s.m.now()
	if at.Before(after.End) {
		at = after.End
	}
	return tvm.CastProgramAt(s.ctx, owner, it.ID, at)
}

// load sends one item to the device.
func (s *session) load(owner *core.Principal, it *castItem, start float64, pr *tv.CastProgram) {
	if s.rend == nil {
		if err := s.ensureRenderer(); err != nil {
			return
		}
	}
	req, l, err := s.buildLoad(s.ctx, owner, it, start, pr)
	if c := s.carry; c != nil {
		s.carry = nil
		if l != nil && c.item == it {
			l.started, l.played, l.recorded = c.started, c.played, c.recorded
		}
	}
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		s.m.log.Debug("cast: item unavailable", "session", s.id, "item", it.key(), "err", err)
		s.cur = &loaded{item: it}
		s.onFailure(codePlaybackFailed, "This item is not available")
		return
	}
	now := s.m.now()
	s.mu.Lock()
	s.state, s.err = StateLoading, nil
	s.pos, s.posAt, s.dur = start, now, l.duration
	s.activity = now
	s.bumpLocked()
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, s.m.loadTimeout)
	err = s.rend.Load(ctx, req)
	cancel()
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		s.m.log.Info("cast: device could not load item", "session", s.id, "device", s.entry.dev.ID, "item", it.key(), "err", err)
		s.cur = l
		s.onFailure(codeFor(err, codePlaybackFailed), "The device could not play this item")
		return
	}
	s.cur = l
	s.lastSave = now
	s.onStatus()
}

/* ------------------------------------------------------------------ progress */

// recordCurrent records the owner's progress on the current item (ended: it finished on its
// own). Media items keep §6 progress, on-demand channels their position, tracks get a §12
// play once (when the item changes, ends or the session stops).
func (s *session) recordCurrent(ended bool) {
	l := s.cur
	if l == nil {
		return
	}
	now := s.m.now()
	s.lastSave = now
	s.mu.Lock()
	pos := s.positionAtLocked(now)
	state := s.state
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	owner := s.ownerPrincipal(ctx)
	if owner == nil {
		return
	}
	var err error
	switch l.item.Kind {
	case KindMedia:
		if s.m.media != nil && (pos > 0 || ended) {
			if ended && l.duration > 0 {
				pos = l.duration
			}
			err = s.m.media.RecordProgress(ctx, owner, l.item.ID, pos, nil)
		}
	case KindChannel:
		if l.program != nil && !l.live && s.m.tv != nil {
			idx, off := l.program.Index, pos
			if ended {
				idx, off = idx+1, 0
			}
			err = s.m.tv.CastSavePosition(ctx, owner, l.item.ID, idx, off)
		}
	case KindTrack:
		if state == StatePlaying || state == StatePaused || ended {
			s.updatePlayed(StateIdle, now) // close the running interval
			if state == StatePlaying && !ended {
				l.playingAt = now // still playing: keep counting from here
			}
		}
		// A play is recorded once, when the item ends or the session stops (recordFinal
		// covers the user moving to another item).
		final := ended || s.ctx.Err() != nil
		if !final || l.recorded || s.m.music == nil {
			return
		}
		if l.played >= trackPlayMinRecorded || ended {
			l.recorded = true
			err = s.m.music.RecordPlay(ctx, owner, l.item.ID, l.started, l.played, ended)
		}
	}
	if err != nil {
		s.m.log.Debug("cast: recording progress failed", "session", s.id, "item", l.item.key(), "err", err)
	}
}

// recordFinal records the current item as left (not ended) and forgets it: the user moves
// elsewhere in the queue or replaces it.
func (s *session) recordFinal() {
	l := s.cur
	if l == nil {
		return
	}
	if l.item.Kind == KindTrack && !l.recorded && s.m.music != nil {
		now := s.m.now()
		s.updatePlayed(StateIdle, now)
		if l.played >= trackPlayMinRecorded {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			if owner := s.ownerPrincipal(ctx); owner != nil {
				l.recorded = true
				if err := s.m.music.RecordPlay(ctx, owner, l.item.ID, l.started, l.played, false); err != nil {
					s.m.log.Debug("cast: recording play failed", "session", s.id, "err", err)
				}
			}
			cancel()
		}
	} else {
		s.recordCurrent(false)
	}
	s.cur = nil
}
