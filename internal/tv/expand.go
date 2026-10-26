package tv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"ludworks/internal/core"
	"ludworks/internal/media/naming"
	"ludworks/internal/tv/schedule"
)

// Skip reasons reported by GET /tv/lineups/{id}/expanded.
const (
	ReasonUnreadable      = "unreadable"       // missing, trashed or not readable by the lineup owner
	ReasonUnknownDuration = "unknown_duration" // no (or a zero/absurd) duration
	ReasonNotFound        = "not_found"        // a folder item whose folder does not exist
)

// cacheTTL bounds staleness for changes that publish no event (library membership,
// media metadata edits).
const cacheTTL = time.Minute

// expFile is one file of an expanded lineup with the metadata programs need.
type expFile struct {
	FileID          string
	SourceItemID    string
	Group           string
	Title           string
	ShowTitle       string
	EpisodeTitle    string
	Season, Episode *int
	Duration        float64
	BrowserPlayable bool
	HasPoster       bool
	ThumbState      int
	Mime            string
	Subs            []fileSub // subtitle tracks (at most MaxProgramSubtitles)
	hasMedia        bool
}

// fileSub is one subtitle track of a file, from media_items.subtitles (Index is its position
// there, which the media service's subtitle endpoints use).
type fileSub struct {
	Index           int
	Language, Label string
	Embedded        bool // extracted by ffmpeg on request
}

// expansion is a lineup expanded to files (API.md §7.1 step 1).
type expansion struct {
	items        []schedule.Item     // playable files in default order (Group set for random items)
	byID         map[string]*expFile // playable files by id
	ordered      []*expFile          // playable files in default order (parallel to items)
	skipped      []SkippedItem       // the first MaxSkippedReported skipped files
	skippedCount int                 // all skipped files
	truncated    bool                // a limit stopped the expansion early
	total        float64
	built        time.Time
}

// count is the number of playable files.
func (e *expansion) count() int { return len(e.items) }

// skip records a skipped file; only the first MaxSkippedReported are kept.
func (e *expansion) skip(s SkippedItem) {
	e.skippedCount++
	if len(e.skipped) < MaxSkippedReported {
		e.skipped = append(e.skipped, s)
	}
}

// weight approximates an expansion's memory for the cache budget.
func (e *expansion) weight() int { return len(e.items) + len(e.skipped) + 1 }

// Cache bounds: expansions kept (by count and by total weight) and cold builds running at
// once server-wide. A build is bounded by MaxExpandWork, but every visible lineup or channel
// of a listing or guide may need one.
const (
	maxCacheEntries     = 2000
	maxCacheWeight      = 500_000
	maxConcurrentBuilds = 4
)

// expCache caches expansions per lineup. Entries are invalidated on lineup/channel changes
// and file events (generation counter) and expire after cacheTTL.
type expCache struct {
	mu       sync.Mutex
	gen      uint64
	entries  map[string]*cacheEntry
	weight   int // sum of the entries' weights
	building map[string]*buildCall
	sem      chan struct{} // cold builds in progress
}

type cacheEntry struct {
	exp *expansion
	gen uint64
}

type buildCall struct {
	done chan struct{}
	exp  *expansion
	err  error
}

func newExpCache() *expCache {
	return &expCache{entries: map[string]*cacheEntry{}, building: map[string]*buildCall{},
		sem: make(chan struct{}, maxConcurrentBuilds)}
}

// invalidateAll drops every cached expansion.
func (c *expCache) invalidateAll() {
	c.mu.Lock()
	c.gen++
	c.entries = map[string]*cacheEntry{}
	c.weight = 0
	c.mu.Unlock()
}

// invalidate drops one lineup's expansion.
func (c *expCache) invalidate(lineupID string) {
	c.mu.Lock()
	c.gen++ // in-flight builds of any lineup may have read the old state
	c.dropLocked(lineupID)
	c.mu.Unlock()
}

func (c *expCache) dropLocked(lineupID string) {
	if e, ok := c.entries[lineupID]; ok {
		c.weight -= e.exp.weight()
		delete(c.entries, lineupID)
	}
}

// storeLocked caches exp, evicting arbitrary entries to stay within the cache bounds.
func (c *expCache) storeLocked(lineupID string, exp *expansion, gen uint64) {
	c.dropLocked(lineupID)
	w := exp.weight()
	for id := range c.entries {
		if len(c.entries) < maxCacheEntries && c.weight+w <= maxCacheWeight {
			break
		}
		c.dropLocked(id)
	}
	c.entries[lineupID] = &cacheEntry{exp: exp, gen: gen}
	c.weight += w
}

// runBuild runs build once one of the maxConcurrentBuilds build slots is free.
func (c *expCache) runBuild(ctx context.Context, build func(context.Context) (*expansion, error)) (*expansion, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	return build(ctx)
}

// get returns the cached expansion of a lineup or builds it (one build per lineup at a time).
func (c *expCache) get(ctx context.Context, lineupID string, now time.Time, build func(context.Context) (*expansion, error)) (*expansion, error) {
	for {
		c.mu.Lock()
		if e, ok := c.entries[lineupID]; ok && e.gen == c.gen && now.Sub(e.exp.built) < cacheTTL && !now.Before(e.exp.built) {
			c.mu.Unlock()
			return e.exp, nil
		}
		if call, ok := c.building[lineupID]; ok {
			c.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if call.err == nil {
				return call.exp, nil
			}
			continue // the other build failed (perhaps its context was cancelled): try ourselves
		}
		call := &buildCall{done: make(chan struct{})}
		c.building[lineupID] = call
		gen := c.gen
		c.mu.Unlock()

		call.exp, call.err = c.runBuild(ctx, build)

		c.mu.Lock()
		delete(c.building, lineupID)
		if call.err == nil && gen == c.gen {
			c.storeLocked(lineupID, call.exp, gen)
		}
		c.mu.Unlock()
		close(call.done)
		return call.exp, call.err
	}
}

/* ---------------------------------------------------------------- building */

// ownerPrincipal builds the principal whose file permissions apply to a lineup/channel
// owner. Disabled or deleted owners get nil (no access to anything).
func (m *Module) ownerPrincipal(ctx context.Context, ownerID string) (*core.Principal, error) {
	if m.d.Users == nil {
		return nil, nil
	}
	u, err := m.d.Users.GetUser(ctx, ownerID)
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	if u == nil || u.Disabled {
		return nil, nil
	}
	return &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Scopes: []string{core.ScopeAll}, Kind: core.KindSession}, nil
}

// expansionFor returns the (cached) expansion of a lineup owned by ownerID.
func (m *Module) expansionFor(ctx context.Context, lineupID, ownerID string) (*expansion, error) {
	return m.cache.get(ctx, lineupID, m.now(), func(ctx context.Context) (*expansion, error) {
		items, err := loadItems(ctx, m.d.DB.SQL, lineupID)
		if err != nil {
			return nil, err
		}
		return m.expand(ctx, ownerID, items)
	})
}

// spaceSets resolves the spaces an owner's lineup may draw from: explicit references (file
// and folder items) may use every space the owner can read; implicit sweeps (show, season,
// query) skip other users' home spaces (admins can read them, but private homes are never
// swept into a channel implicitly — the same rule the files search applies).
func (m *Module) spaceSets(ctx context.Context, p *core.Principal) (explicit map[string]bool, implicit []string, err error) {
	explicit = map[string]bool{}
	implicit = []string{}
	if p == nil || m.files == nil {
		return explicit, implicit, nil
	}
	ids, err := m.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return explicit, implicit, nil
	}
	for _, id := range ids {
		explicit[id] = true
	}
	b, _ := json.Marshal(ids)
	rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE id IN (SELECT value FROM json_each(?))
		AND NOT (kind = 'home' AND owner_id IS NOT ?) ORDER BY id`, string(b), p.UserID)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, nil, err
		}
		implicit = append(implicit, id)
	}
	return explicit, implicit, rs.Err()
}

// mediaReady reports whether the media module's media_items table exists (the media module
// creates it; before that every file has an unknown duration).
func (m *Module) mediaReady(ctx context.Context) (bool, error) {
	if m.mediaTable.Load() {
		return true, nil
	}
	var n int
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'media_items'`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		m.mediaTable.Store(true)
	}
	return n > 0, nil
}

// candidate columns: files LEFT JOIN media_items.
const candCols = `f.id, f.name, f.space_id, f.path, f.mime, f.thumb_state, m.file_id IS NOT NULL,
	COALESCE(m.title, ''), COALESCE(m.show_title, ''), m.season, m.episode, COALESCE(m.episode_title, ''),
	m.duration_seconds, COALESCE(m.browser_playable, 0), m.poster_path IS NOT NULL AND m.poster_path <> '',
	COALESCE(m.subtitles, '[]')`

// candidate cols without media_items (media table missing).
const candColsNoMedia = `f.id, f.name, f.space_id, f.path, f.mime, f.thumb_state, 0, '', '', NULL, NULL, '', NULL, 0, 0, '[]'`

type candidate struct {
	expFile
	spaceID, path string
}

// parseSubs reads media_items.subtitles ([{language,label,format,…}], see the media module),
// keeping the first MaxProgramSubtitles tracks.
func parseSubs(raw string) []fileSub {
	if raw == "" || raw == "[]" {
		return nil
	}
	var stored []struct {
		Language string `json:"language"`
		Label    string `json:"label"`
		Format   string `json:"format"`
	}
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return nil
	}
	out := make([]fileSub, 0, min(len(stored), MaxProgramSubtitles))
	for i, s := range stored {
		if len(out) == MaxProgramSubtitles {
			break
		}
		out = append(out, fileSub{Index: i, Language: s.Language, Label: s.Label, Embedded: s.Format == "embedded"})
	}
	return out
}

// scanCandidates reads at most max candidate rows.
func scanCandidates(rs *sql.Rows, max int) ([]candidate, error) {
	defer rs.Close()
	var out []candidate
	for len(out) < max && rs.Next() {
		var (
			c               candidate
			name, subs      string
			season, episode sql.NullInt64
			dur             sql.NullFloat64
		)
		if err := rs.Scan(&c.FileID, &name, &c.spaceID, &c.path, &c.Mime, &c.ThumbState, &c.hasMedia, &c.Title, &c.ShowTitle,
			&season, &episode, &c.EpisodeTitle, &dur, &c.BrowserPlayable, &c.HasPoster, &subs); err != nil {
			return nil, err
		}
		c.Subs = parseSubs(subs)
		if season.Valid {
			s := int(season.Int64)
			c.Season = &s
		}
		if episode.Valid {
			e := int(episode.Int64)
			c.Episode = &e
		}
		if dur.Valid {
			c.Duration = dur.Float64
		}
		if strings.TrimSpace(c.Title) == "" {
			c.Title = strings.TrimSuffix(name, path.Ext(name))
			if c.Title == "" {
				c.Title = name
			}
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// sourceKey identifies what a lineup item draws from (kind and parameters, without its
// order, limit or label), so a source repeated across items is looked up once per expansion.
func sourceKey(it LineupItem) string {
	b, _ := json.Marshal(itemParams{FileID: it.FileID, ShowTitle: it.ShowTitle, Season: it.Season, SpaceID: it.SpaceID,
		Path: it.Path, Recursive: it.Recursive, Query: it.Query})
	return it.Kind + "|" + string(b)
}

// source is the looked-up result of a lineup item's source.
type source struct {
	cands  []candidate
	cut    bool   // the query was limited by the remaining work budget (there may be more)
	reason string // ReasonUnreadable / ReasonNotFound when the source itself is missing
}

// expand resolves lineup items to files for ownerID (API.md §7.1 step 1).
//
// The work is bounded whatever the lineup's shape: every item and every candidate file it
// yields (playable or skipped, including repeats of a source already looked up) counts
// towards MaxExpandWork; each distinct source is queried once, with a LIMIT of the remaining
// budget; at most MaxExpandedFiles playable files are kept and MaxSkippedReported skipped
// entries listed. Hitting a limit sets truncated.
func (m *Module) expand(ctx context.Context, ownerID string, items []LineupItem) (*expansion, error) {
	exp := &expansion{byID: map[string]*expFile{}, skipped: []SkippedItem{}, built: m.now()}
	p, err := m.ownerPrincipal(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	explicit, implicit, err := m.spaceSets(ctx, p)
	if err != nil {
		return nil, err
	}
	ready, err := m.mediaReady(ctx)
	if err != nil {
		return nil, err
	}
	implicitJSON, _ := json.Marshal(implicit)
	sources := map[string]*source{}
	total, work := 0, 0
	for n, it := range items {
		if total >= MaxExpandedFiles || work >= MaxExpandWork {
			exp.truncated = true
			break
		}
		if n%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		work++ // every item costs at least one unit, even when its source is empty
		key := sourceKey(it)
		src, seen := sources[key]
		if !seen {
			src, err = m.lookupSource(ctx, it, explicit, implicit, implicitJSON, ready, MaxExpandWork-work)
			if err != nil {
				return nil, err
			}
			sources[key] = src
		}
		if src.reason != "" {
			exp.skip(SkippedItem{FileID: missingID(it), Reason: src.reason, SourceItemID: it.ID})
			continue
		}
		cands := src.cands
		if room := MaxExpandWork - work; len(cands) > room {
			cands, exp.truncated = cands[:max(room, 0)], true
		}
		if src.cut {
			exp.truncated = true
		}
		work += len(cands)
		// Split playable from unknown durations, then apply the limit to the playable ones.
		var play []candidate
		for _, c := range cands {
			if schedule.Playable(c.Duration) {
				play = append(play, c)
			} else {
				exp.skip(SkippedItem{FileID: c.FileID, Reason: ReasonUnknownDuration, SourceItemID: it.ID, Title: c.Title})
			}
		}
		if it.Limit != nil && len(play) > *it.Limit {
			if it.Order == OrderRandom {
				// Deterministic subset per lineup item (keeps the cycle length constant).
				schedule.Shuffle(play, schedule.Seed64("limit:"+it.ID))
			}
			play = play[:*it.Limit]
		}
		for _, c := range play {
			if total >= MaxExpandedFiles {
				exp.truncated = true
				break
			}
			f := c.expFile
			f.SourceItemID = it.ID
			if it.Order == OrderRandom {
				f.Group = it.ID
			}
			fp := &f
			exp.ordered = append(exp.ordered, fp)
			exp.items = append(exp.items, schedule.Item{FileID: f.FileID, Duration: f.Duration, SourceItemID: it.ID, Group: f.Group})
			if _, dup := exp.byID[f.FileID]; !dup {
				exp.byID[f.FileID] = fp
			}
			total++
		}
	}
	exp.total = schedule.TotalSeconds(exp.items)
	return exp, nil
}

// missingID is the file_id reported for a lineup item whose source is missing: the file id
// for file items, the lineup item id otherwise (API.md §7.2).
func missingID(it LineupItem) string {
	if it.Kind == KindFile {
		return it.FileID
	}
	return it.ID
}

// lookupSource queries the candidate files of one lineup item's source: at most budget rows
// (and never more than MaxExpandedFiles + 1, the most one item can contribute).
func (m *Module) lookupSource(ctx context.Context, it LineupItem, explicit map[string]bool, implicit []string,
	implicitJSON []byte, ready bool, budget int) (*source, error) {
	limit := max(0, min(budget, MaxExpandedFiles+1))
	q := m.d.DB.SQL
	var (
		rs  *sql.Rows
		err error
	)
	switch it.Kind {
	case KindFile:
		cols := candCols
		from := ` FROM files f LEFT JOIN media_items m ON m.file_id = f.id`
		if !ready {
			cols, from = candColsNoMedia, ` FROM files f`
		}
		rs, err = q.QueryContext(ctx, `SELECT `+cols+from+` WHERE f.id = ? AND f.is_dir = 0 AND f.trashed_at IS NULL`, it.FileID)
		if err != nil {
			return nil, err
		}
		cands, err := scanCandidates(rs, 1)
		if err != nil {
			return nil, err
		}
		if len(cands) == 0 || !explicit[cands[0].spaceID] {
			return &source{reason: ReasonUnreadable}, nil
		}
		return &source{cands: cands}, nil
	case KindShow, KindSeason:
		if !ready || len(implicit) == 0 {
			return &source{}, nil
		}
		args := []any{naming.ShowKey(it.ShowTitle), it.ShowTitle, string(implicitJSON)}
		season := ""
		if it.Kind == KindSeason && it.Season != nil {
			season = ` AND m.season = ?`
			args = append(args, *it.Season)
		}
		rs, err = q.QueryContext(ctx, `SELECT `+candCols+` FROM media_items m JOIN files f ON f.id = m.file_id
			WHERE (m.show_key = ? OR m.show_title = ? COLLATE NOCASE) AND f.is_dir = 0 AND f.trashed_at IS NULL
			AND f.space_id IN (SELECT value FROM json_each(?))`+season+`
			ORDER BY m.season IS NULL, m.season, m.episode IS NULL, m.episode, m.title COLLATE NOCASE, f.path, f.id
			LIMIT ?`, append(args, limit)...)
	case KindFolder:
		if it.Path == nil || !explicit[it.SpaceID] {
			return &source{reason: ReasonNotFound}, nil
		}
		var folderID string
		err := q.QueryRowContext(ctx, `SELECT id FROM files WHERE space_id = ? AND path = ? AND is_dir = 1 AND trashed_at IS NULL`,
			it.SpaceID, *it.Path).Scan(&folderID)
		if errors.Is(err, sql.ErrNoRows) {
			return &source{reason: ReasonNotFound}, nil
		}
		if err != nil {
			return nil, err
		}
		cols, from := candCols, ` FROM files f LEFT JOIN media_items m ON m.file_id = f.id`
		media := `(m.file_id IS NOT NULL OR f.mime LIKE 'video/%' OR f.mime LIKE 'audio/%')`
		if !ready {
			cols, from, media = candColsNoMedia, ` FROM files f`, `(f.mime LIKE 'video/%' OR f.mime LIKE 'audio/%')`
		}
		var where string
		args := []any{}
		switch {
		case it.Recursive != nil && !*it.Recursive:
			where, args = `f.parent_id = ?`, append(args, folderID)
		case *it.Path == "":
			where, args = `f.space_id = ? AND f.path <> ''`, append(args, it.SpaceID)
		default:
			where, args = `f.space_id = ? AND f.path >= ? AND f.path < ?`, append(args, it.SpaceID, *it.Path+"/", *it.Path+"0")
		}
		rs, err = q.QueryContext(ctx, `SELECT `+cols+from+` WHERE `+where+` AND f.is_dir = 0 AND f.trashed_at IS NULL AND `+
			media+` LIMIT ?`, append(args, limit)...)
		if err != nil {
			return nil, err
		}
		cands, err := scanCandidates(rs, limit)
		if err != nil {
			return nil, err
		}
		slices.SortStableFunc(cands, func(a, b candidate) int { return naturalCompare(a.path, b.path) })
		return &source{cands: cands, cut: len(cands) == limit && limit < MaxExpandedFiles+1}, nil
	case KindQuery:
		if !ready || len(implicit) == 0 {
			return &source{}, nil
		}
		where, args := queryFilter(it.Query)
		args = append([]any{string(implicitJSON)}, args...)
		rs, err = q.QueryContext(ctx, `SELECT `+candCols+` FROM media_items m JOIN files f ON f.id = m.file_id
			WHERE f.is_dir = 0 AND f.trashed_at IS NULL AND f.space_id IN (SELECT value FROM json_each(?))`+where+`
			ORDER BY COALESCE(NULLIF(m.show_title, ''), NULLIF(m.sort_title, ''), m.title) COLLATE NOCASE,
			m.season IS NULL, m.season, m.episode IS NULL, m.episode, m.title COLLATE NOCASE, f.path, f.id
			LIMIT ?`, append(args, limit)...)
	default:
		return &source{}, nil
	}
	if err != nil {
		return nil, err
	}
	cands, err := scanCandidates(rs, limit)
	if err != nil {
		return nil, err
	}
	return &source{cands: cands, cut: len(cands) == limit && limit < MaxExpandedFiles+1}, nil
}

// queryFilter builds the WHERE fragment for a kind=query item (parameterised).
func queryFilter(q *LineupQuery) (string, []any) {
	if q == nil {
		q = &LineupQuery{}
	}
	var (
		sb   strings.Builder
		args []any
	)
	cats := q.Category
	if cats == "" {
		cats = "movie,tv"
	}
	cb, _ := json.Marshal(strings.Split(cats, ","))
	sb.WriteString(` AND f.category IN (SELECT value FROM json_each(?))`)
	args = append(args, string(cb))
	if q.Subcategory != "" {
		sb2, _ := json.Marshal(strings.Split(q.Subcategory, ","))
		sb.WriteString(` AND f.subcategory IN (SELECT value FROM json_each(?))`)
		args = append(args, string(sb2))
	}
	if q.Genre != "" {
		sb.WriteString(` AND CASE WHEN json_valid(m.genres) THEN EXISTS (SELECT 1 FROM json_each(m.genres) g
			WHERE g.value = ? COLLATE NOCASE) ELSE 0 END`)
		args = append(args, q.Genre)
	}
	if q.Tag != "" {
		sb.WriteString(` AND CASE WHEN json_valid(f.tags) THEN EXISTS (SELECT 1 FROM json_each(f.tags) t
			WHERE t.value = ? COLLATE NOCASE) ELSE 0 END`)
		args = append(args, q.Tag)
	}
	if q.Year != nil {
		sb.WriteString(` AND m.year = ?`)
		args = append(args, *q.Year)
	}
	return sb.String(), args
}

// naturalCompare orders strings case-insensitively with embedded numbers compared by value
// ("Episode 2" < "Episode 10").
func naturalCompare(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ca, cb := a[i], b[j]
		if isDigit(ca) && isDigit(cb) {
			si := i
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			sj := j
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na := strings.TrimLeft(a[si:i], "0")
			nb := strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				return cmpInt(len(na), len(nb))
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		la, lb := lower(ca), lower(cb)
		if la != lb {
			return cmpInt(int(la), int(lb))
		}
		i++
		j++
	}
	if c := cmpInt(len(a)-i, len(b)-j); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// expandedView renders an expansion for GET /tv/lineups/{id}/expanded.
func (e *expansion) expandedView() Expanded {
	out := Expanded{Items: make([]ExpandedItem, 0, len(e.ordered)), TotalDurationSeconds: e.total, Skipped: e.skipped,
		SkippedCount: e.skippedCount, Truncated: e.truncated}
	for _, f := range e.ordered {
		out.Items = append(out.Items, ExpandedItem{FileID: f.FileID, Title: displayTitle(f), DurationSeconds: f.Duration, SourceItemID: f.SourceItemID})
	}
	if out.Skipped == nil {
		out.Skipped = []SkippedItem{}
	}
	return out
}

// displayTitle is a one-line program title ("Show · S01E02 · Episode" for episodes), with
// the same zero-padded episode code as the web UI and the XMLTV "onscreen" numbering.
func displayTitle(f *expFile) string {
	if f.ShowTitle == "" {
		return f.Title
	}
	parts := []string{f.ShowTitle}
	if f.Season != nil && f.Episode != nil {
		parts = append(parts, "S"+pad2(*f.Season)+"E"+pad2(*f.Episode))
	}
	if t := episodeSubtitle(f); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, " · ")
}

// episodeSubtitle is what to show after the show title and episode code: the episode title,
// else the item title unless it merely repeats the show and episode number — titles guessed
// from file names ("Garden Hour S01E02" for Garden.Hour.S01E02.mkv) would otherwise read
// "Garden Hour · S01E02 · Garden Hour S01E02".
func episodeSubtitle(f *expFile) string {
	if f.EpisodeTitle != "" {
		return f.EpisodeTitle
	}
	if f.Title == "" || titleRepeatsShow(f.Title, f.ShowTitle, f.Season, f.Episode) {
		return ""
	}
	return f.Title
}

// titleRepeatsShow reports whether title is just the show title, optionally followed by the
// episode number in a common form (S01E02, 1x02, E02, Episode 2), ignoring case and punctuation.
func titleRepeatsShow(title, show string, season, episode *int) bool {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	rest, sh := norm(title), norm(show)
	if rest == "" || rest == sh {
		return true
	}
	if sh == "" || !strings.HasPrefix(rest, sh) || episode == nil {
		return false
	}
	rest = rest[len(sh):]
	e := *episode
	forms := []string{"e" + itoa(e), "e" + pad2(e), "episode" + itoa(e), "ep" + itoa(e), "ep" + pad2(e)}
	if season != nil {
		for _, ss := range []string{itoa(*season), pad2(*season)} {
			for _, ee := range []string{itoa(e), pad2(e)} {
				forms = append(forms, "s"+ss+"e"+ee, ss+"x"+ee)
			}
		}
	}
	return slices.Contains(forms, rest)
}

func itoa(n int) string { return strconv.Itoa(n) }
