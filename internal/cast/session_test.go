package cast

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/music"
)

// control posts a control action and returns the session.
func (v *env) control(c *authtest.Client, id string, body map[string]any) sessionResp {
	v.t.Helper()
	return decodeSession(v.t, c.Do("POST", "/api/v1/cast/sessions/"+id+"/control", body).Expect(200))
}

// movies adds n playable movies of the admin.
func (v *env) movies(n int) []string {
	var ids []string
	for i := range n {
		ids = append(ids, v.addMedia(v.adminID, fmt.Sprintf("Movie %d.mp4", i), mediaRow{Title: fmt.Sprintf("Movie %d", i),
			Duration: 600, Container: "mp4", Video: "h264", Audio: "aac", Playable: true}))
	}
	return ids
}

func (v *env) waitLoads(r *fakeRenderer, n int) *renderer.LoadRequest {
	v.t.Helper()
	v.waitFor(fmt.Sprintf("%d loads", n), func() bool { return r.loadCount() >= n })
	return r.lastLoad()
}

func loadedID(req *renderer.LoadRequest) string {
	parts := strings.Split(strings.SplitN(req.URL, "?", 2)[0], "/")
	return parts[len(parts)-2]
}

func TestQueueControls(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	ids := v.movies(3)
	s := v.start(v.adm, dev, item("media", ids[0]), item("media", ids[1]), item("media", ids[2]))
	r := v.cc.last()
	if s.Index != 0 || s.Repeat != RepeatOff || s.Shuffle || s.Volume != 0.5 {
		t.Fatalf("initial: %+v", s)
	}

	s = v.control(v.adm, s.ID, map[string]any{"action": "next"})
	if s.Index != 1 {
		t.Fatalf("next: index %d", s.Index)
	}
	if id := loadedID(v.waitLoads(r, 2)); id != ids[1] {
		t.Fatalf("next loaded %s", id)
	}
	// previous early in an item goes back; later it restarts the item.
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	r.update(func(st *renderer.Status) { st.Position = 30 })
	v.waitSession(v.adm, s.ID, "position", func(s sessionResp) bool { return s.Position >= 30 })
	v.control(v.adm, s.ID, map[string]any{"action": "previous"})
	if cmds := r.commands(); cmds[len(cmds)-1] != "seek 0" {
		t.Fatalf("previous after 30 s: %v", cmds)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "previous"})
	if s.Index != 0 {
		t.Fatalf("previous: index %d", s.Index)
	}
	v.waitLoads(r, 3)
	s = v.control(v.adm, s.ID, map[string]any{"action": "jump", "index": 2})
	if s.Index != 2 {
		t.Fatalf("jump: %d", s.Index)
	}
	v.waitLoads(r, 4)
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "next"}).ExpectError(409, "conflict")
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "jump", "index": 3}).ExpectError(422, "validation_failed")
	s = v.control(v.adm, s.ID, map[string]any{"action": "repeat", "repeat": "all"})
	if s.Repeat != RepeatAll {
		t.Fatal(s.Repeat)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "next"})
	if s.Index != 0 {
		t.Fatalf("next with repeat all: %d", s.Index)
	}
	v.waitLoads(r, 5)
	s = v.control(v.adm, s.ID, map[string]any{"action": "shuffle", "shuffle": true})
	if !s.Shuffle || s.Index != 0 {
		t.Fatalf("shuffle keeps the current item: %+v", s)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "shuffle", "shuffle": false})
	if s.Shuffle || s.Index != 0 {
		t.Fatalf("unshuffle: %+v", s)
	}

	// Transport and volume.
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	s = v.control(v.adm, s.ID, map[string]any{"action": "pause"})
	if s.State != StatePaused {
		t.Fatalf("pause: %s", s.State)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "toggle"})
	if s.State != StatePlaying {
		t.Fatalf("toggle: %s", s.State)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "seek", "position_seconds": 120})
	if s.Position < 120 || s.Position > 125 {
		t.Fatalf("seek: %v", s.Position)
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "volume", "volume": 0.25})
	s = v.control(v.adm, s.ID, map[string]any{"action": "mute", "muted": true})
	if s.Volume != 0.25 || !s.Muted {
		t.Fatalf("volume: %v %v", s.Volume, s.Muted)
	}
	cmds := strings.Join(r.commands(), ",")
	for _, want := range []string{"pause", "play", "seek 120", "volume 0.25", "muted true"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("renderer commands %s lack %q", cmds, want)
		}
	}
	for _, bad := range []map[string]any{{"action": "explode"}, {"action": "seek"}, {"action": "volume", "volume": 1.5},
		{"action": "mute"}, {"action": "repeat", "repeat": "sometimes"}, {"action": "shuffle"}} {
		v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", bad).ExpectError(422, "validation_failed")
	}
	s = v.control(v.adm, s.ID, map[string]any{"action": "stop"})
	if s.State != StateIdle {
		t.Fatalf("stop: %s", s.State)
	}
	n := r.loadCount()
	s = v.control(v.adm, s.ID, map[string]any{"action": "play"})
	v.waitLoads(r, n+1)
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+s.ID, nil).Expect(204)
	if cmds := r.commands(); cmds[len(cmds)-1] != "close" {
		t.Fatalf("delete did not close the renderer: %v", cmds)
	}
	var list []sessionResp
	v.adm.Do("GET", "/api/v1/cast/sessions", nil).Expect(200).JSON(&list)
	if len(list) != 0 {
		t.Fatalf("stopped session still listed: %+v", list)
	}
}

func TestAutoAdvanceRepeatAndFailures(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	ids := v.movies(3)
	s := v.start(v.adm, dev, item("media", ids[0]), item("media", ids[1]))
	r := v.cc.last()
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded; st.Position = 600 })
	if id := loadedID(v.waitLoads(r, 2)); id != ids[1] {
		t.Fatalf("auto-advance loaded %s", id)
	}
	v.waitSession(v.adm, s.ID, "second item", func(s sessionResp) bool { return s.Index == 1 && s.State == StatePlaying })
	// The first item was recorded as watched (it reached its end).
	var completed int
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT completed FROM media_progress WHERE user_id = ? AND file_id = ?`, v.adminID, ids[0]).Scan(&completed); err != nil || completed != 1 {
		t.Fatalf("watched flag: %v %v", completed, err)
	}
	// The end of the queue: idle.
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded })
	v.waitSession(v.adm, s.ID, "idle", func(s sessionResp) bool { return s.State == StateIdle })
	// Repeat one replays the item.
	v.control(v.adm, s.ID, map[string]any{"action": "repeat", "repeat": "one"})
	v.control(v.adm, s.ID, map[string]any{"action": "play"})
	v.waitLoads(r, 3)
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded })
	if id := loadedID(v.waitLoads(r, 4)); id != ids[1] {
		t.Fatalf("repeat one loaded %s", id)
	}

	// Unplayable items are skipped; three failures in a row stop in the error state.
	s = decodeSession(t, v.adm.Do("PUT", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{
		item("media", ids[0]), item("media", ids[1]), item("media", ids[2])}}).Expect(200))
	v.waitLoads(r, 5)
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	r.mu.Lock()
	r.loadErr = errors.New("LOAD_FAILED")
	r.mu.Unlock()
	r.update(func(st *renderer.Status) { st.State = renderer.StateError; st.Err = errors.New("decoder") })
	es := v.waitSession(v.adm, s.ID, "error", func(s sessionResp) bool { return s.State == StateError })
	if es.Error == nil || es.Error.Code != codePlaybackFailed {
		t.Fatalf("error: %+v", es)
	}
	if n := r.loadCount(); n != 7 { // the failing item, then the two skips that failed too
		t.Fatalf("loads after failures: %d", n)
	}
	// Playing again retries.
	r.mu.Lock()
	r.loadErr = nil
	r.mu.Unlock()
	v.control(v.adm, s.ID, map[string]any{"action": "play"})
	v.waitSession(v.adm, s.ID, "recovered", func(s sessionResp) bool { return s.State == StatePlaying })

	// The device taken over by someone else ends the session.
	r.update(func(st *renderer.Status) { st.State = renderer.StateIdle; st.Err = renderer.ErrInterrupted })
	es = v.waitSession(v.adm, s.ID, "interrupted", func(s sessionResp) bool { return s.State == StateStopped })
	if es.Error == nil || es.Error.Code != codeInterrupted {
		t.Fatalf("interrupted: %+v", es)
	}
}

func TestLongPoll(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	ids := v.movies(1)
	s := v.start(v.adm, dev, item("media", ids[0]))
	path := "/api/v1/cast/sessions/" + s.ID
	v.adm.Do("GET", path+"?wait_version=x", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", path+"?wait_version=1&timeout=31", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", path+"?wait_version=1&timeout=0", nil).ExpectError(400, "bad_request")
	// A different version answers at once.
	t0 := time.Now()
	if got := decodeSession(t, v.adm.Do("GET", fmt.Sprintf("%s?wait_version=%d", path, s.Version-1), nil).Expect(200)); got.Version != s.Version {
		t.Fatalf("version %d != %d", got.Version, s.Version)
	}
	// The same version waits for the timeout.
	t0 = time.Now()
	got := decodeSession(t, v.adm.Do("GET", fmt.Sprintf("%s?wait_version=%d&timeout=1", path, s.Version), nil).Expect(200))
	if el := time.Since(t0); el < 900*time.Millisecond || got.Version != s.Version {
		t.Fatalf("long poll returned after %v with version %d", el, got.Version)
	}
	// A change wakes it early.
	var wg sync.WaitGroup
	var woke sessionResp
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := v.adm.Do("GET", fmt.Sprintf("%s?wait_version=%d&timeout=10", path, s.Version), nil)
		r.JSON(&woke)
	}()
	time.Sleep(200 * time.Millisecond)
	t0 = time.Now()
	v.control(v.adm, s.ID, map[string]any{"action": "pause"})
	wg.Wait()
	if time.Since(t0) > 3*time.Second || woke.Version <= s.Version || woke.State != StatePaused {
		t.Fatalf("woken poll: %+v after %v", woke, time.Since(t0))
	}
}

func TestQueueEdits(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	ids := v.movies(4)
	s := v.start(v.adm, dev, item("media", ids[0]), item("media", ids[1]))
	r := v.cc.last()
	// Insert before the current item: what plays stays, its index moves.
	s = decodeSession(t, v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{item("media", ids[2])}, "position": 0}).Expect(200))
	if s.Index != 1 || len(s.Items) != 3 || s.Items[0].ID != ids[2] {
		t.Fatalf("insert at 0: %+v", s)
	}
	s = decodeSession(t, v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{item("media", ids[3])}}).Expect(200))
	if len(s.Items) != 4 || s.Items[3].ID != ids[3] || r.loadCount() != 1 {
		t.Fatalf("append: %+v (loads %d)", s, r.loadCount())
	}
	// Replace: plays the given index from the given position.
	s = decodeSession(t, v.adm.Do("PUT", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{
		item("media", ids[1]), item("media", ids[2])}, "index": 1, "position_seconds": 42}).Expect(200))
	req := v.waitLoads(r, 2)
	if s.Index != 1 || len(s.Items) != 2 || loadedID(req) != ids[2] || req.StartSeconds != 42 {
		t.Fatalf("replace: %+v %+v", s, req)
	}
	v.adm.Do("PUT", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{}}).ExpectError(422, "validation_failed")
	v.adm.Do("PUT", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{item("media", ids[0])}, "index": 3}).ExpectError(422, "validation_failed")
	big := make([]any, MaxQueue-1)
	for i := range big {
		big[i] = item("media", ids[0])
	}
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": big}).ExpectError(422, "validation_failed")
	// Items appended to a finished queue start playing.
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded })
	v.waitSession(v.adm, s.ID, "idle", func(s sessionResp) bool { return s.State == StateIdle })
	s = decodeSession(t, v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{item("media", ids[3])}}).Expect(200))
	if s.Index != 2 || loadedID(v.waitLoads(r, 3)) != ids[3] {
		t.Fatalf("append to a finished queue: %+v", s)
	}
}

func TestTrackCastingAndPlays(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	v.addTrack(v.adminID, "track00001", music.TrackInfo{Title: "Song", Artist: "Band", Album: "LP", Mime: "audio/mpeg", Codec: "mp3", Container: "mp3", DurationSeconds: 200, HasArt: true})
	v.addTrack(v.adminID, "track00002", music.TrackInfo{Title: "Lossless", Artist: "Band", Mime: "audio/mp4", Codec: "alac", Container: "m4a", DurationSeconds: 300})
	s := v.start(v.adm, dev, item("track", "track00001"), item("track", "track00002"))
	if s.Items[0].Title != "Song" || s.Items[0].Subtitle != "Band · LP" || s.Items[0].ArtURL == nil || *s.Items[0].ArtURL != "/api/v1/music/tracks/track00001/art" {
		t.Fatalf("track item: %+v", s.Items[0])
	}
	r := v.cc.last()
	req := r.lastLoad()
	if req.Video || req.Artist != "Band" || req.Album != "LP" || req.ContentType != "audio/mpeg" || !strings.HasSuffix(req.URL, "/track/track00001/stream") ||
		!strings.Contains(req.ArtURL, "/track/track00001/art?size=lg") {
		t.Fatalf("track load: %+v", req)
	}
	// The device can fetch stream and art through the ticket.
	if rec := v.fetchFrom("192.0.2.10:1", ticketPath(t, req.URL), nil); rec.Code != 200 || rec.Body.String() != "STREAM:track00001" {
		t.Fatalf("track stream: %d %q", rec.Code, rec.Body.String())
	}
	if rec := v.fetchFrom("192.0.2.10:1", ticketPath(t, req.ArtURL), nil); rec.Code != 200 {
		t.Fatalf("track art: %d", rec.Code)
	}
	// Played 120 s then finished: a completed play.
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	ss := v.sess(s.ID)
	if err := ss.do(context.Background(), func() error { ss.cur.played = 120; return nil }); err != nil {
		t.Fatal(err)
	}
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded })
	req2 := v.waitLoads(r, 2)
	plays := v.music.recorded()
	if len(plays) != 1 || plays[0].ID != "track00001" || !plays[0].Completed || plays[0].Seconds < 120 {
		t.Fatalf("plays: %+v", plays)
	}
	// ALAC is not Chromecast-playable; without ffmpeg the file is tried anyway, with ffmpeg it
	// is transcoded to AAC.
	if !strings.HasSuffix(req2.URL, "/track/track00002/stream") {
		t.Fatalf("alac without ffmpeg: %q", req2.URL)
	}
	if err := ss.do(context.Background(), func() error { v.m.media = ffmpegMedia{v.m.media}; return nil }); err != nil {
		t.Fatal(err)
	}
	v.control(v.adm, s.ID, map[string]any{"action": "seek", "position_seconds": 10}) // direct: a device seek
	v.control(v.adm, s.ID, map[string]any{"action": "previous"})                     // restart → reload of track 2? (pos 10 > 3: seek 0)
	s = v.control(v.adm, s.ID, map[string]any{"action": "jump", "index": 1})
	req3 := v.waitLoads(r, 3)
	if !strings.Contains(req3.URL, "/track/track00002/transcode?start=0.0") || req3.ContentType != "audio/aac" {
		t.Fatalf("alac with ffmpeg: %q %q", req3.URL, req3.ContentType)
	}
	if rec := v.fetchFrom("192.0.2.10:1", ticketPath(t, req3.URL), nil); rec.Code != 200 || rec.Body.String() != "TRANSCODE-AAC-0:track00002" {
		t.Fatalf("track transcode: %d %q", rec.Code, rec.Body.String())
	}
	// Seeking a transcode restarts it at the position; the session reports item positions.
	v.waitSession(v.adm, s.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	s = v.control(v.adm, s.ID, map[string]any{"action": "seek", "position_seconds": 90})
	req4 := v.waitLoads(r, 4)
	if !strings.Contains(req4.URL, "start=90.0") {
		t.Fatalf("transcode seek: %q", req4.URL)
	}
	r.update(func(st *renderer.Status) { st.Position = 5 })
	v.waitSession(v.adm, s.ID, "item position", func(s sessionResp) bool { return s.Position >= 95 && s.Position < 100 })
	// Stopping the session records the partial play of track 2.
	if err := ss.do(context.Background(), func() error { ss.cur.played = 40; return nil }); err != nil {
		t.Fatal(err)
	}
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+s.ID, nil).Expect(204)
	plays = v.music.recorded()
	if len(plays) < 2 || plays[len(plays)-1].ID != "track00002" || plays[len(plays)-1].Completed || plays[len(plays)-1].Seconds < 40 {
		t.Fatalf("plays after stop: %+v", plays)
	}
}

// ffmpegMedia pretends ffmpeg is installed.
type ffmpegMedia struct{ mediaService }

func (ffmpegMedia) FFmpegPath(context.Context) string { return "/opt/fake/ffmpeg" }

func TestAirPlayAudioUsesPCM(t *testing.T) {
	v := newEnv(t)
	v.setInstances(
		mdns.Instance{Service: renderer.ServiceRAOP, Name: "AABBCCDDEEFF@Kitchen", Host: "kitchen.local", Port: 7000,
			Addrs: mustAddrs("192.0.2.40"), TXT: map[string]string{"am": "AudioAccessory5,1"}},
	)
	v.e.Patch("general", map[string]any{"public_url": "https://nas.example.com"})
	v.enable(nil)
	v.ap.mu.Lock()
	v.ap.caps = renderer.Capabilities{Audio: true}
	v.ap.mu.Unlock()
	movie := v.movies(1)[0]
	v.addTrack(v.adminID, "track00001", music.TrackInfo{Title: "Song", Artist: "Band", Mime: "audio/wav", Codec: "pcm_s16le", Container: "wav", DurationSeconds: 10})
	v.adm.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": "airplay-aabbccddeeff", "items": []any{item("media", movie)}}).ExpectError(422, "unsupported_media")
	v.start(v.adm, "airplay-aabbccddeeff", item("track", "track00001"))
	req := v.ap.last().lastLoad()
	if req.URL != "" || req.PCM == nil || req.Art != nil || req.Title != "Song" {
		t.Fatalf("airplay load: %+v", req)
	}
}

func TestChannelCasting(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	// A live channel of three episodes (61, 62, 63 s) anchored "now".
	var eps []string
	for i := 1; i <= 3; i++ {
		eps = append(eps, v.addMedia(v.adminID, fmt.Sprintf("Show e%02d.mp4", i), mediaRow{Title: fmt.Sprintf("Ep %d", i), ShowTitle: "Show",
			Season: 1, Episode: i, EpisodeTitle: fmt.Sprintf("Ep %d", i), Duration: float64(60 + i), Container: "mp4", Video: "h264", Audio: "aac", Playable: true}))
	}
	var lineup, ch, od map[string]any
	v.adm.Do("POST", "/api/v1/tv/lineups", map[string]any{"name": "Show", "items": []any{map[string]any{"kind": "show", "show_title": "Show"}}}).Expect(201).JSON(&lineup)
	v.adm.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "Show TV", "lineup_id": lineup["id"]}).Expect(201).JSON(&ch)
	v.adm.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "Show OD", "lineup_id": lineup["id"], "playback_mode": "on_demand"}).Expect(201).JSON(&od)
	cid, odid := ch["id"].(string), od["id"].(string)

	tvOnly := v.token(v.adm, "cast", "media:read")
	tvOnly.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("channel", cid)}}).ExpectError(403, "forbidden")
	s := v.start(v.adm, dev, item("channel", cid))
	if s.Items[0].Title != "1 · Show TV" || s.Items[0].Subtitle == "" || s.Items[0].DurationSeconds != nil {
		t.Fatalf("channel item: %+v", s.Items[0])
	}
	r := v.cc.last()
	req := r.lastLoad()
	if !req.Live || !req.Video || req.SeriesTitle != "Show" || req.Season != 1 || !strings.Contains(req.URL, "/channel/"+cid+"/stream?program=") {
		t.Fatalf("channel load: %+v", req)
	}
	first := req.URL
	if rec := v.fetchFrom("192.0.2.10:1", ticketPath(t, req.URL), nil); rec.Code != 200 {
		t.Fatalf("program stream: %d %s", rec.Code, rec.Body.String())
	}
	// A program of another channel's lineup is refused even with a valid ticket.
	foreign := v.addMedia(v.adminID, "Other.mp4", mediaRow{Title: "Other", Duration: 50, Container: "mp4", Video: "h264", Playable: true})
	bad := strings.Replace(ticketPath(t, req.URL), "program="+loadedProgram(req.URL), "program="+foreign, 1)
	if rec := v.fetchFrom("192.0.2.10:1", bad, nil); rec.Code != 404 {
		t.Fatalf("foreign program: %d", rec.Code)
	}
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "seek", "position_seconds": 5}).ExpectError(409, "conflict")
	// The program ends: the next one on the schedule plays.
	v.advance(70 * time.Second)
	r.update(func(st *renderer.Status) { st.State = renderer.StateEnded })
	req = v.waitLoads(r, 2)
	if req.URL == first || !strings.Contains(req.URL, "/channel/"+cid+"/stream?program=") {
		t.Fatalf("next program: %q (first %q)", req.URL, first)
	}

	// On-demand channels play from the owner's position and save it.
	v.adm.Do("PUT", "/api/v1/tv/channels/"+odid+"/position", map[string]any{"index": 1, "offset_seconds": 12.5}).Expect(204)
	s2 := decodeSession(t, v.adm.Do("PUT", "/api/v1/cast/sessions/"+s.ID+"/items", map[string]any{"items": []any{item("channel", odid)}}).Expect(200))
	req = v.waitLoads(r, 3)
	if req.Live || loadedProgram(req.URL) != eps[1] || req.StartSeconds != 12.5 {
		t.Fatalf("on-demand load: %+v", req)
	}
	v.waitSession(v.adm, s2.ID, "playing", func(s sessionResp) bool { return s.State == StatePlaying })
	r.update(func(st *renderer.Status) { st.Position = 30 })
	v.waitSession(v.adm, s2.ID, "position", func(s sessionResp) bool { return s.Position >= 30 })
	v.control(v.adm, s2.ID, map[string]any{"action": "seek", "position_seconds": 40}) // on-demand: seekable
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+s2.ID, nil).Expect(204)
	var now map[string]any
	v.adm.Do("GET", "/api/v1/tv/channels/"+odid+"/now", nil).Expect(200).JSON(&now)
	if off := now["offset_seconds"].(float64); now["index"].(float64) != 1 || off < 40 || off > 45 {
		t.Fatalf("saved on-demand position: index %v offset %v", now["index"], off)
	}
}

func loadedProgram(u string) string {
	_, q, _ := strings.Cut(u, "program=")
	q, _, _ = strings.Cut(q, "&")
	return q
}

func TestSessionLimitIdleAndDisable(t *testing.T) {
	v := newEnv(t)
	var insts []mdns.Instance
	for i := range 4 {
		id := fmt.Sprintf("%032d", i)
		insts = append(insts, chromecastInstance(id, fmt.Sprintf("TV %d", i), fmt.Sprintf("192.0.2.%d", 10+i)))
	}
	v.setInstances(insts...)
	v.e.Patch("general", map[string]any{"public_url": "https://nas.example.com"})
	v.enable(nil)
	v.m.idleTimeout = time.Minute
	ids := v.movies(1)
	var sessions []sessionResp
	for i := range 3 {
		sessions = append(sessions, v.start(v.adm, "chromecast-"+fmt.Sprintf("%032d", i), item("media", ids[0])))
	}
	v.adm.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": "chromecast-" + fmt.Sprintf("%032d", 3),
		"items": []any{item("media", ids[0])}}).ExpectError(409, "session_limit")
	// Replacing one of my own sessions is not a fourth session.
	sessions[0] = v.start(v.adm, "chromecast-"+fmt.Sprintf("%032d", 0), item("media", ids[0]))

	// A paused session without client requests ends after the idle timeout.
	v.control(v.adm, sessions[1].ID, map[string]any{"action": "pause"})
	v.advance(2 * time.Minute)
	idle := v.sess(sessions[1].ID)
	v.waitFor("idle end", func() bool { return !idle.live() })
	if snap := idle.snapshot(false); snap.Error == nil || snap.Error.Code != codeIdle {
		t.Fatalf("idle session: %+v", snap)
	}

	// Turning casting off ends everything and closes the device connections.
	r := v.cc.last()
	v.e.Patch("cast", map[string]any{"enabled": false})
	st := v.waitSession(v.adm, sessions[2].ID, "disabled", func(s sessionResp) bool { return s.State == StateStopped })
	if st.Error == nil || st.Error.Code != codeDisabled {
		t.Fatalf("disabled: %+v", st)
	}
	v.waitFor("renderer closed", func() bool {
		cmds := r.commands()
		return len(cmds) > 0 && cmds[len(cmds)-1] == "close"
	})
	v.br.mu.Lock()
	closed := v.br.closed
	v.br.mu.Unlock()
	if !closed {
		t.Fatal("discovery still running after disabling")
	}
}

func TestLostConnectionReconnects(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	ids := v.movies(1)
	s := v.start(v.adm, dev, item("media", ids[0]))
	r := v.cc.last()
	r.update(func(st *renderer.Status) { st.Position = 100 })
	v.waitSession(v.adm, s.ID, "position", func(s sessionResp) bool { return s.Position >= 100 })
	r.lose()
	v.waitFor("reconnected", func() bool { r2 := v.cc.last(); return r2 != r && r2.loadCount() == 1 })
	if req := v.cc.last().lastLoad(); req.StartSeconds < 100 {
		t.Fatalf("resumed at %v", req.StartSeconds)
	}
	v.waitSession(v.adm, s.ID, "playing again", func(s sessionResp) bool { return s.State == StatePlaying })
	// A device that stays away: the session reports it.
	v.cc.mu.Lock()
	v.cc.connectErr = errors.New("no route to host")
	v.cc.mu.Unlock()
	v.cc.last().lose()
	es := v.waitSession(v.adm, s.ID, "device unavailable", func(s sessionResp) bool { return s.State == StateError })
	if es.Error == nil || es.Error.Code != codeDeviceUnavailable {
		t.Fatalf("lost device: %+v", es)
	}
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "play"}).ExpectError(502, "device_unavailable")
}

func TestPairingFlow(t *testing.T) {
	v := newEnv(t)
	v.setInstances(mdns.Instance{Service: renderer.ServiceAirPlay, Name: "Den", Host: "den.local", Port: 7000,
		Addrs: mustAddrs("192.0.2.30"), TXT: map[string]string{"deviceid": "AA:BB:CC:DD:EE:01"}})
	v.e.Patch("general", map[string]any{"public_url": "https://nas.example.com"})
	v.enable(nil)
	dev := "airplay-aabbccddee01"
	_, bob := v.user("bob")
	var start map[string]string
	bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair", nil).Expect(200).JSON(&start)
	if start["pairing_id"] == "" {
		t.Fatal("no pairing id")
	}
	bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair/confirm", map[string]any{"pairing_id": start["pairing_id"], "pin": "12"}).ExpectError(422, "validation_failed")
	r := bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair/confirm", map[string]any{"pairing_id": start["pairing_id"], "pin": "9999"})
	r.ExpectError(422, "validation_failed")
	if !strings.Contains(string(r.Body), `"pin"`) {
		t.Fatalf("wrong PIN body: %s", r.Body)
	}
	// The attempt is over after a wrong PIN.
	bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair/confirm", map[string]any{"pairing_id": start["pairing_id"], "pin": "1234"}).ExpectError(404, "not_found")
	bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair", nil).Expect(200).JSON(&start)
	// Someone else cannot confirm bob's pairing.
	v.adm.Do("POST", "/api/v1/cast/devices/"+dev+"/pair/confirm", map[string]any{"pairing_id": start["pairing_id"], "pin": "1234"}).ExpectError(404, "not_found")
	bob.Do("POST", "/api/v1/cast/devices/"+dev+"/pair/confirm", map[string]any{"pairing_id": start["pairing_id"], "pin": "1234"}).Expect(204)
	var devs devicesResponse
	bob.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&devs)
	if len(devs.Devices) != 1 || !devs.Devices[0].Paired || !devs.Devices[0].RequiresPairing {
		t.Fatalf("paired device: %+v", devs.Devices)
	}
	// Stored sealed, server-wide.
	var enc string
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT creds_enc FROM cast_pairings WHERE device_id = ?`, dev).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v1.") || strings.Contains(enc, "SECRET") {
		t.Fatalf("credentials not sealed: %q", enc)
	}
	// Connecting hands the credentials to the driver.
	movie := v.movies(1)[0]
	v.start(v.adm, dev, item("media", movie))
	v.ap.mu.Lock()
	creds := v.ap.creds[len(v.ap.creds)-1]
	v.ap.mu.Unlock()
	if string(creds) != "SECRET-PAIRING-CREDS" {
		t.Fatalf("creds passed to Connect: %q", creds)
	}
	// Only admins forget pairings.
	bob.Do("DELETE", "/api/v1/cast/devices/"+dev+"/pairing", nil).ExpectError(403, "forbidden")
	v.adm.Do("DELETE", "/api/v1/cast/devices/"+dev+"/pairing", nil).Expect(204)
	v.adm.Do("DELETE", "/api/v1/cast/devices/"+dev+"/pairing", nil).ExpectError(404, "not_found")
	// Chromecasts have no pairing.
	v.setInstances(chromecastInstance("0123456789abcdef0123456789abcdef", "TV", "192.0.2.10"))
	v.adm.Do("POST", "/api/v1/cast/devices/chromecast-0123456789abcdef0123456789abcdef/pair", nil).ExpectError(422, "pairing_unsupported")
}
