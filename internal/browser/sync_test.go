package browser_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
	"ludworks/internal/browser"
)

// inOrder fails unless the changes are strictly ascending by (updated_at, collection, id).
func inOrder(t *testing.T, changes []bc) {
	t.Helper()
	for i := 1; i < len(changes); i++ {
		a, b := changes[i-1], changes[i]
		if a.UpdatedAt > b.UpdatedAt || (a.UpdatedAt == b.UpdatedAt && (a.Collection > b.Collection ||
			(a.Collection == b.Collection && a.ID >= b.ID))) {
			t.Fatalf("changes out of order at %d: %+v then %+v", i, a, b)
		}
	}
}

// A full sync over several pages while records change between pages: the result equals the
// server's final state — nothing missed (changed records are sent again later), nothing stale
// kept (records deleted after the sync started arrive as tombstones) — and tombstones from
// before the sync are never listed.
func TestFullSyncPagingWithChangesBetweenPages(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	for g := 0; g < 3; g++ { // three requests: three stamps, ten records each
		var batch []map[string]any
		for i := 0; i < 10; i++ {
			batch = append(batch, chg([]string{"bookmarks", "history", "tabs"}[i%3], fmt.Sprintf("g%d-%02d", g, i), sealed(20), nil))
		}
		write(t, c, key, batch...)
	}
	writeOne(t, c, key, chg("history", "deleted-before", sealed(5), nil))
	writeOne(t, c, key, chg("history", "deleted-before", nil, nil))

	local := replica{}
	var stream []bc
	p := getPage(t, c, syncQuery("", "", 7, ""))
	if len(p.Changes) != 7 || !p.More || p.KeyID == nil || *p.KeyID != key {
		t.Fatalf("first page: %+v", p)
	}
	local.apply(p.Changes)
	stream = append(stream, p.Changes...)
	// Two records already sent (one to update, one to delete) and two of the last group, not
	// sent yet (likewise).
	sent, sentDeleted := p.Changes[0], p.Changes[6]
	notYet, notYetDeleted := "tabs/g2-08", "bookmarks/g2-09"
	if _, ok := local[notYet]; ok {
		t.Fatal("fixture: record already sent")
	}

	// Between the pages: update and delete sent and unsent records, create a new one.
	write(t, c, key,
		chg(sent.Collection, sent.ID, sealed(30), sent.Revision),
		chg("tabs", "g2-08", sealed(30), 1),
		chg(sentDeleted.Collection, sentDeleted.ID, nil, nil),
		chg("bookmarks", "g2-09", nil, 1),
		chg("prefs", "prefs", sealed(15), 0),
	)
	cursor := *p.NextCursor
	for pages := 1; ; pages++ {
		p = getPage(t, c, syncQuery("", "", 7, cursor))
		local.apply(p.Changes)
		stream = append(stream, p.Changes...)
		if !p.More {
			break
		}
		cursor = *p.NextCursor
		if pages == 2 { // and once more in the middle of the sync
			writeOne(t, c, key, chg("history", "late", sealed(5), nil))
		}
	}
	inOrder(t, stream)
	sameReplica(t, local, truth(t, c))
	if local[sent.key()].Revision != sent.Revision+1 || local[notYet].Revision != 2 {
		t.Fatalf("updated records: %+v %+v", local[sent.key()], local[notYet])
	}
	if _, ok := local[sentDeleted.key()]; ok {
		t.Fatal("a record deleted during the sync survived")
	}
	if _, ok := local[notYetDeleted]; ok {
		t.Fatal("a record deleted before it was sent survived")
	}
	for _, ch := range stream {
		if ch.ID == "deleted-before" {
			t.Fatalf("a full sync listed a tombstone from before it started: %+v", ch)
		}
	}
	// The final server_time is a valid sync point: nothing is pending.
	if d := getPage(t, c, syncQuery(*p.ServerTime, "", 0, "")); len(d.Changes) != 0 || *d.ServerTime != *p.ServerTime {
		t.Fatalf("delta after the full sync: %+v", d)
	}
}

func TestDeltaSync(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	write(t, c, key, chg("bookmarks", "a", sealed(5), nil), chg("bookmarks", "b", sealed(5), nil), chg("history", "c", sealed(5), nil))
	s1, _, _ := syncAll(t, c, "", "", 0, replica{})

	// Idle: empty delta, same server_time.
	if d := getPage(t, c, syncQuery(s1, "", 0, "")); len(d.Changes) != 0 || *d.ServerTime != s1 {
		t.Fatalf("idle delta: %+v", d)
	}
	writeOne(t, c, key, chg("bookmarks", "a", sealed(6), 1))
	writeOne(t, c, key, chg("bookmarks", "b", nil, 1))
	w := writeOne(t, c, key, chg("prefs", "prefs", sealed(6), 0))
	d := getPage(t, c, syncQuery(s1, "", 0, ""))
	if len(d.Changes) != 3 || *d.ServerTime != w.UpdatedAt {
		t.Fatalf("delta: %+v", d)
	}
	inOrder(t, d.Changes)
	if d.Changes[0].key() != "bookmarks/a" || d.Changes[0].Revision != 2 || d.Changes[1].key() != "bookmarks/b" ||
		d.Changes[1].Data != nil || d.Changes[1].Revision != 2 || d.Changes[2].key() != "prefs/prefs" {
		t.Fatalf("delta changes: %+v", d.Changes)
	}
	// A higher-precision since equal to the stamp is accepted (truncated to ms).
	if d := getPage(t, c, syncQuery(strings.TrimSuffix(w.UpdatedAt, "Z")+"999Z", "", 0, "")); len(d.Changes) != 0 {
		t.Fatalf("sub-ms since: %+v", d)
	}

	// Paged delta: changes made in separate requests, three per page. A record created and then
	// deleted after since comes once, as its tombstone.
	s2, local := *d.ServerTime, truth(t, c)
	for i := 0; i < 8; i++ {
		writeOne(t, c, key, chg("history", fmt.Sprintf("p%d", i), sealed(5), nil))
	}
	writeOne(t, c, key, chg("history", "p3", nil, nil))
	final, pages, stream := syncAll(t, c, s2, "", 3, local)
	if pages != 3 || len(stream) != 8 || stream[7].ID != "p3" || stream[7].Data != nil {
		t.Fatalf("paged delta: %d pages, %+v", pages, stream)
	}
	inOrder(t, stream)
	sameReplica(t, local, truth(t, c))
	if final <= s2 {
		t.Fatalf("server_time did not advance: %s", final)
	}

	// Invalid sync points.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(future, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	c.Do("GET", "/api/v1/browser/sync?since=yesterday", nil).ExpectError(http.StatusBadRequest, "bad_request")
	p := getPage(t, c, syncQuery("", "", 1, ""))
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(s1, "", 1, *p.NextCursor), nil).ExpectError(http.StatusBadRequest, "bad_request")
}

func TestCursorValidation(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	write(t, c, key, chg("bookmarks", "a", sealed(5), nil), chg("bookmarks", "b", sealed(5), nil), chg("prefs", "prefs", sealed(5), nil))
	p := getPage(t, c, syncQuery("", "bookmarks", 1, ""))
	cur := *p.NextCursor
	get := func(q string) *authtest.Response { return c.Do("GET", "/api/v1/browser/sync?"+q, nil) }

	// Tampered or foreign-format cursors are malformed requests.
	tampered := cur[:len(cur)-2] + map[bool]string{true: "AA", false: "BB"}[!strings.HasSuffix(cur, "AA")]
	get(syncQuery("", "bookmarks", 1, tampered)).ExpectError(http.StatusBadRequest, "bad_request")
	get(syncQuery("", "bookmarks", 1, "not-a-cursor")).ExpectError(http.StatusBadRequest, "bad_request")
	// The same collections must be passed again.
	get(syncQuery("", "", 1, cur)).ExpectError(http.StatusBadRequest, "bad_request")
	get(syncQuery("", "bookmarks,prefs", 1, cur)).ExpectError(http.StatusBadRequest, "bad_request")
	// Cursors are valid for an hour.
	start := time.Now()
	e.B.SetClock(func() time.Time { return start.Add(browser.CursorLifetime + time.Minute) })
	get(syncQuery("", "bookmarks", 1, cur)).ExpectError(http.StatusGone, "gone")
	e.B.SetClock(func() time.Time { return start.Add(browser.CursorLifetime - time.Minute) })
	p2 := getPage(t, c, syncQuery("", "bookmarks", 1, cur))
	if len(p2.Changes) != 1 || p2.Changes[0].ID != "b" || p2.More || p2.ServerTime == nil {
		t.Fatalf("continuation: %+v", p2)
	}
	e.B.SetClock(time.Now)
	// A cursor survives the response that used it (a retried request gets the same page).
	if p3 := getPage(t, c, syncQuery("", "bookmarks", 1, cur)); len(p3.Changes) != 1 || p3.Changes[0].ID != "b" {
		t.Fatalf("repeated continuation: %+v", p3)
	}
	// Another user's cursor is no sync point (covered with a real second user in the isolation
	// test); a cursor of a previous vault key neither (key change test).
}

func TestCollectionsFilter(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	var batch []map[string]any
	for i := 0; i < 4; i++ {
		for _, coll := range []string{"bookmarks", "history", "tabs", "prefs"} {
			batch = append(batch, chg(coll, fmt.Sprintf("r%d", i), sealed(5), nil))
		}
	}
	write(t, c, key, batch...)
	local := replica{}
	final, pages, _ := syncAll(t, c, "", "bookmarks,prefs", 3, local)
	if pages != 3 || len(local) != 8 {
		t.Fatalf("filtered full sync: %d pages, %d records", pages, len(local))
	}
	for k := range local {
		if !strings.HasPrefix(k, "bookmarks/") && !strings.HasPrefix(k, "prefs/") {
			t.Fatalf("unselected record %s", k)
		}
	}
	// A delta of the selection only sees the selection's changes.
	write(t, c, key, chg("history", "r0", sealed(6), nil), chg("prefs", "r0", nil, nil))
	d := getPage(t, c, syncQuery(final, " prefs , bookmarks ", 0, ""))
	if len(d.Changes) != 1 || d.Changes[0].key() != "prefs/r0" || d.Changes[0].Data != nil {
		t.Fatalf("filtered delta: %+v", d)
	}
	for _, bad := range []string{"cookies", "bookmarks,", "bookmarks,,prefs", "Bookmarks"} {
		c.Do("GET", "/api/v1/browser/sync?"+syncQuery("", bad, 0, ""), nil).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	}
	// Repeated names are fine.
	if p := getPage(t, c, syncQuery("", "tabs,tabs", 0, "")); len(p.Changes) != 4 {
		t.Fatalf("tabs only: %+v", p)
	}
}

func TestTombstonePurgeAdvancesHorizon(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	write(t, c, key, chg("bookmarks", "keep", sealed(5), nil), chg("bookmarks", "gone", sealed(5), nil))
	before, _, _ := syncAll(t, c, "", "", 0, replica{})
	writeOne(t, c, key, chg("history", "x1", sealed(5), nil))
	writeOne(t, c, key, chg("history", "x2", sealed(5), nil))
	writeOne(t, c, key, chg("bookmarks", "gone", nil, nil))
	after, _, _ := syncAll(t, c, "", "", 0, replica{})
	// A delta sync in progress: its first page (x1) is fetched, the tombstone is still ahead.
	mid := getPage(t, c, syncQuery(before, "", 1, ""))
	if !mid.More || len(mid.Changes) != 1 || mid.Changes[0].ID != "x1" {
		t.Fatalf("fixture: %+v", mid)
	}

	if n, err := browser.PurgeTombstonesForTest(e.B, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("purged %d young tombstones, %v", n, err)
	}
	n, err := browser.PurgeTombstonesForTest(e.B, time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	// A client that synced before the purged deletion must resync from scratch, also in the
	// middle of a sync that still needed the tombstone...
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(before, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery("", "", 1, *mid.NextCursor), nil).ExpectError(http.StatusGone, "gone")
	// ...one that already saw it continues normally.
	if d := getPage(t, c, syncQuery(after, "", 0, "")); len(d.Changes) != 0 {
		t.Fatalf("delta after purge: %+v", d)
	}
	if live, dead := e.rows(t, e.userID(t, authtest.AdminUser)); live != 3 || dead != 0 {
		t.Fatalf("after purge: %d live, %d tombstones", live, dead)
	}
	// The purged id can be created again from scratch.
	if r := writeOne(t, c, key, chg("bookmarks", "gone", sealed(5), 0)); r.Status != "ok" || r.Revision != 1 {
		t.Fatalf("re-create after purge: %+v", r)
	}
}

// Pages stop early when their records are large, and always make progress.
func TestPageByteBudgetAndStreaming(t *testing.T) {
	restore := browser.SetPageBytesForTest(5000)
	defer restore()
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	var batch []map[string]any
	for i := 0; i < 10; i++ {
		batch = append(batch, chg("history", fmt.Sprintf("h%d", i), sealed(1000), nil)) // 1376 characters
	}
	batch = append(batch, chg("tabs", "big", sealed(8000), nil)) // larger than the budget on its own
	write(t, c, key, batch...)
	r := c.Do("GET", "/api/v1/browser/sync", nil).Expect(http.StatusOK)
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", r.Header)
	}
	var first page
	r.JSON(&first)
	if len(first.Changes) != 3 || !first.More {
		t.Fatalf("first page under a 5000-byte budget: %d changes", len(first.Changes))
	}
	local := replica{}
	_, pages, stream := syncAll(t, c, "", "", 0, local)
	if len(local) != 11 || pages != 5 {
		t.Fatalf("%d records in %d pages", len(local), pages)
	}
	inOrder(t, stream)
}

// Stamps are strictly increasing per user even when the wall clock stands still or jumps back,
// and every change of one request shares its stamp.
func TestStampsMonotonic(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	frozen := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	e.B.SetClock(func() time.Time { return frozen })
	var last string
	for i := 0; i < 5; i++ {
		w := write(t, c, key, chg("history", fmt.Sprintf("a%d", i), sealed(5), nil), chg("history", fmt.Sprintf("b%d", i), sealed(5), nil))
		if w.Results[0].UpdatedAt <= last || w.Results[1].UpdatedAt != w.Results[0].UpdatedAt {
			t.Fatalf("stamps %s %s after %s", w.Results[0].UpdatedAt, w.Results[1].UpdatedAt, last)
		}
		last = w.Results[0].UpdatedAt
	}
	frozen = frozen.Add(-time.Hour)
	if r := writeOne(t, c, key, chg("history", "back", sealed(5), nil)); r.UpdatedAt <= last {
		t.Fatalf("clock went back: %s <= %s", r.UpdatedAt, last)
	}
}

// Writers racing with paged delta syncs: the union of all deltas equals the final state.
func TestSyncNeverMissesConcurrentWrites(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	local := replica{}
	since, _, _ := syncAll(t, c, "", "", 0, local)

	const writers, requests = 3, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < requests; i++ {
				changes := []map[string]any{
					chg("history", fmt.Sprintf("w%d-%d", w, i), sealed(20), nil),
					chg("bookmarks", fmt.Sprintf("w%d", w), sealed(20), nil),
				}
				if i%4 == 3 {
					changes = append(changes, chg("history", fmt.Sprintf("w%d-%d", w, i-1), nil, nil))
				}
				r := c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": key, "changes": changes})
				if r.Status != http.StatusOK {
					t.Errorf("write: %d %s", r.Status, r.Body)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	syncs := 0
	for running := true; running; syncs++ {
		select {
		case <-done:
			running = false
		default:
		}
		since, _, _ = syncAll(t, c, since, "", 4, local)
	}
	sameReplica(t, local, truth(t, c))
	if want := writers * (requests - requests/4 + 1); len(local) != want {
		t.Fatalf("%d records, want %d", len(local), want)
	}
	t.Logf("%d delta syncs raced with the writers", syncs-1)
}
