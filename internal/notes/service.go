package notes

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Service is the notes service shared with other modules (mail-in) and used by the HTTP
// handlers. Every method takes the owning user's id and only ever touches that user's rows;
// objects of other users are reported as 404.
type Service struct {
	d         *core.Deps
	attachDir string
	gcKick    func()
}

func newService(d *core.Deps) *Service {
	s := &Service{d: d}
	if d != nil && d.Config != nil {
		s.attachDir = filepath.Join(d.Config.DataDir, "notes", "attachments")
	}
	return s
}

// NoteInput describes a note to create.
type NoteInput struct {
	Title    *string
	Body     string
	FolderID *string
	Tags     []string
	Pinned   bool
	// Created/Updated override the timestamps (import); zero means now.
	Created time.Time
	Updated time.Time
}

// NotePatch is a partial update; nil fields are left unchanged.
type NotePatch struct {
	Title       *string // "" clears the explicit title
	Body        *string
	FolderSet   bool
	FolderID    *string // with FolderSet: nil = no folder
	Tags        *[]string
	Pinned      *bool
	BaseVersion *int64
}

func bodyTooLarge() error {
	return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Note body exceeds 5 MiB")
}

func validateInput(in *NoteInput) (*string, []string, error) {
	fields := map[string]string{}
	var title *string
	if in.Title != nil {
		t, problem := normalizeTitle(*in.Title)
		if problem != "" {
			fields["title"] = problem
		} else if t != "" {
			title = &t
		}
	}
	tags, problem := normalizeTags(in.Tags)
	if problem != "" {
		fields["tags"] = problem
	}
	if len(fields) > 0 {
		return nil, nil, core.Validation(fields)
	}
	if len(in.Body) > MaxBodyBytes {
		return nil, nil, bodyTooLarge()
	}
	return title, tags, nil
}

// CreateNote creates a Markdown note for userID and returns its id (used by IMAP mail-in).
// A non-empty title becomes the note's explicit title.
func (s *Service) CreateNote(ctx context.Context, userID, title, body string) (string, error) {
	in := NoteInput{Body: body}
	if title != "" {
		in.Title = &title
	}
	n, err := s.Create(ctx, userID, in)
	if err != nil {
		return "", err
	}
	return n.ID, nil
}

// Create inserts a note and returns it.
func (s *Service) Create(ctx context.Context, uid string, in NoteInput) (*Note, error) {
	title, tags, err := validateInput(&in)
	if err != nil {
		return nil, err
	}
	id := core.NewID()
	var out *Note
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if in.FolderID != nil {
			if err := folderOwned(ctx, tx, uid, *in.FolderID); err != nil {
				return err
			}
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		created, updated := ts, ts
		if !in.Created.IsZero() {
			created = in.Created
		}
		if !in.Updated.IsZero() {
			updated = in.Updated
		} else if !in.Created.IsZero() {
			updated = in.Created
		}
		if _, err := insertNote(ctx, tx, &noteRow{
			ID: id, UserID: uid, FolderID: in.FolderID, ExplicitTitle: title, Body: in.Body, Tags: tags,
			Pinned: in.Pinned, Version: 1, CreatedAt: created, UpdatedAt: updated, ChangedAt: ts,
		}); err != nil {
			return err
		}
		out, err = getNoteTx(ctx, tx, uid, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one note with its body.
func (s *Service) Get(ctx context.Context, uid, id string) (*Note, error) {
	return getNoteTx(ctx, s.d.DB.SQL, uid, id)
}

// Update applies a partial update with optional optimistic concurrency (BaseVersion).
func (s *Service) Update(ctx context.Context, uid, id string, p NotePatch) (*Note, error) {
	fields := map[string]string{}
	var newTitle *string
	if p.Title != nil {
		t, problem := normalizeTitle(*p.Title)
		if problem != "" {
			fields["title"] = problem
		}
		newTitle = &t
	}
	var newTags []string
	if p.Tags != nil {
		t, problem := normalizeTags(*p.Tags)
		if problem != "" {
			fields["tags"] = problem
		}
		newTags = t
	}
	if len(fields) > 0 {
		return nil, core.Validation(fields)
	}
	if p.Body != nil && len(*p.Body) > MaxBodyBytes {
		return nil, bodyTooLarge()
	}
	var out *Note
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getNoteTx(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		if p.BaseVersion != nil && *p.BaseVersion != cur.Version {
			return core.Conflict("The note was changed elsewhere").WithDetails(map[string]any{"current": cur})
		}
		explicit := cur.explicitTitle
		body := *cur.Body
		tags := cur.Tags
		textChanged := false
		if newTitle != nil {
			old := ""
			if explicit != nil {
				old = *explicit
			}
			if *newTitle != old {
				textChanged = true
				if *newTitle == "" {
					explicit = nil
				} else {
					explicit = newTitle
				}
			}
		}
		if p.Body != nil && *p.Body != body {
			body = *p.Body
			textChanged = true
		}
		if p.Tags != nil && !slices.Equal(newTags, tags) {
			tags = newTags
			textChanged = true
		}
		folderChanged := false
		if p.FolderSet && !equalPtr(p.FolderID, cur.FolderID) {
			if p.FolderID != nil {
				if err := folderOwned(ctx, tx, uid, *p.FolderID); err != nil {
					return err
				}
			}
			folderChanged = true
		}
		pinChanged := p.Pinned != nil && *p.Pinned != cur.Pinned
		if !textChanged && !folderChanged && !pinChanged {
			out = cur
			return nil
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		if textChanged {
			if err := rewriteText(ctx, tx, uid, cur.n, explicit, body, tags); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET updated_at = ? WHERE n = ?`, db.FormatTime(ts), cur.n); err != nil {
				return err
			}
		}
		if folderChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET folder_id = ? WHERE n = ?`, p.FolderID, cur.n); err != nil {
				return err
			}
		}
		if pinChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET pinned = ? WHERE n = ?`, db.Bool(*p.Pinned), cur.n); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET version = version + 1, changed_at = ? WHERE n = ?`, db.FormatTime(ts), cur.n); err != nil {
			return err
		}
		out, err = getNoteTx(ctx, tx, uid, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Delete moves a note to the trash, or removes it (and its attachments) permanently.
func (s *Service) Delete(ctx context.Context, uid, id string, permanent bool) error {
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getNoteTx(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		if !permanent && cur.TrashedAt != nil {
			return nil
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		if permanent {
			if _, err := tx.ExecContext(ctx, `DELETE FROM notes WHERE n = ?`, cur.n); err != nil {
				return err
			}
			if err := unindexNote(ctx, tx, uid, cur.n); err != nil {
				return err
			}
			return addTombstone(ctx, tx, uid, "note", id, ts)
		}
		_, err = tx.ExecContext(ctx, `UPDATE notes SET trashed_at = ?, version = version + 1, changed_at = ? WHERE n = ?`,
			db.FormatTime(ts), db.FormatTime(ts), cur.n)
		return err
	})
	if err == nil && permanent {
		s.kickGC()
	}
	return err
}

// Restore takes a note out of the trash (into "Notes" when its folder is gone).
func (s *Service) Restore(ctx context.Context, uid, id string) (*Note, error) {
	var out *Note
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getNoteTx(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		if cur.TrashedAt == nil {
			out = cur
			return nil
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET trashed_at = NULL, version = version + 1, changed_at = ? WHERE n = ?`,
			db.FormatTime(ts), cur.n); err != nil {
			return err
		}
		out, err = getNoteTx(ctx, tx, uid, id)
		return err
	})
	return out, err
}

// Tags returns the tags of the user's live notes with counts, alphabetically.
func (s *Service) Tags(ctx context.Context, uid string) ([]TagCount, error) {
	rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT j.value, COUNT(*) FROM notes n, json_each(n.tags) j
		WHERE n.user_id = ? AND n.trashed_at IS NULL AND j.type = 'text' GROUP BY j.value ORDER BY j.value`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// purgeUser removes every notes row of a deleted user (normally already gone through the
// users foreign-key cascade), drops the user's search table and schedules the attachment
// files for removal.
func (s *Service) purgeUser(ctx context.Context, uid string) error {
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := dropIndex(ctx, tx, uid, ftsTable(uid)); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM note_attachments WHERE user_id = ?`,
			`DELETE FROM notes WHERE user_id = ?`,
			`DELETE FROM notes_folders WHERE user_id = ?`,
			`DELETE FROM note_tombstones WHERE user_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.runGC(ctx)
}

func (s *Service) kickGC() {
	if s.gcKick != nil {
		s.gcKick()
	}
}
