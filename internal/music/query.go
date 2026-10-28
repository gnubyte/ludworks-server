package music

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Track listings: the caller's scope, filters, sort keys and keyset cursors.

const maxRandomPool = 100_000 // candidates considered for sort=random

// scope resolves the spaces a listing covers: one explicitly requested space (any space the
// caller can read), or implicitly the caller's home plus readable libraries — other users'
// homes are never searched implicitly, even for admins (they can pass space_id).
func (s *Service) scope(ctx context.Context, p *core.Principal, spaceID string) ([]string, error) {
	if spaceID != "" {
		if !httpx.ValidID(spaceID) || !s.files.CanRead(ctx, p, spaceID) {
			return nil, core.NotFound("Space not found")
		}
		return []string{spaceID}, nil
	}
	ids, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE id IN (SELECT value FROM json_each(?))
		AND (kind = 'library' OR owner_id = ?) ORDER BY id`, mustJSON(ids), p.UserID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rs.Err()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// whereBuilder accumulates SQL conditions and their arguments.
type whereBuilder struct {
	conds []string
	args  []any
}

func (w *whereBuilder) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *whereBuilder) sql() string { return strings.Join(w.conds, " AND ") }

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// visibleWhere adds the conditions every listing shares: live music files in the spaces.
func visibleWhere(w *whereBuilder, spaces []string) {
	w.add(eligibleSQL)
	w.add(`f.space_id IN (SELECT value FROM json_each(?))`, mustJSON(spaces))
}

// trackFilter selects tracks (GET /music/tracks).
type trackFilter struct {
	Query       string // FTS5 MATCH expression
	NoMatch     bool   // q had no searchable characters
	ArtistID    string
	AlbumID     string
	Genre       string // folded
	Year        *int64
	Favorite    *bool
	SpaceID     string
	Subcategory string
}

func triBool(v string) (*bool, bool) {
	switch strings.ToLower(v) {
	case "":
		return nil, true
	case "1", "true", "yes", "on":
		b := true
		return &b, true
	case "0", "false", "no", "off":
		b := false
		return &b, true
	}
	return nil, false
}

// idParam validates an optional id-like query parameter (an invalid one matches nothing).
func idParam(v string) string {
	if v == "" || httpx.ValidID(v) {
		return v
	}
	return "-"
}

func parseYearParam(v string) (*int64, error) {
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 9999 {
		return nil, core.BadRequest("year: must be a year")
	}
	return &n, nil
}

func parseTrackFilter(q url.Values) (trackFilter, error) {
	var f trackFilter
	if v := q.Get("q"); v != "" {
		if len(v) > 500 {
			return f, core.BadRequest("q: too long")
		}
		f.Query = db.FTS5Query(v)
		f.NoMatch = f.Query == ""
	}
	f.ArtistID, f.AlbumID = idParam(q.Get("artist_id")), idParam(q.Get("album_id"))
	if v := strings.TrimSpace(q.Get("genre")); v != "" {
		if len(v) > 200 {
			return f, core.BadRequest("genre: too long")
		}
		f.Genre = fold(v)
	}
	y, err := parseYearParam(q.Get("year"))
	if err != nil {
		return f, err
	}
	f.Year = y
	fav, ok := triBool(q.Get("favorite"))
	if !ok {
		return f, core.BadRequest("favorite: must be true or false")
	}
	f.Favorite = fav
	if v := q.Get("space_id"); v != "" {
		if !httpx.ValidID(v) {
			return f, core.NotFound("Space not found")
		}
		f.SpaceID = v
	}
	f.Subcategory = strings.TrimSpace(q.Get("subcategory"))
	if len(f.Subcategory) > 64 {
		return f, core.BadRequest("subcategory: too long")
	}
	return f, nil
}

// filterWhere adds the track filters (the joins of trackFrom must be present).
func filterWhere(w *whereBuilder, f *trackFilter) {
	if f.NoMatch {
		w.add(`0`)
	}
	if f.Query != "" {
		w.add(`t.file_id IN (SELECT fm.file_id FROM music_fts JOIN music_fts_map fm ON fm.rowid = music_fts.rowid
			WHERE music_fts MATCH ?)`, f.Query)
	}
	if f.ArtistID != "" {
		w.add(`EXISTS (SELECT 1 FROM music_track_artists ta WHERE ta.file_id = t.file_id AND ta.artist_id = ?)`, f.ArtistID)
	}
	if f.AlbumID != "" {
		w.add(`t.album_id = ?`, f.AlbumID)
	}
	if f.Genre != "" {
		w.add(`EXISTS (SELECT 1 FROM music_track_genres g WHERE g.file_id = t.file_id AND g.genre_key = ?)`, f.Genre)
	}
	if f.Year != nil {
		w.add(`t.year = ?`, *f.Year)
	}
	if f.Favorite != nil {
		if *f.Favorite {
			w.add(`fv.file_id IS NOT NULL`)
		} else {
			w.add(`fv.file_id IS NULL`)
		}
	}
	if f.Subcategory != "" {
		w.add(`f.subcategory = ?`, f.Subcategory)
	}
}

// trackSorts are the sort keys of GET /music/tracks (also used by smart playlists).
var trackSorts = []string{"title", "artist", "album", "added", "year", "duration", "plays", "last_played", "random"}

// trackSortKeys returns the ORDER BY key expressions (all in the same direction; the file id
// is the final tie-breaker). Nullable numbers use sentinels so missing values sort last.
func trackSortKeys(sortBy, order string) []string {
	big := "1e18"
	if order == "desc" {
		big = "-1e18"
	}
	disc, num := `COALESCE(t.disc_number, `+big+`)`, `COALESCE(t.track_number, `+big+`)`
	switch sortBy {
	case "artist":
		return []string{`t.sort_artist`, `t.sort_album`, `t.album_id`, disc, num, `t.sort_title`}
	case "album":
		return []string{`t.sort_album`, `t.sort_album_artist`, `t.album_id`, disc, num, `t.sort_title`}
	case "added":
		return []string{`t.added_at`}
	case "year":
		return []string{`COALESCE(t.year, ` + big + `)`, `t.sort_album`, disc, num}
	case "duration":
		return []string{`COALESCE(t.duration_seconds, ` + big + `)`, `t.sort_title`}
	case "plays":
		return []string{`COALESCE(ps.play_count, 0)`, `t.sort_title`}
	case "last_played":
		return []string{`COALESCE(ps.last_played_at, '')`, `t.sort_title`}
	}
	return []string{`t.sort_title`, `t.sort_artist`}
}

type listCursor struct {
	Sort   string `json:"s"`
	Order  string `json:"o"`
	Keys   []any  `json:"k,omitempty"`
	ID     string `json:"id,omitempty"`
	Seed   string `json:"seed,omitempty"`
	Offset int    `json:"off,omitempty"`
	Hash   string `json:"h"`
}

// filterHash binds a cursor to its query.
func filterHash(v any) string {
	sum := sha256.Sum256([]byte(mustJSON(v)))
	return hex.EncodeToString(sum[:8])
}

// listQuery is a sort/paging request.
type listQuery struct {
	Sort, Order, Seed, Cursor string
	Limit                     int
}

func parseListQuery(q url.Values, limit int, sorts []string, def string) (listQuery, error) {
	lq := listQuery{Sort: q.Get("sort"), Order: q.Get("order"), Seed: q.Get("seed"), Cursor: q.Get("cursor"), Limit: limit}
	if lq.Sort == "" {
		lq.Sort = def
	}
	if !slices.Contains(sorts, lq.Sort) {
		return lq, core.BadRequest("sort: must be one of " + strings.Join(sorts, ", "))
	}
	switch lq.Order {
	case "":
		lq.Order = "asc"
	case "asc", "desc":
	default:
		return lq, core.BadRequest("order: must be asc or desc")
	}
	if len(lq.Seed) > 64 {
		return lq, core.BadRequest("seed: too long")
	}
	return lq, nil
}

// normKeys converts scanned key values into JSON-friendly values.
func normKeys(kv []any) []any {
	out := make([]any, len(kv))
	for i, v := range kv {
		if b, ok := v.([]byte); ok {
			out[i] = string(b)
		} else {
			out[i] = v
		}
	}
	return out
}

// pageQuery describes a keyset-paginated query: SELECT cols FROM from WHERE where [GROUP BY
// group HAVING having], ordered by keys and then idExpr. For grouped queries the keyset
// condition goes into HAVING (the keys are aggregates).
type pageQuery struct {
	cols, from string
	where      *whereBuilder
	group      string
	having     *whereBuilder
	keys       []string
	idExpr     string
}

// keysetPage runs a keyset-paginated query; scan reads one row given pointers to the key
// values, idOf returns a row's id for the cursor.
func keysetPage[T any](ctx context.Context, q db.Querier, pq pageQuery, lq listQuery, hash string,
	scan func(sc scanner, extra ...any) (T, error), idOf func(T) string) ([]T, string, error) {
	dir, cmp := "ASC", ">"
	if lq.Order == "desc" {
		dir, cmp = "DESC", "<"
	}
	keys := pq.keys
	conds := append([]string(nil), pq.where.conds...)
	args := append([]any(nil), pq.where.args...)
	var having []string
	var havingArgs []any
	if pq.having != nil {
		having = append(having, pq.having.conds...)
		havingArgs = append(havingArgs, pq.having.args...)
	}
	if lq.Cursor != "" {
		var cur listCursor
		if err := httpx.DecodeCursor(lq.Cursor, &cur); err != nil || cur.Hash != hash || cur.Sort != lq.Sort || cur.Order != lq.Order ||
			len(cur.Keys) != len(keys) || cur.ID == "" {
			return nil, "", httpx.ErrInvalidCursor
		}
		cond := `(` + strings.Join(keys, ", ") + `, ` + pq.idExpr + `) ` + cmp + ` (` + placeholders(len(keys)+1) + `)`
		if pq.group != "" {
			having = append(having, cond)
			havingArgs = append(havingArgs, append(cur.Keys, cur.ID)...)
		} else {
			conds = append(conds, cond)
			args = append(args, append(cur.Keys, cur.ID)...)
		}
	}
	order := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		order = append(order, k+" "+dir)
	}
	order = append(order, pq.idExpr+" "+dir)
	query := `SELECT ` + pq.cols + `, ` + strings.Join(keys, ", ") + ` ` + pq.from
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	if pq.group != "" {
		query += ` GROUP BY ` + pq.group
		if len(having) > 0 {
			query += ` HAVING ` + strings.Join(having, " AND ")
		}
		args = append(args, havingArgs...)
	}
	query += ` ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ?`
	rs, err := q.QueryContext(ctx, query, append(args, lq.Limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rs.Close()
	var (
		out     []T
		keyRows [][]any
	)
	for rs.Next() {
		kv := make([]any, len(keys))
		ptrs := make([]any, len(keys))
		for i := range kv {
			ptrs[i] = &kv[i]
		}
		v, err := scan(rs, ptrs...)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
		keyRows = append(keyRows, kv)
	}
	if err := rs.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > lq.Limit {
		out = out[:lq.Limit]
		next = httpx.EncodeCursor(listCursor{Sort: lq.Sort, Order: lq.Order, Keys: normKeys(keyRows[lq.Limit-1]),
			ID: idOf(out[lq.Limit-1]), Hash: hash})
	}
	return out, next, nil
}

// randomPage implements sort=random: a deterministic shuffle of ids by seed, paged by offset.
// Without a seed a random one is chosen and carried in the cursor.
func randomPage(ids []string, lq listQuery, hash string) ([]string, string, error) {
	seed, off := lq.Seed, 0
	if lq.Cursor != "" {
		var cur listCursor
		if err := httpx.DecodeCursor(lq.Cursor, &cur); err != nil || cur.Hash != hash || cur.Sort != "random" ||
			(lq.Seed != "" && lq.Seed != cur.Seed) || cur.Offset < 0 {
			return nil, "", httpx.ErrInvalidCursor
		}
		seed, off = cur.Seed, cur.Offset
	}
	if seed == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		seed = hex.EncodeToString(b[:])
	}
	shuffle(ids, seed)
	off = min(off, len(ids))
	end := min(off+lq.Limit, len(ids))
	next := ""
	if end < len(ids) {
		next = httpx.EncodeCursor(listCursor{Sort: "random", Order: lq.Order, Seed: seed, Offset: end, Hash: hash})
	}
	return ids[off:end], next, nil
}

// shuffle orders ids deterministically by (seed, id).
func shuffle(ids []string, seed string) {
	type cand struct {
		id  string
		key uint64
	}
	all := make([]cand, len(ids))
	for i, id := range ids {
		sum := sha256.Sum256([]byte(seed + "\x00" + id))
		all[i] = cand{id, binary.BigEndian.Uint64(sum[:8])}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].id < all[j].id
	})
	for i := range all {
		ids[i] = all[i].id
	}
}

// listTracks returns one page of tracks for p.
func (s *Service) listTracks(ctx context.Context, p *core.Principal, f trackFilter, lq listQuery) ([]Track, string, error) {
	spaces, err := s.scope(ctx, p, f.SpaceID)
	if err != nil {
		return nil, "", err
	}
	hash := filterHash([]any{"tracks", f, spaces, lq.Sort, lq.Order, p.UserID})
	w := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(w, spaces)
	filterWhere(w, &f)
	if lq.Sort == "random" {
		ids, err := s.queryIDs(ctx, `SELECT t.file_id `+trackFrom+` WHERE `+w.sql()+` LIMIT ?`, append(w.args, maxRandomPool)...)
		if err != nil {
			return nil, "", err
		}
		page, next, err := randomPage(ids, lq, hash)
		if err != nil {
			return nil, "", err
		}
		trs, err := loadTracksByID(ctx, s.d.DB.SQL, p.UserID, page)
		if err != nil {
			return nil, "", err
		}
		return s.renderTracks(ctx, trs), next, nil
	}
	pq := pageQuery{cols: trackCols, from: trackFrom, where: w, keys: trackSortKeys(lq.Sort, lq.Order), idExpr: "t.file_id"}
	trs, next, err := keysetPage(ctx, s.d.DB.SQL, pq, lq, hash, scanTrack, func(t *track) string { return t.FileID })
	if err != nil {
		return nil, "", err
	}
	return s.renderTracks(ctx, trs), next, nil
}

// queryIDs runs a query returning one string column.
func (s *Service) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, query, args...)
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

// visibleTracks returns the caller's tracks among ids (readable files that are music items;
// any space the caller can read, like GET /music/tracks/{id}), in the given order.
func (s *Service) visibleTracks(ctx context.Context, p *core.Principal, ids []string) ([]*track, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	spaces, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	readable := map[string]bool{}
	for _, id := range spaces {
		readable[id] = true
	}
	if err := s.ensureRows(ctx, ids); err != nil {
		return nil, err
	}
	trs, err := loadTracksByID(ctx, s.d.DB.SQL, p.UserID, ids)
	if err != nil {
		return nil, err
	}
	live, err := s.liveMusicFiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := trs[:0]
	for _, tr := range trs {
		if readable[tr.SpaceID] && live[tr.FileID] {
			out = append(out, tr)
		}
	}
	return out, nil
}

// liveMusicFiles reports which ids are live music items.
func (s *Service) liveMusicFiles(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(ids); start += 5000 {
		end := min(start+5000, len(ids))
		rs, err := s.queryIDs(ctx, `SELECT f.id FROM files f WHERE f.id IN (SELECT value FROM json_each(?)) AND `+eligibleSQL,
			mustJSON(ids[start:end]))
		if err != nil {
			return nil, err
		}
		for _, id := range rs {
			out[id] = true
		}
	}
	return out, nil
}
