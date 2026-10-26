package tv

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

type lineupResp struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Description          string       `json:"description"`
	OwnerID              string       `json:"owner_id"`
	Items                []LineupItem `json:"items"`
	ItemCount            int          `json:"item_count"`
	TotalDurationSeconds float64      `json:"total_duration_seconds"`
}

func TestLineupCRUDAndValidation(t *testing.T) {
	v := newEnv(t)
	c := v.adm

	// Validation: every problem is reported per field.
	r := c.Do("POST", "/api/v1/tv/lineups", map[string]any{"name": " ", "items": []any{
		map[string]any{"kind": "file"},
		map[string]any{"kind": "season", "show_title": "X"},
		map[string]any{"kind": "folder", "space_id": "abcdefgh", "path": "a/../b"},
		map[string]any{"kind": "query", "query": map[string]any{"category": "movie;drop", "year": 12}},
		map[string]any{"kind": "nope"},
		map[string]any{"kind": "show", "show_title": "Ok", "order": "sideways", "limit": 0},
	}}).ExpectError(422, "validation_failed")
	fields := r.Map()["error"].(map[string]any)["details"].(map[string]any)["fields"].(map[string]any)
	for _, k := range []string{"name", "items.0.file_id", "items.1.season", "items.2.path", "items.3.query.category",
		"items.3.query.year", "items.4.kind", "items.5.order", "items.5.limit"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("missing field error %q in %v", k, fields)
		}
	}

	var l lineupResp
	do(t, c, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Saturday", "description": "Cartoons\nand more",
		"items": []any{
			map[string]any{"kind": "show", "show_title": "Show A", "label": "A"},
			map[string]any{"kind": "query", "query": map[string]any{"category": "movie, tv", "genre": "Comedy"}, "order": "random", "limit": 5},
			map[string]any{"kind": "folder", "space_id": "abcdefghij", "path": "/Movies/Classics/"},
		}}, 201, &l)
	if l.Name != "Saturday" || l.Description != "Cartoons\nand more" || len(l.Items) != 3 || l.OwnerID != v.adminID {
		t.Fatalf("created lineup = %+v", l)
	}
	if l.Items[1].Query.Category != "movie,tv" || l.Items[1].Order != "random" || *l.Items[1].Limit != 5 {
		t.Errorf("query item = %+v", l.Items[1])
	}
	if *l.Items[2].Path != "Movies/Classics" || !*l.Items[2].Recursive {
		t.Errorf("folder item = %+v", l.Items[2])
	}
	ids := []string{l.Items[0].ID, l.Items[1].ID, l.Items[2].ID}

	// PATCH name/description.
	do(t, c, "PATCH", "/api/v1/tv/lineups/"+l.ID, map[string]any{"name": "Sunday"}, 200, &l)
	if l.Name != "Sunday" || l.Description == "" {
		t.Errorf("patched = %+v", l)
	}

	// PUT reorders and keeps identities of echoed ids; unknown ids get fresh ids.
	do(t, c, "PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": []any{
		map[string]any{"id": ids[2], "kind": "folder", "space_id": "abcdefghij", "path": "Movies"},
		map[string]any{"id": ids[0], "kind": "show", "show_title": "Show A"},
		map[string]any{"id": "notanexistingid", "kind": "file", "file_id": "abcdefghijk"},
	}}, 200, &l)
	if l.Items[0].ID != ids[2] || l.Items[1].ID != ids[0] || l.Items[2].ID == "notanexistingid" {
		t.Errorf("ids after PUT = %v", []string{l.Items[0].ID, l.Items[1].ID, l.Items[2].ID})
	}

	// POST inserts at a position (clamped) and appends.
	do(t, c, "POST", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"position": 1,
		"items": []any{map[string]any{"kind": "season", "show_title": "Show B", "season": 2}}}, 200, &l)
	if len(l.Items) != 4 || l.Items[1].Kind != "season" || *l.Items[1].Season != 2 {
		t.Fatalf("after insert = %+v", l.Items)
	}
	do(t, c, "POST", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"position": 99,
		"items": []any{map[string]any{"kind": "show", "show_title": "Last"}}}, 200, &l)
	if l.Items[4].ShowTitle != "Last" {
		t.Errorf("append = %+v", l.Items[4])
	}

	// DELETE an item.
	do(t, c, "DELETE", "/api/v1/tv/lineups/"+l.ID+"/items/"+l.Items[0].ID, nil, 200, &l)
	if len(l.Items) != 4 || l.Items[0].Kind != "season" {
		t.Errorf("after delete = %+v", l.Items)
	}
	c.Do("DELETE", "/api/v1/tv/lineups/"+l.ID+"/items/zzzzzzzzzz", nil).ExpectError(404, "not_found")

	// The item cap holds for inserts.
	many := make([]any, MaxLineupItems)
	for i := range many {
		many[i] = map[string]any{"kind": "show", "show_title": fmt.Sprintf("S%d", i)}
	}
	c.Do("POST", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": many}).ExpectError(422, "validation_failed")
	c.Do("PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": append(many, many[0])}).ExpectError(422, "validation_failed")
	do(t, c, "PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": many}, 200, &l)
	if len(l.Items) != MaxLineupItems {
		t.Errorf("items = %d", len(l.Items))
	}
	c.Do("PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{}).ExpectError(422, "validation_failed")

	// Paging: name order, opaque cursors.
	for _, n := range []string{"alpha", "Beta", "gamma"} {
		do(t, c, "POST", "/api/v1/tv/lineups", map[string]any{"name": n}, 201, nil)
	}
	var page struct {
		Items      []lineupResp `json:"items"`
		NextCursor *string      `json:"next_cursor"`
	}
	do(t, c, "GET", "/api/v1/tv/lineups?limit=2", nil, 200, &page)
	if len(page.Items) != 2 || page.Items[0].Name != "alpha" || page.Items[1].Name != "Beta" || page.NextCursor == nil {
		t.Fatalf("page 1 = %+v", page)
	}
	do(t, c, "GET", "/api/v1/tv/lineups?limit=2&cursor="+*page.NextCursor, nil, 200, &page)
	if len(page.Items) != 2 || page.Items[0].Name != "gamma" || page.Items[1].Name != "Sunday" || page.NextCursor != nil {
		t.Fatalf("page 2 = %+v", page)
	}
	c.Do("GET", "/api/v1/tv/lineups?cursor=garbage", nil).ExpectError(400, "bad_request")

	// Another user sees none of it.
	_, bob := v.user("bob")
	do(t, bob, "GET", "/api/v1/tv/lineups", nil, 200, &page)
	if len(page.Items) != 0 {
		t.Errorf("bob sees %d lineups", len(page.Items))
	}
	bob.Do("GET", "/api/v1/tv/lineups/"+l.ID, nil).ExpectError(404, "not_found")
	bob.Do("PATCH", "/api/v1/tv/lineups/"+l.ID, map[string]any{"name": "x"}).ExpectError(404, "not_found")
	bob.Do("DELETE", "/api/v1/tv/lineups/"+l.ID, nil).ExpectError(404, "not_found")
	bob.Do("GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil).ExpectError(404, "not_found")

	// Delete.
	c.Do("DELETE", "/api/v1/tv/lineups/"+l.ID, nil).Expect(204)
	c.Do("GET", "/api/v1/tv/lineups/"+l.ID, nil).ExpectError(404, "not_found")
}

type expandedResp struct {
	Items []struct {
		FileID          string  `json:"file_id"`
		Title           string  `json:"title"`
		DurationSeconds float64 `json:"duration_seconds"`
		SourceItemID    string  `json:"source_item_id"`
	} `json:"items"`
	TotalDurationSeconds float64 `json:"total_duration_seconds"`
	Skipped              []struct {
		FileID       string `json:"file_id"`
		Reason       string `json:"reason"`
		SourceItemID string `json:"source_item_id"`
	} `json:"skipped"`
}

func (e expandedResp) titles() []string {
	var out []string
	for _, it := range e.Items {
		out = append(out, it.Title)
	}
	return out
}

func TestExpansion(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	alice, ac := v.user("alice")
	bobID, _ := v.user("bob")

	ep := func(show string, s, e int, title string, d float64) *mediaRow {
		return &mediaRow{Title: title, ShowTitle: show, Season: ip(s), Episode: ip(e), EpisodeTitle: title, Duration: d, Category: "tv", Playable: true}
	}
	// Show episodes created out of order, plus one with an unknown duration.
	e22 := v.addFile(alice, "TV/Show A/Season 2", "a-s02e02.mp4", ep("Show A", 2, 2, "Two-Two", 600))
	e11 := v.addFile(alice, "TV/Show A/Season 1", "a-s01e01.mp4", ep("Show A", 1, 1, "One-One", 1200))
	e21 := v.addFile(alice, "TV/Show A/Season 2", "a-s02e01.mp4", ep("Show A", 2, 1, "Two-One", 900))
	e12 := v.addFile(alice, "TV/Show A/Season 1", "a-s01e02.mp4", ep("Show A", 1, 2, "One-Two", 0))
	// A trashed episode is never scheduled.
	gone := v.addFile(alice, "TV/Show A/Season 1", "a-s01e03.mp4", ep("Show A", 1, 3, "One-Three", 500))
	if err := v.files.Trash(ctx, v.principal(alice), gone); err != nil {
		t.Fatal(err)
	}
	// Movies for queries.
	m1 := v.addFile(alice, "Movies", "Zulu (1964).mp4", &mediaRow{Title: "Zulu", Duration: 8000, Genres: []string{"War", "Drama"}, Year: 1964, Category: "movie"})
	m2 := v.addFile(alice, "Movies", "Alien (1979).mp4", &mediaRow{Title: "Alien", Duration: 7000, Genres: []string{"Horror", "SciFi"}, Year: 1979, Category: "movie"})
	m3 := v.addFile(alice, "Movies", "Airplane (1980).mp4", &mediaRow{Title: "Airplane!", Duration: 5000, Genres: []string{"comedy"}, Year: 1980, Category: "movie"})
	// Folder with natural ordering and a nested folder.
	f2 := v.addFile(alice, "Clips", "Clip 2.mp4", &mediaRow{Title: "Clip 2", Duration: 30})
	f10 := v.addFile(alice, "Clips", "Clip 10.mp4", &mediaRow{Title: "Clip 10", Duration: 30})
	f1 := v.addFile(alice, "Clips/Extra", "Clip 1.mp4", &mediaRow{Title: "Clip 1", Duration: 30})
	v.addFile(alice, "Clips", "notes.txt", nil) // not media: ignored by folder items
	// Bob's file: alice cannot read it.
	bobs := v.addFile(bobID, "", "secret.mp4", &mediaRow{Title: "Secret", Duration: 100, Category: "movie", Genres: []string{"comedy"}})
	// Bob's show with the same title never leaks into alice's show items.
	v.addFile(bobID, "", "bob-a.mp4", ep("Show A", 1, 9, "Bob's", 50))

	home, _ := v.files.HomeSpaceID(ctx, alice)
	var l lineupResp
	do(t, ac, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Mix", "items": []any{
		map[string]any{"kind": "show", "show_title": "show a"},
		map[string]any{"kind": "season", "show_title": "Show A", "season": 2},
		map[string]any{"kind": "query", "query": map[string]any{"category": "movie", "genre": "COMEDY"}},
		map[string]any{"kind": "query", "query": map[string]any{"category": "movie", "year": 1979}},
		map[string]any{"kind": "folder", "space_id": home, "path": "Clips"},
		map[string]any{"kind": "folder", "space_id": home, "path": "Clips", "recursive": false},
		map[string]any{"kind": "folder", "space_id": home, "path": "Nope"},
		map[string]any{"kind": "file", "file_id": bobs},
		map[string]any{"kind": "file", "file_id": m1},
	}}, 201, &l)
	var x expandedResp
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x)
	got := []string{}
	for _, it := range x.Items {
		got = append(got, it.FileID)
	}
	// show (season/episode order, unknown duration skipped), season 2, comedy (case-insensitive
	// genre, Bob's home excluded), 1979, folder recursive (natural path order: Clip 2 < Clip 10 <
	// Extra/Clip 1, notes.txt ignored), folder non-recursive, missing folder, unreadable file,
	// explicit file.
	want := []string{e11, e21, e22, e21, e22, m3, m2, f2, f10, f1, f2, f10, m1}
	if !slices.Equal(got, want) {
		t.Fatalf("expanded order\n got %v\nwant %v\ntitles %v", got, want, x.titles())
	}
	if x.Items[0].Title != "Show A · S01E01 · One-One" {
		t.Errorf("title = %q", x.Items[0].Title)
	}
	wantTotal := 1200.0 + 900 + 600 + 900 + 600 + 5000 + 7000 + 30*5 + 8000
	if x.TotalDurationSeconds != wantTotal {
		t.Errorf("total = %v want %v", x.TotalDurationSeconds, wantTotal)
	}
	reasons := map[string]string{}
	for _, s := range x.Skipped {
		reasons[s.FileID] = s.Reason
	}
	if reasons[e12] != ReasonUnknownDuration || reasons[bobs] != ReasonUnreadable || reasons[l.Items[6].ID] != ReasonNotFound {
		t.Errorf("skipped = %+v", x.Skipped)
	}
	if _, ok := reasons[gone]; ok {
		t.Errorf("trashed file reported: %+v", x.Skipped)
	}
	var lv lineupResp
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID, nil, 200, &lv)
	if lv.ItemCount != len(want) || lv.TotalDurationSeconds != wantTotal {
		t.Errorf("lineup summary = %d / %v", lv.ItemCount, lv.TotalDurationSeconds)
	}

	// Random + limit picks a deterministic subset; the cache is invalidated on file events.
	do(t, ac, "PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": []any{
		map[string]any{"kind": "query", "query": map[string]any{"category": "movie"}, "order": "random", "limit": 2},
	}}, 200, &lv)
	var x1, x2 expandedResp
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x1)
	v.tv.cache.invalidateAll()
	do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x2)
	if len(x1.Items) != 2 || !slices.Equal(x1.titles(), x2.titles()) {
		t.Errorf("random subset not stable: %v vs %v", x1.titles(), x2.titles())
	}

	// A file trashed after expansion disappears once the file event arrives.
	do(t, ac, "PUT", "/api/v1/tv/lineups/"+l.ID+"/items", map[string]any{"items": []any{
		map[string]any{"kind": "file", "file_id": m2}, map[string]any{"kind": "file", "file_id": m3},
	}}, 200, &lv)
	if lv.ItemCount != 2 {
		t.Fatalf("item_count = %d", lv.ItemCount)
	}
	if err := v.files.Trash(ctx, v.principal(alice), m2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var y expandedResp
		do(t, ac, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &y)
		return len(y.Items) == 1 && len(y.Skipped) == 1 && y.Skipped[0].Reason == ReasonUnreadable
	})
}

func TestExpansionAdminOwnerSkipsOtherHomes(t *testing.T) {
	v := newEnv(t)
	bob, _ := v.user("bob")
	mine := v.addFile(v.adminID, "", "mine.mp4", &mediaRow{Title: "Mine", Duration: 60, Category: "movie"})
	bobs := v.addFile(bob, "", "bobs.mp4", &mediaRow{Title: "Bobs", Duration: 60, Category: "movie"})
	var l lineupResp
	do(t, v.adm, "POST", "/api/v1/tv/lineups", map[string]any{"name": "All", "items": []any{
		map[string]any{"kind": "query", "query": map[string]any{"category": "movie"}},
		map[string]any{"kind": "file", "file_id": bobs}, // explicit: admins may read every home
	}}, 201, &l)
	var x expandedResp
	do(t, v.adm, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x)
	if len(x.Items) != 2 || x.Items[0].FileID != mine || x.Items[1].FileID != bobs {
		t.Fatalf("admin expansion = %+v", x.Items)
	}
}

func TestDeleteLineupWithChannels(t *testing.T) {
	v := newEnv(t)
	var l lineupResp
	do(t, v.adm, "POST", "/api/v1/tv/lineups", map[string]any{"name": "L"}, 201, &l)
	var ch Channel
	do(t, v.adm, "POST", "/api/v1/tv/channels", map[string]any{"name": "C", "lineup_id": l.ID}, 201, &ch)
	r := v.adm.Do("DELETE", "/api/v1/tv/lineups/"+l.ID, nil).ExpectError(409, "conflict")
	if !strings.Contains(string(r.Body), ch.ID) {
		t.Errorf("409 details lack the channel: %s", r.Body)
	}
	v.adm.Do("DELETE", "/api/v1/tv/lineups/"+l.ID+"?force=1", nil).Expect(http.StatusNoContent)
	v.adm.Do("GET", "/api/v1/tv/channels/"+ch.ID, nil).ExpectError(404, "not_found")
}
