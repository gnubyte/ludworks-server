package notes

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits shared by the API, import and mail-in.
const (
	MaxBodyBytes      = 5 << 20 // per note body
	MaxTitleRunes     = 500     // explicit titles
	MaxDerivedTitle   = 200     // title derived from the first line
	MaxSnippetRunes   = 200     // NoteSummary.snippet
	MaxTags           = 100
	MaxTagRunes       = 64
	MaxFolderNameRune = 200
	MaxFolderDepth    = 32
	MaxFoldersPerUser = 5000
)

// derived is what the server computes from a note body.
type derived struct {
	FirstLine string   // plain text of the first non-empty line (≤ MaxDerivedTitle runes)
	Rest      string   // plain text after the first non-empty line, whitespace collapsed
	Lines     []string // every plain-text line (for full-text search)
	HasCode   bool     // body contains a fenced code block
	titleLine int      // index in Lines of the title line (valid when FirstLine != "")
}

var (
	reHeading     = regexp.MustCompile(`^#{1,6}(?:[ \t]+|$)`)
	reClosingHash = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	reSetext      = regexp.MustCompile(`^(?:=+|-+)[ \t]*$`)
	reHR          = regexp.MustCompile(`^(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	reListMarker  = regexp.MustCompile(`^(?:[-*+]|\d{1,9}[.)])(?:[ \t]+|$)`)
	reTaskBox     = regexp.MustCompile(`^\[[ xX]\](?:[ \t]+|$)`)
	reTableSep    = regexp.MustCompile(`^\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$`)
	reLinkRefDef  = regexp.MustCompile(`^\[[^\]]+\]:[ \t]*\S`)
	reImage       = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	reLink        = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reRefLink     = regexp.MustCompile(`\[([^\]]+)\]\[[^\]]*\]`)
	reAutolink    = regexp.MustCompile(`<((?:https?|mailto|ftp):[^>\s]*)>`)
	reFence       = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
)

// analyzeBody derives the plain-text lines, first line and has_code flag from Markdown.
// It is a pragmatic stripper (not a full CommonMark parser): good enough for titles,
// snippets and the search index, and linear in the input size.
func analyzeBody(body string) derived {
	var d derived
	var fenceChar byte
	fenceLen := 0
	lines := strings.Split(body, "\n")
	d.Lines = make([]string, 0, len(lines))
	src := make([]string, 0, len(lines)) // container-stripped Markdown per line ("" for code)
	for _, raw := range lines {
		raw = strings.TrimSuffix(raw, "\r")
		if fenceLen > 0 {
			t := strings.TrimLeft(stripContainers(raw), " \t")
			if isClosingFence(t, fenceChar, fenceLen) {
				fenceLen = 0
				continue
			}
			d.Lines = append(d.Lines, cleanText(raw))
			src = append(src, "")
			continue
		}
		line := stripContainers(raw)
		if m := reFence.FindStringSubmatch(strings.TrimLeft(line, " \t")); m != nil && (m[1][0] != '`' || !strings.Contains(m[2], "`")) {
			fenceChar, fenceLen = m[1][0], len(m[1])
			d.HasCode = true
			continue
		}
		d.Lines = append(d.Lines, plainLine(line))
		src = append(src, line)
	}
	// The title is the first line with text; images (their alt text) never make a title.
	for i, l := range d.Lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		t := l
		if src[i] != "" && strings.Contains(src[i], "![") {
			t = plainLine(reImage.ReplaceAllString(src[i], ""))
		}
		if t = collapseSpace(t); t == "" {
			continue
		}
		d.FirstLine = truncateRunes(t, MaxDerivedTitle, false)
		d.titleLine = i
		d.Rest = collapseSpace(strings.Join(d.Lines[i+1:], " "))
		break
	}
	return d
}

// plainText returns the searchable plain text of a body (lines joined with newlines).
func (d derived) plainText() string {
	var b strings.Builder
	for _, l := range d.Lines {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}

// restText is plainText without the derived title. Whatever else was on the title line
// (image alt text, the remainder of an over-long line) stays searchable.
func (d derived) restText() string {
	if d.FirstLine == "" {
		return d.plainText()
	}
	out := make([]string, 0, len(d.Lines))
	for i, l := range d.Lines {
		if i == d.titleLine {
			if extra := strings.TrimSpace(strings.Replace(collapseSpace(l), d.FirstLine, "", 1)); extra != "" {
				out = append(out, extra)
			}
			continue
		}
		out = append(out, l)
	}
	return derived{Lines: out}.plainText()
}

func isClosingFence(t string, ch byte, n int) bool {
	t = strings.TrimRight(t, " \t")
	if len(t) < n {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != ch {
			return false
		}
	}
	return true
}

// stripContainers removes leading indentation, block quote markers, list markers and task
// boxes (repeatedly, for nested containers).
func stripContainers(s string) string {
	for i := 0; i < 16; i++ {
		t := strings.TrimLeft(s, " \t")
		switch {
		case strings.HasPrefix(t, ">"):
			s = strings.TrimPrefix(t[1:], " ")
		case reListMarker.MatchString(t) && !reHR.MatchString(t):
			s = t[len(reListMarker.FindString(t)):]
			if m := reTaskBox.FindString(s); m != "" {
				s = s[len(m):]
			}
		default:
			return t
		}
	}
	return strings.TrimLeft(s, " \t")
}

// plainLine converts one (non-code) Markdown line to plain text.
func plainLine(s string) string {
	switch {
	case s == "":
		return ""
	case reHR.MatchString(s), reSetext.MatchString(s), reTableSep.MatchString(s), reLinkRefDef.MatchString(s):
		return ""
	}
	if m := reHeading.FindString(s); m != "" {
		s = reClosingHash.ReplaceAllString(s[len(m):], "")
	}
	if strings.HasPrefix(s, "|") || strings.Count(s, "|") >= 2 {
		s = splitTableRow(s)
	}
	s = reImage.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = reRefLink.ReplaceAllString(s, "$1")
	s = reAutolink.ReplaceAllString(s, "$1")
	s = stripInlineMarks(s)
	if strings.ContainsRune(s, '&') {
		s = html.UnescapeString(s)
	}
	return cleanText(s)
}

// splitTableRow turns "| a | b |" into "a b" (escaped pipes are kept).
func splitTableRow(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '|' {
			b.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isASCIIPunct(r rune) bool {
	return r < 128 && unicode.IsPunct(r) || r == '`' || r == '^' || r == '|' || r == '~' || r == '<' || r == '>' || r == '=' || r == '+' || r == '$'
}

// stripInlineMarks removes emphasis/strike/code delimiters and backslash escapes while
// keeping literal characters that are not delimiters (2 * 3, snake_case).
func stripInlineMarks(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	isSpace := func(i int) bool { return i < 0 || i >= len(rs) || unicode.IsSpace(rs[i]) }
	isWord := func(i int) bool {
		return i >= 0 && i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i]))
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\\':
			if i+1 < len(rs) && isASCIIPunct(rs[i+1]) {
				b.WriteRune(rs[i+1])
				i++
				continue
			}
			b.WriteRune(r)
		case '*', '_', '~', '`':
			j := i
			for j+1 < len(rs) && rs[j+1] == r {
				j++
			}
			run := j - i + 1
			drop := false
			switch r {
			case '*':
				drop = !(isSpace(i-1) && isSpace(j+1))
			case '_':
				drop = !(isWord(i-1) && isWord(j+1)) && !(isSpace(i-1) && isSpace(j+1))
			case '~':
				drop = run >= 2
			case '`':
				drop = true
			}
			if !drop {
				b.WriteString(string(rs[i : j+1]))
			}
			i = j
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanText replaces control characters (including the FTS highlight markers) with spaces.
func cleanText(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) && utf8.ValidString(s) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// collapseSpace trims and collapses runs of whitespace into single spaces.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes cuts s to at most n runes. With ellipsis, the cut prefers a word boundary
// and ends with "…" (counted in n).
func truncateRunes(s string, n int, ellipsis bool) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	if !ellipsis {
		return strings.TrimRightFunc(string(rs[:n]), unicode.IsSpace)
	}
	cut := rs[:n-1]
	if i := lastSpace(cut); i > (n*6)/10 {
		cut = cut[:i]
	}
	return strings.TrimRightFunc(string(cut), unicode.IsSpace) + "…"
}

func lastSpace(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if unicode.IsSpace(rs[i]) {
			return i
		}
	}
	return -1
}

// noteText computes the stored title, sort key, snippet and search text for a note.
type noteText struct {
	Title     string
	TitleSort string
	Snippet   string
	Search    string
	HasCode   bool
}

func computeText(explicitTitle *string, body string) noteText {
	d := analyzeBody(body)
	var t noteText
	t.HasCode = d.HasCode
	if explicitTitle != nil && *explicitTitle != "" {
		t.Title = *explicitTitle
		t.Snippet = truncateRunes(collapseSpace(strings.Join(d.Lines, " ")), MaxSnippetRunes, true)
		t.Search = d.plainText()
	} else {
		// The first line is the title (indexed in its own column); index the rest as text so
		// search snippets show the body below the title, like the list does.
		t.Title = d.FirstLine
		t.Snippet = truncateRunes(d.Rest, MaxSnippetRunes, true)
		t.Search = d.restText()
	}
	t.TitleSort = sortKey(t.Title)
	return t
}

// sortKey is the case-insensitive collation key for titles and names.
func sortKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normalizeTitle validates an explicit title: trimmed, no control characters, bounded.
func normalizeTitle(s string) (string, string) {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	if strings.ContainsFunc(s, unicode.IsControl) {
		return "", "must not contain control characters"
	}
	if utf8.RuneCountInString(s) > MaxTitleRunes {
		return "", "must be at most 500 characters"
	}
	return s, ""
}

// normalizeTag canonicalises a tag: trimmed, leading '#' removed, lower case, inner
// whitespace collapsed. It returns "" plus a problem description when invalid.
func normalizeTag(s string) (string, string) {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	s = strings.TrimLeft(s, "#")
	s = strings.ToLower(collapseSpace(s))
	switch {
	case s == "":
		return "", "tags must not be empty"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "", "tags must not contain control characters"
	case strings.ContainsAny(s, ",[]\"'"):
		return "", `tags must not contain , [ ] " or '`
	case utf8.RuneCountInString(s) > MaxTagRunes:
		return "", "tags must be at most 64 characters"
	}
	return s, ""
}

// normalizeTags canonicalises and de-duplicates a tag list (order preserved).
func normalizeTags(in []string) ([]string, string) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		t, problem := normalizeTag(raw)
		if problem != "" {
			return nil, problem
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > MaxTags {
		return nil, "at most 100 tags"
	}
	return out, ""
}

// normalizeFolderName validates a folder name.
func normalizeFolderName(s string) (string, string) {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	switch {
	case s == "":
		return "", "is required"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "", "must not contain control characters"
	case utf8.RuneCountInString(s) > MaxFolderNameRune:
		return "", "must be at most 200 characters"
	}
	return s, ""
}

// parseMarked strips the FTS5 highlight markers (\x01 … \x02) from s and returns the plain
// text plus the highlighted ranges as [start, end) offsets in Unicode code points.
func parseMarked(s string) (string, [][2]int) {
	var b strings.Builder
	var ranges [][2]int
	pos, start := 0, -1
	for _, r := range s {
		switch r {
		case '\x01':
			start = pos
		case '\x02':
			if start >= 0 && pos > start {
				ranges = append(ranges, [2]int{start, pos})
			}
			start = -1
		default:
			b.WriteRune(r)
			pos++
		}
	}
	return b.String(), ranges
}

// clipMarked collapses whitespace and truncates a marked-up snippet to max runes while
// keeping the highlight ranges consistent.
func clipMarked(text string, ranges [][2]int, max int) (string, [][2]int) {
	rs := []rune(text)
	// Collapse whitespace, remapping offsets.
	out := make([]rune, 0, len(rs))
	remap := make([]int, len(rs)+1)
	space := true
	for i, r := range rs {
		remap[i] = len(out)
		if unicode.IsSpace(r) {
			if !space {
				out = append(out, ' ')
			}
			space = true
			continue
		}
		space = false
		out = append(out, r)
	}
	remap[len(rs)] = len(out)
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	var rr [][2]int
	for _, g := range ranges {
		s, e := remap[g[0]], remap[g[1]]
		if e > len(out) {
			e = len(out)
		}
		if s < e {
			rr = append(rr, [2]int{s, e})
		}
	}
	if len(out) > max {
		out = append(out[:max-1], '…')
		kept := rr[:0]
		for _, g := range rr {
			if g[0] >= max-1 {
				continue
			}
			if g[1] > max-1 {
				g[1] = max - 1
			}
			kept = append(kept, g)
		}
		rr = kept
	}
	if rr == nil {
		rr = [][2]int{}
	}
	return string(out), rr
}
