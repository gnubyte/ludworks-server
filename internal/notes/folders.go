package notes

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// FolderPatch is a partial folder update.
type FolderPatch struct {
	Name      *string
	ParentSet bool
	ParentID  *string // with ParentSet: nil = top level
	Position  *int
}

const folderCols = `f.id, f.name, f.parent_id, f.position,
	(SELECT COUNT(*) FROM notes n WHERE n.folder_id = f.id AND n.trashed_at IS NULL)`

func scanFolder(rs rowScanner) (*Folder, error) {
	var f Folder
	var parent sql.NullString
	if err := rs.Scan(&f.ID, &f.Name, &parent, &f.Position, &f.NoteCount); err != nil {
		return nil, err
	}
	if parent.Valid {
		f.ParentID = &parent.String
	}
	return &f, nil
}

func getFolderTx(ctx context.Context, q db.Querier, uid, id string) (*Folder, error) {
	f, err := scanFolder(q.QueryRowContext(ctx, `SELECT `+folderCols+` FROM notes_folders f WHERE f.id = ? AND f.user_id = ?`, id, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.NotFound("folder not found")
	}
	return f, err
}

// ListFolders returns all folders of the user (flat; ordered by parent, position, name).
func (s *Service) ListFolders(ctx context.Context, uid string) ([]Folder, error) {
	rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT `+folderCols+` FROM notes_folders f WHERE f.user_id = ?
		ORDER BY COALESCE(f.parent_id, ''), f.position, f.name_key, f.id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// folderDepth returns the depth of folder id (1 = top level).
func folderDepth(ctx context.Context, q db.Querier, id string) (int, error) {
	var d int
	err := q.QueryRowContext(ctx, `WITH RECURSIVE up(id, parent_id, depth) AS (
			SELECT id, parent_id, 1 FROM notes_folders WHERE id = ?
			UNION ALL SELECT f.id, f.parent_id, up.depth + 1 FROM notes_folders f JOIN up ON f.id = up.parent_id WHERE up.depth < 1000)
		SELECT MAX(depth) FROM up`, id).Scan(&d)
	return d, err
}

// subtreeIDs returns id and all of its descendants together with the subtree height
// (1 = no children).
func subtreeIDs(ctx context.Context, q db.Querier, id string) ([]string, int, error) {
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE sub(id, depth) AS (
			SELECT ?, 1
			UNION ALL SELECT f.id, sub.depth + 1 FROM notes_folders f JOIN sub ON f.parent_id = sub.id WHERE sub.depth < 1000)
		SELECT id, depth FROM sub`, id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ids []string
	height := 0
	for rows.Next() {
		var fid string
		var d int
		if err := rows.Scan(&fid, &d); err != nil {
			return nil, 0, err
		}
		ids = append(ids, fid)
		height = max(height, d)
	}
	return ids, height, rows.Err()
}

func nameTaken(ctx context.Context, q db.Querier, uid string, parent *string, nameKey, exceptID string) (bool, error) {
	var x int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM notes_folders WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND name_key = ? AND id <> ?`,
		uid, deref(parent), nameKey, exceptID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func duplicateName() error { return core.Conflict("A folder with this name already exists here") }

// CreateFolder creates a folder at the end of its siblings.
func (s *Service) CreateFolder(ctx context.Context, uid, name string, parentID *string) (*Folder, error) {
	name, problem := normalizeFolderName(name)
	if problem != "" {
		return nil, core.Validation(map[string]string{"name": problem})
	}
	var out *Folder
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		f, err := createFolderTx(ctx, tx, uid, name, parentID, ts)
		out = f
		return err
	})
	return out, err
}

func createFolderTx(ctx context.Context, tx *sql.Tx, uid, name string, parentID *string, ts time.Time) (*Folder, error) {
	if parentID != nil {
		if err := folderOwned(ctx, tx, uid, *parentID); err != nil {
			return nil, err
		}
		depth, err := folderDepth(ctx, tx, *parentID)
		if err != nil {
			return nil, err
		}
		if depth+1 > MaxFolderDepth {
			return nil, core.Validation(map[string]string{"parent_id": "folders can be nested at most 32 levels deep"})
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes_folders WHERE user_id = ?`, uid).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxFoldersPerUser {
		return nil, core.Conflict("Folder limit reached (5000)")
	}
	key := sortKey(name)
	if taken, err := nameTaken(ctx, tx, uid, parentID, key, ""); err != nil {
		return nil, err
	} else if taken {
		return nil, duplicateName()
	}
	var pos int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM notes_folders WHERE user_id = ? AND COALESCE(parent_id, '') = ?`,
		uid, deref(parentID)).Scan(&pos); err != nil {
		return nil, err
	}
	id := core.NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes_folders (id, user_id, parent_id, name, name_key, position, created_at, changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, uid, parentID, name, key, pos, db.FormatTime(ts), db.FormatTime(ts)); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, duplicateName()
		}
		return nil, err
	}
	return getFolderTx(ctx, tx, uid, id)
}

// UpdateFolder renames, moves (re-parents) and/or reorders a folder.
func (s *Service) UpdateFolder(ctx context.Context, uid, id string, p FolderPatch) (*Folder, error) {
	var newName string
	if p.Name != nil {
		n, problem := normalizeFolderName(*p.Name)
		if problem != "" {
			return nil, core.Validation(map[string]string{"name": problem})
		}
		newName = n
	}
	if p.Position != nil && *p.Position < 0 {
		return nil, core.Validation(map[string]string{"position": "must be 0 or greater"})
	}
	var out *Folder
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getFolderTx(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		name := cur.Name
		parent := cur.ParentID
		changed := false
		if p.Name != nil && newName != cur.Name {
			name = newName
			changed = true
		}
		moved := p.ParentSet && !equalPtr(p.ParentID, cur.ParentID)
		if moved {
			parent = p.ParentID
			if parent != nil {
				if err := folderOwned(ctx, tx, uid, *parent); err != nil {
					return err
				}
				sub, height, err := subtreeIDs(ctx, tx, id)
				if err != nil {
					return err
				}
				for _, sid := range sub {
					if sid == *parent {
						return core.Conflict("A folder cannot be moved into itself or one of its subfolders")
					}
				}
				depth, err := folderDepth(ctx, tx, *parent)
				if err != nil {
					return err
				}
				if depth+height > MaxFolderDepth {
					return core.Validation(map[string]string{"parent_id": "folders can be nested at most 32 levels deep"})
				}
			}
			changed = true
		}
		if !changed && p.Position == nil {
			out = cur
			return nil
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		if taken, err := nameTaken(ctx, tx, uid, parent, sortKey(name), id); err != nil {
			return err
		} else if taken {
			return duplicateName()
		}
		if changed {
			if _, err := tx.ExecContext(ctx, `UPDATE notes_folders SET name = ?, name_key = ?, parent_id = ?, changed_at = ? WHERE id = ?`,
				name, sortKey(name), parent, db.FormatTime(ts), id); err != nil {
				if db.IsUniqueViolation(err) {
					return duplicateName()
				}
				return err
			}
		}
		if moved || p.Position != nil {
			want := -1 // append
			if p.Position != nil {
				want = *p.Position
			}
			if err := reorderSiblings(ctx, tx, uid, parent, id, want, ts); err != nil {
				return err
			}
		}
		out, err = getFolderTx(ctx, tx, uid, id)
		return err
	})
	return out, err
}

// reorderSiblings places folder id at index want (-1 = last) among its siblings and
// renumbers positions 0..n-1, bumping changed_at for every folder whose position changed.
func reorderSiblings(ctx context.Context, tx *sql.Tx, uid string, parent *string, id string, want int, ts time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, position FROM notes_folders WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND id <> ?
		ORDER BY position, name_key, id`, uid, deref(parent), id)
	if err != nil {
		return err
	}
	type sib struct {
		id  string
		pos int
	}
	var sibs []sib
	for rows.Next() {
		var s sib
		if err := rows.Scan(&s.id, &s.pos); err != nil {
			rows.Close()
			return err
		}
		sibs = append(sibs, s)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if want < 0 || want > len(sibs) {
		want = len(sibs)
	}
	order := make([]sib, 0, len(sibs)+1)
	order = append(order, sibs[:want]...)
	order = append(order, sib{id: id, pos: -1})
	order = append(order, sibs[want:]...)
	for i, s := range order {
		if s.pos == i {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes_folders SET position = ?, changed_at = ? WHERE id = ?`, i, db.FormatTime(ts), s.id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteFolder deletes a folder and all of its subfolders; their notes are moved to the trash.
func (s *Service) DeleteFolder(ctx context.Context, uid, id string) error {
	return s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := folderOwned(ctx, tx, uid, id); err != nil {
			return err
		}
		ids, _, err := subtreeIDs(ctx, tx, id)
		if err != nil {
			return err
		}
		ts, err := tick(ctx, tx)
		if err != nil {
			return err
		}
		t := db.FormatTime(ts)
		for start := 0; start < len(ids); start += 500 {
			chunk := ids[start:min(start+500, len(ids))]
			args := append([]any{t, t, uid}, anySlice(chunk)...)
			if _, err := tx.ExecContext(ctx, `UPDATE notes SET trashed_at = COALESCE(trashed_at, ?), folder_id = NULL,
				version = version + 1, changed_at = ? WHERE user_id = ? AND folder_id IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
				return err
			}
		}
		for _, fid := range ids {
			if err := addTombstone(ctx, tx, uid, "folder", fid, ts); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM notes_folders WHERE id = ? AND user_id = ?`, id, uid)
		return err
	})
}
