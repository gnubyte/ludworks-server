package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/media/naming"
)

// Field limits (mirrored in openapi.json).
const (
	maxTitle       = 500
	maxDescription = 20000
	maxGenres      = 50
	maxGenreLen    = 64
	maxRating      = 32
	maxBulkIDs     = 1000
)

var imdbRe = regexp.MustCompile(`^tt[0-9]{5,10}$`)

// optInt is a nullable integer field of a patch: Set = present, Null = explicit null.
type optInt struct {
	Set, Null bool
	V         int64
}

// patch is a validated set of media field changes (nil / unset = unchanged).
type patch struct {
	Title, SortTitle, Description, ContentRating, ShowTitle, EpisodeTitle, IMDBID *string
	Year, Season, Episode, TMDBID                                                 optInt
	Genres                                                                        *[]string
	Category, Subcategory                                                         *string
	Watched                                                                       *bool // the caller's watched flag
}

func (p *patch) empty() bool {
	return p.Title == nil && p.SortTitle == nil && p.Description == nil && p.ContentRating == nil &&
		p.ShowTitle == nil && p.EpisodeTitle == nil && p.IMDBID == nil && !p.Year.Set && !p.Season.Set &&
		!p.Episode.Set && !p.TMDBID.Set && p.Genres == nil && p.Category == nil && p.Subcategory == nil && p.Watched == nil
}

func (p *patch) touchesMedia() bool {
	q := *p
	q.Category, q.Subcategory, q.Watched = nil, nil, nil
	return !q.empty()
}

// parsePatch validates a JSON object of media fields. Computed and unknown fields are
// ignored (so a GET result can be sent back); allowCategory also accepts category and
// subcategory (validated by the files taxonomy when applied).
func parsePatch(raw map[string]json.RawMessage, allowCategory bool) (*patch, error) {
	p := &patch{}
	fe := map[string]string{}
	str := func(key string, max int, multiline, required bool) *string {
		v, ok := raw[key]
		if !ok {
			return nil
		}
		var s string
		if isNull(v) {
			if required {
				fe[key] = "must not be null"
				return nil
			}
			return strPtr("")
		}
		if err := json.Unmarshal(v, &s); err != nil {
			fe[key] = "must be a string"
			return nil
		}
		if !multiline && strings.ContainsFunc(s, unicode.IsControl) {
			fe[key] = "must not contain control characters"
			return nil
		}
		if multiline {
			s = cleanMultiline(s, math.MaxInt)
		} else {
			s = strings.TrimSpace(s)
		}
		if len(s) > max {
			fe[key] = "too long (max " + itoa(max) + " bytes)"
			return nil
		}
		if required && s == "" {
			fe[key] = "required"
			return nil
		}
		return &s
	}
	num := func(key string, lo, hi int64) optInt {
		v, ok := raw[key]
		if !ok {
			return optInt{}
		}
		if isNull(v) {
			return optInt{Set: true, Null: true}
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || f != math.Trunc(f) {
			fe[key] = "must be an integer or null"
			return optInt{}
		}
		if f < float64(lo) || f > float64(hi) {
			fe[key] = "must be between " + itoa(int(lo)) + " and " + itoa(int(hi))
			return optInt{}
		}
		return optInt{Set: true, V: int64(f)}
	}
	p.Title = str("title", maxTitle, false, true)
	p.SortTitle = str("sort_title", maxTitle, false, false)
	p.Description = str("description", maxDescription, true, false)
	p.ContentRating = str("content_rating", maxRating, false, false)
	p.ShowTitle = str("show_title", maxTitle, false, false)
	p.EpisodeTitle = str("episode_title", maxTitle, false, false)
	p.IMDBID = str("imdb_id", 16, false, false)
	if p.IMDBID != nil && *p.IMDBID != "" && !imdbRe.MatchString(*p.IMDBID) {
		fe["imdb_id"] = "must look like tt0063350"
	}
	p.Year = num("year", 1800, 3000)
	p.Season = num("season", 0, 10000)
	p.Episode = num("episode", 0, 100000)
	p.TMDBID = num("tmdb_id", 1, 1<<40)
	if v, ok := raw["genres"]; ok {
		var gs []string
		if isNull(v) {
			gs = []string{}
		} else if err := json.Unmarshal(v, &gs); err != nil {
			fe["genres"] = "must be an array of strings"
		}
		if _, bad := fe["genres"]; !bad {
			norm, msg := normalizeGenres(gs)
			if msg != "" {
				fe["genres"] = msg
			} else {
				p.Genres = &norm
			}
		}
	}
	if v, ok := raw["watched"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			fe["watched"] = "must be a boolean"
		} else {
			p.Watched = &b
		}
	}
	if allowCategory {
		p.Category = str("category", 64, false, false)
		p.Subcategory = str("subcategory", 64, false, false)
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	return p, nil
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// normalizeGenres trims, drops empties and case-insensitive duplicates.
func normalizeGenres(in []string) ([]string, string) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, g := range in {
		g = strings.Join(strings.Fields(g), " ")
		if g == "" {
			continue
		}
		if strings.ContainsFunc(g, unicode.IsControl) {
			return nil, "genres must not contain control characters"
		}
		if len(g) > maxGenreLen {
			return nil, "each genre must be at most " + itoa(maxGenreLen) + " bytes"
		}
		k := strings.ToLower(g)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, g)
	}
	if len(out) > maxGenres {
		return nil, "at most " + itoa(maxGenres) + " genres"
	}
	return out, ""
}

// sortTitleFor derives a sort title: lower case without a leading English article.
func sortTitleFor(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(t, a) && len(t) > len(a) {
			t = strings.TrimSpace(t[len(a):])
			break
		}
	}
	return truncate(t, maxTitle)
}

// apply merges p into it (descriptive fields only; category is applied via files).
func (p *patch) apply(it *item) {
	oldDerived := it.SortTitle == sortTitleFor(it.Title)
	if p.Title != nil {
		it.Title = *p.Title
	}
	switch {
	case p.SortTitle != nil && *p.SortTitle != "":
		it.SortTitle = *p.SortTitle
	case p.SortTitle != nil || (p.Title != nil && oldDerived):
		it.SortTitle = sortTitleFor(it.Title)
	}
	if p.Description != nil {
		it.Description = *p.Description
	}
	if p.ContentRating != nil {
		it.ContentRating = *p.ContentRating
	}
	if p.ShowTitle != nil {
		it.ShowTitle = *p.ShowTitle
		it.ShowKey = naming.ShowKey(it.ShowTitle)
	}
	if p.EpisodeTitle != nil {
		it.EpisodeTitle = *p.EpisodeTitle
	}
	if p.IMDBID != nil {
		it.IMDBID = *p.IMDBID
	}
	setInt := func(dst *sql.NullInt64, o optInt) {
		if o.Set {
			*dst = sql.NullInt64{Int64: o.V, Valid: !o.Null}
		}
	}
	setInt(&it.Year, p.Year)
	setInt(&it.Season, p.Season)
	setInt(&it.Episode, p.Episode)
	setInt(&it.TMDBID, p.TMDBID)
	if p.Genres != nil {
		it.Genres = *p.Genres
	}
}

// saveDescriptive writes the descriptive columns of it.
func (s *Service) saveDescriptive(ctx context.Context, q db.Querier, it *item, source string) error {
	_, err := q.ExecContext(ctx, `UPDATE media_items SET title = ?, sort_title = ?, year = ?, description = ?, genres = ?,
		content_rating = ?, show_title = ?, show_key = ?, season = ?, episode = ?, episode_title = ?, tmdb_id = ?, imdb_id = ?,
		show_description = ?, meta_source = ?, updated_at = ? WHERE file_id = ?`,
		it.Title, it.SortTitle, it.Year, it.Description, db.JSON[[]string]{V: it.Genres}, it.ContentRating, it.ShowTitle,
		it.ShowKey, it.Season, it.Episode, it.EpisodeTitle, it.TMDBID, it.IMDBID, it.ShowDescription, source,
		db.FormatTime(s.now()), it.FileID)
	if err == nil {
		s.publishUpdated(it.FileID)
	}
	return err
}

// update applies a patch to one item for p (write access required).
func (s *Service) update(ctx context.Context, p *core.Principal, fileID string, pt *patch) (Media, error) {
	// The watched flag is per user and only needs read access.
	need := needWrite
	if !pt.touchesMedia() && pt.Category == nil && pt.Subcategory == nil {
		need = needRead
	}
	it, err := s.itemFor(ctx, p, fileID, need)
	if err != nil {
		return Media{}, err
	}
	if pt.Category != nil || pt.Subcategory != nil {
		if _, err := s.files.SetMeta(ctx, p, fileID, pt.Category, pt.Subcategory, nil); err != nil {
			return Media{}, err
		}
	}
	if pt.touchesMedia() {
		pt.apply(it)
		if err := s.saveDescriptive(ctx, s.d.DB.SQL, it, srcManual); err != nil {
			return Media{}, err
		}
	}
	if pt.Watched != nil && *pt.Watched != it.Completed {
		if err := s.setWatched(ctx, p, it, *pt.Watched); err != nil {
			return Media{}, err
		}
	}
	return s.mediaJSON(ctx, p, fileID)
}

// ItemError reports a per-item failure of a bulk operation.
type ItemError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func itemError(id string, err error) ItemError {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ItemError{ID: id, Error: ce.Message, Code: ce.Code}
	}
	return ItemError{ID: id, Error: "internal error", Code: "internal"}
}

// bulkUpdate applies pt to many items; items that are missing or not writable are reported.
func (s *Service) bulkUpdate(ctx context.Context, p *core.Principal, ids []string, pt *patch) (int, []ItemError, error) {
	updated := 0
	errs := []ItemError{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.update(ctx, p, id, pt); err != nil {
			var ce *core.Error
			if !errors.As(err, &ce) {
				return updated, errs, err
			}
			errs = append(errs, itemError(id, err))
			continue
		}
		updated++
	}
	return updated, errs, nil
}
