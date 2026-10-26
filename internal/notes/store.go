package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Note is the API representation (NoteSummary when Body is nil).
type Note struct {
	ID         string      `json:"id"`
	FolderID   *string     `json:"folder_id"`
	Title      string      `json:"title"`
	Snippet    string      `json:"snippet"`
	Pinned     bool        `json:"pinned"`
	Tags       []string    `json:"tags"`
	CreatedAt  string      `json:"created_at"`
	UpdatedAt  string      `json:"updated_at"`
	Version    int64       `json:"version"`
	HasCode    bool        `json:"has_code"`
	TrashedAt  *string     `json:"trashed_at"`
	Body       *string     `json:"body,omitempty"`
	Highlights *Highlights `json:"highlights,omitempty"`

	explicitTitle *string
	n             int64
}

// Highlights marks search matches in title and snippet as [start, end) code-point offsets.
type Highlights struct {
	Title   [][2]int `json:"title"`
	Snippet [][2]int `json:"snippet"`
}

// Folder is the API representation of a note folder.
type Folder struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	Position  int     `json:"position"`
	NoteCount int     `json:"note_count"`
}

// TagCount is one entry of GET /notes/tags.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Attachment is the API representation of an uploaded attachment.
type Attachment struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size"`

	noteID    string
	createdAt string
}

const summaryCols = `n.n, n.id, n.folder_id, n.title_explicit, n.title, n.snippet, n.pinned, n.tags, n.created_at, n.updated_at, n.version, n.has_code, n.trashed_at`
const noteCols = summaryCols + `, n.body`

type rowScanner interface{ Scan(dest ...any) error }

func scanNote(rs rowScanner, withBody bool, extra ...any) (*Note, error) {
	var (
		nt       Note
		folder   sql.NullString
		explicit sql.NullString
		pinned   int64
		hasCode  int64
		tags     db.JSON[[]string]
		trashed  sql.NullString
		body     string
	)
	dest := []any{&nt.n, &nt.ID, &folder, &explicit, &nt.Title, &nt.Snippet, &pinned, &tags, &nt.CreatedAt, &nt.UpdatedAt, &nt.Version, &hasCode, &trashed}
	if withBody {
		dest = append(dest, &body)
	}
	dest = append(dest, extra...)
	if err := rs.Scan(dest...); err != nil {
		return nil, err
	}
	if folder.Valid {
		nt.FolderID = &folder.String
	}
	if explicit.Valid {
		nt.explicitTitle = &explicit.String
	}
	if trashed.Valid {
		nt.TrashedAt = &trashed.String
	}
	nt.Pinned = pinned != 0
	nt.HasCode = hasCode != 0
	nt.Tags = tags.V
	if nt.Tags == nil {
		nt.Tags = []string{}
	}
	if withBody {
		nt.Body = &body
	}
	return &nt, nil
}

// tick advances the module clock and returns the timestamp for this write transaction.
// Write transactions are serialized by SQLite, so timestamps are strictly increasing in
// commit order; GET /notes/changes relies on this to never miss a change.
func tick(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var last db.Time
	if err := tx.QueryRowContext(ctx, `SELECT ts FROM notes_clock WHERE k = 1`).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("notes clock: %w", err)
	}
	now := core.Now()
	if !now.After(last.Time) {
		now = last.Time.Add(time.Millisecond)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notes_clock SET ts = ? WHERE k = 1`, db.FormatTime(now)); err != nil {
		return time.Time{}, fmt.Errorf("notes clock: %w", err)
	}
	return now, nil
}

func getNoteTx(ctx context.Context, q db.Querier, uid, id string) (*Note, error) {
	row := q.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes n WHERE n.id = ? AND n.user_id = ?`, id, uid)
	nt, err := scanNote(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.NotFound("note not found")
	}
	return nt, err
}

func folderOwned(ctx context.Context, q db.Querier, uid, id string) error {
	var x int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM notes_folders WHERE id = ? AND user_id = ?`, id, uid).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NotFound("folder not found")
	}
	return err
}

// noteRow is everything needed to insert or rewrite a note row.
type noteRow struct {
	ID            string
	UserID        string
	FolderID      *string
	ExplicitTitle *string
	Body          string
	Tags          []string
	Pinned        bool
	Version       int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ChangedAt     time.Time
	TrashedAt     *string
}

func insertNote(ctx context.Context, tx *sql.Tx, r *noteRow) (int64, error) {
	// The user's search table must exist before the row does: a first-time build indexes
	// every existing note of the user and would otherwise index this one twice.
	tbl, err := ensureIndex(ctx, tx, r.UserID)
	if err != nil {
		return 0, err
	}
	t := computeText(r.ExplicitTitle, r.Body)
	res, err := tx.ExecContext(ctx, `INSERT INTO notes (id, user_id, folder_id, title_explicit, title, title_sort, body, snippet,
		pinned, tags, has_code, version, created_at, updated_at, changed_at, trashed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.UserID, r.FolderID, nullable(r.ExplicitTitle), t.Title, t.TitleSort, r.Body, t.Snippet,
		db.Bool(r.Pinned), db.JSON[[]string]{V: r.Tags}, db.Bool(t.HasCode), r.Version,
		db.FormatTime(r.CreatedAt), db.FormatTime(r.UpdatedAt), db.FormatTime(r.ChangedAt), r.TrashedAt)
	if err != nil {
		return 0, err
	}
	n, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := indexNote(ctx, tx, tbl, n, t, r.Tags); err != nil {
		return 0, err
	}
	return n, nil
}

func nullable(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// rewriteText updates the derived columns and the owner's search index after
// title/body/tags change.
func rewriteText(ctx context.Context, tx *sql.Tx, uid string, n int64, explicit *string, body string, tags []string) error {
	tbl, err := ensureIndex(ctx, tx, uid)
	if err != nil {
		return err
	}
	t := computeText(explicit, body)
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET title_explicit = ?, title = ?, title_sort = ?, body = ?, snippet = ?, has_code = ?, tags = ? WHERE n = ?`,
		nullable(explicit), t.Title, t.TitleSort, body, t.Snippet, db.Bool(t.HasCode), db.JSON[[]string]{V: tags}, n); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE rowid = ?`, n); err != nil {
		return err
	}
	return indexNote(ctx, tx, tbl, n, t, tags)
}

func addTombstone(ctx context.Context, tx *sql.Tx, uid, kind, id string, ts time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO note_tombstones (user_id, kind, id, deleted_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, id) DO UPDATE SET deleted_at = excluded.deleted_at`, uid, kind, id, db.FormatTime(ts))
	return err
}

// placeholders returns "?,?,…" with n marks.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
