package media_test

import (
	"bytes"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"ludworks/internal/media/mediatest"
)

// TestIngestProbeAndGet: uploads become media items with metadata guessed from the path,
// are categorised and probed, and render as Media objects.
func TestIngestProbeAndGet(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	mov := v.upload(v.adm, home, "Movies/Night of the Living Dead (1968)", "Night of the Living Dead (1968).mp4", mediatest.MP4(5760.5, 1440, 1080, 4096))
	id := mov["id"].(string)
	m := v.waitProbed(v.adm, id)
	mi := info(m)
	if mi["title"] != "Night of the Living Dead" || mi["year"] != float64(1968) || mi["sort_title"] != "night of the living dead" {
		t.Fatalf("guessed metadata: %v", mi)
	}
	if m["category"] != "movie" || m["subcategory"] != "feature" {
		t.Fatalf("not auto-categorised: %v/%v", m["category"], m["subcategory"])
	}
	if mi["probe_status"] != "ok" || mi["duration_seconds"] != 5760.5 || mi["width"] != float64(1440) || mi["height"] != float64(1080) ||
		mi["video_codec"] != "h264" || mi["audio_codec"] != "aac" || mi["container"] != "mp4" || mi["browser_playable"] != true {
		t.Fatalf("probe result: %v", mi)
	}
	if mi["poster_url"] != nil || m["has_thumbnail"] != false || mi["watched"] != false || mi["position_seconds"] != float64(0) {
		t.Fatalf("defaults: %v", m)
	}
	if m["name"] != "Night of the Living Dead (1968).mp4" || m["path"] != "Movies/Night of the Living Dead (1968)/Night of the Living Dead (1968).mp4" {
		t.Fatalf("entry fields: %v", m)
	}
	// A non-media file is not a media item.
	doc := v.upload(v.adm, home, "", "notes.txt", []byte("hello"))
	v.adm.Do("GET", "/api/v1/media/"+doc["id"].(string), nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/media/notavalid", nil).ExpectError(404, "not_found")
	// Phone videos stay uncategorised but are media items and listed by default.
	clip := v.upload(v.adm, home, "Camera", "VID_20240101_123456.mp4", mediatest.MP4(12, 1920, 1080, 0))
	cm := v.waitProbed(v.adm, clip["id"].(string))
	if cm["category"] != "" || info(cm)["title"] != "VID 20240101 123456" {
		t.Fatalf("clip: %v", cm)
	}
	page := v.adm.Do("GET", "/api/v1/media", nil).Expect(200).Map()
	if got := titles(page); !slices.Equal(got, []string{"Night of the Living Dead", "VID 20240101 123456"}) {
		t.Fatalf("default listing: %v", got)
	}
	page = v.adm.Do("GET", "/api/v1/media?category=none", nil).Expect(200).Map()
	if got := titles(page); !slices.Equal(got, []string{"VID 20240101 123456"}) {
		t.Fatalf("category=none: %v", got)
	}
}

// TestShowsSubtitlesAndSidecars covers grouping by show, the show page and subtitles.
func TestShowsSubtitlesAndSidecars(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	dir := "Shows/Test Show/Season 1"
	e1 := v.upload(v.adm, home, dir, "Test.Show.S01E01.Pilot.mkv", mediatest.HEVCMKV(1500))
	e2 := v.upload(v.adm, home, dir, "Test.Show.S01E02.mkv", mediatest.HEVCMKV(1400))
	v.upload(v.adm, home, "Shows/Test Show/Season 2", "Test.Show.S02E01.mkv", mediatest.MP4(1300, 1280, 720, 0))
	v.upload(v.adm, home, dir, "Test.Show.S01E01.Pilot.en.srt", []byte(mediatest.SRT))
	id1 := e1["id"].(string)
	v.waitProbed(v.adm, id1)
	v.waitProbed(v.adm, e2["id"].(string))
	var m map[string]any
	eventually(t, "sidecar", func() bool {
		m = v.media(v.adm, id1)
		return len(info(m)["subtitles"].([]any)) == 2
	})
	mi := info(m)
	if mi["show_title"] != "Test Show" || mi["show_key"] != "test-show" || mi["season"] != float64(1) || mi["episode"] != float64(1) ||
		mi["episode_title"] != "Pilot" || mi["title"] != "Pilot" || mi["browser_playable"] != false || mi["video_codec"] != "hevc" {
		t.Fatalf("episode: %v", mi)
	}
	subs := mi["subtitles"].([]any)
	s0, s1 := subs[0].(map[string]any), subs[1].(map[string]any)
	if s0["format"] != "srt" || s0["language"] != "en" || s0["index"] != float64(0) || s1["format"] != "embedded" || s1["index"] != float64(1) {
		t.Fatalf("subtitles: %v", subs)
	}
	r := v.raw(v.adm, "GET", "/api/v1/media/"+id1+"/subtitles/0.vtt", nil, nil).expect(t, 200)
	if !strings.HasPrefix(string(r.Body), "WEBVTT") || !strings.Contains(string(r.Body), "00:00:01.000 --> 00:00:04.000") ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "text/vtt") {
		t.Fatalf("vtt: %q %v", r.Body, r.Header)
	}
	if c := v.raw(v.adm, "GET", "/api/v1/media/"+id1+"/subtitles/1.vtt", nil, nil).expect(t, 501).code(); c != "ffmpeg_unavailable" {
		t.Fatalf("embedded without ffmpeg: %s", c)
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id1+"/subtitles/2.vtt", nil, nil).expect(t, 404)
	v.raw(v.adm, "GET", "/api/v1/media/"+id1+"/subtitles/x.vtt", nil, nil).expect(t, 404)
	v.raw(v.adm, "GET", "/api/v1/media/"+id1+"/subtitles/0.srt", nil, nil).expect(t, 404)

	shows := v.adm.Do("GET", "/api/v1/media/shows", nil).Expect(200).Map()
	list := items(shows)
	if len(list) != 1 || list[0]["show_key"] != "test-show" || list[0]["seasons"] != float64(2) || list[0]["episodes"] != float64(3) {
		t.Fatalf("shows: %v", shows)
	}
	show := v.adm.Do("GET", "/api/v1/media/shows/test-show", nil).Expect(200).Map()
	seasons := show["seasons"].([]any)
	if show["show_title"] != "Test Show" || len(seasons) != 2 {
		t.Fatalf("show: %v", show)
	}
	s1eps := seasons[0].(map[string]any)["episodes"].([]any)
	if len(s1eps) != 2 || info(s1eps[0].(map[string]any))["episode"] != float64(1) {
		t.Fatalf("season 1: %v", s1eps)
	}
	v.adm.Do("GET", "/api/v1/media/shows/nope", nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/media/shows/BAD%20KEY", nil).ExpectError(404, "not_found")
	// ?show= accepts a title or a key; sort=episode orders the episodes.
	page := v.adm.Do("GET", "/api/v1/media?show=Test%20Show&sort=episode", nil).Expect(200).Map()
	if got := titles(page); !slices.Equal(got, []string{"Pilot", "Test Show S01E02", "Test Show S02E01"}) {
		t.Fatalf("episodes: %v", got)
	}
	page = v.adm.Do("GET", "/api/v1/media?show=test-show&season=2", nil).Expect(200).Map()
	if len(items(page)) != 1 {
		t.Fatalf("season filter: %v", titles(page))
	}
	// Moving the sidecar away drops it; renaming the video keeps matching rules.
	srt := v.adm.Do("GET", "/api/v1/files/search?q=Pilot.en", nil).Expect(200).Map()
	srtID := items(srt)[0]["id"].(string)
	v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{srtID}, "dest_space_id": home, "dest_path": "Shows"}).Expect(200)
	eventually(t, "sidecar removal", func() bool { return len(info(v.media(v.adm, id1))["subtitles"].([]any)) == 1 })
	v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{srtID}, "dest_space_id": home, "dest_path": dir}).Expect(200)
	eventually(t, "sidecar back", func() bool { return len(info(v.media(v.adm, id1))["subtitles"].([]any)) == 2 })
	// Trashing an episode hides it; restoring brings it back with its metadata.
	v.adm.Do("DELETE", "/api/v1/files/"+e2["id"].(string), nil).Expect(204)
	if n := len(items(v.adm.Do("GET", "/api/v1/media?show=test-show", nil).Expect(200).Map())); n != 2 {
		t.Fatalf("after trash: %d", n)
	}
	v.adm.Do("GET", "/api/v1/media/"+e2["id"].(string), nil).ExpectError(404, "not_found")
	v.adm.Do("POST", "/api/v1/trash/"+e2["id"].(string)+"/restore", nil).Expect(200)
	if n := len(items(v.adm.Do("GET", "/api/v1/media?show=test-show", nil).Expect(200).Map())); n != 3 {
		t.Fatalf("after restore: %d", n)
	}
	// Permanent deletion cascades.
	v.adm.Do("DELETE", "/api/v1/files/"+e2["id"].(string), nil).Expect(204)
	v.adm.Do("DELETE", "/api/v1/trash/"+e2["id"].(string), nil).Expect(204)
	var n int
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM media_items WHERE file_id = ?`, e2["id"]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("row not deleted: %d %v", n, err)
	}
}

// TestListingFiltersSortsAndPaging covers filters, sort keys, keyset paging and random.
func TestListingFiltersSortsAndPaging(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	type mv struct {
		name string
		dur  float64
	}
	movies := []mv{{"Alpha (2001).mp4", 100}, {"Bravo (1999).mp4", 300}, {"Charlie (2010).mp4", 200}, {"Delta (2005).mp4", 50}, {"The Echo (2003).mp4", 400}}
	ids := map[string]string{}
	for _, x := range movies {
		ids[x.name] = v.upload(v.adm, home, "Movies", x.name, mediatest.MP4(x.dur, 640, 360, 0))["id"].(string)
	}
	for _, id := range ids {
		v.waitProbed(v.adm, id)
	}
	v.adm.Do("PATCH", "/api/v1/media/"+ids["Bravo (1999).mp4"], map[string]any{"genres": []string{"Drama", " comedy "}}).Expect(200)
	v.adm.Do("PATCH", "/api/v1/media/"+ids["Delta (2005).mp4"], map[string]any{"genres": []string{"drama"}}).Expect(200)
	v.adm.Do("PATCH", "/api/v1/files/"+ids["Alpha (2001).mp4"], map[string]any{"tags": []string{"favorite"}}).Expect(200)

	get := func(q string) []string {
		return titles(v.adm.Do("GET", "/api/v1/media?"+q, nil).Expect(200).Map())
	}
	check := func(q string, want ...string) {
		t.Helper()
		if got := get(q); !slices.Equal(got, want) {
			t.Fatalf("%s: got %v want %v", q, got, want)
		}
	}
	check("sort=title", "Alpha", "Bravo", "Charlie", "Delta", "The Echo")
	check("sort=title&order=desc", "The Echo", "Delta", "Charlie", "Bravo", "Alpha")
	check("sort=year", "Bravo", "Alpha", "The Echo", "Delta", "Charlie")
	check("sort=duration&order=desc", "The Echo", "Bravo", "Charlie", "Alpha", "Delta")
	check("genre=DRAMA", "Bravo", "Delta")
	check("genre=comedy", "Bravo")
	check("year=2010", "Charlie")
	check("tag=favorite", "Alpha")
	check("q=ech", "The Echo")
	check("q=2010", "Charlie") // file name is indexed
	check("category=tv")
	check("subcategory=feature&sort=title&limit=2", "Alpha", "Bravo")

	// Keyset paging returns every item exactly once, in order.
	for _, sortBy := range []string{"title", "year", "duration", "added"} {
		for _, order := range []string{"asc", "desc"} {
			var all []string
			cursor := ""
			for i := 0; i < 10; i++ {
				q := url.Values{"sort": {sortBy}, "order": {order}, "limit": {"2"}}
				if cursor != "" {
					q.Set("cursor", cursor)
				}
				page := v.adm.Do("GET", "/api/v1/media?"+q.Encode(), nil).Expect(200).Map()
				all = append(all, titles(page)...)
				nc, _ := page["next_cursor"].(string)
				if nc == "" {
					break
				}
				cursor = nc
			}
			full := get("sort=" + sortBy + "&order=" + order + "&limit=50")
			if !slices.Equal(all, full) || len(all) != 5 {
				t.Fatalf("paging %s %s: %v vs %v", sortBy, order, all, full)
			}
		}
	}
	// A cursor is bound to its query.
	page := v.adm.Do("GET", "/api/v1/media?sort=title&limit=2", nil).Expect(200).Map()
	cur := page["next_cursor"].(string)
	v.adm.Do("GET", "/api/v1/media?sort=year&limit=2&cursor="+url.QueryEscape(cur), nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?sort=title&genre=drama&limit=2&cursor="+url.QueryEscape(cur), nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?sort=bogus", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?order=up", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?year=abc", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?watched=maybe", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media?q=%22%29%20OR%20*", nil).Expect(200) // hostile FTS input is quoted

	// Random: stable for a seed, complete across pages.
	a := get("sort=random&seed=abc")
	b := get("sort=random&seed=abc")
	if !slices.Equal(a, b) || len(a) != 5 {
		t.Fatalf("random not stable: %v %v", a, b)
	}
	var paged []string
	cursor := ""
	for {
		q := "sort=random&seed=abc&limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		p := v.adm.Do("GET", "/api/v1/media?"+q, nil).Expect(200).Map()
		paged = append(paged, titles(p)...)
		if nc, _ := p["next_cursor"].(string); nc != "" {
			cursor = nc
		} else {
			break
		}
	}
	if !slices.Equal(paged, a) {
		t.Fatalf("random paging %v vs %v", paged, a)
	}

	// Watched filter and progress sort.
	v.adm.Do("PUT", "/api/v1/media/"+ids["Charlie (2010).mp4"]+"/progress", map[string]any{"position_seconds": 190}).Expect(204)
	v.adm.Do("PUT", "/api/v1/media/"+ids["Alpha (2001).mp4"]+"/progress", map[string]any{"position_seconds": 10}).Expect(204)
	check("watched=true", "Charlie")
	check("watched=false&sort=title", "Alpha", "Bravo", "Delta", "The Echo")
	check("in_progress=1&sort=progress&order=desc", "Alpha")

	// Facets.
	f := v.adm.Do("GET", "/api/v1/media/facets", nil).Expect(200).Map()
	genres := f["genres"].([]any)
	if len(genres) != 2 || genres[0].(map[string]any)["count"] != float64(2) || genres[0].(map[string]any)["value"] != "Drama" {
		t.Fatalf("genre facets: %v", genres)
	}
	if years := f["years"].([]any); len(years) != 5 || years[0].(map[string]any)["value"] != float64(2010) {
		t.Fatalf("year facets: %v", years)
	}
	if tags := f["tags"].([]any); len(tags) != 1 {
		t.Fatalf("tag facets: %v", tags)
	}
	if subs := f["subcategories"].([]any); len(subs) != 1 || subs[0].(map[string]any)["value"] != "feature" {
		t.Fatalf("subcategory facets: %v", subs)
	}
}

// TestPatchBulkAndValidation covers editing, validation and bulk updates.
func TestPatchBulkAndValidation(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	a := v.upload(v.adm, home, "", "clip one.mp4", mediatest.MP4(60, 640, 360, 0))["id"].(string)
	b := v.upload(v.adm, home, "", "clip two.mp4", mediatest.MP4(60, 640, 360, 0))["id"].(string)
	m := v.adm.Do("PATCH", "/api/v1/media/"+a, map[string]any{
		"title": "  The Great Film ", "year": 1999, "genres": []string{"Drama", "drama", "Sci-Fi"}, "description": "Line one\r\nLine two",
		"content_rating": "PG-13", "imdb_id": "tt0063350", "tmdb_id": 42, "duration_seconds": 1, "category": "movie", "subcategory": "short",
	}).Expect(200).Map()
	mi := info(m)
	if mi["title"] != "The Great Film" || mi["sort_title"] != "great film" || mi["year"] != float64(1999) ||
		!slices.Equal(toStrings(mi["genres"]), []string{"Drama", "Sci-Fi"}) || mi["description"] != "Line one\nLine two" ||
		mi["tmdb_id"] != float64(42) || m["category"] != "movie" || m["subcategory"] != "short" || mi["duration_seconds"] == float64(1) {
		t.Fatalf("patched: %v", m)
	}
	// Explicit sort title is kept; nulls clear.
	mi = info(v.adm.Do("PATCH", "/api/v1/media/"+a, map[string]any{"sort_title": "Film, The Great", "year": nil, "tmdb_id": nil}).Expect(200).Map())
	if mi["sort_title"] != "Film, The Great" || mi["year"] != nil || mi["tmdb_id"] != nil {
		t.Fatalf("sort title / nulls: %v", mi)
	}
	for body, field := range map[string]string{
		`{"title":""}`: "title", `{"year":1700}`: "year", `{"year":"x"}`: "year", `{"season":-1}`: "season",
		`{"imdb_id":"nm123"}`: "imdb_id", `{"genres":"x"}`: "genres", `{"title":"a\u0007b"}`: "title",
		`{"episode":1.5}`: "episode", `{"genres":["` + strings.Repeat("x", 65) + `"]}`: "genres",
	} {
		r := v.raw(v.adm, "PATCH", "/api/v1/media/"+a, map[string]string{"Content-Type": "application/json"}, []byte(body))
		r.expect(t, 422)
		if !strings.Contains(string(r.Body), `"`+field+`"`) {
			t.Fatalf("%s: %s", body, r.Body)
		}
	}
	v.adm.Do("PATCH", "/api/v1/media/"+a, map[string]any{"category": "nope"}).ExpectError(422, "validation_failed")
	// Show title drives the show key.
	mi = info(v.adm.Do("PATCH", "/api/v1/media/"+b, map[string]any{"show_title": "Café Society!", "season": 2, "episode": 5}).Expect(200).Map())
	if mi["show_key"] != "cafe-society" || mi["season"] != float64(2) {
		t.Fatalf("show key: %v", mi)
	}
	// Watched via PATCH.
	if !info(v.adm.Do("PATCH", "/api/v1/media/"+b, map[string]any{"watched": true}).Expect(200).Map())["watched"].(bool) {
		t.Fatal("watched not set")
	}
	// Bulk: two items plus one the caller cannot see.
	uid, user := v.user("bob")
	_ = uid
	foreign := v.upload(user, v.home(user), "", "theirs.mp4", mediatest.MP4(10, 64, 64, 0))["id"].(string)
	r := v.adm.Do("POST", "/api/v1/media/bulk", map[string]any{"file_ids": []string{a, b, a}, "set": map[string]any{"genres": []string{"Horror"}, "category": "tv", "subcategory": "episode"}}).Expect(200).Map()
	if r["updated"] != float64(2) {
		t.Fatalf("bulk: %v", r)
	}
	r = user.Do("POST", "/api/v1/media/bulk", map[string]any{"file_ids": []string{a, foreign}, "set": map[string]any{"year": 2001}}).Expect(200).Map()
	if r["updated"] != float64(1) || len(r["errors"].([]any)) != 1 {
		t.Fatalf("bulk with foreign id: %v", r)
	}
	for _, id := range []string{a, b} {
		m := v.media(v.adm, id)
		if m["category"] != "tv" || !slices.Equal(toStrings(info(m)["genres"]), []string{"Horror"}) {
			t.Fatalf("bulk result: %v", m)
		}
	}
	v.adm.Do("POST", "/api/v1/media/bulk", map[string]any{"file_ids": []string{}, "set": map[string]any{"year": 2000}}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/media/bulk", map[string]any{"file_ids": []string{a}, "set": map[string]any{}}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/media/bulk", map[string]any{"file_ids": []string{a}, "set": map[string]any{"year": 1}}).ExpectError(422, "validation_failed")
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// TestPermissions: other users' items are 404 everywhere, read-only members get 403 on
// writes, and scoped tokens are enforced.
func TestPermissions(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	mine := v.upload(v.adm, home, "Movies", "Secret (2000).mp4", mediatest.MP4(30, 64, 64, 0))["id"].(string)
	v.waitProbed(v.adm, mine)
	bobID, bob := v.user("bob")
	for _, p := range []string{"", "/stream", "/poster", "/transcode", "/backdrop", "/subtitles/0.vtt"} {
		v.raw(bob, "GET", "/api/v1/media/"+mine+p, nil, nil).expect(t, 404)
	}
	bob.Do("PATCH", "/api/v1/media/"+mine, map[string]any{"title": "x"}).ExpectError(404, "not_found")
	bob.Do("PUT", "/api/v1/media/"+mine+"/progress", map[string]any{"position_seconds": 1}).ExpectError(404, "not_found")
	if n := len(items(bob.Do("GET", "/api/v1/media", nil).Expect(200).Map())); n != 0 {
		t.Fatalf("bob sees %d items", n)
	}
	bob.Do("GET", "/api/v1/media?space_id="+home, nil).ExpectError(404, "not_found")
	// Admins do not see other users' homes implicitly, but can open them by id.
	bobHome := v.home(bob)
	bobs := v.upload(bob, bobHome, "", "Bob Movie (2001).mp4", mediatest.MP4(30, 64, 64, 0))["id"].(string)
	v.waitProbed(bob, bobs)
	if got := titles(v.adm.Do("GET", "/api/v1/media", nil).Expect(200).Map()); slices.Contains(got, "Bob Movie") {
		t.Fatalf("admin sees other homes implicitly: %v", got)
	}
	if got := titles(v.adm.Do("GET", "/api/v1/media?space_id="+bobHome, nil).Expect(200).Map()); !slices.Equal(got, []string{"Bob Movie"}) {
		t.Fatalf("admin explicit space: %v", got)
	}
	// Library with a read-only member.
	lib := v.admin.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "Films", "members": []map[string]any{{"user_id": bobID, "access": "read"}}}).Expect(201).Map()
	libID := lib["id"].(string)
	shared := v.upload(v.adm, libID, "", "Shared (1990).mp4", mediatest.MP4(30, 64, 64, 0))["id"].(string)
	v.waitProbed(v.adm, shared)
	bob.Do("GET", "/api/v1/media/"+shared, nil).Expect(200)
	v.raw(bob, "GET", "/api/v1/media/"+shared+"/stream", nil, nil).expect(t, 200)
	bob.Do("PATCH", "/api/v1/media/"+shared, map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	v.raw(bob, "PUT", "/api/v1/media/"+shared+"/poster", map[string]string{"Content-Type": "image/png"}, pngImage(20, 30)).expect(t, 403)
	bob.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": libID, "path": ""}).ExpectError(403, "forbidden")
	bob.Do("PUT", "/api/v1/media/"+shared+"/progress", map[string]any{"position_seconds": 3}).Expect(204) // per-user, read is enough
	bob.Do("PATCH", "/api/v1/media/"+shared, map[string]any{"watched": true}).Expect(200)                 // watched is per user
	if info(v.media(v.adm, shared))["watched"] != false || info(v.media(bob, shared))["watched"] != true {
		t.Fatal("watched flag must be per user")
	}
	if got := titles(bob.Do("GET", "/api/v1/media", nil).Expect(200).Map()); !slices.Equal(got, []string{"Bob Movie", "Shared"}) {
		t.Fatalf("bob listing: %v", got)
	}
	// Scopes: a files-only token cannot use media; a media:read token cannot write nor read
	// non-media files through media endpoints.
	filesTok := v.token(bob, "files:read")
	filesTok.Do("GET", "/api/v1/media", nil).ExpectError(403, "forbidden")
	readTok := v.token(bob, "media:read")
	readTok.Do("GET", "/api/v1/media/"+shared, nil).Expect(200)
	readTok.Do("PATCH", "/api/v1/media/"+bobs, map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	doc := v.upload(bob, bobHome, "", "private.txt", []byte("secret"))["id"].(string)
	v.raw(readTok, "GET", "/api/v1/media/"+doc+"/stream", nil, nil).expect(t, 404)
	// Query tokens work on [qt] routes only.
	v.raw(nil, "GET", "/api/v1/media/"+shared+"/stream?access_token="+url.QueryEscape(readTok.Bearer), nil, nil).expect(t, 200)
	v.raw(nil, "GET", "/api/v1/media/"+shared+"?access_token="+url.QueryEscape(readTok.Bearer), nil, nil).expect(t, 401)
	v.raw(nil, "GET", "/api/v1/media", nil, nil).expect(t, 401)
	// Cookie writes need CSRF.
	saved := v.admin.CSRF
	v.admin.CSRF = ""
	v.admin.Do("PATCH", "/api/v1/media/"+mine, map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	v.admin.CSRF = saved
	v.admin.Do("PATCH", "/api/v1/media/"+mine, map[string]any{"title": "Secret Edited"}).Expect(200)
}

// TestPostersStreamAndProgress covers posters, Range streaming, transcode without ffmpeg
// and progress.
func TestPostersStreamAndProgress(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	content := mediatest.MP4(1000, 640, 360, 10000)
	id := v.upload(v.adm, home, "", "Feature (2015).mp4", content)["id"].(string)
	v.waitProbed(v.adm, id)

	// Posters.
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster", nil, nil).expect(t, 404)
	m := v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/png"}, pngImage(400, 600)).expect(t, 200).json(t)
	if pu, _ := info(m)["poster_url"].(string); !strings.HasPrefix(pu, "/api/v1/media/"+id+"/poster?v=") {
		t.Fatalf("poster url: %v", info(m))
	}
	for size, edge := range map[string]int{"sm": 160, "md": 320, "lg": 640} {
		r := v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster?size="+size, nil, nil).expect(t, 200)
		img, err := jpeg.Decode(bytes.NewReader(r.Body))
		if err != nil || r.Header.Get("Content-Type") != "image/jpeg" || !strings.HasPrefix(r.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("poster %s: %v %v", size, err, r.Header)
		}
		want := min(edge, 600)
		if b := img.Bounds(); b.Dy() != want {
			t.Fatalf("poster %s: %v", size, b)
		}
		etag := r.Header.Get("ETag")
		v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster?size="+size, map[string]string{"If-None-Match": etag}, nil).expect(t, 304)
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster?size=xl", nil, nil).expect(t, 422)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 64)...)
	if c := v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/webp"}, webp).expect(t, 415).code(); c != "unsupported_media_type" {
		t.Fatal(c)
	}
	v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/png"}, []byte("not an image")).expect(t, 415)
	v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/png"}, make([]byte, 21<<20)).expect(t, 413)
	v.raw(v.adm, "DELETE", "/api/v1/media/"+id+"/poster", nil, nil).expect(t, 204)
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster", nil, nil).expect(t, 404)
	if info(v.media(v.adm, id))["poster_url"] != nil {
		t.Fatal("poster_url after delete")
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/backdrop", nil, nil).expect(t, 404)
	v.raw(v.adm, "DELETE", "/api/v1/media/"+id+"/backdrop", nil, nil).expect(t, 204)
	if err := v.svc.GC(ctx()); err != nil {
		t.Fatal(err)
	}

	// Stream with Range.
	r := v.raw(v.adm, "GET", "/api/v1/media/"+id+"/stream", map[string]string{"Range": "bytes=0-99"}, nil).expect(t, 206)
	if len(r.Body) != 100 || !bytes.Equal(r.Body, content[:100]) || r.Header.Get("Content-Range") != "bytes 0-99/"+itoa(len(content)) {
		t.Fatalf("range: %d %v", len(r.Body), r.Header)
	}
	if cd := r.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("disposition %q", cd)
	}
	r = v.raw(v.adm, "GET", "/api/v1/media/"+id+"/stream", nil, nil).expect(t, 200)
	if !bytes.Equal(r.Body, content) {
		t.Fatal("full stream differs")
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/stream", map[string]string{"Range": "bytes=999999999-"}, nil).expect(t, 416)
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/bogus", nil, nil).expect(t, 404)

	// Transcode without ffmpeg.
	if c := v.raw(v.adm, "GET", "/api/v1/media/"+id+"/transcode?start=10&height=720", nil, nil).expect(t, 501).code(); c != "ffmpeg_unavailable" {
		t.Fatal(c)
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/transcode?height=721", nil, nil).expect(t, 400)
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/transcode?start=-1", nil, nil).expect(t, 400)

	// Progress.
	put := func(body map[string]any, status int) {
		t.Helper()
		v.adm.Do("PUT", "/api/v1/media/"+id+"/progress", body).Expect(status)
	}
	state := func() (bool, float64) {
		mi := info(v.media(v.adm, id))
		return mi["watched"].(bool), mi["position_seconds"].(float64)
	}
	put(map[string]any{"position_seconds": 100}, 204)
	if w, p := state(); w || p != 100 {
		t.Fatalf("progress: %v %v", w, p)
	}
	put(map[string]any{"position_seconds": 950}, 204)
	if w, p := state(); !w || p != 950 {
		t.Fatalf("90%% rule: %v %v", w, p)
	}
	put(map[string]any{"position_seconds": 50}, 204) // rewatching keeps the watched flag
	if w, _ := state(); !w {
		t.Fatal("watched flag lost")
	}
	put(map[string]any{"position_seconds": 50, "completed": false}, 204)
	if w, p := state(); w || p != 50 {
		t.Fatalf("unwatch: %v %v", w, p)
	}
	put(map[string]any{"position_seconds": 500, "completed": true}, 204)
	if w, p := state(); !w || p != 0 {
		t.Fatalf("completed: %v %v", w, p)
	}
	put(map[string]any{"position_seconds": -1}, 422)
	put(map[string]any{}, 422)
	put(map[string]any{"position_seconds": "x"}, 400)
	m = v.media(v.adm, id)
	if info(m)["last_watched_at"] == nil {
		t.Fatal("last_watched_at missing")
	}
}

// TestServiceContract exercises the methods the tv module calls with the owner principal.
func TestServiceContract(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	content := mediatest.MP4(100, 320, 240, 500)
	id := v.upload(v.adm, home, "", "Contract (2020).mp4", content)["id"].(string)
	v.waitProbed(v.adm, id)
	v.upload(v.adm, home, "", "Contract (2020).srt", []byte(mediatest.SRT))
	owner := v.principalOf(v.adm)
	_, bob := v.user("bob")
	stranger := v.principalOf(bob)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Range", "bytes=10-19")
	if err := v.svc.ServeStream(rec, req, owner, id); err != nil || rec.Code != 206 || !bytes.Equal(rec.Body.Bytes(), content[10:20]) {
		t.Fatalf("ServeStream: %v %d", err, rec.Code)
	}
	if err := v.svc.ServeStream(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), stranger, id); err == nil {
		t.Fatal("stranger could stream")
	}
	if err := v.svc.ServeTranscode(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), owner, id, 0, 720); err == nil || !strings.Contains(err.Error(), "ffmpeg_unavailable") {
		t.Fatalf("ServeTranscode: %v", err)
	}
	if err := v.svc.ServePoster(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), owner, id, "md"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("ServePoster without poster: %v", err)
	}
	eventually(t, "sidecar", func() bool { return len(info(v.media(v.adm, id))["subtitles"].([]any)) == 1 })
	rec = httptest.NewRecorder()
	if err := v.svc.ServeSubtitle(rec, httptest.NewRequest("GET", "/x", nil), owner, id, 0); err != nil || !strings.HasPrefix(rec.Body.String(), "WEBVTT") {
		t.Fatalf("ServeSubtitle: %v %q", err, rec.Body.String())
	}
	if err := v.svc.ServeSubtitle(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), stranger, id, 0); err == nil {
		t.Fatal("stranger got subtitles")
	}
	if v.svc.FFmpegPath(ctx()) != "" {
		t.Fatal("ffmpeg unexpectedly available")
	}
	_ = http.StatusOK
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(n)) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
