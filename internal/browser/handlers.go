package browser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// Result statuses of POST /browser/records.
const (
	statusOK       = "ok"
	statusConflict = "conflict"
	statusError    = "error"
)

// Result reports one change of POST /browser/records (BR), in request order.
type Result struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	// Revision and UpdatedAt describe the record after an applied change (for a delete of a
	// record that is already deleted: its tombstone; absent when the id never existed).
	Revision  int64  `json:"revision,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Current is the server's copy on a conflict: the live record, its tombstone, or null when
	// the id never existed. Absent otherwise.
	Current json.RawMessage `json:"current,omitempty"`
	Error   *ResultError    `json:"error,omitempty"`
}

// ResultError says why a change was not applied (status "error").
type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type writeInput struct {
	KeyID string `json:"key_id"`
	// Changes are decoded one by one (parseChange), so a malformed change is reported on its
	// own instead of failing the whole request.
	Changes []json.RawMessage `json:"changes"`
}

type writeOutput struct {
	Results    []Result `json:"results"`
	ServerTime string   `json:"server_time"`
}

var errTooManyChanges = core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
	fmt.Sprintf("A request may carry at most %d changes", MaxBatch))

// writeRecords applies POST /browser/records: up to MaxBatch changes, in order, in one
// transaction. Each change is reported separately (ok, conflict with the server's copy, or
// error); a conflict or error does not stop the others. All changes applied by one request
// share its stamp.
func (m *Module) writeRecords(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in writeInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Changes == nil {
		return core.Validation(map[string]string{"changes": "is required"})
	}
	if len(in.Changes) > MaxBatch {
		return errTooManyChanges
	}
	if err := checkKeyID(in.KeyID); err != nil {
		return err
	}
	changes := make([]change, len(in.Changes))
	for i, raw := range in.Changes {
		changes[i] = parseChange(raw)
	}
	var out writeOutput
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		out = writeOutput{Results: make([]Result, 0, len(changes))}
		st, err := m.writeEpoch(ctx, tx, p.UserID, in.KeyID)
		if err != nil {
			return err
		}
		b, err := m.newBatch(ctx, tx, p.UserID, st)
		if err != nil {
			return err
		}
		defer b.close()
		for i := range changes {
			res, err := b.apply(ctx, &changes[i])
			if err != nil {
				return err
			}
			out.Results = append(out.Results, res)
		}
		out.ServerTime = st.Clock
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// batch applies the changes of one write transaction.
type batch struct {
	m      *Module
	tx     *sql.Tx
	userID string
	st     *state
	stamp  string // the transaction's stamp, taken on the first applied change
	count  int64  // live records
	bytes  int64  // their total size

	get, insert, update, remove *sql.Stmt
}

func (m *Module) newBatch(ctx context.Context, tx *sql.Tx, userID string, st *state) (*batch, error) {
	b := &batch{m: m, tx: tx, userID: userID, st: st}
	var err error
	if b.count, b.bytes, err = liveUsage(ctx, tx, userID); err != nil {
		return nil, err
	}
	stmts := []struct {
		dst **sql.Stmt
		sql string
	}{
		{&b.get, `SELECT ` + recordCols + ` FROM browser_records WHERE user_id = ? AND collection = ? AND id = ?`},
		{&b.insert, `INSERT INTO browser_records (user_id, collection, id, data, size, revision, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?)`},
		// Re-creating a deleted id continues its revisions, so a writer holding a revision from
		// before the deletion still gets a conflict.
		{&b.update, `UPDATE browser_records SET data = ?1, size = ?2, revision = revision + 1, updated_at = ?3,
			created_at = CASE WHEN deleted_at IS NULL THEN created_at ELSE ?3 END, deleted_at = NULL
			WHERE user_id = ?4 AND collection = ?5 AND id = ?6`},
		{&b.remove, `UPDATE browser_records SET data = NULL, size = 0, revision = revision + 1, deleted_at = ?1, updated_at = ?1
			WHERE user_id = ?2 AND collection = ?3 AND id = ?4`},
	}
	for _, s := range stmts {
		if *s.dst, err = tx.PrepareContext(ctx, s.sql); err != nil {
			b.close()
			return nil, err
		}
	}
	return b, nil
}

func (b *batch) close() {
	for _, s := range []*sql.Stmt{b.get, b.insert, b.update, b.remove} {
		if s != nil {
			s.Close()
		}
	}
}

// nextStamp returns the transaction's stamp, advancing the clock on first use (a request that
// changes nothing leaves the clock alone).
func (b *batch) nextStamp(ctx context.Context) (string, error) {
	if b.stamp == "" {
		stamp, err := b.m.advance(ctx, b.tx, b.userID, b.st)
		if err != nil {
			return "", err
		}
		b.stamp = stamp
	}
	return b.stamp, nil
}

func (b *batch) record(ctx context.Context, collection, id string) (*record, error) {
	r, err := scanRecord(b.get.QueryRowContext(ctx, b.userID, collection, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func quotaResult(res Result, msg string) Result {
	res.Status, res.Error = statusError, &ResultError{Code: codeQuota, Message: msg}
	return res
}

// conflictResult reports a base_revision mismatch with the server's copy (null: never existed).
func conflictResult(res Result, cur *record) (Result, error) {
	res.Status, res.Current = statusConflict, json.RawMessage("null")
	if cur != nil {
		b, err := json.Marshal(cur.Change)
		if err != nil {
			return res, err
		}
		res.Current = b
	}
	return res, nil
}

// apply applies one change (API.md §16 Writes). base_revision null writes unconditionally,
// 0 creates (the record must not be live; a deleted id may be re-created), n > 0 updates or
// deletes only the live record at revision n. Deleting a record that does not exist or is
// already deleted is ok and changes nothing, whatever base_revision says: the client's intent
// is already the server's state. Only changes that add data are checked against the quotas.
func (b *batch) apply(ctx context.Context, ch *change) (Result, error) {
	res := Result{Collection: clip(ch.Collection, 64), ID: clip(ch.ID, 64)}
	if ch.errCode != "" {
		res.Status, res.Error = statusError, &ResultError{Code: ch.errCode, Message: ch.errMsg}
		return res, nil
	}
	cur, err := b.record(ctx, ch.Collection, ch.ID)
	if err != nil {
		return res, err
	}
	live := cur != nil && !cur.Deleted

	if ch.Data == nil { // delete
		if !live {
			res.Status = statusOK
			if cur != nil {
				res.Revision, res.UpdatedAt = cur.Revision, cur.UpdatedAt
			}
			return res, nil
		}
		if ch.Base != nil && *ch.Base != cur.Revision {
			return conflictResult(res, cur)
		}
		stamp, err := b.nextStamp(ctx)
		if err != nil {
			return res, err
		}
		if _, err := b.remove.ExecContext(ctx, stamp, b.userID, ch.Collection, ch.ID); err != nil {
			return res, err
		}
		b.count--
		b.bytes -= cur.Size
		res.Status, res.Revision, res.UpdatedAt = statusOK, cur.Revision+1, stamp
		return res, nil
	}

	switch {
	case ch.Base == nil: // last writer wins
	case *ch.Base == 0:
		if live {
			return conflictResult(res, cur)
		}
	case !live || cur.Revision != *ch.Base:
		return conflictResult(res, cur)
	}
	size := int64(len(*ch.Data))
	if live {
		if grow := size - cur.Size; grow > 0 && b.bytes+grow > lim.bytes {
			return quotaResult(res, b.bytesMessage()), nil
		}
	} else {
		if b.count+1 > lim.records {
			return quotaResult(res, fmt.Sprintf("Browser sync is limited to %d records", lim.records)), nil
		}
		if b.bytes+size > lim.bytes {
			return quotaResult(res, b.bytesMessage()), nil
		}
	}
	stamp, err := b.nextStamp(ctx)
	if err != nil {
		return res, err
	}
	rev := int64(1)
	if cur == nil {
		_, err = b.insert.ExecContext(ctx, b.userID, ch.Collection, ch.ID, *ch.Data, size, stamp, stamp)
	} else {
		rev = cur.Revision + 1
		_, err = b.update.ExecContext(ctx, *ch.Data, size, stamp, b.userID, ch.Collection, ch.ID)
	}
	if err != nil {
		return res, err
	}
	if live {
		b.bytes += size - cur.Size
	} else {
		b.count++
		b.bytes += size
	}
	res.Status, res.Revision, res.UpdatedAt = statusOK, rev, stamp
	return res, nil
}

func (b *batch) bytesMessage() string {
	return fmt.Sprintf("Browser sync is limited to %d MiB of record data", lim.bytes>>20)
}

// deleteAll answers DELETE /browser/records: every live record (of the selected collections)
// becomes a tombstone with one new stamp, so other devices receive the deletions on sync.
func (m *Module) deleteAll(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	keyID := q.Get("key_id")
	if err := checkKeyID(keyID); err != nil {
		return err
	}
	sel, err := parseSelection(q.Get("collections"))
	if err != nil {
		return err
	}
	var out struct {
		Deleted    int64  `json:"deleted"`
		ServerTime string `json:"server_time"`
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		out.Deleted = 0
		st, err := m.writeEpoch(ctx, tx, p.UserID, keyID)
		if err != nil {
			return err
		}
		filter, fargs := sel.sqlFilter()
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM browser_records WHERE user_id = ? AND deleted_at IS NULL`+filter,
			append([]any{p.UserID}, fargs...)...).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			stamp, err := m.advance(ctx, tx, p.UserID, st)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE browser_records SET data = NULL, size = 0, revision = revision + 1,
				deleted_at = ?, updated_at = ? WHERE user_id = ? AND deleted_at IS NULL`+filter,
				append([]any{stamp, stamp, p.UserID}, fargs...)...)
			if err != nil {
				return err
			}
			out.Deleted, _ = res.RowsAffected()
		}
		out.ServerTime = st.Clock
		return nil
	})
	if err != nil {
		return err
	}
	if out.Deleted > 0 {
		m.d.Audit.Record(ctx, "browser.delete_all", "user", p.UserID,
			map[string]any{"deleted": out.Deleted, "collections": sel.names()})
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// CollectionUsage is one collection's live records and their total size.
type CollectionUsage struct {
	Name    string `json:"name"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
}

// Limits are the per-user limits of browser sync.
type Limits struct {
	Records     int64 `json:"records"`
	Bytes       int64 `json:"bytes"`
	RecordBytes int64 `json:"record_bytes"`
	Batch       int   `json:"batch"`
}

// Usage is GET /browser/usage.
type Usage struct {
	Collections []CollectionUsage `json:"collections"`
	Limits      Limits            `json:"limits"`
}

// usage answers GET /browser/usage: live records and bytes per collection (every collection is
// listed, in canonical order) and the limits.
func (m *Module) usage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var out Usage
	err := m.readEpoch(ctx, p.UserID, func(tx *sql.Tx, st *state) error {
		out = Usage{Limits: Limits{Records: lim.records, Bytes: lim.bytes, RecordBytes: MaxRecordBytes, Batch: MaxBatch}}
		byName := map[string]CollectionUsage{}
		rows, err := tx.QueryContext(ctx, `SELECT collection, count(*), COALESCE(SUM(size), 0) FROM browser_records
			WHERE user_id = ? AND deleted_at IS NULL GROUP BY collection`, p.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u CollectionUsage
			if err := rows.Scan(&u.Name, &u.Records, &u.Bytes); err != nil {
				return err
			}
			byName[u.Name] = u
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range collections {
			u := byName[c]
			u.Name = c
			out.Collections = append(out.Collections, u)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}
