package airplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"ludworks/internal/cast/renderer"
)

// The audio player streams LoadRequest.PCM to a RAOP session in real time.
//
// Timeline. Media frames count from the start of the current PCM source (the item's start
// offset or the last seek target). Every (re)start of streaming is an anchor: at monotonic
// time t0 the sender is at media frame f0 and RTP time r0; packet k of the anchor carries
// media frames f0+352k… and RTP time r0+352k and is due at t0+352k/44100, but goes out
// prebufferFrames early. The receiver plays what the sender sent L+R frames earlier, so the
// audible media frame at time t is f0 + (t−t0)·44100 − (L+R), clamped to what was sent.
//
// Pause stops the sender, FLUSHes the receiver and puts the audio it had buffered but not
// played (from the ring of sent packets) in front of the source; Play anchors again from
// there. Seek FLUSHes and opens a new PCM source at the target. Each anchor starts with a
// sync packet with the extension bit and an audio packet with the marker bit.

const (
	prebufferFrames = 11025 // 0.25 s
	sendTick        = 10 * time.Millisecond
	pcmQueueChunks  = 64 // ≈ 0.5 s read ahead
	maxArtBytes     = 1 << 20
)

// pcmQueue reads a PCM source ahead in a goroutine. Only the sender (or a control
// operation while no sender runs) consumes it.
type pcmQueue struct {
	rc    io.ReadCloser
	ch    chan []byte
	ready chan struct{}
	quit  chan struct{}
	err   error // why ch was closed (io.EOF at the end); read after ch is drained

	pending []byte // pushed-back PCM, delivered before ch
	cur     []byte // rest of the chunk being consumed
	ended   bool
	once    sync.Once
}

func newPCMQueue(rc io.ReadCloser) *pcmQueue {
	q := &pcmQueue{rc: rc, ch: make(chan []byte, pcmQueueChunks), ready: make(chan struct{}, 1), quit: make(chan struct{})}
	go q.read()
	return q
}

func (q *pcmQueue) read() {
	for {
		buf := make([]byte, pcmPacketBytes)
		n, err := io.ReadFull(q.rc, buf)
		n -= n % pcmFrameBytes
		if n > 0 {
			select {
			case q.ch <- buf[:n]:
				q.signal()
			case <-q.quit:
				close(q.ch)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				err = io.EOF
			}
			q.err = err
			close(q.ch)
			q.signal()
			return
		}
	}
}

func (q *pcmQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// waitData blocks until the source has data or ended; false when stop or lost closed first.
func (q *pcmQueue) waitData(stop, lost <-chan struct{}) bool {
	for {
		buf := make([]byte, pcmPacketBytes)
		n, res := q.pull(buf)
		if res != pullStarved {
			q.pending = append(buf[:n:n], q.pending...) // put back what was taken
			return true
		}
		select {
		case <-stop:
			return false
		case <-lost:
			return false
		case <-q.ready:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type pullResult int

const (
	pullOK pullResult = iota
	pullStarved
	pullEnded
)

// pull fills dst (pcmPacketBytes) with the next PCM and returns the bytes filled. A short
// fill happens only at the end of the source.
func (q *pcmQueue) pull(dst []byte) (int, pullResult) {
	n := 0
	for n < len(dst) {
		switch {
		case len(q.pending) > 0:
			c := copy(dst[n:], q.pending)
			q.pending = q.pending[c:]
			n += c
			continue
		case len(q.cur) > 0:
			c := copy(dst[n:], q.cur)
			q.cur = q.cur[c:]
			n += c
			continue
		case q.ended:
			if n > 0 {
				return n, pullOK
			}
			return 0, pullEnded
		}
		select {
		case chunk, ok := <-q.ch:
			if !ok {
				q.ended = true
			}
			q.cur = chunk
		default:
			q.pending = bytes.Clone(dst[:n])
			return 0, pullStarved
		}
	}
	return n, pullOK
}

// failed returns the source's error once it has ended with one.
func (q *pcmQueue) failed() error {
	if q.ended && q.err != nil && !errors.Is(q.err, io.EOF) {
		return q.err
	}
	return nil
}

func (q *pcmQueue) close() {
	q.once.Do(func() {
		close(q.quit)
		q.rc.Close()
	})
}

// audioItem is the loaded item.
type audioItem struct {
	req      *renderer.LoadRequest
	ctx      context.Context // lives with the item; PCM sources run under it
	cancel   context.CancelFunc
	startSec float64 // item position of media frame 0
	q        *pcmQueue

	nextFrame int64 // media frame of the next frame to send
	pausedAt  int64
	eofFrame  int64 // media frames of the source once it ended, else -1
}

type audioPlayer struct {
	r   *airRenderer
	ctl sync.Mutex // serialises control operations

	mu    sync.Mutex // guards the fields below; never held across I/O
	sess  *raopSession
	item  *audioItem
	st    *stream
	state string
	err   error
}

func newAudioPlayer(r *airRenderer) *audioPlayer {
	return &audioPlayer{r: r, state: renderer.StateIdle}
}

func (p *audioPlayer) setState(state string, err error) {
	p.mu.Lock()
	changed := p.state != state || !sameErr(p.err, err)
	p.state, p.err = state, err
	p.mu.Unlock()
	if changed {
		p.r.notify()
	}
}

// fail reports err as StateError and returns it.
func (p *audioPlayer) fail(err error) error {
	p.setState(renderer.StateError, err)
	return err
}

func (p *audioPlayer) load(ctx context.Context, req *renderer.LoadRequest) error {
	if req.PCM == nil {
		return fmt.Errorf("airplay: no PCM source for this item: %w", renderer.ErrUnsupported)
	}
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.stopStream()
	p.dropItem()
	p.setState(renderer.StateLoading, nil)

	p.mu.Lock()
	sess := p.sess
	p.mu.Unlock()
	if sess != nil && sess.usable() {
		if err := sess.flush(ctx); err != nil {
			p.r.log().Debug("airplay: FLUSH before the next item failed", "err", err)
		}
	}
	if sess == nil || !sess.usable() {
		if sess != nil {
			sess.close()
		}
		var err error
		if sess, err = p.r.openRAOP(ctx); err != nil {
			p.mu.Lock()
			p.sess = nil
			p.mu.Unlock()
			return p.fail(err)
		}
		p.mu.Lock()
		p.sess = sess
		p.mu.Unlock()
		go p.watch(sess)
		if level, muted, set := p.r.volumeState(); set {
			if err := sess.setVolume(ctx, volumeDB(level, muted)); err != nil {
				p.r.log().Debug("airplay: volume not applied", "err", err)
			}
		} else if db, ok := sess.getVolume(ctx); ok {
			p.r.learnVolume(db)
		}
	}

	ictx, cancel := context.WithCancel(p.r.ctx)
	rc, err := req.PCM(ictx, req.StartSeconds)
	if err != nil {
		cancel()
		return p.fail(fmt.Errorf("airplay: open PCM: %w", err))
	}
	it := &audioItem{req: req, ctx: ictx, cancel: cancel, startSec: req.StartSeconds, q: newPCMQueue(rc), eofFrame: -1}
	p.mu.Lock()
	p.item = it
	p.mu.Unlock()
	r0 := p.startStream()

	// Item metadata, as far as the receiver wants it (md in the TXT record), stamped with
	// the RTP time of the item's first packet.
	info := p.r.info
	if info.mdText && (req.Title != "" || req.Artist != "" || req.Album != "") {
		if err := sess.setMetadata(ctx, r0, req.Title, req.Artist, req.Album); err != nil {
			p.r.log().Debug("airplay: metadata not accepted", "err", err)
		}
	}
	p.sendProgress(ctx, sess, it, r0, 0)
	if info.mdArt && req.Art != nil {
		go p.sendArt(sess, it, r0)
	}
	return nil
}

// sendProgress tells the receiver where the anchor at RTP time r0 (media frame f0) is in
// the item.
func (p *audioPlayer) sendProgress(ctx context.Context, sess *raopSession, it *audioItem, r0 uint32, f0 int64) {
	if !p.r.info.mdProgress || it.req.DurationSeconds <= 0 || it.req.Live {
		return
	}
	start := r0 - uint32(int64(it.startSec*rtpSampleRate)+f0)
	end := start + uint32(int64(it.req.DurationSeconds*rtpSampleRate))
	if err := sess.setProgress(ctx, start, r0, end); err != nil {
		p.r.log().Debug("airplay: progress not accepted", "err", err)
	}
}

func (p *audioPlayer) sendArt(sess *raopSession, it *audioItem, rtp uint32) {
	ctx, cancel := context.WithTimeout(it.ctx, 15*time.Second)
	defer cancel()
	img, err := it.req.Art(ctx)
	if err != nil || len(img) < 4 || len(img) > maxArtBytes {
		return
	}
	ctype := ""
	switch {
	case bytes.HasPrefix(img, []byte{0xFF, 0xD8, 0xFF}):
		ctype = "image/jpeg"
	case bytes.HasPrefix(img, []byte("\x89PNG")):
		ctype = "image/png"
	default:
		return
	}
	p.mu.Lock()
	current := p.item == it && p.sess == sess
	p.mu.Unlock()
	if current {
		if err := sess.setArtwork(ctx, rtp, ctype, img); err != nil {
			p.r.log().Debug("airplay: artwork not accepted", "err", err)
		}
	}
}

// watch turns a broken RTSP connection into StateError.
func (p *audioPlayer) watch(sess *raopSession) {
	select {
	case <-sess.done:
	case <-sess.lost:
		p.ctl.Lock()
		defer p.ctl.Unlock()
		p.mu.Lock()
		current := p.sess == sess
		p.mu.Unlock()
		if current { // not replaced or stopped meanwhile
			p.stopStream()
			p.setState(renderer.StateError, interrupted("receiver", sess.lostErr))
		}
	}
}

// startStream anchors at the item's next frame and starts the sender; it returns the
// anchor's RTP time. Caller holds ctl, no sender runs.
func (p *audioPlayer) startStream() uint32 {
	p.mu.Lock()
	it, sess := p.item, p.sess
	seq, r0 := sess.next()
	st := &stream{
		p: p, sess: sess, it: it, stop: make(chan struct{}), done: make(chan struct{}),
		t0: time.Now(), f0: it.nextFrame, r0: r0, seq0: seq, lat: int64(sess.latency + sess.recvLatency),
	}
	p.st = st
	p.mu.Unlock()
	p.setState(renderer.StateBuffering, nil)
	go st.run()
	return r0
}

// stopStream stops the sender and returns it (nil when none ran). Caller holds ctl.
func (p *audioPlayer) stopStream() *stream {
	p.mu.Lock()
	st := p.st
	p.st = nil
	p.mu.Unlock()
	if st != nil {
		close(st.stop)
		<-st.done
	}
	return st
}

// dropItem releases the current item. Caller holds ctl, no sender runs.
func (p *audioPlayer) dropItem() {
	p.mu.Lock()
	it := p.item
	p.item = nil
	p.mu.Unlock()
	if it != nil {
		it.q.close()
		it.cancel()
	}
}

func (p *audioPlayer) pause(ctx context.Context) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	st := p.stopStream()
	if st == nil {
		return nil // paused, ended, idle: nothing streams
	}
	audible := st.audibleFrame(time.Now())
	it := st.it
	it.q.pending = append(st.sess.unplayed(st.seq0, audible), it.q.pending...)
	it.nextFrame, it.pausedAt = audible, audible
	p.setState(renderer.StatePaused, nil)
	if err := st.sess.flush(ctx); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *audioPlayer) play(ctx context.Context) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.mu.Lock()
	state, it, sess := p.state, p.item, p.sess
	p.mu.Unlock()
	if state != renderer.StatePaused || it == nil || sess == nil {
		return nil
	}
	if !sess.usable() {
		return p.fail(fmt.Errorf("airplay: connection to the receiver lost: %w", errConnDead))
	}
	r0 := p.startStream()
	p.sendProgress(ctx, sess, it, r0, it.nextFrame)
	return nil
}

func (p *audioPlayer) seek(ctx context.Context, seconds float64) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.mu.Lock()
	state, it, sess := p.state, p.item, p.sess
	p.mu.Unlock()
	if it == nil || sess == nil {
		return fmt.Errorf("airplay: nothing loaded")
	}
	if it.req.Live {
		return renderer.ErrNotSeekable
	}
	seconds = max(seconds, 0)
	if d := it.req.DurationSeconds; d > 0 {
		seconds = min(seconds, d)
	}
	p.stopStream()
	if err := sess.flush(ctx); err != nil {
		return p.fail(err)
	}
	rc, err := it.req.PCM(it.ctx, seconds)
	if err != nil {
		return p.fail(fmt.Errorf("airplay: open PCM: %w", err))
	}
	it.q.close()
	p.mu.Lock()
	it.q = newPCMQueue(rc)
	it.startSec, it.nextFrame, it.pausedAt, it.eofFrame = seconds, 0, 0, -1
	p.mu.Unlock()
	if state == renderer.StatePaused {
		p.r.notify()
		return nil
	}
	r0 := p.startStream()
	p.sendProgress(ctx, sess, it, r0, 0)
	return nil
}

// stop ends playback and the RAOP session (TEARDOWN).
func (p *audioPlayer) stop(ctx context.Context) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.stopStream()
	p.dropItem()
	p.mu.Lock()
	sess := p.sess
	p.sess = nil
	p.mu.Unlock()
	if sess != nil {
		sess.teardown(ctx)
	}
	p.setState(renderer.StateIdle, nil)
	return nil
}

func (p *audioPlayer) setVolume(ctx context.Context, level float64, muted bool) error {
	p.mu.Lock()
	sess := p.sess
	p.mu.Unlock()
	if sess == nil || !sess.usable() {
		return nil // applied when the next session starts
	}
	return sess.setVolume(ctx, volumeDB(level, muted))
}

// status returns the player's state and position.
func (p *audioPlayer) status() (state string, pos, dur float64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, err = p.state, p.err
	it := p.item
	if it == nil {
		return state, 0, 0, err
	}
	dur = it.req.DurationSeconds
	if it.req.Live {
		dur = 0
	}
	frame := it.pausedAt
	switch {
	case p.st != nil:
		frame = p.st.audibleFrame(time.Now())
	case state == renderer.StateEnded && it.eofFrame >= 0:
		frame = it.eofFrame
	}
	return state, it.startSec + float64(frame)/rtpSampleRate, dur, err
}

// stream is one run of the sender, from an anchor until stopped or the item ended.
type stream struct {
	p    *audioPlayer
	sess *raopSession
	it   *audioItem
	stop chan struct{}
	done chan struct{}

	t0   time.Time
	f0   int64  // media frame at the anchor
	r0   uint32 // RTP time at the anchor
	seq0 uint16 // first sequence number of the anchor
	lat  int64  // L+R in frames

	mu      sync.Mutex // guards sent and media (read by status)
	sent    int64      // frames sent since the anchor, padding included
	media   int64      // media frames sent since the anchor
	padding int64      // silence frames sent after the end of the source
}

// audibleFrame is the media frame the receiver plays at time t.
func (st *stream) audibleFrame(t time.Time) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	played := int64(t.Sub(st.t0).Seconds()*rtpSampleRate) - st.lat
	return st.f0 + min(max(played, 0), st.media)
}

func (st *stream) run() {
	defer close(st.done)
	p, sess, it := st.p, st.sess, st.it
	marker, firstSync := true, true
	nextSync := int64(0)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		now := time.Now()
		clock := int64(now.Sub(st.t0).Seconds() * rtpSampleRate)
		if clock >= nextSync {
			// Syncs map whole seconds after the anchor: the first one is exactly the
			// anchor (RTP time r0 at t0), as receivers expect.
			at := st.t0.Add(time.Duration(nextSync) * time.Second / rtpSampleRate)
			sess.sendSync(firstSync, at, st.r0+uint32(nextSync))
			if !firstSync {
				p.r.notify() // position moved
			}
			firstSync = false
			nextSync = (clock/rtpSampleRate + 1) * rtpSampleRate
		}

		starved := false
		for st.sent < clock+prebufferFrames {
			pcm := make([]byte, pcmPacketBytes)
			n, res := it.q.pull(pcm)
			if res == pullStarved {
				starved = true
				break
			}
			frames := n / pcmFrameBytes
			if res == pullEnded {
				if err := it.q.failed(); err != nil {
					p.streamFailed(st, fmt.Errorf("airplay: PCM source: %w", err))
					return
				}
				p.mu.Lock()
				it.eofFrame = it.nextFrame
				p.mu.Unlock()
				if st.padding >= st.lat+prebufferFrames {
					break // the receiver has enough silence to play out the end
				}
				frames = 0
				st.padding += rtpFramesPerPacket
			}
			sess.sendAudio(marker, pcm, it.nextFrame, frames)
			marker = false
			st.mu.Lock()
			st.sent += rtpFramesPerPacket
			st.media += int64(frames)
			st.mu.Unlock()
			p.mu.Lock()
			it.nextFrame += int64(frames)
			p.mu.Unlock()
		}

		played := clock - st.lat
		if played >= 0 {
			p.streamPlaying(st)
		}
		if eof := it.eofFrame; eof >= 0 && st.f0+played >= eof && it.q.ended && len(it.q.pending) == 0 {
			p.streamEnded(st)
			return
		}
		if starved && st.sent < clock-st.lat/2 {
			// The source fell behind what the receiver is already playing: wait for data,
			// then anchor again (FLUSH, marker, sync with the extension bit).
			p.streamBuffering(st)
			if !it.q.waitData(st.stop, sess.lost) {
				return
			}
			if st.sent > 0 {
				ctx, cancel := context.WithTimeout(it.ctx, exchangeTimeout)
				err := sess.flush(ctx)
				cancel()
				if err != nil {
					p.streamFailed(st, err)
					return
				}
			}
			seq, r := sess.next()
			st.mu.Lock()
			st.t0, st.f0, st.r0, st.seq0 = time.Now(), it.nextFrame, r, seq
			st.sent, st.media, st.padding = 0, 0, 0
			st.mu.Unlock()
			marker, firstSync, nextSync = true, true, 0
			continue
		}

		timer.Reset(sendTick) // no stale fires since Go 1.23
		select {
		case <-st.stop:
			return
		case <-sess.lost:
			return
		case <-timer.C:
		case <-it.q.ready:
		}
	}
}

// The sender reports through these; they ignore a sender that is no longer current.

func (p *audioPlayer) streamPlaying(st *stream) {
	p.mu.Lock()
	ok := p.st == st && p.state != renderer.StatePlaying
	if ok {
		p.state, p.err = renderer.StatePlaying, nil
	}
	p.mu.Unlock()
	if ok {
		p.r.notify()
	}
}

func (p *audioPlayer) streamBuffering(st *stream) {
	p.mu.Lock()
	ok := p.st == st && p.state != renderer.StateBuffering
	if ok {
		p.state = renderer.StateBuffering
	}
	p.mu.Unlock()
	if ok {
		p.r.notify()
	}
}

func (p *audioPlayer) streamEnded(st *stream) {
	p.mu.Lock()
	ok := p.st == st
	if ok {
		p.st = nil
		p.state, p.err = renderer.StateEnded, nil
	}
	p.mu.Unlock()
	if ok {
		p.r.notify()
	}
}

func (p *audioPlayer) streamFailed(st *stream, err error) {
	p.mu.Lock()
	ok := p.st == st
	if ok {
		p.st = nil
		p.state, p.err = renderer.StateError, err
	}
	p.mu.Unlock()
	if ok {
		p.r.notify()
	}
}
