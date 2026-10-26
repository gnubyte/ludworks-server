package naming

import (
	"strconv"
	"strings"
)

// token is one word of a cleaned file or folder name.
type token struct {
	raw     string // as it appeared (case preserved, brackets kept)
	low     string // lower-case, surrounding brackets/parens and edge punctuation removed
	bracket bool   // was wrapped in () or []
}

// tokenize splits a name into words. Scene-style names without spaces use
// dots and underscores as separators; names that contain spaces keep
// meaningful dots ("Mr. Robot") but still split dot-joined runs such as
// "2010.1080p.BluRay". Runs of single letters ("S.H.I.E.L.D") are re-joined
// into one acronym token.
func tokenize(name string) []token {
	spaced := strings.Contains(name, " ")
	var words []string
	if spaced {
		for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '_' || r == '\t' }) {
			words = append(words, splitDots(w)...)
		}
	} else {
		words = strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' || r == ' ' })
	}
	words = joinAcronyms(words)
	out := make([]token, 0, len(words))
	for _, w := range words {
		if w == "" {
			continue
		}
		out = append(out, makeToken(w))
	}
	return out
}

func makeToken(w string) token {
	t := token{raw: w}
	inner := w
	if len(inner) >= 2 && ((inner[0] == '(' && inner[len(inner)-1] == ')') || (inner[0] == '[' && inner[len(inner)-1] == ']') || (inner[0] == '{' && inner[len(inner)-1] == '}')) {
		inner = inner[1 : len(inner)-1]
		t.bracket = true
	}
	inner = strings.TrimRight(strings.Trim(inner, ",;!?"), ".")
	t.low = strings.ToLower(inner)
	return t
}

// splitDots splits a space-delimited word on dots unless the dots are part
// of an abbreviation ("Mr.", "Dr.", "Vol.") or an acronym ("S.H.I.E.L.D.").
func splitDots(w string) []string {
	if !strings.Contains(w, ".") {
		return []string{w}
	}
	core := strings.TrimRight(w, ".")
	if !strings.Contains(core, ".") {
		// Only trailing dot(s): "Mr." or a sentence end — keep the word.
		return []string{w}
	}
	if isAcronym(w) {
		return []string{w}
	}
	// Decimal numbers like "5.1" or "2.0" stay whole only in tag context; a
	// plain split is fine because tags are stripped anyway.
	return strings.Split(w, ".")
}

func isAcronym(w string) bool {
	parts := strings.Split(strings.TrimRight(w, "."), ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if len(p) != 1 || !isLetter(p[0]) {
			return false
		}
	}
	return true
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// joinAcronyms merges runs of ≥2 single-letter words into "A.B.C.".
func joinAcronyms(words []string) []string {
	out := words[:0:0]
	for i := 0; i < len(words); {
		j := i
		for j < len(words) && len(words[j]) == 1 && isLetter(words[j][0]) {
			j++
		}
		if j-i >= 2 {
			var b strings.Builder
			for _, w := range words[i:j] {
				b.WriteString(w)
				b.WriteByte('.')
			}
			out = append(out, b.String())
			i = j
			continue
		}
		out = append(out, words[i])
		i++
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// Plausible release-year range. Fixed (not "now+1") so parsing is a pure
// function of the input.
const (
	minYear = 1888
	maxYear = 2100
)

func yearOf(t token) int {
	if len(t.low) != 4 || !isDigits(t.low) {
		return 0
	}
	y := atoi(t.low)
	if y < minYear || y > maxYear {
		return 0
	}
	return y
}

// tag classification ------------------------------------------------------

type tagClass int

const (
	tagNone   tagClass = iota
	tagWeak            // only meaningful after the title region (PROPER, WEB, EXTENDED…)
	tagStrong          // ends a title wherever it appears (1080p, x264, BluRay…)
)

type tagInfo struct {
	class   tagClass
	quality string
	source  string
}

var tagTable = func() map[string]tagInfo {
	m := map[string]tagInfo{}
	q := func(canon string, keys ...string) {
		for _, k := range keys {
			m[k] = tagInfo{class: tagStrong, quality: canon}
		}
	}
	s := func(class tagClass, canon string, keys ...string) {
		for _, k := range keys {
			m[k] = tagInfo{class: class, source: canon}
		}
	}
	strong := func(keys ...string) {
		for _, k := range keys {
			m[k] = tagInfo{class: tagStrong}
		}
	}
	weak := func(keys ...string) {
		for _, k := range keys {
			m[k] = tagInfo{class: tagWeak}
		}
	}
	q("2160p", "2160p", "4k", "uhd", "4kuhd", "uhd4k")
	q("4320p", "4320p", "8k")
	q("1440p", "1440p")
	q("1080p", "1080p", "fhd")
	q("1080i", "1080i")
	q("720p", "720p")
	q("576p", "576p")
	q("576i", "576i")
	q("480p", "480p")
	q("480i", "480i")
	q("360p", "360p")
	s(tagStrong, "bluray", "bluray", "blueray", "bdrip", "brrip", "bdrmux", "bdremux", "bd25", "bd50", "bdmv")
	s(tagStrong, "remux", "remux")
	s(tagStrong, "web-dl", "webdl")
	s(tagStrong, "webrip", "webrip")
	s(tagWeak, "web", "web")
	s(tagStrong, "hdtv", "hdtv", "pdtv", "sdtv", "dsr", "dsrip", "tvrip", "hdtvrip")
	s(tagStrong, "dvd", "dvdrip", "dvd5", "dvd9", "dvdr", "dvdscr")
	s(tagStrong, "hdrip", "hdrip")
	s(tagStrong, "cam", "hdcam", "camrip")
	s(tagStrong, "telesync", "telesync", "hdts")
	s(tagStrong, "vhs", "vhsrip")
	strong("x264", "x265", "h264", "h265", "hevc", "avc", "xvid", "divx", "av1", "vp9", "10bit", "8bit", "hi10p", "hi10",
		"aac", "aac2", "aac5", "ac3", "eac3", "dts", "dtshd", "dtsx", "dtsma", "truehd", "ddp", "ddp2", "ddp5", "ddp7",
		"dd2", "dd5", "dd7", "dd+", "flac", "opus", "lpcm", "mp3", "hdr", "hdr10", "hdr10+", "hdr10plus", "dovi", "sdr",
		"nf", "amzn", "dsnp", "hmax", "atvp", "pcok", "pmtp", "hulu")
	weak("proper", "repack", "rerip", "real", "internal", "limited", "extended", "unrated", "uncut", "remastered",
		"theatrical", "imax", "3d", "hsbs", "multi", "dubbed", "subbed", "complete", "dv", "atmos", "ma", "hd",
		"v2", "v3", "v4", "dual", "dualaudio", "criterion", "uhdremux", "dc", "hc", "festival", "readnfo", "ws", "fs")
	return m
}()

func normTag(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("-", "", ".", "", "_", "", " ", "").Replace(s)
}

// classify returns the tag info for token i (looking at the next token for
// split forms like "H.264").
func classify(toks []token, i int) tagInfo {
	t := toks[i]
	if t.low == "h" && i+1 < len(toks) && (toks[i+1].low == "264" || toks[i+1].low == "265") {
		return tagInfo{class: tagStrong}
	}
	if ti, ok := tagTable[normTag(t.low)]; ok {
		return ti
	}
	// "x264-GRP", "WEB-DL-GRP": classify the part before the release group.
	if k := strings.LastIndexByte(t.low, '-'); k > 0 {
		if ti, ok := tagTable[normTag(t.low[:k])]; ok {
			return ti
		}
	}
	// Anime CRC32 like "[ABCD1234]" and bracketed resolution "[1920x1080]".
	if t.bracket && (isCRC(t.low) || isResolution(t.low)) {
		return tagInfo{class: tagStrong}
	}
	if isResolution(t.low) {
		return tagInfo{class: tagStrong}
	}
	return tagInfo{}
}

func isCRC(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isResolution(s string) bool {
	w, h, ok := strings.Cut(s, "x")
	return ok && len(w) >= 3 && len(w) <= 4 && len(h) >= 3 && len(h) <= 4 && isDigits(w) && isDigits(h)
}

func isDash(t token) bool {
	switch t.raw {
	case "-", "–", "—", "~", ":", "|", "+", "&-":
		return true
	}
	return false
}

// joinTitle joins tokens into a display title, trimming separator tokens and
// bracketed tag tokens at the edges.
func joinTitle(toks []token) string {
	for len(toks) > 0 && (isDash(toks[0]) || toks[0].low == "") {
		toks = toks[1:]
	}
	for len(toks) > 0 {
		last := toks[len(toks)-1]
		if isDash(last) || last.low == "" || (last.bracket && classify(toks, len(toks)-1).class != tagNone) {
			toks = toks[:len(toks)-1]
			continue
		}
		break
	}
	parts := make([]string, 0, len(toks))
	for i, t := range toks {
		if t.bracket && classify(toks, i).class != tagNone {
			continue
		}
		parts = append(parts, t.raw)
	}
	s := strings.Join(parts, " ")
	return strings.TrimSpace(strings.Trim(s, " -–—:,|~"))
}
