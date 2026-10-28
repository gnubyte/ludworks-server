package chromecast

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"math"
	"net"
	"reflect"
	"testing"
	"time"

	"ludworks/internal/cast/renderer"
)

// nopConn is a net.Conn that discards writes (unit tests of the player without a device).
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) LocalAddr() net.Addr              { return nil }
func (nopConn) RemoteAddr() net.Addr             { return nil }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

// asMap marshals v and decodes it back as generic JSON.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildLoad(t *testing.T) {
	art := "https://nas.example/api/v1/cast/media/T/media/abc/art?size=lg"
	for _, tc := range []struct {
		name     string
		req      renderer.LoadRequest
		metadata map[string]any
		stream   string
	}{
		{
			name: "movie",
			req: renderer.LoadRequest{URL: "u", ContentType: "video/mp4", Video: true, Title: "Night of the Living Dead",
				Subtitle: "1968", Year: 1968, DurationSeconds: 5760, ArtURL: art},
			metadata: map[string]any{"metadataType": 1.0, "title": "Night of the Living Dead", "subtitle": "1968", "releaseDate": "1968",
				"images": []any{map[string]any{"url": art}}},
			stream: "BUFFERED",
		},
		{
			name: "episode",
			req: renderer.LoadRequest{URL: "u", ContentType: "video/mp4", Video: true, Title: "Pilot", Subtitle: "Show · S1E1",
				SeriesTitle: "Show", Season: 1, Episode: 1},
			metadata: map[string]any{"metadataType": 2.0, "title": "Pilot", "seriesTitle": "Show", "season": 1.0, "episode": 1.0},
			stream:   "BUFFERED",
		},
		{
			name:     "episode without numbers",
			req:      renderer.LoadRequest{URL: "u", Video: true, Title: "Special", SeriesTitle: "Show", Season: -1},
			metadata: map[string]any{"metadataType": 2.0, "title": "Special", "seriesTitle": "Show"},
			stream:   "BUFFERED",
		},
		{
			name: "track",
			req: renderer.LoadRequest{URL: "u", ContentType: "audio/flac", Title: "Song", Artist: "Band", Album: "Record",
				Year: 2001, ArtURL: art},
			metadata: map[string]any{"metadataType": 3.0, "title": "Song", "artist": "Band", "albumName": "Record", "releaseDate": "2001",
				"images": []any{map[string]any{"url": art}}},
			stream: "BUFFERED",
		},
		{
			name:     "generic audio",
			req:      renderer.LoadRequest{URL: "u", ContentType: "audio/mpeg", Title: "Recording", Subtitle: "Memo"},
			metadata: map[string]any{"metadataType": 0.0, "title": "Recording", "subtitle": "Memo"},
			stream:   "BUFFERED",
		},
		{
			name:     "live without duration",
			req:      renderer.LoadRequest{URL: "u", Video: true, Live: true, Title: "News"},
			metadata: map[string]any{"metadataType": 1.0, "title": "News"},
			stream:   "LIVE",
		},
		{
			name:     "live program with a duration",
			req:      renderer.LoadRequest{URL: "u", Video: true, Live: true, Title: "News", DurationSeconds: 1800, StartSeconds: 600},
			metadata: map[string]any{"metadataType": 1.0, "title": "News"},
			stream:   "BUFFERED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := asMap(t, buildLoad(9, "app-session", &tc.req))
			if m["type"] != "LOAD" || m["requestId"] != 9.0 || m["sessionId"] != "app-session" || m["autoplay"] != true {
				t.Fatalf("envelope: %v", m)
			}
			if ids, ok := m["activeTrackIds"].([]any); !ok || len(ids) != 0 {
				t.Fatalf("activeTrackIds must be present and empty: %v", m["activeTrackIds"])
			}
			if m["currentTime"] != tc.req.StartSeconds {
				t.Fatalf("currentTime %v", m["currentTime"])
			}
			media := m["media"].(map[string]any)
			if media["contentId"] != "u" || media["streamType"] != tc.stream {
				t.Fatalf("media: %v", media)
			}
			if !reflect.DeepEqual(media["metadata"], tc.metadata) {
				t.Fatalf("metadata:\n got %v\nwant %v", media["metadata"], tc.metadata)
			}
			if d, ok := media["duration"]; tc.req.DurationSeconds > 0 != ok || ok && d != tc.req.DurationSeconds {
				t.Fatalf("duration %v", d)
			}
			if _, ok := media["tracks"]; ok {
				t.Fatal("tracks without subtitles")
			}
			if _, ok := media["textTrackStyle"]; ok {
				t.Fatal("textTrackStyle without subtitles")
			}
		})
	}
}

func TestBuildLoadTracks(t *testing.T) {
	req := renderer.LoadRequest{URL: "u", Video: true, Subtitles: []renderer.SubtitleTrack{
		{URL: "https://h/s/0.vtt", Language: "en", Label: "English"},
		{URL: "https://h/s/1.vtt", Label: "Commentary"},
	}}
	media := asMap(t, buildLoad(1, "", &req))["media"].(map[string]any)
	want := []any{
		map[string]any{"trackId": 1.0, "type": "TEXT", "subtype": "SUBTITLES", "trackContentId": "https://h/s/0.vtt",
			"trackContentType": "text/vtt", "name": "English", "language": "en"},
		map[string]any{"trackId": 2.0, "type": "TEXT", "subtype": "SUBTITLES", "trackContentId": "https://h/s/1.vtt",
			"trackContentType": "text/vtt", "name": "Commentary"},
	}
	if !reflect.DeepEqual(media["tracks"], want) {
		t.Fatalf("tracks: %v", media["tracks"])
	}
	style, ok := media["textTrackStyle"].(map[string]any)
	if !ok || style["foregroundColor"] != "#FFFFFFFF" || style["edgeType"] != "OUTLINE" || style["fontScale"] != 1.0 {
		t.Fatalf("textTrackStyle: %v", media["textTrackStyle"])
	}
}

func TestBuildLoadSanitisesNumbers(t *testing.T) {
	for _, start := range []float64{-5, math.NaN(), math.Inf(1), 1e12} {
		req := renderer.LoadRequest{URL: "u", StartSeconds: start, DurationSeconds: math.Inf(1)}
		m := asMap(t, buildLoad(1, "", &req))
		if m["currentTime"] != 0.0 {
			t.Fatalf("start %v → currentTime %v", start, m["currentTime"])
		}
		if _, ok := m["media"].(map[string]any)["duration"]; ok {
			t.Fatalf("infinite duration sent")
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://10.0.0.2:8099/api/v1/cast/media/SECRETTICKET/media/abc/stream":       "http://10.0.0.2:8099/api/v1/cast/media/[redacted]/media/abc/stream",
		"https://nas/api/v1/cast/media/SECRET/channel/c/transcode?program=x&start=5": "https://nas/api/v1/cast/media/[redacted]/channel/c/transcode",
		"https://nas/api/v1/cast/media/SECRET":                                       "https://nas/api/v1/cast/media/[redacted]",
		"https://other/file.mp4?token=abc":                                           "https://other/file.mp4",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValueChecks(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		v  *float64
		ok bool
	}{{nil, false}, {f(-1), false}, {f(0), true}, {f(12.5), true}, {f(maxSeconds + 1), false}, {f(math.NaN()), false}} {
		if _, ok := seconds(tc.v); ok != tc.ok {
			t.Errorf("seconds(%v) ok = %v", tc.v, ok)
		}
	}
	if rate(nil) != 1 || rate(f(-2)) != 1 || rate(f(100)) != maxPlaybackRate || rate(f(0.5)) != 0.5 {
		t.Error("rate")
	}
	if lv, ok := level(f(5)); !ok || lv != 1 {
		t.Error("level clamps high")
	}
	if lv, ok := level(f(-1)); !ok || lv != 0 {
		t.Error("level clamps low")
	}
	if _, ok := level(nil); ok {
		t.Error("level nil")
	}
	for s, ok := range map[string]bool{"abc-123": true, "": false, "a b": false, "ü": false, string(make([]byte, 300)): false} {
		if validID(s) != ok {
			t.Errorf("validID(%q) != %v", s, ok)
		}
	}
}

// TestPlayerStatusMapping drives the media-status state machine directly.
func TestPlayerStatusMapping(t *testing.T) {
	p := newPlayer(New(Options{}), renderer.Device{Name: "unit"}, "127.0.0.1", 8009)
	defer p.cancel()
	l := newLink(nopConn{}, "sender-0", time.Second, func(*Message) {})
	p.appSession, p.transport, p.mediaSession = "s", "t", 4
	media := func(entry string) {
		p.onMessage(l, &Message{SourceID: "t", Namespace: NamespaceMedia, PayloadUTF8: `{"type":"MEDIA_STATUS","requestId":0,"status":[` + entry + `]}`})
	}
	drain := func() bool {
		select {
		case <-p.Changed():
			return true
		default:
			return false
		}
	}
	media(`{"mediaSessionId":4,"playerState":"PLAYING","currentTime":10,"media":{"duration":100}}`)
	if st := p.Status(); st.State != renderer.StatePlaying || st.Duration != 100 || st.Position < 10 || !drain() {
		t.Fatalf("playing: %+v", st)
	}
	time.Sleep(60 * time.Millisecond)
	if st := p.Status(); st.Position < 10.05 {
		t.Fatalf("position does not advance: %v", st.Position)
	}
	media(`{"mediaSessionId":99,"playerState":"PAUSED","currentTime":50}`) // someone else's session
	if st := p.Status(); st.State != renderer.StatePlaying || drain() {
		t.Fatalf("foreign session applied: %+v", st)
	}
	p.onMessage(l, &Message{SourceID: "evil", Namespace: NamespaceMedia, PayloadUTF8: `{"type":"MEDIA_STATUS","status":[{"mediaSessionId":4,"playerState":"PAUSED"}]}`})
	if p.Status().State != renderer.StatePlaying {
		t.Fatal("status from a foreign transport applied")
	}
	media(`{"mediaSessionId":4,"playerState":"PAUSED","currentTime":20}`)
	if st := p.Status(); st.State != renderer.StatePaused || st.Position != 20 || !drain() {
		t.Fatalf("paused: %+v", st)
	}
	media(`{"mediaSessionId":4,"playerState":"BUFFERING"}`)
	if st := p.Status(); st.State != renderer.StateBuffering || st.Position != 20 {
		t.Fatalf("buffering: %+v", st)
	}
	media(`{"mediaSessionId":4,"playerState":"IDLE","idleReason":"FINISHED"}`)
	if st := p.Status(); st.State != renderer.StateEnded || st.Position != 100 || st.Err != nil || p.mediaSession != 0 {
		t.Fatalf("finished: %+v", st)
	}

	p.mediaSession = 5
	media(`{"mediaSessionId":5,"playerState":"IDLE","idleReason":"ERROR"}`)
	if st := p.Status(); st.State != renderer.StateError || st.Err == nil {
		t.Fatalf("error: %+v", st)
	}
	p.mediaSession = 6
	media(`{"mediaSessionId":6,"playerState":"IDLE","idleReason":"INTERRUPTED"}`)
	if st := p.Status(); st.State != renderer.StateIdle || st.Err != renderer.ErrInterrupted {
		t.Fatalf("interrupted: %+v", st)
	}
	p.mediaSession, p.ownStop = 7, true
	media(`{"mediaSessionId":7,"playerState":"IDLE","idleReason":"CANCELLED"}`)
	if st := p.Status(); st.State != renderer.StateIdle || st.Err != nil {
		t.Fatalf("own stop: %+v", st)
	}

	// Receiver status: volume, and the app going away while something plays.
	p.mediaSession = 8
	media(`{"mediaSessionId":8,"playerState":"PLAYING","currentTime":1}`)
	p.onMessage(l, &Message{SourceID: ReceiverID, Namespace: NamespaceReceiver,
		PayloadUTF8: `{"type":"RECEIVER_STATUS","status":{"applications":[{"appId":"CC1AD845","sessionId":"s","transportId":"t"}],"volume":{"level":0.25,"muted":true}}}`})
	if st := p.Status(); st.Volume != 0.25 || !st.Muted || st.State != renderer.StatePlaying {
		t.Fatalf("volume: %+v", st)
	}
	p.onMessage(l, &Message{SourceID: ReceiverID, Namespace: NamespaceReceiver,
		PayloadUTF8: `{"type":"RECEIVER_STATUS","status":{"applications":[{"appId":"233637DE","sessionId":"x","transportId":"y"}]}}`})
	if st := p.Status(); st.State != renderer.StateIdle || st.Err != renderer.ErrInterrupted || p.appSession != "" {
		t.Fatalf("app gone: %+v", st)
	}
}

func TestTLSConfig(t *testing.T) {
	for host, sni := range map[string]string{
		"192.168.1.5": "", "[fe80::1%eth0]": "", "fe80::1%eth0": "", "::1": "", "chromecast-abc.local": "chromecast-abc.local",
	} {
		c := tlsConfig(host)
		if c.ServerName != sni || !c.InsecureSkipVerify || c.MinVersion != tls.VersionTLS12 {
			t.Errorf("tlsConfig(%q): ServerName %q, InsecureSkipVerify %v, MinVersion %x", host, c.ServerName, c.InsecureSkipVerify, c.MinVersion)
		}
	}
}
