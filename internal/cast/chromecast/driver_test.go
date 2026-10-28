package chromecast_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/cast/chromecast"
	"ludworks/internal/cast/chromecast/chromecasttest"
	"ludworks/internal/cast/renderer"
)

// testDriver returns a driver with intervals short enough for tests.
func testDriver(o chromecast.Options) *chromecast.Driver {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = 100 * time.Millisecond
	}
	if o.StatusInterval == 0 {
		o.StatusInterval = 50 * time.Millisecond
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 2 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 2 * time.Second
	}
	if o.ReconnectDelay == 0 {
		o.ReconnectDelay = 20 * time.Millisecond
	}
	return chromecast.New(o)
}

func connect(t *testing.T, d *chromecast.Driver, dev *chromecasttest.Device) renderer.Renderer {
	t.Helper()
	rd := dev.RendererDevice()
	r, err := d.Connect(context.Background(), &rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// waitFor polls r.Status until cond holds (at most 5 s).
func waitFor(t *testing.T, r renderer.Renderer, what string, cond func(renderer.Status) bool) renderer.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := r.Status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; status %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitUntil polls cond (at most 5 s).
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func state(s string) func(renderer.Status) bool {
	return func(st renderer.Status) bool { return st.State == s }
}

// waitClosed waits (at most 10 s) until ch is closed, consuming pending signals.
func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Changed was not closed")
		}
	}
}

// drain empties Changed and reports whether a signal was pending.
func drain(r renderer.Renderer) bool {
	select {
	case _, ok := <-r.Changed():
		return ok
	default:
		return false
	}
}

func movie(url string) *renderer.LoadRequest {
	return &renderer.LoadRequest{URL: url, ContentType: "video/mp4", Video: true, Title: "Movie", DurationSeconds: 600}
}

const mediaURL = "http://127.0.0.1:9/api/v1/cast/media/TICKET/media/abcdefgh/stream"

func TestDescribe(t *testing.T) {
	d := chromecast.New(chromecast.Options{})
	if d.Kind() != renderer.KindChromecast {
		t.Fatal(d.Kind())
	}
	dev := func(ca string, has bool) *renderer.Device {
		txt := map[string]string{}
		if has {
			txt["ca"] = ca
		}
		return &renderer.Device{Endpoints: []renderer.Endpoint{{Service: renderer.ServiceGoogleCast, TXT: txt}}}
	}
	for _, tc := range []struct {
		dev          *renderer.Device
		video, audio bool
	}{
		{dev("4101", true), true, true},        // Chromecast
		{dev("200709", true), true, true},      // Chromecast with Google TV
		{dev("2052", true), false, true},       // Google Home / Nest speaker
		{dev("2084", true), false, true},       // speaker group
		{dev("1", true), true, false},          // video only
		{dev(" 4101 ", true), true, true},      // whitespace
		{dev("", true), true, true},            // empty: assume both
		{dev("abc", true), true, true},         // garbage: assume both
		{dev("-1", true), true, true},          // negative: assume both
		{dev("", false), true, true},           // no ca at all
		{&renderer.Device{}, true, true},       // no endpoint
		{nil, true, true},                      // no device
		{dev("99999999999", true), true, true}, // overflow: assume both
	} {
		c := d.Describe(tc.dev)
		if c.Video != tc.video || c.Audio != tc.audio || c.RequiresPairing || c.PairingSupported {
			t.Errorf("Describe(%v) = %+v, want video %v audio %v", tc.dev, c, tc.video, tc.audio)
		}
	}
}

func TestConnectAndLoad(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	cmds := dev.Commands()
	if !slices.Contains(cmds, "connection:CONNECT") || !slices.Contains(cmds, "receiver:GET_STATUS") {
		t.Fatalf("connect: %v", cmds)
	}
	if slices.Contains(cmds, "receiver:LAUNCH") {
		t.Fatal("the receiver app must not be launched before the first load")
	}
	a, ok := r.(renderer.Addresser)
	if !ok {
		t.Fatal("renderer does not implement renderer.Addresser")
	}
	if got, want := a.RemoteAddr().String(), net.JoinHostPort(dev.Host(), strconv.Itoa(dev.Port())); got != want {
		t.Fatalf("RemoteAddr %s, want %s", got, want)
	}
	if la, ok := a.LocalAddr().(*net.TCPAddr); !ok || !la.IP.IsLoopback() {
		t.Fatalf("LocalAddr %v", a.LocalAddr())
	}
	if st := r.Status(); st.State != renderer.StateIdle || st.Volume != 1 {
		t.Fatalf("initial status %+v", st)
	}

	req := &renderer.LoadRequest{URL: mediaURL, ContentType: "video/mp4", Video: true, Title: "Night of the Living Dead",
		Year: 1968, DurationSeconds: 5760, StartSeconds: 30, ArtURL: "http://127.0.0.1:9/art.jpg"}
	if err := r.Load(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ld := dev.WaitLoads(t, 1)[0]
	if ld.ContentID != mediaURL || ld.ContentType != "video/mp4" || ld.StreamType != "BUFFERED" || ld.Duration != 5760 ||
		ld.CurrentTime != 30 || !ld.Autoplay || ld.MetadataType != 1 {
		t.Fatalf("load: %+v", ld)
	}
	if ld.Metadata["title"] != "Night of the Living Dead" || ld.Metadata["releaseDate"] != "1968" {
		t.Fatalf("metadata: %v", ld.Metadata)
	}
	if imgs, _ := ld.Metadata["images"].([]any); len(imgs) != 1 || imgs[0].(map[string]any)["url"] != "http://127.0.0.1:9/art.jpg" {
		t.Fatalf("images: %v", ld.Metadata["images"])
	}
	if !ld.HasActiveTrackIDs || len(ld.ActiveTrackIDs) != 0 || ld.Tracks != nil || ld.TextTrackStyle != nil {
		t.Fatalf("tracks: %+v", ld)
	}
	if _, session := dev.App(); session == "" || ld.SessionID != session {
		t.Fatalf("LOAD sessionId %q, app session %q", ld.SessionID, session)
	}
	cmds = dev.Commands()
	launch, load := slices.Index(cmds, "receiver:LAUNCH"), slices.Index(cmds, "media:LOAD")
	if launch < 0 || load < launch || !slices.Contains(cmds[launch:load], "connection:CONNECT") {
		t.Fatalf("expected LAUNCH, CONNECT (transport), LOAD: %v", cmds)
	}
	st := waitFor(t, r, "playing", state(renderer.StatePlaying))
	if st.Duration != 5760 || st.Position < 30 || st.Err != nil {
		t.Fatalf("playing: %+v", st)
	}
}

func TestLoadMetadataTypes(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	reqs := []*renderer.LoadRequest{
		{URL: mediaURL + "?1", ContentType: "video/mp4", Video: true, Title: "Pilot", SeriesTitle: "Show", Season: 1, Episode: 2,
			Subtitles: []renderer.SubtitleTrack{{URL: "http://h/0.vtt", Language: "en", Label: "English"}, {URL: "http://h/1.vtt", Label: "Forced"}}},
		{URL: mediaURL + "?2", ContentType: "audio/flac", Title: "Song", Artist: "Band", Album: "Record", Year: 2001},
		{URL: mediaURL + "?3", ContentType: "audio/mpeg", Title: "Recording"},
		{URL: mediaURL + "?4", ContentType: "video/mp4", Video: true, Live: true, Title: "Live"},
	}
	for i, req := range reqs {
		if err := r.Load(ctx, req); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		waitFor(t, r, "playing", state(renderer.StatePlaying))
		if st := r.Status(); st.Err != nil {
			t.Fatalf("load %d: a load replacing our own media must not count as an interruption: %+v", i, st)
		}
	}
	loads := dev.WaitLoads(t, 4)
	if ep := loads[0]; ep.MetadataType != 2 || ep.Metadata["seriesTitle"] != "Show" || ep.Metadata["season"] != 1.0 ||
		ep.Metadata["episode"] != 2.0 || ep.Metadata["title"] != "Pilot" {
		t.Fatalf("episode: %+v", ep)
	}
	if ep := loads[0]; len(ep.Tracks) != 2 || ep.Tracks[0]["trackContentType"] != "text/vtt" || ep.Tracks[0]["language"] != "en" ||
		ep.Tracks[1]["trackId"] != 2.0 || ep.TextTrackStyle == nil || !ep.HasActiveTrackIDs || len(ep.ActiveTrackIDs) != 0 {
		t.Fatalf("episode tracks: %+v", ep)
	}
	if tr := loads[1]; tr.MetadataType != 3 || tr.Metadata["artist"] != "Band" || tr.Metadata["albumName"] != "Record" || tr.Metadata["releaseDate"] != "2001" {
		t.Fatalf("track: %+v", tr)
	}
	if g := loads[2]; g.MetadataType != 0 || g.Metadata["title"] != "Recording" {
		t.Fatalf("generic: %+v", g)
	}
	if live := loads[3]; live.StreamType != "LIVE" {
		t.Fatalf("live: %+v", live)
	}
	if n := dev.CountCommand("receiver:LAUNCH"); n != 1 {
		t.Fatalf("the receiver app was launched %d times", n)
	}
	if err := r.Seek(ctx, 10); !errors.Is(err, renderer.ErrNotSeekable) {
		t.Fatalf("seeking live media: %v", err)
	}
}

func TestControls(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	if err := r.Play(ctx); err == nil {
		t.Fatal("play without media must fail")
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("stop without media: %v", err)
	}
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))

	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "paused", state(renderer.StatePaused))
	if dev.State() != "PAUSED" {
		t.Fatalf("device state %s", dev.State())
	}
	if err := r.Play(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	if err := r.Seek(ctx, 42); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, r, "seeked", func(s renderer.Status) bool { return s.Position >= 42 && s.Position < 43.5 })
	if p := dev.Position(); p < 42 || p > 43.5 {
		t.Fatalf("device position %v (status %+v)", p, st)
	}
	if err := r.Seek(ctx, -1); err == nil {
		t.Fatal("negative seek accepted")
	}

	if err := r.SetVolume(ctx, 0.3); err != nil {
		t.Fatal(err)
	}
	if lv, _ := dev.Volume(); lv != 0.3 {
		t.Fatalf("device volume %v", lv)
	}
	waitFor(t, r, "volume", func(s renderer.Status) bool { return s.Volume == 0.3 })
	if err := r.SetMuted(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, muted := dev.Volume(); !muted {
		t.Fatal("device not muted")
	}
	waitFor(t, r, "muted", func(s renderer.Status) bool { return s.Muted })
	if err := r.SetVolume(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if lv, _ := dev.Volume(); lv != 1 {
		t.Fatalf("volume not clamped: %v", lv)
	}

	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	st = waitFor(t, r, "stopped", state(renderer.StateIdle))
	if st.Err != nil {
		t.Fatalf("our own stop reported as an error: %+v", st)
	}
	if dev.State() != "" {
		t.Fatalf("device still has media: %s", dev.State())
	}
	if err := r.Pause(ctx); err == nil {
		t.Fatal("pause after stop must fail")
	}
}

func TestPositionExtrapolation(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	// A slow status poll: the position must advance between device reports.
	r := connect(t, testDriver(chromecast.Options{StatusInterval: time.Hour}), dev)
	req := movie(mediaURL)
	req.StartSeconds = 10
	if err := r.Load(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	time.Sleep(300 * time.Millisecond)
	if p := r.Status().Position; p < 10.25 || p > 12 {
		t.Fatalf("position %v after 300 ms from 10 s", p)
	}
}

func TestEndedAndError(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	drain(r)
	dev.Finish()
	select {
	case <-r.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("no Changed signal for the end of the media")
	}
	st := waitFor(t, r, "ended", state(renderer.StateEnded))
	if st.Err != nil || st.Position != 600 {
		t.Fatalf("ended: %+v", st)
	}

	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.Fail()
	st = waitFor(t, r, "error", state(renderer.StateError))
	if !errors.Is(st.Err, chromecast.ErrPlaybackFailed) {
		t.Fatalf("error: %v", st.Err)
	}

	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.SetPlayerState("BUFFERING")
	waitFor(t, r, "buffering", state(renderer.StateBuffering))
	dev.SetPlayerState("PLAYING")
	waitFor(t, r, "playing again", state(renderer.StatePlaying))
	dev.SetPosition(300)
	waitFor(t, r, "external seek", func(s renderer.Status) bool { return s.Position >= 300 })
}

func TestLoadErrors(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Range") != "bytes=0-" {
			http.Error(w, "receivers send Range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fake mp4"))
	}))
	t.Cleanup(media.Close)
	dev := chromecasttest.Start(t, chromecasttest.Options{FetchMedia: true, Client: media.Client()})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()

	if err := r.Load(ctx, &renderer.LoadRequest{ContentType: "audio/aac"}); !errors.Is(err, renderer.ErrUnsupported) {
		t.Fatalf("load without URL: %v", err)
	}
	if err := r.Load(ctx, nil); !errors.Is(err, renderer.ErrUnsupported) {
		t.Fatalf("nil load: %v", err)
	}
	err := r.Load(ctx, movie(media.URL+"/missing.mp4"))
	if !errors.Is(err, chromecast.ErrPlaybackFailed) {
		t.Fatalf("load of a missing file: %v", err)
	}
	if st := r.Status(); st.State != renderer.StateError || st.Err == nil {
		t.Fatalf("status after a failed load: %+v", st)
	}
	if ld := dev.WaitLoads(t, 1)[0]; ld.FetchStatus != http.StatusNotFound {
		t.Fatalf("fetch status %d", ld.FetchStatus)
	}

	req := movie(media.URL + "/movie.mp4")
	req.Subtitles = []renderer.SubtitleTrack{{URL: media.URL + "/missing.vtt"}}
	if err := r.Load(ctx, req); err != nil {
		t.Fatal(err)
	}
	ld := dev.WaitLoads(t, 2)[1]
	if ld.FetchStatus != http.StatusOK || len(ld.TrackFetchStatus) != 1 || ld.TrackFetchStatus[0] != http.StatusNotFound {
		t.Fatalf("fetches: %+v", ld)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
}

func TestSlowLaunch(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{SlowLaunch: true})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	if err := r.Load(context.Background(), movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
}

func TestInterruptions(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	interrupted := func(s renderer.Status) bool {
		return s.State == renderer.StateIdle && errors.Is(s.Err, renderer.ErrInterrupted)
	}

	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.InterruptMedia() // another sender loaded something
	waitFor(t, r, "interrupted by another sender", interrupted)

	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.TakeOver() // another app
	waitFor(t, r, "taken over", interrupted)
	if err := r.Play(ctx); err == nil {
		t.Fatal("play after a take-over must fail")
	}
	// The renderer stays usable: a new load relaunches the receiver app.
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing after relaunch", state(renderer.StatePlaying))
	if n := dev.CountCommand("receiver:LAUNCH"); n != 2 {
		t.Fatalf("LAUNCH sent %d times", n)
	}
}

func TestHeartbeat(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{PingInterval: 20 * time.Millisecond})
	connect(t, testDriver(chromecast.Options{}), dev)
	waitUntil(t, "a PONG answering the device and a PING of our own", func() bool {
		return dev.CountCommand("heartbeat:PONG") > 0 && dev.CountCommand("heartbeat:PING") > 0
	})
}

func TestReconnectAfterHeartbeatLoss(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	before := dev.CountCommand("receiver:GET_STATUS")
	dev.Partition() // the device stops hearing us and we stop hearing it
	waitUntil(t, "a reconnect", func() bool { return dev.CountCommand("receiver:GET_STATUS") > before })
	waitUntil(t, "the receiver app re-joined", func() bool {
		return slices.Contains(dev.Commands()[before:], "media:GET_STATUS")
	})
	waitUntil(t, "controls work again", func() bool { return r.Pause(ctx) == nil })
	st := waitFor(t, r, "paused", state(renderer.StatePaused))
	if st.Err != nil {
		t.Fatalf("status after reconnect: %+v", st)
	}
	if n := dev.CountCommand("receiver:LAUNCH"); n != 1 {
		t.Fatalf("the app was relaunched (%d LAUNCH)", n)
	}
	if dev.State() != "PAUSED" {
		t.Fatalf("device state %s", dev.State())
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	first := r.(renderer.Addresser).LocalAddr().String()
	dev.DropConnections()
	waitUntil(t, "a new connection", func() bool { return r.(renderer.Addresser).LocalAddr().String() != first })
	waitUntil(t, "controls work again", func() bool { return r.SetVolume(ctx, 0.5) == nil })
	if lv, _ := dev.Volume(); lv != 0.5 {
		t.Fatalf("volume %v", lv)
	}
	if st := r.Status(); st.State != renderer.StatePlaying || st.Err != nil {
		t.Fatalf("status after reconnect: %+v", st)
	}

	// Media that vanished while the connection was down, far from its end, was interrupted...
	dev.Partition()
	dev.Finish()
	waitFor(t, r, "interrupted after reconnect", func(s renderer.Status) bool {
		return s.State == renderer.StateIdle && errors.Is(s.Err, renderer.ErrInterrupted)
	})
	// ... and near its end it finished.
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.SetPosition(595)
	waitFor(t, r, "near the end", func(s renderer.Status) bool { return s.Position >= 595 })
	dev.Partition()
	dev.Finish()
	st := waitFor(t, r, "ended after reconnect", state(renderer.StateEnded))
	if st.Err != nil || st.Position != 600 {
		t.Fatalf("%+v", st)
	}
}

func TestReconnectFailure(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{ConnectTimeout: 300 * time.Millisecond, ReconnectAttempts: 2}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.Silence(true)
	waitClosed(t, r.Changed())
	st := r.Status()
	if st.State != renderer.StateError || st.Err == nil || !strings.Contains(st.Err.Error(), "lost connection") {
		t.Fatalf("status after giving up: %+v", st)
	}
	if err := r.Pause(ctx); err == nil {
		t.Fatal("control after the connection was lost for good")
	}
	if err := r.Load(ctx, movie(mediaURL)); err == nil {
		t.Fatal("load after the connection was lost for good")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseStopsAppWithoutLeaks(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	d := testDriver(chromecast.Options{})
	base := runtime.NumGoroutine()
	rd := dev.RendererDevice()
	r, err := d.Connect(context.Background(), &rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Load(context.Background(), movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	dev.DropConnections() // exercise a reconnect before closing
	waitUntil(t, "a reconnect", func() bool { return r.SetMuted(context.Background(), false) == nil })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	waitClosed(t, r.Changed())
	if !slices.Contains(dev.Commands(), "receiver:STOP") {
		t.Fatalf("close did not stop the receiver app: %v", dev.Commands())
	}
	// The CLOSE messages are not answered: wait for the device to read them.
	waitUntil(t, "CLOSE of both virtual connections", func() bool { return dev.CountCommand("connection:CLOSE") == 2 })
	if app, _ := dev.App(); app != "" {
		t.Fatalf("receiver app still running: %s", app)
	}
	if err := r.Play(context.Background()); err == nil {
		t.Fatal("play after Close")
	}
	waitUntil(t, "the device to see the connection closed", func() bool { return dev.Connections() == 0 })
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d after Close, %d before\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostileDevice(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{})
	r := connect(t, testDriver(chromecast.Options{}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))
	apps := dev.ApplicationsJSON()
	// Malformed or absurd messages are ignored (or clamped) without disturbing playback.
	for _, p := range []string{
		`{"type":"RECEIVER_STATUS","status":{"applications":` + apps + `,"volume":{"level":1e999}}}`,
		`{"type":"RECEIVER_STATUS","status":{"applications":` + apps + `,"volume":{"level":7,"muted":"yes"}}}`,
		`{"type":"RECEIVER_STATUS","status":{"applications":` + apps + `,"volume":{"level":-3}}}`,
		`{"type":"RECEIVER_STATUS","status":{"applications":"none"}}`,
		`{"type":"RECEIVER_STATUS","requestId":-1,"status":null}`,
		`garbage`, `[]`, `null`, `{"type":`,
	} {
		dev.Send(chromecast.NamespaceReceiver, p)
	}
	for _, p := range []string{
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,"playerState":"PLAYING","currentTime":-50,"playbackRate":1e9,"media":{"duration":-1}}]}`,
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,"playerState":"WEIRD","currentTime":1e300}]}`,
		`{"type":"MEDIA_STATUS","status":"nope"}`,
		`{"type":"MEDIA_STATUS","requestId":99999999999,"status":[]}`,
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":"1","playerState":"IDLE","idleReason":"ERROR"}]}`,
		`{"type":"UNKNOWN"}`, `garbage`,
	} {
		dev.Send(chromecast.NamespaceMedia, p)
	}
	dev.Send(chromecast.NamespaceHeartbeat, `{"type":"PING","junk":[1,2,3]}`)
	dev.Send("urn:x-cast:unknown", `{"type":"WHATEVER"}`)
	waitFor(t, r, "volume clamped to 0", func(s renderer.Status) bool { return s.Volume == 0 })
	st := r.Status()
	if st.State != renderer.StatePlaying || st.Err != nil || st.Duration != 600 || st.Position < 0 || st.Position > 600 {
		t.Fatalf("status after hostile messages: %+v", st)
	}
	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "paused", state(renderer.StatePaused))

	// The device closing the platform connection (CLOSE from receiver-0) makes us reconnect.
	before := dev.CountCommand("receiver:GET_STATUS")
	dev.Send(chromecast.NamespaceConnection, `{"type":"CLOSE","reasonCode":"nope"}`)
	waitUntil(t, "a reconnect after CLOSE", func() bool { return dev.CountCommand("receiver:GET_STATUS") > before })
	waitUntil(t, "controls after reconnecting", func() bool { return r.Play(ctx) == nil })
	waitFor(t, r, "playing", state(renderer.StatePlaying))

	// A well-formed status that no longer lists our app means it is gone.
	dev.Send(chromecast.NamespaceReceiver, `{"type":"RECEIVER_STATUS","status":{"applications":[{"appId":"CC1AD845","sessionId":"bad id","transportId":""}]}}`)
	waitFor(t, r, "interrupted", func(s renderer.Status) bool { return errors.Is(s.Err, renderer.ErrInterrupted) })
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, "playing", state(renderer.StatePlaying))

	// An oversized frame header kills the connection; the renderer reconnects.
	before = dev.CountCommand("receiver:GET_STATUS")
	dev.SendRawFrame([]byte{0x7f, 0xff, 0xff, 0xff})
	waitUntil(t, "a reconnect after an oversized frame", func() bool { return dev.CountCommand("receiver:GET_STATUS") > before })
	waitUntil(t, "controls after reconnecting", func() bool { return r.Play(ctx) == nil })
	waitFor(t, r, "playing", state(renderer.StatePlaying))
}

func TestConnectFailures(t *testing.T) {
	d := testDriver(chromecast.Options{ConnectTimeout: 300 * time.Millisecond})
	ctx := context.Background()
	if _, err := d.Connect(ctx, &renderer.Device{Name: "x"}, nil); err == nil {
		t.Fatal("device without a _googlecast endpoint")
	}
	if _, err := d.Connect(ctx, nil, nil); err == nil {
		t.Fatal("nil device")
	}

	// Nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dev := &renderer.Device{Endpoints: []renderer.Endpoint{{Service: renderer.ServiceGoogleCast, Host: "127.0.0.1", Port: port}}}
	if _, err := d.Connect(ctx, dev, nil); err == nil {
		t.Fatal("connect to a closed port")
	}

	// A peer that accepts TCP but never speaks TLS: the ctx bounds the attempt.
	mute, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mute.Close() })
	var conns sync.WaitGroup
	conns.Add(1)
	go func() {
		defer conns.Done()
		c, err := mute.Accept()
		if err == nil {
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	dev.Endpoints[0].Port = mute.Addr().(*net.TCPAddr).Port
	slow := testDriver(chromecast.Options{ConnectTimeout: 10 * time.Second})
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	start := time.Now()
	_, err = slow.Connect(cctx, dev, nil)
	cancel()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("connect to a mute peer: %v after %s", err, time.Since(start))
	}
	conns.Wait()

	// A device that never answers GET_STATUS.
	silent := chromecasttest.Start(t, chromecasttest.Options{})
	silent.Silence(true)
	rd := silent.RendererDevice()
	start = time.Now()
	if _, err := d.Connect(ctx, &rd, nil); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("connect to a silent device: %v after %s", err, time.Since(start))
	}
}

// TestConcurrentUse exercises the renderer from several goroutines (run with -race).
func TestConcurrentUse(t *testing.T) {
	dev := chromecasttest.Start(t, chromecasttest.Options{PingInterval: 10 * time.Millisecond})
	r := connect(t, testDriver(chromecast.Options{StatusInterval: 10 * time.Millisecond}), dev)
	ctx := context.Background()
	if err := r.Load(ctx, movie(mediaURL)); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-r.Changed():
				default:
				}
				_ = r.Status()
				_ = r.(renderer.Addresser).RemoteAddr()
			}
		}()
	}
	for i := 0; i < 5; i++ {
		_ = r.Pause(ctx)
		_ = r.Play(ctx)
		_ = r.SetVolume(ctx, float64(i)/10)
		_ = r.Seek(ctx, float64(i*10))
	}
	if err := r.Load(ctx, movie(mediaURL+"?again")); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	_ = r.Close()
}
