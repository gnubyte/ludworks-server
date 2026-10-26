package tv

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type feedsResp struct {
	M3UURL   string  `json:"m3u_url"`
	XMLTVURL string  `json:"xmltv_url"`
	HDHRURL  *string `json:"hdhr_url"`
}

func tokenOf(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if tok := p.Query().Get("access_token"); tok != "" {
		return tok
	}
	return p.Path[strings.LastIndex(p.Path, "/")+1:]
}

func pathOf(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return p.RequestURI()
}

func TestFeedsM3UXMLTVAndHDHR(t *testing.T) {
	v := newEnv(t)
	alice, ac := v.user("alice")
	lid, ids := v.seedShow(alice, ac, `Quote "Show"`, 3)
	var ch, ch2 Channel
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "Quote \"One\"", "lineup_id": lid, "group": "Fun"}, 201, &ch)
	ac.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "Bad\nname", "lineup_id": lid}).ExpectError(422, "validation_failed")
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "B & <C>", "lineup_id": lid}, 201, &ch2)

	var f feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f)
	tok := tokenOf(t, f.M3UURL)
	if !strings.HasPrefix(tok, "lwp_") || tokenOf(t, f.XMLTVURL) != tok || f.HDHRURL != nil {
		t.Fatalf("feeds = %+v", f)
	}
	if !strings.HasPrefix(f.M3UURL, v.e.URL+"/api/v1/tv/channels.m3u?access_token=") {
		t.Errorf("m3u url = %s", f.M3UURL)
	}
	var f2 feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f2)
	if f2.M3UURL != f.M3UURL {
		t.Error("feed token changed between calls")
	}
	// The token is a scoped PAT listed in the user's tokens.
	var toks []map[string]any
	do(t, ac, "GET", "/api/v1/me/tokens", nil, 200, &toks)
	// ... with tv:feed only: no media:read or tv:read, so a leaked feed URL reaches the feeds
	// and streams but neither the library nor the rest of the TV API.
	if sc, _ := toks[0]["scopes"].([]any); len(toks) != 1 || len(sc) != 1 || sc[0] != "tv:feed" {
		t.Errorf("tokens = %v", toks)
	}
	// A credential with tv:read (all the feed token has) may reveal it; one without may not
	// mint or reveal it.
	var pat struct {
		Token string `json:"token"`
	}
	do(t, ac, "POST", "/api/v1/me/tokens", map[string]any{"name": "tv only", "scopes": []string{"tv:read", "tv:write"}}, 201, &pat)
	var f4 feedsResp
	do(t, v.e.WithBearer(pat.Token), "GET", "/api/v1/tv/feeds", nil, 200, &f4)
	if f4.M3UURL != f.M3UURL {
		t.Error("a tv:read credential got a different feed token")
	}
	var wpat struct {
		Token string `json:"token"`
	}
	do(t, ac, "POST", "/api/v1/me/tokens", map[string]any{"name": "tv write only", "scopes": []string{"tv:write"}}, 201, &wpat)
	writeOnly := v.e.WithBearer(wpat.Token)
	writeOnly.Do("GET", "/api/v1/tv/feeds", nil).ExpectError(403, "forbidden")
	writeOnly.Do("POST", "/api/v1/tv/feeds/rotate", nil).ExpectError(403, "forbidden")

	// M3U: attributes are sanitised, URLs carry the token, without ffmpeg channels use /play.
	code, hdr, body := v.raw(nil, "GET", pathOf(t, f.M3UURL), nil, nil)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "audio/x-mpegurl") {
		t.Fatalf("m3u = %d %s", code, hdr.Get("Content-Type"))
	}
	m3u := string(body)
	if !strings.HasPrefix(m3u, "#EXTM3U url-tvg=\""+f.XMLTVURL+"\"") {
		t.Errorf("m3u header: %s", m3u)
	}
	wantInf := `#EXTINF:-1 tvg-id="` + ch.ID + `" tvg-chno="1" tvg-name="Quote 'One'" group-title="Fun",1 Quote 'One'`
	if !strings.Contains(m3u, wantInf+"\n"+v.e.URL+"/api/v1/tv/channels/"+ch.ID+"/play?access_token="+tok+"\n") {
		t.Errorf("m3u entry missing:\n%s", m3u)
	}
	if strings.Count(m3u, "#EXTINF") != 2 {
		t.Errorf("m3u entries: %s", m3u)
	}
	// /play via the embedded token works for an external player.
	code, hdr, _ = v.raw(nil, "GET", "/api/v1/tv/channels/"+ch.ID+"/play?access_token="+tok, nil, nil)
	if code != 302 || !strings.Contains(hdr.Get("Location"), "/programs/"+ids[0]+"/stream?access_token=") {
		t.Errorf("play = %d %s", code, hdr.Get("Location"))
	}

	// XMLTV: well-formed, one <channel> per channel and contiguous programmes.
	code, hdr, body = v.raw(nil, "GET", pathOf(t, f.XMLTVURL), nil, nil)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/xml") {
		t.Fatalf("xmltv = %d", code)
	}
	var tv struct {
		Channels []struct {
			ID    string   `xml:"id,attr"`
			Names []string `xml:"display-name"`
		} `xml:"channel"`
		Programmes []struct {
			Start   string   `xml:"start,attr"`
			Stop    string   `xml:"stop,attr"`
			Channel string   `xml:"channel,attr"`
			Title   string   `xml:"title"`
			Sub     string   `xml:"sub-title"`
			EpNums  []string `xml:"episode-num"`
		} `xml:"programme"`
	}
	if err := xml.Unmarshal(body, &tv); err != nil {
		t.Fatalf("xmltv parse: %v\n%s", err, body[:min(len(body), 800)])
	}
	if len(tv.Channels) != 2 || tv.Channels[1].Names[0] != "B & <C>" {
		t.Errorf("xmltv channels = %+v", tv.Channels)
	}
	var progs int
	prevStop := ""
	for _, p := range tv.Programmes {
		if p.Channel != ch.ID {
			continue
		}
		if prevStop != "" && p.Start != prevStop {
			t.Fatalf("xmltv gap %s → %s", prevStop, p.Start)
		}
		prevStop = p.Stop
		progs++
	}
	// guide_days 3 → 72 h of ~63 s programmes (capped per channel).
	if progs < 3000 || tv.Programmes[0].Title != `Quote "Show"` || tv.Programmes[0].EpNums[0] != "0.0." || tv.Programmes[0].EpNums[1] != "S01E01" {
		t.Errorf("xmltv programmes = %d, first %+v", progs, tv.Programmes[0])
	}

	// Rotation: new token, the old one stops working everywhere.
	var f3 feedsResp
	do(t, ac, "POST", "/api/v1/tv/feeds/rotate", nil, 200, &f3)
	if tokenOf(t, f3.M3UURL) == tok {
		t.Fatal("rotate kept the token")
	}
	if code, _, _ = v.raw(nil, "GET", pathOf(t, f.M3UURL), nil, nil); code != 401 {
		t.Errorf("old token m3u = %d", code)
	}
	tok = tokenOf(t, f3.M3UURL)

	// HDHomeRun: 404 while disabled, for unknown tokens and for PATs that are not feed tokens.
	hd := "/api/v1/hdhr/" + tok
	if code, _, _ = v.raw(nil, "GET", hd+"/discover.json", nil, nil); code != 404 {
		t.Errorf("hdhr disabled = %d", code)
	}
	v.e.Patch("tv", map[string]any{"hdhr_enabled": true, "hdhr_device_name": "Den TV", "hdhr_tuner_count": 3})
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f3)
	if f3.HDHRURL == nil || *f3.HDHRURL != v.e.URL+hd {
		t.Fatalf("hdhr url = %v", f3.HDHRURL)
	}
	for _, bad := range []string{"lwp_doesnotexist12345", "nottoken", pat.Token} {
		if code, _, _ = v.raw(nil, "GET", "/api/v1/hdhr/"+bad+"/discover.json", nil, nil); code != 404 {
			t.Errorf("hdhr with %q = %d", bad, code)
		}
	}
	code, _, body = v.raw(nil, "GET", hd+"/discover.json", nil, nil)
	var disc HDHRDiscover
	if code != 200 || json.Unmarshal(body, &disc) != nil {
		t.Fatalf("discover = %d %s", code, body)
	}
	if disc.FriendlyName != "Den TV" || disc.TunerCount != 3 || disc.BaseURL != v.e.URL+hd || disc.LineupURL != v.e.URL+hd+"/lineup.json" || len(disc.DeviceID) != 8 {
		t.Errorf("discover = %+v", disc)
	}
	if strings.Contains(disc.DeviceAuth, tok) {
		t.Error("DeviceAuth leaks the token")
	}
	code, _, body = v.raw(nil, "GET", hd+"/lineup.json", nil, nil)
	var lineup []HDHRLineupEntry
	if code != 200 || json.Unmarshal(body, &lineup) != nil || len(lineup) != 2 {
		t.Fatalf("lineup.json = %d %s", code, body)
	}
	if lineup[0].GuideNumber != "1" || lineup[0].URL != v.e.URL+"/api/v1/tv/channels/"+ch.ID+"/play?access_token="+tok {
		t.Errorf("lineup[0] = %+v", lineup[0])
	}
	code, _, body = v.raw(nil, "GET", hd+"/lineup_status.json", nil, nil)
	if code != 200 || !strings.Contains(string(body), `"ScanPossible":1`) {
		t.Errorf("lineup_status = %d %s", code, body)
	}
	code, hdr, body = v.raw(nil, "GET", hd+"/device.xml", nil, nil)
	var dev struct {
		URLBase string `xml:"URLBase"`
		Name    string `xml:"device>friendlyName"`
	}
	if code != 200 || xml.Unmarshal(body, &dev) != nil || dev.Name != "Den TV" || dev.URLBase != v.e.URL+hd {
		t.Errorf("device.xml = %d %+v", code, dev)
	}
	if code, _, _ = v.raw(nil, "POST", hd+"/lineup.post?scan=start", nil, nil); code != 200 {
		t.Errorf("lineup.post = %d", code)
	}
	// Revoking the token from the account's token list disables the HDHR device too.
	do(t, ac, "GET", "/api/v1/me/tokens", nil, 200, &toks)
	for _, tk := range toks {
		if tk["name"] == feedTokenName {
			ac.Do("DELETE", "/api/v1/me/tokens/"+tk["id"].(string), nil).Expect(204)
		}
	}
	if code, _, _ = v.raw(nil, "GET", hd+"/discover.json", nil, nil); code != 404 {
		t.Errorf("revoked hdhr = %d", code)
	}
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f3)
	if tokenOf(t, f3.M3UURL) == tok {
		t.Error("revoked feed token was reused")
	}

	// Cookie sessions get M3U URLs with the user's feed token (no cookie in URLs).
	r := v.admin.Do("GET", "/api/v1/tv/channels.m3u", nil).Expect(200)
	if !strings.Contains(string(r.Body), "#EXTM3U") {
		t.Errorf("admin m3u = %s", r.Body)
	}
}

func TestLiveTSChainsPrograms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg is a shell script")
	}
	v := newEnv(t)
	lid, ids := v.seedShow(v.adminID, v.adm, "Live", 3)
	var ch Channel
	do(t, v.adm, "POST", "/api/v1/tv/channels", map[string]any{"name": "Live", "lineup_id": lid}, 201, &ch)
	path := "/api/v1/tv/channels/" + ch.ID + "/live.ts"
	v.adm.Do("GET", path, nil).ExpectError(501, "ffmpeg_unavailable")

	// Fake ffmpeg: prints one line per invocation with -ss, -output_ts_offset and the input.
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
ss=0; off=0; in=""
while [ $# -gt 0 ]; do
  case "$1" in
    -ss) ss="$2"; shift ;;
    -output_ts_offset) off="$2"; shift ;;
    -i) in="$2"; shift ;;
  esac
  shift
done
printf 'PROG ss=%s off=%s in=%s\n' "$ss" "$off" "$in"
sleep 0.05
`
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	v.media.mu.Lock()
	v.media.ffmpeg = ff
	v.media.mu.Unlock()
	v.tv.ffMu.Lock()
	v.tv.ffChecked = time.Time{}
	v.tv.ffMu.Unlock()
	v.advance(30 * time.Second) // 30 s into Ep1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", v.e.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+v.adm.Bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("live.ts = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for len(lines) < 4 && sc.Scan() {
		lines = append(lines, sc.Text())
	}
	cancel()
	if len(lines) < 4 {
		t.Fatalf("lines = %v (%v)", lines, sc.Err())
	}
	if !strings.HasPrefix(lines[0], "PROG ss=30.000 off=0 in=file:/") || !strings.Contains(lines[0], "Live e01.mp4") {
		t.Errorf("first = %s", lines[0])
	}
	prevOff := 0.0
	for i, ln := range lines[1:] {
		if !strings.HasPrefix(ln, "PROG ss=0 off=") {
			t.Errorf("line %d = %s", i+1, ln)
			continue
		}
		offStr := strings.Fields(ln)[2][len("off="):]
		off, err := strconv.ParseFloat(offStr, 64)
		if err != nil || off <= prevOff {
			t.Errorf("output_ts_offset not increasing: %s after %v", ln, prevOff)
		}
		prevOff = off
	}
	if !strings.Contains(lines[1], "Live e02.mp4") || !strings.Contains(lines[2], "Live e03.mp4") || !strings.Contains(lines[3], "Live e01.mp4") {
		t.Errorf("program chain = %v (want e02, e03, e01; ids %v)", lines, ids)
	}
	// The tuner is released once the client is gone.
	waitFor(t, func() bool {
		v.tv.liveMu.Lock()
		defer v.tv.liveMu.Unlock()
		return v.tv.liveTotal == 0
	})
	// Per-user tuner limit.
	v.e.Patch("tv", map[string]any{"hdhr_tuner_count": 1})
	rel, ok := v.tv.acquireTuner(v.adminID, 1)
	if !ok {
		t.Fatal("tuner not acquired")
	}
	v.adm.Do("GET", path, nil).ExpectError(429, "rate_limited")
	rel()
}
