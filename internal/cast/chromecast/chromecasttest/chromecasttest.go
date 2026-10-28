// Package chromecasttest is an in-process fake Chromecast for tests: a TLS server on
// 127.0.0.1 speaking the Cast v2 framing, with a platform receiver (receiver-0) and a Default
// Media Receiver that accept the messages a sender uses (CONNECT/CLOSE, PING/PONG,
// GET_STATUS, LAUNCH, STOP, SET_VOLUME, LOAD, PLAY, PAUSE, SEEK) and answer with realistic
// RECEIVER_STATUS / MEDIA_STATUS payloads. Tests drive it to simulate the end of the media,
// playback errors, other senders and apps taking over, heartbeat loss and dropped
// connections, and can have it fetch the media URLs it is told to load, like a real receiver.
package chromecasttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/cast/chromecast"
	"ludworks/internal/cast/renderer"
)

// Options configures a fake device.
type Options struct {
	// FetchMedia makes LOAD fetch the contentId (GET with "Range: bytes=0-") and every track's
	// trackContentId like a real receiver; a failed or non-2xx fetch of the contentId answers
	// LOAD_FAILED and reports IDLE / ERROR.
	FetchMedia bool
	// Client performs those fetches (default: an http.Client with a 10 s timeout).
	Client *http.Client
	// CapabilityBits is advertised as TXT "ca" by RendererDevice (default 4101: video + audio).
	CapabilityBits int
	// PingInterval is how often the device PINGs its senders (default 5 s).
	PingInterval time.Duration
	// SlowLaunch answers LAUNCH before the app runs and lists it in a later RECEIVER_STATUS.
	SlowLaunch bool
	// Name is the friendly name (TXT "fn"); default "Fake Chromecast".
	Name string
}

// Load is one LOAD request the device received.
type Load struct {
	ContentID, ContentType, StreamType string
	Duration, CurrentTime              float64
	Autoplay                           bool
	SessionID                          string // the LOAD's receiver app sessionId
	MetadataType                       int
	Metadata                           map[string]any
	Tracks                             []map[string]any
	ActiveTrackIDs                     []int
	HasActiveTrackIDs                  bool // activeTrackIds was present
	TextTrackStyle                     map[string]any
	FetchStatus                        int   // FetchMedia: HTTP status of the contentId (0 = request failed)
	TrackFetchStatus                   []int // FetchMedia: per track
	Raw                                string
}

// Well-known ids.
const (
	OtherAppID = "233637DE" // the app TakeOver launches
)

// Device is a running fake Chromecast.
type Device struct {
	t      testing.TB
	o      Options
	ln     net.Listener
	addr   *net.TCPAddr
	id     string
	stop   chan struct{}
	wg     sync.WaitGroup
	loadCh chan struct{}

	mu        sync.Mutex
	conns     map[*fconn]struct{}
	silent    bool
	volume    float64
	muted     bool
	app       *appState
	media     *mediaState
	nextMedia int64
	loadGen   int64
	nextApp   int
	loads     []Load
	commands  []string
	closed    bool
}

type appState struct {
	appID, sessionID, transportID string
}

type mediaState struct {
	sessionID                          int64
	contentID, contentType, streamType string
	duration                           float64
	state, idleReason                  string
	base                               float64
	at                                 time.Time
	metadata                           map[string]any
	tracks                             []map[string]any
	activeTrackIDs                     []int
}

// position is where playback is now (advancing while PLAYING, clamped to the duration).
func (m *mediaState) position(now time.Time) float64 {
	p := m.base
	if m.state == "PLAYING" && !m.at.IsZero() {
		p += now.Sub(m.at).Seconds()
	}
	if m.duration > 0 && p > m.duration {
		p = m.duration
	}
	return math.Max(p, 0)
}

// fconn is one client connection with its virtual connections (destination → sources).
type fconn struct {
	c           net.Conn
	wmu         sync.Mutex
	virt        map[string]map[string]bool
	partitioned bool
}

// Start runs a fake device until the test ends.
func Start(t testing.TB, o Options) *Device {
	t.Helper()
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.CapabilityBits == 0 {
		o.CapabilityBits = 4101
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 5 * time.Second
	}
	if o.Name == "" {
		o.Name = "Fake Chromecast"
	}
	cert, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var idb [16]byte
	_, _ = rand.Read(idb[:])
	d := &Device{
		t: t, o: o, addr: raw.Addr().(*net.TCPAddr), id: hex.EncodeToString(idb[:]), stop: make(chan struct{}),
		loadCh: make(chan struct{}, 1), conns: map[*fconn]struct{}{}, volume: 1,
	}
	d.ln = tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	d.wg.Add(2)
	go d.accept()
	go d.pinger()
	t.Cleanup(d.Close)
	return d
}

// selfSigned makes an ECDSA P-256 certificate like a device's (not chained to anything).
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "fake-chromecast"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Close stops the device and drops every connection (also run at test cleanup).
func (d *Device) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.stop)
	for fc := range d.conns {
		_ = fc.c.Close()
	}
	d.mu.Unlock()
	_ = d.ln.Close()
	d.wg.Wait()
}

// Host is the device's address.
func (d *Device) Host() string { return d.addr.IP.String() }

// Port is the device's Cast port.
func (d *Device) Port() int { return d.addr.Port }

// ID is the device's Cast id (TXT "id").
func (d *Device) ID() string { return d.id }

// RendererDevice describes the device as discovery would.
func (d *Device) RendererDevice() renderer.Device {
	return renderer.Device{
		ID: "chromecast-" + d.id, Kind: renderer.KindChromecast, Name: d.o.Name, Model: "Chromecast",
		Endpoints: []renderer.Endpoint{{
			Service: renderer.ServiceGoogleCast, Host: d.Host(), Port: d.Port(),
			TXT: map[string]string{"id": d.id, "fn": d.o.Name, "md": "Chromecast", "ca": strconv.Itoa(d.o.CapabilityBits)},
		}},
	}
}

/* -------------------------------------------------------------- inspection */

// Loads returns every LOAD received so far.
func (d *Device) Loads() []Load {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Load(nil), d.loads...)
}

// WaitLoads waits (up to 5 s) until n LOADs arrived.
func (d *Device) WaitLoads(t testing.TB, n int) []Load {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if l := d.Loads(); len(l) >= n {
			return l
		}
		select {
		case <-d.loadCh:
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			t.Fatalf("chromecasttest: timed out waiting for %d loads (have %d)", n, len(d.Loads()))
		}
	}
}

// Commands lists the received messages as "<namespace>:<type>" (namespace = connection,
// heartbeat, receiver or media), in order.
func (d *Device) Commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.commands...)
}

// CountCommand counts received messages equal to cmd (e.g. "receiver:LAUNCH").
func (d *Device) CountCommand(cmd string) int {
	n := 0
	for _, c := range d.Commands() {
		if c == cmd {
			n++
		}
	}
	return n
}

// State is the current playerState ("" when nothing is loaded).
func (d *Device) State() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.media == nil {
		return ""
	}
	return d.media.state
}

// Position is the current media position in seconds.
func (d *Device) Position() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.media == nil {
		return 0
	}
	return d.media.position(time.Now())
}

// Volume is the device volume.
func (d *Device) Volume() (level float64, muted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.volume, d.muted
}

// App returns the running app's id and session ("" when none).
func (d *Device) App() (appID, sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.app == nil {
		return "", ""
	}
	return d.app.appID, d.app.sessionID
}

// ApplicationsJSON is the "applications" array of the current RECEIVER_STATUS as JSON, for
// tests that craft receiver statuses of their own with Send.
func (d *Device) ApplicationsJSON() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	apps, ok := d.receiverStatusLocked(0)["status"].(map[string]any)["applications"]
	if !ok {
		return "[]"
	}
	b, _ := json.Marshal(apps)
	return string(b)
}

// Connections is the number of open client connections.
func (d *Device) Connections() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

/* --------------------------------------------------------------- simulation */

// Finish ends the current media on its own (IDLE / FINISHED).
func (d *Device) Finish() { d.endMedia("FINISHED") }

// Fail reports a playback error for the current media (IDLE / ERROR).
func (d *Device) Fail() { d.endMedia("ERROR") }

// InterruptMedia ends the current media as another sender's LOAD would (IDLE / INTERRUPTED).
func (d *Device) InterruptMedia() { d.endMedia("INTERRUPTED") }

func (d *Device) endMedia(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.media
	if m == nil {
		return
	}
	now := time.Now()
	m.base, m.at = m.position(now), time.Time{}
	if reason == "FINISHED" && m.duration > 0 {
		m.base = m.duration
	}
	m.state, m.idleReason = "IDLE", reason
	d.broadcastMediaLocked(nil, "", 0, []any{d.mediaEntryLocked(now)})
	d.media = nil
}

// SetPlayerState changes the playerState of the current media (e.g. "BUFFERING") and reports it.
func (d *Device) SetPlayerState(state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.media == nil {
		return
	}
	now := time.Now()
	d.media.base, d.media.at = d.media.position(now), time.Time{}
	d.media.state = state
	if state == "PLAYING" {
		d.media.at = now
	}
	d.broadcastMediaLocked(nil, "", 0, []any{d.mediaEntryLocked(now)})
}

// SetPosition moves the current media (as a seek by another sender would) and reports it.
func (d *Device) SetPosition(sec float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.media == nil {
		return
	}
	now := time.Now()
	d.media.base = sec
	if d.media.state == "PLAYING" {
		d.media.at = now
	}
	d.broadcastMediaLocked(nil, "", 0, []any{d.mediaEntryLocked(now)})
}

// TakeOver launches another app: the Default Media Receiver session ends (its transport
// CLOSEs its senders) and RECEIVER_STATUS lists only the other app.
func (d *Device) TakeOver() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.app != nil {
		d.closeTransportLocked(d.app.transportID)
	}
	d.nextApp++
	n := strconv.Itoa(d.nextApp)
	d.app = &appState{appID: OtherAppID, sessionID: "other-session-" + n, transportID: "other-transport-" + n}
	d.media = nil
	d.broadcastReceiverLocked(nil, "", 0)
}

// Silence stops (or resumes) all traffic: nothing is answered or sent, as if the device hung.
// New connections are accepted (TLS completes) but stay silent too.
func (d *Device) Silence(on bool) {
	d.mu.Lock()
	d.silent = on
	d.mu.Unlock()
}

// Partition makes the current connections silent in both directions (as if the network
// dropped them); new connections work normally.
func (d *Device) Partition() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for fc := range d.conns {
		fc.partitioned = true
	}
}

// DropConnections closes every client connection.
func (d *Device) DropConnections() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for fc := range d.conns {
		_ = fc.c.Close()
	}
}

// Send sends a raw JSON payload on namespace ns to every connected sender: from the app's
// transport on the media namespace, else from receiver-0 (for hostile-input tests).
func (d *Device) Send(ns, payload string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	src := chromecast.ReceiverID
	if ns == chromecast.NamespaceMedia && d.app != nil {
		src = d.app.transportID
	}
	for fc := range d.conns {
		for dst := range fc.sourcesOf(src) {
			d.writeLocked(fc, &chromecast.Message{SourceID: src, DestinationID: dst, Namespace: ns, PayloadUTF8: payload})
		}
		if len(fc.sourcesOf(src)) == 0 {
			d.writeLocked(fc, &chromecast.Message{SourceID: src, DestinationID: "*", Namespace: ns, PayloadUTF8: payload})
		}
	}
}

// SendRawFrame writes raw bytes to every client connection (e.g. a bogus frame header).
func (d *Device) SendRawFrame(b []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for fc := range d.conns {
		fc.wmu.Lock()
		_ = fc.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = fc.c.Write(b)
		fc.wmu.Unlock()
	}
}

/* ----------------------------------------------------------------- serving */

func (d *Device) accept() {
	defer d.wg.Done()
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		fc := &fconn{c: c, virt: map[string]map[string]bool{}}
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			_ = c.Close()
			return
		}
		d.conns[fc] = struct{}{}
		d.wg.Add(1)
		d.mu.Unlock()
		go d.serve(fc)
	}
}

func (d *Device) serve(fc *fconn) {
	defer d.wg.Done()
	defer func() {
		_ = fc.c.Close()
		d.mu.Lock()
		delete(d.conns, fc)
		d.mu.Unlock()
	}()
	for {
		m, err := chromecast.ReadMessage(fc.c)
		if err != nil {
			return
		}
		d.handle(fc, m)
	}
}

// pinger sends the device's own heartbeat PINGs.
func (d *Device) pinger() {
	defer d.wg.Done()
	t := time.NewTicker(d.o.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.mu.Lock()
			for fc := range d.conns {
				for src := range fc.sourcesOf(chromecast.ReceiverID) {
					d.sendLocked(fc, chromecast.NamespaceHeartbeat, chromecast.ReceiverID, src, map[string]any{"type": "PING"})
				}
			}
			d.mu.Unlock()
		}
	}
}

func (fc *fconn) sourcesOf(dest string) map[string]bool { return fc.virt[dest] }

type envelope struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
}

func short(ns string) string {
	switch ns {
	case chromecast.NamespaceConnection:
		return "connection"
	case chromecast.NamespaceHeartbeat:
		return "heartbeat"
	case chromecast.NamespaceReceiver:
		return "receiver"
	case chromecast.NamespaceMedia:
		return "media"
	}
	return ns
}

func (d *Device) handle(fc *fconn, m *chromecast.Message) {
	var env envelope
	_ = json.Unmarshal([]byte(m.PayloadUTF8), &env)
	if m.Namespace == chromecast.NamespaceMedia && env.Type == "LOAD" {
		d.handleLoad(fc, m, env)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.silent || fc.partitioned {
		return
	}
	d.commands = append(d.commands, short(m.Namespace)+":"+env.Type)
	switch m.Namespace {
	case chromecast.NamespaceConnection:
		switch env.Type {
		case "CONNECT":
			if fc.virt[m.DestinationID] == nil {
				fc.virt[m.DestinationID] = map[string]bool{}
			}
			fc.virt[m.DestinationID][m.SourceID] = true
		case "CLOSE":
			delete(fc.virt[m.DestinationID], m.SourceID)
		}
	case chromecast.NamespaceHeartbeat:
		if env.Type == "PING" {
			d.sendLocked(fc, chromecast.NamespaceHeartbeat, m.DestinationID, m.SourceID, map[string]any{"type": "PONG"})
		}
	case chromecast.NamespaceReceiver:
		if m.DestinationID == chromecast.ReceiverID {
			d.handleReceiver(fc, m, env)
		}
	case chromecast.NamespaceMedia:
		if d.app != nil && d.app.appID == chromecast.DefaultMediaReceiverAppID && m.DestinationID == d.app.transportID &&
			fc.virt[m.DestinationID][m.SourceID] {
			d.handleMedia(fc, m, env)
		}
	}
}

func (d *Device) handleReceiver(fc *fconn, m *chromecast.Message, env envelope) {
	var req struct {
		AppID     string `json:"appId"`
		SessionID string `json:"sessionId"`
		Volume    struct {
			Level *float64 `json:"level"`
			Muted *bool    `json:"muted"`
		} `json:"volume"`
	}
	_ = json.Unmarshal([]byte(m.PayloadUTF8), &req)
	switch env.Type {
	case "GET_STATUS":
		d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID)
	case "LAUNCH":
		if req.AppID != chromecast.DefaultMediaReceiverAppID {
			d.sendLocked(fc, chromecast.NamespaceReceiver, chromecast.ReceiverID, m.SourceID,
				map[string]any{"type": "LAUNCH_ERROR", "requestId": env.RequestID, "reason": "NOT_FOUND"})
			return
		}
		if d.app != nil && d.app.appID == chromecast.DefaultMediaReceiverAppID {
			d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID) // already running: same session
			return
		}
		if d.app != nil {
			d.closeTransportLocked(d.app.transportID)
		}
		d.media = nil
		d.nextApp++
		n := strconv.Itoa(d.nextApp)
		next := &appState{appID: chromecast.DefaultMediaReceiverAppID, sessionID: "session-" + n + "-" + d.id[:8], transportID: "transport-" + n}
		if d.o.SlowLaunch {
			d.app = nil
			d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID)
			time.AfterFunc(30*time.Millisecond, func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.closed {
					return
				}
				d.app = next
				d.broadcastReceiverLocked(nil, "", 0)
			})
			return
		}
		d.app = next
		d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID)
	case "STOP":
		if d.app != nil && (req.SessionID == "" || req.SessionID == d.app.sessionID) {
			d.closeTransportLocked(d.app.transportID)
			d.app, d.media = nil, nil
		}
		d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID)
	case "SET_VOLUME":
		if req.Volume.Level != nil {
			d.volume = math.Max(0, math.Min(*req.Volume.Level, 1))
		}
		if req.Volume.Muted != nil {
			d.muted = *req.Volume.Muted
		}
		d.broadcastReceiverLocked(fc, m.SourceID, env.RequestID)
	}
}

func (d *Device) handleMedia(fc *fconn, m *chromecast.Message, env envelope) {
	var req struct {
		MediaSessionID int64    `json:"mediaSessionId"`
		CurrentTime    *float64 `json:"currentTime"`
	}
	_ = json.Unmarshal([]byte(m.PayloadUTF8), &req)
	now := time.Now()
	if env.Type == "GET_STATUS" {
		status := []any{}
		if d.media != nil {
			status = append(status, d.mediaEntryLocked(now))
		}
		d.sendLocked(fc, chromecast.NamespaceMedia, d.app.transportID, m.SourceID,
			map[string]any{"type": "MEDIA_STATUS", "requestId": env.RequestID, "status": status})
		return
	}
	md := d.media
	if md == nil || req.MediaSessionID != md.sessionID {
		d.sendLocked(fc, chromecast.NamespaceMedia, d.app.transportID, m.SourceID,
			map[string]any{"type": "INVALID_REQUEST", "requestId": env.RequestID, "reason": "INVALID_MEDIA_SESSION_ID"})
		return
	}
	switch env.Type {
	case "PLAY":
		md.base, md.at, md.state = md.position(now), now, "PLAYING"
	case "PAUSE":
		md.base, md.at, md.state = md.position(now), time.Time{}, "PAUSED"
	case "SEEK":
		if req.CurrentTime != nil {
			md.base = math.Max(*req.CurrentTime, 0)
			if md.duration > 0 {
				md.base = math.Min(md.base, md.duration)
			}
		}
		if md.state == "PLAYING" {
			md.at = now
		}
	case "STOP":
		md.base, md.at = md.position(now), time.Time{}
		md.state, md.idleReason = "IDLE", "CANCELLED"
		entry := d.mediaEntryLocked(now)
		d.media = nil
		d.broadcastMediaLocked(fc, m.SourceID, env.RequestID, []any{entry})
		return
	default:
		d.sendLocked(fc, chromecast.NamespaceMedia, d.app.transportID, m.SourceID,
			map[string]any{"type": "INVALID_REQUEST", "requestId": env.RequestID, "reason": "INVALID_COMMAND"})
		return
	}
	d.broadcastMediaLocked(fc, m.SourceID, env.RequestID, []any{d.mediaEntryLocked(now)})
}

// handleLoad records a LOAD, fetches its media when configured (outside the lock) and answers.
func (d *Device) handleLoad(fc *fconn, m *chromecast.Message, env envelope) {
	var req struct {
		SessionID      string   `json:"sessionId"`
		CurrentTime    *float64 `json:"currentTime"`
		Autoplay       *bool    `json:"autoplay"`
		ActiveTrackIDs *[]int   `json:"activeTrackIds"`
		Media          struct {
			ContentID      string           `json:"contentId"`
			ContentType    string           `json:"contentType"`
			StreamType     string           `json:"streamType"`
			Duration       float64          `json:"duration"`
			Metadata       map[string]any   `json:"metadata"`
			Tracks         []map[string]any `json:"tracks"`
			TextTrackStyle map[string]any   `json:"textTrackStyle"`
		} `json:"media"`
	}
	_ = json.Unmarshal([]byte(m.PayloadUTF8), &req)
	d.mu.Lock()
	if d.silent || fc.partitioned {
		d.mu.Unlock()
		return
	}
	d.commands = append(d.commands, "media:LOAD")
	ok := d.app != nil && d.app.appID == chromecast.DefaultMediaReceiverAppID && m.DestinationID == d.app.transportID &&
		fc.virt[m.DestinationID][m.SourceID]
	d.mu.Unlock()
	if !ok {
		return
	}
	ld := Load{
		ContentID: req.Media.ContentID, ContentType: req.Media.ContentType, StreamType: req.Media.StreamType,
		Duration: req.Media.Duration, Autoplay: req.Autoplay != nil && *req.Autoplay, SessionID: req.SessionID,
		Metadata: req.Media.Metadata, Tracks: req.Media.Tracks, TextTrackStyle: req.Media.TextTrackStyle, Raw: m.PayloadUTF8,
	}
	if req.CurrentTime != nil {
		ld.CurrentTime = *req.CurrentTime
	}
	if req.ActiveTrackIDs != nil {
		ld.HasActiveTrackIDs, ld.ActiveTrackIDs = true, *req.ActiveTrackIDs
	}
	if mt, ok := req.Media.Metadata["metadataType"].(float64); ok {
		ld.MetadataType = int(mt)
	}
	if !d.o.FetchMedia {
		d.finishLoad(fc, m.SourceID, env.RequestID, ld, false, 0)
		return
	}
	// Like a real receiver, fetch while the connection keeps being served (heartbeats).
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.loadGen++
	gen := d.loadGen
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		ld.FetchStatus = d.fetch(ld.ContentID, true)
		failed := ld.FetchStatus < 200 || ld.FetchStatus > 299
		for _, tr := range ld.Tracks {
			u, _ := tr["trackContentId"].(string)
			ld.TrackFetchStatus = append(ld.TrackFetchStatus, d.fetch(u, false))
		}
		d.finishLoad(fc, m.SourceID, env.RequestID, ld, failed, gen)
	}()
}

// finishLoad records a LOAD and answers it: LOAD_CANCELLED when a newer LOAD superseded it
// (gen ≠ 0 and not the latest), LOAD_FAILED + IDLE/ERROR when its media could not be
// fetched, else a new media session (the previous one ends with IDLE/INTERRUPTED).
func (d *Device) finishLoad(fc *fconn, src string, requestID int64, ld Load, failed bool, gen int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loads = append(d.loads, ld)
	select {
	case d.loadCh <- struct{}{}:
	default:
	}
	if d.closed || d.app == nil || d.silent || fc.partitioned {
		return
	}
	if gen != 0 && gen != d.loadGen {
		d.sendLocked(fc, chromecast.NamespaceMedia, d.app.transportID, src,
			map[string]any{"type": "LOAD_CANCELLED", "requestId": requestID})
		return
	}
	now := time.Now()
	if old := d.media; old != nil {
		old.base, old.at = old.position(now), time.Time{}
		old.state, old.idleReason = "IDLE", "INTERRUPTED"
		d.broadcastMediaLocked(nil, "", 0, []any{d.mediaEntryLocked(now)})
		d.media = nil
	}
	d.nextMedia++
	md := &mediaState{
		sessionID: d.nextMedia, contentID: ld.ContentID, contentType: ld.ContentType, streamType: ld.StreamType,
		duration: ld.Duration, metadata: ld.Metadata, tracks: ld.Tracks, activeTrackIDs: ld.ActiveTrackIDs,
		base: math.Max(ld.CurrentTime, 0),
	}
	if failed {
		md.state, md.idleReason = "IDLE", "ERROR"
		d.sendLocked(fc, chromecast.NamespaceMedia, d.app.transportID, src,
			map[string]any{"type": "LOAD_FAILED", "requestId": requestID})
		d.media = md
		d.broadcastMediaLocked(nil, "", 0, []any{d.mediaEntryLocked(now)})
		d.media = nil
		return
	}
	md.state = "PAUSED"
	if ld.Autoplay {
		md.state, md.at = "PLAYING", now
	}
	d.media = md
	d.broadcastMediaLocked(fc, src, requestID, []any{d.mediaEntryLocked(now)})
}

// fetch GETs u like a receiver and returns the status (0 when the request failed). Only the
// response headers are read: the body may be an endless live transcode.
func (d *Device) fetch(u string, ranged bool) int {
	if u == "" {
		return 0
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0
	}
	if ranged {
		req.Header.Set("Range", "bytes=0-")
	}
	resp, err := d.o.Client.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

/* ------------------------------------------------------------------ output */

func (d *Device) receiverStatusLocked(requestID int64) map[string]any {
	st := map[string]any{
		"volume":        map[string]any{"level": d.volume, "muted": d.muted, "controlType": "attenuation", "stepInterval": 0.05},
		"isActiveInput": true, "isStandBy": false,
	}
	if a := d.app; a != nil {
		name := "Default Media Receiver"
		if a.appID != chromecast.DefaultMediaReceiverAppID {
			name = "Other App"
		}
		st["applications"] = []any{map[string]any{
			"appId": a.appID, "displayName": name, "sessionId": a.sessionID, "transportId": a.transportID,
			"statusText": "Ready To Cast", "isIdleScreen": false,
			"namespaces": []any{map[string]any{"name": chromecast.NamespaceMedia}},
		}}
	}
	return map[string]any{"type": "RECEIVER_STATUS", "requestId": requestID, "status": st}
}

func (d *Device) mediaEntryLocked(now time.Time) map[string]any {
	m := d.media
	media := map[string]any{"contentId": m.contentID, "contentType": m.contentType, "streamType": m.streamType, "metadata": m.metadata}
	if m.duration > 0 {
		media["duration"] = m.duration
	}
	if len(m.tracks) > 0 {
		media["tracks"] = m.tracks
	}
	active := m.activeTrackIDs
	if active == nil {
		active = []int{}
	}
	e := map[string]any{
		"mediaSessionId": m.sessionID, "playbackRate": 1, "playerState": m.state, "currentTime": m.position(now),
		"supportedMediaCommands": 274447, "volume": map[string]any{"level": 1, "muted": false},
		"activeTrackIds": active, "media": media,
	}
	if m.idleReason != "" {
		e["idleReason"] = m.idleReason
	}
	return e
}

// broadcastReceiverLocked answers a receiver request (to reqConn/reqSrc with requestID) and
// sends the same status, with requestId 0, to every other sender connected to receiver-0.
func (d *Device) broadcastReceiverLocked(reqConn *fconn, reqSrc string, requestID int64) {
	if reqConn != nil {
		d.sendLocked(reqConn, chromecast.NamespaceReceiver, chromecast.ReceiverID, reqSrc, d.receiverStatusLocked(requestID))
	}
	for fc := range d.conns {
		for src := range fc.sourcesOf(chromecast.ReceiverID) {
			if fc == reqConn && src == reqSrc {
				continue
			}
			d.sendLocked(fc, chromecast.NamespaceReceiver, chromecast.ReceiverID, src, d.receiverStatusLocked(0))
		}
	}
}

// broadcastMediaLocked answers a media request and reports status to the app's other senders.
func (d *Device) broadcastMediaLocked(reqConn *fconn, reqSrc string, requestID int64, status []any) {
	if d.app == nil {
		return
	}
	tr := d.app.transportID
	if reqConn != nil {
		d.sendLocked(reqConn, chromecast.NamespaceMedia, tr, reqSrc, map[string]any{"type": "MEDIA_STATUS", "requestId": requestID, "status": status})
	}
	for fc := range d.conns {
		for src := range fc.sourcesOf(tr) {
			if fc == reqConn && src == reqSrc {
				continue
			}
			d.sendLocked(fc, chromecast.NamespaceMedia, tr, src, map[string]any{"type": "MEDIA_STATUS", "requestId": 0, "status": status})
		}
	}
}

// closeTransportLocked CLOSEs the virtual connections of a transport (its app ended).
func (d *Device) closeTransportLocked(tr string) {
	for fc := range d.conns {
		for src := range fc.sourcesOf(tr) {
			d.sendLocked(fc, chromecast.NamespaceConnection, tr, src, map[string]any{"type": "CLOSE"})
		}
		delete(fc.virt, tr)
	}
}

func (d *Device) sendLocked(fc *fconn, ns, src, dst string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	d.writeLocked(fc, &chromecast.Message{SourceID: src, DestinationID: dst, Namespace: ns, PayloadUTF8: string(b)})
}

func (d *Device) writeLocked(fc *fconn, m *chromecast.Message) {
	if d.silent || fc.partitioned {
		return
	}
	fc.wmu.Lock()
	defer fc.wmu.Unlock()
	_ = fc.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := chromecast.WriteMessage(fc.c, m); err != nil && !strings.Contains(err.Error(), "closed") {
		_ = fc.c.Close()
	}
}
