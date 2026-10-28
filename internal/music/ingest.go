package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/db"
	"ludworks/internal/files"
)

// Ingest keeps music_tracks in step with the file index:
//   - file events create rows for new music files (values guessed from the path), requeue
//     reads when content changes, re-derive path-based values after moves and renames, and
//     refresh a folder's cover image and .lrc links when images or lyrics files change;
//   - a startup pass creates rows for music files that have none;
//   - DB-driven queues (probe_state = 0, music_folder_art.state = 0) are drained by one
//     background job each.

const ingestBatch = 500

// onFileEvent handles file.created / updated / deleted / moved.
func (s *Service) onFileEvent(payload any) {
	ev, ok := payload.(files.Event)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.d.Jobs.Context(), 2*time.Minute)
	defer cancel()
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := s.handleEvent(ctx, ev); err != nil && ctx.Err() == nil {
		s.log.Warn("music: file event", "kind", ev.Kind, "id", ev.Entry.ID, "err", err)
	}
}

func (s *Service) handleEvent(ctx context.Context, ev files.Event) error {
	e := ev.Entry
	ids := []string{e.ID}
	if ev.Recursive && e.IsDir {
		sub, err := s.files.SubtreeIDs(ctx, e.ID)
		if err != nil {
			return err
		}
		ids = append(ids, sub...)
	}
	switch ev.Kind {
	case files.KindCreated, files.KindRestored:
		return s.syncFiles(ctx, ids, ev.Kind == files.KindRestored)
	case files.KindMeta:
		return s.syncFiles(ctx, ids, false)
	case files.KindUpdated:
		if !e.IsDir {
			// Content replaced: tags and art are stale.
			if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_tracks SET probe_state = 0, updated_at = ? WHERE file_id = ?`,
				db.FormatTime(s.now()), e.ID); err != nil {
				return err
			}
			s.kickProbe()
		}
		return s.syncFiles(ctx, ids, false)
	case files.KindMoved, files.KindRenamed:
		if !e.IsDir && isSidecar(oldName(ev)) {
			// A cover image or .lrc file left its old folder (or changed its name there).
			space := ev.OldSpace
			if space == "" {
				space = e.SpaceID
			}
			if err := s.refreshDirAt(ctx, space, path.Dir("/"+ev.OldPath)); err != nil {
				return err
			}
		}
		return s.syncFiles(ctx, ids, true)
	case files.KindTrashed, files.KindDeleted:
		// A cover image or .lrc file left its folder (trashed, purged or gone from disk).
		// Trashed and deleted tracks need nothing: every query re-checks the file.
		if !e.IsDir && isSidecar(e.Name) && e.ParentID != "" {
			return s.refreshDir(ctx, e.SpaceID, e.ParentID)
		}
	}
	return nil
}

func oldName(ev files.Event) string {
	if ev.OldPath == "" {
		return ev.Entry.Name
	}
	return path.Base(ev.OldPath)
}

// imageExts are the folder image types that can become cover art (decodable by imaging).
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true}

// isSidecar reports names whose appearance changes a folder's cover image or a track's lyrics.
func isSidecar(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return imageExts[ext] || ext == ".lrc"
}

// fileInfo is the subset of a files row ingest needs.
type fileInfo struct {
	ID, SpaceID, ParentID, Path, Name, Mime, Category string
	CreatedAt                                         time.Time
	HasRow                                            bool
	RowName                                           string
}

func (fi *fileInfo) eligible() bool {
	return isMusicMime(fi.Mime) && (fi.Category == "" || fi.Category == "music")
}

// loadFileInfos loads live, non-directory files by id.
func (s *Service) loadFileInfos(ctx context.Context, ids []string) ([]fileInfo, error) {
	b, _ := json.Marshal(ids)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.space_id, COALESCE(f.parent_id, ''), f.path, f.name, f.mime,
		f.category, f.created_at, t.file_id IS NOT NULL, COALESCE(t.file_name, '')
		FROM files f LEFT JOIN music_tracks t ON t.file_id = f.id
		WHERE f.id IN (SELECT value FROM json_each(?)) AND f.is_dir = 0 AND f.trashed_at IS NULL`, string(b))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []fileInfo
	for rs.Next() {
		var (
			fi      fileInfo
			created db.Time
			hasRow  int64
		)
		if err := rs.Scan(&fi.ID, &fi.SpaceID, &fi.ParentID, &fi.Path, &fi.Name, &fi.Mime, &fi.Category, &created,
			&hasRow, &fi.RowName); err != nil {
			return nil, err
		}
		fi.CreatedAt, fi.HasRow = created.Time, hasRow == 1
		out = append(out, fi)
	}
	return out, rs.Err()
}

type dirKey struct{ space, parent string }

// syncFiles creates missing rows for eligible files, re-derives path-based values of tracks
// whose path changed (moved: the event changed paths), and refreshes the folders whose images
// or lyrics files changed.
func (s *Service) syncFiles(ctx context.Context, ids []string, moved bool) error {
	dirs := map[dirKey]bool{}
	created := false
	for start := 0; start < len(ids); start += ingestBatch {
		end := min(start+ingestBatch, len(ids))
		infos, err := s.loadFileInfos(ctx, ids[start:end])
		if err != nil {
			return err
		}
		var changed []string // tracks whose path or name changed: path-based values follow
		for i := range infos {
			fi := &infos[i]
			if fi.ParentID != "" && (isSidecar(fi.Name) || (fi.HasRow && moved)) {
				dirs[dirKey{fi.SpaceID, fi.ParentID}] = true
			}
			switch {
			case fi.HasRow && (moved || fi.RowName != fi.Name):
				changed = append(changed, fi.ID)
				if fi.ParentID != "" {
					dirs[dirKey{fi.SpaceID, fi.ParentID}] = true // the .lrc matches by base name
				}
			case !fi.HasRow && fi.eligible():
				if err := s.createRow(ctx, fi); err != nil {
					return err
				}
				created = true
				if fi.ParentID != "" {
					dirs[dirKey{fi.SpaceID, fi.ParentID}] = true
				}
			}
		}
		if len(changed) > 0 {
			// One transaction per batch: a moved folder may hold thousands of tracks.
			if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
				for _, id := range changed {
					if err := s.recompute(ctx, tx, id); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	for k := range dirs {
		if err := s.refreshDir(ctx, k.space, k.parent); err != nil {
			return err
		}
	}
	if created {
		s.kickProbe()
	}
	return nil
}

// ensureRows synchronously creates rows for the given files when they are music items.
func (s *Service) ensureRows(ctx context.Context, ids []string) error {
	infos, err := s.loadFileInfos(ctx, ids)
	if err != nil {
		return err
	}
	created := false
	for i := range infos {
		fi := &infos[i]
		if fi.HasRow || !fi.eligible() {
			continue
		}
		if err := s.createRow(ctx, fi); err != nil {
			return err
		}
		created = true
		if fi.ParentID != "" {
			if err := s.refreshDir(ctx, fi.SpaceID, fi.ParentID); err != nil {
				return err
			}
		}
	}
	if created {
		s.kickProbe()
	}
	return nil
}

// createRow inserts a pending row whose values are guessed from the path until the tags are
// read. Files are never recategorised.
func (s *Service) createRow(ctx context.Context, fi *fileInfo) error {
	now := db.FormatTime(s.now())
	e := computeEffective(fileTags{}, nil, fi.Path, fi.Name)
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO music_tracks (file_id, added_at, updated_at, file_name, probe_state)
			VALUES (?, ?, ?, ?, 0) ON CONFLICT (file_id) DO NOTHING`, fi.ID, db.FormatTime(fi.CreatedAt), now, fi.Name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return storeEffective(ctx, tx, fi.ID, e, now)
	})
	if db.IsForeignKeyViolation(err) {
		return nil // file deleted meanwhile
	}
	return err
}

// reconcile creates rows for music files that have none (startup), then queues reads.
func (s *Service) reconcile(ctx context.Context) error {
	for ctx.Err() == nil {
		rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id FROM files f LEFT JOIN music_tracks t ON t.file_id = f.id
			WHERE t.file_id IS NULL AND `+eligibleSQL+` LIMIT ?`, ingestBatch)
		if err != nil {
			return err
		}
		var ids []string
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				rs.Close()
				return err
			}
			ids = append(ids, id)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		s.eventMu.Lock()
		err = s.syncFiles(ctx, ids, false)
		s.eventMu.Unlock()
		if err != nil {
			return err
		}
	}
	// Names that changed while the server was down.
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.file_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id
		WHERE t.file_name <> f.name AND f.trashed_at IS NULL LIMIT 100000`)
	if err != nil {
		return err
	}
	var renamed []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return err
		}
		renamed = append(renamed, id)
	}
	rs.Close()
	for start := 0; start < len(renamed); start += ingestBatch {
		batch := renamed[start:min(start+ingestBatch, len(renamed))]
		if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
			for _, id := range batch {
				if err := s.recompute(ctx, tx, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	s.kickProbe()
	s.kickFolderArt()
	return nil
}

/* ------------------------------------------------- folder images and .lrc links */

// refreshDirAt refreshes the directory at dirPath ("/a/b" form, "/" = root).
func (s *Service) refreshDirAt(ctx context.Context, spaceID, dirPath string) error {
	p := strings.Trim(dirPath, "/")
	var id string
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT id FROM files WHERE space_id = ? AND path = ? AND is_dir = 1 AND trashed_at IS NULL`,
		spaceID, p).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.refreshDir(ctx, spaceID, id)
}

// folderCoverNames are the preferred cover image base names, in order.
var folderCoverNames = []string{"cover", "folder", "front", "album", "albumart"}

// chooseFolderImage picks a folder's cover image: one named cover/folder/front/album/albumart
// (jpg/jpeg/png/gif, case-insensitive; JPEG preferred), else the only image there.
func chooseFolderImage(images []sibling) string {
	if len(images) == 0 {
		return ""
	}
	best, bestRank := "", -1
	for _, im := range images {
		lower := strings.ToLower(im.name)
		ext := path.Ext(lower)
		stem := strings.TrimSuffix(lower, ext)
		for i, n := range folderCoverNames {
			if stem != n {
				continue
			}
			rank := (len(folderCoverNames)-i)*10 + extRank(ext)
			if rank > bestRank || (rank == bestRank && im.id < best) {
				best, bestRank = im.id, rank
			}
		}
	}
	if best != "" {
		return best
	}
	if len(images) == 1 {
		return images[0].id
	}
	return ""
}

func extRank(ext string) int {
	switch ext {
	case ".jpg", ".jpeg":
		return 3
	case ".png":
		return 2
	}
	return 1
}

type sibling struct {
	id, name   string
	size       int64
	modTime    string
	isTrack    bool
	trackImage sql.NullString
	trackLRC   sql.NullString
}

// discFolder matches the disc folders of a multi-disc album (CD1, CD 2, Disc 01, disk-3, …).
var discFolder = regexp.MustCompile(`(?i)^(cd|disc|disk)[ ._-]*0*[1-9][0-9]?$`)

// refreshDir recomputes the cover image and the .lrc links of every track in a directory from
// the indexed sibling files, and queues new or changed images for decoding. Disc folders below it
// are refreshed too: one without a cover of its own uses this folder's (Album/cover.jpg for
// Album/CD1/…), so a cover added to or removed from the album folder reaches them.
func (s *Service) refreshDir(ctx context.Context, spaceID, parentID string) error {
	if err := s.refreshOneDir(ctx, spaceID, parentID); err != nil {
		return err
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id, name FROM files
		WHERE space_id = ? AND parent_id = ? AND is_dir = 1 AND trashed_at IS NULL LIMIT 1000`, spaceID, parentID)
	if err != nil {
		return err
	}
	var discs []string
	for rs.Next() {
		var id, name string
		if err := rs.Scan(&id, &name); err != nil {
			rs.Close()
			return err
		}
		if discFolder.MatchString(name) {
			discs = append(discs, id)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, id := range discs {
		if err := s.refreshOneDir(ctx, spaceID, id); err != nil {
			return err
		}
	}
	return nil
}

// albumFolderImages returns the images of the album folder above disc folder dirID, or nil when
// dirID is not a disc folder.
func (s *Service) albumFolderImages(ctx context.Context, spaceID, dirID string) ([]sibling, error) {
	var name string
	var up sql.NullString
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT name, parent_id FROM files WHERE id = ? AND space_id = ? AND is_dir = 1`,
		dirID, spaceID).Scan(&name, &up)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!up.Valid || !discFolder.MatchString(name))) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	images, _, _, err := s.listDir(ctx, spaceID, up.String)
	return images, err
}

// refreshOneDir does refreshDir's work for one directory.
func (s *Service) refreshOneDir(ctx context.Context, spaceID, parentID string) error {
	images, tracks, lrcs, err := s.listDir(ctx, spaceID, parentID)
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		return nil
	}
	cover := chooseFolderImage(images)
	if cover == "" {
		if images, err = s.albumFolderImages(ctx, spaceID, parentID); err != nil {
			return err
		}
		cover = chooseFolderImage(images)
	}
	if cover != "" {
		var im sibling
		for _, x := range images {
			if x.id == cover {
				im = x
			}
		}
		if err := s.queueFolderImage(ctx, im); err != nil {
			return err
		}
	}
	now := db.FormatTime(s.now())
	for _, tr := range tracks {
		stem := strings.TrimSuffix(tr.name, path.Ext(tr.name))
		lrc := ""
		for _, l := range lrcs[strings.ToLower(stem)] {
			if strings.TrimSuffix(l.name, path.Ext(l.name)) == stem || lrc == "" {
				lrc = l.id // exact case wins
			}
		}
		if tr.trackImage.String == cover && tr.trackLRC.String == lrc && tr.trackImage.Valid == (cover != "") && tr.trackLRC.Valid == (lrc != "") {
			continue
		}
		if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_tracks SET folder_image_id = ?, lrc_file_id = ?, updated_at = ?
			WHERE file_id = ?`, nullStr(cover), nullStr(lrc), now, tr.id); err != nil {
			return err
		}
	}
	return nil
}

// listDir returns a directory's usable images, its tracks and its .lrc files by lower-case stem.
func (s *Service) listDir(ctx context.Context, spaceID, parentID string) (images, tracks []sibling, lrcs map[string][]sibling, err error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.name, f.mime, f.size, f.mod_time, f.category,
		t.file_id IS NOT NULL, t.folder_image_id, t.lrc_file_id
		FROM files f LEFT JOIN music_tracks t ON t.file_id = f.id
		WHERE f.space_id = ? AND f.parent_id = ? AND f.is_dir = 0 AND f.trashed_at IS NULL LIMIT 20000`, spaceID, parentID)
	if err != nil {
		return nil, nil, nil, err
	}
	lrcs = map[string][]sibling{}
	for rs.Next() {
		var (
			sb        sibling
			mime, cat string
			isTrack   int64
		)
		if err := rs.Scan(&sb.id, &sb.name, &mime, &sb.size, &sb.modTime, &cat, &isTrack, &sb.trackImage, &sb.trackLRC); err != nil {
			rs.Close()
			return nil, nil, nil, err
		}
		ext := strings.ToLower(path.Ext(sb.name))
		switch {
		case isTrack == 1:
			sb.isTrack = true
			tracks = append(tracks, sb)
		case ext == ".lrc":
			k := strings.ToLower(strings.TrimSuffix(sb.name, path.Ext(sb.name)))
			lrcs[k] = append(lrcs[k], sb)
		case imageExts[ext] && sb.size > 0 && sb.size <= maxFolderImage:
			images = append(images, sb)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, nil, err
	}
	return images, tracks, lrcs, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// imageSourceKey identifies a version of an image file.
func imageSourceKey(size int64, modTime string) string {
	return strconv.FormatInt(size, 10) + "|" + modTime
}

// queueFolderImage records a folder image for decoding (again when it changed).
func (s *Service) queueFolderImage(ctx context.Context, im sibling) error {
	key := imageSourceKey(im.size, im.modTime)
	res, err := s.d.DB.SQL.ExecContext(ctx, `INSERT INTO music_folder_art (file_id, source_key, state, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT (file_id) DO UPDATE SET source_key = excluded.source_key, state = 0, art_id = NULL, updated_at = excluded.updated_at
		WHERE music_folder_art.source_key <> excluded.source_key`, im.id, key, db.FormatTime(s.now()))
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return nil
		}
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.kickFolderArt()
	}
	return nil
}

/* -------------------------------------------------------------------- queues */

// kickProbe schedules the tag-reading queue drain.
func (s *Service) kickProbe() {
	s.probeDirty.Store(true)
	s.d.Jobs.GoOnce("music.probe", s.drainProbes)
}

// drainProbes reads pending tracks until none are left.
func (s *Service) drainProbes(ctx context.Context) error {
	for {
		s.probeDirty.Store(false)
		for {
			if ctx.Err() != nil {
				return nil
			}
			ids, err := s.pendingProbes(ctx, 20)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				if err := s.scanTrack(ctx, id); err != nil && ctx.Err() == nil {
					s.log.Debug("music: read tags", "id", id, "err", err)
				}
			}
		}
		if !s.probeDirty.Load() {
			return nil
		}
	}
}

func (s *Service) pendingProbes(ctx context.Context, n int) ([]string, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.file_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id
		WHERE t.probe_state = 0 AND `+eligibleSQL+` ORDER BY t.added_at LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rs.Err()
}

// kickFolderArt schedules the folder image queue drain.
func (s *Service) kickFolderArt() {
	s.folderDirty.Store(true)
	s.d.Jobs.GoOnce("music.folder-art", s.drainFolderArt)
}

func (s *Service) drainFolderArt(ctx context.Context) error {
	for {
		s.folderDirty.Store(false)
		for ctx.Err() == nil {
			rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT a.file_id, a.source_key FROM music_folder_art a
				JOIN files f ON f.id = a.file_id WHERE a.state = 0 AND f.trashed_at IS NULL ORDER BY a.updated_at LIMIT 20`)
			if err != nil {
				return err
			}
			type item struct{ id, key string }
			var items []item
			for rs.Next() {
				var it item
				if err := rs.Scan(&it.id, &it.key); err != nil {
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
				s.decodeFolderImage(ctx, it.id, it.key)
			}
		}
		if ctx.Err() != nil || !s.folderDirty.Load() {
			return nil
		}
	}
}

// decodeFolderImage stores a folder image as art (state 1) or marks it unusable (state 2).
func (s *Service) decodeFolderImage(ctx context.Context, fileID, key string) {
	artID, err := s.folderImageArt(ctx, fileID)
	if ctx.Err() != nil {
		return
	}
	state := 1
	if err != nil || artID == "" {
		state = 2
		s.log.Debug("music: folder image", "id", fileID, "err", err)
	}
	if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_folder_art SET art_id = ?, state = ?, updated_at = ?
		WHERE file_id = ? AND source_key = ?`, nullStr(artID), state, db.FormatTime(s.now()), fileID, key); err != nil {
		s.log.Warn("music: store folder image", "id", fileID, "err", err)
	}
}
