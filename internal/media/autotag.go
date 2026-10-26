package media

import (
	"context"
	"database/sql"
	"io"
	"path"
	"slices"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/media/naming"
)

// maxAutoTagFiles bounds one auto-tag request.
const maxAutoTagFiles = 20000

// AutoTagRequest is POST /media/auto-tag.
type AutoTagRequest struct {
	SpaceID   string `json:"space_id"`
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
	Overwrite bool   `json:"overwrite"`
}

// AutoTagResult is the auto-tag summary.
type AutoTagResult struct {
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

type tagFile struct {
	id, parentID, path, name, category, subcategory string
}

type nfoFile struct {
	id, dir, name string
}

// cleanDirPath normalises a space-relative directory path ("" = root).
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

// subtreeBounds returns [lo, hi) such that every descendant path of dir satisfies
// lo <= path < hi (BINARY collation): dir + "/" .. dir + "0" ('0' follows '/').
func subtreeBounds(dir string) (string, string) { return dir + "/", dir + "0" }

// autoTag parses file names (and Kodi .nfo sidecars) under a folder and fills metadata.
func (s *Service) autoTag(ctx context.Context, p *core.Principal, req AutoTagRequest) (*AutoTagResult, error) {
	if !httpx.ValidID(req.SpaceID) || !s.files.CanRead(ctx, p, req.SpaceID) {
		return nil, core.NotFound("Space not found")
	}
	if !s.files.CanWrite(ctx, p, req.SpaceID) {
		return nil, core.Forbidden("You only have read access to this space")
	}
	dir, ok := cleanDirPath(req.Path)
	if !ok || len(req.Path) > 4096 {
		return nil, core.Validation(map[string]string{"path": "invalid path"})
	}
	recursive := req.Recursive == nil || *req.Recursive
	var dirID string
	if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT id FROM files WHERE space_id = ? AND path = ? AND is_dir = 1 AND trashed_at IS NULL`,
		req.SpaceID, dir).Scan(&dirID); err != nil {
		if isNoRows(err) {
			return nil, core.NotFound("Folder not found")
		}
		return nil, err
	}

	// Candidate media files and every .nfo in scope.
	cond, args := `f.parent_id = ?`, []any{dirID}
	if recursive {
		if dir == "" {
			cond, args = `f.path <> ''`, nil
		} else {
			lo, hi := subtreeBounds(dir)
			cond, args = `f.path >= ? AND f.path < ?`, []any{lo, hi}
		}
	}
	base := `FROM files f WHERE f.space_id = ? AND f.trashed_at IS NULL AND f.is_dir = 0 AND ` + cond
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, COALESCE(f.parent_id, ''), f.path, f.name, f.category, f.subcategory `+base+
		` AND `+eligibleSQL+` ORDER BY f.path LIMIT ?`, append(append([]any{req.SpaceID}, args...), maxAutoTagFiles+1)...)
	if err != nil {
		return nil, err
	}
	var cands []tagFile
	for rs.Next() {
		var t tagFile
		if err := rs.Scan(&t.id, &t.parentID, &t.path, &t.name, &t.category, &t.subcategory); err != nil {
			rs.Close()
			return nil, err
		}
		cands = append(cands, t)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(cands) > maxAutoTagFiles {
		return nil, core.Validation(map[string]string{"path": "too many media files in this folder (max 20000); choose a subfolder"})
	}
	nfos, err := s.nfoIndex(ctx, req.SpaceID, base, args, dir)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.id
	}
	if err := s.ensureRows(ctx, ids); err != nil {
		return nil, err
	}
	res := &AutoTagResult{}
	cache := map[string]*naming.NFO{}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changed, err := s.autoTagOne(ctx, p, c, nfos, cache, req.Overwrite)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Updated++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

// nfoIndex maps directory path → .nfo files there (the scanned subtree plus tvshow.nfo in
// the ancestors of the scanned folder).
func (s *Service) nfoIndex(ctx context.Context, spaceID, base string, args []any, dir string) (map[string][]nfoFile, error) {
	out := map[string][]nfoFile{}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.path, f.name `+base+` AND lower(f.name) LIKE '%.nfo' AND f.size <= ? LIMIT 50000`,
		append(append([]any{spaceID}, args...), naming.MaxNFOBytes)...)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var n nfoFile
		var p string
		if err := rs.Scan(&n.id, &p, &n.name); err != nil {
			rs.Close()
			return nil, err
		}
		n.dir = parentPath(p)
		out[n.dir] = append(out[n.dir], n)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	// Ancestors of the scanned folder (a season folder's show folder holds tvshow.nfo).
	for d := dir; d != ""; {
		d = parentPath(d)
		var n nfoFile
		p := strings.TrimPrefix(d+"/tvshow.nfo", "/")
		err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT id, name FROM files WHERE space_id = ? AND path = ? COLLATE NOCASE
			AND is_dir = 0 AND trashed_at IS NULL AND size <= ?`, spaceID, p, naming.MaxNFOBytes).Scan(&n.id, &n.name)
		if err == nil {
			n.dir = d
			out[d] = append(out[d], n)
		} else if !isNoRows(err) {
			return nil, err
		}
	}
	return out, nil
}

func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// readNFO parses an .nfo file the caller can read (nil on any problem).
func (s *Service) readNFO(ctx context.Context, p *core.Principal, id string, cache map[string]*naming.NFO) *naming.NFO {
	if n, ok := cache[id]; ok {
		return n
	}
	var out *naming.NFO
	if f, _, err := s.files.Open(ctx, p, id); err == nil {
		n, err := naming.ParseNFO(io.LimitReader(f, naming.MaxNFOBytes+1))
		f.Close()
		if err == nil {
			out = n
		}
	}
	cache[id] = out
	return out
}

func findNFO(list []nfoFile, names ...string) *nfoFile {
	for _, want := range names {
		for i := range list {
			if strings.EqualFold(list[i].name, want) {
				return &list[i]
			}
		}
	}
	return nil
}

// tagGuess combines the parsed path with NFO data.
func (s *Service) tagGuess(ctx context.Context, p *core.Principal, c tagFile, nfos map[string][]nfoFile, cache map[string]*naming.NFO) guess {
	g := guessFromPath(c.path)
	dir := parentPath(c.path)
	stem := strings.TrimSuffix(c.name, path.Ext(c.name))
	var own *naming.NFO
	if n := findNFO(nfos[dir], stem+".nfo"); n != nil {
		own = s.readNFO(ctx, p, n.id, cache)
	}
	if own == nil {
		if n := findNFO(nfos[dir], "movie.nfo"); n != nil {
			own = s.readNFO(ctx, p, n.id, cache)
		}
	}
	var show *naming.NFO
	for d := dir; ; d = parentPath(d) {
		if n := findNFO(nfos[d], "tvshow.nfo"); n != nil {
			show = s.readNFO(ctx, p, n.id, cache)
			break
		}
		if d == "" {
			break
		}
	}
	if own != nil {
		switch own.Kind {
		case naming.NFOMovie:
			g.kind = naming.KindMovie
			g.showTitle, g.epTitle, g.season, g.episode = "", "", sql.NullInt64{}, sql.NullInt64{}
		case naming.NFOEpisode:
			g.kind = naming.KindTV
		}
	}
	if show != nil && g.kind != naming.KindMovie {
		g.kind = naming.KindTV
		if t := cleanText(show.Title, maxTitle); t != "" {
			g.showTitle = t
		}
		g.showDescription = cleanMultiline(show.Plot, maxDescription)
		if len(show.Genres) > 0 {
			g.genres, _ = normalizeGenres(show.Genres)
		}
		g.rating = cleanText(show.MPAA, maxRating)
		if show.TMDBID > 0 {
			g.tmdb = sql.NullInt64{Int64: show.TMDBID, Valid: true}
		}
		if show.Year > 0 && !g.year.Valid {
			g.year = sql.NullInt64{Int64: int64(show.Year), Valid: true}
		}
	}
	if own != nil {
		if g.kind == naming.KindTV {
			if t := cleanText(own.ShowTitle, maxTitle); t != "" && show == nil {
				g.showTitle = t
			}
			if own.Season != nil {
				g.season = sql.NullInt64{Int64: int64(*own.Season), Valid: true}
			}
			if own.Episode != nil {
				g.episode = sql.NullInt64{Int64: int64(*own.Episode), Valid: true}
			}
			if t := cleanText(own.Title, maxTitle); t != "" {
				g.epTitle = t
			}
		} else if t := cleanText(own.Title, maxTitle); t != "" {
			g.title = t
			g.sortTitle = cleanText(own.SortTitle, maxTitle)
		}
		if own.Year > 0 {
			g.year = sql.NullInt64{Int64: int64(own.Year), Valid: true}
		}
		g.description = cleanMultiline(own.Plot, maxDescription)
		if len(own.Genres) > 0 {
			g.genres, _ = normalizeGenres(own.Genres)
		}
		if r := cleanText(own.MPAA, maxRating); r != "" {
			g.rating = r
		}
		if own.TMDBID > 0 && (g.kind == naming.KindMovie || !g.tmdb.Valid) {
			g.tmdb = sql.NullInt64{Int64: own.TMDBID, Valid: true}
		}
		if imdbRe.MatchString(own.IMDBID) {
			g.imdb = own.IMDBID
		}
	}
	if g.kind == naming.KindTV && g.showTitle != "" {
		switch {
		case g.epTitle != "":
			g.title = g.epTitle
		case g.season.Valid && g.episode.Valid:
			g.title = g.showTitle + " S" + pad2(g.season.Int64) + "E" + pad2(g.episode.Int64)
		}
	}
	return g
}

func pad2(n int64) string {
	s := itoa(int(n))
	if len(s) < 2 {
		s = "0" + s
	}
	return s
}

// autoTagOne applies the guess to one item; it reports whether anything changed.
func (s *Service) autoTagOne(ctx context.Context, p *core.Principal, c tagFile, nfos map[string][]nfoFile, cache map[string]*naming.NFO, overwrite bool) (bool, error) {
	it, err := loadItem(ctx, s.d.DB.SQL, p.UserID, c.id)
	if err != nil || it == nil {
		return false, err
	}
	g := s.tagGuess(ctx, p, c, nfos, cache)
	if g.kind == naming.KindUnknown {
		return false, nil
	}
	before := *it
	before.Genres = slices.Clone(it.Genres)
	replace := overwrite || it.MetaSource == srcFilename
	setStr := func(dst *string, v string) {
		if v != "" && (replace || *dst == "") {
			*dst = v
		}
	}
	setInt := func(dst *sql.NullInt64, v sql.NullInt64) {
		if v.Valid && (replace || !dst.Valid) {
			*dst = v
		}
	}
	if replace {
		// Identity fields follow the guess entirely (a movie loses stale show fields).
		it.Title = g.title
		it.SortTitle = g.sortTitle
		if it.SortTitle == "" {
			it.SortTitle = sortTitleFor(it.Title)
		}
		it.ShowTitle, it.ShowKey = g.showTitle, naming.ShowKey(g.showTitle)
		it.Season, it.Episode, it.EpisodeTitle = g.season, g.episode, g.epTitle
		if g.year.Valid {
			it.Year = g.year
		}
	} else {
		if it.ShowTitle == "" && g.showTitle != "" {
			it.ShowTitle, it.ShowKey = g.showTitle, naming.ShowKey(g.showTitle)
		}
		setInt(&it.Season, g.season)
		setInt(&it.Episode, g.episode)
		setStr(&it.EpisodeTitle, g.epTitle)
		setInt(&it.Year, g.year)
	}
	setStr(&it.Description, g.description)
	setStr(&it.ContentRating, g.rating)
	setStr(&it.IMDBID, g.imdb)
	setStr(&it.ShowDescription, g.showDescription)
	setInt(&it.TMDBID, g.tmdb)
	if len(g.genres) > 0 && (replace || len(it.Genres) == 0) {
		it.Genres = g.genres
	}
	changed := !sameDescriptive(&before, it)
	if changed {
		source := srcAutotag
		if !replace && it.MetaSource != srcFilename {
			source = it.MetaSource // filling gaps keeps the record's provenance
		}
		if err := s.saveDescriptive(ctx, s.d.DB.SQL, it, source); err != nil {
			return false, err
		}
	} else if it.MetaSource == srcFilename {
		// Confirmed by auto-tag: later runs only fill gaps.
		if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE media_items SET meta_source = 'autotag' WHERE file_id = ?`, c.id); err != nil {
			return false, err
		}
	}
	cat, sub := "movie", "feature"
	if g.kind == naming.KindTV {
		cat, sub = "tv", "episode"
	}
	if c.category == "" || (overwrite && c.category != cat) {
		if c.category == cat {
			sub = c.subcategory
		}
		if _, err := s.files.SetMeta(ctx, p, c.id, &cat, &sub, nil); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func sameDescriptive(a, b *item) bool {
	return a.Title == b.Title && a.SortTitle == b.SortTitle && a.Year == b.Year && a.Description == b.Description &&
		slices.Equal(a.Genres, b.Genres) && a.ContentRating == b.ContentRating && a.ShowTitle == b.ShowTitle &&
		a.ShowKey == b.ShowKey && a.Season == b.Season && a.Episode == b.Episode && a.EpisodeTitle == b.EpisodeTitle &&
		a.TMDBID == b.TMDBID && a.IMDBID == b.IMDBID && a.ShowDescription == b.ShowDescription
}
