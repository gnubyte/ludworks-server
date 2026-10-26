package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/media/naming"
)

// Listing limits.
const (
	maxListValues   = 20     // comma-list values per filter
	maxRandomPool   = 100000 // candidates considered for sort=random
	maxShowEpisodes = 5000   // episodes returned by the show detail
	maxFacetValues  = 500
)

var showKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// Filter selects media items (GET /media and facets).
type Filter struct {
	Categories    []string // nil = default (movie, tv and uncategorized videos); "none" = no category
	Subcategories []string
	Genre         string
	Year          *int64
	ShowKey       string
	Season        *int64
	Query         string // FTS5 MATCH expression
	Tag           string
	SpaceID       string
	Watched       *bool
	InProgress    bool
}

// listQuery is a parsed GET /media request.
type listQuery struct {
	Filter
	Sort, Order, Seed, Cursor string
	Limit                     int
}

var validSorts = []string{"title", "added", "year", "duration", "random", "episode", "progress"}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
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

// parseFilter reads the filter parameters shared by listings and facets.
func parseFilter(q url.Values, full bool) (Filter, error) {
	var f Filter
	bad := func(field, msg string) (Filter, error) {
		return Filter{}, core.BadRequest(field + ": " + msg)
	}
	if v := q.Get("category"); v != "" {
		f.Categories = splitList(v)
		if len(f.Categories) > maxListValues {
			return bad("category", "too many values")
		}
	}
	if !full {
		return f, nil
	}
	if v := q.Get("subcategory"); v != "" {
		f.Subcategories = splitList(v)
		if len(f.Subcategories) > maxListValues {
			return bad("subcategory", "too many values")
		}
	}
	f.Genre = strings.TrimSpace(q.Get("genre"))
	f.Tag = strings.TrimSpace(q.Get("tag"))
	if len(f.Genre) > 200 || len(f.Tag) > 200 {
		return bad("genre", "too long")
	}
	if v := q.Get("year"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 9999 {
			return bad("year", "must be a year")
		}
		f.Year = &n
	}
	if v := q.Get("season"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 100000 {
			return bad("season", "must be a non-negative integer")
		}
		f.Season = &n
	}
	if v := strings.TrimSpace(q.Get("show")); v != "" {
		if len(v) > 512 {
			return bad("show", "too long")
		}
		if showKeyRe.MatchString(v) {
			f.ShowKey = v
		} else {
			f.ShowKey = naming.ShowKey(v)
		}
		if f.ShowKey == "" {
			f.ShowKey = "-" // matches nothing
		}
	}
	if v := q.Get("q"); v != "" {
		if len(v) > 500 {
			return bad("q", "too long")
		}
		f.Query = db.FTS5Query(v)
	}
	if v := q.Get("space_id"); v != "" {
		if !httpx.ValidID(v) {
			return Filter{}, core.NotFound("Space not found")
		}
		f.SpaceID = v
	}
	w, ok := triBool(q.Get("watched"))
	if !ok {
		return bad("watched", "must be true or false")
	}
	f.Watched = w
	ip, ok := triBool(q.Get("in_progress"))
	if !ok {
		return bad("in_progress", "must be true or false")
	}
	f.InProgress = ip != nil && *ip
	return f, nil
}

func parseListQuery(r *requestValues) (*listQuery, error) {
	f, err := parseFilter(r.q, true)
	if err != nil {
		return nil, err
	}
	lq := &listQuery{Filter: f, Sort: r.q.Get("sort"), Order: r.q.Get("order"), Seed: r.q.Get("seed"),
		Cursor: r.q.Get("cursor"), Limit: r.limit}
	if lq.Sort == "" {
		lq.Sort = "title"
	}
	if !slices.Contains(validSorts, lq.Sort) {
		return nil, core.BadRequest("sort: must be one of " + strings.Join(validSorts, ", "))
	}
	switch lq.Order {
	case "":
		lq.Order = "asc"
	case "asc", "desc":
	default:
		return nil, core.BadRequest("order: must be asc or desc")
	}
	if len(lq.Seed) > 64 {
		return nil, core.BadRequest("seed: too long")
	}
	return lq, nil
}

// requestValues carries the query string and parsed limit of a request.
type requestValues struct {
	q     url.Values
	limit int
}

/* ------------------------------------------------------------- base filter */

// scope resolves the spaces a listing covers: one explicitly requested space (any space the
// caller can read), or implicitly the caller's home plus readable libraries — other users'
// homes are never searched implicitly, even for admins (privacy; they can pass space_id).
func (s *Service) scope(ctx context.Context, p *core.Principal, spaceID string) ([]string, error) {
	if spaceID != "" {
		if !s.files.CanRead(ctx, p, spaceID) {
			return nil, core.NotFound("Space not found")
		}
		return []string{spaceID}, nil
	}
	ids, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ids)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE id IN (SELECT value FROM json_each(?))
		AND (kind = 'library' OR owner_id = ?) ORDER BY id`, string(b), p.UserID)
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

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func anyList(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// baseWhere adds the conditions every listing shares: live media files in the given spaces
// and the category filter.
func baseWhere(w *whereBuilder, spaces []string, categories []string) {
	b, _ := json.Marshal(spaces)
	w.add(`f.trashed_at IS NULL AND f.is_dir = 0 AND ` + eligibleSQL)
	w.add(`f.space_id IN (SELECT value FROM json_each(?))`, string(b))
	if categories == nil {
		w.add(`(f.category IN ('movie', 'tv') OR (f.category = '' AND f.mime LIKE 'video/%'))`)
		return
	}
	var real []string
	none := false
	for _, c := range categories {
		if c == "none" {
			none = true
		} else {
			real = append(real, c)
		}
	}
	var parts []string
	if len(real) > 0 {
		parts = append(parts, `f.category IN (`+placeholders(len(real))+`)`)
		w.args = append(w.args, anyList(real)...)
	}
	if none {
		parts = append(parts, `f.category = ''`)
	}
	w.conds = append(w.conds, "("+strings.Join(parts, " OR ")+")")
}

// filterWhere adds the remaining filters (the progress join must be present).
func filterWhere(w *whereBuilder, f *Filter) {
	if len(f.Subcategories) > 0 {
		w.add(`f.subcategory IN (`+placeholders(len(f.Subcategories))+`)`, anyList(f.Subcategories)...)
	}
	if f.Genre != "" {
		w.add(`EXISTS (SELECT 1 FROM json_each(m.genres) g WHERE g.value = ? COLLATE NOCASE)`, f.Genre)
	}
	if f.Tag != "" {
		w.add(`EXISTS (SELECT 1 FROM json_each(f.tags) t WHERE t.value = ? COLLATE NOCASE)`, f.Tag)
	}
	if f.Year != nil {
		w.add(`m.year = ?`, *f.Year)
	}
	if f.ShowKey != "" {
		w.add(`m.show_key = ?`, f.ShowKey)
	}
	if f.Season != nil {
		w.add(`m.season = ?`, *f.Season)
	}
	if f.Query != "" {
		w.add(`m.file_id IN (SELECT mp.file_id FROM media_fts JOIN media_fts_map mp ON mp.rowid = media_fts.rowid
			WHERE media_fts MATCH ?)`, f.Query)
	}
	if f.Watched != nil {
		if *f.Watched {
			w.add(`COALESCE(p.completed, 0) = 1`)
		} else {
			w.add(`COALESCE(p.completed, 0) = 0`)
		}
	}
	if f.InProgress {
		w.add(`p.position_seconds > 0 AND COALESCE(p.completed, 0) = 0`)
	}
}

/* ----------------------------------------------------------------- listing */

// sortKeys returns the ORDER BY key expressions (all sorted in the same direction; the
// file id is the final tie-breaker). Nullable numbers use sentinels so that missing values
// sort last in both directions.
func sortKeys(sortBy, order string) []string {
	big := "1e18"
	if order == "desc" {
		big = "-1e18"
	}
	title := `m.sort_title COLLATE NOCASE`
	switch sortBy {
	case "added":
		return []string{`m.added_at`}
	case "year":
		return []string{`COALESCE(m.year, ` + big + `)`, title}
	case "duration":
		return []string{`COALESCE(m.duration_seconds, ` + big + `)`, title}
	case "episode":
		return []string{`m.show_key`, `COALESCE(m.season, ` + big + `)`, `COALESCE(m.episode, ` + big + `)`, title}
	case "progress":
		return []string{`COALESCE(p.updated_at, '')`}
	}
	return []string{title}
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
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// List returns one page of media items for p.
func (s *Service) list(ctx context.Context, p *core.Principal, lq *listQuery) ([]Media, string, error) {
	spaces, err := s.scope(ctx, p, lq.SpaceID)
	if err != nil {
		return nil, "", err
	}
	hash := filterHash([]any{lq.Filter, spaces, lq.Sort, lq.Order, p.UserID})
	var cur *listCursor
	if lq.Cursor != "" {
		cur = &listCursor{}
		if err := httpx.DecodeCursor(lq.Cursor, cur); err != nil || cur.Hash != hash || cur.Sort != lq.Sort || cur.Order != lq.Order {
			return nil, "", httpx.ErrInvalidCursor
		}
	}
	w := &whereBuilder{args: []any{p.UserID}}
	baseWhere(w, spaces, lq.Categories)
	filterWhere(w, &lq.Filter)
	if lq.Sort == "random" {
		return s.listRandom(ctx, lq, w, cur, hash)
	}
	keys := sortKeys(lq.Sort, lq.Order)
	dir, cmp := "ASC", ">"
	if lq.Order == "desc" {
		dir, cmp = "DESC", "<"
	}
	if cur != nil {
		if len(cur.Keys) != len(keys) || cur.ID == "" {
			return nil, "", httpx.ErrInvalidCursor
		}
		w.add(`(`+strings.Join(keys, ", ")+`, m.file_id) `+cmp+` (`+placeholders(len(keys)+1)+`)`, append(cur.Keys, cur.ID)...)
	}
	order := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		order = append(order, k+" "+dir)
	}
	order = append(order, "m.file_id "+dir)
	keyCols := ""
	for _, k := range keys {
		keyCols += ", " + k
	}
	query := `SELECT ` + itemCols + keyCols + ` ` + itemFrom + ` WHERE ` + w.sql() + ` ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ?`
	rs, err := s.d.DB.SQL.QueryContext(ctx, query, append(w.args, lq.Limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rs.Close()
	var (
		items    []*item
		keysList [][]any
	)
	for rs.Next() {
		kv := make([]any, len(keys))
		it, err := scanItem(&extraScanner{rs: rs, extra: kv})
		if err != nil {
			return nil, "", err
		}
		items = append(items, it)
		keysList = append(keysList, kv)
	}
	if err := rs.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > lq.Limit {
		items = items[:lq.Limit]
		last := items[lq.Limit-1]
		next = httpx.EncodeCursor(listCursor{Sort: lq.Sort, Order: lq.Order, Keys: normKeys(keysList[lq.Limit-1]), ID: last.FileID, Hash: hash})
	}
	return s.renderItems(ctx, items), next, nil
}

// normKeys converts scanned key values into JSON-friendly values.
func normKeys(kv []any) []any {
	out := make([]any, len(kv))
	for i, v := range kv {
		switch x := v.(type) {
		case []byte:
			out[i] = string(x)
		case int64:
			out[i] = x
		default:
			out[i] = x
		}
	}
	return out
}

// extraScanner scans the item columns followed by extra key columns.
type extraScanner struct {
	rs interface {
		Scan(dest ...any) error
	}
	extra []any
}

func (e *extraScanner) Scan(dest ...any) error {
	ptrs := make([]any, len(e.extra))
	for i := range e.extra {
		ptrs[i] = &e.extra[i]
	}
	return e.rs.Scan(append(dest, ptrs...)...)
}

// listRandom implements sort=random: a deterministic shuffle by (seed, file id), paged by
// offset. Without a seed a random one is chosen and carried in the cursor.
func (s *Service) listRandom(ctx context.Context, lq *listQuery, w *whereBuilder, cur *listCursor, hash string) ([]Media, string, error) {
	seed, off := lq.Seed, 0
	if cur != nil {
		seed, off = cur.Seed, cur.Offset
		if lq.Seed != "" && lq.Seed != cur.Seed {
			return nil, "", httpx.ErrInvalidCursor
		}
	}
	if seed == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		seed = hex.EncodeToString(b[:])
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.file_id `+itemFrom+` WHERE `+w.sql()+` LIMIT ?`, append(w.args, maxRandomPool)...)
	if err != nil {
		return nil, "", err
	}
	type cand struct {
		id  string
		key uint64
	}
	var all []cand
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return nil, "", err
		}
		sum := sha256.Sum256([]byte(seed + "\x00" + id))
		all = append(all, cand{id, binary.BigEndian.Uint64(sum[:8])})
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, "", err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].id < all[j].id
	})
	if off > len(all) {
		off = len(all)
	}
	end := min(off+lq.Limit, len(all))
	ids := make([]string, 0, end-off)
	for _, c := range all[off:end] {
		ids = append(ids, c.id)
	}
	items, err := s.itemsByID(ctx, w.args[0].(string), ids)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if end < len(all) {
		next = httpx.EncodeCursor(listCursor{Sort: lq.Sort, Order: lq.Order, Seed: seed, Offset: end, Hash: hash})
	}
	return s.renderItems(ctx, items), next, nil
}

// itemsByID loads items in the given order (missing ids are skipped).
func (s *Service) itemsByID(ctx context.Context, userID string, ids []string) ([]*item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(ids)
	items, err := queryItems(ctx, s.d.DB.SQL, `SELECT `+itemCols+` `+itemFrom+` WHERE m.file_id IN (SELECT value FROM json_each(?))`, userID, string(b))
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*item, len(items))
	for _, it := range items {
		byID[it.FileID] = it
	}
	out := make([]*item, 0, len(ids))
	for _, id := range ids {
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

/* ------------------------------------------------------------------ facets */

// FacetValue is one facet entry.
type FacetValue struct {
	Value any `json:"value"`
	Count int `json:"count"`
}

// Facets are the filter values of a library view.
type Facets struct {
	Genres        []FacetValue `json:"genres"`
	Years         []FacetValue `json:"years"`
	Shows         []FacetValue `json:"shows"`
	Subcategories []FacetValue `json:"subcategories"`
	Tags          []FacetValue `json:"tags"`
	Categories    []FacetValue `json:"categories"`
	Spaces        []FacetValue `json:"spaces"`
}

func (s *Service) facets(ctx context.Context, p *core.Principal, f Filter) (*Facets, error) {
	spaces, err := s.scope(ctx, p, f.SpaceID)
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{}
	baseWhere(w, spaces, f.Categories)
	from := ` FROM media_items m JOIN files f ON f.id = m.file_id `
	where := ` WHERE ` + w.sql()
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
				n int
			)
			if err := rs.Scan(&v, &n); err != nil {
				return nil, err
			}
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if isInt {
				if f, ok := v.(float64); ok {
					v = int64(f)
				}
			}
			out = append(out, FacetValue{Value: v, Count: n})
		}
		return out, rs.Err()
	}
	out := &Facets{}
	if out.Genres, err = run(`SELECT MIN(g.value), COUNT(*)`+from+`, json_each(m.genres) g`+where+
		` GROUP BY lower(g.value) ORDER BY COUNT(*) DESC, lower(MIN(g.value)) LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Years, err = run(`SELECT m.year, COUNT(*)`+from+where+` AND m.year IS NOT NULL GROUP BY m.year ORDER BY m.year DESC LIMIT ?`, true); err != nil {
		return nil, err
	}
	if out.Shows, err = run(`SELECT MIN(m.show_title), COUNT(*)`+from+where+` AND m.show_key <> '' GROUP BY m.show_key
		ORDER BY lower(MIN(m.show_title)) LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Subcategories, err = run(`SELECT f.subcategory, COUNT(*)`+from+where+` AND f.subcategory <> '' GROUP BY f.subcategory
		ORDER BY COUNT(*) DESC, f.subcategory LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Tags, err = run(`SELECT MIN(t.value), COUNT(*)`+from+`, json_each(f.tags) t`+where+
		` GROUP BY lower(t.value) ORDER BY COUNT(*) DESC, lower(MIN(t.value)) LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Categories, err = run(`SELECT CASE f.category WHEN '' THEN 'none' ELSE f.category END, COUNT(*)`+from+where+
		` GROUP BY f.category ORDER BY COUNT(*) DESC LIMIT ?`, false); err != nil {
		return nil, err
	}
	if out.Spaces, err = run(`SELECT f.space_id, COUNT(*)`+from+where+` GROUP BY f.space_id ORDER BY COUNT(*) DESC LIMIT ?`, false); err != nil {
		return nil, err
	}
	return out, nil
}

/* ------------------------------------------------------------------- shows */

// ShowSummary is one entry of GET /media/shows.
type ShowSummary struct {
	ShowKey   string  `json:"show_key"`
	ShowTitle string  `json:"show_title"`
	Seasons   int     `json:"seasons"`
	Episodes  int     `json:"episodes"`
	Watched   int     `json:"watched"`
	PosterURL *string `json:"poster_url"`
	Year      *int64  `json:"year"`
	AddedAt   string  `json:"added_at"`
}

type showCursor struct {
	Title string `json:"t"`
	Key   string `json:"k"`
	Hash  string `json:"h"`
}

func (s *Service) shows(ctx context.Context, p *core.Principal, q string, cursor string, limit int) ([]ShowSummary, string, error) {
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, "", err
	}
	hash := filterHash([]any{"shows", q, spaces})
	w := &whereBuilder{args: []any{p.UserID}}
	baseWhere(w, spaces, nil)
	w.add(`m.show_key <> ''`)
	if q = strings.TrimSpace(q); q != "" {
		w.add(`m.show_title LIKE ? ESCAPE '\'`, "%"+db.EscapeLike(q)+"%")
	}
	having := ""
	args := w.args
	if cursor != "" {
		var c showCursor
		if err := httpx.DecodeCursor(cursor, &c); err != nil || c.Hash != hash {
			return nil, "", httpx.ErrInvalidCursor
		}
		having = ` HAVING (lower(MIN(m.show_title)), m.show_key) > (?, ?)`
		args = append(args, c.Title, c.Key)
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.show_key, MIN(m.show_title), COUNT(DISTINCT m.season), COUNT(*),
		SUM(COALESCE(p.completed, 0)), MIN(m.year), MAX(m.added_at)
		`+itemFrom+` WHERE `+w.sql()+` GROUP BY m.show_key`+having+`
		ORDER BY lower(MIN(m.show_title)), m.show_key LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	var out []ShowSummary
	for rs.Next() {
		var (
			sm    ShowSummary
			year  *int64
			added string
		)
		if err := rs.Scan(&sm.ShowKey, &sm.ShowTitle, &sm.Seasons, &sm.Episodes, &sm.Watched, &year, &added); err != nil {
			rs.Close()
			return nil, "", err
		}
		sm.Year, sm.AddedAt = year, added
		out = append(out, sm)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = httpx.EncodeCursor(showCursor{Title: strings.ToLower(last.ShowTitle), Key: last.ShowKey, Hash: hash})
	}
	if out == nil {
		out = []ShowSummary{}
	}
	keys := make([]string, len(out))
	for i, sm := range out {
		keys[i] = sm.ShowKey
	}
	reps, err := s.showRepresentatives(ctx, p, spaces, keys)
	if err != nil {
		return nil, "", err
	}
	for i := range out {
		if rep := reps[out[i].ShowKey]; rep != nil {
			out[i].PosterURL = rep.poster
		}
	}
	return out, next, nil
}

type showRep struct {
	fileID           string
	poster, backdrop *string
	description      string
}

// showRepresentatives picks, per show, the poster (first episode with a custom poster, else
// the first episode's thumbnail), the backdrop and the series description.
func (s *Service) showRepresentatives(ctx context.Context, p *core.Principal, spaces, keys []string) (map[string]*showRep, error) {
	out := map[string]*showRep{}
	if len(keys) == 0 {
		return out, nil
	}
	kb, _ := json.Marshal(keys)
	w := &whereBuilder{args: []any{p.UserID}}
	baseWhere(w, spaces, nil)
	w.add(`m.show_key IN (SELECT value FROM json_each(?))`, string(kb))
	items, err := queryItems(ctx, s.d.DB.SQL, `SELECT `+itemCols+` `+itemFrom+` WHERE `+w.sql()+` AND m.file_id IN (
		SELECT file_id FROM (SELECT m2.file_id, ROW_NUMBER() OVER (PARTITION BY m2.show_key
			ORDER BY (m2.poster_path IS NULL), COALESCE(m2.season, 1e9), COALESCE(m2.episode, 1e9), m2.file_id) AS rn
			FROM media_items m2 JOIN files f2 ON f2.id = m2.file_id
			WHERE m2.show_key IN (SELECT value FROM json_each(?)) AND f2.trashed_at IS NULL
			AND (f2.category IN ('movie', 'tv') OR f2.mime LIKE 'video/%')
			AND f2.space_id IN (SELECT value FROM json_each(?)))
		WHERE rn = 1)`, append(w.args, string(kb), mustJSON(spaces))...)
	if err != nil {
		return nil, err
	}
	video := s.videoThumbs(ctx)
	for _, it := range items {
		out[it.ShowKey] = &showRep{fileID: it.FileID, poster: posterURL(it, video)}
	}
	// Backdrop and description: first episode that has one.
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.show_key,
		(SELECT m3.file_id || '/' || m3.backdrop_path FROM media_items m3 JOIN files f3 ON f3.id = m3.file_id WHERE m3.show_key = m.show_key
			AND m3.backdrop_path IS NOT NULL AND f3.trashed_at IS NULL AND f3.space_id IN (SELECT value FROM json_each(?2))
			ORDER BY m3.season, m3.episode LIMIT 1),
		(SELECT m4.show_description FROM media_items m4 JOIN files f4 ON f4.id = m4.file_id WHERE m4.show_key = m.show_key
			AND m4.show_description <> '' AND f4.trashed_at IS NULL AND f4.space_id IN (SELECT value FROM json_each(?2))
			ORDER BY m4.season, m4.episode LIMIT 1)
		FROM (SELECT DISTINCT value AS show_key FROM json_each(?1)) m`, string(kb), mustJSON(spaces))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var (
			key      string
			backdrop *string
			desc     *string
		)
		if err := rs.Scan(&key, &backdrop, &desc); err != nil {
			return nil, err
		}
		rep := out[key]
		if rep == nil {
			continue
		}
		if backdrop != nil {
			if id, rel, ok := strings.Cut(*backdrop, "/"); ok {
				rep.backdrop = strPtr("/api/v1/media/" + id + "/backdrop?v=" + assetVersion(rel))
			}
		}
		if desc != nil {
			rep.description = *desc
		}
	}
	return out, rs.Err()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ShowSeason groups the episodes of one season.
type ShowSeason struct {
	Season   int64   `json:"season"`
	Episodes []Media `json:"episodes"`
}

// ShowDetail is GET /media/shows/{show_key}.
type ShowDetail struct {
	ShowKey     string       `json:"show_key"`
	ShowTitle   string       `json:"show_title"`
	PosterURL   *string      `json:"poster_url"`
	BackdropURL *string      `json:"backdrop_url"`
	Description string       `json:"description"`
	Year        *int64       `json:"year"`
	Genres      []string     `json:"genres"`
	Episodes    int          `json:"episodes"`
	Watched     int          `json:"watched"`
	Seasons     []ShowSeason `json:"seasons"`
}

func (s *Service) showDetail(ctx context.Context, p *core.Principal, key string) (*ShowDetail, error) {
	if !showKeyRe.MatchString(key) {
		return nil, core.NotFound("Show not found")
	}
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{args: []any{p.UserID}}
	baseWhere(w, spaces, nil)
	w.add(`m.show_key = ?`, key)
	items, err := queryItems(ctx, s.d.DB.SQL, `SELECT `+itemCols+` `+itemFrom+` WHERE `+w.sql()+`
		ORDER BY COALESCE(m.season, 0), COALESCE(m.episode, 1e9), m.sort_title COLLATE NOCASE, m.file_id LIMIT ?`,
		append(w.args, maxShowEpisodes)...)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, core.NotFound("Show not found")
	}
	out := &ShowDetail{ShowKey: key, Seasons: []ShowSeason{}, Genres: []string{}, Episodes: len(items)}
	titles := map[string]int{}
	genreSeen := map[string]bool{}
	video := s.videoThumbs(ctx)
	for _, it := range items {
		titles[it.ShowTitle]++
		if it.Year.Valid && (out.Year == nil || it.Year.Int64 < *out.Year) {
			y := it.Year.Int64
			out.Year = &y
		}
		for _, g := range it.Genres {
			if k := strings.ToLower(g); !genreSeen[k] && len(out.Genres) < maxGenres {
				genreSeen[k] = true
				out.Genres = append(out.Genres, g)
			}
		}
		if it.Completed {
			out.Watched++
		}
		season := int64(0)
		if it.Season.Valid {
			season = it.Season.Int64
		}
		if n := len(out.Seasons); n == 0 || out.Seasons[n-1].Season != season {
			out.Seasons = append(out.Seasons, ShowSeason{Season: season, Episodes: []Media{}})
		}
		last := &out.Seasons[len(out.Seasons)-1]
		last.Episodes = append(last.Episodes, it.toAPI(video))
	}
	best := 0
	for t, n := range titles {
		if n > best || (n == best && t < out.ShowTitle) {
			out.ShowTitle, best = t, n
		}
	}
	reps, err := s.showRepresentatives(ctx, p, spaces, []string{key})
	if err != nil {
		return nil, err
	}
	if rep := reps[key]; rep != nil {
		out.PosterURL, out.BackdropURL, out.Description = rep.poster, rep.backdrop, rep.description
	}
	return out, nil
}
