package airplay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"ludworks/internal/cast/renderer"
)

// airRenderer is a connected AirPlay device: audio items go to the RAOP player, video items
// to the /play player. One of them is active at a time.
type airRenderer struct {
	d     *Driver
	info  *deviceInfo
	creds *credentials

	ctx     context.Context // until Close; PCM sources run under it
	cancel  context.CancelFunc
	changed chan struct{}

	mu           sync.Mutex
	closed       bool
	active       string // "audio", "video" or ""
	volume       float64
	muted        bool
	volumeSet    bool
	laddr, raddr net.Addr // of the latest connection to the device (renderer.Addresser)

	audio *audioPlayer
	video *videoPlayer
}

var errClosed = errors.New("airplay: renderer closed")

func newAirRenderer(d *Driver, info *deviceInfo, creds *credentials, laddr, raddr net.Addr) *airRenderer {
	ctx, cancel := context.WithCancel(context.Background())
	r := &airRenderer{d: d, info: info, creds: creds, ctx: ctx, cancel: cancel, changed: make(chan struct{}, 1), volume: 1,
		laddr: laddr, raddr: raddr}
	r.audio = newAudioPlayer(r)
	r.video = newVideoPlayer(r)
	return r
}

// LocalAddr implements renderer.Addresser: this server's address on the device's network.
func (r *airRenderer) LocalAddr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.laddr
}

// RemoteAddr implements renderer.Addresser: the device's address (the one that fetches
// video URLs).
func (r *airRenderer) RemoteAddr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.raddr
}

func (r *airRenderer) setAddrs(c *msgConn) {
	r.mu.Lock()
	r.laddr, r.raddr = c.raw.LocalAddr(), c.raw.RemoteAddr()
	r.mu.Unlock()
}

// interrupted classifies the loss of a device connection: the device closing it (another
// sender took over, or its own controls ended the session) is renderer.ErrInterrupted;
// timeouts and other failures are plain connection errors.
func interrupted(what string, err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("airplay: connection to the %s lost: %w", what, err)
	}
	return fmt.Errorf("airplay: the %s closed the connection (%w): %w", what, err, renderer.ErrInterrupted)
}

func (r *airRenderer) log() *slog.Logger { return r.d.opts.Log }

// notify signals Changed (coalesced, never blocking).
func (r *airRenderer) notify() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *airRenderer) volumeState() (level float64, muted, set bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.volume, r.muted, r.volumeSet
}

// learnVolume takes the receiver's own volume (dB, −144 = mute) unless one was set.
func (r *airRenderer) learnVolume(db float64) {
	r.mu.Lock()
	if r.volumeSet {
		r.mu.Unlock()
		return
	}
	if db <= -144 {
		r.muted = true
	} else {
		r.volume, r.muted = min(max((db+30)/30, 0), 1), false
	}
	r.mu.Unlock()
	r.notify()
}

// openRAOP starts a RAOP session; an et=4 receiver refusing ANNOUNCE gets a second try
// with auth-setup.
func (r *airRenderer) openRAOP(ctx context.Context) (*raopSession, error) {
	s, err := startRAOP(ctx, r.d, r.info, false)
	if errors.Is(err, errRAOPForbidden) && r.info.encMFi && !r.info.authSetupFirst() {
		s, err = startRAOP(ctx, r.d, r.info, true)
	}
	return s, err
}

// activate makes kind the active player, stopping the other one.
func (r *airRenderer) activate(ctx context.Context, kind string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errClosed
	}
	prev := r.active
	r.active = kind
	r.mu.Unlock()
	switch {
	case prev == "audio" && kind != "audio":
		return r.audio.stop(ctx)
	case prev == "video" && kind != "video":
		return r.video.stop(ctx)
	}
	return nil
}

func (r *airRenderer) current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ""
	}
	return r.active
}

// Load implements renderer.Renderer.
func (r *airRenderer) Load(ctx context.Context, req *renderer.LoadRequest) error {
	if req == nil {
		return fmt.Errorf("airplay: no item: %w", renderer.ErrUnsupported)
	}
	if req.Video {
		if !r.info.video() {
			return fmt.Errorf("airplay: the device plays no video: %w", renderer.ErrUnsupported)
		}
		if err := r.activate(ctx, "video"); err != nil {
			return err
		}
		return r.video.load(ctx, req)
	}
	if !r.info.audio() {
		return fmt.Errorf("airplay: the device takes no RAOP audio: %w", renderer.ErrUnsupported)
	}
	if req.PCM == nil {
		return fmt.Errorf("airplay: no PCM source for this item: %w", renderer.ErrUnsupported)
	}
	if err := r.activate(ctx, "audio"); err != nil {
		return err
	}
	return r.audio.load(ctx, req)
}

// Play implements renderer.Renderer.
func (r *airRenderer) Play(ctx context.Context) error {
	switch r.current() {
	case "audio":
		return r.audio.play(ctx)
	case "video":
		return r.video.play(ctx)
	}
	return nil
}

// Pause implements renderer.Renderer.
func (r *airRenderer) Pause(ctx context.Context) error {
	switch r.current() {
	case "audio":
		return r.audio.pause(ctx)
	case "video":
		return r.video.pause(ctx)
	}
	return nil
}

// Seek implements renderer.Renderer.
func (r *airRenderer) Seek(ctx context.Context, seconds float64) error {
	switch r.current() {
	case "audio":
		return r.audio.seek(ctx, seconds)
	case "video":
		return r.video.seek(ctx, seconds)
	}
	return fmt.Errorf("airplay: nothing loaded")
}

// Stop implements renderer.Renderer.
func (r *airRenderer) Stop(ctx context.Context) error {
	switch r.current() {
	case "audio":
		return r.audio.stop(ctx)
	case "video":
		return r.video.stop(ctx)
	}
	return nil
}

// SetVolume implements renderer.Renderer.
func (r *airRenderer) SetVolume(ctx context.Context, level float64) error {
	level = min(max(level, 0), 1)
	r.mu.Lock()
	r.volume, r.volumeSet = level, true
	muted := r.muted
	r.mu.Unlock()
	r.notify()
	return r.applyVolume(ctx, level, muted)
}

// SetMuted implements renderer.Renderer.
func (r *airRenderer) SetMuted(ctx context.Context, muted bool) error {
	r.mu.Lock()
	r.muted, r.volumeSet = muted, true
	level := r.volume
	r.mu.Unlock()
	r.notify()
	return r.applyVolume(ctx, level, muted)
}

func (r *airRenderer) applyVolume(ctx context.Context, level float64, muted bool) error {
	switch r.current() {
	case "audio":
		return r.audio.setVolume(ctx, level, muted)
	case "video":
		return r.video.setVolume(ctx, level, muted)
	}
	return nil
}

// Status implements renderer.Renderer.
func (r *airRenderer) Status() renderer.Status {
	r.mu.Lock()
	active, closed := r.active, r.closed
	st := renderer.Status{State: renderer.StateIdle, Volume: r.volume, Muted: r.muted}
	r.mu.Unlock()
	if closed {
		return st
	}
	switch active {
	case "audio":
		st.State, st.Position, st.Duration, st.Err = r.audio.status()
	case "video":
		st.State, st.Position, st.Duration, st.Err = r.video.status()
	}
	return st
}

// Changed implements renderer.Renderer.
func (r *airRenderer) Changed() <-chan struct{} { return r.changed }

// Close implements renderer.Renderer: it ends playback (TEARDOWN, /stop) and disconnects.
func (r *airRenderer) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.audio.stop(ctx)
	r.video.stop(ctx)
	r.cancel()
	r.mu.Lock()
	close(r.changed)
	r.mu.Unlock()
	return nil
}

// sameErr compares errors by identity or message (errors may hold uncomparable values).
func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

var (
	_ renderer.Renderer  = (*airRenderer)(nil)
	_ renderer.Addresser = (*airRenderer)(nil)
)
