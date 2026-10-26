package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/media/naming"
	"ludworks/internal/media/probe"
	"ludworks/internal/media/subtitles"
)

// Ingest keeps media_items in step with the file index:
//   - file events create rows for new media files (metadata guessed from the path), refresh
//     sidecar subtitles when subtitle files appear/disappear and requeue probes when
//     content changes;
//   - a startup pass creates rows for media files that have none;
//   - a DB-driven probe queue (probe_state = 0) is drained by a single background job.

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
		s.log.Warn("media: file event", "kind", ev.Kind, "id", ev.Entry.ID, "err", err)
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
	case files.KindCreated, files.KindRestored, files.KindMeta:
		return s.syncFiles(ctx, ids)
	case files.KindUpdated:
		// Content replaced: technical metadata is stale.
		if !e.IsDir {
			if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET probe_state = 0, updated_at = ? WHERE file_id = ?`,
				db.FormatTime(s.now()), e.ID); err != nil {
				return err
			}
			s.kickProbe()
		}
		return s.syncFiles(ctx, ids)
	case files.KindMoved, files.KindRenamed:
		if !e.IsDir && isSubtitleName(oldName(ev)) {
			// A subtitle left its old directory (or changed name there).
			space := ev.OldSpace
			if space == "" {
				space = e.SpaceID
			}
			if err := s.refreshSidecarsAt(ctx, space, path.Dir("/"+ev.OldPath)); err != nil {
				return err
			}
		}
		if err := s.syncFiles(ctx, ids); err != nil {
			return err
		}
		if !e.IsDir && e.ParentID != "" {
			// A moved video gets the sidecars of its new directory.
			return s.refreshSidecars(ctx, e.SpaceID, e.ParentID)
		}
		return nil
	case files.KindTrashed:
		if !e.IsDir && isSubtitleName(e.Name) && e.ParentID != "" {
			return s.refreshSidecars(ctx, e.SpaceID, e.ParentID)
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

func isSubtitleName(name string) bool {
	return subtitles.FormatForExt(path.Ext(name)) != ""
}

// fileInfo is the subset of a files row ingest needs.
type fileInfo struct {
	ID, SpaceID, ParentID, Path, Name, Mime, Category, Subcategory string
	CreatedAt                                                      time.Time
	HasRow                                                         bool
	RowName                                                        string
}

func (fi *fileInfo) eligible() bool {
	return fi.Category == "movie" || fi.Category == "tv" || strings.HasPrefix(fi.Mime, "video/")
}

// loadFileInfos loads live, non-directory files by id.
func (s *Service) loadFileInfos(ctx context.Context, ids []string) ([]fileInfo, error) {
	b, _ := json.Marshal(ids)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.space_id, COALESCE(f.parent_id, ''), f.path, f.name, f.mime,
		f.category, f.subcategory, f.created_at, m.file_id IS NOT NULL, COALESCE(m.file_name, '')
		FROM files f LEFT JOIN media_items m ON m.file_id = f.id
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
		if err := rs.Scan(&fi.ID, &fi.SpaceID, &fi.ParentID, &fi.Path, &fi.Name, &fi.Mime, &fi.Category, &fi.Subcategory,
			&created, &hasRow, &fi.RowName); err != nil {
			return nil, err
		}
		fi.CreatedAt, fi.HasRow = created.Time, hasRow == 1
		out = append(out, fi)
	}
	return out, rs.Err()
}

// syncFiles creates missing rows for eligible files, keeps file_name current, refreshes the
// sidecar subtitles of directories whose subtitle files changed, and queues probes.
func (s *Service) syncFiles(ctx context.Context, ids []string) error {
	type dirKey struct{ space, parent string }
	dirs := map[dirKey]bool{}
	created := false
	for start := 0; start < len(ids); start += ingestBatch {
		end := min(start+ingestBatch, len(ids))
		infos, err := s.loadFileInfos(ctx, ids[start:end])
		if err != nil {
			return err
		}
		for i := range infos {
			fi := &infos[i]
			if isSubtitleName(fi.Name) && fi.ParentID != "" {
				dirs[dirKey{fi.SpaceID, fi.ParentID}] = true
			}
			switch {
			case fi.HasRow && fi.RowName != fi.Name:
				if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET file_name = ? WHERE file_id = ?`, fi.Name, fi.ID); err != nil {
					return err
				}
				if fi.ParentID != "" {
					dirs[dirKey{fi.SpaceID, fi.ParentID}] = true // sidecars match by base name
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
	}
	for k := range dirs {
		if err := s.refreshSidecars(ctx, k.space, k.parent); err != nil {
			return err
		}
	}
	if created {
		s.kickProbe()
	}
	return nil
}

// ensureRows synchronously creates rows for the given files when they are eligible.
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
			if err := s.refreshSidecars(ctx, fi.SpaceID, fi.ParentID); err != nil {
				return err
			}
		}
	}
	if created {
		s.kickProbe()
	}
	return nil
}

// guess holds metadata inferred from a path (and optionally NFO files).
type guess struct {
	kind                       string
	title, showTitle, epTitle  string
	year, season, episode      sql.NullInt64
	description, rating, imdb  string
	showDescription, sortTitle string
	genres                     []string
	tmdb                       sql.NullInt64
}

func guessFromPath(p string) guess {
	ps := naming.Parse(p)
	g := guess{kind: ps.Kind, title: cleanText(ps.Title, maxTitle)}
	if ps.Year > 0 {
		g.year = sql.NullInt64{Int64: int64(ps.Year), Valid: true}
	}
	if ps.Kind == naming.KindTV {
		g.showTitle = cleanText(ps.ShowTitle, maxTitle)
		g.season = sql.NullInt64{Int64: int64(ps.Season), Valid: true}
		g.episode = sql.NullInt64{Int64: int64(ps.Episode), Valid: true}
		g.epTitle = cleanText(ps.EpisodeTitle, maxTitle)
		if g.showTitle == "" {
			g.season, g.episode = sql.NullInt64{}, sql.NullInt64{}
		}
	}
	if g.title == "" {
		g.title = cleanText(strings.TrimSuffix(path.Base(p), path.Ext(p)), maxTitle)
	}
	if g.title == "" {
		g.title = cleanText(path.Base(p), maxTitle)
	}
	return g
}

// createRow inserts a row with metadata guessed from the path. Confidently recognised new
// files without a category are categorised (tv/episode, or movie/feature when a year is
// present) so they show up in the right library section.
func (s *Service) createRow(ctx context.Context, fi *fileInfo) error {
	g := guessFromPath(fi.Path)
	now := s.now()
	_, err := s.d.DB.SQL.ExecContext(ctx, `INSERT INTO media_items (file_id, title, sort_title, year, show_title, show_key,
		season, episode, episode_title, added_at, updated_at, file_name, meta_source, probe_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'filename', 0) ON CONFLICT (file_id) DO NOTHING`,
		fi.ID, g.title, sortTitleFor(g.title), g.year, g.showTitle, naming.ShowKey(g.showTitle), g.season, g.episode,
		g.epTitle, db.FormatTime(fi.CreatedAt), db.FormatTime(now), fi.Name)
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return nil // file deleted meanwhile
		}
		return err
	}
	s.publishUpdated(fi.ID)
	if fi.Category == "" {
		cat, sub := "", ""
		switch {
		case g.kind == naming.KindTV && g.showTitle != "":
			cat, sub = "tv", "episode"
		case g.kind == naming.KindMovie && g.year.Valid:
			cat, sub = "movie", "feature"
		}
		if cat != "" {
			if _, err := s.files.SetMeta(ctx, s.sys, fi.ID, &cat, &sub, nil); err != nil {
				s.log.Debug("media: auto-categorize", "id", fi.ID, "err", err)
			}
		}
	}
	return nil
}

// reconcile creates rows for media files that have none (startup), then queues probes.
func (s *Service) reconcile(ctx context.Context) error {
	if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET file_name = f.name
		FROM files f WHERE f.id = media_items.file_id AND media_items.file_name <> f.name`); err != nil {
		return err
	}
	for ctx.Err() == nil {
		rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id FROM files f LEFT JOIN media_items m ON m.file_id = f.id
			WHERE m.file_id IS NULL AND f.is_dir = 0 AND f.trashed_at IS NULL AND `+eligibleSQL+` LIMIT ?`, ingestBatch)
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
		err = s.syncFiles(ctx, ids)
		s.eventMu.Unlock()
		if err != nil {
			return err
		}
	}
	s.kickProbe()
	return nil
}

/* ----------------------------------------------------------------- sidecars */

// refreshSidecarsAt refreshes the directory at dirPath ("/a/b" form, "/" = root).
func (s *Service) refreshSidecarsAt(ctx context.Context, spaceID, dirPath string) error {
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
	return s.refreshSidecars(ctx, spaceID, id)
}

// refreshSidecars recomputes the sidecar subtitle tracks of every media item in a
// directory from the indexed sibling files (embedded tracks are kept).
func (s *Service) refreshSidecars(ctx context.Context, spaceID, parentID string) error {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.name, m.file_id IS NOT NULL, COALESCE(m.subtitles, '[]')
		FROM files f LEFT JOIN media_items m ON m.file_id = f.id
		WHERE f.space_id = ? AND f.parent_id = ? AND f.is_dir = 0 AND f.trashed_at IS NULL LIMIT 20000`, spaceID, parentID)
	if err != nil {
		return err
	}
	type video struct {
		id, name string
		subs     []storedSub
	}
	var (
		names  []string
		byName = map[string]string{}
		videos []video
	)
	for rs.Next() {
		var (
			id, name string
			isMedia  int64
			subs     db.JSON[[]storedSub]
		)
		if err := rs.Scan(&id, &name, &isMedia, &subs); err != nil {
			rs.Close()
			return err
		}
		names = append(names, name)
		byName[name] = id
		if isMedia == 1 {
			videos = append(videos, video{id: id, name: name, subs: subs.V})
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, v := range videos {
		next := sidecarSubs(v.name, names, byName)
		for _, sub := range v.subs {
			if sub.Format == "embedded" {
				next = append(next, sub)
			}
		}
		if subsEqual(next, v.subs) {
			continue
		}
		if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET subtitles = ?, updated_at = ? WHERE file_id = ?`,
			db.JSON[[]storedSub]{V: next}, db.FormatTime(s.now()), v.id); err != nil {
			return err
		}
	}
	return nil
}

func sidecarSubs(videoName string, siblings []string, byName map[string]string) []storedSub {
	var out []storedSub
	for _, t := range probe.MatchSidecars(videoName, siblings) {
		id := byName[t.File]
		format := subtitles.FormatForExt(path.Ext(t.File))
		if id == "" || format == "" {
			continue
		}
		out = append(out, storedSub{Language: t.Language, Label: cleanText(t.Label, 200), Format: format, Codec: t.Codec,
			FileID: id, Forced: t.Forced})
	}
	return out
}

func subsEqual(a, b []storedSub) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return string(x) == string(y)
}

/* -------------------------------------------------------------------- probes */

// kickProbe schedules the probe queue drain.
func (s *Service) kickProbe() {
	s.probeDirty.Store(true)
	s.d.Jobs.GoOnce("media.probe", s.drainProbes)
}

// drainProbes probes pending items until none are left (auto_probe only).
func (s *Service) drainProbes(ctx context.Context) error {
	for {
		s.probeDirty.Store(false)
		if !s.settings(ctx).AutoProbe {
			return nil
		}
		for {
			if err := ctx.Err(); err != nil {
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
				if err := s.probeItem(ctx, id); err != nil && ctx.Err() == nil {
					s.log.Debug("media: probe", "id", id, "err", err)
				}
			}
		}
		if !s.probeDirty.Load() {
			return nil
		}
	}
}

func (s *Service) pendingProbes(ctx context.Context, n int) ([]string, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.file_id FROM media_items m JOIN files f ON f.id = m.file_id
		WHERE m.probe_state = 0 AND f.trashed_at IS NULL ORDER BY m.added_at LIMIT ?`, n)
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

// probeItem probes one file (ffprobe when available, else the pure-Go parsers), stores the
// technical metadata and subtitle tracks, and pre-generates the thumbnail when enabled. The
// row always leaves the pending state unless ctx is cancelled.
func (s *Service) probeItem(ctx context.Context, id string) error {
	info, perr := s.runProbe(ctx, id)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := db.FormatTime(s.now())
	if perr != nil {
		_, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET probe_state = 2, probed_at = ?, updated_at = ? WHERE file_id = ?`,
			now, now, id)
		s.publishUpdated(id)
		return errors.Join(perr, err)
	}
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var cur db.JSON[[]storedSub]
		if err := tx.QueryRowContext(ctx, `SELECT subtitles FROM media_items WHERE file_id = ?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		subs := make([]storedSub, 0, len(cur.V)+len(info.Subtitles))
		for _, sub := range cur.V {
			if sub.Format != "embedded" {
				subs = append(subs, sub)
			}
		}
		stream := 0
		for _, t := range info.Subtitles {
			if !t.Embedded {
				continue
			}
			n := stream
			stream++
			if !t.IsText() {
				continue // image-based (PGS/VobSub) tracks cannot become WebVTT
			}
			subs = append(subs, storedSub{Language: t.Language, Label: cleanText(t.Label, 200), Format: "embedded",
				Codec: t.Codec, Stream: &n, Forced: t.Forced})
		}
		w, h := info.Width, info.Height
		if info.Rotation == 90 || info.Rotation == 270 {
			w, h = h, w
		}
		_, err := tx.ExecContext(ctx, `UPDATE media_items SET duration_seconds = ?, width = ?, height = ?, video_codec = ?,
			audio_codec = ?, container = ?, browser_playable = ?, subtitles = ?, probed_at = ?, probe_state = 1, updated_at = ?
			WHERE file_id = ?`,
			nullPositive(info.DurationSeconds), nullPositiveInt(w), nullPositiveInt(h), cleanText(info.VideoCodec, 32),
			cleanText(info.AudioCodec, 32), cleanText(info.Container, 32), db.Bool(info.BrowserPlayable),
			db.JSON[[]storedSub]{V: subs}, now, now, id)
		return err
	})
	if err != nil {
		return err
	}
	s.publishUpdated(id)
	if s.settings(ctx).AutoThumbnails && s.ffmpeg() != "" {
		if e, err := s.files.Get(ctx, s.sys, id); err == nil && strings.HasPrefix(e.Mime, "video/") {
			tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			_, _, _ = s.files.ThumbnailFile(tctx, s.sys, id, "md")
			cancel()
		}
	}
	return nil
}

// runProbe returns the technical metadata of a file.
func (s *Service) runProbe(ctx context.Context, id string) (*probe.Info, error) {
	f, e, err := s.files.Open(ctx, s.sys, id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fp := s.ffprobe(); fp != "" {
		if local, err := s.files.LocalPath(ctx, s.sys, id); err == nil {
			pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			info, err := probe.FFprobe(pctx, fp, local)
			cancel()
			if err == nil {
				return info, nil
			}
			s.log.Debug("media: ffprobe failed, using built-in parsers", "id", id, "err", err)
		}
	}
	// The built-in parsers are bounded on their own; the deadline is defence in depth (and
	// lets job shutdown or a cancelled request stop a probe of a slow file).
	pctx, cancel := context.WithTimeout(ctx, builtinProbeTimeout)
	defer cancel()
	return probe.ProbeContext(pctx, f, e.Size, e.Name)
}

func nullPositive(v float64) sql.NullFloat64 {
	if v > 0 && v < 1e8 {
		return sql.NullFloat64{Float64: v, Valid: true}
	}
	return sql.NullFloat64{}
}

func nullPositiveInt(v int) sql.NullInt64 {
	if v > 0 {
		return sql.NullInt64{Int64: int64(v), Valid: true}
	}
	return sql.NullInt64{}
}

// cleanText trims, removes control characters and truncates s to max bytes (on a rune
// boundary) — for values that come from files or upstream services.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError {
			return -1
		}
		if unicode.IsControl(r) {
			if r == '\n' || r == '\t' {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	return truncate(s, max)
}

// cleanMultiline is cleanText for long text: newlines are kept (CRLF normalised).
func cleanMultiline(s string, max int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), max)
}

// truncate cuts s to at most max bytes on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return strings.TrimSpace(s[:i])
}
