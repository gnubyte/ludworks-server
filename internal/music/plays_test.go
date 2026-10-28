package music_test

import (
	"bytes"

	"net/http/httptest"
	"testing"
	"time"

	mt "ludworks/internal/music/musictest"
)

func TestPlaysHistoryAndHome(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v) // every track is 200 s long
	home := v.home(v.adm)
	long := v.upload(v.adm, home, "", "long.mp3", taggedMP3(600, "TIT2", "Long"))
	v.waitProbed(v.adm, long)
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	albumOf := v.track(v.adm, ids["Hey You"])["album_id"].(string)
	plays := []map[string]any{
		{"track_id": ids["Hey You"], "played_at": at(-10 * time.Minute), "seconds_played": 100, "context": map[string]any{"kind": "album", "id": albumOf}}, // 50% of 200 s
		{"track_id": ids["Hey You"], "played_at": at(-10 * time.Minute), "seconds_played": 150},                                                            // duplicate
		{"track_id": ids["Dogs"], "played_at": at(-9 * time.Minute), "seconds_played": 99},                                                                 // too short
		{"track_id": ids["Dogs"], "played_at": at(-8 * time.Minute), "seconds_played": 5, "completed": true},                                               // completed
		{"track_id": long, "played_at": at(-7 * time.Minute), "seconds_played": 240},                                                                       // 240 s cap
		{"track_id": long, "played_at": at(-6 * time.Minute), "seconds_played": 239},                                                                       // below the cap
		{"track_id": ids["Song A"], "played_at": at(10 * time.Minute), "seconds_played": 200},                                                              // future
		{"track_id": ids["Song A"], "played_at": at(-400 * 24 * time.Hour), "seconds_played": 200},                                                         // too old
		{"track_id": "zzzzzzzzzzzzzzzzzzzzzzzzzz", "played_at": at(-time.Minute), "seconds_played": 200},                                                   // unknown
		{"track_id": ids["Song B"], "played_at": at(-5 * time.Minute), "seconds_played": 200, "context": map[string]any{"kind": "queue"}},
	}
	r := v.adm.Do("POST", "/api/v1/music/plays", map[string]any{"plays": plays}).Expect(200).Map()
	if num(r["recorded"]) != 4 || num(r["ignored"]) != 6 {
		t.Fatalf("plays %v", r)
	}
	// Replaying the queue changes nothing.
	r = v.adm.Do("POST", "/api/v1/music/plays", map[string]any{"plays": plays}).Expect(200).Map()
	if num(r["recorded"]) != 0 {
		t.Fatalf("replay recorded %v", r)
	}
	tr := v.track(v.adm, ids["Hey You"])
	if num(tr["play_count"]) != 1 || tr["last_played_at"] == nil {
		t.Fatalf("stats %v", tr)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/tracks?sort=plays&order=desc&limit=4", nil).Expect(200).Map()), "title")[:4]; !eq(sorted(got),
		sorted([]string{"Dogs", "Hey You", "Long", "Song B"})) {
		t.Fatalf("sort=plays %v", got)
	}
	// History: newest first, paged, with contexts.
	h := v.adm.Do("GET", "/api/v1/music/history?limit=3", nil).Expect(200).Map()
	hi := items(h)
	if len(hi) != 3 || hi[0]["track"].(map[string]any)["title"] != "Song B" || hi[0]["context"].(map[string]any)["kind"] != "queue" ||
		h["next_cursor"] == nil {
		t.Fatalf("history %v", h)
	}
	h2 := v.adm.Do("GET", "/api/v1/music/history?limit=3&cursor="+h["next_cursor"].(string), nil).Expect(200).Map()
	if hi2 := items(h2); len(hi2) != 1 || hi2[0]["track"].(map[string]any)["title"] != "Hey You" || hi2[0]["context"].(map[string]any)["id"] != albumOf {
		t.Fatalf("history page 2 %v", h2)
	}
	// Home: recently played by context (album; queue → the album), most played.
	hm := v.adm.Do("GET", "/api/v1/music/home", nil).Expect(200).Map()
	rp := list(hm["recently_played"])
	if len(rp) != 4 || rp[0]["kind"] != "album" || rp[0]["album"].(map[string]any)["title"] != "Hits" ||
		rp[2]["kind"] != "album" || rp[2]["album"].(map[string]any)["title"] != "Animals" {
		t.Fatalf("recently played %v", rp)
	}
	var trackKind bool
	for _, it := range rp {
		if it["kind"] == "track" && it["track"].(map[string]any)["title"] == "Long" {
			trackKind = true
		}
	}
	if !trackKind {
		t.Fatalf("a track without album is a track entry: %v", rp)
	}
	if len(list(hm["most_played"])) != 4 {
		t.Fatalf("most played %v", hm["most_played"])
	}
	// Validation: shape errors are 422.
	for _, bad := range []map[string]any{
		{"plays": []map[string]any{{"track_id": ids["Dogs"], "played_at": "yesterday", "seconds_played": 1}}},
		{"plays": []map[string]any{{"track_id": ids["Dogs"], "played_at": at(0), "seconds_played": -1}}},
		{"plays": []map[string]any{{"track_id": ids["Dogs"], "played_at": at(0), "context": map[string]any{"kind": "radio"}}}},
	} {
		v.adm.Do("POST", "/api/v1/music/plays", bad).ExpectError(422, "validation_failed")
	}
	many := make([]map[string]any, 501)
	for i := range many {
		many[i] = map[string]any{"track_id": ids["Dogs"], "played_at": at(time.Duration(-i) * time.Second)}
	}
	v.adm.Do("POST", "/api/v1/music/plays", map[string]any{"plays": many}).ExpectError(422, "validation_failed")
	// Clearing the history clears counts.
	v.adm.Do("DELETE", "/api/v1/music/history", nil).Expect(204)
	if tr := v.track(v.adm, ids["Hey You"]); num(tr["play_count"]) != 0 || tr["last_played_at"] != nil {
		t.Fatalf("after clear %v", tr)
	}
	if len(items(v.adm.Do("GET", "/api/v1/music/history", nil).Expect(200).Map())) != 0 {
		t.Fatal("history not cleared")
	}
}

// TestServiceContract exercises the API the cast module uses.
func TestServiceContract(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	cover := mt.JPEG(100, 100, 1)
	id := v.upload(v.adm, home, "", "c.mp3", mt.MP3Xing(mt.ID3v2(mt.ID3Options{}, mt.Text("TIT2", mt.UTF8, "Cast Me"),
		mt.Text("TPE1", mt.UTF8, "Caster"), mt.Text("TALB", mt.UTF8, "Casting"), mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 180, nil))
	v.waitProbed(v.adm, id)
	text := v.upload(v.adm, home, "", "t.txt", []byte("x"))
	p := v.principalOf(v.adm)
	_, bob := v.user("bob")
	pb := v.principalOf(bob)

	info, err := v.svc.TrackInfo(ctx(), p, id)
	if err != nil || info.Title != "Cast Me" || info.Artist != "Caster" || info.Album != "Casting" || info.Mime != "audio/mpeg" ||
		info.Codec != "mp3" || info.Container != "mp3" || info.DurationSeconds < 179 || !info.HasArt {
		t.Fatalf("info %+v %v", info, err)
	}
	if _, err := v.svc.TrackInfo(ctx(), pb, id); err == nil {
		t.Fatal("bob read the admin's track")
	}
	if _, err := v.svc.TrackInfo(ctx(), p, text); err == nil {
		t.Fatal("non-music file accepted")
	}
	jpg, err := v.svc.ArtJPEG(ctx(), p, id, "sm")
	if err != nil || !bytes.HasPrefix(jpg, []byte{0xFF, 0xD8}) {
		t.Fatalf("art jpeg %d %v", len(jpg), err)
	}
	none := v.upload(v.adm, home, "", "n.mp3", taggedMP3(10))
	v.waitProbed(v.adm, none)
	if b, err := v.svc.ArtJPEG(ctx(), p, none, "md"); b != nil || err != nil {
		t.Fatalf("no art: %v %v", b, err)
	}
	// Serve* write responses directly (cast media routes carry no credentials).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Range", "bytes=0-9")
	if err := v.svc.ServeStream(rec, req, p, id); err != nil || rec.Code != 206 || rec.Body.Len() != 10 {
		t.Fatalf("stream %d %d %v", rec.Code, rec.Body.Len(), err)
	}
	rec = httptest.NewRecorder()
	if err := v.svc.ServeArt(rec, httptest.NewRequest("GET", "/x", nil), p, id, "lg"); err != nil || rec.Code != 200 {
		t.Fatalf("art %d %v", rec.Code, err)
	}
	if err := v.svc.ServeTranscode(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), p, id, "aac", 0, 0); err == nil {
		t.Fatal("transcode without ffmpeg")
	}
	if err := v.svc.ServeTranscode(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil), p, id, "flac", 0, 0); err == nil {
		t.Fatal("bad format accepted")
	}
	// RecordPlay applies the plays rules.
	if err := v.svc.RecordPlay(ctx(), p, id, time.Now().Add(-time.Minute), 10, false); err != nil {
		t.Fatal(err)
	}
	if err := v.svc.RecordPlay(ctx(), p, id, time.Now().Add(-time.Minute), 100, false); err != nil {
		t.Fatal(err)
	}
	if tr := v.track(v.adm, id); num(tr["play_count"]) != 1 {
		t.Fatalf("record play %v", tr["play_count"])
	}
	if err := v.svc.RecordPlay(ctx(), pb, id, time.Now(), 100, true); err == nil {
		t.Fatal("bob recorded a play of an unreadable track")
	}
}

// TestUserDeletion: a deleted user's playlists, plays, favorites and ratings go; items they
// added to others' playlists stay.
func TestUserDeletion(t *testing.T) {
	v := newEnv(t)
	bobID, bob := v.user("bob")
	lib := v.library("L", map[string]any{"user_id": "*", "access": "write"})
	song := v.upload(v.adm, lib, "", "s.mp3", taggedMP3(100, "TIT2", "S"))
	v.waitProbed(v.adm, song)
	shared := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Collab", "visibility": "shared", "collaborative": true}).Expect(201).Map()
	bob.Do("POST", "/api/v1/music/playlists/"+shared["id"].(string)+"/items", map[string]any{"track_ids": []string{song}}).Expect(200)
	own := bob.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Bob's", "visibility": "shared", "track_ids": []string{song}}).Expect(201).Map()
	bob.Do("PUT", "/api/v1/music/tracks/"+song+"/rating", map[string]any{"rating": 2}).Expect(204)
	bob.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "track", "id": song, "favorite": true}).Expect(204)
	bob.Do("POST", "/api/v1/music/plays", map[string]any{"plays": []map[string]any{{"track_id": song, "played_at": timeNow(), "completed": true}}}).Expect(200)

	v.admin.Do("DELETE", "/api/v1/admin/users/"+bobID, nil).Expect(204)
	v.adm.Do("GET", "/api/v1/music/playlists/"+own["id"].(string), nil).ExpectError(404, "not_found")
	d := v.adm.Do("GET", "/api/v1/music/playlists/"+shared["id"].(string), nil).Expect(200).Map()
	if it := list(d["items"]); len(it) != 1 || it[0]["added_by"] != "" {
		t.Fatalf("collaborator's item %v", d["items"])
	}
	var n int
	for _, q := range []string{"SELECT count(*) FROM music_plays", "SELECT count(*) FROM music_ratings",
		"SELECT count(*) FROM music_favorite_tracks", "SELECT count(*) FROM music_play_stats"} {
		if err := v.e.Deps.DB.SQL.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", q, n, err)
		}
	}

}
