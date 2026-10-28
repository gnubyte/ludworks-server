package music

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// Smart playlists (API.md §12): rules validated against a whitelist and compiled to
// parameterised SQL over the caller's tracks and their plays, favorites and ratings. Field
// names and operators never reach the SQL text except through the fixed tables below.

const (
	maxConditions  = 20
	maxSmartLimit  = 5000
	maxRuleText    = 500
	maxRuleDays    = 36500
	smartPoolLimit = maxPlaylistItems // tracks a smart playlist without a limit holds
)

// SmartRules is the rules object of a smart playlist.
type SmartRules struct {
	Match      string      `json:"match"`
	Conditions []Condition `json:"conditions"`
	Sort       string      `json:"sort"`
	Order      string      `json:"order"`
	Limit      *int        `json:"limit"`
}

// Condition is one rule.
type Condition struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

const (
	kindText = iota
	kindNumber
	kindDate
	kindBool
)

// ruleFields maps rule fields to their kind and column (or the special handling below).
var ruleFields = map[string]struct {
	kind int
	col  string
}{
	"title": {kindText, "t.title_key"}, "artist": {kindText, "t.artist_key"}, "album": {kindText, "t.album_key"},
	"album_artist": {kindText, "t.album_artist_key"}, "genre": {kindText, ""}, "composer": {kindText, "t.composer_key"},
	"path": {kindText, "f.path"}, "codec": {kindText, "t.codec"}, "subcategory": {kindText, "f.subcategory"},
	"year": {kindNumber, "t.year"}, "duration_seconds": {kindNumber, "t.duration_seconds"},
	"play_count": {kindNumber, "COALESCE(ps.play_count, 0)"}, "rating": {kindNumber, "ra.rating"},
	"bitrate_kbps": {kindNumber, "t.bitrate_kbps"},
	"added_at":     {kindDate, "t.added_at"}, "last_played_at": {kindDate, "ps.last_played_at"},
	"favorite": {kindBool, "fv.file_id IS NOT NULL"}, "compilation": {kindBool, "t.compilation = 1"},
}

var ruleOps = map[int][]string{
	kindText:   {"is", "is_not", "contains", "not_contains", "starts_with"},
	kindNumber: {"eq", "ne", "gt", "gte", "lt", "lte", "between"},
	kindDate:   {"in_last_days", "not_in_last_days", "before", "after"},
	kindBool:   {"is_true", "is_false"},
}

var numberOps = map[string]string{"eq": "=", "ne": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// foldedCols are compared with folded values; the others case-insensitively as stored.
var foldedCols = map[string]bool{"title": true, "artist": true, "album": true, "album_artist": true, "composer": true, "genre": true}

// validateRules checks and normalises rules (422 with rules.* field errors).
func validateRules(raw json.RawMessage) (*SmartRules, error) {
	var r SmartRules
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&r); err != nil {
		return nil, core.Validation(map[string]string{"rules": "must be an object with match, conditions, sort, order and limit"})
	}
	fe := map[string]string{}
	switch r.Match {
	case "":
		r.Match = "all"
	case "all", "any":
	default:
		fe["rules.match"] = "must be all or any"
	}
	if len(r.Conditions) < 1 || len(r.Conditions) > maxConditions {
		fe["rules.conditions"] = fmt.Sprintf("1-%d conditions are required", maxConditions)
	}
	if r.Sort == "" {
		r.Sort = "title"
	}
	if !slices.Contains(trackSorts, r.Sort) {
		fe["rules.sort"] = "must be one of " + strings.Join(trackSorts, ", ")
	}
	switch r.Order {
	case "":
		r.Order = "asc"
	case "asc", "desc":
	default:
		fe["rules.order"] = "must be asc or desc"
	}
	if r.Limit != nil && (*r.Limit < 1 || *r.Limit > maxSmartLimit) {
		fe["rules.limit"] = fmt.Sprintf("must be between 1 and %d, or null", maxSmartLimit)
	}
	for i := range r.Conditions {
		c := &r.Conditions[i]
		key := fmt.Sprintf("rules.conditions[%d]", i)
		f, ok := ruleFields[c.Field]
		if !ok {
			fe[key+".field"] = "unknown field"
			continue
		}
		if !slices.Contains(ruleOps[f.kind], c.Op) {
			fe[key+".op"] = "must be one of " + strings.Join(ruleOps[f.kind], ", ")
			continue
		}
		v, msg := normalizeRuleValue(f.kind, c.Op, c.Value)
		if msg != "" {
			fe[key+".value"] = msg
			continue
		}
		c.Value = v
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	return &r, nil
}

// normalizeRuleValue validates a condition value and returns its canonical JSON.
func normalizeRuleValue(kind int, op string, v json.RawMessage) (json.RawMessage, string) {
	marshal := func(x any) json.RawMessage { b, _ := json.Marshal(x); return b }
	switch kind {
	case kindText:
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, "must be a string"
		}
		s = strings.TrimSpace(s)
		if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 }) || len([]rune(s)) > maxRuleText {
			return nil, fmt.Sprintf("must be a single line of at most %d characters", maxRuleText)
		}
		if s == "" && op != "is" && op != "is_not" {
			return nil, "must not be empty"
		}
		return marshal(s), ""
	case kindNumber:
		finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e15 }
		if op == "between" {
			var pair []float64
			if json.Unmarshal(v, &pair) != nil || len(pair) != 2 || !finite(pair[0]) || !finite(pair[1]) || pair[0] > pair[1] {
				return nil, "must be [a, b] with a <= b"
			}
			return marshal(pair), ""
		}
		var f float64
		if json.Unmarshal(v, &f) != nil || !finite(f) {
			return nil, "must be a number"
		}
		return marshal(f), ""
	case kindDate:
		if op == "in_last_days" || op == "not_in_last_days" {
			var f float64
			if json.Unmarshal(v, &f) != nil || f != math.Trunc(f) || f < 1 || f > maxRuleDays {
				return nil, fmt.Sprintf("must be a number of days (1-%d)", maxRuleDays)
			}
			return marshal(int(f)), ""
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, "must be an RFC 3339 time"
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, "must be an RFC 3339 time"
		}
		return marshal(db.FormatTime(t)), ""
	}
	return json.RawMessage("null"), ""
}

// compileRules turns validated rules into a SQL condition (trackFrom joins required).
func compileRules(r *SmartRules, now time.Time) (string, []any) {
	parts := make([]string, 0, len(r.Conditions))
	var args []any
	for _, c := range r.Conditions {
		sql, a := compileCondition(c, now)
		parts = append(parts, "("+sql+")")
		args = append(args, a...)
	}
	sep := " AND "
	if r.Match == "any" {
		sep = " OR "
	}
	return "(" + strings.Join(parts, sep) + ")", args
}

func compileCondition(c Condition, now time.Time) (string, []any) {
	f := ruleFields[c.Field]
	switch f.kind {
	case kindText:
		var s string
		_ = json.Unmarshal(c.Value, &s)
		if foldedCols[c.Field] {
			s = fold(s)
		}
		pos, arg := "", any(s)
		switch c.Op {
		case "is", "is_not":
			pos = "= ?"
			if !foldedCols[c.Field] {
				pos = "= ? COLLATE NOCASE"
			}
		case "contains", "not_contains":
			pos, arg = `LIKE ? ESCAPE '\'`, "%"+db.EscapeLike(s)+"%"
		case "starts_with":
			pos, arg = `LIKE ? ESCAPE '\'`, db.EscapeLike(s)+"%"
		}
		var expr string
		var args []any
		switch c.Field {
		case "genre":
			expr, args = `EXISTS (SELECT 1 FROM music_track_genres g WHERE g.file_id = t.file_id AND g.genre_key `+pos+`)`, []any{arg}
		case "artist":
			// The display string or any of the individual track artists.
			expr = `(t.artist_key ` + pos + ` OR EXISTS (SELECT 1 FROM music_track_artists ta WHERE ta.file_id = t.file_id
				AND ta.role & 2 AND ta.name_key ` + pos + `))`
			args = []any{arg, arg}
		default:
			expr, args = f.col+" "+pos, []any{arg}
		}
		if c.Op == "is_not" || c.Op == "not_contains" {
			expr = "NOT " + expr
		}
		return expr, args
	case kindNumber:
		if c.Op == "between" {
			var pair []float64
			_ = json.Unmarshal(c.Value, &pair)
			return f.col + " BETWEEN ? AND ?", []any{pair[0], pair[1]}
		}
		var v float64
		_ = json.Unmarshal(c.Value, &v)
		return f.col + " " + numberOps[c.Op] + " ?", []any{v}
	case kindDate:
		switch c.Op {
		case "in_last_days", "not_in_last_days":
			var days int
			_ = json.Unmarshal(c.Value, &days)
			since := db.FormatTime(now.Add(-time.Duration(days) * 24 * time.Hour))
			if c.Op == "in_last_days" {
				return f.col + " >= ?", []any{since}
			}
			return "(" + f.col + " < ? OR " + f.col + " IS NULL)", []any{since}
		default:
			var t string
			_ = json.Unmarshal(c.Value, &t)
			if c.Op == "before" {
				return f.col + " < ?", []any{t}
			}
			return f.col + " > ?", []any{t}
		}
	default:
		if c.Op == "is_true" {
			return f.col, nil
		}
		return "NOT (" + f.col + ")", nil
	}
}

// evaluateSmart returns the track ids of a smart playlist for p: their library, their plays,
// favorites and ratings, sorted by the rules (random is stable for a day).
func (s *Service) evaluateSmart(ctx context.Context, p *core.Principal, playlistID string, r *SmartRules) ([]string, error) {
	spaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, err
	}
	now := s.now()
	w := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(w, spaces)
	cond, args := compileRules(r, now)
	w.add(cond, args...)
	limit := smartPoolLimit
	if r.Limit != nil {
		limit = min(*r.Limit, smartPoolLimit)
	}
	if r.Sort == "random" {
		ids, err := s.queryIDs(ctx, `SELECT t.file_id `+trackFrom+` WHERE `+w.sql()+` LIMIT ?`, append(w.args, maxRandomPool)...)
		if err != nil {
			return nil, err
		}
		shuffle(ids, playlistID+"|"+now.UTC().Format("2006-01-02"))
		return ids[:min(len(ids), limit)], nil
	}
	dir := "ASC"
	if r.Order == "desc" {
		dir = "DESC"
	}
	keys := trackSortKeys(r.Sort, r.Order)
	order := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		order = append(order, k+" "+dir)
	}
	order = append(order, "t.file_id "+dir)
	return s.queryIDs(ctx, `SELECT t.file_id `+trackFrom+` WHERE `+w.sql()+` ORDER BY `+strings.Join(order, ", ")+` LIMIT ?`,
		append(w.args, limit)...)
}
