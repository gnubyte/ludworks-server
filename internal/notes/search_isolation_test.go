package notes_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ludworks/internal/notes"
)

// TestSearchRankIsolatedFromOtherUsers is the regression test for the cross-user
// document-frequency oracle: bm25 used to be computed over one server-wide FTS5 index, so
// another user's notes changed both the order of a user's own search results and the score
// carried in next_cursor. Nothing a user observes about their own search may change when a
// different user adds or removes notes.
func TestSearchRankIsolatedFromOtherUsers(t *testing.T) {
	e := newEnv(t)
	mallory, _ := e.user(t, "mallory")
	alice, _ := e.user(t, "alice")

	// Mallory's own corpus: two notes with a probe word, a probe/calibration pair whose
	// relative order depends on the words' document frequencies, and fillers.
	create(t, mallory, map[string]any{"body": "Zebracorp kickoff\nnotes about zebracorp"})
	create(t, mallory, map[string]any{"body": "Zebracorp budget\nzebracorp numbers"})
	x := create(t, mallory, map[string]any{"body": "Xenon\nquokka quokka quokka wombat"})
	y := create(t, mallory, map[string]any{"body": "Yttrium\nwombat wombat wombat quokka"})
	create(t, mallory, map[string]any{"body": "Calibration one\nwombat"})
	create(t, mallory, map[string]any{"body": "Calibration two\nwombat"})
	for i := 0; i < 6; i++ {
		create(t, mallory, map[string]any{"body": fmt.Sprintf("Filler %d\nunrelated words here", i)})
	}

	queries := []string{"zebracorp", "zebr", "quokka wombat", "wombat quokk"}
	observe := func() map[string]string {
		out := map[string]string{}
		for _, q := range queries {
			qs := "?q=" + url.QueryEscape(q)
			var ids []string
			for _, n := range listAll(t, mallory, qs) {
				ids = append(ids, n.ID)
			}
			first := list(t, mallory, qs+"&limit=1")
			cur := ""
			if first.NextCursor != nil {
				cur = *first.NextCursor
			}
			out[q] = strings.Join(ids, ",") + " | " + cur
		}
		return out
	}
	before := observe()
	if !strings.HasPrefix(before["quokka wombat"], x.ID+","+y.ID) {
		t.Fatalf("baseline order: want the quokka-heavy note first, got %s", before["quokka wombat"])
	}
	for _, q := range []string{"zebracorp", "zebr"} {
		if !strings.Contains(before[q], " | ") || strings.HasSuffix(before[q], " | ") {
			t.Fatalf("%q: expected a next_cursor with limit=1: %s", q, before[q])
		}
	}

	// Another user adds notes with the same words, words sharing the prefix, and long
	// unrelated notes (changing row count, document frequencies and average lengths).
	var created []string
	for i := 0; i < 10; i++ {
		created = append(created, create(t, alice, map[string]any{"body": "Quokka sighting\nquokka zebracorp zebrafish"}).ID)
	}
	for i := 0; i < 5; i++ {
		created = append(created, create(t, alice, map[string]any{"body": "Long\n" + strings.Repeat("lorem ipsum dolor ", 200)}).ID)
	}
	after := observe()
	for _, q := range queries {
		if before[q] != after[q] {
			t.Errorf("q=%q: another user's notes changed this user's search\nbefore %s\nafter  %s", q, before[q], after[q])
		}
	}

	// ... and removes them again (permanently).
	for _, id := range created {
		alice.Do("DELETE", "/api/v1/notes/"+id+"?permanent=1", nil).Expect(http.StatusNoContent)
	}
	if final := observe(); fmt.Sprint(final) != fmt.Sprint(before) {
		t.Errorf("after the other user's deletions:\nbefore %v\nafter  %v", before, final)
	}
}

// TestSearchIndexLazyBuild covers notes written before per-user indexes existed (and a
// table lost for any other reason): the first search or write rebuilds the user's index
// from the notes table, and a user without notes gets no table from a search.
func TestSearchIndexLazyBuild(t *testing.T) {
	e := newEnv(t)
	c, uid := e.user(t, "carol")
	tbl := notes.FTSTable(uid)
	exists := func() bool {
		t.Helper()
		var n int
		if err := e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	drop := func() {
		t.Helper()
		if _, err := e.Deps.DB.SQL.Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Deps.DB.SQL.Exec(`DELETE FROM notes_search_indexes WHERE user_id = ?`, uid); err != nil {
			t.Fatal(err)
		}
	}

	if p := list(t, c, "?q=anything"); len(p.Items) != 0 || exists() {
		t.Fatalf("search without notes: %d items, table created: %v", len(p.Items), exists())
	}
	a := create(t, c, map[string]any{"title": "Heron", "body": "grey heron by the lake", "tags": []string{"birds"}})
	b := create(t, c, map[string]any{"body": "Kingfisher\nblue flash over the lake"})
	if !exists() {
		t.Fatal("first note did not create the search table")
	}

	// Search rebuilds a missing table, including explicit titles and tags.
	drop()
	if p := list(t, c, "?q=lake"); len(p.Items) != 2 {
		t.Fatalf("after rebuild by search: %d items", len(p.Items))
	}
	if p := list(t, c, "?q=heron&tag=birds"); len(p.Items) != 1 || p.Items[0].ID != a.ID || len(p.Items[0].Highlights.Title) == 0 {
		t.Fatalf("explicit title/tag after rebuild: %+v", p.Items)
	}

	// An update rebuilds it too, and the updated text replaces the old one exactly once.
	drop()
	c.Do("PATCH", "/api/v1/notes/"+b.ID, map[string]any{"body": "Kingfisher\nperched on the jetty"}).Expect(http.StatusOK)
	if p := list(t, c, "?q=jetty"); len(p.Items) != 1 || p.Items[0].ID != b.ID {
		t.Fatalf("updated note: %+v", p.Items)
	}
	if p := list(t, c, "?q=flash"); len(p.Items) != 0 {
		t.Fatal("stale text still indexed")
	}
	if p := list(t, c, "?q=lake"); len(p.Items) != 1 {
		t.Fatalf("lake after update: %d", len(p.Items))
	}

	// A permanent delete with no table is fine and the note never comes back.
	drop()
	c.Do("DELETE", "/api/v1/notes/"+a.ID+"?permanent=1", nil).Expect(http.StatusNoContent)
	if p := list(t, c, "?q=heron"); len(p.Items) != 0 {
		t.Fatal("deleted note found")
	}
	// A new note after a lost table is indexed once (no duplicate rowid from the rebuild).
	drop()
	create(t, c, map[string]any{"body": "Heron again\nat the lake"})
	if p := list(t, c, "?q=lake"); len(p.Items) != 1 {
		t.Fatalf("new note after rebuild: %d", len(p.Items))
	}
}

// TestSearchIndexSweep drops the search table of an account that disappeared without the
// user-deleted event being handled.
func TestSearchIndexSweep(t *testing.T) {
	e := newEnv(t)
	c, uid := e.user(t, "dora")
	keep, keepID := e.user(t, "erin")
	create(t, c, map[string]any{"body": "dora's private words"})
	create(t, keep, map[string]any{"body": "erin's words"})
	if _, err := e.Auth.DeleteUser(context.Background(), mustUserID(t, e, e.admin), uid); err != nil {
		t.Fatal(err)
	}
	// No UserDeleted event: only the sweep can clean up.
	if err := e.notes.SweepIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := func(id string) (n int) {
		t.Helper()
		if err := e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name LIKE ?`, notes.FTSTable(id)+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(uid); n != 0 {
		t.Fatalf("deleted user's search tables left: %d", n)
	}
	if n := count(keepID); n == 0 {
		t.Fatal("live user's search table dropped")
	}
	if p := list(t, keep, "?q=words"); len(p.Items) != 1 {
		t.Fatalf("live user's search: %d", len(p.Items))
	}
}
