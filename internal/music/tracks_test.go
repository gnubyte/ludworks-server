package music_test

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mt "ludworks/internal/music/musictest"
)

// TestIngestAndTags uploads every supported format and checks the tags, technical values,
// identity and art that GET /music/tracks/{id} reports.
func TestIngestAndTags(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	cover := mt.JPEG(64, 64, 10)

	mp3 := mt.MP3(mt.ID3v2(mt.ID3Options{Version: 4},
		mt.Text("TIT2", mt.UTF8, "Ünïcode Song"), mt.Text("TPE1", mt.UTF8, "Artist A", "Artist B"),
		mt.Text("TALB", mt.UTF8, "The Album"), mt.Text("TPE2", mt.UTF8, "The Band"), mt.Text("TRCK", mt.Latin1, "3/12"),
		mt.Text("TPOS", mt.Latin1, "1/2"), mt.Text("TDRC", mt.Latin1, "1999"), mt.Text("TCON", mt.Latin1, "Rock;Pop"),
		mt.Text("TCOM", mt.UTF8, "Composer"), mt.TXXX("REPLAYGAIN_TRACK_GAIN", "-6.5 dB"), mt.TXXX("REPLAYGAIN_TRACK_PEAK", "0.9"),
		mt.COMM(mt.UTF8, "eng", "", "nice"), mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 2, nil)
	id := v.upload(v.adm, home, "Music", "song.mp3", mp3)
	tr := v.waitProbed(v.adm, id)
	if tr["probe_status"] != "ok" || tr["title"] != "Ünïcode Song" || tr["artist"] != "Artist A; Artist B" {
		t.Fatalf("mp3 track: %v", tr)
	}
	if fmt.Sprint(tr["artists"]) != "[Artist A Artist B]" || tr["album"] != "The Album" || tr["album_artist"] != "The Band" ||
		num(tr["track_number"]) != 3 || num(tr["track_total"]) != 12 || num(tr["disc_number"]) != 1 || num(tr["disc_total"]) != 2 ||
		num(tr["year"]) != 1999 || fmt.Sprint(tr["genres"]) != "[Rock Pop]" || tr["composer"] != "Composer" || tr["comment"] != "nice" {
		t.Fatalf("mp3 values: %v", tr)
	}
	if tr["codec"] != "mp3" || tr["container"] != "mp3" || num(tr["bitrate_kbps"]) < 120 || num(tr["bitrate_kbps"]) > 136 ||
		num(tr["sample_rate"]) != 44100 || num(tr["channels"]) != 2 || num(tr["duration_seconds"]) < 1.9 {
		t.Fatalf("mp3 technical: %v", tr)
	}
	rg := tr["replaygain"].(map[string]any)
	if num(rg["track_gain_db"]) != -6.5 || num(rg["track_peak"]) != 0.9 || rg["album_gain_db"] != nil {
		t.Fatalf("replaygain %v", rg)
	}
	if tr["has_art"] != true || !strings.HasPrefix(tr["art_url"].(string), "/api/v1/music/tracks/"+id+"/art?v=") ||
		len(tr["album_id"].(string)) != 26 || len(tr["artist_id"].(string)) != 26 || tr["mime"] != "audio/mpeg" ||
		tr["favorite"] != false || tr["rating"] != nil || num(tr["play_count"]) != 0 || tr["has_lyrics"] != false {
		t.Fatalf("mp3 misc: %v", tr)
	}

	flac := mt.FLAC(mt.FLACOptions{Rate: 48000, Bits: 24, Seconds: 30, Comments: []string{"TITLE=Flac One", "ARTIST=Solo",
		"ALBUM=Lossless", "TRACKNUMBER=1", "COMPILATION=1", "GENRE=Jazz"}})
	fid := v.upload(v.adm, home, "Music", "one.flac", flac)
	ft := v.waitProbed(v.adm, fid)
	if ft["codec"] != "flac" || num(ft["bit_depth"]) != 24 || num(ft["sample_rate"]) != 48000 || num(ft["duration_seconds"]) != 30 ||
		ft["album_artist"] != "Various Artists" || ft["compilation"] != true || ft["has_art"] != false || ft["art_url"] != nil {
		t.Fatalf("flac: %v", ft)
	}

	m4a := mt.M4A(mt.M4AOptions{Seconds: 61, Items: []mt.Item{mt.TextItem("\xa9nam", "M4A Song"), mt.TextItem("\xa9ART", "M4A Artist"),
		mt.TextItem("\xa9alb", "M4A Album"), mt.PairItem("trkn", 2, 9)}})
	mid := v.upload(v.adm, home, "Music", "song.m4a", m4a)
	m := v.waitProbed(v.adm, mid)
	if m["codec"] != "aac" || m["container"] != "m4a" || m["title"] != "M4A Song" || num(m["track_number"]) != 2 ||
		m["album_artist"] != "M4A Artist" || num(m["duration_seconds"]) != 61 {
		t.Fatalf("m4a: %v", m)
	}

	ogg := mt.Ogg(mt.OggOptions{Fields: []string{"TITLE=Ogg Song", "ARTIST=Ogg Artist"}, Seconds: 10})
	oid := v.upload(v.adm, home, "Music", "song.ogg", ogg)
	o := v.waitProbed(v.adm, oid)
	if o["codec"] != "vorbis" || o["title"] != "Ogg Song" || num(o["duration_seconds"]) < 9.9 {
		t.Fatalf("ogg: %v", o)
	}

	wav := mt.WAV(1, []string{"INAM", "Wav Song", "IART", "Wav Artist"}, nil)
	wid := v.upload(v.adm, home, "Music", "song.wav", wav)
	w := v.waitProbed(v.adm, wid)
	if w["codec"] != "pcm" || w["title"] != "Wav Song" || num(w["bit_depth"]) != 16 || num(w["bitrate_kbps"]) != 1411 {
		t.Fatalf("wav: %v", w)
	}

	aiff := mt.AIFF(1, "", "", mt.ID3v2(mt.ID3Options{Version: 3}, mt.Text("TIT2", mt.Latin1, "Aiff Song")))
	aid := v.upload(v.adm, home, "Music", "song.aiff", aiff)
	a := v.waitProbed(v.adm, aid)
	if a["container"] != "aiff" || a["title"] != "Aiff Song" || num(a["duration_seconds"]) != 1 || num(a["sample_rate"]) != 44100 {
		t.Fatalf("aiff: %v", a)
	}

	// Unknown audio (MIDI): a music item whose read fails; values come from the path.
	midi := v.upload(v.adm, home, "Artist X/Album Y", "07 - Some Tune.mid", []byte("MThd\x00\x00\x00\x06\x00\x00\x00\x01\x00\x60"))
	mi := v.waitProbed(v.adm, midi)
	if mi["probe_status"] != "failed" || mi["title"] != "Some Tune" || mi["artist"] != "Artist X" || mi["album"] != "Album Y" ||
		num(mi["track_number"]) != 7 || mi["duration_seconds"] != nil {
		t.Fatalf("midi: %v", mi)
	}
	// A file that is MP3 only by its name.
	junk := v.upload(v.adm, home, "", "junk.mp3", bytes.Repeat([]byte("not audio "), 100))
	if j := v.waitProbed(v.adm, junk); j["probe_status"] != "failed" || j["title"] != "junk" || j["codec"] != "" {
		t.Fatalf("junk: %v", j)
	}
}

// TestPathFallbacksAndIdentity: values missing from the tags come from the path; album and
// artist ids are case-, whitespace- and diacritic-insensitive.
func TestPathFallbacksAndIdentity(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	a := v.upload(v.adm, home, "Björk/Homogenic (1997)/CD1", "01 - Hunter.mp3", mt.MP3(nil, 1, nil))
	b := v.upload(v.adm, home, "Bjork - Homogenic/CD 2", "1-02 Joga.mp3", mt.MP3(nil, 1, nil))
	c := v.upload(v.adm, home, "Loose", "Artist Z - Lonely.mp3", mt.MP3(nil, 1, nil))
	ta, tb, tc := v.waitProbed(v.adm, a), v.waitProbed(v.adm, b), v.waitProbed(v.adm, c)
	if ta["title"] != "Hunter" || ta["artist"] != "Björk" || ta["album"] != "Homogenic" || num(ta["year"]) != 1997 ||
		num(ta["disc_number"]) != 1 || num(ta["track_number"]) != 1 {
		t.Fatalf("a: %v", ta)
	}
	if tb["title"] != "Joga" || tb["artist"] != "Bjork" || tb["album"] != "Homogenic" || num(tb["disc_number"]) != 1 && num(tb["disc_number"]) != 2 ||
		num(tb["track_number"]) != 2 {
		t.Fatalf("b: %v", tb)
	}
	if ta["album_id"] != tb["album_id"] || ta["artist_id"] != tb["artist_id"] {
		t.Fatalf("diacritics/case should not split the album: %v %v", ta["album_id"], tb["album_id"])
	}
	if tc["artist"] != "Artist Z" || tc["title"] != "Lonely" || tc["album"] != "Loose" {
		t.Fatalf("c: %v", tc)
	}
	// Tags win over the path; tracks without an album have no album id.
	d := v.upload(v.adm, home, "Some/Where", "x.mp3", taggedMP3(1, "TIT2", "Tagged", "TPE1", "Real Artist"))
	td := v.waitProbed(v.adm, d)
	if td["title"] != "Tagged" || td["artist"] != "Real Artist" || td["album"] != "Where" {
		t.Fatalf("d: %v", td)
	}
	e := v.upload(v.adm, home, "", "single.mp3", taggedMP3(1, "TIT2", "Single"))
	te := v.waitProbed(v.adm, e)
	if te["album"] != "" || te["album_id"] != "" || te["artist_id"] != "" {
		t.Fatalf("e: %v", te)
	}
}

// TestEligibilityAndEvents: only audio with category ""/music is music; moves, renames, trash
// and restore keep the library in step.
func TestEligibilityAndEvents(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	id := v.upload(v.adm, home, "A/B", "01 - First.mp3", mt.MP3(nil, 1, nil))
	v.waitProbed(v.adm, id)
	game := v.uploadQ(v.adm, home, "Games", "theme.mp3", mt.MP3(nil, 1, nil), url.Values{"category": {"game"}, "subcategory": {"soundtrack"}})
	doc := v.upload(v.adm, home, "", "notes.txt", []byte("hello"))
	video := v.upload(v.adm, home, "", "clip.mp4", mt.M4A(mt.M4AOptions{Seconds: 1}))
	v.adm.Do("GET", "/api/v1/music/tracks/"+game, nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/music/tracks/"+doc, nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/music/tracks/"+video, nil).ExpectError(404, "not_found")
	page := v.adm.Do("GET", "/api/v1/music/tracks", nil).Expect(200).Map()
	if got := field(items(page), "id"); !eq(got, []string{id}) {
		t.Fatalf("listing %v", got)
	}
	// Recategorising into music adds it (files are never recategorised by music itself).
	cat := "music"
	sub := "soundtrack"
	v.adm.Do("PATCH", "/api/v1/files/"+game, map[string]any{"category": cat, "subcategory": sub}).Expect(200)
	eventually(t, "recategorised track", func() bool { return v.adm.Do("GET", "/api/v1/music/tracks/"+game, nil).Status == 200 })
	if tr := v.track(v.adm, game); tr["subcategory"] != "soundtrack" {
		t.Fatalf("subcategory %v", tr["subcategory"])
	}
	v.adm.Do("PATCH", "/api/v1/files/"+game, map[string]any{"category": "game"}).Expect(200)
	eventually(t, "recategorised away", func() bool { return v.adm.Do("GET", "/api/v1/music/tracks/"+game, nil).Status == 404 })

	// Rename: path-based values follow.
	v.adm.Do("PATCH", "/api/v1/files/"+id, map[string]any{"name": "02 - Renamed.mp3"}).Expect(200)
	eventually(t, "rename", func() bool { tr := v.track(v.adm, id); return tr["title"] == "Renamed" && num(tr["track_number"]) == 2 })
	// Move the folder: the album follows the new path (recursive event).
	var dirs map[string]any
	dirs = v.adm.Do("GET", "/api/v1/files?path=A", nil).Expect(200).Map()
	var bID string
	for _, it := range items(dirs) {
		if it["name"] == "B" {
			bID = it["id"].(string)
		}
	}
	v.mkdirAll(v.adm, home, "Other Artist")
	v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{bID}, "dest_space_id": home, "dest_path": "Other Artist"}).Expect(200)
	eventually(t, "move", func() bool { tr := v.track(v.adm, id); return tr["artist"] == "Other Artist" && tr["album"] == "B" })
	// Trash hides it, restore brings it back with its values.
	v.adm.Do("PATCH", "/api/v1/music/tracks/"+id, map[string]any{"title": "Manual Title"}).Expect(200)
	v.adm.Do("DELETE", "/api/v1/files/"+id, nil).Expect(204)
	v.adm.Do("GET", "/api/v1/music/tracks/"+id, nil).ExpectError(404, "not_found")
	if n := len(items(v.adm.Do("GET", "/api/v1/music/tracks", nil).Expect(200).Map())); n != 0 {
		t.Fatalf("trashed track listed (%d)", n)
	}
	var trash map[string]any
	trash = v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()
	v.adm.Do("POST", "/api/v1/trash/"+items(trash)[0]["id"].(string)+"/restore", nil).Expect(200)
	eventually(t, "restore", func() bool { return v.adm.Do("GET", "/api/v1/music/tracks/"+id, nil).Status == 200 })
	if tr := v.track(v.adm, id); tr["title"] != "Manual Title" {
		t.Fatalf("manual edit lost over trash/restore: %v", tr["title"])
	}
	// Content replaced: re-read.
	q := url.Values{"space_id": {home}, "path": {"Other Artist/B"}, "name": {"02 - Renamed.mp3"}, "overwrite": {"1"}}
	v.raw(v.adm, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": "application/octet-stream"},
		taggedMP3(1, "TALB", "Replaced Album")).expect(t, 201)
	eventually(t, "re-read", func() bool { return v.track(v.adm, id)["album"] == "Replaced Album" })

	// Reconcile creates rows for files without (e.g. ingested while the module was not running).
	if err := v.svc.Reconcile(ctx()); err != nil {
		t.Fatal(err)
	}
}

// TestPermissions: unreadable or non-music files are 404 everywhere; read-only spaces are 403
// for edits; tokens need the music scopes and cannot use music routes to read other files.
func TestPermissions(t *testing.T) {
	v := newEnv(t)
	bobID, bob := v.user("bob")
	_, carol := v.user("carol")
	lib := v.library("Shared", map[string]any{"user_id": bobID, "access": "read"})
	adminHome := v.home(v.adm)
	private := v.upload(v.adm, adminHome, "", "private.mp3", taggedMP3(1, "TIT2", "Private"))
	shared := v.upload(v.adm, lib, "", "shared.mp3", taggedMP3(1, "TIT2", "Shared"))
	v.waitProbed(v.adm, private)
	v.waitProbed(v.adm, shared)
	text := v.upload(v.adm, lib, "", "readme.txt", []byte("not audio"))

	for _, path := range []string{"", "/stream", "/download", "/art", "/lyrics", "/transcode"} {
		bob.Do("GET", "/api/v1/music/tracks/"+private+path, nil).ExpectError(404, "not_found")
		bob.Do("GET", "/api/v1/music/tracks/"+text+path, nil).ExpectError(404, "not_found")
	}
	bob.Do("PUT", "/api/v1/music/tracks/"+private+"/rating", map[string]any{"rating": 3}).ExpectError(404, "not_found")
	bob.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "track", "id": private, "favorite": true}).ExpectError(404, "not_found")
	// Readable but not writable: 403 for edits, fine for per-user state.
	v.track(bob, shared)
	bob.Do("PATCH", "/api/v1/music/tracks/"+shared, map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	bob.Do("POST", "/api/v1/music/tracks/"+shared+"/probe", nil).ExpectError(403, "forbidden")
	bob.Do("PUT", "/api/v1/music/tracks/"+shared+"/rating", map[string]any{"rating": 4}).Expect(204)
	if tr := v.track(bob, shared); num(tr["rating"]) != 4 {
		t.Fatalf("bob's rating %v", tr["rating"])
	}
	if tr := v.track(v.adm, shared); tr["rating"] != nil {
		t.Fatal("ratings are per user")
	}
	// Carol is not a member.
	carol.Do("GET", "/api/v1/music/tracks/"+shared, nil).ExpectError(404, "not_found")
	if n := len(items(carol.Do("GET", "/api/v1/music/tracks", nil).Expect(200).Map())); n != 0 {
		t.Fatalf("carol sees %d tracks", n)
	}
	// Listings: bob sees the library, not the admin's home; the admin sees their home and the
	// library but never other homes implicitly.
	got := field(items(bob.Do("GET", "/api/v1/music/tracks", nil).Expect(200).Map()), "id")
	if !eq(got, []string{shared}) {
		t.Fatalf("bob's listing %v", got)
	}
	bobTrack := v.upload(bob, v.home(bob), "", "bob.mp3", taggedMP3(1, "TIT2", "Bob"))
	v.waitProbed(bob, bobTrack)
	if got := field(items(v.adm.Do("GET", "/api/v1/music/tracks?sort=title", nil).Expect(200).Map()), "title"); !eq(got, []string{"Private", "Shared"}) {
		t.Fatalf("admin listing %v", got)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/tracks?space_id="+v.home(bob), nil).Expect(200).Map()), "title"); !eq(got, []string{"Bob"}) {
		t.Fatalf("admin explicit space %v", got)
	}
	bob.Do("GET", "/api/v1/music/tracks?space_id="+adminHome, nil).ExpectError(404, "not_found")

	// Scopes: a music:read token reads music but cannot write or read other files.
	readTok := v.token(bob, "music:read")
	readTok.Do("GET", "/api/v1/music/tracks/"+shared, nil).Expect(200)
	readTok.Do("GET", "/api/v1/files/"+shared, nil).ExpectError(403, "forbidden")
	readTok.Do("PATCH", "/api/v1/music/tracks/"+shared, map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	readTok.Do("GET", "/api/v1/music/tracks/"+text+"/stream", nil).ExpectError(404, "not_found")
	filesTok := v.token(bob, "files:read")
	filesTok.Do("GET", "/api/v1/music/tracks", nil).ExpectError(403, "forbidden")
	// ?access_token= works on streams and art only.
	r := v.raw(nil, "GET", "/api/v1/music/tracks/"+shared+"/stream?access_token="+readTok.Bearer, nil, nil)
	r.expect(t, 200)
	if r.Header.Get("Content-Type") != "audio/mpeg" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("stream headers %v", r.Header)
	}
	v.raw(nil, "GET", "/api/v1/music/tracks/"+shared+"?access_token="+readTok.Bearer, nil, nil).expect(t, 401)
}

// TestListingFiltersSortsAndPaging covers GET /music/tracks.
func TestListingFiltersSortsAndPaging(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	type spec struct {
		title, artist, album, genre, year string
		track                             int
	}
	specs := []spec{
		{"Alpha", "The Zebras", "Album One", "Rock", "2001", 2},
		{"beta", "Ant", "Album One", "Rock", "2001", 1},
		{"Gamma", "Ant", "Album Two", "Jazz", "1990", 1},
		{"Delta", "Émile", "Album Two", "Jazz;Blues", "", 2},
		{"Epsilon", "Bee", "", "Pop", "2010", 0},
	}
	ids := map[string]string{}
	for i, s := range specs {
		frames := []string{"TIT2", s.title, "TPE1", s.artist, "TCON", s.genre}
		if s.album != "" {
			frames = append(frames, "TALB", s.album)
		}
		if s.year != "" {
			frames = append(frames, "TDRC", s.year)
		}
		if s.track > 0 {
			frames = append(frames, "TRCK", fmt.Sprint(s.track))
		}
		ids[s.title] = v.upload(v.adm, home, "", fmt.Sprintf("t%d.mp3", i), taggedMP3(float64(10+i), frames...))
	}
	for _, id := range ids {
		v.waitProbed(v.adm, id)
	}
	get := func(q string) []string {
		t.Helper()
		return field(items(v.adm.Do("GET", "/api/v1/music/tracks?"+q, nil).Expect(200).Map()), "title")
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"sort=title", []string{"Alpha", "beta", "Delta", "Epsilon", "Gamma"}},
		{"sort=title&order=desc", []string{"Gamma", "Epsilon", "Delta", "beta", "Alpha"}},
		{"sort=artist", []string{"beta", "Gamma", "Epsilon", "Delta", "Alpha"}}, // Ant(One,Two), Bee, Émile, (The) Zebras
		{"sort=album", []string{"Epsilon", "beta", "Alpha", "Gamma", "Delta"}},
		{"sort=year", []string{"Gamma", "beta", "Alpha", "Epsilon", "Delta"}},
		{"sort=duration&order=desc", []string{"Epsilon", "Delta", "Gamma", "beta", "Alpha"}},
		{"genre=jazz&sort=title", []string{"Delta", "Gamma"}},
		{"genre=BLUES", []string{"Delta"}},
		{"year=2001&sort=title", []string{"Alpha", "beta"}},
		{"q=emile", []string{"Delta"}},
		{"q=albu&sort=title", []string{"Alpha", "beta", "Delta", "Gamma"}},
		{"q=%21%21", nil},
	} {
		if got := get(tc.q); !eq(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.q, got, tc.want)
		}
	}
	tr := v.track(v.adm, ids["Gamma"])
	// Same album title, different album artists: different albums.
	if got := get("album_id=" + tr["album_id"].(string) + "&sort=title"); !eq(got, []string{"Gamma"}) {
		t.Errorf("album_id filter %v", got)
	}
	if got := get("artist_id=" + tr["artist_id"].(string) + "&sort=title"); !eq(got, []string{"beta", "Gamma"}) {
		t.Errorf("artist_id filter %v", got)
	}
	// Paging with cursors, for keyset and random sorts.
	for _, sort := range []string{"title", "added", "random&seed=abc", "plays", "last_played"} {
		var all []string
		cursor := ""
		for range 10 {
			q := "limit=2&sort=" + sort
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			page := v.adm.Do("GET", "/api/v1/music/tracks?"+q, nil).Expect(200).Map()
			all = append(all, field(items(page), "id")...)
			if page["next_cursor"] == nil {
				break
			}
			cursor = page["next_cursor"].(string)
		}
		if len(all) != 5 || len(uniqStrings(all)) != 5 {
			t.Errorf("paging %s: %v", sort, all)
		}
	}
	if a, b := get("sort=random&seed=x"), get("sort=random&seed=x"); !eq(a, b) {
		t.Error("random with a seed is not stable")
	}
	v.adm.Do("GET", "/api/v1/music/tracks?sort=bogus", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/music/tracks?cursor=garbage", nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/music/tracks?favorite=maybe", nil).ExpectError(400, "bad_request")

	// Lookup: readable tracks and missing ids.
	r := v.adm.Do("POST", "/api/v1/music/tracks/lookup", map[string]any{"ids": []string{ids["Alpha"], "aaaaaaaaaaaaaaaaaaaaaaaaaa", ids["Gamma"]}}).Expect(200).Map()
	if len(list(r["tracks"])) != 2 || fmt.Sprint(r["missing"]) != "[aaaaaaaaaaaaaaaaaaaaaaaaaa]" {
		t.Fatalf("lookup %v", r)
	}
	big := make([]string, 501)
	for i := range big {
		big[i] = fmt.Sprintf("id%024d", i)
	}
	v.adm.Do("POST", "/api/v1/music/tracks/lookup", map[string]any{"ids": big}).ExpectError(422, "validation_failed")
}

func uniqStrings(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// TestEditsBulkAndProbe: manual values survive re-reads, ?reset=1 drops them, bulk reports
// per-item errors, and the manual-probe limit applies.
func TestEditsBulkAndProbe(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	id := v.upload(v.adm, home, "", "a.mp3", taggedMP3(1, "TIT2", "Tag Title", "TPE1", "Tag Artist", "TALB", "Tag Album", "TDRC", "2000"))
	v.waitProbed(v.adm, id)
	tr := v.adm.Do("PATCH", "/api/v1/music/tracks/"+id, map[string]any{"title": " New Title ", "year": nil, "genres": []string{"A", "a", "B;C"},
		"artists": []string{"X", "Y"}, "compilation": true, "subcategory": "album", "play_count": 99}).Expect(200).Map()
	if tr["title"] != "New Title" || tr["year"] != nil || fmt.Sprint(tr["genres"]) != "[A B C]" || fmt.Sprint(tr["artists"]) != "[X Y]" ||
		tr["artist"] != "Tag Artist" || tr["album_artist"] != "Various Artists" || tr["subcategory"] != "album" || num(tr["play_count"]) != 0 {
		t.Fatalf("patched %v", tr)
	}
	// The file was categorised as music/album by the subcategory edit.
	if f := v.adm.Do("GET", "/api/v1/files/"+id, nil).Expect(200).Map(); f["category"] != "music" || f["subcategory"] != "album" {
		t.Fatalf("file %v", f)
	}
	v.adm.Do("POST", "/api/v1/music/tracks/"+id+"/probe", nil).Expect(200)
	if tr := v.track(v.adm, id); tr["title"] != "New Title" || tr["year"] != nil || tr["album"] != "Tag Album" {
		t.Fatalf("manual values lost by a re-read: %v", tr)
	}
	tr = v.adm.Do("POST", "/api/v1/music/tracks/"+id+"/probe?reset=1", nil).Expect(200).Map()
	if tr["title"] != "Tag Title" || num(tr["year"]) != 2000 || tr["album_artist"] != "Tag Artist" {
		t.Fatalf("reset %v", tr)
	}
	for body, field := range map[string]string{
		`{"title":123}`: "title", `{"year":0}`: "year", `{"track_number":1.5}`: "track_number",
		`{"genres":"x"}`: "genres", `{"compilation":null}`: "compilation", `{"title":"a\nb"}`: "title",
	} {
		r := v.raw(v.adm, "PATCH", "/api/v1/music/tracks/"+id, map[string]string{"Content-Type": "application/json"}, []byte(body))
		if r.Status != 422 || !strings.Contains(string(r.Body), `"`+field+`"`) {
			t.Errorf("%s: %d %s", body, r.Status, r.Body)
		}
	}
	v.adm.Do("PATCH", "/api/v1/music/tracks/"+id, map[string]any{"subcategory": "nope"}).ExpectError(422, "validation_failed")
	// Bulk.
	other := v.upload(v.adm, home, "", "b.mp3", taggedMP3(1, "TIT2", "B"))
	v.waitProbed(v.adm, other)
	_, bob := v.user("bob")
	bobs := v.upload(bob, v.home(bob), "", "c.mp3", taggedMP3(1))
	r := v.adm.Do("POST", "/api/v1/music/tracks/bulk", map[string]any{"file_ids": []string{id, other, "zzzzzzzzzzzzzzzzzzzzzzzzzz"},
		"set": map[string]any{"album": "Bulk Album", "album_artist": "Bulk AA"}}).Expect(200).Map()
	if num(r["updated"]) != 2 || len(list(r["errors"])) != 1 {
		t.Fatalf("bulk %v", r)
	}
	if a, b := v.track(v.adm, id), v.track(v.adm, other); a["album_id"] != b["album_id"] || a["album"] != "Bulk Album" {
		t.Fatalf("bulk album %v %v", a["album_id"], b["album_id"])
	}
	r = bob.Do("POST", "/api/v1/music/tracks/bulk", map[string]any{"file_ids": []string{bobs, id}, "set": map[string]any{"title": "x"}}).Expect(200).Map()
	if num(r["updated"]) != 1 || list(r["errors"])[0]["code"] != "not_found" {
		t.Fatalf("bob bulk %v", r)
	}
	v.adm.Do("POST", "/api/v1/music/tracks/bulk", map[string]any{"file_ids": []string{id}, "set": map[string]any{"year": "x"}}).
		ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/tracks/bulk", map[string]any{"file_ids": []string{id}, "set": map[string]any{}}).
		ExpectError(422, "validation_failed")
	// One manual probe per user at a time.
	me := v.principalOf(v.adm)
	release, ok := v.svc.HoldProbe(me.UserID)
	if !ok {
		t.Fatal("hold probe")
	}
	pr := v.raw(v.adm, "POST", "/api/v1/music/tracks/"+id+"/probe", nil, nil)
	if pr.Status != 429 || pr.Header.Get("Retry-After") == "" {
		t.Fatalf("probe limit: %d %v", pr.Status, pr.Header)
	}
	release()
	// Rescan queues re-reads (and validates its input).
	v.adm.Do("POST", "/api/v1/music/rescan", map[string]any{}).Expect(202)
	v.adm.Do("POST", "/api/v1/music/rescan", map[string]any{"space_id": home, "path": "../x"}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/rescan", map[string]any{"space_id": home, "path": "missing"}).ExpectError(404, "not_found")
	r2 := v.adm.Do("POST", "/api/v1/music/rescan", map[string]any{"space_id": home}).Expect(202).Map()
	if r2["job"] != "music.rescan" {
		t.Fatalf("rescan %v", r2)
	}
}

// TestLyrics: embedded unsynced lyrics, an .lrc sidecar winning over them, synced embedded
// lyrics winning over an unsynced .lrc.
func TestLyrics(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	plain := v.upload(v.adm, home, "L", "plain.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.USLT(mt.UTF8, "eng", "one\ntwo")), 100, nil))
	tr := v.waitProbed(v.adm, plain)
	if tr["has_lyrics"] != true {
		t.Fatal("has_lyrics")
	}
	l := v.adm.Do("GET", "/api/v1/music/tracks/"+plain+"/lyrics", nil).Expect(200).Map()
	if l["synced"] != false || l["source"] != "embedded" || l["text"] != "one\ntwo" || list(l["lines"])[0]["time_seconds"] != nil {
		t.Fatalf("embedded %v", l)
	}
	v.upload(v.adm, home, "L", "plain.lrc", []byte("[ar:x]\n[00:01.50]first\n[00:03.00]second\n"))
	eventually(t, "lrc", func() bool {
		return v.adm.Do("GET", "/api/v1/music/tracks/"+plain+"/lyrics", nil).Map()["source"] == "lrc"
	})
	l = v.adm.Do("GET", "/api/v1/music/tracks/"+plain+"/lyrics", nil).Expect(200).Map()
	if l["synced"] != true || num(list(l["lines"])[0]["time_seconds"]) != 1.5 || l["text"] != "first\nsecond" {
		t.Fatalf("lrc %v", l)
	}
	synced := v.upload(v.adm, home, "L", "synced.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{},
		mt.SYLT(mt.UTF8, mt.SyncLine{MS: 2000, Text: "embedded synced"})), 100, nil))
	v.waitProbed(v.adm, synced)
	v.upload(v.adm, home, "L", "SYNCED.LRC", []byte("just text"))
	time.Sleep(200 * time.Millisecond)
	l = v.adm.Do("GET", "/api/v1/music/tracks/"+synced+"/lyrics", nil).Expect(200).Map()
	if l["source"] != "embedded" || l["synced"] != true {
		t.Fatalf("synced embedded should win over an unsynced lrc: %v", l)
	}
	none := v.upload(v.adm, home, "", "none.mp3", taggedMP3(1))
	v.waitProbed(v.adm, none)
	v.adm.Do("GET", "/api/v1/music/tracks/"+none+"/lyrics", nil).ExpectError(404, "not_found")
}

// TestArt: embedded art, folder art (and the setting), sizes, ETags, decoding once per picture.
func TestArt(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	cover := mt.JPEG(1500, 1000, 30)
	a := v.upload(v.adm, home, "Art/Album", "01.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.Text("TALB", mt.UTF8, "Arty"),
		mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 100, nil))
	b := v.upload(v.adm, home, "Art/Album", "02.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.Text("TALB", mt.UTF8, "Arty"),
		mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 100, nil))
	ta, tb := v.waitProbed(v.adm, a), v.waitProbed(v.adm, b)
	if ta["art_url"] == nil || ta["art_url"] != strings.Replace(tb["art_url"].(string), b, a, 1) {
		t.Fatalf("same picture, same version: %v %v", ta["art_url"], tb["art_url"])
	}
	for size, edge := range map[string]int{"sm": 160, "md": 320, "lg": 640, "xl": 1200} {
		r := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+a+"/art?size="+size, nil, nil).expect(t, 200)
		w, h, err := decodeSize(r.Body)
		if err != nil || max(w, h) != edge || r.Header.Get("Content-Type") != "image/jpeg" || r.Header.Get("ETag") == "" ||
			!strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("size %s: %dx%d %v %v", size, w, h, err, r.Header)
		}
		r2 := v.raw(v.adm, "GET", "/api/v1/music/tracks/"+a+"/art?size="+size, map[string]string{"If-None-Match": r.Header.Get("ETag")}, nil)
		if r2.Status != 304 {
			t.Fatalf("etag revalidation %d", r2.Status)
		}
	}
	v.adm.Do("GET", "/api/v1/music/tracks/"+a+"/art?size=huge", nil).ExpectError(422, "validation_failed")
	// Stored once.
	entries, _ := os.ReadDir(filepath.Join(v.svc.Dir(), "art"))
	jpgs := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			jpgs++
		}
	}
	if jpgs != 1 {
		t.Fatalf("%d stored pictures for one distinct cover", jpgs)
	}

	// Folder art.
	c := v.upload(v.adm, home, "Folder/Album", "01.mp3", taggedMP3(1, "TALB", "Foldered"))
	v.upload(v.adm, home, "Folder/Album", "back.png", mt.PNG(20, 20))
	v.waitProbed(v.adm, c)
	eventually(t, "only image becomes folder art", func() bool { return v.track(v.adm, c)["has_art"] == true })
	first := v.track(v.adm, c)["art_url"]
	v.upload(v.adm, home, "Folder/Album", "Cover.JPG", mt.JPEG(40, 40, 99))
	eventually(t, "cover.jpg wins", func() bool { u := v.track(v.adm, c)["art_url"]; return u != nil && u != first })
	v.e.Patch("music", map[string]any{"folder_art": false})
	if tr := v.track(v.adm, c); tr["has_art"] != false || tr["art_url"] != nil {
		t.Fatalf("folder_art off: %v", tr["art_url"])
	}
	v.adm.Do("GET", "/api/v1/music/tracks/"+c+"/art", nil).ExpectError(404, "not_found")
	v.e.Patch("music", map[string]any{"folder_art": true})
	if v.track(v.adm, c)["has_art"] != true {
		t.Fatal("folder_art back on")
	}
	// Multi-disc albums: Album/cover.jpg is the cover of Album/CD1/… until CD1 has its own;
	// other subfolders do not borrow it.
	m := v.upload(v.adm, home, "Multi/Album/CD1", "01.mp3", taggedMP3(1, "TALB", "Multi"))
	n := v.upload(v.adm, home, "Multi/Album/Extras", "01.mp3", taggedMP3(2, "TALB", "Multi extras"))
	v.waitProbed(v.adm, m)
	v.waitProbed(v.adm, n)
	if v.track(v.adm, m)["has_art"] != false {
		t.Fatal("disc track has art before there is a cover")
	}
	v.upload(v.adm, home, "Multi/Album", "cover.jpg", mt.JPEG(30, 30, 7))
	eventually(t, "the album folder's cover reaches the disc folder", func() bool { return v.track(v.adm, m)["has_art"] == true })
	albumCover := v.track(v.adm, m)["art_url"]
	if v.track(v.adm, n)["has_art"] != false {
		t.Fatal("a folder that is not a disc folder borrowed the album cover")
	}
	v.upload(v.adm, home, "Multi/Album/CD1", "folder.png", mt.PNG(24, 24))
	eventually(t, "the disc folder's own cover wins", func() bool { u := v.track(v.adm, m)["art_url"]; return u != nil && u != albumCover })
	// Embedded art wins over folder art.
	d := v.upload(v.adm, home, "Folder/Album", "02.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 1, nil))
	if v.waitProbed(v.adm, d)["art_url"] != strings.Replace(ta["art_url"].(string), a, d, 1) {
		t.Fatal("embedded should win over folder art")
	}
	// Undecodable embedded pictures are ignored.
	e := v.upload(v.adm, home, "", "bad.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.APIC(mt.Latin1, "image/jpeg", 3, "", []byte("not a jpeg"))), 1, nil))
	if v.waitProbed(v.adm, e)["has_art"] != false {
		t.Fatal("bad picture shown as art")
	}
	if err := v.svc.GC(ctx()); err != nil {
		t.Fatal(err)
	}
	v.raw(v.adm, "GET", "/api/v1/music/tracks/"+a+"/art", nil, nil).expect(t, 200)
}

func decodeSize(b []byte) (int, int, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	return cfg.Width, cfg.Height, err
}
