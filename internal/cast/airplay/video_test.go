package airplay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/cast/renderer"
)

var (
	appleTV4KTXT = map[string]string{"features": "0x4A7FFFF7,0x3C155FDE", "flags": "0x244", "model": "AppleTV6,2",
		"deviceid": "AA:BB:CC:DD:EE:01", "srcvers": "690.7.1", "pk": "00"}
	openAppleTVTXT = map[string]string{"features": "0x4A7FFFF7,0x3C155FDE", "flags": "0x44", "model": "AppleTV6,2"}
	legacyATVTXT   = map[string]string{"features": "0x5A7FFFF7,0xE", "flags": "0x44", "model": "AppleTV3,1"}
)

func atvDevice(eps ...renderer.Endpoint) *renderer.Device {
	return &renderer.Device{ID: "airplay-AABBCCDDEE01", Kind: renderer.KindAirPlay, Name: "Living Room TV", Model: "AppleTV6,2", Endpoints: eps}
}

func videoItem() *renderer.LoadRequest {
	return &renderer.LoadRequest{Video: true, URL: "http://127.0.0.1:18189/api/v1/cast/media/TICKET/media/m1/stream",
		ContentType: "video/mp4", StartSeconds: 0.5, DurationSeconds: 3, Title: "Movie"}
}

func TestAirPlayVideoPairingAndPlayback(t *testing.T) {
	f := newFakeAppleTV(t)
	f.modern, f.requireAuth = true, true
	d := New(Options{Name: "Test NAS"})
	dev := atvDevice(f.endpoint(appleTV4KTXT))
	if caps := d.Describe(dev); caps != (renderer.Capabilities{Video: true, RequiresPairing: true, PairingSupported: true}) {
		t.Fatalf("capabilities %+v", caps)
	}
	ctx := context.Background()

	r, err := d.Connect(ctx, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Load(ctx, videoItem()); !errors.Is(err, renderer.ErrPairingRequired) {
		t.Fatalf("unpaired load: %v", err)
	}
	r.Close()

	// A wrong PIN, then the right one.
	p, err := d.StartPairing(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if f.snapshot().pinShown != 1 {
		t.Fatal("no PIN shown")
	}
	if _, err := p.Confirm(ctx, "0000"); !errors.Is(err, renderer.ErrWrongPIN) {
		t.Fatalf("wrong PIN: %v", err)
	}
	if _, err := p.Confirm(ctx, "0000"); err == nil {
		t.Error("a pairing confirmed twice")
	}
	p.Close()
	p, err = d.StartPairing(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := p.Confirm(ctx, f.snapshot().pin)
	p.Close()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := parseCredentials(creds); err != nil || string(c.deviceID) != f.id {
		t.Fatalf("credentials %v for %q", err, c.deviceID)
	}

	r, err = d.Connect(ctx, dev, creds)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Load(ctx, videoItem()); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.State != renderer.StateLoading {
		t.Errorf("right after load: %+v", st)
	}
	st := waitState(t, r, 5*time.Second, renderer.StatePlaying)
	if st.Duration != 3 || st.Position < 0.5 {
		t.Errorf("playing status %+v", st)
	}
	snap := f.snapshot()
	if len(snap.plays) != 1 || snap.plays[0]["Content-Location"] != videoItem().URL || snap.plays[0]["Start-Position-Seconds"] != 0.5 {
		t.Fatalf("/play %v", snap.plays)
	}
	if len(snap.setups) != 1 || !slices.Equal(snap.rates, []string{"1.000000"}) {
		t.Errorf("setups %d rates %q", len(snap.setups), snap.rates)
	}
	waitFor(t, 3*time.Second, "event channel ack and timing reply", func() bool {
		s := f.snapshot()
		return s.eventAcks == 1 && s.timingOK
	})

	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.State != renderer.StatePaused {
		t.Errorf("after pause %+v", st)
	}
	if err := r.Play(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Seek(ctx, 2.5); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Position != 2.5 {
		t.Errorf("after seek %+v", st)
	}
	if err := r.SetVolume(ctx, 0.5); err != nil {
		t.Fatal(err)
	}
	snap = f.snapshot()
	if !slices.Equal(snap.rates, []string{"1.000000", "0.000000", "1.000000"}) || !slices.Equal(snap.scrubs, []string{"2.500000"}) ||
		!slices.Equal(snap.volumes, []string{"-15.000000"}) {
		t.Errorf("rates %q scrubs %q volumes %q", snap.rates, snap.scrubs, snap.volumes)
	}
	waitState(t, r, 5*time.Second, renderer.StateEnded)

	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	snap = f.snapshot()
	if snap.stops != 1 || len(snap.problems) > 0 {
		t.Errorf("stops %d problems %q", snap.stops, snap.problems)
	}
	if last := snap.requests[len(snap.requests)-1]; !strings.HasPrefix(last, "TEARDOWN ") {
		t.Errorf("last request %q", last)
	}
}

func TestAirPlayVideoTransientPairing(t *testing.T) {
	f := newFakeAppleTV(t)
	f.modern, f.requireAuth, f.allowTransient = true, true, true
	d := New(Options{})
	dev := atvDevice(f.endpoint(openAppleTVTXT))
	if caps := d.Describe(dev); caps.RequiresPairing || !caps.Video || !caps.PairingSupported {
		t.Fatalf("capabilities %+v", caps)
	}
	r, err := d.Connect(context.Background(), dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Load(context.Background(), videoItem()); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 5*time.Second, renderer.StatePlaying)
	if s := f.snapshot(); s.pinShown != 0 || len(s.problems) > 0 {
		t.Errorf("PIN shown %d times, problems %q", s.pinShown, s.problems)
	}
}

func TestAirPlayVideoLegacyReceiver(t *testing.T) {
	f := newFakeAppleTV(t)
	d := New(Options{})
	dev := atvDevice(f.endpoint(legacyATVTXT))
	if caps := d.Describe(dev); caps != (renderer.Capabilities{Video: true}) {
		t.Fatalf("capabilities %+v", caps)
	}
	r, err := d.Connect(context.Background(), dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Load(context.Background(), videoItem()); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 5*time.Second, renderer.StatePlaying)
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := f.snapshot()
	start, _ := s.plays[0]["Start-Position"].(float64)
	if math.Abs(start-0.5/3) > 1e-9 || len(s.setups) != 0 || !slices.Equal(s.rates, []string{"0.000000"}) {
		t.Errorf("legacy /play %v setups %d rates %q", s.plays[0], len(s.setups), s.rates)
	}
	if err := r.SetVolume(context.Background(), 0.3); !errors.Is(err, renderer.ErrUnsupported) {
		t.Errorf("legacy video volume: %v", err)
	}
}

func TestAirPlayVideoCredentialProblems(t *testing.T) {
	f := newFakeAppleTV(t)
	f.modern, f.requireAuth = true, true
	d := New(Options{})
	dev := atvDevice(f.endpoint(appleTV4KTXT))
	ctx := context.Background()
	if _, err := d.Connect(ctx, dev, []byte{9, 9, 9}); !errors.Is(err, renderer.ErrPairingRequired) {
		t.Errorf("corrupt credentials: %v", err)
	}
	// Credentials for a device that no longer knows us (or another device).
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	devPK, _, _ := ed25519.GenerateKey(rand.Reader)
	stale := (&credentials{id: []byte("CONTROLLER"), key: key, deviceID: []byte(f.id), devicePK: devPK}).marshal()
	r, err := d.Connect(ctx, dev, stale)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Load(ctx, videoItem()); !errors.Is(err, renderer.ErrPairingRequired) {
		t.Errorf("stale credentials: %v", err)
	}
}

func TestAirPlayVideoStoppedOnDevice(t *testing.T) {
	f := newFakeAppleTV(t)
	f.duration = 60
	d := New(Options{})
	r, err := d.Connect(context.Background(), atvDevice(f.endpoint(legacyATVTXT)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ad, ok := r.(renderer.Addresser)
	if !ok {
		t.Fatal("renderer does not implement renderer.Addresser")
	}
	if ad.LocalAddr().String() == "" || ad.RemoteAddr().String() != f.ln.Addr().String() {
		t.Errorf("addresses %v → %v, device at %v", ad.LocalAddr(), ad.RemoteAddr(), f.ln.Addr())
	}
	item := videoItem()
	item.DurationSeconds = 60
	if err := r.Load(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 5*time.Second, renderer.StatePlaying)
	f.stopOnDevice()
	st := waitState(t, r, 5*time.Second, renderer.StateIdle)
	if !errors.Is(st.Err, renderer.ErrInterrupted) {
		t.Errorf("stopped on the device: %+v", st)
	}
}

func TestAirPlayVideoPlaybackError(t *testing.T) {
	f := newFakeAppleTV(t)
	f.failPlayback = true
	d := New(Options{})
	r, err := d.Connect(context.Background(), atvDevice(f.endpoint(legacyATVTXT)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Load(context.Background(), videoItem()); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, r, 5*time.Second, renderer.StateError)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "cannot play") {
		t.Errorf("error %v", st.Err)
	}
}

// An Apple TV with both endpoints plays audio over RAOP and video with /play, one at a time.
func TestAppleTVAudioThenVideo(t *testing.T) {
	fr := newFakeRAOP(t, testRSAKey(t))
	fa := newFakeAppleTV(t)
	fa.modern, fa.requireAuth, fa.allowTransient = true, true, true
	d := New(Options{})
	d.rsaKey = &testRSAKey(t).PublicKey
	raopTXT := map[string]string{"et": "0,3,5", "cn": "0,1,2,3", "md": "0,1,2", "tp": "UDP", "am": "AppleTV6,2", "sf": "0x44",
		"ft": "0x4A7FFFF7,0x3C155FDE"}
	dev := atvDevice(fr.endpoint(raopTXT), fa.endpoint(openAppleTVTXT))
	if caps := d.Describe(dev); !caps.Audio || !caps.Video {
		t.Fatalf("capabilities %+v", caps)
	}
	ctx := context.Background()
	r, err := d.Connect(ctx, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var opened []float64
	var mu sync.Mutex
	if err := r.Load(ctx, &renderer.LoadRequest{Title: "Track", PCM: rampPCM(10*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
	if err := r.Load(ctx, videoItem()); err != nil {
		t.Fatal(err)
	}
	reqs := fr.snapshot().requests
	if last := reqs[len(reqs)-1]; !strings.HasPrefix(last, "TEARDOWN ") {
		t.Errorf("RAOP session not torn down for the video: %q", reqs)
	}
	waitState(t, r, 5*time.Second, renderer.StatePlaying)
	if len(fa.snapshot().plays) != 1 {
		t.Error("video not sent to the TV")
	}
}
