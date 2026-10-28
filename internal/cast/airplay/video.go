package airplay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"ludworks/internal/cast/renderer"
)

// AirPlay video: the device fetches the media URL itself (POST /play) and is controlled with
// POST /rate?value=0|1, POST /scrub?position=s and POST /stop; GET /playback-info (binary
// plist) is polled once a second for the state. The connection must stay open while the
// video plays.
//
// Before /play the connection is authenticated: pair-verify with stored credentials, else
// transient pair-setup when the device supports it, else nothing (legacy receivers that
// allow anyone). Receivers with AirPlay 2 features additionally need an AirPlay 2 session:
// RTSP SETUP (binary plist with our NTP timing port), an encrypted event connection to the
// port the device names, RECORD, and POST /feedback every 2 s.

const (
	videoPollInterval = time.Second
	videoStartTimeout = 30 // polls without the video starting
	// videoEndSlack: playback that disappears more than this before the end (polled once a
	// second, plus seeks) was stopped on the device rather than finished.
	videoEndSlack   = 5.0
	videoUserAgent  = "AirPlay/550.10"
	legacyUserAgent = "MediaControl/1.0"
	bplistType      = "application/x-apple-binary-plist"
)

type videoSession struct {
	log     *slog.Logger
	conn    *msgConn
	modern  bool   // AirPlay 2 session (SETUP/RECORD) under the /play
	uuid    string // X-Apple-Session-ID
	url     string // rtsp URL of the AirPlay 2 session
	timing  *net.UDPConn
	events  net.Conn
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	running bool // /play accepted
}

// openVideo connects and authenticates; for AirPlay 2 receivers it also sets up the
// session /play needs.
func openVideo(ctx context.Context, d *Driver, info *deviceInfo, creds *credentials) (_ *videoSession, err error) {
	if info.password || info.accessHome {
		return nil, fmt.Errorf("airplay: the device requires a password or Home membership: %w", renderer.ErrPairingUnsupported)
	}
	conn, err := dialMsgConn(ctx, info.ap.Host, info.ap.Port, []hdr{{"X-Apple-Client-Name", d.name}})
	if err != nil {
		return nil, err
	}
	v := &videoSession{log: d.opts.Log, conn: conn, uuid: newUUID(), done: make(chan struct{})}
	defer func() {
		if err != nil {
			v.close()
		}
	}()

	var secret []byte
	switch {
	case creds != nil:
		if secret, err = pairVerify(ctx, conn, creds); err != nil {
			return nil, err
		}
	case info.transient():
		if secret, err = transientPairSetup(ctx, conn); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			return nil, fmt.Errorf("airplay: unpaired access refused (%w): %w", err, renderer.ErrPairingRequired)
		}
	case info.videoNeedsPairing() && info.hap():
		return nil, renderer.ErrPairingRequired
	case info.videoNeedsPairing():
		return nil, fmt.Errorf("airplay: the device needs a legacy pairing: %w", renderer.ErrPairingUnsupported)
	}
	if secret != nil {
		if err := encryptControl(conn, secret); err != nil {
			return nil, err
		}
	}
	if secret == nil || !info.airplay2() {
		return v, nil
	}

	// AirPlay 2 session.
	v.modern = true
	local := conn.localAddr()
	v.url = "rtsp://" + urlHost(local.IP) + "/" + strconv.FormatUint(uint64(randUint32()), 10)
	if v.timing, err = net.ListenUDP("udp", &net.UDPAddr{IP: local.IP, Zone: local.Zone}); err != nil {
		return nil, fmt.Errorf("airplay: timing port: %w", err)
	}
	v.wg.Add(1)
	go func() { defer v.wg.Done(); serveTiming(v.timing, conn.remoteIP()) }()
	setup, err := encodePlist(map[string]any{
		"deviceID":                 d.deviceID,
		"macAddress":               d.deviceID,
		"sessionUUID":              newUUID(),
		"timingPort":               int64(v.timing.LocalAddr().(*net.UDPAddr).Port),
		"timingProtocol":           "NTP",
		"isMultiSelectAirPlay":     true,
		"groupContainsGroupLeader": false,
		"senderSupportsRelay":      false,
		"statsCollectionEnabled":   false,
		"name":                     d.name,
		"model":                    "iPhone14,3",
		"osName":                   "iPhone OS",
		"osVersion":                "16.5",
		"osBuildVersion":           "20F66",
		"sourceVersion":            "690.7.1",
	})
	if err != nil {
		return nil, err
	}
	resp, err := conn.do(ctx, "SETUP", v.url, rtspProto, v.headers(bplistType), setup)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, fmt.Errorf("airplay: SETUP: status %d: %w", resp.status, errVideoStatus(resp.status))
	}
	if port, ok := plistPort(resp.body, "eventPort"); ok {
		if err := v.openEvents(ctx, secret, port); err != nil {
			return nil, err
		}
	}
	if resp, err = conn.do(ctx, "RECORD", v.url, rtspProto, v.headers(""), nil); err != nil {
		return nil, err
	} else if resp.status != 200 {
		return nil, fmt.Errorf("airplay: RECORD: status %d: %w", resp.status, errVideoStatus(resp.status))
	}
	if resp, err := conn.do(ctx, "POST", "/feedback", rtspProto, v.headers(""), nil); err != nil {
		return nil, err
	} else if resp.status == 200 {
		v.wg.Add(1)
		go func() { defer v.wg.Done(); v.feedback() }()
	}
	return v, nil
}

// errVideoStatus classifies a failed AirPlay video status.
func errVideoStatus(status int) error {
	switch status {
	case 401, 403, 470:
		return renderer.ErrPairingRequired
	case 415, 501:
		return renderer.ErrUnsupported
	}
	return errProtocol
}

func plistPort(body []byte, key string) (int, bool) {
	v, err := decodePlist(body)
	if err != nil {
		return 0, false
	}
	m, _ := v.(map[string]any)
	n, ok := plistNumber(m[key])
	if !ok || n <= 0 || n > 65535 {
		return 0, false
	}
	return int(n), true
}

// plistNumber reads an integer or real plist value.
func plistNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func (v *videoSession) headers(ctype string) []hdr {
	h := []hdr{{"User-Agent", videoUserAgent}, {"X-Apple-Session-ID", v.uuid}}
	if !v.modern {
		h[0].v = legacyUserAgent
	}
	if ctype != "" {
		h = append(h, hdr{"Content-Type", ctype})
	}
	if v.modern {
		h = append(h, hdr{"X-Apple-ProtocolVersion", "1"}, hdr{"X-Apple-Stream-ID", "1"})
	}
	return h
}

// openEvents connects the AirPlay 2 event channel and acknowledges whatever the device
// sends on it.
func (v *videoSession) openEvents(ctx context.Context, secret []byte, port int) error {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(v.conn.remoteIP().String(), strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("airplay: event channel: %w", err)
	}
	// The device writes events: our read key is its write key.
	sc, err := newSecureConn(c,
		hkdfKey(secret, "Events-Salt", "Events-Read-Encryption-Key"),
		hkdfKey(secret, "Events-Salt", "Events-Write-Encryption-Key"))
	if err != nil {
		c.Close()
		return err
	}
	v.events = sc
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		defer sc.Close()
		br := bufio.NewReaderSize(sc, maxHeaderLine)
		for {
			req, err := readMessage(br, false)
			if err != nil {
				return
			}
			resp := &message{proto: req.proto, status: 200, reason: "OK", header: []hdr{{"Audio-Latency", "0"}}}
			if cs := req.get("CSeq"); cs != "" {
				resp.header = append(resp.header, hdr{"CSeq", cs})
			}
			sc.SetWriteDeadline(time.Now().Add(exchangeTimeout))
			if writeMessage(sc, resp) != nil {
				return
			}
		}
	}()
	return nil
}

func (v *videoSession) feedback() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
		_, err := v.conn.do(ctx, "POST", "/feedback", rtspProto, v.headers(""), nil)
		cancel()
		if err != nil {
			return
		}
	}
}

// play sends /play.
func (v *videoSession) play(ctx context.Context, req *renderer.LoadRequest) error {
	body := map[string]any{
		"Content-Location":       req.URL,
		"Start-Position-Seconds": max(req.StartSeconds, 0),
		"uuid":                   newUUID(),
	}
	if v.modern {
		body["streamType"] = int64(1)
		body["mediaType"] = "file"
		body["rate"] = 1.0
		body["volume"] = 1.0
		body["clientProcName"] = "ludworks"
	} else {
		start := 0.0
		if req.DurationSeconds > 0 {
			start = min(max(req.StartSeconds/req.DurationSeconds, 0), 1)
		}
		body["Start-Position"] = start
	}
	b, err := encodePlist(body)
	if err != nil {
		return err
	}
	var resp *message
	for attempt := 0; ; attempt++ {
		if resp, err = v.conn.do(ctx, "POST", "/play", "HTTP/1.1", v.headers(bplistType), b); err != nil {
			return err
		}
		// Apple TVs sometimes answer 500 to a /play that works on the next try.
		if resp.status != 500 || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if resp.status != 200 {
		return fmt.Errorf("airplay: /play: status %d: %w", resp.status, errVideoStatus(resp.status))
	}
	v.running = true
	if v.modern { // AirPlay 2 receivers start paused otherwise
		return v.control(ctx, "/rate?value=1.000000")
	}
	return nil
}

// control posts a playback command.
func (v *videoSession) control(ctx context.Context, uri string) error {
	proto := "HTTP/1.1"
	if v.modern {
		proto = rtspProto
	}
	resp, err := v.conn.do(ctx, "POST", uri, proto, v.headers(""), nil)
	if err != nil {
		return err
	}
	if resp.status != 200 {
		return fmt.Errorf("airplay: %s: status %d: %w", uri, resp.status, errVideoStatus(resp.status))
	}
	return nil
}

// playbackInfo fetches and decodes /playback-info.
func (v *videoSession) playbackInfo(ctx context.Context) (map[string]any, error) {
	resp, err := v.conn.do(ctx, "GET", "/playback-info", "HTTP/1.1", v.headers(""), nil)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, fmt.Errorf("airplay: /playback-info: status %d: %w", resp.status, errProtocol)
	}
	if len(resp.body) == 0 {
		return map[string]any{}, nil
	}
	pl, err := decodePlist(resp.body)
	if err != nil {
		return nil, err
	}
	m, ok := pl.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: /playback-info is not a dictionary", errPlist)
	}
	return m, nil
}

func (v *videoSession) setVolume(ctx context.Context, level float64, muted bool) error {
	if !v.modern {
		return fmt.Errorf("airplay: volume of a video: %w", renderer.ErrUnsupported)
	}
	resp, err := v.conn.do(ctx, "SET_PARAMETER", v.url, rtspProto, v.headers("text/parameters"),
		[]byte("volume: "+strconv.FormatFloat(volumeDB(level, muted), 'f', 6, 64)+"\r\n"))
	if err != nil {
		return err
	}
	if resp.status != 200 {
		return fmt.Errorf("airplay: volume of a video: status %d: %w", resp.status, renderer.ErrUnsupported)
	}
	return nil
}

// stop ends playback and the session.
func (v *videoSession) stop(ctx context.Context) {
	if v.conn.alive() {
		if v.running {
			if err := v.control(ctx, "/stop"); err != nil {
				v.log.Debug("airplay: /stop failed", "err", err)
			}
		}
		if v.modern {
			v.conn.do(ctx, "TEARDOWN", v.url, rtspProto, v.headers(""), nil)
		}
	}
	v.close()
}

func (v *videoSession) close() {
	v.once.Do(func() {
		close(v.done)
		v.conn.close()
		if v.timing != nil {
			v.timing.Close()
		}
		if v.events != nil {
			v.events.Close()
		}
	})
	v.wg.Wait()
}

// videoPlayer drives one video item.
type videoPlayer struct {
	r   *airRenderer
	ctl sync.Mutex

	mu        sync.Mutex
	sess      *videoSession
	req       *renderer.LoadRequest
	state     string
	err       error
	pos, dur  float64
	pollStop  chan struct{}
	pollDone  chan struct{}
	startSeek bool // a legacy start position still to apply
}

func newVideoPlayer(r *airRenderer) *videoPlayer {
	return &videoPlayer{r: r, state: renderer.StateIdle}
}

func (p *videoPlayer) set(f func()) {
	p.mu.Lock()
	state, err, pos, dur := p.state, p.err, p.pos, p.dur
	f()
	changed := state != p.state || !sameErr(err, p.err) || pos != p.pos || dur != p.dur
	p.mu.Unlock()
	if changed {
		p.r.notify()
	}
}

func (p *videoPlayer) load(ctx context.Context, req *renderer.LoadRequest) error {
	if req.URL == "" {
		return fmt.Errorf("airplay: video item without URL: %w", renderer.ErrUnsupported)
	}
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.end(ctx)
	p.set(func() {
		p.state, p.err, p.pos, p.dur, p.req = renderer.StateLoading, nil, req.StartSeconds, req.DurationSeconds, req
	})
	sess, err := openVideo(ctx, p.r.d, p.r.info, p.r.creds)
	if err == nil {
		err = sess.play(ctx, req)
		if err != nil {
			sess.close()
		}
	}
	if err != nil {
		p.set(func() { p.state, p.err = renderer.StateError, err })
		return err
	}
	p.r.setAddrs(sess.conn)
	stop, done := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.sess, p.pollStop, p.pollDone = sess, stop, done
	p.startSeek = !sess.modern && req.StartSeconds > 0 && req.DurationSeconds <= 0
	p.mu.Unlock()
	go p.poll(sess, stop, done)
	return nil
}

// end stops polling and the session. Caller holds ctl.
func (p *videoPlayer) end(ctx context.Context) {
	p.mu.Lock()
	sess, stop, done := p.sess, p.pollStop, p.pollDone
	p.sess, p.pollStop, p.pollDone = nil, nil, nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	if sess != nil {
		sess.stop(ctx)
	}
}

func (p *videoPlayer) poll(sess *videoSession, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(videoPollInterval)
	defer t.Stop()
	started, waited := false, 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
		info, err := sess.playbackInfo(ctx)
		cancel()
		if err != nil {
			if !sess.conn.alive() {
				p.set(func() { p.state, p.err = renderer.StateError, interrupted("device", err) })
				return
			}
			continue
		}
		if e, ok := info["error"]; ok {
			p.set(func() {
				p.state, p.err = renderer.StateError, fmt.Errorf("airplay: the device cannot play the video (%v)", e)
			})
			return
		}
		dur, hasDur := plistNumber(info["duration"])
		if !hasDur {
			if started {
				// Playback is gone: the end of the video, or stopped on the TV well before it.
				p.set(func() {
					if p.dur > 0 && p.pos < p.dur-videoEndSlack && p.req != nil && !p.req.Live {
						p.state, p.err = renderer.StateIdle, fmt.Errorf("airplay: video stopped on the device: %w", renderer.ErrInterrupted)
					} else {
						p.state, p.err, p.pos = renderer.StateEnded, nil, max(p.pos, p.dur)
					}
				})
				return
			}
			if waited++; waited > videoStartTimeout {
				p.set(func() { p.state, p.err = renderer.StateError, errors.New("airplay: the video did not start") })
				return
			}
			continue
		}
		started = true
		pos, _ := plistNumber(info["position"])
		rate, _ := plistNumber(info["rate"])
		ready, _ := info["readyToPlay"].(bool)
		empty, _ := info["playbackBufferEmpty"].(bool)
		keepUp, hasKeepUp := info["playbackLikelyToKeepUp"].(bool)
		state := renderer.StatePlaying
		switch {
		case !ready:
			state = renderer.StateLoading
		case rate == 0:
			state = renderer.StatePaused
		case empty && (!hasKeepUp || !keepUp):
			state = renderer.StateBuffering
		}
		p.mu.Lock()
		seek, req := p.startSeek, p.req
		if seek && ready {
			p.startSeek = false
		}
		p.mu.Unlock()
		if seek && ready && req != nil && pos+2 < req.StartSeconds {
			ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
			sess.control(ctx, "/scrub?position="+strconv.FormatFloat(req.StartSeconds, 'f', 6, 64))
			cancel()
		}
		p.set(func() {
			p.state, p.err, p.pos = state, nil, max(pos, 0)
			if dur > 0 && p.req != nil && !p.req.Live {
				p.dur = dur
			}
		})
	}
}

// command runs a control request on the current session.
func (p *videoPlayer) command(ctx context.Context, uri string, after func()) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.mu.Lock()
	sess, state := p.sess, p.state
	p.mu.Unlock()
	if sess == nil || state == renderer.StateEnded {
		return nil
	}
	if err := sess.control(ctx, uri); err != nil {
		return err
	}
	if after != nil {
		p.set(after)
	}
	return nil
}

func (p *videoPlayer) pause(ctx context.Context) error {
	return p.command(ctx, "/rate?value=0.000000", func() {
		if p.state == renderer.StatePlaying || p.state == renderer.StateBuffering {
			p.state = renderer.StatePaused
		}
	})
}

func (p *videoPlayer) play(ctx context.Context) error {
	return p.command(ctx, "/rate?value=1.000000", func() {
		if p.state == renderer.StatePaused {
			p.state = renderer.StatePlaying
		}
	})
}

func (p *videoPlayer) seek(ctx context.Context, seconds float64) error {
	p.mu.Lock()
	req := p.req
	p.mu.Unlock()
	if req != nil && req.Live {
		return renderer.ErrNotSeekable
	}
	seconds = max(seconds, 0)
	return p.command(ctx, "/scrub?position="+strconv.FormatFloat(seconds, 'f', 6, 64), func() { p.pos = seconds })
}

func (p *videoPlayer) stop(ctx context.Context) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.end(ctx)
	p.set(func() { p.state, p.err, p.pos, p.req = renderer.StateIdle, nil, 0, nil })
	return nil
}

func (p *videoPlayer) setVolume(ctx context.Context, level float64, muted bool) error {
	p.mu.Lock()
	sess := p.sess
	p.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.setVolume(ctx, level, muted)
}

func (p *videoPlayer) status() (state string, pos, dur float64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, p.pos, p.dur, p.err
}
