package tv

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type nowResp struct {
	Channel       Channel   `json:"channel"`
	Program       *Program  `json:"program"`
	OffsetSeconds float64   `json:"offset_seconds"`
	ServerTime    string    `json:"server_time"`
	Next          []Program `json:"next"`
	Index         *int64    `json:"index"`
}

type guideResp struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Channels []struct {
		Channel  Channel   `json:"channel"`
		Programs []Program `json:"programs"`
	} `json:"channels"`
}

// seedShow creates n one-minute-ish episodes ("Ep i", durations 60+i s) for userID and a
// lineup with the show; it returns the lineup id and the file ids in episode order.
func (v *env) seedShow(userID string, c *authtest.Client, show string, n int) (string, []string) {
	v.t.Helper()
	var ids []string
	for i := 1; i <= n; i++ {
		ids = append(ids, v.addFile(userID, "TV/"+show, fmt.Sprintf("%s e%02d.mp4", show, i), &mediaRow{
			Title: fmt.Sprintf("Ep %d", i), ShowTitle: show, Season: ip(1), Episode: ip(i), EpisodeTitle: fmt.Sprintf("Ep %d", i),
			Duration: float64(60 + i), Category: "tv", Playable: i%2 == 1}))
	}
	var l lineupResp
	do(v.t, c, "POST", "/api/v1/tv/lineups", map[string]any{"name": show, "items": []any{
		map[string]any{"kind": "show", "show_title": show}}}, 201, &l)
	return l.ID, ids
}

func TestChannelsScheduleAndGuide(t *testing.T) {
	v := newEnv(t)
	c := v.adm
	lid, ids := v.seedShow(v.adminID, c, "Toons", 5) // T = 61+62+63+64+65 = 315 s

	// Validation and numbering.
	c.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "X"}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "X", "lineup_id": lid, "number": 10000}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "X", "lineup_id": lid, "shuffle_mode": "weekly"}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "X", "lineup_id": "zzzzzzzzzzzz"}).ExpectError(404, "not_found")
	var ch1, ch2, ch3 Channel
	do(t, c, "POST", "/api/v1/tv/channels", map[string]any{"name": "Toons", "lineup_id": lid, "group": "Kids"}, 201, &ch1)
	do(t, c, "POST", "/api/v1/tv/channels", map[string]any{"name": "Toons 2", "lineup_id": lid, "number": 7}, 201, &ch2)
	do(t, c, "POST", "/api/v1/tv/channels", map[string]any{"name": "Toons 3", "lineup_id": lid}, 201, &ch3)
	if ch1.Number != 1 || ch2.Number != 7 || ch3.Number != 8 {
		t.Errorf("numbers = %d %d %d", ch1.Number, ch2.Number, ch3.Number)
	}
	if ch1.LineupName != "Toons" || ch1.ShuffleMode != "per_cycle" || ch1.PlaybackMode != "live" || ch1.Visibility != "private" || ch1.LogoURL != nil {
		t.Errorf("defaults = %+v", ch1)
	}
	c.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "Dup", "lineup_id": lid, "number": 7}).ExpectError(409, "conflict")
	c.Do("PATCH", "/api/v1/tv/channels/"+ch3.ID, map[string]any{"number": 7}).ExpectError(409, "conflict")
	do(t, c, "PATCH", "/api/v1/tv/channels/"+ch3.ID, map[string]any{"number": 3, "name": "Three"}, 200, &ch3)
	if ch3.Number != 3 || ch3.Name != "Three" {
		t.Errorf("patched = %+v", ch3)
	}
	var list []Channel
	do(t, c, "GET", "/api/v1/tv/channels", nil, 200, &list)
	if len(list) != 3 || list[0].Number != 1 || list[1].Number != 3 || list[2].Number != 7 {
		t.Errorf("list order = %+v", list)
	}

	// /now: deterministic, offset from the anchor (channel created at the fake clock).
	v.advance(100 * time.Second) // 100 s into Ep1 (61 s) → Ep2 at 39 s
	var n1, n2 nowResp
	do(t, c, "GET", "/api/v1/tv/channels/"+ch1.ID+"/now", nil, 200, &n1)
	do(t, c, "GET", "/api/v1/tv/channels/"+ch1.ID+"/now", nil, 200, &n2)
	if n1.Program == nil || n1.Program.FileID != ids[1] || n1.OffsetSeconds != 39 || n1.Program.Title != "Ep 2" {
		t.Fatalf("now = %+v %+v", n1, n1.Program)
	}
	if n1.Program.FileID != n2.Program.FileID || n1.OffsetSeconds != n2.OffsetSeconds || n1.Program.StartsAt != n2.Program.StartsAt {
		t.Errorf("not deterministic: %+v vs %+v", n1, n2)
	}
	p := n1.Program
	base := "/api/v1/tv/channels/" + ch1.ID + "/programs/" + ids[1]
	if p.StreamURL != base+"/stream" || p.TranscodeURL != nil || p.PosterURL != nil || p.BrowserPlayable {
		t.Errorf("program urls = %+v", p)
	}
	if len(n1.Next) != 3 || n1.Next[0].FileID != ids[2] || n1.Next[0].StartsAt != p.EndsAt || *n1.Index != 1 {
		t.Errorf("next = %+v index %v", n1.Next, n1.Index)
	}
	// With ffmpeg the transcode URL appears.
	v.media.mu.Lock()
	v.media.ffmpeg = "/usr/bin/ffmpeg"
	v.media.mu.Unlock()
	v.tv.ffMu.Lock()
	v.tv.ffChecked = time.Time{}
	v.tv.ffMu.Unlock()
	do(t, c, "GET", "/api/v1/tv/channels/"+ch1.ID+"/now", nil, 200, &n2)
	if n2.Program.TranscodeURL == nil || *n2.Program.TranscodeURL != base+"/transcode" || n2.Program.PosterURL == nil {
		t.Errorf("with ffmpeg = %+v", n2.Program)
	}

	// Guide across a cycle boundary: contiguous slots, the same file sequence every cycle.
	start := ch1.AnchorAt
	var g guideResp
	do(t, c, "GET", "/api/v1/tv/guide?start="+url.QueryEscape(start)+"&hours=1&channel_ids="+ch1.ID, nil, 200, &g)
	if len(g.Channels) != 1 {
		t.Fatalf("guide channels = %d", len(g.Channels))
	}
	progs := g.Channels[0].Programs
	if len(progs) < 11 { // 3600 / 315 ≈ 11.4 cycles × 5
		t.Fatalf("guide programs = %d", len(progs))
	}
	for i := 1; i < len(progs); i++ {
		if progs[i].StartsAt != progs[i-1].EndsAt {
			t.Fatalf("gap at %d: %s != %s", i, progs[i].StartsAt, progs[i-1].EndsAt)
		}
		if progs[i].FileID != ids[i%5] {
			t.Fatalf("slot %d = %s, want %s", i, progs[i].FileID, ids[i%5])
		}
	}
	if progs[5].StartsAt != time.Date(2026, 10, 26, 12, 5, 15, 0, time.UTC).Format("2006-01-02T15:04:05.000Z") {
		t.Errorf("second cycle starts at %s", progs[5].StartsAt)
	}
	c.Do("GET", "/api/v1/tv/guide?hours=73", nil).ExpectError(400, "bad_request")
	c.Do("GET", "/api/v1/tv/guide?start=yesterday", nil).ExpectError(400, "bad_request")
	c.Do("GET", "/api/v1/tv/guide?channel_ids=bad", nil).ExpectError(400, "bad_request")
	do(t, c, "GET", "/api/v1/tv/guide", nil, 200, &g)
	if len(g.Channels) != 3 {
		t.Errorf("default guide channels = %d", len(g.Channels))
	}

	// Shuffle: per_cycle gives each cycle its own (deterministic) order; reshuffle changes it.
	do(t, c, "PATCH", "/api/v1/tv/channels/"+ch1.ID, map[string]any{"shuffle": true, "shuffle_mode": "fixed"}, 200, &ch1)
	order := func() []string {
		var gg guideResp
		do(t, c, "GET", "/api/v1/tv/guide?start="+url.QueryEscape(start)+"&hours=1&channel_ids="+ch1.ID, nil, 200, &gg)
		var out []string
		for _, p := range gg.Channels[0].Programs[:10] {
			out = append(out, p.FileID)
		}
		return out
	}
	fixed := order()
	if !slices.Equal(fixed[:5], fixed[5:10]) {
		t.Errorf("fixed shuffle differs between cycles: %v", fixed)
	}
	if !slices.Equal(order(), fixed) {
		t.Error("fixed shuffle not deterministic")
	}
	changed := false
	for i := 0; i < 8 && !changed; i++ { // 5! orders: a new seed almost surely changes it
		do(t, c, "POST", "/api/v1/tv/channels/"+ch1.ID+"/reshuffle", nil, 200, &ch1)
		changed = !slices.Equal(order(), fixed)
	}
	if !changed {
		t.Error("reshuffle never changed the order")
	}
	do(t, c, "PATCH", "/api/v1/tv/channels/"+ch1.ID, map[string]any{"shuffle_mode": "per_cycle"}, 200, &ch1)
	pc := order()
	sorted := func(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
	if !slices.Equal(sorted(pc[:5]), sorted(ids)) || !slices.Equal(sorted(pc[5:10]), sorted(ids)) {
		t.Errorf("per_cycle slates are not permutations: %v", pc)
	}

	// Restart re-anchors at now.
	v.advance(10 * time.Second)
	do(t, c, "POST", "/api/v1/tv/channels/"+ch2.ID+"/restart", nil, 200, &ch2)
	if ch2.AnchorAt != v.now().Format("2006-01-02T15:04:05.000Z") {
		t.Errorf("anchor = %s", ch2.AnchorAt)
	}
	do(t, c, "GET", "/api/v1/tv/channels/"+ch2.ID+"/now", nil, 200, &n1)
	if n1.Program.FileID != ids[0] || n1.OffsetSeconds != 0 {
		t.Errorf("after restart = %+v", n1)
	}

	// On-demand: per-user position.
	c.Do("PUT", "/api/v1/tv/channels/"+ch2.ID+"/position", map[string]any{"index": 1, "offset_seconds": 5}).ExpectError(409, "conflict")
	do(t, c, "PATCH", "/api/v1/tv/channels/"+ch2.ID, map[string]any{"playback_mode": "on_demand"}, 200, &ch2)
	do(t, c, "GET", "/api/v1/tv/channels/"+ch2.ID+"/now", nil, 200, &n1)
	if n1.Program.FileID != ids[0] || n1.OffsetSeconds != 0 || *n1.Index != 0 {
		t.Errorf("fresh on-demand = %+v", n1)
	}
	c.Do("PUT", "/api/v1/tv/channels/"+ch2.ID+"/position", map[string]any{"index": -1, "offset_seconds": 5}).ExpectError(422, "validation_failed")
	c.Do("PUT", "/api/v1/tv/channels/"+ch2.ID+"/position", map[string]any{"index": 1}).ExpectError(422, "validation_failed")
	c.Do("PUT", "/api/v1/tv/channels/"+ch2.ID+"/position", map[string]any{"index": 7, "offset_seconds": 12.5}).Expect(204)
	v.advance(time.Hour) // on-demand does not move while nobody watches
	do(t, c, "GET", "/api/v1/tv/channels/"+ch2.ID+"/now", nil, 200, &n1)
	if n1.Program.FileID != ids[2] || n1.OffsetSeconds != 12.5 || *n1.Index != 7 {
		t.Errorf("resumed on-demand = %+v index %d", n1.Program, *n1.Index)
	}
	// Finished program → next one.
	c.Do("PUT", "/api/v1/tv/channels/"+ch2.ID+"/position", map[string]any{"index": 7, "offset_seconds": 1000}).Expect(204)
	do(t, c, "GET", "/api/v1/tv/channels/"+ch2.ID+"/now", nil, 200, &n1)
	if n1.Program.FileID != ids[3] || n1.OffsetSeconds != 0 || *n1.Index != 8 {
		t.Errorf("after finishing = %+v", n1.Program)
	}
	do(t, c, "GET", "/api/v1/tv/guide?hours=1&channel_ids="+ch2.ID, nil, 200, &g)
	if len(g.Channels[0].Programs) == 0 || g.Channels[0].Programs[0].FileID != ids[3] {
		t.Errorf("on-demand guide = %+v", g.Channels[0].Programs)
	}

	// Empty lineup: nothing on air.
	var empty lineupResp
	do(t, c, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Empty"}, 201, &empty)
	var ce Channel
	do(t, c, "POST", "/api/v1/tv/channels", map[string]any{"name": "Dead air", "lineup_id": empty.ID}, 201, &ce)
	r := c.Do("GET", "/api/v1/tv/channels/"+ce.ID+"/now", nil).Expect(200)
	if !strings.Contains(string(r.Body), `"program":null`) || !strings.Contains(string(r.Body), `"next":[]`) {
		t.Errorf("empty now = %s", r.Body)
	}
	c.Do("GET", "/api/v1/tv/channels/"+ce.ID+"/play", nil).ExpectError(404, "not_found")

	c.Do("DELETE", "/api/v1/tv/channels/"+ce.ID, nil).Expect(204)
	c.Do("GET", "/api/v1/tv/channels/"+ce.ID, nil).ExpectError(404, "not_found")
}

func TestSharedChannelAccessAndPrograms(t *testing.T) {
	v := newEnv(t)
	alice, ac := v.user("alice")
	bob, bc := v.user("bob")
	lid, ids := v.seedShow(alice, ac, "Docs", 3)
	other := v.addFile(alice, "", "not-in-lineup.mp4", &mediaRow{Title: "Other", Duration: 30, Category: "movie"})
	var ch Channel
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "Docs", "lineup_id": lid, "number": 5}, 201, &ch)
	stream := "/api/v1/tv/channels/" + ch.ID + "/programs/" + ids[0] + "/stream"

	// Private: Bob sees nothing (404 everywhere).
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID, nil).ExpectError(404, "not_found")
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil).ExpectError(404, "not_found")
	bc.Do("GET", stream, nil).ExpectError(404, "not_found")
	var list []Channel
	do(t, bc, "GET", "/api/v1/tv/channels", nil, 200, &list)
	if len(list) != 0 {
		t.Errorf("bob lists %d channels", len(list))
	}

	// Shared: Bob can watch, but not change it, and only the channel's own programs.
	do(t, ac, "PATCH", "/api/v1/tv/channels/"+ch.ID, map[string]any{"visibility": "shared"}, 200, &ch)
	do(t, bc, "GET", "/api/v1/tv/channels", nil, 200, &list)
	if len(list) != 1 || list[0].ID != ch.ID {
		t.Fatalf("bob list = %+v", list)
	}
	var n nowResp
	do(t, bc, "GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil, 200, &n)
	if n.Program == nil || n.Program.FileID != ids[0] {
		t.Fatalf("bob now = %+v", n)
	}
	r := bc.Do("GET", stream, nil).Expect(200)
	if string(r.Body) != "STREAM:"+ids[0] || v.media.last() != "stream "+alice+" "+ids[0] {
		t.Errorf("stream = %q via %q", r.Body, v.media.last())
	}
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[1]+"/transcode?start=12.5&height=480", nil).Expect(200)
	if v.media.last() != "transcode "+alice+" "+ids[1]+" 12.5 480" {
		t.Errorf("transcode delegated as %q", v.media.last())
	}
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[1]+"/transcode?height=999", nil).ExpectError(400, "bad_request")
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[1]+"/transcode?start=-1", nil).ExpectError(400, "bad_request")
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[2]+"/poster?size=lg", nil).Expect(200)
	if v.media.last() != "poster "+alice+" "+ids[2]+" lg" {
		t.Errorf("poster delegated as %q", v.media.last())
	}
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[2]+"/poster?size=xl", nil).ExpectError(422, "validation_failed")
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[2]+"/subtitles/3.vtt", nil).Expect(200)
	if v.media.last() != "subtitle "+alice+" "+ids[2]+" 3" {
		t.Errorf("subtitle delegated as %q", v.media.last())
	}
	for _, bad := range []string{"x.vtt", "3.srt", "03.vtt", "-1.vtt"} {
		bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+ids[2]+"/subtitles/"+bad, nil).ExpectError(404, "not_found")
	}
	// Files outside the expansion are never reachable through the channel.
	bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/programs/"+other+"/stream", nil).ExpectError(404, "not_found")
	// Bob still cannot read Alice's files directly.
	bc.Do("GET", "/api/v1/files/"+ids[0]+"/content", nil).ExpectError(404, "not_found")
	// Read-only for Bob.
	bc.Do("PATCH", "/api/v1/tv/channels/"+ch.ID, map[string]any{"name": "Mine"}).ExpectError(403, "forbidden")
	bc.Do("DELETE", "/api/v1/tv/channels/"+ch.ID, nil).ExpectError(403, "forbidden")
	bc.Do("POST", "/api/v1/tv/channels/"+ch.ID+"/reshuffle", nil).ExpectError(403, "forbidden")
	bc.Do("POST", "/api/v1/tv/channels", map[string]any{"name": "Steal", "lineup_id": lid}).ExpectError(404, "not_found")
	// The lineup of a shared channel is readable, not writable.
	bc.Do("GET", "/api/v1/tv/lineups/"+lid, nil).Expect(200)
	bc.Do("GET", "/api/v1/tv/lineups/"+lid+"/expanded", nil).Expect(200)
	bc.Do("PATCH", "/api/v1/tv/lineups/"+lid, map[string]any{"name": "x"}).ExpectError(403, "forbidden")
	// Query-token access for streams (as players use it).
	var tok struct {
		Token string `json:"token"`
	}
	do(t, bc, "POST", "/api/v1/me/tokens", map[string]any{"name": "player", "scopes": []string{"tv:read"}}, 201, &tok)
	code, _, body := v.raw(nil, "GET", stream+"?access_token="+tok.Token, nil, nil)
	if code != 200 || string(body) != "STREAM:"+ids[0] {
		t.Errorf("query token stream = %d %s", code, body)
	}
	code, _, _ = v.raw(nil, "GET", "/api/v1/tv/channels/"+ch.ID+"/now?access_token="+tok.Token, nil, nil)
	if code != 401 {
		t.Errorf("/now accepted a query token: %d", code)
	}

	// Bob's on-demand position is his own.
	do(t, ac, "PATCH", "/api/v1/tv/channels/"+ch.ID, map[string]any{"playback_mode": "on_demand"}, 200, &ch)
	bc.Do("PUT", "/api/v1/tv/channels/"+ch.ID+"/position", map[string]any{"index": 2, "offset_seconds": 3}).Expect(204)
	do(t, bc, "GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil, 200, &n)
	if n.Program.FileID != ids[2] {
		t.Errorf("bob on-demand = %+v", n.Program)
	}
	do(t, ac, "GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil, 200, &n)
	if n.Program.FileID != ids[0] {
		t.Errorf("alice on-demand = %+v", n.Program)
	}

	// /play redirects to the current program, carrying a query token.
	do(t, ac, "PATCH", "/api/v1/tv/channels/"+ch.ID, map[string]any{"playback_mode": "live"}, 200, &ch)
	v.advance(70 * time.Second) // Ep1 is 61 s → Ep2 at 9 s
	code, hdr, _ := v.raw(nil, "GET", "/api/v1/tv/channels/"+ch.ID+"/play?access_token="+tok.Token, nil, nil)
	want := "/api/v1/tv/channels/" + ch.ID + "/programs/" + ids[1] + "/stream?access_token=" + tok.Token + "#t=9"
	if code != 302 || hdr.Get("Location") != want {
		t.Errorf("play = %d %q want %q", code, hdr.Get("Location"), want)
	}

	// Disabling the owner takes the channel's programs off the air for everyone.
	v.admin.Do("PATCH", "/api/v1/admin/users/"+alice, map[string]any{"disabled": true}).Expect(200)
	v.tv.cache.invalidateAll()
	bc.Do("GET", stream, nil).ExpectError(404, "not_found")
	_ = bob
}

func TestChannelLogo(t *testing.T) {
	v := newEnv(t)
	var l lineupResp
	do(t, v.adm, "POST", "/api/v1/tv/lineups", map[string]any{"name": "L"}, 201, &l)
	var ch Channel
	do(t, v.adm, "POST", "/api/v1/tv/channels", map[string]any{"name": "C", "lineup_id": l.ID}, 201, &ch)
	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for x := 0; x < 800; x++ {
		for y := 0; y < 400; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	path := "/api/v1/tv/channels/" + ch.ID + "/logo"
	code, _, body := v.raw(v.adm, "PUT", path, map[string]string{"Content-Type": "image/png"}, []byte("<svg/>"))
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("svg logo = %d %s", code, body)
	}
	code, _, body = v.raw(v.adm, "PUT", path, map[string]string{"Content-Type": "image/png"}, buf.Bytes())
	if code != 200 {
		t.Fatalf("put logo = %d %s", code, body)
	}
	v.adm.Do("GET", "/api/v1/tv/channels/"+ch.ID, nil).JSON(&ch)
	if ch.LogoURL == nil || !strings.HasPrefix(*ch.LogoURL, path+"?v=") {
		t.Fatalf("logo_url = %v", ch.LogoURL)
	}
	code, hdr, body := v.raw(v.adm, "GET", *ch.LogoURL, nil, nil)
	if code != 200 || hdr.Get("Content-Type") != "image/jpeg" || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		t.Fatalf("get logo = %d %s", code, hdr.Get("Content-Type"))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width != 512 || cfg.Height != 256 {
		t.Errorf("logo size = %+v %v", cfg, err)
	}
	code, _, _ = v.raw(v.adm, "GET", *ch.LogoURL, map[string]string{"If-None-Match": hdr.Get("ETag")}, nil)
	if code != http.StatusNotModified {
		t.Errorf("conditional logo = %d", code)
	}
	_, bc := v.user("bob")
	bc.Do("GET", path, nil).ExpectError(404, "not_found")
	v.adm.Do("DELETE", path, nil).Expect(204)
	v.adm.Do("GET", path, nil).ExpectError(404, "not_found")
}

func TestAutoGenerateAndUserDeletion(t *testing.T) {
	v := newEnv(t)
	bob, bc := v.user("bob")
	for _, show := range []string{"Alpha", "Beta"} {
		for i := 1; i <= 3; i++ {
			v.addFile(bob, "TV/"+show, fmt.Sprintf("%s %d.mp4", show, i), &mediaRow{Title: "x", ShowTitle: show,
				Season: ip(1), Episode: ip(i), Duration: 100, Category: "tv"})
		}
	}
	v.addFile(bob, "TV/Gamma", "g1.mp4", &mediaRow{Title: "g", ShowTitle: "Gamma", Season: ip(1), Episode: ip(1), Duration: 100, Category: "tv"})
	v.addFile(bob, "Movies", "m1.mp4", &mediaRow{Title: "M1", Duration: 100, Category: "movie", Genres: []string{"Drama"}})
	v.addFile(bob, "Movies", "m2.mp4", &mediaRow{Title: "M2", Duration: 100, Category: "movie", Genres: []string{"drama", "War"}})

	bc.Do("POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "nope", "shuffle": false, "min_items": 1}).ExpectError(422, "validation_failed")
	var res struct {
		Created []Channel `json:"created"`
	}
	do(t, bc, "POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "per_show", "shuffle": true, "min_items": 2}, 200, &res)
	if len(res.Created) != 2 || res.Created[0].Name != "Alpha" || res.Created[1].Name != "Beta" || !res.Created[0].Shuffle || res.Created[0].Group != "Shows" {
		t.Fatalf("per_show = %+v", res.Created)
	}
	var n nowResp
	do(t, bc, "GET", "/api/v1/tv/channels/"+res.Created[0].ID+"/now", nil, 200, &n)
	if n.Program == nil || n.Program.ShowTitle != "Alpha" {
		t.Errorf("auto channel now = %+v", n.Program)
	}
	do(t, bc, "POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "per_show", "shuffle": false, "min_items": 1}, 200, &res)
	if len(res.Created) != 1 || res.Created[0].Name != "Gamma" {
		t.Errorf("second per_show = %+v", res.Created)
	}
	do(t, bc, "POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "per_genre", "shuffle": false, "min_items": 2}, 200, &res)
	if len(res.Created) != 1 || !strings.EqualFold(res.Created[0].Name, "Drama Movies") {
		t.Errorf("per_genre = %+v", res.Created)
	}
	do(t, bc, "POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "movies", "shuffle": false, "min_items": 3}, 200, &res)
	if len(res.Created) != 0 {
		t.Errorf("movies with min 3 = %+v", res.Created)
	}
	do(t, bc, "POST", "/api/v1/tv/channels/auto-generate", map[string]any{"strategy": "all_tv", "shuffle": true, "min_items": 1}, 200, &res)
	if len(res.Created) != 1 || res.Created[0].Name != "All TV" {
		t.Errorf("all_tv = %+v", res.Created)
	}
	var list []Channel
	do(t, bc, "GET", "/api/v1/tv/channels", nil, 200, &list)
	if len(list) != 5 || list[4].Number != 5 {
		t.Errorf("channels = %d", len(list))
	}

	// Deleting the user removes everything (no FK conflict).
	v.admin.Do("DELETE", "/api/v1/admin/users/"+bob+"?delete_files=true", nil).Expect(204)
	var cnt int
	_ = v.e.Deps.DB.SQL.QueryRow(`SELECT (SELECT count(*) FROM tv_channels) + (SELECT count(*) FROM tv_lineups)`).Scan(&cnt)
	if cnt != 0 {
		t.Errorf("%d rows left after user deletion", cnt)
	}
}
