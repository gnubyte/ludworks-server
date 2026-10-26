package tv

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/tv/schedule"
)

// TestFeedTokenCannotReadLibrary: the feed token (embedded in URLs handed to IPTV apps) only
// carries tv:read, so a leaked feed URL cannot list or stream media outside the channels.
func TestFeedTokenCannotReadLibrary(t *testing.T) {
	v := newEnvWith(t, true)
	alice, ac := v.user("alice")
	lid, ids := v.seedShow(alice, ac, "Garden", 2)
	private := v.addFile(alice, "Home videos", "private.mp4", &mediaRow{Title: "Private", Duration: 30})
	var ch Channel
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "Garden", "lineup_id": lid}, 201, &ch)
	var f feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f)
	tok := tokenOf(t, f.M3UURL)

	p, err := auth.VerifyToken(context.Background(), v.e.Deps, tok)
	if err != nil || p == nil || !isFeedScopes(p.Scopes) || p.Has("media:read") {
		t.Fatalf("feed token principal = %+v, %v", p, err)
	}
	feed := v.e.WithBearer(tok)
	feed.Do("GET", "/api/v1/media", nil).ExpectError(403, "forbidden")
	feed.Do("GET", "/api/v1/media/"+private, nil).ExpectError(403, "forbidden")
	for _, asset := range []string{"stream", "transcode", "poster"} {
		if code, _, _ := v.raw(nil, "GET", "/api/v1/media/"+private+"/"+asset+"?access_token="+tok, nil, nil); code != 403 {
			t.Errorf("feed token on /media/{id}/%s = %d, want 403", asset, code)
		}
	}
	// The library itself is fine for a normal session (control).
	ac.Do("GET", "/api/v1/media", nil).Expect(200)

	// Every feed route still works with the narrowed token.
	for _, path := range []string{
		"/api/v1/tv/channels.m3u", "/api/v1/tv/xmltv.xml",
		"/api/v1/tv/channels/" + ch.ID + "/programs/" + ids[0] + "/stream",
		"/api/v1/tv/channels/" + ch.ID + "/programs/" + ids[0] + "/poster",
		"/api/v1/tv/channels/" + ch.ID + "/programs/" + ids[0] + "/subtitles/0.vtt",
	} {
		if code, _, body := v.raw(nil, "GET", path+"?access_token="+tok, nil, nil); code != 200 {
			t.Errorf("%s with the feed token = %d %s", path, code, body)
		}
	}
	if code, _, _ := v.raw(nil, "GET", "/api/v1/tv/channels/"+ch.ID+"/play?access_token="+tok, nil, nil); code != 302 {
		t.Errorf("play with the feed token = %d", code)
	}
	v.e.Patch("tv", map[string]any{"hdhr_enabled": true})
	if code, _, _ := v.raw(nil, "GET", "/api/v1/hdhr/"+tok+"/lineup.json", nil, nil); code != 200 {
		t.Errorf("hdhr lineup with the feed token = %d", code)
	}
}

// TestLegacyFeedTokenNarrowed: feed tokens minted with media:read before migration 0002 are
// narrowed in place (URLs keep working); one that still carries extra scopes is replaced.
func TestLegacyFeedTokenNarrowed(t *testing.T) {
	v := newEnv(t)
	_, ac := v.user("alice")
	var f feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f)
	tok := tokenOf(t, f.M3UURL)
	sqlDB := v.e.Deps.DB.SQL
	widen := func() {
		t.Helper()
		if _, err := sqlDB.Exec(`UPDATE personal_access_tokens SET scopes = '["media:read","tv:read"]'
			WHERE id IN (SELECT token_id FROM tv_feed_tokens)`); err != nil {
			t.Fatal(err)
		}
	}
	scopesOf := func(tok string) []string {
		t.Helper()
		p, err := auth.VerifyToken(context.Background(), v.e.Deps, tok)
		if err != nil || p == nil {
			return nil
		}
		return p.Scopes
	}

	widen() // as minted by an older version
	mig, err := fs.ReadFile(v.tv.Migrations(), "0002_feed_token_scopes.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(string(mig)); err != nil {
		t.Fatal(err)
	}
	if got := scopesOf(tok); !isFeedScopes(got) {
		t.Fatalf("migrated scopes = %v", got)
	}
	var f2 feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f2)
	if f2.M3UURL != f.M3UURL {
		t.Fatal("migration changed the feed URL")
	}

	// A feed token that somehow still carries more than tv:read is not handed out again.
	widen()
	var f3 feedsResp
	do(t, ac, "GET", "/api/v1/tv/feeds", nil, 200, &f3)
	tok3 := tokenOf(t, f3.M3UURL)
	if tok3 == tok || !isFeedScopes(scopesOf(tok3)) || scopesOf(tok) != nil {
		t.Fatalf("over-scoped feed token kept: %v / new %v", scopesOf(tok), scopesOf(tok3))
	}
}

// TestExpansionWorkIsBounded: a lineup cannot make one expansion examine or report an
// unbounded number of files (5 000 items × a folder of unknown-duration files used to keep
// 5 000 × N skipped entries in memory and in the /expanded response).
func TestExpansionWorkIsBounded(t *testing.T) {
	v := newEnv(t)
	alice, ac := v.user("alice")
	space, err := v.files.HomeSpaceID(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		v.addFile(alice, "Junk", fmt.Sprintf("junk %02d.mp3", i), &mediaRow{Title: fmt.Sprintf("Junk %d", i)}) // no duration
	}
	for i := range 20 {
		v.addFile(alice, "Clips", fmt.Sprintf("clip %02d.mp4", i), &mediaRow{Title: fmt.Sprintf("Clip %d", i), Duration: 10})
	}
	items := func(path string, limit int) []any {
		out := make([]any, MaxLineupItems)
		for i := range out {
			it := map[string]any{"kind": "folder", "space_id": space, "path": path}
			if limit > 0 {
				it["limit"] = limit
			}
			out[i] = it
		}
		return out
	}

	// (a) Unknown durations: the skipped list is capped, the count and truncation reported.
	var l lineupResp
	do(t, ac, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Junk", "items": items("Junk", 0)}, 201, &l)
	start := time.Now()
	r := ac.Do("GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil).Expect(200)
	var exp Expanded
	r.JSON(&exp)
	if len(exp.Skipped) != MaxSkippedReported || exp.SkippedCount <= MaxSkippedReported ||
		exp.SkippedCount > MaxExpandWork || !exp.Truncated || len(exp.Items) != 0 {
		t.Fatalf("skipped %d / count %d / truncated %v / items %d", len(exp.Skipped), exp.SkippedCount, exp.Truncated, len(exp.Items))
	}
	if n := len(r.Body); n > 256<<10 {
		t.Fatalf("/expanded is %d bytes", n)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("expansion took %v", el)
	}

	// (b) Playable files, one per item: the scan budget stops the expansion (it used to scan
	// and sort the whole folder for every one of the 5 000 items).
	do(t, ac, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Clips", "items": items("Clips", 1)}, 201, &l)
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &exp)
	if !exp.Truncated || len(exp.Items) == 0 || len(exp.Items) >= MaxLineupItems || exp.SkippedCount != 0 {
		t.Fatalf("items %d / truncated %v / skipped %d", len(exp.Items), exp.Truncated, exp.SkippedCount)
	}

	// (c) A normal lineup reports every skipped file, in order, and is not truncated.
	do(t, ac, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Both", "items": []any{
		map[string]any{"kind": "folder", "space_id": space, "path": "Clips"},
		map[string]any{"kind": "folder", "space_id": space, "path": "Junk"},
		map[string]any{"kind": "folder", "space_id": space, "path": "Clips", "limit": 2},
	}}, 201, &l)
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &exp)
	if exp.Truncated || len(exp.Items) != 22 || len(exp.Skipped) != 20 || exp.SkippedCount != 20 ||
		exp.Items[0].Title != "Clip 0" || exp.Items[20].Title != "Clip 0" || exp.Skipped[0].Title != "Junk 0" {
		t.Fatalf("normal lineup: %d items, %d skipped (%d), truncated %v", len(exp.Items), len(exp.Skipped), exp.SkippedCount, exp.Truncated)
	}
}

// TestExpCacheBoundedByWeight: cached expansions are evicted by total size, not only count.
func TestExpCacheBoundedByWeight(t *testing.T) {
	c := newExpCache()
	big := func() *expansion {
		return &expansion{items: make([]schedule.Item, maxCacheWeight/3), built: time.Now()}
	}
	for i := range 10 {
		id := fmt.Sprint("l", i)
		if _, err := c.get(context.Background(), id, time.Now(), func(context.Context) (*expansion, error) { return big(), nil }); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.weight > maxCacheWeight || len(c.entries) == 0 || len(c.entries) > 3 {
		t.Fatalf("cache holds %d entries of weight %d", len(c.entries), c.weight)
	}
	sum := 0
	for _, e := range c.entries {
		sum += e.exp.weight()
	}
	if sum != c.weight {
		t.Fatalf("weight %d, entries sum to %d", c.weight, sum)
	}
}

// TestOnDemandGuideLargeLineup: the guide of an on-demand channel over a 50 000-item lineup
// of short programs (20 000 slots in 72 h) used to rebuild the schedule engine per slot.
func TestOnDemandGuideLargeLineup(t *testing.T) {
	v := newEnv(t)
	alice, ac := v.user("alice")
	lid, ids := v.seedShow(alice, ac, "Shorts", 2)
	var ch Channel
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "Shorts", "lineup_id": lid,
		"playback_mode": "on_demand", "shuffle": true}, 201, &ch)
	// Swap in a huge expansion of short items (as 5 000 folder items × 10 clips would give).
	exp := &expansion{byID: map[string]*expFile{}, built: v.now()}
	for i := range MaxExpandedFiles {
		f := &expFile{FileID: ids[i%2], Title: "Short", Duration: 10 + float64(i%3)}
		exp.items = append(exp.items, schedule.Item{FileID: f.FileID, Duration: f.Duration})
		exp.ordered = append(exp.ordered, f)
		if exp.byID[f.FileID] == nil {
			exp.byID[f.FileID] = f
		}
	}
	exp.total = schedule.TotalSeconds(exp.items)
	v.tv.cache.mu.Lock()
	v.tv.cache.storeLocked(lid, exp, v.tv.cache.gen)
	v.tv.cache.mu.Unlock()

	start := time.Now()
	var g Guide
	do(t, ac, "GET", "/api/v1/tv/guide?hours=72&channel_ids="+ch.ID, nil, 200, &g)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("on-demand guide took %v", el)
	}
	if len(g.Channels) != 1 || len(g.Channels[0].Programs) != MaxGuidePrograms {
		t.Fatalf("guide: %d channels", len(g.Channels))
	}
	progs := g.Channels[0].Programs
	for i := 1; i < len(progs); i++ {
		if progs[i].StartsAt != progs[i-1].EndsAt {
			t.Fatalf("program %d starts %s after %s", i, progs[i].StartsAt, progs[i-1].EndsAt)
		}
	}
}

// TestProgramSubtitles: programs list their subtitle tracks with channel-scoped URLs that a
// viewer of a shared channel can fetch; embedded tracks only when ffmpeg is available.
func TestProgramSubtitles(t *testing.T) {
	v := newEnv(t)
	alice, ac := v.user("alice")
	_, bc := v.user("bob")
	id := v.addFile(alice, "Movies", "film.mkv", &mediaRow{Title: "Film", Duration: 600})
	subs := `[{"language":"en","label":"English","format":"srt","file_id":"aaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"language":"de","label":"Deutsch","format":"embedded","codec":"subrip","stream":0},
		{"language":"","label":"Commentary","format":"vtt","file_id":"bbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
	if _, err := v.e.Deps.DB.SQL.Exec(`UPDATE media_items SET subtitles = ? WHERE file_id = ?`, subs, id); err != nil {
		t.Fatal(err)
	}
	v.tv.cache.invalidateAll()
	var l lineupResp
	do(t, ac, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Films", "items": []any{map[string]any{"kind": "file", "file_id": id}}}, 201, &l)
	var ch Channel
	do(t, ac, "POST", "/api/v1/tv/channels", map[string]any{"name": "Films", "lineup_id": l.ID, "visibility": "shared"}, 201, &ch)
	base := "/api/v1/tv/channels/" + ch.ID + "/programs/" + id + "/subtitles/"

	var now NowPlaying
	do(t, bc, "GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil, 200, &now)
	want := []ProgramSubtitle{
		{Index: 0, Language: "en", Label: "English", URL: base + "0.vtt"},
		{Index: 2, Language: "", Label: "Commentary", URL: base + "2.vtt"},
	}
	if got, _ := json.Marshal(now.Program.Subtitles); string(got) != mustJSON(want) {
		t.Fatalf("subtitles without ffmpeg = %s", got)
	}
	v.media.mu.Lock()
	v.media.ffmpeg = "/usr/bin/ffmpeg"
	v.media.mu.Unlock()
	v.tv.ffMu.Lock()
	v.tv.ffChecked = time.Time{}
	v.tv.ffMu.Unlock()
	var g Guide
	do(t, bc, "GET", "/api/v1/tv/guide?hours=1&channel_ids="+ch.ID, nil, 200, &g)
	want = []ProgramSubtitle{want[0], {Index: 1, Language: "de", Label: "Deutsch", URL: base + "1.vtt"}, want[1]}
	if got, _ := json.Marshal(g.Channels[0].Programs[0].Subtitles); string(got) != mustJSON(want) {
		t.Fatalf("guide subtitles with ffmpeg = %s", got)
	}
	// The viewer fetches a track through the channel; the media service runs as the owner.
	if code, _, body := v.raw(bc, "GET", base+"1.vtt", nil, nil); code != 200 || !strings.HasPrefix(string(body), "WEBVTT") {
		t.Fatalf("subtitle = %d %s", code, body)
	}
	if last := v.media.last(); last != "subtitle "+alice+" "+id+" 1" {
		t.Fatalf("delegation = %q", last)
	}
	// Programs without subtitles have an empty list, not null.
	plain := v.addFile(alice, "Movies", "plain.mp4", &mediaRow{Title: "Plain", Duration: 600})
	do(t, ac, "PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": []any{map[string]any{"kind": "file", "file_id": plain}}}, 200, nil)
	r := bc.Do("GET", "/api/v1/tv/channels/"+ch.ID+"/now", nil).Expect(200)
	if !strings.Contains(string(r.Body), `"subtitles":[]`) {
		t.Fatalf("plain program: %s", r.Body)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
