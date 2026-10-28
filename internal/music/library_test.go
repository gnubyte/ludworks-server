package music_test

import (
	"fmt"
	"testing"

	mt "ludworks/internal/music/musictest"
)

// buildLibrary uploads a small library to the admin's home:
//
//	Pink Floyd / The Wall (1979): In the Flesh (1), Hey You (2, feat. Gilmour as a second artist)
//	Pink Floyd / Animals (1977): Dogs
//	Various Artists / Hits (compilation): Song A (by Artist A), Song B (by David Gilmour)
//	David Gilmour / On an Island: Castellorizon
func buildLibrary(v *env) map[string]string {
	v.t.Helper()
	home := v.home(v.adm)
	ids := map[string]string{}
	add := func(dir, name, title string, frames ...mt.Frame) {
		all := append([]mt.Frame{mt.Text("TIT2", mt.UTF8, title)}, frames...)
		ids[title] = v.upload(v.adm, home, dir, name, mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, all...), 200, nil))
	}
	wall := []mt.Frame{mt.Text("TALB", mt.UTF8, "The Wall"), mt.Text("TDRC", mt.Latin1, "1979"), mt.Text("TCON", mt.Latin1, "Rock")}
	add("PF/Wall", "1.mp3", "In the Flesh", append(wall, mt.Text("TPE1", mt.UTF8, "Pink Floyd"), mt.Text("TRCK", mt.Latin1, "1"))...)
	add("PF/Wall", "2.mp3", "Hey You", append(wall, mt.Text("TPE1", mt.UTF8, "Pink Floyd", "David Gilmour"),
		mt.Text("TPE2", mt.UTF8, "Pink Floyd"), mt.Text("TRCK", mt.Latin1, "2"))...)
	add("PF/Animals", "1.mp3", "Dogs", mt.Text("TPE1", mt.UTF8, "pink  floyd"), mt.Text("TALB", mt.UTF8, "Animals"),
		mt.Text("TDRC", mt.Latin1, "1977"), mt.Text("TCON", mt.Latin1, "Progressive Rock"))
	add("Comp", "1.mp3", "Song A", mt.Text("TPE1", mt.UTF8, "Artist A"), mt.Text("TALB", mt.UTF8, "Hits"), mt.Text("TCMP", mt.Latin1, "1"),
		mt.Text("TCON", mt.Latin1, "Pop"))
	add("Comp", "2.mp3", "Song B", mt.Text("TPE1", mt.UTF8, "David Gilmour"), mt.Text("TALB", mt.UTF8, "Hits"), mt.Text("TCMP", mt.Latin1, "1"),
		mt.Text("TCON", mt.Latin1, "Pop"))
	add("DG", "1.mp3", "Castellorizon", mt.Text("TPE1", mt.UTF8, "David Gilmour"), mt.Text("TALB", mt.UTF8, "On an Island"),
		mt.APIC(mt.Latin1, "image/jpeg", 3, "", mt.JPEG(50, 50, 7)))
	for _, id := range ids {
		v.waitProbed(v.adm, id)
	}
	return ids
}

func TestAlbumsArtistsGenresFacets(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v)
	albums := items(v.adm.Do("GET", "/api/v1/music/albums?sort=title", nil).Expect(200).Map())
	if got := field(albums, "title"); !eq(got, []string{"Animals", "Hits", "On an Island", "The Wall"}) {
		t.Fatalf("albums %v", got)
	}
	byTitle := map[string]map[string]any{}
	for _, a := range albums {
		byTitle[a["title"].(string)] = a
	}
	wall, hits := byTitle["The Wall"], byTitle["Hits"]
	if wall["artist"] != "Pink Floyd" || num(wall["track_count"]) != 2 || num(wall["year"]) != 1979 || num(wall["disc_count"]) != 1 ||
		num(wall["duration_seconds"]) < 399 || wall["compilation"] != false || wall["art_url"] != nil || fmt.Sprint(wall["genres"]) != "[Rock]" {
		t.Fatalf("wall %v", wall)
	}
	if hits["artist"] != "Various Artists" || hits["compilation"] != true || num(hits["track_count"]) != 2 {
		t.Fatalf("hits %v", hits)
	}
	if byTitle["On an Island"]["art_url"] == nil {
		t.Fatal("album art from the embedded picture")
	}
	// Same album artist across case/whitespace variants: one artist id.
	if byTitle["Animals"]["artist_id"] != wall["artist_id"] {
		t.Fatalf("pink floyd variants: %v %v", byTitle["Animals"]["artist_id"], wall["artist_id"])
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"sort=year", []string{"Animals", "The Wall", "Hits", "On an Island"}},
		{"sort=artist", []string{"On an Island", "Animals", "The Wall", "Hits"}}, // David Gilmour, Pink Floyd, Various
		{"compilation=true", []string{"Hits"}},
		{"genre=rock", []string{"The Wall"}},
		{"year=1977", []string{"Animals"}},
		{"q=wall", []string{"The Wall"}},
		{"q=floyd&sort=title", []string{"Animals", "The Wall"}},
		{"artist_id=" + wall["artist_id"].(string) + "&sort=title", []string{"Animals", "The Wall"}},
	} {
		if got := field(items(v.adm.Do("GET", "/api/v1/music/albums?"+tc.q, nil).Expect(200).Map()), "title"); !eq(got, tc.want) {
			t.Errorf("albums %s: %v want %v", tc.q, got, tc.want)
		}
	}
	// Paging through albums.
	page := v.adm.Do("GET", "/api/v1/music/albums?limit=3&sort=added&order=desc", nil).Expect(200).Map()
	rest := v.adm.Do("GET", "/api/v1/music/albums?limit=3&sort=added&order=desc&cursor="+page["next_cursor"].(string), nil).Expect(200).Map()
	if len(items(page)) != 3 || len(items(rest)) != 1 || rest["next_cursor"] != nil {
		t.Fatalf("album paging %d %d", len(items(page)), len(items(rest)))
	}
	// Album detail: tracks by disc/number/title.
	d := v.adm.Do("GET", "/api/v1/music/albums/"+wall["id"].(string), nil).Expect(200).Map()
	if got := field(list(d["tracks"]), "title"); !eq(got, []string{"In the Flesh", "Hey You"}) {
		t.Fatalf("album tracks %v", got)
	}
	v.adm.Do("GET", "/api/v1/music/albums/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/music/albums/bad", nil).ExpectError(404, "not_found")

	// Artists: album artists by default, track-only artists with role=all.
	artists := items(v.adm.Do("GET", "/api/v1/music/artists", nil).Expect(200).Map())
	if got := field(artists, "name"); !eq(got, []string{"David Gilmour", "Pink Floyd", "Various Artists"}) {
		t.Fatalf("artists %v", got)
	}
	all := field(items(v.adm.Do("GET", "/api/v1/music/artists?role=all", nil).Expect(200).Map()), "name")
	if !eq(all, []string{"Artist A", "David Gilmour", "Pink Floyd", "Various Artists"}) {
		t.Fatalf("role=all %v", all)
	}
	var gilmour map[string]any
	for _, a := range artists {
		if a["name"] == "David Gilmour" {
			gilmour = a
		}
	}
	if num(gilmour["album_count"]) != 1 || num(gilmour["track_count"]) != 3 || gilmour["art_url"] == nil {
		t.Fatalf("gilmour %v", gilmour)
	}
	ad := v.adm.Do("GET", "/api/v1/music/artists/"+gilmour["id"].(string), nil).Expect(200).Map()
	if got := field(list(ad["albums"]), "title"); !eq(got, []string{"On an Island"}) {
		t.Fatalf("own albums %v", got)
	}
	if got := field(list(ad["appears_on"]), "title"); !eq(got, []string{"The Wall", "Hits"}) && !eq(got, []string{"Hits", "The Wall"}) {
		t.Fatalf("appears on %v", got)
	}
	if len(list(ad["top_tracks"])) != 3 {
		t.Fatalf("top tracks %v", ad["top_tracks"])
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/artists?q=gilm", nil).Expect(200).Map()), "name"); !eq(got, []string{"David Gilmour"}) {
		t.Fatalf("artist search %v", got)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/artists?genre=pop&role=all", nil).Expect(200).Map()), "name"); !eq(got,
		[]string{"Artist A", "David Gilmour", "Various Artists"}) {
		t.Fatalf("artist genre %v", got)
	}
	r := v.raw(v.adm, "GET", "/api/v1/music/artists/"+gilmour["id"].(string)+"/art?size=sm", nil, nil).expect(t, 200)
	if r.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatal("artist art")
	}
	v.adm.Do("GET", "/api/v1/music/artists/"+wall["artist_id"].(string)+"/art", nil).ExpectError(404, "not_found")

	// Genres and facets.
	genres := list(anyJSON(v.adm.Do("GET", "/api/v1/music/genres", nil).Expect(200)))
	if got := field(genres, "name"); !eq(got, []string{"Pop", "Progressive Rock", "Rock"}) {
		t.Fatalf("genres %v", got)
	}
	if num(genres[0]["track_count"]) != 2 || num(genres[0]["album_count"]) != 1 {
		t.Fatalf("pop %v", genres[0])
	}
	f := v.adm.Do("GET", "/api/v1/music/facets", nil).Expect(200).Map()
	totals := f["totals"].(map[string]any)
	if num(totals["tracks"]) != 6 || num(totals["albums"]) != 4 || num(totals["artists"]) != 3 || num(totals["size_bytes"]) <= 0 {
		t.Fatalf("totals %v", totals)
	}
	if sp := list(f["spaces"]); len(sp) != 1 || sp[0]["label"] != "Home" || num(sp[0]["count"]) != 6 {
		t.Fatalf("spaces %v", f["spaces"])
	}
	if ys := list(f["years"]); len(ys) != 2 || num(ys[0]["value"]) != 1979 {
		t.Fatalf("years %v", f["years"])
	}

	// Search.
	s := v.adm.Do("GET", "/api/v1/music/search?q=floyd", nil).Expect(200).Map()
	if len(list(s["tracks"])) != 3 || len(list(s["albums"])) != 2 || field(list(s["artists"]), "name")[0] != "Pink Floyd" {
		t.Fatalf("search %v", s)
	}
	s = v.adm.Do("GET", "/api/v1/music/search?q=hey&limit=1", nil).Expect(200).Map()
	if got := field(list(s["tracks"]), "title"); !eq(got, []string{"Hey You"}) {
		t.Fatalf("search tracks %v", got)
	}
	s = v.adm.Do("GET", "/api/v1/music/search?q=%2A%2A", nil).Expect(200).Map()
	if len(list(s["tracks"])) != 0 {
		t.Fatal("empty search")
	}
	_ = ids
}

func TestFavoritesHomeAndAlbumArt(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v)
	bobID, bob := v.user("bob")
	wall := v.track(v.adm, ids["Hey You"])
	albumID, artistID := wall["album_id"].(string), wall["artist_id"].(string)
	for _, body := range []map[string]any{
		{"kind": "track", "id": ids["Dogs"], "favorite": true},
		{"kind": "album", "id": albumID, "favorite": true},
		{"kind": "artist", "id": artistID, "favorite": true},
	} {
		v.adm.Do("PUT", "/api/v1/music/favorites", body).Expect(204)
	}
	v.adm.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "song", "id": albumID, "favorite": true}).ExpectError(422, "validation_failed")
	v.adm.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "album", "id": albumID}).ExpectError(422, "validation_failed")
	if v.track(v.adm, ids["Dogs"])["favorite"] != true {
		t.Fatal("track favorite")
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/tracks?favorite=true", nil).Expect(200).Map()), "title"); !eq(got, []string{"Dogs"}) {
		t.Fatalf("favorite tracks %v", got)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/albums?favorite=true", nil).Expect(200).Map()), "title"); !eq(got, []string{"The Wall"}) {
		t.Fatalf("favorite albums %v", got)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/artists?favorite=true", nil).Expect(200).Map()), "name"); !eq(got, []string{"Pink Floyd"}) {
		t.Fatalf("favorite artists %v", got)
	}
	// Bob cannot favorite what he cannot see; unfavoriting always works.
	bob.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "album", "id": albumID, "favorite": true}).ExpectError(404, "not_found")
	bob.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "album", "id": albumID, "favorite": false}).Expect(204)

	home := v.adm.Do("GET", "/api/v1/music/home", nil).Expect(200).Map()
	if len(list(home["recently_added"])) != 4 || field(list(home["favorite_albums"]), "title")[0] != "The Wall" ||
		len(list(home["recently_played"])) != 0 || len(list(home["most_played"])) != 0 {
		t.Fatalf("home %v", home)
	}

	// Custom album art: needs write access to every space holding the album.
	lib := v.library("Lib", map[string]any{"user_id": bobID, "access": "read"})
	shared := v.upload(v.adm, lib, "", "x.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.Text("TPE1", mt.UTF8, "Pink Floyd"),
		mt.Text("TALB", mt.UTF8, "The Wall")), 10, nil))
	v.waitProbed(v.adm, shared)
	if v.track(v.adm, shared)["album_id"] != albumID {
		t.Fatal("same album across spaces")
	}
	bob.Do("PUT", "/api/v1/music/albums/"+albumID+"/art", nil).ExpectError(403, "forbidden")
	png := mt.PNG(300, 200)
	r := v.raw(v.adm, "PUT", "/api/v1/music/albums/"+albumID+"/art", map[string]string{"Content-Type": "image/png"}, png).expect(t, 200)
	a := r.json(t)
	if a["has_custom_art"] != true || a["art_url"] == nil {
		t.Fatalf("custom art %v", a)
	}
	if tr := v.track(bob, shared); tr["has_art"] != true || tr["art_url"] == nil {
		t.Fatalf("bob sees the library's custom art: %v", tr["art_url"])
	}
	if tr := v.track(v.adm, ids["In the Flesh"]); tr["has_art"] != true {
		t.Fatal("home track shows the custom art")
	}
	v.raw(v.adm, "PUT", "/api/v1/music/albums/"+albumID+"/art", nil, []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")).expect(t, 415)
	v.raw(v.adm, "PUT", "/api/v1/music/albums/"+albumID+"/art", nil, []byte("garbage")).expect(t, 415)
	v.raw(v.adm, "DELETE", "/api/v1/music/albums/"+albumID+"/art", nil, nil).expect(t, 204)
	if tr := v.track(bob, shared); tr["has_art"] != false {
		t.Fatal("custom art removed")
	}
	// A user's album copy in their own home is not affected by another user's art.
	bobOwn := v.upload(bob, v.home(bob), "", "w.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.Text("TPE1", mt.UTF8, "Pink Floyd"),
		mt.Text("TALB", mt.UTF8, "The Wall")), 10, nil))
	v.waitProbed(bob, bobOwn)
	v.raw(v.adm, "PUT", "/api/v1/music/albums/"+albumID+"/art", map[string]string{"Content-Type": "image/png"}, png).expect(t, 200)
	if tr := v.track(bob, bobOwn); tr["has_art"] != false {
		t.Fatal("admin's custom art leaked into bob's home copy")
	}
	r = v.raw(bob, "GET", "/api/v1/music/albums/"+albumID+"/art?size=lg", nil, nil).expect(t, 200)
	if w, h, err := decodeSize(r.Body); err != nil || w != 300 || h != 200 {
		t.Fatalf("bob's album art (from the library) %dx%d %v", w, h, err)
	}
}
