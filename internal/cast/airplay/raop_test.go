package airplay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ludworks/internal/cast/renderer"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// testRSAKey stands in for the AirPort Express key pair in the tests.
func testRSAKey(t *testing.T) *rsa.PrivateKey {
	testKeyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKey
}

// rampSource is PCM whose every frame names its own index (from the item start):
// left = low 16 bits, right = high 16 bits.
type rampSource struct {
	pos, total int64
	closed     atomic.Bool
}

func (s *rampSource) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for n+4 <= len(p) && s.pos < s.total {
		binary.LittleEndian.PutUint16(p[n:], uint16(s.pos))
		binary.LittleEndian.PutUint16(p[n+2:], uint16(s.pos>>16))
		s.pos++
		n += 4
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (s *rampSource) Close() error { s.closed.Store(true); return nil }

func frameIndex(pcm []byte) int64 {
	return int64(binary.LittleEndian.Uint16(pcm))<<0 | int64(binary.LittleEndian.Uint16(pcm[2:]))<<16
}

// rampPCM returns a LoadRequest.PCM of total frames that records where it was opened.
func rampPCM(total int64, opened *[]float64, mu *sync.Mutex) func(context.Context, float64) (io.ReadCloser, error) {
	return func(_ context.Context, start float64) (io.ReadCloser, error) {
		mu.Lock()
		*opened = append(*opened, start)
		mu.Unlock()
		return &rampSource{pos: int64(math.Round(start * rtpSampleRate)), total: total}, nil
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitState(t *testing.T, r renderer.Renderer, timeout time.Duration, state string) renderer.Status {
	t.Helper()
	var st renderer.Status
	waitFor(t, timeout, "state "+state, func() bool {
		st = r.Status()
		if st.State == renderer.StateError && state != renderer.StateError {
			t.Fatalf("renderer failed: %v", st.Err)
		}
		return st.State == state
	})
	return st
}

var homePodTXT = map[string]string{"et": "0,3,5", "cn": "0,1,2,3", "md": "0,1,2", "tp": "UDP", "am": "AudioAccessory5,1", "sf": "0x4"}

func connectRAOP(t *testing.T, fake *fakeRAOP, txt map[string]string) (*Driver, renderer.Renderer) {
	t.Helper()
	d := New(Options{Name: "Test NAS"})
	d.rsaKey = &fake.priv.PublicKey
	dev := &renderer.Device{ID: "airplay-fake", Kind: renderer.KindAirPlay, Name: "Fake", Endpoints: []renderer.Endpoint{fake.endpoint(txt)}}
	r, err := d.Connect(context.Background(), dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return d, r
}

// segment returns the packets from sequence number from (a RECORD's or FLUSH's RTP-Info)
// up to, not including, until (0 = no end), in sequence order. A receiver tells the
// segments apart the same way: packets before a FLUSH's seq are dropped even when they
// arrive after it.
func segment(pkts []fakePacket, from, until uint32) []fakePacket {
	var out []fakePacket
	for _, p := range pkts {
		off := p.seq - uint16(from)
		if off >= 0x8000 || (until != 0 && off >= uint16(until)-uint16(from)) {
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b fakePacket) int { return int(a.seq-uint16(from)) - int(b.seq-uint16(from)) })
	return out
}

// checkStream verifies RTP sequence numbers, timestamps, markers and SSRC of the packets
// of one segment, starting at seq/ts.
func checkStream(t *testing.T, pkts []fakePacket, seq uint16, ts uint32) {
	t.Helper()
	if len(pkts) == 0 {
		t.Fatal("no packets")
	}
	for i, p := range pkts {
		if p.seq != seq+uint16(i) || p.ts != ts+uint32(i*352) || p.marker != (i == 0) || p.ssrc != pkts[0].ssrc {
			t.Fatalf("packet %d: seq %d ts %d marker %v ssrc %x; want seq %d ts %d", i, p.seq, p.ts, p.marker, p.ssrc, seq+uint16(i), ts+uint32(i*352))
		}
	}
}

func TestRAOPStreamsItem(t *testing.T) {
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{7}, 5000)...)
	for _, tc := range []struct {
		name    string
		txt     map[string]string
		encrypt bool
	}{
		{"unencrypted", homePodTXT, false},
		{"rsa-aes", map[string]string{"et": "0,1", "ek": "1", "cn": "0,1", "md": "0,1,2", "tp": "UDP", "ch": "2", "sr": "44100", "ss": "16"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRAOP(t, testRSAKey(t))
			_, r := connectRAOP(t, fake, tc.txt)
			ctx := context.Background()
			if err := r.SetVolume(ctx, 0.5); err != nil {
				t.Fatal(err)
			}
			const frames = 44100
			var opened []float64
			var mu sync.Mutex
			req := &renderer.LoadRequest{
				Title: "Song", Artist: "Artist", Album: "Album", DurationSeconds: 1,
				PCM: rampPCM(frames, &opened, &mu),
				Art: func(context.Context) ([]byte, error) { return jpeg, nil },
			}
			if err := r.Load(ctx, req); err != nil {
				t.Fatal(err)
			}
			st := waitState(t, r, 3*time.Second, renderer.StatePlaying)
			if st.Duration != 1 || st.Volume != 0.5 {
				t.Errorf("status %+v", st)
			}
			st = waitState(t, r, 5*time.Second, renderer.StateEnded)
			if math.Abs(st.Position-1) > 0.001 {
				t.Errorf("ended at %v s", st.Position)
			}
			waitFor(t, 2*time.Second, "artwork", func() bool { return len(fake.snapshot().artwork) == 1 })
			snap := fake.snapshot()
			if len(snap.problems) > 0 {
				t.Fatalf("protocol problems: %q", snap.problems)
			}
			if !snap.timingOK {
				t.Error("timing request not answered before SETUP")
			}
			var methods []string
			for _, r := range snap.requests {
				m, uri, _ := strings.Cut(r, " ")
				if m == "POST" {
					m += " " + uri
				}
				methods = append(methods, m)
			}
			if !slices.Equal(methods[:6], []string{"OPTIONS", "ANNOUNCE", "SETUP", "RECORD", "POST /feedback", "SET_PARAMETER"}) {
				t.Errorf("requests %q", methods)
			}
			if (snap.key != nil) != tc.encrypt {
				t.Errorf("encrypted %v, want %v", snap.key != nil, tc.encrypt)
			}
			if !slices.Equal(snap.volumes, []string{"-15.000000"}) {
				t.Errorf("volumes %q", snap.volumes)
			}
			if len(snap.metadata) != 1 || snap.metadata[0]["minm"] != "Song" || snap.metadata[0]["asar"] != "Artist" || snap.metadata[0]["asal"] != "Album" {
				t.Errorf("metadata %v", snap.metadata)
			}
			if !bytes.Equal(snap.artwork[0], jpeg) {
				t.Error("artwork differs")
			}
			if len(snap.progress) != 1 {
				t.Fatalf("progress %q", snap.progress)
			}
			var start, cur, end uint32
			fmt.Sscanf(snap.progress[0], "%d/%d/%d", &start, &cur, &end)
			if start != snap.record.rtp || cur != start || end-start != 44100 {
				t.Errorf("progress %q with RECORD rtptime %d", snap.progress[0], snap.record.rtp)
			}

			pkts := segment(snap.packets, snap.record.seq, 0)
			if len(pkts) != len(snap.packets) {
				t.Fatalf("%d packets outside the stream", len(snap.packets)-len(pkts))
			}
			checkStream(t, pkts, uint16(snap.record.seq), snap.record.rtp)
			var pcm []byte
			for _, p := range pkts {
				b, err := fake.pcm(p)
				if err != nil {
					t.Fatalf("packet %d: %v", p.seq, err)
				}
				pcm = append(pcm, b...)
			}
			for i := range int64(frames) {
				if got := frameIndex(pcm[4*i:]); got != i {
					t.Fatalf("frame %d carries %d", i, got)
				}
			}
			if !bytes.Equal(pcm[4*frames:], make([]byte, len(pcm)-4*frames)) {
				t.Error("audio after the end is not silence")
			}
			if pad := len(pcm)/4 - frames; pad < 22050 {
				t.Errorf("only %d frames of silence after the end", pad)
			}

			if len(snap.syncs) < 2 || snap.syncs[0][0] != 0x90 || snap.syncs[1][0] != 0x80 {
				t.Fatalf("sync packets %d", len(snap.syncs))
			}
			for i, s := range snap.syncs {
				playing, next := binary.BigEndian.Uint32(s[4:]), binary.BigEndian.Uint32(s[16:])
				if next-playing != 11025 || s[2] != 0 || s[3] != 7 {
					t.Errorf("sync %d: % x", i, s)
				}
				if d := time.Since(ntpTime(binary.BigEndian.Uint64(s[8:])).time()); d < 0 || d > 10*time.Second {
					t.Errorf("sync %d NTP time off by %v", i, d)
				}
			}
			if first := binary.BigEndian.Uint32(snap.syncs[0][16:]); first != snap.record.rtp {
				t.Errorf("first sync at RTP %d, stream starts at %d", first, snap.record.rtp)
			}
			// All syncs describe one clock: RTP time advances 44100 per NTP second.
			t0 := ntpTime(binary.BigEndian.Uint64(snap.syncs[0][8:])).time()
			for i, s := range snap.syncs[1:] {
				dt := ntpTime(binary.BigEndian.Uint64(s[8:])).time().Sub(t0).Seconds()
				dr := binary.BigEndian.Uint32(s[16:]) - snap.record.rtp
				if math.Abs(float64(dr)-dt*44100) > 1 {
					t.Errorf("sync %d: %d frames after the first at %.6f s", i+1, dr, dt)
				}
			}

			r.Close()
			waitFor(t, 2*time.Second, "TEARDOWN", func() bool {
				reqs := fake.snapshot().requests
				return len(reqs) > 0 && strings.HasPrefix(reqs[len(reqs)-1], "TEARDOWN ")
			})
			assertClosed(t, r.Changed())
		})
	}
}

func TestRAOPPauseResumeSeek(t *testing.T) {
	fake := newFakeRAOP(t, testRSAKey(t))
	_, r := connectRAOP(t, fake, homePodTXT)
	ctx := context.Background()
	var opened []float64
	var mu sync.Mutex
	if err := r.Load(ctx, &renderer.LoadRequest{DurationSeconds: 10, PCM: rampPCM(10*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
	time.Sleep(300 * time.Millisecond)

	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	if st.State != renderer.StatePaused || st.Position <= 0 || st.Position > 1 {
		t.Fatalf("paused status %+v", st)
	}
	pausedFrame := int64(math.Round(st.Position * rtpSampleRate))
	time.Sleep(300 * time.Millisecond)
	if again := r.Status(); again.Position != st.Position {
		t.Errorf("position moved while paused: %v → %v", st.Position, again.Position)
	}
	if err := r.Pause(ctx); err != nil { // pausing twice is harmless
		t.Fatal(err)
	}
	snap := fake.snapshot()
	if len(snap.flushes) != 1 {
		t.Fatalf("flushes %v", snap.flushes)
	}

	if err := r.Play(ctx); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
	resumedFrom := snap.flushes[0].seq
	waitFor(t, 2*time.Second, "audio after resume", func() bool {
		return len(segment(fake.snapshot().packets, resumedFrom, 0)) > 0
	})
	snap = fake.snapshot()
	resumed := segment(snap.packets, resumedFrom, 0)
	checkStream(t, resumed, uint16(snap.flushes[0].seq), snap.flushes[0].rtp)
	pcm, err := fake.pcm(resumed[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := frameIndex(pcm); got != pausedFrame {
		t.Errorf("resumed at frame %d, paused at %d", got, pausedFrame)
	}
	firstSyncs := 0
	for _, s := range snap.syncs {
		if s[0] == 0x90 {
			firstSyncs++
		}
	}
	if firstSyncs != 2 {
		t.Errorf("%d sync packets with the extension bit, want 2", firstSyncs)
	}

	if err := r.Seek(ctx, 3); err != nil {
		t.Fatal(err)
	}
	snap = fake.snapshot()
	if len(snap.flushes) != 2 {
		t.Fatalf("flushes %v", snap.flushes)
	}
	seekedFrom := snap.flushes[1].seq
	waitFor(t, 2*time.Second, "audio after seek", func() bool {
		return len(segment(fake.snapshot().packets, seekedFrom, 0)) > 0
	})
	snap = fake.snapshot()
	if before := segment(snap.packets, resumedFrom, seekedFrom); len(before) == 0 || before[len(before)-1].seq != uint16(seekedFrom)-1 {
		t.Errorf("the seek FLUSH does not name the next sequence number")
	}
	seeked := segment(snap.packets, seekedFrom, 0)
	checkStream(t, seeked, uint16(seekedFrom), snap.flushes[1].rtp)
	if pcm, err := fake.pcm(seeked[0]); err != nil {
		t.Fatal(err)
	} else if got := frameIndex(pcm); got != 3*44100 {
		t.Errorf("first packet after seek: frame %d", got)
	}
	mu.Lock()
	if !slices.Equal(opened, []float64{0, 3}) {
		t.Errorf("PCM opened at %v", opened)
	}
	mu.Unlock()
	if st := r.Status(); st.Position < 3 || st.Position > 4 {
		t.Errorf("position after seek %v", st.Position)
	}

	// Seeking while paused stays paused at the target.
	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Seek(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.State != renderer.StatePaused || st.Position != 5 {
		t.Errorf("after a seek while paused: %+v", st)
	}

	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.State != renderer.StateIdle {
		t.Errorf("after stop: %+v", st)
	}
	snap = fake.snapshot()
	if last := snap.requests[len(snap.requests)-1]; !strings.HasPrefix(last, "TEARDOWN ") {
		t.Errorf("last request %q", last)
	}
	if len(snap.problems) > 0 {
		t.Errorf("protocol problems: %q", snap.problems)
	}
}

func TestRAOPRetransmits(t *testing.T) {
	fake := newFakeRAOP(t, testRSAKey(t))
	_, r := connectRAOP(t, fake, homePodTXT)
	var opened []float64
	var mu sync.Mutex
	if err := r.Load(context.Background(), &renderer.LoadRequest{PCM: rampPCM(5*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "packets", func() bool { return len(fake.snapshot().packets) > 10 })
	pkts := fake.snapshot().packets
	fake.requestResend(pkts[3].seq, 2)
	waitFor(t, 2*time.Second, "retransmissions", func() bool { return len(fake.snapshot().resent) == 2 })
	for i, re := range fake.snapshot().resent {
		if re[0] != 0x80 || re[1] != 0xD6 || !bytes.Equal(re[4:], pkts[3+i].raw) {
			t.Errorf("retransmission %d: % x…", i, re[:16])
		}
	}
	fake.requestResend(pkts[0].seq-1000, 5) // long gone: nothing to send
	time.Sleep(100 * time.Millisecond)
	if n := len(fake.snapshot().resent); n != 2 {
		t.Errorf("%d retransmissions", n)
	}
}

func TestRAOPReceiverRefusals(t *testing.T) {
	ctx := context.Background()
	var opened []float64
	var mu sync.Mutex
	req := func() *renderer.LoadRequest { return &renderer.LoadRequest{PCM: rampPCM(44100, &opened, &mu)} }

	t.Run("password", func(t *testing.T) {
		fake := newFakeRAOP(t, testRSAKey(t))
		fake.password = true
		_, r := connectRAOP(t, fake, homePodTXT)
		err := r.Load(ctx, req())
		if !errors.Is(err, renderer.ErrPairingUnsupported) {
			t.Fatalf("err %v", err)
		}
		if st := r.Status(); st.State != renderer.StateError || st.Err == nil {
			t.Errorf("status %+v", st)
		}
	})
	t.Run("auth-setup after 403", func(t *testing.T) {
		fake := newFakeRAOP(t, testRSAKey(t))
		fake.needAuthSetup = true
		_, r := connectRAOP(t, fake, map[string]string{"et": "0,4", "cn": "0,1", "md": "0,2", "am": "Speaker1,1"})
		if err := r.Load(ctx, req()); err != nil {
			t.Fatal(err)
		}
		snap := fake.snapshot()
		announces := 0
		for _, r := range snap.requests {
			if strings.HasPrefix(r, "ANNOUNCE") {
				announces++
			}
		}
		if announces != 2 || len(snap.authSetup) != 1 || len(snap.authSetup[0]) != 33 || snap.authSetup[0][0] != 1 {
			t.Errorf("%d ANNOUNCEs, auth-setup %d", announces, len(snap.authSetup))
		}
	})
	t.Run("auth-setup first for AirPort Express", func(t *testing.T) {
		fake := newFakeRAOP(t, testRSAKey(t))
		fake.needAuthSetup = true
		_, r := connectRAOP(t, fake, map[string]string{"et": "0,4", "cn": "0,1", "am": "AirPort10,115"})
		if err := r.Load(ctx, req()); err != nil {
			t.Fatal(err)
		}
		snap := fake.snapshot()
		if snap.requests[1] != "POST /auth-setup" || !strings.HasPrefix(snap.requests[2], "ANNOUNCE") {
			t.Errorf("requests %q", snap.requests)
		}
	})
	t.Run("older receiver", func(t *testing.T) {
		fake := newFakeRAOP(t, testRSAKey(t))
		fake.noAudioLatency, fake.noPost = true, true
		_, r := connectRAOP(t, fake, map[string]string{"et": "0", "cn": "1"})
		if err := r.Load(ctx, req()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 2*time.Second, "sync", func() bool { return len(fake.snapshot().syncs) > 0 })
		snap := fake.snapshot()
		s := snap.syncs[0]
		if got := binary.BigEndian.Uint32(s[16:]) - binary.BigEndian.Uint32(s[4:]); got != 77175 {
			t.Errorf("sync latency %d", got)
		}
		if len(snap.problems) > 0 {
			t.Errorf("problems %q", snap.problems)
		}
	})
	t.Run("feedback refused", func(t *testing.T) {
		fake := newFakeRAOP(t, testRSAKey(t))
		fake.noFeedback = true
		_, r := connectRAOP(t, fake, homePodTXT)
		if err := r.Load(ctx, req()); err != nil {
			t.Fatal(err)
		}
		waitState(t, r, 3*time.Second, renderer.StatePlaying)
	})
}

func TestRAOPVolume(t *testing.T) {
	ctx := context.Background()
	var opened []float64
	var mu sync.Mutex
	fake := newFakeRAOP(t, testRSAKey(t))
	fake.volume = "-15.000000"
	_, r := connectRAOP(t, fake, homePodTXT)
	if st := r.Status(); st.Volume != 1 {
		t.Errorf("volume before any session %v", st.Volume)
	}
	if err := r.Load(ctx, &renderer.LoadRequest{PCM: rampPCM(5*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Volume != 0.5 || st.Muted {
		t.Errorf("learned volume %v muted %v", st.Volume, st.Muted)
	}
	if err := r.SetVolume(ctx, 0.8); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMuted(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMuted(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.snapshot().volumes; !slices.Equal(got, []string{"-6.000000", "-144.000000", "-6.000000"}) {
		t.Errorf("volumes %q", got)
	}
	if st := r.Status(); st.Volume != 0.8 || st.Muted {
		t.Errorf("status %+v", st)
	}
}

func TestRAOPHostileReceiver(t *testing.T) {
	ctx := context.Background()
	var opened []float64
	var mu sync.Mutex
	fake := newFakeRAOP(t, testRSAKey(t))
	fake.garbage = "RTSP/1.0 200 OK\r\nX: " + strings.Repeat("a", 100000) + "\r\n\r\n"
	_, r := connectRAOP(t, fake, homePodTXT)
	start := time.Now()
	if err := r.Load(ctx, &renderer.LoadRequest{PCM: rampPCM(44100, &opened, &mu)}); err == nil {
		t.Fatal("loaded from a receiver answering garbage")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v", el)
	}

	// A receiver that accepts the connection and never answers.
	silent := newSilentServer(t)
	d := New(Options{})
	dev := &renderer.Device{Name: "Silent", Endpoints: []renderer.Endpoint{silent.endpoint(renderer.ServiceRAOP, homePodTXT)}}
	r2, err := d.Connect(ctx, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := r2.Load(tctx, &renderer.LoadRequest{PCM: rampPCM(44100, &opened, &mu)}); err == nil {
		t.Fatal("loaded from a silent receiver")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("silent receiver took %v", el)
	}
}

func TestRAOPConnectionLostAndNextItem(t *testing.T) {
	fake := newFakeRAOP(t, testRSAKey(t))
	_, r := connectRAOP(t, fake, homePodTXT)
	ctx := context.Background()
	var opened []float64
	var mu sync.Mutex
	if err := r.Load(ctx, &renderer.LoadRequest{Title: "One", PCM: rampPCM(10*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
	// The next item replaces the first in the same session.
	if err := r.Load(ctx, &renderer.LoadRequest{Title: "Two", PCM: rampPCM(10*44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
	snap := fake.snapshot()
	if len(snap.flushes) != 1 || len(snap.metadata) != 2 || snap.metadata[1]["minm"] != "Two" {
		t.Errorf("flushes %v metadata %v", snap.flushes, snap.metadata)
	}
	announces := 0
	for _, r := range snap.requests {
		if strings.HasPrefix(r, "ANNOUNCE") {
			announces++
		}
	}
	if announces != 1 {
		t.Errorf("%d ANNOUNCEs for two items", announces)
	}

	// The receiver closing the connection (another sender took over) interrupts playback.
	fake.dropConnections()
	st := waitState(t, r, 6*time.Second, renderer.StateError)
	if !errors.Is(st.Err, renderer.ErrInterrupted) {
		t.Errorf("lost connection reported as %v", st.Err)
	}
	// The renderer recovers with a new session.
	if err := r.Load(ctx, &renderer.LoadRequest{Title: "Three", PCM: rampPCM(44100, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, 3*time.Second, renderer.StatePlaying)
}

func TestRAOPRejectsUnsupportedItems(t *testing.T) {
	fake := newFakeRAOP(t, testRSAKey(t))
	_, r := connectRAOP(t, fake, homePodTXT)
	ctx := context.Background()
	if err := r.Load(ctx, &renderer.LoadRequest{Title: "no pcm"}); !errors.Is(err, renderer.ErrUnsupported) {
		t.Errorf("no PCM: %v", err)
	}
	if err := r.Load(ctx, &renderer.LoadRequest{Video: true, URL: "http://x/v.mp4"}); !errors.Is(err, renderer.ErrUnsupported) {
		t.Errorf("video on a speaker: %v", err)
	}
	if err := r.Load(ctx, nil); !errors.Is(err, renderer.ErrUnsupported) {
		t.Errorf("nil item: %v", err)
	}
	var opened []float64
	var mu sync.Mutex
	if err := r.Load(ctx, &renderer.LoadRequest{Live: true, PCM: rampPCM(44100*5, &opened, &mu)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Seek(ctx, 1); !errors.Is(err, renderer.ErrNotSeekable) {
		t.Errorf("seek in a live item: %v", err)
	}
	if st := r.Status(); st.Duration != 0 {
		t.Errorf("live duration %v", st.Duration)
	}
}

// silentServer accepts connections and never answers.
type silentServer struct{ addr string }

func newSilentServer(t *testing.T) *silentServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return &silentServer{addr: ln.Addr().String()}
}

func (s *silentServer) endpoint(service string, txt map[string]string) renderer.Endpoint {
	host, port, _ := net.SplitHostPort(s.addr)
	p, _ := strconv.Atoi(port)
	return renderer.Endpoint{Service: service, Host: host, Port: p, TXT: txt}
}

// assertClosed checks that Changed was closed.
func assertClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Changed not closed")
		}
	}
}
