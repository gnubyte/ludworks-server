package music

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Albums are aggregated per caller from the tracks they can see (album_id groups tracks with
// the same folded album artist and title). Album-level filters are HAVING clauses so they look
// at the caller's tracks only.

// Album is the Album object of API.md §12.
type Album struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Artist          string   `json:"artist"`
	ArtistID        string   `json:"artist_id"`
	Year            *int64   `json:"year"`
	Genres          []string `json:"genres"`
	TrackCount      int64    `json:"track_count"`
	DiscCount       int64    `json:"disc_count"`
	DurationSeconds float64  `json:"duration_seconds"`
	Compilation     bool     `json:"compilation"`
	ArtURL          *string  `json:"art_url"`
	HasCustomArt    bool     `json:"has_custom_art"`
	AddedAt         string   `json:"added_at"`
	Favorite        bool     `json:"favorite"`
	PlayCount       int64    `json:"play_count"`
}

// AlbumDetail is GET /music/albums/{id}.
type AlbumDetail struct {
	Album
	Tracks []Track `json:"tracks"`
}

const albumBaseCols = `t.album_id, MIN(t.album), MIN(t.album_artist), MIN(t.album_artist_id), MAX(t.year), COUNT(*),
	MAX(MAX(COALESCE(t.disc_total, 0), COALESCE(t.disc_number, 1))), SUM(COALESCE(t.duration_seconds, 0)),
	MAX(t.compilation), MIN(t.added_at)`

const albumBaseFrom = `FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id`

// albumQuery returns the aggregate columns and FROM clause of an album query. The caller's
// favorite flag and plays need a join per track; they are aggregated here only when the query
// sorts or filters by them (the first argument is then the caller's user id), otherwise
// decorateAlbums adds them for the albums of the page.
func albumQuery(withUser bool) (cols, from string) {
	if !withUser {
		return albumBaseCols + `, 0, 0`, albumBaseFrom
	}
	return albumBaseCols + `, MAX(af.item_id IS NOT NULL), SUM(COALESCE(ps.play_count, 0))`, albumBaseFrom + `
		CROSS JOIN (SELECT ? AS uid) me
		LEFT JOIN music_play_stats ps ON ps.user_id = me.uid AND ps.file_id = t.file_id
		LEFT JOIN music_favorites af ON af.user_id = me.uid AND af.kind = 'album' AND af.item_id = t.album_id`
}

func scanAlbum(sc scanner, extra ...any) (*Album, error) {
	var (
		a           Album
		year        sql.NullInt64
		compilation int64
		added       db.Time
		fav         int64
	)
	dest := []any{&a.ID, &a.Title, &a.Artist, &a.ArtistID, &year, &a.TrackCount, &a.DiscCount, &a.DurationSeconds,
		&compilation, &added, &fav, &a.PlayCount}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	a.Year = intPtr(year)
	a.Compilation, a.Favorite = compilation == 1, fav == 1
	a.AddedAt = db.FormatTime(added.Time)
	a.Genres = []string{}
	return &a, nil
}

// albumFilter selects albums (GET /music/albums).
type albumFilter struct {
	Query       string // FTS5 MATCH expression
	NoMatch     bool
	ArtistID    string
	Genre       string // folded
	Year        *int64
	Favorite    *bool
	Compilation *bool
	SpaceID     string
}

var albumSorts = []string{"title", "artist", "year", "added", "plays", "random"}

func parseAlbumFilter(q url.Values) (albumFilter, error) {
	tf, err := parseTrackFilter(q)
	if err != nil {
		return albumFilter{}, err
	}
	f := albumFilter{Query: tf.Query, NoMatch: tf.NoMatch, ArtistID: tf.ArtistID, Genre: tf.Genre, Year: tf.Year, Favorite: tf.Favorite, SpaceID: tf.SpaceID}
	c, ok := triBool(q.Get("compilation"))
	if !ok {
		return f, core.BadRequest("compilation: must be true or false")
	}
	f.Compilation = c
	return f, nil
}

func albumSortKeys(sortBy, order string) []string {
	big := "1e18"
	if order == "desc" {
		big = "-1e18"
	}
	switch sortBy {
	case "artist":
		return []string{`MIN(t.sort_album_artist)`, `MIN(t.sort_album)`}
	case "year":
		return []string{`COALESCE(MAX(t.year), ` + big + `)`, `MIN(t.sort_album)`}
	case "added":
		return []string{`MIN(t.added_at)`}
	case "plays":
		return []string{`SUM(COALESCE(ps.play_count, 0))`, `MIN(t.sort_album)`}
	}
	return []string{`MIN(t.sort_album)`, `MIN(t.sort_album_artist)`}
}

// albumWhere builds the WHERE and HAVING parts of an album query (withUser: see albumQuery).
func albumWhere(userID string, spaces []string, f *albumFilter, withUser bool) (*whereBuilder, *whereBuilder) {
	w := &whereBuilder{}
	if withUser {
		w.args = []any{userID}
	}
	visibleWhere(w, spaces)
	w.add(`t.album_id <> ''`)
	if f.NoMatch {
		w.add(`0`)
	}
	if f.ArtistID != "" {
		w.add(`t.album_artist_id = ?`, f.ArtistID)
	}
	h := &whereBuilder{}
	if f.Query != "" {
		h.add(`MAX(t.file_id IN (SELECT fm.file_id FROM music_fts JOIN music_fts_map fm ON fm.rowid = music_fts.rowid
			WHERE music_fts MATCH ?))`, "{album album_artist} : ("+f.Query+")")
	}
	if f.Genre != "" {
		h.add(`MAX(EXISTS (SELECT 1 FROM music_track_genres g WHERE g.file_id = t.file_id AND g.genre_key = ?))`, f.Genre)
	}
	if f.Year != nil {
		h.add(`MAX(t.year) = ?`, *f.Year)
	}
	if f.Favorite != nil {
		h.add(`MAX(af.item_id IS NOT NULL) = ?`, db.Bool(*f.Favorite))
	}
	if f.Compilation != nil {
		h.add(`MAX(t.compilation) = ?`, db.Bool(*f.Compilation))
	}
	return w, h
}

// listAlbums returns one page of albums for p.
func (s *Service) listAlbums(ctx context.Context, p *core.Principal, f albumFilter, lq listQuery) ([]Album, string, error) {
	spaces, err := s.scope(ctx, p, f.SpaceID)
	if err != nil {
		return nil, "", err
	}
	hash := filterHash([]any{"albums", f, spaces, lq.Sort, lq.Order, p.UserID})
	withUser := lq.Sort == "plays" || f.Favorite != nil
	w, h := albumWhere(p.UserID, spaces, &f, withUser)
	cols, from := albumQuery(withUser)
	if lq.Sort == "random" {
		query := `SELECT t.album_id ` + from + ` WHERE ` + w.sql() + ` GROUP BY t.album_id`
		args := w.args
		if len(h.conds) > 0 {
			query += ` HAVING ` + h.sql()
			args = append(append([]any{}, args...), h.args...)
		}
		ids, err := s.queryIDs(ctx, query+` LIMIT ?`, append(args, maxRandomPool)...)
		if err != nil {
			return nil, "", err
		}
		page, next, err := randomPage(ids, lq, hash)
		if err != nil {
			return nil, "", err
		}
		albums, err := s.albumsByID(ctx, p, spaces, page)
		return albums, next, err
	}
	pq := pageQuery{cols: cols, from: from, where: w, group: "t.album_id", having: h,
		keys: albumSortKeys(lq.Sort, lq.Order), idExpr: "t.album_id"}
	albums, next, err := keysetPage(ctx, s.d.DB.SQL, pq, lq, hash, scanAlbum, func(a *Album) string { return a.ID })
	if err != nil {
		return nil, "", err
	}
	if err := s.decorateAlbums(ctx, p.UserID, spaces, albums, !withUser); err != nil {
		return nil, "", err
	}
	return derefAlbums(albums), next, nil
}

func derefAlbums(in []*Album) []Album {
	out := make([]Album, 0, len(in))
	for _, a := range in {
		out = append(out, *a)
	}
	return out
}

// albumsByID aggregates the given albums for p in the given order (invisible ones are skipped).
func (s *Service) albumsByID(ctx context.Context, p *core.Principal, spaces, ids []string) ([]Album, error) {
	if len(ids) == 0 {
		return []Album{}, nil
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`t.album_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	cols, from := albumQuery(false)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT `+cols+` `+from+` WHERE `+w.sql()+` GROUP BY t.album_id`, w.args...)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Album{}
	var all []*Album
	for rs.Next() {
		a, err := scanAlbum(rs)
		if err != nil {
			rs.Close()
			return nil, err
		}
		byID[a.ID] = a
		all = append(all, a)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if err := s.decorateAlbums(ctx, p.UserID, spaces, all, true); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id]; ok {
			out = append(out, *a)
		}
	}
	return out, nil
}

// albumArtInfo is the art an album shows to a caller.
type albumArtInfo struct {
	art    string
	custom bool
}

// albumArts resolves the art of albums from the caller's tracks: custom art set for one of the
// spaces holding them, else the first track (by disc and number) with art.
func (s *Service) albumArts(ctx context.Context, spaces, ids []string) (map[string]albumArtInfo, error) {
	out := map[string]albumArtInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	folderArt := s.settings(ctx).FolderArt
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`t.album_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.album_id, aa.art_id, t.art_id, fa.art_id FROM music_tracks t
		CROSS JOIN files f ON f.id = t.file_id
		LEFT JOIN music_album_art aa ON aa.album_id = t.album_id AND aa.space_id = f.space_id
		LEFT JOIN music_folder_art fa ON fa.file_id = t.folder_image_id AND fa.state = 1
		WHERE `+w.sql()+` ORDER BY t.album_id, aa.art_id IS NULL, aa.updated_at DESC, COALESCE(t.disc_number, 1e9),
		COALESCE(t.track_number, 1e9), t.sort_title, t.file_id`, w.args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var (
			id                string
			custom, emb, fold sql.NullString
		)
		if err := rs.Scan(&id, &custom, &emb, &fold); err != nil {
			return nil, err
		}
		if cur, ok := out[id]; ok && cur.art != "" {
			continue
		}
		switch {
		case custom.Valid:
			out[id] = albumArtInfo{art: custom.String, custom: true}
		case emb.Valid:
			out[id] = albumArtInfo{art: emb.String}
		case folderArt && fold.Valid:
			out[id] = albumArtInfo{art: fold.String}
		}
	}
	return out, rs.Err()
}

// decorateAlbums adds art, genres and (perUser) the caller's favorite flags and plays to
// aggregated albums.
func (s *Service) decorateAlbums(ctx context.Context, userID string, spaces []string, albums []*Album, perUser bool) error {
	if len(albums) == 0 {
		return nil
	}
	ids := make([]string, len(albums))
	for i, a := range albums {
		ids[i] = a.ID
	}
	arts, err := s.albumArts(ctx, spaces, ids)
	if err != nil {
		return err
	}
	if perUser {
		if err := s.albumUserState(ctx, userID, spaces, ids, albums); err != nil {
			return err
		}
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`t.album_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.album_id, MIN(g.name), COUNT(*) FROM music_tracks t
		CROSS JOIN files f ON f.id = t.file_id JOIN music_track_genres g ON g.file_id = t.file_id
		WHERE `+w.sql()+` GROUP BY t.album_id, g.genre_key ORDER BY t.album_id, COUNT(*) DESC, MIN(g.position), g.genre_key`, w.args...)
	if err != nil {
		return err
	}
	genres := map[string][]string{}
	for rs.Next() {
		var (
			id, name string
			n        int
		)
		if err := rs.Scan(&id, &name, &n); err != nil {
			rs.Close()
			return err
		}
		if len(genres[id]) < 10 {
			genres[id] = append(genres[id], name)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, a := range albums {
		if g := genres[a.ID]; g != nil {
			a.Genres = g
		}
		if ai := arts[a.ID]; ai.art != "" {
			a.ArtURL = strPtr("/api/v1/music/albums/" + a.ID + "/art?v=" + artVersion(ai.art))
			a.HasCustomArt = ai.custom
		}
	}
	return nil
}

// albumUserState fills the caller's favorite flags and play counts of albums.
func (s *Service) albumUserState(ctx context.Context, userID string, spaces, ids []string, albums []*Album) error {
	favs, err := s.queryIDs(ctx, `SELECT item_id FROM music_favorites WHERE user_id = ? AND kind = 'album'
		AND item_id IN (SELECT value FROM json_each(?))`, userID, mustJSON(ids))
	if err != nil {
		return err
	}
	isFav := map[string]bool{}
	for _, id := range favs {
		isFav[id] = true
	}
	w := &whereBuilder{args: []any{userID}}
	visibleWhere(w, spaces)
	w.add(`t.album_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.album_id, SUM(ps.play_count) FROM music_tracks t
		CROSS JOIN files f ON f.id = t.file_id
		JOIN music_play_stats ps ON ps.user_id = ? AND ps.file_id = t.file_id
		WHERE `+strings.Join(w.conds, " AND ")+` GROUP BY t.album_id`, w.args...)
	if err != nil {
		return err
	}
	plays := map[string]int64{}
	for rs.Next() {
		var (
			id string
			n  int64
		)
		if err := rs.Scan(&id, &n); err != nil {
			rs.Close()
			return err
		}
		plays[id] = n
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, a := range albums {
		a.Favorite, a.PlayCount = isFav[a.ID], plays[a.ID]
	}
	return nil
}

// album resolves one album for p from their tracks (404 when they have none).
func (s *Service) album(ctx context.Context, p *core.Principal, id string) (*Album, []string, error) {
	if !validEntityID(id) {
		return nil, nil, errNoAlbum
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, nil, err
	}
	albums, err := s.albumsByID(ctx, p, spaces, []string{id})
	if err != nil {
		return nil, nil, err
	}
	if len(albums) == 0 {
		return nil, nil, errNoAlbum
	}
	return &albums[0], spaces, nil
}

var errNoAlbum = core.NotFound("Album not found")

// validEntityID checks the shape of album and artist ids (26 lowercase base32 characters).
func validEntityID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			return false
		}
	}
	return true
}

// albumDetail is an album with its tracks (by disc, track number, title).
func (s *Service) albumDetail(ctx context.Context, p *core.Principal, id string) (*AlbumDetail, error) {
	a, spaces, err := s.album(ctx, p, id)
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(w, spaces)
	w.add(`t.album_id = ?`, id)
	trs, err := queryTracks(ctx, s.d.DB.SQL, `SELECT `+trackCols+` `+trackFrom+` WHERE `+w.sql()+`
		ORDER BY COALESCE(t.disc_number, 1), COALESCE(t.track_number, 1e9), t.sort_title, t.file_id LIMIT 10000`, w.args...)
	if err != nil {
		return nil, err
	}
	return &AlbumDetail{Album: *a, Tracks: s.renderTracks(ctx, trs)}, nil
}

// albumArtID is the art an album shows to p ("" when none).
func (s *Service) albumArtID(ctx context.Context, p *core.Principal, id string) (string, error) {
	if _, _, err := s.album(ctx, p, id); err != nil {
		return "", err
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return "", err
	}
	arts, err := s.albumArts(ctx, spaces, []string{id})
	if err != nil {
		return "", err
	}
	return arts[id].art, nil
}

// albumSpaces returns the spaces holding p's tracks of an album, checking write access to
// every one of them (custom album art applies per space).
func (s *Service) albumSpaces(ctx context.Context, p *core.Principal, id string) ([]string, error) {
	if _, _, err := s.album(ctx, p, id); err != nil {
		return nil, err
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`t.album_id = ?`, id)
	ids, err := s.queryIDs(ctx, `SELECT DISTINCT f.space_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id WHERE `+w.sql(), w.args...)
	if err != nil {
		return nil, err
	}
	for _, sp := range ids {
		if !s.files.CanWrite(ctx, p, sp) {
			return nil, core.Forbidden("Album art can only be changed with write access to every space holding the album")
		}
	}
	return ids, nil
}

// setAlbumArt stores custom art for an album (per space holding p's tracks of it).
func (s *Service) setAlbumArt(ctx context.Context, p *core.Principal, id string, body io.Reader) (*Album, error) {
	spaces, err := s.albumSpaces(ctx, p, id)
	if err != nil {
		return nil, err
	}
	artID, err := s.normalizeUpload(ctx, body)
	if err != nil {
		return nil, err
	}
	now := db.FormatTime(s.now())
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, sp := range spaces {
			if _, err := tx.ExecContext(ctx, `INSERT INTO music_album_art (album_id, space_id, art_id, set_by, updated_at)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT (album_id, space_id) DO UPDATE SET art_id = excluded.art_id,
				set_by = excluded.set_by, updated_at = excluded.updated_at`, id, sp, artID, p.UserID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	a, _, err := s.album(ctx, p, id)
	return a, err
}

// clearAlbumArt removes custom art of an album in the spaces holding p's tracks of it.
func (s *Service) clearAlbumArt(ctx context.Context, p *core.Principal, id string) error {
	spaces, err := s.albumSpaces(ctx, p, id)
	if err != nil {
		return err
	}
	_, err = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_album_art WHERE album_id = ? AND space_id IN (SELECT value FROM json_each(?))`,
		id, mustJSON(spaces))
	return err
}
