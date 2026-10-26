package notes

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// TombstoneRetention is how long ids of permanently deleted notes/folders are kept for
// incremental sync. A client whose `since` is older gets a full snapshot with reset=true.
const TombstoneRetention = 365 * 24 * time.Hour

// writeChanges streams the sync document for uid. All reads happen in one read-only
// transaction (a consistent snapshot), and server_time is the module clock as of that
// snapshot: every change committed later gets a strictly greater timestamp, so passing
// server_time back as `since` never misses or repeats a change.
func (s *Service) writeChanges(ctx context.Context, uid string, since *time.Time, out io.Writer, begin func()) error {
	tx, err := s.d.DB.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var clock, pruned db.Time
	if err := tx.QueryRowContext(ctx, `SELECT ts, pruned_before FROM notes_clock WHERE k = 1`).Scan(&clock, &pruned); err != nil {
		return err
	}
	// A since older than the tombstone horizon may have missed deletions; one later than the
	// clock was issued by a server state that no longer exists (the clock went back, e.g. a
	// database backup was restored), so changes made since then could be skipped. Both get a
	// full snapshot with reset=true. server_time itself (== clock) is a normal delta.
	reset := since != nil && (since.Before(pruned.Time) || since.After(clock.Time))
	full := since == nil || reset
	sinceStr := ""
	if !full {
		sinceStr = db.FormatTime(*since)
	}

	begin()
	w := bufio.NewWriterSize(out, 64<<10)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	w.WriteString(`{"notes":[`)
	q := `SELECT ` + noteCols + ` FROM notes n WHERE n.user_id = ?`
	args := []any{uid}
	if !full {
		q += ` AND n.changed_at > ?`
		args = append(args, sinceStr)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY n.changed_at, n.id`, args...)
	if err != nil {
		return err
	}
	first := true
	for rows.Next() {
		n, err := scanNote(rows, true)
		if err != nil {
			rows.Close()
			return err
		}
		if !first {
			w.WriteByte(',')
		}
		first = false
		if err := enc.Encode(n); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	w.WriteString(`],"folders":`)
	fq := `SELECT ` + folderCols + ` FROM notes_folders f WHERE f.user_id = ?`
	fargs := []any{uid}
	if !full {
		fq += ` AND f.changed_at > ?`
		fargs = append(fargs, sinceStr)
	}
	frows, err := tx.QueryContext(ctx, fq+` ORDER BY COALESCE(f.parent_id, ''), f.position, f.id`, fargs...)
	if err != nil {
		return err
	}
	folders := []Folder{}
	for frows.Next() {
		f, err := scanFolder(frows)
		if err != nil {
			frows.Close()
			return err
		}
		folders = append(folders, *f)
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return err
	}
	if err := enc.Encode(folders); err != nil {
		return err
	}

	deleted := map[string][]string{"note": {}, "folder": {}}
	if !full {
		trows, err := tx.QueryContext(ctx, `SELECT kind, id FROM note_tombstones WHERE user_id = ? AND deleted_at > ? ORDER BY deleted_at, id`, uid, sinceStr)
		if err != nil {
			return err
		}
		for trows.Next() {
			var kind, id string
			if err := trows.Scan(&kind, &id); err != nil {
				trows.Close()
				return err
			}
			deleted[kind] = append(deleted[kind], id)
		}
		trows.Close()
		if err := trows.Err(); err != nil {
			return err
		}
	}
	tail := struct {
		DeletedNotes   []string `json:"deleted_note_ids"`
		DeletedFolders []string `json:"deleted_folder_ids"`
		ServerTime     string   `json:"server_time"`
		Reset          bool     `json:"reset"`
	}{deleted["note"], deleted["folder"], db.FormatTime(clock.Time), reset}
	b, err := json.Marshal(tail)
	if err != nil {
		return err
	}
	w.WriteByte(',')
	w.Write(b[1:]) // splice the remaining fields into the open object
	return w.Flush()
}

// pruneTombstones forgets deletions older than the retention and advances pruned_before.
func (s *Service) pruneTombstones(ctx context.Context) error {
	cutoff := core.Now().Add(-TombstoneRetention)
	return s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM note_tombstones WHERE deleted_at < ?`, db.FormatTime(cutoff))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE notes_clock SET pruned_before = ? WHERE k = 1 AND pruned_before < ?`,
			db.FormatTime(cutoff), db.FormatTime(cutoff))
		return err
	})
}
