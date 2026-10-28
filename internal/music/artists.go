package music

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Artists are aggregated per caller from music_track_artists over the tracks they can see:
// album artists (role bit 1) and, with role=all, track artists that only appear on other
// artists' albums (role bit 2).

// Artist is the Artist object of API.md §12.
type Artist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	AlbumCount int64   `json:"album_count"`
	TrackCount int64   `json:"track_count"`
	ArtURL     *string `json:"art_url"`
	Favorite   bool    `json:"favorite"`
}

// ArtistDetail is GET /music/artists/{id}.
type ArtistDetail struct {
	Artist
	Albums    []Album `json:"albums"`
	AppearsOn []Album `json:"appears_on"`
	TopTracks []Track `json:"top_tracks"`
}

const artistBaseCols = `ta.artist_id, MIN(ta.name),
	COUNT(DISTINCT CASE WHEN ta.role & 1 AND t.album_id <> '' THEN t.album_id END), COUNT(*)`

const artistBaseFrom = `FROM music_track_artists ta
	CROSS JOIN music_tracks t ON t.file_id = ta.file_id
	CROSS JOIN files f ON f.id = t.file_id`

// artistQuery returns the columns and FROM clause of an artist query; like albumQuery, the
// per-user joins are only made when the query sorts or filters by them.
func artistQuery(withUser bool) (cols, from string) {
	if !withUser {
		return artistBaseCols + `, 0`, artistBaseFrom
	}
	return artistBaseCols + `, MAX(arf.item_id IS NOT NULL)`, artistBaseFrom + `
		CROSS JOIN (SELECT ? AS uid) me
		LEFT JOIN music_play_stats ps ON ps.user_id = me.uid AND ps.file_id = t.file_id
		LEFT JOIN music_favorites arf ON arf.user_id = me.uid AND arf.kind = 'artist' AND arf.item_id = ta.artist_id`
}

func scanArtist(sc scanner, extra ...any) (*Artist, error) {
	var (
		a   Artist
		fav int64
	)
	if err := sc.Scan(append([]any{&a.ID, &a.Name, &a.AlbumCount, &a.TrackCount, &fav}, extra...)...); err != nil {
		return nil, err
	}
	a.Favorite = fav == 1
	return &a, nil
}

// artistFilter selects artists (GET /music/artists).
type artistFilter struct {
	Terms    []string // folded search terms (substring match on the name)
	NoMatch  bool
	Genre    string
	Favorite *bool
	All      bool // role=all
}

var artistSorts = []string{"name", "added", "plays", "random"}

func parseArtistFilter(q url.Values) (artistFilter, error) {
	var f artistFilter
	if v := q.Get("q"); v != "" {
		if len(v) > 500 {
			return f, core.BadRequest("q: too long")
		}
		f.Terms = queryTerms(v)
		f.NoMatch = len(f.Terms) == 0
	}
	if v := strings.TrimSpace(q.Get("genre")); v != "" {
		if len(v) > 200 {
			return f, core.BadRequest("genre: too long")
		}
		f.Genre = fold(v)
	}
	fav, ok := triBool(q.Get("favorite"))
	if !ok {
		return f, core.BadRequest("favorite: must be true or false")
	}
	f.Favorite = fav
	switch q.Get("role") {
	case "", "album":
	case "all":
		f.All = true
	default:
		return f, core.BadRequest("role: must be album or all")
	}
	return f, nil
}

func artistSortKeys(sortBy string) []string {
	switch sortBy {
	case "added":
		return []string{`MIN(t.added_at)`}
	case "plays":
		return []string{`SUM(COALESCE(ps.play_count, 0))`, `MIN(ta.sort_name)`}
	}
	return []string{`MIN(ta.sort_name)`}
}

func artistWhere(userID string, spaces []string, f *artistFilter, withUser bool) (*whereBuilder, *whereBuilder) {
	w := &whereBuilder{}
	if withUser {
		w.args = []any{userID}
	}
	visibleWhere(w, spaces)
	if f.NoMatch {
		w.add(`0`)
	}
	for _, t := range f.Terms {
		w.add(`ta.name_key LIKE ? ESCAPE '\'`, "%"+db.EscapeLike(t)+"%")
	}
	h := &whereBuilder{}
	if !f.All {
		h.add(`MAX(ta.role & 1) = 1`)
	}
	if f.Genre != "" {
		h.add(`MAX(EXISTS (SELECT 1 FROM music_track_genres g WHERE g.file_id = t.file_id AND g.genre_key = ?))`, f.Genre)
	}
	if f.Favorite != nil {
		h.add(`MAX(arf.item_id IS NOT NULL) = ?`, db.Bool(*f.Favorite))
	}
	return w, h
}

// listArtists returns one page of artists for p.
func (s *Service) listArtists(ctx context.Context, p *core.Principal, f artistFilter, lq listQuery) ([]Artist, string, error) {
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, "", err
	}
	hash := filterHash([]any{"artists", f, spaces, lq.Sort, lq.Order, p.UserID})
	withUser := lq.Sort == "plays" || f.Favorite != nil
	w, h := artistWhere(p.UserID, spaces, &f, withUser)
	cols, from := artistQuery(withUser)
	if lq.Sort == "random" {
		query := `SELECT ta.artist_id ` + from + ` WHERE ` + w.sql() + ` GROUP BY ta.artist_id`
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
		artists, err := s.artistsByID(ctx, p, spaces, page)
		return artists, next, err
	}
	pq := pageQuery{cols: cols, from: from, where: w, group: "ta.artist_id", having: h,
		keys: artistSortKeys(lq.Sort), idExpr: "ta.artist_id"}
	artists, next, err := keysetPage(ctx, s.d.DB.SQL, pq, lq, hash, scanArtist, func(a *Artist) string { return a.ID })
	if err != nil {
		return nil, "", err
	}
	if err := s.decorateArtists(ctx, p.UserID, spaces, artists, !withUser); err != nil {
		return nil, "", err
	}
	out := make([]Artist, 0, len(artists))
	for _, a := range artists {
		out = append(out, *a)
	}
	return out, next, nil
}

// artistsByID aggregates the given artists for p in the given order (invisible ones skipped).
func (s *Service) artistsByID(ctx context.Context, p *core.Principal, spaces, ids []string) ([]Artist, error) {
	if len(ids) == 0 {
		return []Artist{}, nil
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`ta.artist_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	cols, from := artistQuery(false)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT `+cols+` `+from+` WHERE `+w.sql()+` GROUP BY ta.artist_id`, w.args...)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Artist{}
	var all []*Artist
	for rs.Next() {
		a, err := scanArtist(rs)
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
	if err := s.decorateArtists(ctx, p.UserID, spaces, all, true); err != nil {
		return nil, err
	}
	out := make([]Artist, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id]; ok {
			out = append(out, *a)
		}
	}
	return out, nil
}

// artistArts resolves the art of artists: the art of one of their albums (as album artist,
// by album title), else of any of their tracks.
func (s *Service) artistArts(ctx context.Context, spaces, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	folderArt := s.settings(ctx).FolderArt
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`ta.artist_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT ta.artist_id, aa.art_id, t.art_id, fa.art_id FROM music_track_artists ta
		CROSS JOIN music_tracks t ON t.file_id = ta.file_id
		CROSS JOIN files f ON f.id = t.file_id
		LEFT JOIN music_album_art aa ON aa.album_id = t.album_id AND aa.space_id = f.space_id
		LEFT JOIN music_folder_art fa ON fa.file_id = t.folder_image_id AND fa.state = 1
		WHERE `+w.sql()+` AND (aa.art_id IS NOT NULL OR t.art_id IS NOT NULL OR fa.art_id IS NOT NULL)
		ORDER BY ta.artist_id, (ta.role & 1) DESC, t.album_id = '', t.sort_album, t.album_id, COALESCE(t.disc_number, 1e9),
		COALESCE(t.track_number, 1e9), t.file_id`, w.args...)
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
		if out[id] != "" {
			continue
		}
		switch {
		case custom.Valid:
			out[id] = custom.String
		case emb.Valid:
			out[id] = emb.String
		case folderArt && fold.Valid:
			out[id] = fold.String
		}
	}
	return out, rs.Err()
}

// decorateArtists adds art and (perUser) the caller's favorite flags.
func (s *Service) decorateArtists(ctx context.Context, userID string, spaces []string, artists []*Artist, perUser bool) error {
	if len(artists) == 0 {
		return nil
	}
	ids := make([]string, len(artists))
	for i, a := range artists {
		ids[i] = a.ID
	}
	arts, err := s.artistArts(ctx, spaces, ids)
	if err != nil {
		return err
	}
	if perUser {
		favs, err := s.queryIDs(ctx, `SELECT item_id FROM music_favorites WHERE user_id = ? AND kind = 'artist'
			AND item_id IN (SELECT value FROM json_each(?))`, userID, mustJSON(ids))
		if err != nil {
			return err
		}
		isFav := map[string]bool{}
		for _, id := range favs {
			isFav[id] = true
		}
		for _, a := range artists {
			a.Favorite = isFav[a.ID]
		}
	}
	for _, a := range artists {
		if art := arts[a.ID]; art != "" {
			a.ArtURL = strPtr("/api/v1/music/artists/" + a.ID + "/art?v=" + artVersion(art))
		}
	}
	return nil
}

var errNoArtist = core.NotFound("Artist not found")

// artist resolves one artist for p (404 when they have no track of it).
func (s *Service) artist(ctx context.Context, p *core.Principal, id string) (*Artist, []string, error) {
	if !validEntityID(id) {
		return nil, nil, errNoArtist
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, nil, err
	}
	artists, err := s.artistsByID(ctx, p, spaces, []string{id})
	if err != nil {
		return nil, nil, err
	}
	if len(artists) == 0 {
		return nil, nil, errNoArtist
	}
	return &artists[0], spaces, nil
}

const maxTopTracks = 10

// artistDetail is an artist with their albums, the albums they appear on and top tracks.
func (s *Service) artistDetail(ctx context.Context, p *core.Principal, id string) (*ArtistDetail, error) {
	a, spaces, err := s.artist(ctx, p, id)
	if err != nil {
		return nil, err
	}
	out := &ArtistDetail{Artist: *a, Albums: []Album{}, AppearsOn: []Album{}, TopTracks: []Track{}}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	own, err := s.queryIDs(ctx, `SELECT t.album_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id
		WHERE `+w.sql()+` AND t.album_artist_id = ? AND t.album_id <> '' GROUP BY t.album_id
		ORDER BY COALESCE(MAX(t.year), 1e9), MIN(t.sort_album), t.album_id LIMIT 1000`, append(w.args, id)...)
	if err != nil {
		return nil, err
	}
	if out.Albums, err = s.albumsByID(ctx, p, spaces, own); err != nil {
		return nil, err
	}
	other, err := s.queryIDs(ctx, `SELECT t.album_id FROM music_track_artists ta CROSS JOIN music_tracks t ON t.file_id = ta.file_id
		CROSS JOIN files f ON f.id = t.file_id WHERE `+w.sql()+` AND ta.artist_id = ? AND ta.role & 2 AND t.album_artist_id <> ?
		AND t.album_id <> '' GROUP BY t.album_id ORDER BY COALESCE(MAX(t.year), 1e9), MIN(t.sort_album), t.album_id LIMIT 1000`,
		append(w.args, id, id)...)
	if err != nil {
		return nil, err
	}
	if out.AppearsOn, err = s.albumsByID(ctx, p, spaces, other); err != nil {
		return nil, err
	}
	tw := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(tw, spaces)
	tw.add(`EXISTS (SELECT 1 FROM music_track_artists ta WHERE ta.file_id = t.file_id AND ta.artist_id = ?)`, id)
	trs, err := queryTracks(ctx, s.d.DB.SQL, `SELECT `+trackCols+` `+trackFrom+` WHERE `+tw.sql()+`
		ORDER BY COALESCE(ps.play_count, 0) DESC, COALESCE(ra.rating, 0) DESC, fv.file_id IS NULL, t.sort_title, t.file_id LIMIT ?`,
		append(tw.args, maxTopTracks)...)
	if err != nil {
		return nil, err
	}
	out.TopTracks = s.renderTracks(ctx, trs)
	return out, nil
}

// artistArtID is the art an artist shows to p ("" when none).
func (s *Service) artistArtID(ctx context.Context, p *core.Principal, id string) (string, error) {
	_, spaces, err := s.artist(ctx, p, id)
	if err != nil {
		return "", err
	}
	arts, err := s.artistArts(ctx, spaces, []string{id})
	if err != nil {
		return "", err
	}
	return arts[id], nil
}
