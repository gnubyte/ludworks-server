package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
)

// TopicUserDeleteFiles is published by the admin module after events.UserDeleted when the
// user's files must be removed too (payload: user id). Declared here because importing the
// admin package would create a cycle.
const TopicUserDeleteFiles = "user.delete_files"

/* ------------------------------------------------------------------ sha256 */

// scheduleHash queues the background SHA-256 pass.
func (s *Service) scheduleHash() {
	s.hashDirty.Store(true)
	s.d.Jobs.GoOnce("files.sha256", s.hashJob)
}

// hashJob computes missing SHA-256 sums (files.compute_sha256). A sum is only stored when the
// file did not change while it was hashed.
func (s *Service) hashJob(ctx context.Context) error {
	for s.hashDirty.Swap(false) {
		if !s.filesSettings(ctx).ComputeSHA256 {
			return nil
		}
		spaces := map[string]*space{}
		last := ""
		for {
			rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.space_id, f.path, f.size, f.mod_time FROM files f
				WHERE f.sha256 = '' AND f.is_dir = 0 AND f.trashed_at IS NULL AND f.id > ? ORDER BY f.id LIMIT 100`, last)
			if err != nil {
				return err
			}
			type item struct {
				id, space, path string
				size            int64
				mod             string
			}
			var items []item
			for rs.Next() {
				var it item
				if err := rs.Scan(&it.id, &it.space, &it.path, &it.size, &it.mod); err != nil {
					rs.Close()
					return err
				}
				items = append(items, it)
			}
			rs.Close()
			if len(items) == 0 {
				break
			}
			for _, it := range items {
				last = it.id
				if err := ctx.Err(); err != nil {
					return err
				}
				sp := spaces[it.space]
				if sp == nil {
					if sp, err = loadSpace(ctx, s.d.DB.SQL, it.space); err != nil || sp == nil {
						continue
					}
					spaces[it.space] = sp
				}
				sum, err := s.hashFile(ctx, sp, it.path, it.size)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					continue
				}
				_ = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE files SET sha256 = ? WHERE id = ? AND size = ? AND mod_time = ? AND sha256 = ''`,
						sum, it.id, it.size, it.mod)
					return err
				})
			}
		}
	}
	return nil
}

func (s *Service) hashFile(ctx context.Context, sp *space, p string, size int64) (string, error) {
	f, err := s.openFile(sp, p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, copyBuf))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", errors.New("file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

/* ----------------------------------------------------------------- trash */

// purgeExpiredTrash permanently deletes trash older than files.trash_retention_days.
func (s *Service) purgeExpiredTrash(ctx context.Context) error {
	days := s.filesSettings(ctx).TrashRetentionDays
	if days <= 0 {
		return nil
	}
	cutoff := db.FormatTime(s.now().Add(-time.Duration(days) * 24 * time.Hour))
	for {
		items, err := s.trashRoots(ctx, `SELECT id, space_id FROM files WHERE trash_origin IS NOT NULL AND trashed_at < ? LIMIT 200`, cutoff)
		if err != nil || len(items) == 0 {
			return err
		}
		res, err := s.purgeMany(ctx, items)
		if err != nil || res.Deleted == 0 {
			return err
		}
		s.log.Info("purged expired trash", "entries", res.Deleted, "bytes", res.Freed)
	}
}

/* ------------------------------------------------------------------ rescan */

// ScheduleRescan queues a reconciliation of the index with the disk for a space subtree
// (dirPath "" = whole space). Files added on disk are indexed, changed files updated and
// vanished ones removed from the index; file.* events are published for every change.
func (s *Service) ScheduleRescan(spaceID, dirPath string) {
	s.d.Jobs.GoOnce("files.rescan:"+spaceID+":"+dirPath, func(ctx context.Context) error {
		return s.Rescan(ctx, spaceID, dirPath)
	})
}

// RescanStats summarises a rescan.
type RescanStats struct{ Added, Updated, Removed, Skipped int }

// Rescan reconciles synchronously (see ScheduleRescan).
func (s *Service) Rescan(ctx context.Context, spaceID, dirPath string) error {
	sp, err := loadSpace(ctx, s.d.DB.SQL, spaceID)
	if err != nil || sp == nil {
		return err
	}
	var stats RescanStats
	queue := []string{dirPath}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := queue[0]
		queue = queue[1:]
		subdirs, err := s.rescanDir(ctx, sp, dir, &stats)
		if err != nil {
			return err
		}
		queue = append(queue, subdirs...)
	}
	if stats.Added+stats.Updated > 0 {
		s.scheduleHash()
	}
	s.log.Info("rescan finished", "space", spaceID, "path", dirPath, "added", stats.Added, "updated", stats.Updated,
		"removed", stats.Removed, "skipped", stats.Skipped)
	return nil
}

// rescanDir reconciles one directory and returns its subdirectories to visit.
func (s *Service) rescanDir(ctx context.Context, sp *space, dirPath string, st *RescanStats) ([]string, error) {
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	dir, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, dirPath)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		if dirPath != "" {
			return nil, nil // vanished meanwhile
		}
		if dir, err = s.ensureDirLocked(ctx, sp, "", "", &pd); err != nil {
			return nil, err
		}
	}
	if !dir.IsDir {
		return nil, nil
	}
	sr, err := s.spaceRoot(sp)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	disk := map[string]fs.FileInfo{}
	if d, err := sr.Open(osRel(dirPath)); err == nil {
		ents, rerr := d.ReadDir(-1)
		d.Close()
		if rerr != nil {
			return nil, rerr
		}
		for _, e := range ents {
			name := e.Name()
			if e.Type()&fs.ModeSymlink != 0 || checkName(name) != "" {
				st.Skipped++
				continue
			}
			info, err := e.Info()
			if err != nil || !(info.Mode().IsRegular() || info.IsDir()) {
				st.Skipped++
				continue
			}
			disk[name] = info
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	indexed, err := queryRows(ctx, s.d.DB.SQL, `SELECT `+entryCols+` FROM files f WHERE f.parent_id = ? AND f.trashed_at IS NULL`, dir.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var subdirs []string
	seen := map[string]bool{}
	owner := ownerFor(sp, "")
	for _, r := range indexed {
		seen[r.Name] = true
		info, ok := disk[r.Name]
		if ok && info.IsDir() == r.IsDir {
			if r.IsDir {
				subdirs = append(subdirs, r.Path)
				continue
			}
			mt := info.ModTime().UTC().Truncate(time.Millisecond)
			if info.Size() == r.Size && mt.Equal(r.ModTime) {
				continue
			}
			newMime := r.Mime
			if f, err := sr.Open(osRel(r.Path)); err == nil {
				newMime = detectMimeReader(r.Name, f)
				f.Close()
			}
			if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE files SET size = ?, mod_time = ?, mime = ?, sha256 = '', thumb_state = 0, updated_at = ? WHERE id = ?`,
					info.Size(), db.FormatTime(mt), newMime, db.FormatTime(now), r.ID)
				return err
			}); err != nil {
				return nil, err
			}
			e := r.Entry
			e.Size, e.ModTime, e.Mime, e.SHA256, e.UpdatedAt = info.Size(), mt, newMime, "", now
			s.ev(&pd, events.FileUpdated, Event{Kind: KindUpdated, Entry: e})
			st.Updated++
			continue
		}
		// Gone from disk (or changed type): drop the subtree from the index.
		lo, hi := subtreeBounds(r.Path)
		if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`, sp.ID, r.Path, lo, hi)
			return err
		}); err != nil {
			return nil, err
		}
		s.ev(&pd, events.FileDeleted, Event{Kind: KindDeleted, Entry: r.Entry, Recursive: r.IsDir})
		st.Removed++
		if ok {
			seen[r.Name] = false // re-add with the new type below
		}
	}
	for name, info := range disk {
		if seen[name] {
			continue
		}
		p := joinPath(dirPath, name)
		nr := &row{Entry: Entry{ID: core.NewID(), SpaceID: sp.ID, ParentID: dir.ID, Path: p, Name: name, IsDir: info.IsDir(),
			ModTime: info.ModTime().UTC().Truncate(time.Millisecond), Tags: []string{}, OwnerID: owner, CreatedAt: now, UpdatedAt: now}}
		if info.IsDir() {
			nr.Mime = DirMime
			subdirs = append(subdirs, p)
		} else {
			nr.Size = info.Size()
			nr.Mime = detectMime(name, nil)
			if f, err := sr.Open(osRel(p)); err == nil {
				nr.Mime = detectMimeReader(name, f)
				f.Close()
			}
		}
		if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error { return insertRow(ctx, tx, nr) }); err != nil {
			if db.IsUniqueViolation(err) {
				continue
			}
			return nil, err
		}
		s.ev(&pd, events.FileCreated, Event{Kind: KindCreated, Entry: nr.Entry})
		st.Added++
	}
	return subdirs, nil
}

/* ----------------------------------------------------------- user deletion */

// onUserDeleted keeps a deleted user's files for an admin to recover: the home directory is
// moved to storage/orphaned/<user id>/ and its trash to storage/orphaned/<user id>-trash/.
// The index rows are already gone (cascade from users → spaces → files).
func (s *Service) onUserDeleted(userID string) {
	if !httpx.ValidID(userID) {
		return
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	ctx := context.Background()
	root, err := s.dataRoot()
	if err != nil {
		return
	}
	_, _ = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM space_members WHERE user_id = ?`, userID)
	homeSpace := s.homeSpaceOf(ctx, userID)
	if err := root.MkdirAll(filepath.Join("storage", "orphaned"), 0o700); err != nil {
		s.log.Error("cannot create orphaned dir", "err", err)
		return
	}
	move := func(src, dst string) {
		if _, err := root.Lstat(src); err != nil {
			return
		}
		target := dst
		for i := 1; ; i++ {
			if _, err := root.Lstat(target); errors.Is(err, os.ErrNotExist) {
				break
			}
			target = dst + "." + time.Now().UTC().Format("20060102T150405") + "-" + core.NewID()[:6]
			if i > 3 {
				return
			}
		}
		if err := moveWithin(ctx, root, src, target); err != nil {
			s.log.Error("moving deleted user's files failed", "user", userID, "src", src, "err", err)
			return
		}
		s.log.Info("kept deleted user's files for recovery", "user", userID, "path", filepath.Join(s.dataDir, target))
	}
	move(filepath.Join("storage", "home", userID), filepath.Join("storage", "orphaned", userID))
	if homeSpace != "" {
		move(filepath.Join("storage", ".trash", homeSpace), filepath.Join("storage", "orphaned", userID+"-trash"))
	}
	_, _ = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM home_spaces WHERE user_id = ?`, userID)
}

// onUserDeleteFiles removes a deleted user's home files (also any orphaned copy).
func (s *Service) onUserDeleteFiles(userID string) {
	if !httpx.ValidID(userID) {
		return
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	ctx := context.Background()
	homeSpace := s.homeSpaceOf(ctx, userID)
	for _, rel := range []string{
		filepath.Join("storage", "home", userID),
		filepath.Join("storage", "orphaned", userID),
		filepath.Join("storage", "orphaned", userID+"-trash"),
	} {
		s.removeLater(rel)
	}
	if homeSpace != "" {
		s.removeLater(filepath.Join("storage", ".trash", homeSpace))
	}
	s.log.Info("deleted user's files removed", "user", userID)
}

func (s *Service) homeSpaceOf(ctx context.Context, userID string) string {
	var id string
	_ = s.d.DB.SQL.QueryRowContext(ctx, `SELECT space_id FROM home_spaces WHERE user_id = ?`, userID).Scan(&id)
	if !httpx.ValidID(id) {
		return ""
	}
	return id
}

// payloadString extracts a string event payload.
func payloadString(p any) string {
	s, _ := p.(string)
	return strings.TrimSpace(s)
}
