package music_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	mt "ludworks/internal/music/musictest"
)

func itemTitles(pl map[string]any) []string {
	var out []string
	for _, it := range list(pl["items"]) {
		out = append(out, it["track"].(map[string]any)["title"].(string))
	}
	return out
}

func itemIDs(pl map[string]any) []string {
	var out []string
	for _, it := range list(pl["items"]) {
		out = append(out, it["id"].(string))
	}
	return out
}

func TestPlaylistsCRUDAndPermissions(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v)
	bobID, bob := v.user("bob")
	_, carol := v.user("carol")
	lib := v.library("Shared", map[string]any{"user_id": "*", "access": "read"}, map[string]any{"user_id": bobID, "access": "write"})
	shared := v.upload(v.adm, lib, "", "s.mp3", taggedMP3(300, "TIT2", "Library Song", "TALB", "Lib Album",
		"TPE1", "Lib Artist"))
	v.waitProbed(v.adm, shared)

	pl := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": " Mix ", "description": "line1\nline2",
		"track_ids": []string{ids["Dogs"], shared, ids["Hey You"], ids["Dogs"]}}).Expect(201).Map()
	plID := pl["id"].(string)
	if pl["name"] != "Mix" || pl["visibility"] != "private" || pl["kind"] != "manual" || pl["rules"] != nil ||
		num(pl["track_count"]) != 4 || num(pl["unavailable_count"]) != 0 || pl["can_edit"] != true || num(pl["version"]) != 1 ||
		pl["owner_name"] != "admin" || pl["description"] != "line1\nline2" || num(pl["duration_seconds"]) < 899 || pl["art_url"] != nil {
		t.Fatalf("created %v", pl)
	}
	d := v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if got := itemTitles(d); !eq(got, []string{"Dogs", "Library Song", "Hey You", "Dogs"}) {
		t.Fatalf("items %v", got)
	}
	for i, it := range list(d["items"]) {
		if num(it["position"]) != float64(i) || it["added_by"] == "" || it["added_at"] == "" {
			t.Fatalf("item %d %v", i, it)
		}
	}
	// Private: others get 404.
	bob.Do("GET", "/api/v1/music/playlists/"+plID, nil).ExpectError(404, "not_found")
	bob.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"name": "x"}).ExpectError(404, "not_found")
	if n := len(items(bob.Do("GET", "/api/v1/music/playlists", nil).Expect(200).Map())); n != 0 {
		t.Fatalf("bob lists %d playlists", n)
	}
	// Shared: visible, only readable tracks listed; edits by others are 403.
	pl = v.adm.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"visibility": "shared", "base_version": 1}).Expect(200).Map()
	if num(pl["version"]) != 2 {
		t.Fatalf("version %v", pl["version"])
	}
	bd := bob.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if got := itemTitles(bd); !eq(got, []string{"Library Song"}) || num(bd["unavailable_count"]) != 3 || num(bd["track_count"]) != 1 ||
		bd["can_edit"] != false || num(list(bd["items"])[0]["position"]) != 0 {
		t.Fatalf("bob's view %v", bd)
	}
	bob.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"name": "x"}).ExpectError(403, "forbidden")
	bob.Do("DELETE", "/api/v1/music/playlists/"+plID, nil).ExpectError(403, "forbidden")
	bob.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{shared}}).ExpectError(403, "forbidden")
	bob.Do("PUT", "/api/v1/music/playlists/"+plID+"/art", nil).ExpectError(403, "forbidden")
	if got := field(items(bob.Do("GET", "/api/v1/music/playlists?owner=others", nil).Expect(200).Map()), "name"); !eq(got, []string{"Mix"}) {
		t.Fatalf("bob others %v", got)
	}
	if n := len(items(bob.Do("GET", "/api/v1/music/playlists?owner=me", nil).Expect(200).Map())); n != 0 {
		t.Fatal("owner=me")
	}
	// Stale base_version: 409 with the current playlist.
	r := v.adm.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"name": "Old", "base_version": 1})
	r.ExpectError(409, "conflict")
	var conflict struct {
		Error struct {
			Details struct {
				Current map[string]any `json:"current"`
			} `json:"details"`
		} `json:"error"`
	}
	r.JSON(&conflict)
	if num(conflict.Error.Details.Current["version"]) != 2 || conflict.Error.Details.Current["name"] != "Mix" {
		t.Fatalf("conflict details %s", r.Body)
	}
	// Collaborative: everyone edits items (only the items they see); settings stay owner-only.
	v.adm.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"collaborative": true}).Expect(200)
	bd = bob.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if bd["can_edit"] != true {
		t.Fatal("collaborative can_edit")
	}
	bobSong := v.upload(bob, lib, "", "b.mp3", taggedMP3(10, "TIT2", "Bob Song"))
	v.waitProbed(bob, bobSong)
	bd = bob.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{bobSong}, "position": 0}).Expect(200).Map()
	if got := itemTitles(bd); !eq(got, []string{"Bob Song", "Library Song"}) {
		t.Fatalf("bob inserted %v", got)
	}
	ad := v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if got := itemTitles(ad); !eq(got, []string{"Dogs", "Bob Song", "Library Song", "Hey You", "Dogs"}) {
		t.Fatalf("owner's view after bob's insert at his position 0: %v", got)
	}
	// Bob replaces his view: the items he cannot see keep their places.
	bob.Do("PUT", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{shared, bobSong, shared}}).Expect(200)
	ad = v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if got := itemTitles(ad); !eq(got, []string{"Dogs", "Library Song", "Bob Song", "Hey You", "Dogs", "Library Song"}) {
		t.Fatalf("after bob's replacement: %v", got)
	}
	// Move: the listed items keep their relative order and land at position in the list
	// without them.
	all := itemIDs(ad)
	ad = v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items/move", map[string]any{"item_ids": []string{all[5], all[0]}, "position": 1}).Expect(200).Map()
	if got := itemTitles(ad); !eq(got, []string{"Library Song", "Dogs", "Library Song", "Bob Song", "Hey You", "Dogs"}) {
		t.Fatalf("move: %v", got)
	}
	ad = v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items/move", map[string]any{"item_ids": []string{itemIDs(ad)[0]}, "position": 99}).Expect(200).Map()
	if got := itemTitles(ad); got[len(got)-1] != "Library Song" {
		t.Fatalf("move to end: %v", got)
	}
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items/move", map[string]any{"item_ids": []string{"zzzzzzzzzzzzzzzzzzzzzzzzzz"}, "position": 0}).
		ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items/move", map[string]any{"item_ids": []string{itemIDs(ad)[0]}}).
		ExpectError(422, "validation_failed")
	// Remove (unknown ids ignored); base_version conflicts.
	cur := num(ad["version"])
	ad = v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items/remove", map[string]any{"item_ids": []string{itemIDs(ad)[0], "zzzzzzzzzzzzzzzzzzzzzzzzzz"},
		"base_version": cur}).Expect(200).Map()
	if num(ad["version"]) != cur+1 || len(list(ad["items"])) != 5 {
		t.Fatalf("remove %v", ad)
	}
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{shared}, "base_version": cur}).ExpectError(409, "conflict")
	// Validation and limits.
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{"aaaaaaaaaaaaaaaaaaaaaaaaaa"}}).ExpectError(422, "validation_failed")
	carolSong := v.upload(carol, v.home(carol), "", "c.mp3", taggedMP3(1))
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{carolSong}}).Expect(200) // admins read every space
	bob.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{carolSong}}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": strings.Repeat("x", 201)}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": ""}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "x", "description": strings.Repeat("d", 2001)}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "x", "visibility": "public"}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "x", "rules": map[string]any{}}).ExpectError(422, "validation_failed")

	// Listing, sorting, search.
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Another"}).Expect(201)
	if got := field(items(v.adm.Do("GET", "/api/v1/music/playlists?sort=name", nil).Expect(200).Map()), "name"); !eq(got, []string{"Another", "Mix"}) {
		t.Fatalf("list %v", got)
	}
	if got := field(items(v.adm.Do("GET", "/api/v1/music/playlists?q=mi", nil).Expect(200).Map()), "name"); !eq(got, []string{"Mix"}) {
		t.Fatalf("q %v", got)
	}
	s := v.adm.Do("GET", "/api/v1/music/search?q=anoth", nil).Expect(200).Map()
	if got := field(list(s["playlists"]), "name"); !eq(got, []string{"Another"}) {
		t.Fatalf("search playlists %v", got)
	}

	// Art: the mosaic needs covers; custom art wins; owner only.
	if ad := v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map(); ad["art_url"] != nil {
		t.Fatalf("no covers, no art: %v", ad["art_url"])
	}
	v.adm.Do("GET", "/api/v1/music/playlists/"+plID+"/art", nil).ExpectError(404, "not_found")
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{ids["Castellorizon"]}}).Expect(200)
	ad = v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if ad["art_url"] == nil || ad["has_custom_art"] != false {
		t.Fatalf("single cover art %v", ad["art_url"])
	}
	v.raw(v.adm, "GET", "/api/v1/music/playlists/"+plID+"/art?size=sm", nil, nil).expect(t, 200)
	pr := v.raw(v.adm, "PUT", "/api/v1/music/playlists/"+plID+"/art", map[string]string{"Content-Type": "image/png"}, mt.PNG(80, 80)).expect(t, 200)
	if p := pr.json(t); p["has_custom_art"] != true {
		t.Fatalf("custom playlist art %v", p)
	}
	v.raw(v.adm, "DELETE", "/api/v1/music/playlists/"+plID+"/art", nil, nil).expect(t, 204)

	// Duplicate (anyone who sees it): the caller's readable items, private.
	dup := bob.Do("POST", "/api/v1/music/playlists/"+plID+"/duplicate", map[string]any{}).Expect(201).Map()
	if dup["name"] != "Mix (copy)" || dup["owner_id"] != bobID || dup["visibility"] != "private" || num(dup["unavailable_count"]) != 0 {
		t.Fatalf("dup %v", dup)
	}
	bdup := bob.Do("GET", "/api/v1/music/playlists/"+dup["id"].(string), nil).Expect(200).Map()
	for _, title := range itemTitles(bdup) {
		if title != "Library Song" && title != "Bob Song" {
			t.Fatalf("duplicate copied an unreadable track: %v", itemTitles(bdup))
		}
	}
	// Delete (owner only).
	v.adm.Do("DELETE", "/api/v1/music/playlists/"+plID, nil).Expect(204)
	v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).ExpectError(404, "not_found")
}

func TestSmartPlaylists(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v)
	_, bob := v.user("bob")
	rules := map[string]any{"match": "all", "conditions": []map[string]any{
		{"field": "artist", "op": "contains", "value": "gilmour"},
	}, "sort": "title", "order": "asc", "limit": nil}
	pl := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Gilmour", "kind": "smart", "rules": rules,
		"visibility": "shared"}).Expect(201).Map()
	plID := pl["id"].(string)
	if pl["kind"] != "smart" || pl["rules"] == nil || num(pl["track_count"]) != 3 {
		t.Fatalf("smart %v", pl)
	}
	d := v.adm.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map()
	if got := itemTitles(d); !eq(got, []string{"Castellorizon", "Hey You", "Song B"}) {
		t.Fatalf("smart items %v", got)
	}
	// Evaluated per caller: bob has none of these tracks.
	if bd := bob.Do("GET", "/api/v1/music/playlists/"+plID, nil).Expect(200).Map(); num(bd["track_count"]) != 0 || len(list(bd["items"])) != 0 {
		t.Fatalf("bob's evaluation %v", bd)
	}
	// Read-only lists.
	v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{ids["Dogs"]}}).ExpectError(409, "conflict")
	v.adm.Do("PUT", "/api/v1/music/playlists/"+plID+"/items", map[string]any{"track_ids": []string{}}).ExpectError(409, "conflict")
	// Per-user stats in rules: plays, favorites, ratings.
	v.adm.Do("PUT", "/api/v1/music/tracks/"+ids["Dogs"]+"/rating", map[string]any{"rating": 5}).Expect(204)
	v.adm.Do("PUT", "/api/v1/music/favorites", map[string]any{"kind": "track", "id": ids["Song A"], "favorite": true}).Expect(204)
	now := timeNow()
	v.adm.Do("POST", "/api/v1/music/plays", map[string]any{"plays": []map[string]any{
		{"track_id": ids["Hey You"], "played_at": now, "seconds_played": 200},
	}}).Expect(200)
	cases := []struct {
		rules map[string]any
		want  []string
	}{
		{map[string]any{"match": "any", "conditions": []map[string]any{
			{"field": "rating", "op": "gte", "value": 4}, {"field": "favorite", "op": "is_true"}}}, []string{"Dogs", "Song A"}},
		{map[string]any{"conditions": []map[string]any{{"field": "play_count", "op": "gt", "value": 0}}}, []string{"Hey You"}},
		{map[string]any{"conditions": []map[string]any{{"field": "last_played_at", "op": "in_last_days", "value": 1}}}, []string{"Hey You"}},
		{map[string]any{"conditions": []map[string]any{{"field": "year", "op": "between", "value": []int{1970, 1978}}}}, []string{"Dogs"}},
		{map[string]any{"conditions": []map[string]any{{"field": "genre", "op": "is", "value": "ROCK"}}, "sort": "title"}, []string{"Hey You", "In the Flesh"}},
		{map[string]any{"conditions": []map[string]any{{"field": "genre", "op": "not_contains", "value": "rock"},
			{"field": "compilation", "op": "is_false"}}}, []string{"Castellorizon"}},
		{map[string]any{"conditions": []map[string]any{{"field": "album", "op": "starts_with", "value": "the"}}, "sort": "title", "order": "desc", "limit": 1},
			[]string{"In the Flesh"}},
		{map[string]any{"conditions": []map[string]any{{"field": "path", "op": "contains", "value": "pf/"}}, "sort": "title"},
			[]string{"Dogs", "Hey You", "In the Flesh"}},
		{map[string]any{"conditions": []map[string]any{{"field": "added_at", "op": "before", "value": "2000-01-01T00:00:00Z"}}}, nil},
		{map[string]any{"conditions": []map[string]any{{"field": "title", "op": "is_not", "value": "dogs"},
			{"field": "album_artist", "op": "is", "value": "pink floyd"}}, "sort": "title"}, []string{"Hey You", "In the Flesh"}},
	}
	for i, tc := range cases {
		pl := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": fmt.Sprint("case ", i), "kind": "smart", "rules": tc.rules}).Expect(201).Map()
		d := v.adm.Do("GET", "/api/v1/music/playlists/"+pl["id"].(string), nil).Expect(200).Map()
		got := itemTitles(d)
		if tc.want == nil && len(got) == 0 {
			continue
		}
		if !eq(sorted(got), sorted(tc.want)) || (tc.rules["sort"] != nil && !eq(got, tc.want)) {
			t.Errorf("case %d: got %v want %v", i, got, tc.want)
		}
	}
	// Random is stable (for the day).
	rp := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Random", "kind": "smart", "rules": map[string]any{
		"conditions": []map[string]any{{"field": "duration_seconds", "op": "gt", "value": 0}}, "sort": "random"}}).Expect(201).Map()
	a := itemTitles(v.adm.Do("GET", "/api/v1/music/playlists/"+rp["id"].(string), nil).Expect(200).Map())
	b := itemTitles(v.adm.Do("GET", "/api/v1/music/playlists/"+rp["id"].(string), nil).Expect(200).Map())
	if !eq(a, b) || len(a) != 6 {
		t.Fatalf("random %v %v", a, b)
	}
	// Invalid rules are 422 with field paths.
	for _, bad := range []map[string]any{
		{"conditions": []map[string]any{}},
		{"conditions": []map[string]any{{"field": "nope", "op": "is", "value": "x"}}},
		{"conditions": []map[string]any{{"field": "year", "op": "contains", "value": "x"}}},
		{"conditions": []map[string]any{{"field": "year", "op": "between", "value": []int{2000, 1990}}}},
		{"conditions": []map[string]any{{"field": "added_at", "op": "in_last_days", "value": 0}}},
		{"conditions": []map[string]any{{"field": "added_at", "op": "before", "value": "yesterday"}}},
		{"conditions": []map[string]any{{"field": "title", "op": "contains", "value": ""}}},
		{"conditions": []map[string]any{{"field": "title", "op": "is", "value": 5}}},
		{"conditions": []map[string]any{{"field": "title", "op": "is", "value": "x"}}, "limit": 5001},
		{"conditions": []map[string]any{{"field": "title", "op": "is", "value": "x"}}, "sort": "rating"},
		{"conditions": []map[string]any{{"field": "title", "op": "is", "value": "x"}}, "match": "some"},
	} {
		r := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "bad", "kind": "smart", "rules": bad})
		if r.Status != 422 || !strings.Contains(string(r.Body), "rules") {
			t.Errorf("rules %v: %d %s", bad, r.Status, r.Body)
		}
	}
	conds := make([]map[string]any, 21)
	for i := range conds {
		conds[i] = map[string]any{"field": "title", "op": "is", "value": "x"}
	}
	v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "bad", "kind": "smart", "rules": map[string]any{"conditions": conds}}).
		ExpectError(422, "validation_failed")
	// Update rules; rules on a manual playlist are refused.
	pl = v.adm.Do("PATCH", "/api/v1/music/playlists/"+plID, map[string]any{"rules": map[string]any{
		"conditions": []map[string]any{{"field": "title", "op": "is", "value": "dogs"}}}}).Expect(200).Map()
	if num(pl["track_count"]) != 1 {
		t.Fatalf("updated rules %v", pl)
	}
	manual := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "m"}).Expect(201).Map()
	v.adm.Do("PATCH", "/api/v1/music/playlists/"+manual["id"].(string), map[string]any{"rules": rules}).ExpectError(422, "validation_failed")
	// Duplicate: rules (smart) or a frozen snapshot (manual).
	dup := v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/duplicate", map[string]any{"name": "Copy"}).Expect(201).Map()
	frozen := v.adm.Do("POST", "/api/v1/music/playlists/"+plID+"/duplicate", map[string]any{"freeze": true}).Expect(201).Map()
	if dup["kind"] != "smart" || frozen["kind"] != "manual" || num(frozen["track_count"]) != 1 || frozen["rules"] != nil {
		t.Fatalf("dup %v frozen %v", dup, frozen)
	}
}

func TestM3UImportExport(t *testing.T) {
	v := newEnv(t)
	ids := buildLibrary(v)
	home := v.home(v.adm)
	lib := v.library("Shared Music", map[string]any{"user_id": "*", "access": "read"})
	shared := v.upload(v.adm, lib, "Sub", "song.mp3", taggedMP3(100, "TIT2", "Shared Song", "TPE1", "Someone"))
	v.waitProbed(v.adm, shared)
	pl := v.adm.Do("POST", "/api/v1/music/playlists", map[string]any{"name": "Road/Trip", "track_ids": []string{ids["Dogs"], shared}}).Expect(201).Map()
	r := v.raw(v.adm, "GET", "/api/v1/music/playlists/"+pl["id"].(string)+"/export.m3u8", nil, nil).expect(t, 200)
	body := string(r.Body)
	if !strings.HasPrefix(body, "#EXTM3U\n") || !strings.Contains(body, "#EXTINF:200,pink  floyd - Dogs\nHome/PF/Animals/1.mp3\n") ||
		!strings.Contains(body, "Shared Music/Sub/song.mp3") || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(r.Header.Get("Content-Disposition"), "Road_Trip.m3u8") {
		t.Fatalf("export %q %v", body, r.Header)
	}
	// Import the export back (space-name paths), plus relative paths, #EXTINF fallbacks and junk.
	m3u := body + "#EXTINF:100,Pink Floyd - In the Flesh\nC:\\Users\\me\\Music\\missing.mp3\n" +
		"PF/Wall/2.mp3\n../../etc/passwd\nhttp://example.com/stream.mp3\nnot/there.mp3\n"
	q := url.Values{"name": {"Imported"}}
	ir := v.raw(v.adm, "POST", "/api/v1/music/playlists/import?"+q.Encode(), map[string]string{"Content-Type": "audio/x-mpegurl"}, []byte(m3u)).expect(t, 201)
	imp := ir.json(t)
	if imp["name"] != "Imported" || num(imp["track_count"]) != 4 {
		t.Fatalf("import %v", imp)
	}
	um := imp["unmatched"].([]any)
	if len(um) != 3 || um[0] != "../../etc/passwd" {
		t.Fatalf("unmatched %v", um)
	}
	d := v.adm.Do("GET", "/api/v1/music/playlists/"+imp["id"].(string), nil).Expect(200).Map()
	if got := itemTitles(d); !eq(got, []string{"Dogs", "Shared Song", "In the Flesh", "Hey You"}) {
		t.Fatalf("imported items %v", got)
	}
	// base_path, Latin-1 bodies and the size limit.
	latin := []byte("#EXTM3U\n1.mp3\n#EXTINF:1,Caf\xe9 - Nothing\nnope.mp3\n")
	q = url.Values{"space_id": {home}, "base_path": {"PF/Wall"}}
	ir = v.raw(v.adm, "POST", "/api/v1/music/playlists/import?"+q.Encode(), nil, latin).expect(t, 201)
	if imp := ir.json(t); imp["name"] != "Imported playlist" || num(imp["track_count"]) != 1 {
		t.Fatalf("base_path import %v", imp)
	}
	big := make([]byte, 5<<20+1)
	v.raw(v.adm, "POST", "/api/v1/music/playlists/import", nil, big).expect(t, 413)
	q = url.Values{"base_path": {"../x"}}
	v.raw(v.adm, "POST", "/api/v1/music/playlists/import?"+q.Encode(), nil, []byte("a.mp3")).expect(t, 422)
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
