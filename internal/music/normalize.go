package music

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Normalisation: folded keys (case-, whitespace- and diacritic-insensitive) for identity and
// matching, sort keys and the stable album/artist ids derived from them.

// foldTable maps precomposed Latin letters to their base letters (the standard library has no
// Unicode decomposition; combining marks of decomposed input are dropped separately).
var foldTable = func() map[rune]string {
	m := map[rune]string{}
	add := func(to, from string) {
		for _, r := range from {
			m[r] = to
		}
	}
	add("a", "ÀÁÂÃÄÅàáâãäåĀāĂăĄąǍǎǞǟǠǡǺǻȀȁȂȃȦȧẠạẢảẤấẦầẨẩẪẫẬậẮắẰằẲẳẴẵẶặ")
	add("c", "ÇçĆćĈĉĊċČč")
	add("d", "ĎďĐđÐð")
	add("e", "ÈÉÊËèéêëĒēĔĕĖėĘęĚěȄȅȆȇȨȩẸẹẺẻẼẽẾếỀềỂểỄễỆệ")
	add("g", "ĜĝĞğĠġĢģǦǧǴǵ")
	add("h", "ĤĥĦħ")
	add("i", "ÌÍÎÏìíîïĨĩĪīĬĭĮįİıǏǐȈȉȊȋỈỉỊị")
	add("j", "Ĵĵ")
	add("k", "Ķķǩ")
	add("l", "ĹĺĻļĽľĿŀŁł")
	add("n", "ÑñŃńŅņŇňŉǸǹ")
	add("o", "ÒÓÔÕÖØòóôõöøŌōŎŏŐőƠơǑǒǪǫǾǿȌȍȎȏȪȫȬȭȮȯȰȱỌọỎỏỐốỒồỔổỖỗỘộỚớỜờỞởỠỡỢợ")
	add("r", "ŔŕŖŗŘřȐȑȒȓ")
	add("s", "ŚśŜŝŞşŠšȘș")
	add("t", "ŢţŤťŦŧȚț")
	add("u", "ÙÚÛÜùúûüŨũŪūŬŭŮůŰűŲųƯưǓǔǕǖǗǘǙǚǛǜȔȕȖȗỤụỦủỨứỪừỬửỮữỰự")
	add("w", "Ŵŵ")
	add("y", "ÝýÿŶŷŸȲȳỲỳỴỵỶỷỸỹ")
	add("z", "ŹźŻżŽž")
	add("ss", "ß")
	add("ae", "ÆæǼǽ")
	add("oe", "Œœ")
	add("th", "Þþ")
	add("'", "‘’‚‛′`´")
	add(`"`, "“”„‟″")
	add("-", "‐‑‒–—―")
	return m
}()

// fold lowercases s, removes diacritics and collapses whitespace: the identity and matching
// key of a name ("  Björk " and "bjork" fold alike).
func fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if rep, ok := foldTable[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// sortKey is the sort key of a value without an explicit sort tag: folded, without leading
// punctuation and without a leading "The ", "A " or "An ".
func sortKey(s string) string {
	k := fold(s)
	if t := strings.TrimLeftFunc(k, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }); t != "" {
		k = t
	}
	for _, art := range []string{"the ", "a ", "an "} {
		if rest, ok := strings.CutPrefix(k, art); ok && rest != "" {
			return rest
		}
	}
	return k
}

// explicitSortKey is the sort key of an explicit sort tag (TSOP, ALBUMSORT, …): folded only.
func explicitSortKey(tag string) string { return fold(tag) }

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// entityID derives a stable 26-character lowercase base32 id from a kind and folded parts.
func entityID(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte("ludworks-music-" + kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return strings.ToLower(idEncoding.EncodeToString(h.Sum(nil)[:16]))
}

// albumIDFor is the album id of (album artist, album title); "" without a title.
func albumIDFor(albumArtist, album string) string {
	if fold(album) == "" {
		return ""
	}
	return entityID("album", fold(albumArtist), fold(album))
}

// artistIDFor is the artist id of a name; "" for an empty name.
func artistIDFor(name string) string {
	k := fold(name)
	if k == "" {
		return ""
	}
	return entityID("artist", k)
}

// words splits a folded string into its letter/number runs (for search matching that mirrors
// the FTS5 unicode61 tokenizer).
func words(s string) []string {
	return strings.FieldsFunc(fold(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// matchesTerms reports whether every query term is a word of name (the last term may be a
// prefix), like the FTS5 queries built by db.FTS5Query.
func matchesTerms(name string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	ws := words(name)
	for i, t := range terms {
		found := false
		for _, w := range ws {
			if w == t || (i == len(terms)-1 && strings.HasPrefix(w, t)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// queryTerms folds a search query into terms.
func queryTerms(q string) []string {
	t := words(q)
	if len(t) > 32 {
		t = t[:32]
	}
	return t
}

// cleanText trims, removes control characters (newlines and tabs become spaces) and
// truncates s to max runes — for values from files, requests and upstream data.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError {
			return -1
		}
		if unicode.IsControl(r) {
			if r == '\n' || r == '\t' || r == '\r' {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), max)
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return strings.TrimSpace(s[:j])
		}
		i++
	}
	return s
}
