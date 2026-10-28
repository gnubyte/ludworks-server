package chromecast

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/cast/renderer"
)

// Errors of the renderer.
var (
	// ErrPlaybackFailed is wrapped by Load errors and Status.Err when the device could not play
	// the media (LOAD_FAILED, or an IDLE media status with idleReason ERROR).
	ErrPlaybackFailed = errors.New("chromecast: the device could not play the media")

	errClosed       = errors.New("chromecast: renderer closed")
	errNotConnected = errors.New("chromecast: not connected to the device (reconnecting)")
	errNoMedia      = errors.New("chromecast: nothing is loaded on the device")
)

const (
	// closeTimeout bounds the best-effort STOP/CLOSE messages of Close.
	closeTimeout = 2 * time.Second
	// finishSlack: media that vanished while the connection was down within this many seconds
	// of its end counts as finished rather than interrupted.
	finishSlack = 10
)

// player is a connected Chromecast: the renderer.Renderer returned by Driver.Connect.
type player struct {
	d      *Driver
	log    *slog.Logger
	dev    renderer.Device
	host   string
	port   int
	sender string
	ids    atomic.Int64

	ctx    context.Context // cancelled by Close: stops the supervisor and reconnects
	cancel context.CancelFunc
	wg     sync.WaitGroup // supervisor and link readers

	loadMu    sync.Mutex // serialises Load
	closeOnce sync.Once

	mu           sync.Mutex
	link         *link // nil while reconnecting and after the end
	laddr, raddr net.Addr
	ended        bool   // closed, or the connection was lost for good
	appSession   string // our Default Media Receiver session ("" = none)
	transport    string // its transportId (destination of media messages)
	launching    bool   // a LAUNCH is in flight: the next listed receiver app becomes ours
	appSeen      chan struct{}
	mediaSession int64 // current media session (0 = none)
	loading      bool
	loadReqID    int64 // requestId of the LOAD in flight (its answer carries the new media session)
	live         bool  // the current item was loaded as live (not seekable)
	ownStop      bool  // we asked for STOP: the IDLE that follows is not an interruption
	st           renderer.Status
	basePos      float64   // last known position ...
	posAt        time.Time // ... and when it was current (zero = not advancing)
	rate         float64
	changed      chan struct{}
	changedDone  bool
}

func newPlayer(d *Driver, dev renderer.Device, host string, port int) *player {
	ctx, cancel := context.WithCancel(context.Background())
	return &player{
		d: d, log: d.log.With("device", dev.Name), dev: dev, host: host, port: port, sender: senderID(),
		ctx: ctx, cancel: cancel, appSeen: make(chan struct{}, 1), changed: make(chan struct{}, 1),
		st: renderer.Status{State: renderer.StateIdle, Volume: 1}, rate: 1,
	}
}

func (p *player) nextID() int64 { return p.ids.Add(1) }

// start opens the first connection (bounded by ctx and ConnectTimeout) and starts the
// supervisor.
func (p *player) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.d.o.ConnectTimeout)
	defer cancel()
	l, err := p.open(ctx)
	if err != nil {
		p.cancel()
		p.wg.Wait()
		return err
	}
	p.mu.Lock()
	p.link = l
	p.mu.Unlock()
	p.wg.Add(1)
	go p.run(l)
	return nil
}

// open dials the device, opens the virtual connection to the platform receiver and waits for
// its RECEIVER_STATUS (which also tells whether our receiver app still runs).
func (p *player) open(ctx context.Context) (*link, error) {
	conn, err := p.d.dial(ctx, p.host, p.port)
	if err != nil {
		return nil, err
	}
	var l *link
	l = newLink(conn, p.sender, missedHeartbeats*p.d.o.HeartbeatInterval, func(m *Message) { p.onMessage(l, m) })
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		l.readLoop()
	}()
	stop := context.AfterFunc(ctx, func() { l.fail(ctx.Err()) })
	fail := func(err error) (*link, error) {
		stop()
		l.fail(err)
		return nil, err
	}
	if err := l.sendTo(NamespaceConnection, ReceiverID, connectMsg{Type: "CONNECT"}); err != nil {
		return fail(fmt.Errorf("chromecast: connect to the receiver: %w", err))
	}
	id := p.nextID()
	r, err := l.request(ctx, NamespaceReceiver, ReceiverID, id, requestMsg{Type: "GET_STATUS", RequestID: id}, p.d.o.ConnectTimeout)
	if err != nil {
		return fail(fmt.Errorf("chromecast: no receiver status: %w", err))
	}
	if r.typ != "RECEIVER_STATUS" {
		return fail(fmt.Errorf("chromecast: unexpected answer %q to GET_STATUS", truncate(r.typ)))
	}
	if !stop() {
		return nil, fmt.Errorf("chromecast: connect: %w", ctx.Err())
	}
	p.mu.Lock()
	p.laddr, p.raddr = conn.LocalAddr(), conn.RemoteAddr()
	p.mu.Unlock()
	return l, nil
}

// run supervises the connection: heartbeats, status polls and reconnects, until Close or
// until reconnecting fails for good.
func (p *player) run(l *link) {
	defer p.wg.Done()
	for {
		p.supervise(l)
		if p.ctx.Err() != nil {
			l.fail(errLinkClosed)
			return
		}
		nl, err := p.reconnect(l.cause())
		if err != nil {
			if p.ctx.Err() == nil {
				p.lose(err)
			}
			return
		}
		l = nl
	}
}

// supervise sends heartbeats and status polls on l until it dies or the player closes.
func (p *player) supervise(l *link) {
	hb := time.NewTicker(p.d.o.HeartbeatInterval)
	defer hb.Stop()
	poll := time.NewTicker(p.d.o.StatusInterval)
	defer poll.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-l.done:
			return
		case <-hb.C:
			if l.sendTo(NamespaceHeartbeat, ReceiverID, typeOnly{Type: "PING"}) != nil {
				return
			}
		case <-poll.C:
			if dest, ok := p.pollTarget(); ok {
				id := p.nextID()
				if l.sendTo(NamespaceMedia, dest, requestMsg{Type: "GET_STATUS", RequestID: id}) != nil {
					return
				}
			}
		}
	}
}

// pollTarget returns the transport to poll while media is buffering or playing.
func (p *player) pollTarget() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ok := p.transport != "" && p.mediaSession != 0 &&
		(p.st.State == renderer.StatePlaying || p.st.State == renderer.StateBuffering)
	return p.transport, ok
}

// reconnect replaces a lost connection, re-joining our receiver app when it still runs.
func (p *player) reconnect(cause error) (*link, error) {
	p.mu.Lock()
	p.link = nil
	p.mu.Unlock()
	p.log.Info("chromecast: connection lost, reconnecting", "err", cause)
	err := cause
	for i := 0; i < p.d.o.ReconnectAttempts; i++ {
		t := time.NewTimer(p.d.o.ReconnectDelay << i)
		select {
		case <-p.ctx.Done():
			t.Stop()
			return nil, errClosed
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(p.ctx, p.d.o.ConnectTimeout)
		l, e := p.open(ctx)
		if e == nil {
			if e = p.rejoin(ctx, l); e != nil {
				l.fail(e)
			}
		}
		cancel()
		if e == nil {
			p.mu.Lock()
			if p.ctx.Err() != nil {
				p.mu.Unlock()
				l.fail(errLinkClosed)
				return nil, errClosed
			}
			p.link = l
			p.mu.Unlock()
			p.log.Info("chromecast: reconnected")
			return l, nil
		}
		err = e
		if p.ctx.Err() != nil {
			return nil, errClosed
		}
	}
	return nil, err
}

// rejoin re-attaches to our receiver app after a reconnect (open has already processed the
// receiver status, which forgets the app when it no longer runs) and refreshes the media
// status. Media that ended while the connection was down is reported as ended (near its end)
// or interrupted.
func (p *player) rejoin(ctx context.Context, l *link) error {
	p.mu.Lock()
	app, transport := p.appSession, p.transport
	p.mu.Unlock()
	if app == "" {
		return nil
	}
	if err := l.sendTo(NamespaceConnection, transport, connectMsg{Type: "CONNECT"}); err != nil {
		return err
	}
	id := p.nextID()
	r, err := l.request(ctx, NamespaceMedia, transport, id, requestMsg{Type: "GET_STATUS", RequestID: id}, p.d.o.RequestTimeout)
	if err != nil {
		return fmt.Errorf("chromecast: media status after reconnect: %w", err)
	}
	var ms mediaStatusMsg
	decode(r.payload, &ms)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mediaSession == 0 {
		return nil
	}
	for _, s := range limitStatus(ms.Status) {
		if s.MediaSessionID == p.mediaSession {
			return nil // still there (onMessage applied it)
		}
	}
	now := time.Now()
	prev := p.snapshotLocked(now)
	p.mediaSession = 0
	p.basePos, p.posAt = prev.Position, time.Time{}
	if p.st.Duration > 0 && prev.Position >= p.st.Duration-finishSlack {
		p.st.State, p.st.Err, p.basePos = renderer.StateEnded, nil, p.st.Duration
	} else if active(p.st.State) {
		p.st.State, p.st.Err = renderer.StateIdle, renderer.ErrInterrupted
	}
	p.notifyIfChangedLocked(prev, now)
	return nil
}

// lose ends the renderer after every reconnect attempt failed.
func (p *player) lose(err error) {
	p.log.Warn("chromecast: connection lost for good", "err", err)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	prev := p.snapshotLocked(now)
	p.link, p.ended = nil, true
	p.basePos, p.posAt = prev.Position, time.Time{}
	p.st.State, p.st.Err = renderer.StateError, fmt.Errorf("chromecast: lost connection: %w", err)
	p.notifyIfChangedLocked(prev, now)
	p.closeChangedLocked()
}

/* ----------------------------------------------------------------- inbound */

// onMessage handles a device message (called from the link's reader goroutine).
func (p *player) onMessage(l *link, m *Message) {
	switch m.Namespace {
	case NamespaceReceiver:
		if m.SourceID != ReceiverID {
			return
		}
		var rs receiverStatusMsg
		if !decode(m.PayloadUTF8, &rs) || rs.Type != "RECEIVER_STATUS" || rs.Status == nil {
			return
		}
		p.onReceiverStatus(&rs)
	case NamespaceMedia:
		var ms mediaStatusMsg
		if !decode(m.PayloadUTF8, &ms) || ms.Type != "MEDIA_STATUS" {
			return
		}
		p.onMediaStatus(m.SourceID, &ms)
	case NamespaceConnection:
		var t typeOnly
		if !decode(m.PayloadUTF8, &t) || t.Type != "CLOSE" {
			return
		}
		if m.SourceID == ReceiverID {
			l.fail(errors.New("chromecast: the device closed the connection"))
			return
		}
		p.mu.Lock()
		if p.transport != "" && m.SourceID == p.transport {
			now := time.Now()
			prev := p.snapshotLocked(now)
			p.appGoneLocked(now)
			p.notifyIfChangedLocked(prev, now)
		}
		p.mu.Unlock()
	}
}

// onReceiverStatus applies RECEIVER_STATUS: volume, and whether our receiver app still runs.
func (p *player) onReceiverStatus(rs *receiverStatusMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	now := time.Now()
	prev := p.snapshotLocked(now)
	if v := rs.Status.Volume; v != nil {
		if lv, ok := level(v.Level); ok {
			p.st.Volume = lv
		}
		if v.Muted != nil {
			p.st.Muted = *v.Muted
		}
	}
	var dmr *application
	apps := rs.Status.Applications
	if len(apps) > maxStatusItems {
		apps = apps[:maxStatusItems]
	}
	for i := range apps {
		a := &apps[i]
		if a.AppID == DefaultMediaReceiverAppID && validID(a.SessionID) && validID(a.TransportID) {
			dmr = a
			break
		}
	}
	switch {
	case p.launching:
		if dmr != nil {
			p.appSession, p.transport = dmr.SessionID, dmr.TransportID
			select {
			case p.appSeen <- struct{}{}:
			default:
			}
		}
	case p.appSession != "":
		if dmr == nil || dmr.SessionID != p.appSession {
			// Another app was launched, or ours was stopped on the device.
			p.appGoneLocked(now)
		} else {
			p.transport = dmr.TransportID
		}
	}
	p.notifyIfChangedLocked(prev, now)
}

// appGoneLocked forgets our receiver app; playing media counts as interrupted.
func (p *player) appGoneLocked(now time.Time) {
	p.appSession, p.transport = "", ""
	p.mediaSession = 0
	if p.loading {
		return // the Load in flight fails on its own
	}
	if active(p.st.State) {
		p.basePos, p.posAt = p.snapshotLocked(now).Position, time.Time{}
		p.st.State, p.st.Err = renderer.StateIdle, renderer.ErrInterrupted
	}
}

// active reports a state in which something is loaded on the device.
func active(state string) bool {
	switch state {
	case renderer.StateLoading, renderer.StateBuffering, renderer.StatePlaying, renderer.StatePaused:
		return true
	}
	return false
}

// limitStatus bounds how many MEDIA_STATUS entries are examined.
func limitStatus(s []mediaStatus) []mediaStatus {
	if len(s) > maxStatusItems {
		return s[:maxStatusItems]
	}
	return s
}

// onMediaStatus applies a MEDIA_STATUS from transport src to our media session. The answer to
// our LOAD introduces the new session.
func (p *player) onMediaStatus(src string, ms *mediaStatusMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended || p.transport == "" || src != p.transport {
		return
	}
	now := time.Now()
	prev := p.snapshotLocked(now)
	statuses := limitStatus(ms.Status)
	if p.loading && p.mediaSession == 0 && ms.RequestID > 0 && ms.RequestID == p.loadReqID {
		for _, s := range statuses {
			if s.MediaSessionID > 0 {
				p.mediaSession = s.MediaSessionID
				break
			}
		}
	}
	if p.mediaSession == 0 {
		return
	}
	for i := range statuses {
		if statuses[i].MediaSessionID == p.mediaSession {
			p.applyLocked(&statuses[i], now)
			break
		}
	}
	p.notifyIfChangedLocked(prev, now)
}

// applyLocked applies one status entry of our media session.
func (p *player) applyLocked(s *mediaStatus, now time.Time) {
	// Re-base the position at now, so extrapolation continues seamlessly unless the device
	// reports a new currentTime.
	p.basePos = p.snapshotLocked(now).Position
	if p.st.State == renderer.StatePlaying {
		p.posAt = now
	}
	if s.Media != nil {
		if d, ok := seconds(s.Media.Duration); ok {
			p.st.Duration = d
		}
	}
	if t, ok := seconds(s.CurrentTime); ok {
		p.basePos = t
	}
	p.rate = rate(s.PlaybackRate)
	switch s.PlayerState {
	case "PLAYING":
		p.st.State, p.st.Err = renderer.StatePlaying, nil
	case "PAUSED":
		p.st.State, p.st.Err = renderer.StatePaused, nil
	case "BUFFERING", "LOADING":
		p.st.State, p.st.Err = renderer.StateBuffering, nil
	case "IDLE":
		switch s.IdleReason {
		case "FINISHED":
			p.st.State, p.st.Err = renderer.StateEnded, nil
			if p.st.Duration > 0 {
				p.basePos = p.st.Duration
			}
		case "ERROR":
			p.st.State, p.st.Err = renderer.StateError, fmt.Errorf("%w (the device reported a playback error)", ErrPlaybackFailed)
		case "CANCELLED", "INTERRUPTED":
			if p.ownStop {
				p.st.State, p.st.Err = renderer.StateIdle, nil
			} else {
				p.st.State, p.st.Err = renderer.StateIdle, renderer.ErrInterrupted
			}
		default:
			// IDLE without a reason: nothing terminal happened (some receivers report it
			// briefly around a load); keep following the session.
			if !p.loading {
				p.st.State, p.st.Err = renderer.StateIdle, nil
			}
			p.posAt = time.Time{}
			return
		}
		p.mediaSession, p.ownStop = 0, false
	default:
		return // unknown state: ignore
	}
	if p.st.State == renderer.StatePlaying {
		p.posAt = now
	} else {
		p.posAt = time.Time{}
	}
}

/* ------------------------------------------------------------------- state */

// snapshotLocked is the status at now, with the position extrapolated while playing.
func (p *player) snapshotLocked(now time.Time) renderer.Status {
	s := p.st
	s.Position = p.basePos
	if s.State == renderer.StatePlaying && !p.posAt.IsZero() {
		if el := now.Sub(p.posAt).Seconds(); el > 0 {
			s.Position += el * p.rate
		}
	}
	if s.Duration > 0 && s.Position > s.Duration {
		s.Position = s.Duration
	}
	if s.Position < 0 || math.IsNaN(s.Position) {
		s.Position = 0
	}
	return s
}

// notifyIfChangedLocked signals Changed when the status differs from prev (both at now):
// state, duration, volume, mute, error, or a position jump of more than a second.
func (p *player) notifyIfChangedLocked(prev renderer.Status, now time.Time) {
	next := p.snapshotLocked(now)
	if prev.State != next.State || prev.Duration != next.Duration || prev.Volume != next.Volume ||
		prev.Muted != next.Muted || errText(prev.Err) != errText(next.Err) || math.Abs(prev.Position-next.Position) > 1 {
		p.notifyLocked()
	}
}

func (p *player) notifyLocked() {
	if p.changedDone {
		return
	}
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *player) closeChangedLocked() {
	if !p.changedDone {
		p.changedDone = true
		close(p.changed)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// current returns the live link, or why there is none.
func (p *player) current() (*link, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return nil, errClosed
	}
	if p.link == nil || !p.link.alive() {
		return nil, errNotConnected
	}
	return p.link, nil
}

/* ---------------------------------------------------------------- renderer */

// Status implements renderer.Renderer.
func (p *player) Status() renderer.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked(time.Now())
}

// Changed implements renderer.Renderer.
func (p *player) Changed() <-chan struct{} { return p.changed }

// LocalAddr implements renderer.Addresser (the current connection's, or the last one's).
func (p *player) LocalAddr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.laddr
}

// RemoteAddr implements renderer.Addresser.
func (p *player) RemoteAddr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.raddr
}

// Load implements renderer.Renderer: it launches the Default Media Receiver when it is not
// running for us yet, then loads req. It returns once the device accepted the media (its
// MEDIA_STATUS answer), not when playback starts. A failed load leaves StateError.
func (p *player) Load(ctx context.Context, req *renderer.LoadRequest) error {
	if req == nil || req.URL == "" {
		return fmt.Errorf("%w: no media URL", renderer.ErrUnsupported)
	}
	p.loadMu.Lock()
	defer p.loadMu.Unlock()
	l, err := p.current()
	if err != nil {
		return err
	}
	p.beginLoad(req)
	err = p.load(ctx, l, req)
	p.endLoad(err)
	return err
}

func (p *player) beginLoad(req *renderer.LoadRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	prev := p.snapshotLocked(now)
	p.loading, p.loadReqID, p.mediaSession, p.ownStop = true, 0, 0, false
	p.live = req.Live
	p.st.State, p.st.Err = renderer.StateLoading, nil
	p.st.Duration = 0
	if d := req.DurationSeconds; d > 0 && d <= maxSeconds {
		p.st.Duration = d
	}
	p.basePos, p.posAt, p.rate = 0, time.Time{}, 1
	if s := req.StartSeconds; s > 0 && s <= maxSeconds {
		p.basePos = s
	}
	p.notifyIfChangedLocked(prev, now)
}

func (p *player) endLoad(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	prev := p.snapshotLocked(now)
	p.loading, p.loadReqID = false, 0
	if err != nil {
		p.mediaSession = 0
		p.posAt = time.Time{}
		p.st.State, p.st.Err = renderer.StateError, err
	} else if p.st.State == renderer.StateLoading {
		p.st.State = renderer.StateBuffering // accepted; the device reports progress next
	}
	p.notifyIfChangedLocked(prev, now)
}

func (p *player) load(ctx context.Context, l *link, req *renderer.LoadRequest) error {
	app, transport, err := p.ensureApp(ctx, l)
	if err != nil {
		return err
	}
	id := p.nextID()
	p.mu.Lock()
	p.loadReqID = id
	p.mu.Unlock()
	p.log.Debug("chromecast: load", "url", redactURL(req.URL), "content_type", req.ContentType,
		"start", req.StartSeconds, "live", req.Live)
	r, err := l.request(ctx, NamespaceMedia, transport, id, buildLoad(id, app, req), p.d.o.RequestTimeout)
	if err != nil {
		return fmt.Errorf("chromecast: load: %w", err)
	}
	var ms mediaStatusMsg
	decode(r.payload, &ms)
	switch r.typ {
	case "MEDIA_STATUS":
		p.mu.Lock()
		state, stErr, session := p.st.State, p.st.Err, p.mediaSession
		p.mu.Unlock()
		switch {
		case state == renderer.StateError && stErr != nil:
			return stErr
		case session == 0 && state != renderer.StateEnded:
			return fmt.Errorf("%w (no media session in the answer)", ErrPlaybackFailed)
		}
		return nil
	case "LOAD_FAILED":
		if ms.DetailedErrorCode != 0 {
			return fmt.Errorf("%w (LOAD_FAILED, code %d)", ErrPlaybackFailed, ms.DetailedErrorCode)
		}
		return fmt.Errorf("%w (LOAD_FAILED)", ErrPlaybackFailed)
	case "LOAD_CANCELLED":
		return errors.New("chromecast: the load was cancelled by a newer request")
	default:
		return fmt.Errorf("chromecast: load refused: %s %s", truncate(r.typ), truncate(ms.Reason))
	}
}

// ensureApp returns our Default Media Receiver session, launching it (and connecting to its
// transport) when it does not run for us.
func (p *player) ensureApp(ctx context.Context, l *link) (string, string, error) {
	p.mu.Lock()
	app, transport := p.appSession, p.transport
	if app == "" {
		p.launching = true
		select {
		case <-p.appSeen:
		default:
		}
	}
	p.mu.Unlock()
	if app != "" {
		return app, transport, nil
	}
	defer func() {
		p.mu.Lock()
		p.launching = false
		p.mu.Unlock()
	}()
	id := p.nextID()
	r, err := l.request(ctx, NamespaceReceiver, ReceiverID, id, launchMsg{Type: "LAUNCH", RequestID: id, AppID: DefaultMediaReceiverAppID}, p.d.o.RequestTimeout)
	if err != nil {
		return "", "", fmt.Errorf("chromecast: launch the media receiver: %w", err)
	}
	if r.typ != "RECEIVER_STATUS" {
		var rs receiverStatusMsg
		decode(r.payload, &rs)
		return "", "", fmt.Errorf("chromecast: launching the media receiver failed: %s %s", truncate(r.typ), truncate(rs.Reason))
	}
	// The answer normally lists the app already; otherwise wait for the status that does.
	timer := time.NewTimer(p.d.o.RequestTimeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		app, transport = p.appSession, p.transport
		p.mu.Unlock()
		if app != "" {
			break
		}
		select {
		case <-p.appSeen:
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-timer.C:
			return "", "", fmt.Errorf("chromecast: launch the media receiver: %w", errTimeout)
		case <-l.done:
			return "", "", l.cause()
		}
	}
	if err := l.sendTo(NamespaceConnection, transport, connectMsg{Type: "CONNECT"}); err != nil {
		return "", "", err
	}
	return app, transport, nil
}

// Play implements renderer.Renderer.
func (p *player) Play(ctx context.Context) error { return p.mediaCmd(ctx, "PLAY", nil) }

// Pause implements renderer.Renderer.
func (p *player) Pause(ctx context.Context) error { return p.mediaCmd(ctx, "PAUSE", nil) }

// Seek implements renderer.Renderer; live items are not seekable.
func (p *player) Seek(ctx context.Context, seconds float64) error {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > maxSeconds {
		return fmt.Errorf("chromecast: invalid seek position %v", seconds)
	}
	return p.mediaCmd(ctx, "SEEK", &seconds)
}

// Stop implements renderer.Renderer: it stops the current media (the receiver app keeps
// running); without media it does nothing.
func (p *player) Stop(ctx context.Context) error { return p.mediaCmd(ctx, "STOP", nil) }

// mediaCmd sends a media command for the current media session and waits for its answer.
func (p *player) mediaCmd(ctx context.Context, typ string, at *float64) error {
	l, err := p.current()
	if err != nil {
		return err
	}
	p.mu.Lock()
	session, transport, live := p.mediaSession, p.transport, p.live
	if typ == "STOP" && session != 0 {
		p.ownStop = true
	}
	p.mu.Unlock()
	if typ == "SEEK" && live {
		return renderer.ErrNotSeekable
	}
	if session == 0 || transport == "" {
		if typ == "STOP" {
			return nil
		}
		return errNoMedia
	}
	id := p.nextID()
	r, err := l.request(ctx, NamespaceMedia, transport, id, mediaCmdMsg{Type: typ, RequestID: id, MediaSessionID: session, CurrentTime: at}, p.d.o.RequestTimeout)
	if err != nil {
		return fmt.Errorf("chromecast: %s: %w", strings.ToLower(typ), err)
	}
	if r.typ != "MEDIA_STATUS" {
		var ms mediaStatusMsg
		decode(r.payload, &ms)
		return fmt.Errorf("chromecast: %s refused: %s %s", strings.ToLower(typ), truncate(r.typ), truncate(ms.Reason))
	}
	if typ == "STOP" {
		// The answer normally reports IDLE/CANCELLED; some receivers just drop the session.
		p.mu.Lock()
		if p.mediaSession == session {
			now := time.Now()
			prev := p.snapshotLocked(now)
			p.mediaSession, p.ownStop = 0, false
			p.basePos, p.posAt = prev.Position, time.Time{}
			p.st.State, p.st.Err = renderer.StateIdle, nil
			p.notifyIfChangedLocked(prev, now)
		}
		p.mu.Unlock()
	}
	return nil
}

// SetVolume implements renderer.Renderer (the device's volume, 0–1).
func (p *player) SetVolume(ctx context.Context, lv float64) error {
	if math.IsNaN(lv) || math.IsInf(lv, 0) {
		return fmt.Errorf("chromecast: invalid volume %v", lv)
	}
	lv = math.Max(0, math.Min(lv, 1))
	return p.setVolume(ctx, volumeLevel{Level: &lv})
}

// SetMuted implements renderer.Renderer.
func (p *player) SetMuted(ctx context.Context, muted bool) error {
	return p.setVolume(ctx, volumeLevel{Muted: &muted})
}

func (p *player) setVolume(ctx context.Context, v volumeLevel) error {
	l, err := p.current()
	if err != nil {
		return err
	}
	id := p.nextID()
	r, err := l.request(ctx, NamespaceReceiver, ReceiverID, id, setVolumeMsg{Type: "SET_VOLUME", RequestID: id, Volume: v}, p.d.o.RequestTimeout)
	if err != nil {
		return fmt.Errorf("chromecast: set volume: %w", err)
	}
	if r.typ != "RECEIVER_STATUS" {
		return fmt.Errorf("chromecast: set volume refused: %s", truncate(r.typ))
	}
	return nil
}

// Close implements renderer.Renderer: it stops our receiver app on the device (best effort,
// briefly), closes the connection and ends every goroutine. It is idempotent.
func (p *player) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		l, app, transport := p.link, p.appSession, p.transport
		p.ended = true
		p.mu.Unlock()
		if l != nil && l.alive() {
			ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			if app != "" {
				id := p.nextID()
				_, _ = l.request(ctx, NamespaceReceiver, ReceiverID, id, stopAppMsg{Type: "STOP", RequestID: id, SessionID: app}, closeTimeout)
			}
			if transport != "" {
				_ = l.sendTo(NamespaceConnection, transport, typeOnly{Type: "CLOSE"})
			}
			_ = l.sendTo(NamespaceConnection, ReceiverID, typeOnly{Type: "CLOSE"})
			cancel()
		}
		p.cancel()
		if l != nil {
			l.fail(errLinkClosed)
		}
		p.wg.Wait()
		p.mu.Lock()
		p.link = nil
		p.closeChangedLocked()
		p.mu.Unlock()
	})
	return nil
}

// truncate shortens device-provided strings for errors and logs.
func truncate(s string) string {
	const max = 64
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.ToValidUTF8(s, "?")
}

var (
	_ renderer.Renderer  = (*player)(nil)
	_ renderer.Addresser = (*player)(nil)
)
