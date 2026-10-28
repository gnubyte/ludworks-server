package tags

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LRC lyrics: "[mm:ss.xx]text" lines (several stamps per line allowed), "[ar:…]"-style
// metadata lines, an "[offset:±ms]" adjustment and enhanced-LRC word stamps ("<mm:ss.xx>",
// removed). Text without any time stamp is unsynced lyrics.

const (
	maxLyricLines    = 10_000
	maxLyricLineRune = 1000
	// MaxLRCBytes is the largest LRC text that is parsed (the rest is ignored).
	MaxLRCBytes = 1 << 20
)

// ParseLyricsText returns lyrics from embedded text: synced when it has LRC time stamps,
// otherwise one unsynced line per text line. nil when the text is empty.
func ParseLyricsText(text string) *Lyrics { return ParseLRC(text) }

// ParseLRC parses LRC (or plain) lyrics text. nil when there are no lines.
func ParseLRC(text string) *Lyrics {
	if len(text) > MaxLRCBytes {
		text = text[:MaxLRCBytes]
	}
	text = strings.ToValidUTF8(strings.TrimPrefix(text, "\xef\xbb\xbf"), "")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var (
		timed  []LyricLine
		plain  []LyricLine
		offset float64
	)
	for _, raw := range strings.Split(text, "\n") {
		if len(timed) >= maxLyricLines || len(plain) >= maxLyricLines {
			break
		}
		rest := strings.TrimSpace(raw)
		var stamps []float64
		meta := false
		for strings.HasPrefix(rest, "[") {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				break
			}
			tag := rest[1:end]
			if t, ok := parseStamp(tag); ok {
				stamps = append(stamps, t)
			} else if key, val, ok := strings.Cut(tag, ":"); ok && isMetaKey(key) {
				meta = true
				if strings.EqualFold(strings.TrimSpace(key), "offset") {
					if ms, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && ms > -1e7 && ms < 1e7 {
						offset = float64(ms) / 1000
					}
				}
			} else {
				break
			}
			rest = strings.TrimSpace(rest[end+1:])
		}
		switch {
		case len(stamps) > 0:
			line := cleanLine(stripWordStamps(rest))
			for _, t := range stamps {
				if len(timed) < maxLyricLines {
					timed = append(timed, LyricLine{Time: t, Text: line})
				}
			}
		case meta && rest == "":
			// metadata line
		default:
			line := cleanLine(strings.TrimSpace(raw))
			if line == "" && (len(plain) == 0 || plain[len(plain)-1].Text == "") {
				continue // leading or repeated blank lines
			}
			plain = append(plain, LyricLine{Time: -1, Text: line})
		}
	}
	if len(timed) > 0 {
		for i := range timed {
			timed[i].Time = max(0, timed[i].Time-offset)
		}
		sortLines(timed)
		return &Lyrics{Synced: true, Lines: timed}
	}
	for len(plain) > 0 && plain[len(plain)-1].Text == "" {
		plain = plain[:len(plain)-1]
	}
	if len(plain) == 0 {
		return nil
	}
	return &Lyrics{Lines: plain}
}

func isMetaKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "ar", "ti", "al", "au", "by", "re", "ve", "length", "offset", "#", "tool", "lang", "la":
		return true
	}
	return false
}

// parseStamp parses "mm:ss", "mm:ss.x", "mm:ss.xx", "mm:ss.xxx" or "mm:ss:xx" into seconds.
func parseStamp(s string) (float64, bool) {
	m, rest, ok := strings.Cut(s, ":")
	if !ok || len(m) == 0 || len(m) > 3 || !isDigits(m) {
		return 0, false
	}
	sec, frac := rest, ""
	if i := strings.IndexAny(rest, ".:"); i >= 0 {
		sec, frac = rest[:i], rest[i+1:]
		if frac == "" || len(frac) > 3 || !isDigits(frac) {
			return 0, false
		}
	}
	if len(sec) == 0 || len(sec) > 2 || !isDigits(sec) {
		return 0, false
	}
	mi, _ := strconv.Atoi(m)
	si, _ := strconv.Atoi(sec)
	if si >= 60 {
		return 0, false
	}
	t := float64(mi*60 + si)
	if frac != "" {
		f, _ := strconv.Atoi(frac)
		t += float64(f) / pow10(len(frac))
	}
	return t, true
}

func pow10(n int) float64 {
	v := 1.0
	for range n {
		v *= 10
	}
	return v
}

// stripWordStamps removes enhanced-LRC "<mm:ss.xx>" word time stamps.
func stripWordStamps(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var sb strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			sb.WriteString(s)
			break
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			sb.WriteString(s)
			break
		}
		if _, ok := parseStamp(s[i+1 : i+j]); ok {
			sb.WriteString(s[:i])
		} else {
			sb.WriteString(s[:i+j+1])
		}
		s = s[i+j+1:]
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// cleanLine removes control characters and caps a lyric line.
func cleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == utf8.RuneError || r == 0xFEFF {
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxLyricLineRune)
}

func sortLines(l []LyricLine) {
	sort.SliceStable(l, func(i, j int) bool { return l[i].Time < l[j].Time })
}
