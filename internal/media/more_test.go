package media_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"ludworks/internal/media"
	"ludworks/internal/media/mediatest"
)

// TestAutoTag parses names and Kodi .nfo sidecars, respecting manual edits unless asked to
// overwrite.
func TestAutoTag(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	ep := v.upload(v.adm, home, "TV/Some Show/Season 1", "Some.Show.S01E01.mkv", mediatest.HEVCMKV(1200))["id"].(string)
	ep2 := v.upload(v.adm, home, "TV/Some Show/Season 1", "Some.Show.S01E02.mkv", mediatest.HEVCMKV(1200))["id"].(string)
	v.upload(v.adm, home, "TV/Some Show/Season 1", "Some.Show.S01E01.nfo", []byte(`<?xml version="1.0"?>
<episodedetails><title>Opening Night</title><season>1</season><episode>1</episode><plot>The first one.</plot></episodedetails>`))
	v.upload(v.adm, home, "TV/Some Show", "tvshow.nfo", []byte(`<tvshow><title>Some Show (Official)</title><plot>A show about things.</plot>
<genre>Comedy</genre><mpaa>TV-14</mpaa><uniqueid type="tmdb">4242</uniqueid></tvshow>`))
	mov := v.upload(v.adm, home, "Movies/Weird Name", "movie.mkv", mediatest.HEVCMKV(7000))["id"].(string)
	v.upload(v.adm, home, "Movies/Weird Name", "movie.nfo", []byte(`<movie><title>Real Title</title><year>1977</year><genre>Sci-Fi</genre>
<genre>Adventure</genre><plot>Long ago.</plot><mpaa>Rated PG</mpaa><uniqueid type="imdb">tt0076759</uniqueid></movie>`))
	for _, id := range []string{ep, ep2, mov} {
		v.waitProbed(v.adm, id)
	}
	// The movie is unknown from its file name alone and stays uncategorised until auto-tag.
	if m := v.media(v.adm, mov); m["category"] != "" {
		t.Fatalf("movie categorised early: %v", m["category"])
	}
	r := v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "TV/Some Show/Season 1"}).Expect(200).Map()
	if r["updated"] != float64(2) || r["skipped"] != float64(0) {
		t.Fatalf("auto-tag tv: %v", r)
	}
	mi := info(v.media(v.adm, ep))
	if mi["show_title"] != "Some Show (Official)" || mi["episode_title"] != "Opening Night" || mi["title"] != "Opening Night" ||
		mi["description"] != "The first one." || mi["content_rating"] != "TV-14" || !slices.Equal(toStrings(mi["genres"]), []string{"Comedy"}) ||
		mi["tmdb_id"] != float64(4242) {
		t.Fatalf("episode after auto-tag: %v", mi)
	}
	show := v.adm.Do("GET", "/api/v1/media/shows/"+mi["show_key"].(string), nil).Expect(200).Map()
	if show["description"] != "A show about things." || show["episodes"] != float64(2) {
		t.Fatalf("show detail: %v", show)
	}
	// Running again changes nothing.
	r = v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "TV/Some Show/Season 1"}).Expect(200).Map()
	if r["updated"] != float64(0) || r["skipped"] != float64(2) {
		t.Fatalf("second run: %v", r)
	}
	// Movies via movie.nfo, from the space root, non-recursive scope excludes subfolders.
	r = v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "", "recursive": false}).Expect(200).Map()
	if r["updated"] != float64(0) || r["skipped"] != float64(0) {
		t.Fatalf("non-recursive root: %v", r)
	}
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "Movies"}).Expect(200)
	m := v.media(v.adm, mov)
	mi = info(m)
	if mi["title"] != "Real Title" || mi["year"] != float64(1977) || mi["imdb_id"] != "tt0076759" || mi["content_rating"] != "PG" ||
		!slices.Equal(toStrings(mi["genres"]), []string{"Sci-Fi", "Adventure"}) || m["category"] != "movie" || m["subcategory"] != "feature" {
		t.Fatalf("movie after auto-tag: %v", m)
	}
	// Manual edits survive a normal run; overwrite replaces them.
	v.adm.Do("PATCH", "/api/v1/media/"+mov, map[string]any{"title": "My Title", "description": ""}).Expect(200)
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "Movies"}).Expect(200)
	mi = info(v.media(v.adm, mov))
	if mi["title"] != "My Title" || mi["description"] != "Long ago." {
		t.Fatalf("gap fill: %v", mi)
	}
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "Movies", "overwrite": true}).Expect(200)
	if mi = info(v.media(v.adm, mov)); mi["title"] != "Real Title" {
		t.Fatalf("overwrite: %v", mi)
	}
	// Errors.
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "Nope"}).ExpectError(404, "not_found")
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": "../x"}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": "nonexistent1", "path": ""}).ExpectError(404, "not_found")
	v.adm.Do("POST", "/api/v1/media/auto-tag", map[string]any{"path": ""}).ExpectError(422, "validation_failed")
	_, bob := v.user("bob")
	bob.Do("POST", "/api/v1/media/auto-tag", map[string]any{"space_id": home, "path": ""}).ExpectError(404, "not_found")
}

// TestTMDB covers search and identify against a fake TMDB.
func TestTMDB(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	var calls atomic.Int32
	poster := pngImage(300, 450)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/img/") && r.URL.Query().Get("api_key") != "0123456789abcdef0123" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/search/movie":
			if r.URL.Query().Get("query") != "night of the" || r.URL.Query().Get("year") != "1968" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"results":[{"id":10331,"title":"Night of the Living Dead","original_title":"Night of the Living Dead","release_date":"1968-10-04","overview":"Zombies.","poster_path":"/abc.jpg"},{"id":2,"title":"Bad","poster_path":"../etc/passwd"}]}`)
		case "/3/search/tv":
			io.WriteString(w, `{"results":[{"id":77,"name":"Fake Show","first_air_date":"2001-01-02","overview":"Show."}]}`)
		case "/3/movie/10331":
			io.WriteString(w, `{"title":"Night of the Living Dead","release_date":"1968-10-04","overview":"Zombies attack.","genres":[{"name":"Horror"}],"poster_path":"/abc.jpg","backdrop_path":"/bd.png","imdb_id":"tt0063350","release_dates":{"results":[{"iso_3166_1":"US","release_dates":[{"certification":"NR"}]}]}}`)
		case "/3/tv/77":
			io.WriteString(w, `{"name":"Fake Show","first_air_date":"2001-01-02","overview":"A fake show.","genres":[{"name":"Drama"}],"poster_path":"/tv.jpg","content_ratings":{"results":[{"iso_3166_1":"US","rating":"TV-PG"}]},"external_ids":{"imdb_id":"tt1234567"}}`)
		case "/3/tv/77/season/1/episode/2":
			io.WriteString(w, `{"name":"The Second","overview":"Episode two.","air_date":"2001-01-09"}`)
		case "/3/movie/404":
			w.WriteHeader(404)
		case "/3/movie/500":
			w.WriteHeader(500)
		case "/img/w780/abc.jpg", "/img/w780/tv.jpg", "/img/w1280/bd.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(poster)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	v.svc.SetTMDBEndpoints(srv.URL+"/3", srv.URL+"/img", nil)
	id := v.upload(v.adm, home, "", "notld.mp4", mediatest.MP4(5000, 640, 480, 0))["id"].(string)
	v.waitProbed(v.adm, id)

	// No key → 501.
	v.adm.Do("GET", "/api/v1/media/tmdb/search?q=x", nil).ExpectError(501, "not_implemented")
	v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 10331, "type": "movie"}).ExpectError(501, "not_implemented")
	v.e.Patch("media", map[string]any{"tmdb_api_key": "0123456789abcdef0123"})

	res := v.adm.Do("GET", "/api/v1/media/tmdb/search?q=night%20of%20the&type=movie&year=1968", nil).Expect(200).Map()
	results := res["results"].([]any)
	first := results[0].(map[string]any)
	if len(results) != 2 || first["tmdb_id"] != float64(10331) || first["year"] != float64(1968) || first["poster_url"] != "https://image.tmdb.org/t/p/w185/abc.jpg" ||
		results[1].(map[string]any)["poster_url"] != nil {
		t.Fatalf("search: %v", res)
	}
	tv := v.adm.Do("GET", "/api/v1/media/tmdb/search?q=fake&type=tv", nil).Expect(200).Map()
	if r := tv["results"].([]any)[0].(map[string]any); r["title"] != "Fake Show" || r["type"] != "tv" || r["year"] != float64(2001) {
		t.Fatalf("tv search: %v", tv)
	}
	v.adm.Do("GET", "/api/v1/media/tmdb/search?q=", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/media/tmdb/search?q=x&type=game", nil).ExpectError(400, "bad_request")

	m := v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 10331, "type": "movie"}).Expect(200).Map()
	mi := info(m)
	if mi["title"] != "Night of the Living Dead" || mi["year"] != float64(1968) || mi["description"] != "Zombies attack." ||
		mi["imdb_id"] != "tt0063350" || mi["content_rating"] != "NR" || mi["tmdb_id"] != float64(10331) ||
		mi["poster_url"] == nil || mi["backdrop_url"] == nil || m["category"] != "movie" {
		t.Fatalf("identify movie: %v", m)
	}
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/poster?size=lg", nil, nil).expect(t, 200)
	v.raw(v.adm, "GET", "/api/v1/media/"+id+"/backdrop?size=sm", nil, nil).expect(t, 200)
	v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 404, "type": "movie"}).ExpectError(404, "not_found")
	if c := v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 500, "type": "movie"}).Expect(502).ErrCode(); c != "tmdb_failed" {
		t.Fatal(c)
	}
	v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 0, "type": "movie"}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/media/"+id+"/identify", map[string]any{"tmdb_id": 1, "type": "book"}).ExpectError(422, "validation_failed")

	// TV identify with apply_to_show propagates the series identity.
	e1 := v.upload(v.adm, home, "Fake/Season 1", "Fake.S01E01.mkv", mediatest.HEVCMKV(100))["id"].(string)
	e2 := v.upload(v.adm, home, "Fake/Season 1", "Fake.S01E02.mkv", mediatest.HEVCMKV(100))["id"].(string)
	v.waitProbed(v.adm, e1)
	v.waitProbed(v.adm, e2)
	m = v.adm.Do("POST", "/api/v1/media/"+e2+"/identify", map[string]any{"tmdb_id": 77, "type": "tv", "apply_to_show": true}).Expect(200).Map()
	mi = info(m)
	if mi["show_title"] != "Fake Show" || mi["episode_title"] != "The Second" || mi["title"] != "The Second" || mi["description"] != "Episode two." ||
		mi["content_rating"] != "TV-PG" || mi["year"] != float64(2001) || mi["poster_url"] == nil {
		t.Fatalf("identify tv: %v", mi)
	}
	o := info(v.media(v.adm, e1))
	if o["show_title"] != "Fake Show" || o["title"] != "Fake Show S01E01" || o["poster_url"] == nil || !slices.Equal(toStrings(o["genres"]), []string{"Drama"}) {
		t.Fatalf("propagated: %v", o)
	}
	show := v.adm.Do("GET", "/api/v1/media/shows/fake-show", nil).Expect(200).Map()
	if show["description"] != "A fake show." || show["poster_url"] == nil {
		t.Fatalf("show after identify: %v", show)
	}
	// A wrong key is reported as an upstream failure.
	v.e.Patch("media", map[string]any{"tmdb_api_key": "ffffffffffffffffffff"})
	if c := v.adm.Do("GET", "/api/v1/media/tmdb/search?q=x", nil).Expect(502).ErrCode(); c != "tmdb_failed" {
		t.Fatal(c)
	}
	_ = calls.Load()
}

// TestReconcileAndRename: the startup pass creates missing rows and renames keep the
// search index current.
func TestReconcileAndRename(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	id := v.upload(v.adm, home, "", "Oldname (2011).mp4", mediatest.MP4(10, 64, 64, 0))["id"].(string)
	v.waitProbed(v.adm, id)
	if _, err := v.e.Deps.DB.SQL.Exec(`DELETE FROM media_items WHERE file_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := v.svc.Reconcile(ctx()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM media_items WHERE file_id = ?`, id).Scan(&n)
	if n != 1 {
		t.Fatal("reconcile did not recreate the row")
	}
	v.adm.Do("PATCH", "/api/v1/files/"+id, map[string]any{"name": "Brandnew (2011).mp4"}).Expect(200)
	eventually(t, "rename indexed", func() bool {
		return len(items(v.adm.Do("GET", "/api/v1/media?q=brandnew", nil).Expect(200).Map())) == 1
	})
	// Probe endpoint re-probes on demand.
	m := v.adm.Do("POST", "/api/v1/media/"+id+"/probe", nil).Expect(200).Map()
	if info(m)["probe_status"] != "ok" {
		t.Fatalf("probe: %v", info(m))
	}
	// Overwriting the content requeues the probe with the new duration.
	v.raw(v.adm, "POST", "/api/v1/files/upload?space_id="+home+"&name="+strings.ReplaceAll("Brandnew (2011).mp4", " ", "%20")+"&overwrite=1",
		map[string]string{"Content-Type": "application/octet-stream"}, mediatest.MP4(99, 64, 64, 0)).expect(t, 201)
	eventually(t, "re-probe after overwrite", func() bool {
		d, _ := info(v.media(v.adm, id))["duration_seconds"].(float64)
		return d == 99
	})
}

// TestHelpers covers the pure helpers.
func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{"The Matrix": "matrix", "A Quiet Place": "quiet place", "An": "an", "Theory": "theory", "  Up ": "up"} {
		if got := media.SortTitleFor(in); got != want {
			t.Errorf("SortTitleFor(%q) = %q, want %q", in, got, want)
		}
	}
	if got := media.CleanText("a\x00b\nc\tD\xff", 100); got != "ab c D" {
		t.Errorf("CleanText = %q", got)
	}
	if got := media.CleanText(strings.Repeat("é", 10), 5); got != "éé" {
		t.Errorf("CleanText truncation = %q", got)
	}
	if got := media.CleanMultiline("one\r\ntwo\x07\n", 100); got != "one\ntwo" {
		t.Errorf("CleanMultiline = %q", got)
	}
	g, msg := media.NormalizeGenres([]string{" Drama ", "drama", "", "Sci  Fi"})
	if msg != "" || !slices.Equal(g, []string{"Drama", "Sci Fi"}) {
		t.Errorf("NormalizeGenres = %v %q", g, msg)
	}
	for in, ok := range map[string]bool{"0.vtt": true, "12.vtt": true, "-1.vtt": false, "01.vtt": false, "x.vtt": false, "1.srt": false, ".vtt": false, "9999999.vtt": false} {
		if _, got := media.TrimVTT(in); got != ok {
			t.Errorf("TrimVTT(%q) = %v", in, got)
		}
	}
	for in, ok := range map[string]bool{"": true, "a/b": true, "/a/b/": true, "a/../b": false, "a//b": false, "./a": false} {
		if _, got := media.CleanDirPath(in); got != ok {
			t.Errorf("CleanDirPath(%q) = %v", in, got)
		}
	}
	for in, ok := range map[string]bool{"posters/abc-123.jpg": true, "backdrops/x.jpg": true, "../posters/x.jpg": false, "posters/../x.jpg": false,
		"cache/x.jpg": false, "posters/x.png": false, "posters/a b.jpg": false} {
		if got := media.ValidStoredImage(in); got != ok {
			t.Errorf("ValidStoredImage(%q) = %v", in, got)
		}
	}
}
