package tv

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// fieldErrs collects validation messages keyed by field path.
type fieldErrs map[string]string

func (f fieldErrs) set(k, msg string) {
	if _, ok := f[k]; !ok {
		f[k] = msg
	}
}

func (f fieldErrs) err() error {
	if len(f) == 0 {
		return nil
	}
	return core.Validation(f)
}

// hasControl reports control characters (tabs/newlines allowed when multiline).
func hasControl(s string, multiline bool) bool {
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t' || r == '\r') {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return true
		}
	}
	return false
}

// checkText validates a free-text field: valid UTF-8, no control characters, at most max
// characters; required rejects blank values. It returns the trimmed value.
func checkText(errs fieldErrs, field, v string, required bool, max int, multiline bool) string {
	if !multiline {
		v = strings.TrimSpace(v)
	}
	switch {
	case !utf8.ValidString(v):
		errs.set(field, "must be valid UTF-8")
	case required && strings.TrimSpace(v) == "":
		errs.set(field, "is required")
	case utf8.RuneCountInString(v) > max:
		errs.set(field, fmt.Sprintf("must be at most %d characters", max))
	case hasControl(v, multiline):
		errs.set(field, "must not contain control characters")
	}
	return v
}

// cleanFolderPath normalises a space-relative folder path ("" = space root). It rejects
// "." / ".." segments, backslashes and control characters rather than resolving them.
func cleanFolderPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	if len(p) > pathMax || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00") || hasControl(p, false) {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

// itemInput is a lineup item as sent by clients (id optional: keeps an existing item).
type itemInput struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	FileID    string       `json:"file_id"`
	ShowTitle string       `json:"show_title"`
	Season    *int         `json:"season"`
	SpaceID   string       `json:"space_id"`
	Path      *string      `json:"path"`
	Recursive *bool        `json:"recursive"`
	Query     *LineupQuery `json:"query"`
	Order     string       `json:"order"`
	Limit     *int         `json:"limit"`
	Label     string       `json:"label"`
}

// checkCSV validates a comma-separated list of slugs (categories/subcategories).
func checkCSV(errs fieldErrs, field, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	parts := strings.Split(v, ",")
	if len(parts) > maxCategoryListSize {
		errs.set(field, "too many values")
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if len(p) > 64 || strings.IndexFunc(p, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) >= 0 {
			errs.set(field, "must be a slug or a comma-separated list of slugs")
			return ""
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// normalizeItem validates one item input and returns the stored form. prefix is the field
// path used in error keys (e.g. "items.3").
func normalizeItem(errs fieldErrs, prefix string, in itemInput) LineupItem {
	f := func(name string) string { return prefix + "." + name }
	it := LineupItem{Kind: in.Kind, Order: in.Order}
	if it.Order == "" {
		it.Order = OrderDefault
	}
	if it.Order != OrderDefault && it.Order != OrderRandom {
		errs.set(f("order"), "must be default or random")
	}
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > MaxItemLimit {
			errs.set(f("limit"), fmt.Sprintf("must be between 1 and %d", MaxItemLimit))
		} else {
			n := *in.Limit
			it.Limit = &n
		}
	}
	it.Label = checkText(errs, f("label"), in.Label, false, labelMax, false)
	switch in.Kind {
	case KindFile:
		if !httpx.ValidID(in.FileID) {
			errs.set(f("file_id"), "is required for kind file")
		}
		it.FileID = in.FileID
	case KindShow, KindSeason:
		it.ShowTitle = checkText(errs, f("show_title"), in.ShowTitle, true, showTitleMax, false)
		if in.Kind == KindSeason {
			if in.Season == nil || *in.Season < 0 || *in.Season > maxSeason {
				errs.set(f("season"), "is required for kind season (0 or more)")
			} else {
				s := *in.Season
				it.Season = &s
			}
		}
	case KindFolder:
		if !httpx.ValidID(in.SpaceID) {
			errs.set(f("space_id"), "is required for kind folder")
		}
		it.SpaceID = in.SpaceID
		p := ""
		if in.Path != nil {
			var ok bool
			if p, ok = cleanFolderPath(*in.Path); !ok {
				errs.set(f("path"), "is not a valid folder path")
			}
		}
		it.Path = &p
		rec := true
		if in.Recursive != nil {
			rec = *in.Recursive
		}
		it.Recursive = &rec
	case KindQuery:
		q := LineupQuery{}
		if in.Query != nil {
			q.Category = checkCSV(errs, f("query.category"), in.Query.Category)
			q.Subcategory = checkCSV(errs, f("query.subcategory"), in.Query.Subcategory)
			q.Genre = checkText(errs, f("query.genre"), in.Query.Genre, false, queryFieldMax, false)
			q.Tag = checkText(errs, f("query.tag"), in.Query.Tag, false, queryFieldMax, false)
			if in.Query.Year != nil {
				if *in.Query.Year < 1800 || *in.Query.Year > 3000 {
					errs.set(f("query.year"), "must be between 1800 and 3000")
				} else {
					y := *in.Query.Year
					q.Year = &y
				}
			}
		}
		it.Query = &q
	case "":
		errs.set(f("kind"), "is required")
	default:
		errs.set(f("kind"), "must be one of file, show, season, folder, query")
	}
	return it
}

// normalizeItems validates a list of item inputs. existing maps the lineup's current item
// ids so a client can keep an item's identity (and its random-order seed) across a full
// replacement by echoing its id; unknown or duplicate ids get a fresh id.
func normalizeItems(errs fieldErrs, field string, in []itemInput, existing map[string]bool) []LineupItem {
	out := make([]LineupItem, 0, len(in))
	seen := map[string]bool{}
	for i, raw := range in {
		it := normalizeItem(errs, field+"."+strconv.Itoa(i), raw)
		if raw.ID != "" && existing[raw.ID] && !seen[raw.ID] {
			it.ID = raw.ID
		} else {
			it.ID = core.NewID()
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	return out
}

// validPositionOffset reports whether v is a finite, non-negative number of seconds.
func validSeconds(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1e9
}
