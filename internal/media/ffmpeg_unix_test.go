//go:build !windows

package media_test

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
	"ludworks/internal/media/mediatest"
)

const fakeFFmpeg = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    -version) echo "ffmpeg version 9.9-fake Copyright (c) the fakers"; exit 0;;
    webvtt) printf 'WEBVTT\n\nSTYLE\n::cue { color: red }\n\n00:00:01.000 --> 00:00:02.000\nembedded <i>hello</i>\n'; exit 0;;
    image2pipe) exit 1;;
  esac
done
echo $$ >> "PIDFILE"
printf 'FAKEFMP4'
exec sleep 30
`

// TestFFmpegPaths uses a fake ffmpeg to cover transcoding (streaming, the concurrency limit,
// killing ffmpeg on disconnect) and embedded subtitle extraction.
func TestFFmpegPaths(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(fakeFFmpeg, "PIDFILE", pidFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	v.e.Patch("media", map[string]any{"ffmpeg_path": script, "auto_thumbnails": false})
	v.svc.RefreshTools(ctx())
	if v.svc.FFmpegPath(ctx()) != script {
		t.Fatalf("ffmpeg not detected: %q", v.svc.FFmpegPath(ctx()))
	}
	v.svc.SetTranscodeTiming(300*time.Millisecond, 30*time.Second)
	id := v.upload(v.adm, home, "", "Show.S01E01.mkv", mediatest.HEVCMKV(600))["id"].(string)
	v.waitProbed(v.adm, id)
	_, bob := v.user("bob")
	bobID := v.upload(bob, v.home(bob), "", "Bob.S01E01.mkv", mediatest.HEVCMKV(600))["id"].(string)
	v.waitProbed(bob, bobID)
	_, carol := v.user("carol")
	carolID := v.upload(carol, v.home(carol), "", "Carol.S01E01.mkv", mediatest.HEVCMKV(600))["id"].(string)
	v.waitProbed(carol, carolID)

	openAs := func(c *authtest.Client, fileID string) (*http.Response, context.CancelFunc) {
		t.Helper()
		cx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(cx, "GET", v.e.URL+"/api/v1/media/"+fileID+"/transcode?start=12.5&height=480", nil)
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, cancel
	}
	open := func() (*http.Response, context.CancelFunc) { t.Helper(); return openAs(v.adm, id) }
	resp1, cancel1 := open()
	if resp1.StatusCode != 200 || resp1.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("transcode: %d %v", resp1.StatusCode, resp1.Header)
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(bufio.NewReader(resp1.Body), head); err != nil || string(head) != "FAKEFMP4" {
		t.Fatalf("first bytes %q %v", head, err)
	}
	// One transcode per user: the admin's second one waits briefly, then gets 429.
	r := v.raw(v.adm, "GET", "/api/v1/media/"+id+"/transcode", nil, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || r.code() != "rate_limited" {
		t.Fatalf("second transcode of the same user: %d %v %s", r.Status, r.Header, r.Body)
	}
	// Another user still gets the second server-wide slot.
	resp2, cancel2 := openAs(bob, bobID)
	if resp2.StatusCode != 200 {
		t.Fatalf("other user's transcode: %d", resp2.StatusCode)
	}
	r = v.raw(carol, "GET", "/api/v1/media/"+carolID+"/transcode", nil, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || r.code() != "rate_limited" {
		t.Fatalf("third transcode: %d %v %s", r.Status, r.Header, r.Body)
	}
	cancel1()
	cancel2()
	resp1.Body.Close()
	resp2.Body.Close()
	// Both ffmpeg processes are killed once the clients disconnect.
	eventually(t, "two ffmpeg pids", func() bool { return len(readPIDs(pidFile)) == 2 })
	for _, pid := range readPIDs(pidFile) {
		eventually(t, "ffmpeg "+strconv.Itoa(pid)+" killed", func() bool {
			err := syscall.Kill(pid, 0)
			return errors.Is(err, syscall.ESRCH)
		})
	}
	// The limit is released afterwards.
	eventually(t, "slot released", func() bool {
		resp, cancel := open()
		defer cancel()
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	// HEAD does not start ffmpeg.
	before := len(readPIDs(pidFile))
	v.raw(v.adm, "HEAD", "/api/v1/media/"+id+"/transcode", nil, nil).expect(t, 200)
	time.Sleep(100 * time.Millisecond)
	if after := len(readPIDs(pidFile)); after != before {
		t.Fatalf("HEAD spawned ffmpeg (%d → %d)", before, after)
	}

	// Embedded subtitles: extracted, normalised (STYLE dropped) and cached.
	r = v.raw(v.adm, "GET", "/api/v1/media/"+id+"/subtitles/0.vtt", nil, nil).expect(t, 200)
	body := string(r.Body)
	if !strings.HasPrefix(body, "WEBVTT") || !strings.Contains(body, "embedded <i>hello</i>") || strings.Contains(body, "STYLE") {
		t.Fatalf("embedded vtt: %q", body)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/subtitles/0.vtt", nil, nil).expect(t, 200) // from cache

	// Disabling transcoding hides ffmpeg from the tv module and the transcode endpoint.
	v.e.Patch("media", map[string]any{"transcode_enabled": false})
	if v.svc.FFmpegPath(ctx()) != "" {
		t.Fatal("FFmpegPath with transcoding disabled")
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/transcode", nil, nil).expect(t, 501)
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

// endlessFFmpeg writes output forever (like ffmpeg without -re, which only stops on pipe
// backpressure).
const endlessFFmpeg = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    -version) echo "ffmpeg version 9.9-fake Copyright (c) the fakers"; exit 0;;
    image2pipe) exit 1;;
  esac
done
echo $$ >> "PIDFILE"
exec cat /dev/zero
`

// TestTranscodeStalledClientReleasesSlot: a client that keeps the connection open but stops
// reading must not hold a transcode slot (and an ffmpeg process) forever.
func TestTranscodeStalledClientReleasesSlot(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(endlessFFmpeg, "PIDFILE", pidFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	v.e.Patch("media", map[string]any{"ffmpeg_path": script, "auto_thumbnails": false})
	v.svc.RefreshTools(ctx())
	v.svc.SetTranscodeTiming(3*time.Second, time.Second)
	id := v.upload(v.adm, home, "", "Show.S01E01.mkv", mediatest.HEVCMKV(600))["id"].(string)
	v.waitProbed(v.adm, id)
	adminID := v.principalOf(v.adm).UserID

	// Raw connection: send the request, read nothing, keep the socket open.
	u, _ := url.Parse(v.e.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	fmt.Fprintf(conn, "GET /api/v1/media/%s/transcode HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", id, u.Host, v.adm.Bearer)
	eventually(t, "transcode started", func() bool { total, _ := v.svc.TranscodesInUse(adminID); return total == 1 })
	eventually(t, "ffmpeg pid", func() bool { return len(readPIDs(pidFile)) == 1 })

	// Within the write timeout (plus margin) the write fails, ffmpeg is killed and the slot freed.
	eventually(t, "stalled transcode slot released", func() bool { total, _ := v.svc.TranscodesInUse(adminID); return total == 0 })
	pid := readPIDs(pidFile)[0]
	eventually(t, "ffmpeg killed", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })

	// A client that reads keeps streaming past the timeout.
	req, _ := http.NewRequest("GET", v.e.URL+"/api/v1/media/"+id+"/transcode", nil)
	req.Header.Set("Authorization", "Bearer "+v.adm.Bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("transcode: %v %v", resp, err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	var n int64
	buf := make([]byte, 64<<10)
	for time.Now().Before(deadline) {
		m, err := resp.Body.Read(buf)
		n += int64(m)
		if err != nil {
			t.Fatalf("reading client cut off after %d bytes: %v", n, err)
		}
		time.Sleep(time.Millisecond)
	}
	// The same user's next transcode (a seek) waits for the old one to be torn down.
	done := make(chan int, 1)
	go func() {
		r := v.raw(v.adm, "HEAD", "/api/v1/media/"+id+"/transcode", nil, nil)
		done <- r.Status
	}()
	if st := <-done; st != 200 {
		t.Fatalf("HEAD: %d", st)
	}
	go func() {
		req, _ := http.NewRequest("GET", v.e.URL+"/api/v1/media/"+id+"/transcode?start=30", nil)
		req.Header.Set("Authorization", "Bearer "+v.adm.Bearer)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		r.Body.Close()
		done <- r.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	resp.Body.Close() // the player drops the old stream
	if st := <-done; st != 200 {
		t.Fatalf("seek transcode while the old one was closing: %d", st)
	}
}
