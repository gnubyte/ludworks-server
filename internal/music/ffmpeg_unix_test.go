//go:build !windows

package music_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
	mt "ludworks/internal/music/musictest"
)

// fakeFFmpeg answers -version, writes a small "rendition" to the output file of an encode
// (the last file: argument) and otherwise streams a marker and sleeps (live transcodes).
const fakeFFmpeg = `#!/bin/sh
out=""
mode="stream"
for a in "$@"; do
  case "$a" in
    -version) echo "ffmpeg version 9.9-fake Copyright (c) the fakers"; exit 0;;
    -movflags) mode="file";;
    file:*) out="${a#file:}";;
  esac
done
echo "$@" >> "ARGSFILE"
if [ "$mode" = "file" ]; then
  printf 'FAKEM4A' > "$out"
  head -c 5000 /dev/zero >> "$out"
  exit 0
fi
echo $$ >> "PIDFILE"
printf 'FAKEAUDIO'
exec sleep 30
`

type fakeTools struct {
	dir, pids, args string
}

func installFFmpeg(t *testing.T, v *env) *fakeTools {
	t.Helper()
	dir := t.TempDir()
	ft := &fakeTools{dir: dir, pids: filepath.Join(dir, "pids"), args: filepath.Join(dir, "args")}
	script := filepath.Join(dir, "ffmpeg")
	body := strings.NewReplacer("PIDFILE", ft.pids, "ARGSFILE", ft.args).Replace(fakeFFmpeg)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	v.e.Patch("media", map[string]any{"ffmpeg_path": script, "auto_thumbnails": false})
	eventually(t, "ffmpeg detected", func() bool { return v.media.FFmpegPath(ctx()) == script })
	return ft
}

func (f *fakeTools) argLines() []string {
	b, _ := os.ReadFile(f.args)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func readPIDs(path string) []int {
	b, _ := os.ReadFile(path)
	var out []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func TestTranscodeSlotsAndKill(t *testing.T) {
	v := newEnv(t)
	ft := installFFmpeg(t, v)
	v.svc.SetTranscodeTiming(300*time.Millisecond, 30*time.Second)
	home := v.home(v.adm)
	id := v.upload(v.adm, home, "", "a.flac", mt.FLAC(mt.FLACOptions{Seconds: 100, Comments: []string{"TITLE=x"}}))
	v.waitProbed(v.adm, id)
	open := func(c *authtest.Client, q string) (*http.Response, context.CancelFunc) {
		t.Helper()
		cx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(cx, "GET", v.e.URL+"/api/v1/music/tracks/"+id+"/transcode?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, cancel
	}
	r1, c1 := open(v.adm, "format=mp3&bitrate=96&start=12.5")
	if r1.StatusCode != 200 || r1.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("transcode %d %v", r1.StatusCode, r1.Header)
	}
	head := make([]byte, 9)
	if _, err := io.ReadFull(bufio.NewReader(r1.Body), head); err != nil || string(head) != "FAKEAUDIO" {
		t.Fatalf("body %q %v", head, err)
	}
	r2, c2 := open(v.adm, "format=opus")
	if r2.StatusCode != 200 || r2.Header.Get("Content-Type") != "audio/ogg" {
		t.Fatalf("second transcode %d", r2.StatusCode)
	}
	// Two per user: the third waits, then 429.
	r := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/transcode", nil, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || r.code() != "rate_limited" {
		t.Fatalf("third transcode: %d %v %s", r.Status, r.Header, r.Body)
	}
	if total, mine := v.svc.TranscodesInUse(v.principalOf(v.adm).UserID); total != 2 || mine != 2 {
		t.Fatalf("slots %d %d", total, mine)
	}
	eventually(t, "two ffmpeg", func() bool { return len(readPIDs(ft.pids)) == 2 })
	args := strings.Join(ft.argLines(), "\n")
	if !strings.Contains(args, "-ss 12.500") || !strings.Contains(args, "-c:a libmp3lame -b:a 96k -ac 2 -f mp3 pipe:1") ||
		!strings.Contains(args, "-c:a libopus -b:a 160k") || !strings.Contains(args, "-protocol_whitelist file") {
		t.Fatalf("ffmpeg args %s", args)
	}
	c1()
	c2()
	r1.Body.Close()
	r2.Body.Close()
	for _, pid := range readPIDs(ft.pids) {
		eventually(t, "ffmpeg "+strconv.Itoa(pid)+" killed", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	}
	eventually(t, "slots released", func() bool { total, _ := v.svc.TranscodesInUse(""); return total == 0 })
	// HEAD does not start ffmpeg; bad parameters are 400.
	before := len(readPIDs(ft.pids))
	hr := v.raw(v.adm, "HEAD", "/api/v1/music/tracks/"+id+"/transcode", nil, nil).expect(t, 200)
	if hr.Header.Get("Content-Type") != "audio/aac" {
		t.Fatalf("default format %v", hr.Header)
	}
	time.Sleep(100 * time.Millisecond)
	if after := len(readPIDs(ft.pids)); after != before {
		t.Fatal("HEAD spawned ffmpeg")
	}
	for _, q := range []string{"format=flac", "bitrate=32", "bitrate=999", "start=-1", "start=x"} {
		v.adm.Do("GET", "/api/v1/music/tracks/"+id+"/transcode?"+q, nil).ExpectError(400, "bad_request")
	}
	// Disabling transcoding in the media settings disables audio transcodes too.
	v.e.Patch("media", map[string]any{"transcode_enabled": false})
	v.adm.Do("GET", "/api/v1/music/tracks/"+id+"/transcode", nil).ExpectError(501, "ffmpeg_unavailable")
}

func TestDownloadRenditions(t *testing.T) {
	v := newEnv(t)
	ft := installFFmpeg(t, v)
	home := v.home(v.adm)
	// A 128 kbps MP3: renditions never exceed its bitrate.
	id := v.upload(v.adm, home, "", "song.mp3", mt.MP3(mt.ID3v2(mt.ID3Options{}, mt.Text("TIT2", mt.UTF8, "Song")), 3, nil))
	tr := v.waitProbed(v.adm, id)
	if num(tr["bitrate_kbps"]) != 128 {
		t.Fatalf("source bitrate %v", tr["bitrate_kbps"])
	}
	orig := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download", nil, nil).expect(t, 200)
	if orig.Header.Get("Content-Type") != "audio/mpeg" || !strings.HasPrefix(orig.Header.Get("Content-Disposition"), "attachment") ||
		orig.Header.Get("ETag") == "" || orig.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("original %v", orig.Header)
	}
	rr := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=original", map[string]string{"Range": "bytes=0-2"}, nil).expect(t, 206)
	if string(rr.Body) != "ID3" {
		t.Fatalf("range %q", rr.Body)
	}
	prep := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", nil, nil).expect(t, 202)
	if prep.Header.Get("Retry-After") == "" || prep.json(t)["status"] != "preparing" {
		t.Fatalf("preparing %v %s", prep.Header, prep.Body)
	}
	var ready *rawResp
	eventually(t, "rendition", func() bool {
		ready = v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", nil, nil)
		return ready.Status == 200
	})
	if ready.Header.Get("Content-Type") != "audio/mp4" || !strings.HasPrefix(string(ready.Body), "FAKEM4A") || len(ready.Body) != 5007 ||
		!strings.Contains(ready.Header.Get("Content-Disposition"), "song.m4a") || ready.Header.Get("ETag") == orig.Header.Get("ETag") {
		t.Fatalf("rendition %v %d", ready.Header, len(ready.Body))
	}
	if args := strings.Join(ft.argLines(), "\n"); !strings.Contains(args, "-c:a aac -b:a 128k") || !strings.Contains(args, "+faststart") {
		t.Fatalf("encode args %s", args)
	}
	etag := ready.Header.Get("ETag")
	rr = v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", map[string]string{"Range": "bytes=0-6", "If-Range": etag}, nil).expect(t, 206)
	if string(rr.Body) != "FAKEM4A" {
		t.Fatalf("rendition range %q", rr.Body)
	}
	// A stale If-Range restarts the file.
	v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", map[string]string{"Range": "bytes=0-6", "If-Range": `"stale"`}, nil).expect(t, 200)
	// Low quality is below the source bitrate.
	eventually(t, "low rendition", func() bool {
		return v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=low", nil, nil).Status == 200
	})
	if args := strings.Join(ft.argLines(), "\n"); !strings.Contains(args, "-b:a 96k") {
		t.Fatalf("low args %s", args)
	}
	// LRU: a cache for two renditions evicts the least recently used.
	v.e.Patch("music", map[string]any{"download_cache_bytes": 12000})
	v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", nil, nil).expect(t, 200) // touch normal
	eventually(t, "high rendition", func() bool {
		return v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=high", nil, nil).Status == 200
	})
	var n int
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM music_renditions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d renditions cached (%v)", n, err)
	}
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM music_renditions WHERE quality = 'low'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("least recently used rendition kept")
	}
	files, _ := os.ReadDir(filepath.Join(v.svc.Dir(), "cache"))
	if len(files) != 2 {
		t.Fatalf("%d cache files", len(files))
	}
	// A changed source gets a new rendition and ETag.
	q := "space_id=" + home + "&path=&name=song.mp3&overwrite=1"
	v.raw(v.adm, "POST", "/api/v1/files/upload?"+q, map[string]string{"Content-Type": "application/octet-stream"},
		mt.MP3(mt.ID3v2(mt.ID3Options{}, mt.Text("TIT2", mt.UTF8, "Song v2")), 2, nil)).expect(t, 201)
	eventually(t, "re-read", func() bool { return v.track(v.adm, id)["title"] == "Song v2" })
	v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", nil, nil).expect(t, 202)
	eventually(t, "new rendition", func() bool {
		r := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download?quality=normal", nil, nil)
		return r.Status == 200 && r.Header.Get("ETag") != etag
	})
	// AAC in MP4 at or below the bitrate is served as is (no encode).
	m4a := v.upload(v.adm, home, "", "a.m4a", mt.M4A(mt.M4AOptions{Seconds: 60, Items: []mt.Item{mt.TextItem("\xa9nam", "AAC")}}))
	v.waitProbed(v.adm, m4a)
	encodes := len(ft.argLines())
	pt := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+m4a+"/download?quality=high", nil, nil).expect(t, 200)
	if pt.Header.Get("Content-Type") != "audio/mp4" || len(ft.argLines()) != encodes {
		t.Fatalf("passthrough %v", pt.Header)
	}
	v.adm.Do("GET", "/api/v1/music/tracks/"+id+"/download?quality=ultra", nil).ExpectError(400, "bad_request")
	// A 0-byte cache disables renditions; originals still work.
	v.e.Patch("music", map[string]any{"download_cache_bytes": 0})
	v.adm.Do("GET", "/api/v1/music/tracks/"+id+"/download?quality=low", nil).ExpectError(501, "ffmpeg_unavailable")
	v.raw(v.adm, "GET", "/api/v1/music/tracks/"+id+"/download", nil, nil).expect(t, 200)
	if err := v.svc.GC(ctx()); err != nil {
		t.Fatal(err)
	}
}

// fakeFFprobe describes any input as a WMA file with tags.
const fakeFFprobe = `#!/bin/sh
cat <<'JSON'
{"streams":[{"codec_type":"audio","codec_name":"wmav2","sample_rate":"44100","channels":2,"duration":"123.4"}],
 "format":{"format_name":"asf","duration":"123.4","bit_rate":"128000",
  "tags":{"title":"WMA Song","artist":"WMA Artist","album":"WMA Album","track":"4/10","genre":"Ambient;Chill","date":"2003",
   "REPLAYGAIN_TRACK_GAIN":"-3.00 dB","lyrics-eng":"la la"}}}
JSON
`

func TestFFprobeFallback(t *testing.T) {
	v := newEnv(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(script, []byte(fakeFFprobe), 0o755); err != nil {
		t.Fatal(err)
	}
	v.e.Patch("media", map[string]any{"ffprobe_path": script})
	v.svc.RefreshTools(ctx())
	id := v.upload(v.adm, v.home(v.adm), "", "song.wma", []byte("0&\xb2\x75\x8e\x66\xcf\x11 not really asf"))
	tr := v.waitProbed(v.adm, id)
	if tr["probe_status"] != "ok" || tr["title"] != "WMA Song" || tr["artist"] != "WMA Artist" || num(tr["track_number"]) != 4 ||
		num(tr["track_total"]) != 10 || num(tr["year"]) != 2003 || tr["codec"] != "wmav2" || tr["container"] != "asf" ||
		num(tr["duration_seconds"]) != 123.4 || len(tr["genres"].([]any)) != 2 || tr["has_lyrics"] != true ||
		num(tr["replaygain"].(map[string]any)["track_gain_db"]) != -3 {
		t.Fatalf("ffprobe track %v", tr)
	}
}

// endlessFFmpeg writes output forever (like ffmpeg, which only stops on pipe backpressure).
const endlessFFmpeg = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    -version) echo "ffmpeg version 9.9-fake Copyright (c) the fakers"; exit 0;;
  esac
done
echo $$ >> "PIDFILE"
exec cat /dev/zero
`

// TestTranscodeStalledClient: a client that keeps the connection open but stops reading must
// not hold a transcode slot (and an ffmpeg process) forever.
func TestTranscodeStalledClient(t *testing.T) {
	v := newEnv(t)
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(endlessFFmpeg, "PIDFILE", pids)), 0o755); err != nil {
		t.Fatal(err)
	}
	v.e.Patch("media", map[string]any{"ffmpeg_path": script})
	eventually(t, "ffmpeg detected", func() bool { return v.media.FFmpegPath(ctx()) == script })
	v.svc.SetTranscodeTiming(3*time.Second, time.Second)
	id := v.upload(v.adm, v.home(v.adm), "", "a.flac", mt.FLAC(mt.FLACOptions{Seconds: 100}))
	v.waitProbed(v.adm, id)
	me := v.principalOf(v.adm).UserID

	u, _ := url.Parse(v.e.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	fmt.Fprintf(conn, "GET /api/v1/music/tracks/%s/transcode HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", id, u.Host, v.adm.Bearer)
	eventually(t, "transcode started", func() bool { total, _ := v.svc.TranscodesInUse(me); return total == 1 })
	eventually(t, "ffmpeg pid", func() bool { return len(readPIDs(pids)) == 1 })
	eventually(t, "stalled transcode released", func() bool { total, _ := v.svc.TranscodesInUse(me); return total == 0 })
	pid := readPIDs(pids)[0]
	eventually(t, "ffmpeg killed", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}
