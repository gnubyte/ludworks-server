package browser

import (
	"bufio"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

var (
	errSyncExpired = core.NewError(http.StatusGone, "gone",
		"This sync point is no longer valid; do a full sync (without `since` or `cursor`) and replace the local copy")
	errSinceAndCursor  = core.BadRequest("Pass either `since` (to start a delta sync) or `cursor` (to continue a sync), not both")
	errCursorSelection = core.BadRequest(
		"This cursor continues a sync of other collections: pass the same `collections` as on the first page")
)

// cursorKind marks browser sync cursors, so a signed cursor of another endpoint is not accepted.
const cursorKind = "browser.sync.v1"

// syncCursor is the signed position of a sync in progress (next_cursor). It names the user and
// the epoch (vault key_id) it belongs to and when it was issued, so it is useless to anyone else,
// after a key change and after CursorLifetime.
type syncCursor struct {
	Kind   string    `json:"k"`
	User   string    `json:"u"`
	Key    string    `json:"e"`           // epoch: vault key_id of the records sent so far
	Delta  bool      `json:"d,omitempty"` // false: full sync
	Start  string    `json:"s"`           // full sync: its start clock S0; delta sync: since
	Stamp  string    `json:"p"`           // position: the last change sent (stamp, collection, id)
	Coll   string    `json:"pc"`
	ID     string    `json:"pi"`
	Sel    selection `json:"c"`
	Issued int64     `json:"t"` // unix milliseconds
}

// pageRequest is one GET /browser/sync request.
type pageRequest struct {
	sel   selection
	limit int
	delta bool
	start string    // delta: since; full: S0 (empty on the first page of a full sync)
	pos   *position // nil on the first page
	key   string    // the epoch the cursor was issued for
}

type position struct{ Stamp, Collection, ID string }

// sync answers GET /browser/sync (API.md §16). Changes come in ascending (stamp, collection, id)
// order, at most limit per page; while more is true, next_cursor continues the same sync and
// server_time is null; the final page carries server_time, the user's change clock read in that
// page's transaction.
//
// Without since the sync is a full sync: every live record, plus tombstones newer than the clock
// S0 read on its first page (records deleted while the pages are fetched). A record changed
// between pages gets a stamp later than every position handed out so far, so it is sent again
// further on (deleted ones as tombstones): nothing is missed and nothing stale survives. With
// since (a previous final server_time) it is a delta sync: every change, tombstones included,
// stamped after since. Stamps strictly increase in commit order, so any change committed after a
// final page's snapshot is newer than its server_time and comes with the next delta.
//
// A since or cursor position before the sync horizon (the data was dropped after a vault key
// change, or tombstones it still needs were purged), after the clock (database restored from a
// backup), sent while no vault exists, a cursor of another epoch or user, or an expired cursor
// gets 410 gone: the client drops its sync point and does a full sync.
//
// Each page is streamed from one read transaction.
func (m *Module) sync(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	sel, err := parseSelection(q.Get("collections"))
	if err != nil {
		return err
	}
	req := pageRequest{sel: sel, limit: httpx.ParseLimit(r, DefaultPageSize, MaxPageSize)}
	since, cur := q.Get("since"), q.Get("cursor")
	switch {
	case since != "" && cur != "":
		return errSinceAndCursor
	case cur != "":
		if err := m.resume(&req, p.UserID, cur); err != nil {
			return err
		}
	case since != "":
		t, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return core.BadRequest("since must be an RFC 3339 timestamp (the server_time of a previous final page)")
		}
		req.delta, req.start = true, db.FormatTime(t)
	}
	started := false
	return m.readEpoch(ctx, p.UserID, func(tx *sql.Tx, st *state) error {
		if started {
			// db.ReadTx retries on SQLITE_BUSY; never append a second document to a response
			// that is already streaming.
			return errors.New("browser: sync retried after the response started")
		}
		if err := req.check(st); err != nil {
			return err
		}
		started = true
		return m.writePage(ctx, w, tx, p.UserID, st, &req)
	})
}

// resume fills req from a next_cursor. A malformed or tampered cursor is a 400; another user's
// cursor or one older than CursorLifetime is a 410 (the epoch is checked against the snapshot).
func (m *Module) resume(req *pageRequest, userID, s string) error {
	var c syncCursor
	if err := httpx.DecodeCursor(s, &c); err != nil || c.Kind != cursorKind || c.Stamp == "" || c.Start == "" {
		return httpx.ErrInvalidCursor
	}
	if subtle.ConstantTimeCompare([]byte(c.User), []byte(userID)) != 1 {
		return errSyncExpired
	}
	if age := m.now().Sub(time.UnixMilli(c.Issued)); age > CursorLifetime || age < -time.Minute {
		return errSyncExpired
	}
	if c.Sel != req.sel {
		return errCursorSelection
	}
	req.delta, req.start, req.key = c.Delta, c.Start, c.Key
	req.pos = &position{Stamp: c.Stamp, Collection: c.Coll, ID: c.ID}
	return nil
}

// check validates the sync point against the snapshot's state (410 when it is no longer valid).
func (req *pageRequest) check(st *state) error {
	if req.pos == nil && !req.delta {
		return nil // a full sync starts here
	}
	if st.KeyID == "" || (req.pos != nil && req.key != st.KeyID) {
		// No vault (never initialised, or reset): no sync point is valid, and answering an empty
		// delta would let a client keep records of a retired key. Likewise a cursor of an
		// earlier epoch.
		return errSyncExpired
	}
	// Every tombstone after base must still exist: purges advance the horizon past the
	// tombstones they remove.
	base := req.start
	if req.pos != nil && req.pos.Stamp > base {
		base = req.pos.Stamp
	}
	if base < st.Horizon || base > st.Clock {
		return errSyncExpired
	}
	return nil
}

// pageQuery builds the page query: rows after the position (or since), in sync order. A full
// sync returns live records and the tombstones newer than its start S0. One row more than the
// limit is read to learn whether more follow.
func pageQuery(userID string, req *pageRequest, start string) (string, []any) {
	var b strings.Builder
	args := []any{userID}
	b.WriteString(`SELECT ` + recordCols + ` FROM browser_records WHERE user_id = ?`)
	if req.pos != nil {
		// The plain range term lets SQLite seek the (user_id, updated_at, collection, id) index.
		b.WriteString(` AND updated_at >= ? AND (updated_at, collection, id) > (?, ?, ?)`)
		args = append(args, req.pos.Stamp, req.pos.Stamp, req.pos.Collection, req.pos.ID)
	}
	switch {
	case !req.delta:
		b.WriteString(` AND (deleted_at IS NULL OR updated_at > ?)`)
		args = append(args, start)
	case req.pos == nil:
		b.WriteString(` AND updated_at > ?`)
		args = append(args, start)
	}
	if f, fa := req.sel.sqlFilter(); f != "" {
		b.WriteString(f)
		args = append(args, fa...)
	}
	b.WriteString(` ORDER BY updated_at, collection, id LIMIT ?`)
	args = append(args, req.limit+1)
	return b.String(), args
}

// writePage streams one page: {"changes":[BC],"more","next_cursor","key_id","server_time"}.
func (m *Module) writePage(ctx context.Context, w http.ResponseWriter, tx *sql.Tx, userID string, st *state, req *pageRequest) error {
	var rows *sql.Rows
	start := req.start
	if st.KeyID != "" {
		if !req.delta && req.pos == nil {
			start = st.Clock // S0: the full sync starts at this snapshot
		}
		query, args := pageQuery(userID, req, start)
		var err error
		if rows, err = tx.QueryContext(ctx, query, args...); err != nil {
			return err
		}
		defer rows.Close()
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriterSize(w, 64<<10)
	s := &stream{w: bw, first: true}
	s.raw(`{"changes":[`)
	n, size, more := 0, int64(0), false
	var last *record
	for rows != nil && rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return err
		}
		// Stop at the limit, or before the record that would take the page past its byte
		// budget (a page always carries at least one change).
		if n == req.limit || (n > 0 && size+rec.Size > int64(lim.pageBytes)) {
			more = true
			break
		}
		s.elem(rec.Change)
		if s.err != nil {
			return s.err
		}
		n++
		size += rec.Size
		last = rec
	}
	if rows != nil {
		if err := rows.Err(); err != nil {
			return err
		}
	}
	// key_id names the vault key every record of this sync is sealed under (null without a
	// vault): a client holding a different key must not use the records.
	var keyID any
	if st.KeyID != "" {
		keyID = st.KeyID
	}
	if more {
		next := httpx.EncodeCursor(syncCursor{Kind: cursorKind, User: userID, Key: st.KeyID, Delta: req.delta,
			Start: start, Stamp: last.UpdatedAt, Coll: last.Collection, ID: last.ID, Sel: req.sel,
			Issued: m.now().UnixMilli()})
		s.raw(`],"more":true,"next_cursor":`)
		s.value(next)
		s.raw(`,"key_id":`)
		s.value(keyID)
		s.raw(`,"server_time":null}`)
	} else {
		s.raw(`],"more":false,"next_cursor":null,"key_id":`)
		s.value(keyID)
		s.raw(`,"server_time":`)
		s.value(st.Clock)
		s.raw("}")
	}
	s.raw("\n")
	if s.err != nil {
		return s.err
	}
	return bw.Flush()
}

// stream writes a JSON document piecewise and remembers the first write error.
type stream struct {
	w     *bufio.Writer
	first bool // the next elem is the first of its array
	err   error
}

func (s *stream) raw(str string) {
	if s.err == nil {
		_, s.err = s.w.WriteString(str)
	}
}

func (s *stream) value(v any) {
	if s.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		s.err = err
		return
	}
	_, s.err = s.w.Write(b)
}

// elem writes one array element, adding the separator after the first.
func (s *stream) elem(v any) {
	if !s.first {
		s.raw(",")
	}
	s.first = false
	s.value(v)
}
