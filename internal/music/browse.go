package music

import (
	"context"
	"sort"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Genres, facets, search and the home view.

// Genre is one entry of GET /music/genres.
type Genre struct {
	Name       string `json:"name"`
	TrackCount int64  `json:"track_count"`
	AlbumCount int64  `json:"album_count"`
}

func (s *Service) genres(ctx context.Context, p *core.Principal, spaceID string) ([]Genre, error) {
	spaces, err := s.scope(ctx, p, spaceID)
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT MIN(g.name), COUNT(*), COUNT(DISTINCT CASE WHEN t.album_id <> '' THEN t.album_id END)
		FROM music_track_genres g CROSS JOIN music_tracks t ON t.file_id = g.file_id CROSS JOIN files f ON f.id = t.file_id
		WHERE `+w.sql()+` GROUP BY g.genre_key ORDER BY g.genre_key LIMIT 5000`, w.args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Genre{}
	for rs.Next() {
		var g Genre
		if err := rs.Scan(&g.Name, &g.TrackCount, &g.AlbumCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rs.Err()
}

// FacetValue is one facet entry.
type FacetValue struct {
	Value any   `json:"value"`
	Count int64 `json:"count"`
}

// SpaceFacet is one space of the facets.
type SpaceFacet struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// Totals summarise the caller's library.
type Totals struct {
	Tracks          int64   `json:"tracks"`
	Albums          int64   `json:"albums"`
	Artists         int64   `json:"artists"`
	DurationSeconds float64 `json:"duration_seconds"`
	SizeBytes       int64   `json:"size_bytes"`
}

// Facets is GET /music/facets.
type Facets struct {
	Genres        []FacetValue `json:"genres"`
	Years         []FacetValue `json:"years"`
	Subcategories []FacetValue `json:"subcategories"`
	Spaces        []SpaceFacet `json:"spaces"`
	Totals        Totals       `json:"totals"`
}

const maxFacetValues = 500

func (s *Service) facets(ctx context.Context, p *core.Principal, spaceID string) (*Facets, error) {
	spaces, err := s.scope(ctx, p, spaceID)
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	from := ` FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id `
	run := func(query string, isInt bool) ([]FacetValue, error) {
		rs, err := s.d.DB.SQL.QueryContext(ctx, query, append(append([]any{}, w.args...), maxFacetValues)...)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		out := []FacetValue{}
		for rs.Next() {
			var (
				v any
				n int64
			)
			if err := rs.Scan(&v, &n); err != nil {
				return nil, err
			}
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if f, ok := v.(float64); ok && isInt {
				v = int64(f)
			}
			out = append(out, FacetValue{Value: v, Count: n})
		}
		return out, rs.Err()
	}
	out := &Facets{Spaces: []SpaceFacet{}}
	if out.Genres, err = run(`SELECT MIN(g.name), COUNT(*)`+from+`JOIN music_track_genres g ON g.file_id = t.file_id WHERE `+
		w.sql()+` GROUP BY g.genre_key ORDER BY COUNT(*) DESC, g.genre_key LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Years, err = run(`SELECT t.year, COUNT(*)`+from+`WHERE `+w.sql()+` AND t.year IS NOT NULL GROUP BY t.year
		ORDER BY t.year DESC LIMIT ?`, true); err != nil {
		return nil, err
	}
	if out.Subcategories, err = run(`SELECT f.subcategory, COUNT(*)`+from+`WHERE `+w.sql()+` AND f.subcategory <> ''
		GROUP BY f.subcategory ORDER BY COUNT(*) DESC, f.subcategory LIMIT ?`, false); err != nil {
		return nil, err
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT f.space_id, sp.name, COUNT(*)`+from+`JOIN spaces sp ON sp.id = f.space_id
		WHERE `+w.sql()+` GROUP BY f.space_id ORDER BY COUNT(*) DESC, f.space_id LIMIT ?`, append(append([]any{}, w.args...), maxFacetValues)...)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var sf SpaceFacet
		if err := rs.Scan(&sf.Value, &sf.Label, &sf.Count); err != nil {
			rs.Close()
			return nil, err
		}
		out.Spaces = append(out.Spaces, sf)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	err = s.d.DB.SQL.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT CASE WHEN t.album_id <> '' THEN t.album_id END),
		COUNT(DISTINCT CASE WHEN t.album_artist_id <> '' THEN t.album_artist_id END), COALESCE(SUM(t.duration_seconds), 0),
		COALESCE(SUM(f.size), 0)`+from+`WHERE `+w.sql(), w.args...).Scan(&out.Totals.Tracks, &out.Totals.Albums,
		&out.Totals.Artists, &out.Totals.DurationSeconds, &out.Totals.SizeBytes)
	return out, err
}

/* -------------------------------------------------------------------- search */

// SearchResult is GET /music/search.
type SearchResult struct {
	Tracks    []Track    `json:"tracks"`
	Albums    []Album    `json:"albums"`
	Artists   []Artist   `json:"artists"`
	Playlists []Playlist `json:"playlists"`
}

// search matches tracks, albums, artists and playlists. Results are ordered by where the
// terms matched (title/name first) and then alphabetically — never by corpus-wide relevance
// scores, which would let other users' libraries influence (and leak through) the order.
func (s *Service) search(ctx context.Context, p *core.Principal, q string, limit int) (*SearchResult, error) {
	out := &SearchResult{Tracks: []Track{}, Albums: []Album{}, Artists: []Artist{}, Playlists: []Playlist{}}
	match := db.FTS5Query(q)
	terms := queryTerms(q)
	if match == "" || len(terms) == 0 {
		return out, nil
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, err
	}
	ftsIDs := `SELECT fm.file_id FROM music_fts JOIN music_fts_map fm ON fm.rowid = music_fts.rowid WHERE music_fts MATCH ?`

	// Tracks: title matches first.
	tw := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(tw, spaces)
	tw.add(`t.file_id IN (`+ftsIDs+`)`, match)
	trs, err := queryTracks(ctx, s.d.DB.SQL, `SELECT `+trackCols+` `+trackFrom+` WHERE `+tw.sql()+`
		ORDER BY t.file_id IN (`+ftsIDs+`) DESC, t.sort_title, t.file_id LIMIT ?`, append(tw.args, "{title} : ("+match+")", limit)...)
	if err != nil {
		return nil, err
	}
	out.Tracks = s.renderTracks(ctx, trs)

	// Albums whose title or album artist matches; title matches first.
	aw := &whereBuilder{}
	visibleWhere(aw, spaces)
	aw.add(`t.album_id <> ''`)
	aw.add(`t.file_id IN (`+ftsIDs+`)`, "{album album_artist} : ("+match+")")
	albumIDs, err := s.queryIDs(ctx, `SELECT t.album_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id WHERE `+aw.sql()+`
		GROUP BY t.album_id ORDER BY MAX(t.file_id IN (`+ftsIDs+`)) DESC, MIN(t.sort_album), t.album_id LIMIT ?`,
		append(aw.args, "{album} : ("+match+")", limit)...)
	if err != nil {
		return nil, err
	}
	if out.Albums, err = s.albumsByID(ctx, p, spaces, albumIDs); err != nil {
		return nil, err
	}

	// Artists whose name contains the terms; whole-word matches first.
	rw := &whereBuilder{}
	visibleWhere(rw, spaces)
	for _, t := range terms {
		rw.add(`ta.name_key LIKE ? ESCAPE '\'`, "%"+db.EscapeLike(t)+"%")
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT ta.artist_id, MIN(ta.name), MIN(ta.sort_name) FROM music_track_artists ta
		CROSS JOIN music_tracks t ON t.file_id = ta.file_id CROSS JOIN files f ON f.id = t.file_id WHERE `+rw.sql()+`
		GROUP BY ta.artist_id ORDER BY MIN(ta.sort_name), ta.artist_id LIMIT 500`, rw.args...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id, name, sortName string
		rank               int
	}
	var cands []cand
	for rs.Next() {
		var c cand
		if err := rs.Scan(&c.id, &c.name, &c.sortName); err != nil {
			rs.Close()
			return nil, err
		}
		switch {
		case fold(c.name) == strings.Join(terms, " "):
			c.rank = 0
		case matchesTerms(c.name, terms):
			c.rank = 1
		default:
			c.rank = 2
		}
		cands = append(cands, c)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	var artistIDs []string
	for _, c := range cands {
		if len(artistIDs) >= limit {
			break
		}
		artistIDs = append(artistIDs, c.id)
	}
	if out.Artists, err = s.artistsByID(ctx, p, spaces, artistIDs); err != nil {
		return nil, err
	}

	if out.Playlists, err = s.searchPlaylists(ctx, p, terms, limit); err != nil {
		return nil, err
	}
	return out, nil
}

/* ---------------------------------------------------------------------- home */

// RecentItem is one entry of the home view's recently_played.
type RecentItem struct {
	Kind     string    `json:"kind"`
	PlayedAt string    `json:"played_at"`
	Album    *Album    `json:"album,omitempty"`
	Artist   *Artist   `json:"artist,omitempty"`
	Playlist *Playlist `json:"playlist,omitempty"`
	Track    *Track    `json:"track,omitempty"`
}

// Home is GET /music/home.
type Home struct {
	RecentlyAdded  []Album      `json:"recently_added"`
	RecentlyPlayed []RecentItem `json:"recently_played"`
	MostPlayed     []Track      `json:"most_played"`
	FavoriteAlbums []Album      `json:"favorite_albums"`
	Playlists      []Playlist   `json:"playlists"`
}

const (
	homeLimit        = 20
	mostPlayedWindow = 90 * 24 * time.Hour
)

func (s *Service) home(ctx context.Context, p *core.Principal) (*Home, error) {
	out := &Home{RecentlyPlayed: []RecentItem{}, MostPlayed: []Track{}}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, err
	}
	if out.RecentlyAdded, _, err = s.listAlbums(ctx, p, albumFilter{}, listQuery{Sort: "added", Order: "desc", Limit: homeLimit}); err != nil {
		return nil, err
	}
	if out.RecentlyPlayed, err = s.recentlyPlayed(ctx, p, spaces); err != nil {
		return nil, err
	}
	// Most played: plays of the last 90 days.
	w := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(w, spaces)
	since := db.FormatTime(s.now().Add(-mostPlayedWindow))
	ids, err := s.queryIDs(ctx, `SELECT pl.file_id FROM music_plays pl WHERE pl.user_id = ? AND pl.played_at >= ?
		GROUP BY pl.file_id ORDER BY COUNT(*) DESC, MAX(pl.played_at) DESC, pl.file_id LIMIT 200`, p.UserID, since)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		w.add(`t.file_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
		trs, err := queryTracks(ctx, s.d.DB.SQL, `SELECT `+trackCols+` `+trackFrom+` WHERE `+w.sql(), w.args...)
		if err != nil {
			return nil, err
		}
		byID := map[string]*track{}
		for _, tr := range trs {
			byID[tr.FileID] = tr
		}
		var ordered []*track
		for _, id := range ids {
			if tr := byID[id]; tr != nil && len(ordered) < homeLimit {
				ordered = append(ordered, tr)
			}
		}
		out.MostPlayed = s.renderTracks(ctx, ordered)
	}
	favIDs, err := s.queryIDs(ctx, `SELECT item_id FROM music_favorites WHERE user_id = ? AND kind = 'album'
		ORDER BY created_at DESC LIMIT 200`, p.UserID)
	if err != nil {
		return nil, err
	}
	fav, err := s.albumsByID(ctx, p, spaces, favIDs)
	if err != nil {
		return nil, err
	}
	out.FavoriteAlbums = fav[:min(len(fav), homeLimit)]
	if out.Playlists, _, err = s.listPlaylists(ctx, p, playlistFilter{Owner: "all"}, listQuery{Sort: "updated", Order: "desc", Limit: homeLimit}); err != nil {
		return nil, err
	}
	return out, nil
}

// recentlyPlayed groups the caller's latest plays by context: the album, artist or playlist a
// play came from; plays without one (or from a genre, search or queue) count as their album,
// or as the track when it has no album.
func (s *Service) recentlyPlayed(ctx context.Context, p *core.Principal, spaces []string) ([]RecentItem, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT pl.file_id, pl.played_at, COALESCE(pl.context_kind, ''), COALESCE(pl.context_id, ''),
		COALESCE(t.album_id, '') FROM music_plays pl LEFT JOIN music_tracks t ON t.file_id = pl.file_id
		WHERE pl.user_id = ? ORDER BY pl.played_at DESC, pl.id LIMIT 1000`, p.UserID)
	if err != nil {
		return nil, err
	}
	type entry struct {
		kind, id string
		at       time.Time
	}
	seen := map[string]bool{}
	var entries []entry
	for rs.Next() {
		var (
			fileID, kind, ctxID, albumID string
			at                           db.Time
		)
		if err := rs.Scan(&fileID, &at, &kind, &ctxID, &albumID); err != nil {
			rs.Close()
			return nil, err
		}
		e := entry{at: at.Time}
		switch {
		case (kind == "album" || kind == "artist" || kind == "playlist") && ctxID != "":
			e.kind, e.id = kind, ctxID
		case albumID != "":
			e.kind, e.id = "album", albumID
		default:
			e.kind, e.id = "track", fileID
		}
		if k := e.kind + ":" + e.id; !seen[k] && len(entries) < 2*homeLimit {
			seen[k] = true
			entries = append(entries, e)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	var albumIDs, artistIDs, playlistIDs, trackIDs []string
	for _, e := range entries {
		switch e.kind {
		case "album":
			albumIDs = append(albumIDs, e.id)
		case "artist":
			artistIDs = append(artistIDs, e.id)
		case "playlist":
			playlistIDs = append(playlistIDs, e.id)
		default:
			trackIDs = append(trackIDs, e.id)
		}
	}
	albums, err := s.albumsByID(ctx, p, spaces, albumIDs)
	if err != nil {
		return nil, err
	}
	artists, err := s.artistsByID(ctx, p, spaces, artistIDs)
	if err != nil {
		return nil, err
	}
	playlists, err := s.playlistsByID(ctx, p, playlistIDs)
	if err != nil {
		return nil, err
	}
	trs, err := s.visibleTracks(ctx, p, trackIDs)
	if err != nil {
		return nil, err
	}
	albumMap, artistMap, playlistMap, trackMap := map[string]*Album{}, map[string]*Artist{}, map[string]*Playlist{}, map[string]*Track{}
	for i := range albums {
		albumMap[albums[i].ID] = &albums[i]
	}
	for i := range artists {
		artistMap[artists[i].ID] = &artists[i]
	}
	for i := range playlists {
		playlistMap[playlists[i].ID] = &playlists[i]
	}
	rendered := s.renderTracks(ctx, trs)
	for i := range rendered {
		trackMap[rendered[i].ID] = &rendered[i]
	}
	out := []RecentItem{}
	for _, e := range entries {
		if len(out) >= homeLimit {
			break
		}
		it := RecentItem{Kind: e.kind, PlayedAt: db.FormatTime(e.at)}
		switch e.kind {
		case "album":
			it.Album = albumMap[e.id]
			if it.Album == nil {
				continue
			}
		case "artist":
			it.Artist = artistMap[e.id]
			if it.Artist == nil {
				continue
			}
		case "playlist":
			it.Playlist = playlistMap[e.id]
			if it.Playlist == nil {
				continue
			}
		default:
			it.Track = trackMap[e.id]
			if it.Track == nil {
				continue
			}
		}
		out = append(out, it)
	}
	return out, nil
}
