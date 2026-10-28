package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Manual edits (PATCH / bulk), re-reads, ratings and favorites.

// trackPatch is a validated set of track field changes: the manual values (JSON, merged into
// music_tracks.manual) and the subcategory (a files column).
type trackPatch struct {
	manual      map[string]json.RawMessage
	subcategory *string
}

func (tp *trackPatch) empty() bool { return len(tp.manual) == 0 && tp.subcategory == nil }

// parseTrackPatch validates a JSON object of track fields. Unknown and computed fields are
// ignored (so a GET result can be sent back).
func parseTrackPatch(raw map[string]json.RawMessage) (*trackPatch, error) {
	tp := &trackPatch{manual: map[string]json.RawMessage{}}
	fe := map[string]string{}
	setJSON := func(key string, v any) {
		b, _ := json.Marshal(v)
		tp.manual[key] = b
	}
	for _, key := range []string{"title", "artist", "album", "album_artist", "composer"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if !isNull(v) {
			if err := json.Unmarshal(v, &s); err != nil {
				fe[key] = "must be a string"
				continue
			}
		}
		if strings.ContainsFunc(s, unicode.IsControl) {
			fe[key] = "must not contain control characters"
			continue
		}
		s = strings.TrimSpace(s)
		if len([]rune(s)) > maxText {
			fe[key] = "too long (max " + strconv.Itoa(maxText) + " characters)"
			continue
		}
		setJSON(key, s)
	}
	for _, key := range []string{"artists", "genres"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var l []string
		if !isNull(v) {
			if err := json.Unmarshal(v, &l); err != nil {
				fe[key] = "must be an array of strings"
				continue
			}
		}
		maxItems, maxRunes := maxArtists, maxText
		if key == "genres" {
			maxItems, maxRunes = maxGenres, maxGenreRunes
		}
		if len(l) > maxItems {
			fe[key] = "at most " + strconv.Itoa(maxItems) + " values"
			continue
		}
		bad := false
		for _, s := range l {
			if strings.ContainsFunc(s, unicode.IsControl) || len([]rune(s)) > maxRunes {
				bad = true
			}
		}
		if bad {
			fe[key] = "values must be single-line and at most " + strconv.Itoa(maxRunes) + " characters"
			continue
		}
		norm := normalizeList(l, maxItems, maxRunes, key == "genres")
		if norm == nil {
			norm = []string{}
		}
		setJSON(key, norm)
	}
	for _, key := range []string{"track_number", "track_total", "disc_number", "disc_total", "year"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if isNull(v) {
			tp.manual[key] = json.RawMessage("null")
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || f != math.Trunc(f) || f < 1 || f > 9999 {
			fe[key] = "must be an integer between 1 and 9999, or null"
			continue
		}
		setJSON(key, int(f))
	}
	if v, ok := raw["compilation"]; ok {
		var b bool
		if isNull(v) || json.Unmarshal(v, &b) != nil {
			fe["compilation"] = "must be a boolean"
		} else {
			setJSON("compilation", b)
		}
	}
	if v, ok := raw["subcategory"]; ok {
		var s string
		if !isNull(v) {
			if err := json.Unmarshal(v, &s); err != nil {
				fe["subcategory"] = "must be a string"
			}
		}
		s = strings.TrimSpace(s)
		if len(s) > 64 {
			fe["subcategory"] = "too long"
		}
		tp.subcategory = &s
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	return tp, nil
}

// applyPatch stores manual values for a track p may write and re-derives its values.
func (s *Service) applyPatch(ctx context.Context, p *core.Principal, fileID string, tp *trackPatch) error {
	tr, err := s.trackFor(ctx, p, fileID, needWrite)
	if err != nil {
		return err
	}
	if tp.subcategory != nil && *tp.subcategory != tr.Subcategory {
		var cat *string
		e, err := s.files.Get(ctx, p, fileID)
		if err != nil {
			return err
		}
		if *tp.subcategory != "" && e.Category == "" {
			music := "music" // a subcategory needs a category; never anything but music
			cat = &music
		}
		if _, err := s.files.SetMeta(ctx, p, fileID, cat, tp.subcategory, nil); err != nil {
			return err
		}
	}
	if len(tp.manual) == 0 {
		return nil
	}
	return s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var cur db.JSON[map[string]json.RawMessage]
		if err := tx.QueryRowContext(ctx, `SELECT manual FROM music_tracks WHERE file_id = ?`, fileID).Scan(&cur); err != nil {
			if err == sql.ErrNoRows {
				return errNotTrack
			}
			return err
		}
		m := cur.V
		if m == nil {
			m = map[string]json.RawMessage{}
		}
		for k, v := range tp.manual {
			m[k] = v
		}
		if _, err := tx.ExecContext(ctx, `UPDATE music_tracks SET manual = ? WHERE file_id = ?`,
			db.JSON[map[string]json.RawMessage]{V: m}, fileID); err != nil {
			return err
		}
		return s.recompute(ctx, tx, fileID)
	})
}

// ItemError is a per-item failure of a bulk request.
type ItemError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func itemError(id string, err error) ItemError {
	if ce, ok := err.(*core.Error); ok {
		return ItemError{ID: id, Error: ce.Message, Code: ce.Code}
	}
	return ItemError{ID: id, Error: "internal error", Code: "internal"}
}

// bulkPatch applies a patch to several tracks; failures are reported per id.
func (s *Service) bulkPatch(ctx context.Context, p *core.Principal, ids []string, tp *trackPatch) (int, []ItemError, error) {
	updated := 0
	errs := []ItemError{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := s.applyPatch(ctx, p, id, tp); err != nil {
			if _, ok := err.(*core.Error); !ok {
				if ctx.Err() != nil {
					return updated, errs, ctx.Err()
				}
				s.log.Warn("music: bulk edit", "id", id, "err", err)
			}
			errs = append(errs, itemError(id, err))
			continue
		}
		updated++
	}
	return updated, errs, nil
}

// reprobe re-reads a track's tags and art now (reset drops manual edits first).
func (s *Service) reprobe(ctx context.Context, p *core.Principal, fileID string, reset bool) error {
	if _, err := s.trackFor(ctx, p, fileID, needWrite); err != nil {
		return err
	}
	release, ok := s.probes.tryAcquire(p.UserID)
	if !ok {
		return errProbeBusy
	}
	defer release()
	if reset {
		if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_tracks SET manual = '{}' WHERE file_id = ?`, fileID); err != nil {
			return err
		}
	}
	tr, err := loadTrack(ctx, s.d.DB.SQL, p.UserID, fileID)
	if err != nil || tr == nil {
		return err
	}
	var parentID string
	if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM files WHERE id = ?`, fileID).Scan(&parentID); err != nil {
		return err
	}
	if parentID != "" {
		if err := s.refreshDir(ctx, tr.SpaceID, parentID); err != nil {
			return err
		}
		s.drainFolderArtNow(ctx, fileID)
	}
	return s.scanTrack(ctx, fileID)
}

// drainFolderArtNow decodes the folder image of a track synchronously when it is pending.
func (s *Service) drainFolderArtNow(ctx context.Context, fileID string) {
	var imgID, key string
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT a.file_id, a.source_key FROM music_tracks t
		JOIN music_folder_art a ON a.file_id = t.folder_image_id WHERE t.file_id = ? AND a.state = 0`, fileID).Scan(&imgID, &key)
	if err == nil {
		s.decodeFolderImage(ctx, imgID, key)
	}
}

// setRating sets or clears the caller's rating of a track.
func (s *Service) setRating(ctx context.Context, p *core.Principal, fileID string, rating *int) error {
	if _, err := s.trackFor(ctx, p, fileID, needRead); err != nil {
		return err
	}
	if rating == nil {
		_, err := s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_ratings WHERE user_id = ? AND file_id = ?`, p.UserID, fileID)
		return err
	}
	_, err := s.d.DB.SQL.ExecContext(ctx, `INSERT INTO music_ratings (user_id, file_id, rating, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, file_id) DO UPDATE SET rating = excluded.rating, updated_at = excluded.updated_at`,
		p.UserID, fileID, *rating, db.FormatTime(s.now()))
	return err
}

// FavoriteRequest is PUT /music/favorites.
type FavoriteRequest struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Favorite *bool  `json:"favorite"`
}

// setFavorite marks or unmarks a track, album or artist for the caller. Marking needs the
// item to be visible; unmarking always works.
func (s *Service) setFavorite(ctx context.Context, p *core.Principal, req FavoriteRequest) error {
	fe := map[string]string{}
	if req.Kind != "track" && req.Kind != "album" && req.Kind != "artist" {
		fe["kind"] = "must be track, album or artist"
	}
	if !httpx.ValidID(req.ID) {
		fe["id"] = "required"
	}
	if req.Favorite == nil {
		fe["favorite"] = "required"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	now := db.FormatTime(s.now())
	if !*req.Favorite {
		var err error
		if req.Kind == "track" {
			_, err = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_favorite_tracks WHERE user_id = ? AND file_id = ?`, p.UserID, req.ID)
		} else {
			_, err = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_favorites WHERE user_id = ? AND kind = ? AND item_id = ?`,
				p.UserID, req.Kind, req.ID)
		}
		return err
	}
	switch req.Kind {
	case "track":
		if _, err := s.trackFor(ctx, p, req.ID, needRead); err != nil {
			return err
		}
		_, err := s.d.DB.SQL.ExecContext(ctx, `INSERT INTO music_favorite_tracks (user_id, file_id, created_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, p.UserID, req.ID, now)
		return err
	case "album":
		if _, _, err := s.album(ctx, p, req.ID); err != nil {
			return err
		}
	case "artist":
		if _, _, err := s.artist(ctx, p, req.ID); err != nil {
			return err
		}
	}
	_, err := s.d.DB.SQL.ExecContext(ctx, `INSERT INTO music_favorites (user_id, kind, item_id, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, p.UserID, req.Kind, req.ID, now)
	return err
}

// rescan re-reads the tags of the caller's music items below a folder (all their music
// without a space) in the background.
func (s *Service) rescan(ctx context.Context, p *core.Principal, spaceID, dirPath string) error {
	var spaces []string
	if spaceID != "" {
		if !httpx.ValidID(spaceID) || !s.files.CanRead(ctx, p, spaceID) {
			return core.NotFound("Space not found")
		}
		spaces = []string{spaceID}
	} else {
		var err error
		if spaces, err = s.scope(ctx, p, ""); err != nil {
			return err
		}
		if dirPath != "" {
			return core.Validation(map[string]string{"path": "requires space_id"})
		}
	}
	dir, ok := cleanDirPath(dirPath)
	if !ok || len(dirPath) > 4096 {
		return core.Validation(map[string]string{"path": "invalid path"})
	}
	if dir != "" {
		var n int
		if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE space_id = ? AND path = ? AND is_dir = 1
			AND trashed_at IS NULL`, spaceID, dir).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return core.NotFound("Folder not found")
		}
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	if dir != "" {
		w.add(`f.path >= ? AND f.path < ?`, dir+"/", dir+"0")
	}
	if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_tracks SET probe_state = 0 WHERE file_id IN (
		SELECT f.id FROM files f WHERE `+w.sql()+`)`, w.args...); err != nil {
		return err
	}
	// Files that were never ingested (e.g. while the module was off) are picked up as well.
	s.d.Jobs.GoOnce("music.reconcile", s.reconcile)
	s.kickProbe()
	return nil
}

// cleanDirPath normalises a folder path ("" = root); false for invalid paths.
func cleanDirPath(p string) (string, bool) {
	p = strings.Trim(strings.ReplaceAll(p, `\`, "/"), "/")
	if p == "" {
		return "", true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsRune(seg, 0) {
			return "", false
		}
	}
	return p, true
}
