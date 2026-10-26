package vault

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

var errSyncExpired = core.NewError(http.StatusGone, "gone",
	"This sync point is no longer valid; sync again without `since` to get the full vault")

// sync answers GET /vault/sync?since=. Without since it returns every live folder and item;
// with since (a previous server_time) it returns records changed after it plus the ids of
// records deleted after it.
//
// server_time is the user's change clock (the stamp of the newest change visible in this
// snapshot). Stamps strictly increase in commit order, so any change committed after this
// snapshot has a stamp greater than server_time and is returned by the next delta sync: no
// change is ever missed, even while writes race with the sync. A since that predates the sync
// horizon (vault re-initialised, or tombstones purged), lies after the clock, or is sent while
// no vault exists (before init or after a reset) gets 410 gone,
// and clients fall back to a full sync.
//
// The response is streamed from one read transaction, so large vaults never sit in memory.
func (m *Module) sync(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	since := ""
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return core.BadRequest("since must be an RFC 3339 timestamp (a previous server_time)")
		}
		since = db.FormatTime(t)
	}
	started := false
	return m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		if started {
			// db.ReadTx retries on SQLITE_BUSY; never append a second document to a
			// response that is already streaming.
			return errors.New("vault: sync retried after the response started")
		}
		k, err := getKeys(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		// Without a vault nothing can change until init, whose stamp starts the sync horizon,
		// so any server_time is safe here; the clock (if a previous vault left one) or now.
		serverTime, err := readClock(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if serverTime == "" {
			serverTime = db.FormatTime(m.now())
		}
		if k != nil {
			if since != "" && (since < k.SyncHorizon || since > k.Clock) {
				return errSyncExpired
			}
			serverTime = k.Clock
		} else if since != "" {
			// No vault (never initialised, or reset): no sync point can be valid. Answering an
			// empty delta would let a client keep every item of a deleted vault; 410 makes it
			// drop its copy and do a full sync, which is empty until the next init.
			return errSyncExpired
		}
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		started = true
		bw := bufio.NewWriterSize(w, 64<<10)
		s := &syncStream{w: bw}
		s.raw(`{"folders":[`)
		if k != nil {
			if err := m.streamFolders(ctx, tx, s, p.UserID, since); err != nil {
				return err
			}
		}
		s.raw(`],"items":[`)
		if k != nil {
			if err := m.streamItems(ctx, tx, s, p.UserID, since); err != nil {
				return err
			}
		}
		s.raw(`],"deleted_item_ids":[`)
		if k != nil && since != "" {
			if err := streamTombstones(ctx, tx, s, "vault_items", p.UserID, since); err != nil {
				return err
			}
		}
		s.raw(`],"deleted_folder_ids":[`)
		if k != nil && since != "" {
			if err := streamTombstones(ctx, tx, s, "vault_folders", p.UserID, since); err != nil {
				return err
			}
		}
		// key_id names the vault key every record in this response is sealed under (null without
		// a vault): a client holding a different key must lock instead of using the records.
		s.raw(`],"key_id":`)
		if k != nil {
			s.value(k.KeyID)
		} else {
			s.raw("null")
		}
		s.raw(`,"server_time":`)
		s.value(serverTime)
		s.raw("}\n")
		if s.err != nil {
			return s.err
		}
		return bw.Flush()
	})
}

// syncStream writes a JSON document piecewise and remembers the first write error.
type syncStream struct {
	w     *bufio.Writer
	first bool
	err   error
}

func (s *syncStream) raw(str string) {
	if s.err == nil {
		_, s.err = s.w.WriteString(str)
	}
	s.first = true
}

func (s *syncStream) value(v any) {
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
func (s *syncStream) elem(v any) {
	if !s.first {
		s.raw(",")
	}
	s.first = false
	s.value(v)
}

// changedClause selects live rows, all of them or those changed after since.
func changedClause(since string) (string, []any) {
	if since == "" {
		return ` AND deleted_at IS NULL`, nil
	}
	return ` AND deleted_at IS NULL AND updated_at > ?`, []any{since}
}

func (m *Module) streamFolders(ctx context.Context, tx *sql.Tx, s *syncStream, userID, since string) error {
	clause, args := changedClause(since)
	rows, err := tx.QueryContext(ctx, `SELECT `+folderCols+` FROM vault_folders WHERE user_id = ?`+clause+
		` ORDER BY updated_at, id`, append([]any{userID}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return err
		}
		s.elem(f.Folder)
		if s.err != nil {
			return s.err
		}
	}
	return rows.Err()
}

func (m *Module) streamItems(ctx context.Context, tx *sql.Tx, s *syncStream, userID, since string) error {
	clause, args := changedClause(since)
	rows, err := tx.QueryContext(ctx, `SELECT `+itemCols+` FROM vault_items WHERE user_id = ?`+clause+
		` ORDER BY updated_at, id`, append([]any{userID}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return err
		}
		s.elem(it.Item)
		if s.err != nil {
			return s.err
		}
	}
	return rows.Err()
}

// streamTombstones writes the ids of rows deleted after since. table is a fixed identifier.
func streamTombstones(ctx context.Context, tx *sql.Tx, s *syncStream, table, userID, since string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table+
		` WHERE user_id = ? AND deleted_at IS NOT NULL AND updated_at > ? ORDER BY updated_at, id`, userID, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.elem(id)
		if s.err != nil {
			return s.err
		}
	}
	return rows.Err()
}
