// Package subtitles converts sidecar subtitle files (SubRip .srt, SubStation
// .ass/.ssa) to WebVTT and validates/normalises existing .vtt files, so the
// browser's native <track> support can display them.
//
// Input is untrusted: size is capped, text is decoded leniently (UTF-8 with
// BOM handling, UTF-16 with BOM, otherwise Windows-1252 when the bytes are not
// valid UTF-8), and cue text is re-escaped so only the simple <b>/<i>/<u>
// markup survives. The output is always well-formed WebVTT.
package subtitles

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxInputBytes caps subtitle input. Feature-length subtitle files are
// ~100 KiB; heavily typeset ASS files reach a few MiB.
const MaxInputBytes = 16 << 20

// maxCues bounds output size for pathological inputs.
const maxCues = 200_000

var (
	// ErrTooLarge is returned when input exceeds MaxInputBytes.
	ErrTooLarge = errors.New("subtitles: input too large")
	// ErrInvalid is returned for input that is not the declared format.
	ErrInvalid = errors.New("subtitles: invalid subtitle data")
	// ErrUnsupported is returned by Convert for unknown formats.
	ErrUnsupported = errors.New("subtitles: unsupported format")
)

// Formats accepted by Convert (ffprobe-style codec names are accepted too).
const (
	FormatSRT = "srt"
	FormatVTT = "vtt"
	FormatASS = "ass"
)

// FormatForExt maps a file extension (with or without dot) or a codec name to
// a Convert format, or "" when unsupported.
func FormatForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "srt", "subrip":
		return FormatSRT
	case "vtt", "webvtt":
		return FormatVTT
	case "ass", "ssa":
		return FormatASS
	}
	return ""
}

// Convert writes WebVTT converted from r in the given format (see
// FormatForExt).
func Convert(format string, r io.Reader, w io.Writer) error {
	switch FormatForExt(format) {
	case FormatSRT:
		return SRTToVTT(r, w)
	case FormatASS:
		return ASSToVTT(r, w)
	case FormatVTT:
		return NormalizeVTT(r, w)
	}
	return fmt.Errorf("%w: %q", ErrUnsupported, format)
}

// cue is one timed caption; times in milliseconds.
type cue struct {
	start, end int64
	id         string
	settings   string
	lines      []string // already WebVTT-safe
}

// readText reads and decodes a subtitle file into normalised UTF-8 lines.
func readText(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("subtitles: read: %w", err)
	}
	if len(data) > MaxInputBytes {
		return nil, ErrTooLarge
	}
	s := decode(data)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.Split(s, "\n"), nil
}

func decode(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		b = b[3:]
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		return decodeUTF16(b[2:], false)
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		return decodeUTF16(b[2:], true)
	case len(b) >= 4 && b[0] != 0 && b[1] == 0 && b[2] != 0 && b[3] == 0:
		return decodeUTF16(b, false) // BOM-less UTF-16LE (ASCII-range start)
	case len(b) >= 4 && b[0] == 0 && b[1] != 0 && b[2] == 0 && b[3] != 0:
		return decodeUTF16(b, true)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return decodeWindows1252(b)
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return string(utf16.Decode(u))
}

var win1252 = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

func decodeWindows1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/4)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xA0:
			sb.WriteRune(win1252[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// parseTimestamp parses [h:]mm:ss[.,]fff where the fraction is decimal
// (".5" = 500 ms, ".50" = 500 ms, ".05" = 50 ms) so SRT, ASS centiseconds
// and VTT share one parser.
func parseTimestamp(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	frac := ""
	if i := strings.LastIndexAny(s, ".,"); i >= 0 {
		frac = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var h, m, sec int64
	var err error
	hasHours := len(parts) == 3
	if hasHours {
		if h, err = parseUint(parts[0], 6); err != nil {
			return 0, false
		}
		parts = parts[1:]
	}
	if m, err = parseUint(parts[0], 2); err != nil || m > 59 && hasHours {
		return 0, false
	}
	if sec, err = parseUint(parts[1], 2); err != nil || sec > 59 {
		return 0, false
	}
	var ms int64
	if frac != "" {
		if len(frac) > 3 {
			frac = frac[:3] // more precision than we keep
		}
		f, err := parseUint(frac, 3)
		if err != nil {
			return 0, false
		}
		for i := len(frac); i < 3; i++ {
			f *= 10
		}
		ms = f
	}
	return ((h*60+m)*60+sec)*1000 + ms, true
}

func parseUint(s string, maxLen int) (int64, error) {
	if s == "" || len(s) > maxLen {
		return 0, strconv.ErrSyntax
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func formatTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	m := ms / 60_000 % 60
	s := ms / 1000 % 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms%1000)
}

// writeVTT emits the header and cues (sorted by start; invalid ones dropped).
func writeVTT(w io.Writer, cues []cue) error {
	slices.SortStableFunc(cues, func(a, b cue) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	bw := bufio.NewWriter(w)
	bw.WriteString("WEBVTT\n")
	for _, c := range cues {
		if c.end <= c.start || len(c.lines) == 0 {
			continue
		}
		bw.WriteByte('\n')
		if c.id != "" {
			bw.WriteString(c.id)
			bw.WriteByte('\n')
		}
		bw.WriteString(formatTimestamp(c.start))
		bw.WriteString(" --> ")
		bw.WriteString(formatTimestamp(c.end))
		if c.settings != "" {
			bw.WriteByte(' ')
			bw.WriteString(c.settings)
		}
		bw.WriteByte('\n')
		for _, l := range c.lines {
			bw.WriteString(l)
			bw.WriteByte('\n')
		}
	}
	return bw.Flush()
}

// ---------------------------------------------------------------------------
// Cue text sanitising shared by SRT and ASS.

// textBuilder accumulates WebVTT cue text, escaping everything except a
// balanced set of <b>, <i>, <u> tags.
type textBuilder struct {
	sb    strings.Builder
	stack []byte // open tags: 'b', 'i', 'u'
}

func (t *textBuilder) text(s string) {
	for _, r := range s {
		switch r {
		case '&':
			t.sb.WriteString("&amp;")
		case '<':
			t.sb.WriteString("&lt;")
		case '>':
			t.sb.WriteString("&gt;")
		default:
			t.sb.WriteRune(r)
		}
	}
}

func (t *textBuilder) open(tag byte) {
	if slices.Contains(t.stack, tag) {
		return
	}
	t.stack = append(t.stack, tag)
	t.sb.WriteString("<" + string(tag) + ">")
}

// close closes tag, closing and re-opening any tags opened after it so the
// output stays properly nested.
func (t *textBuilder) close(tag byte) {
	i := slices.Index(t.stack, tag)
	if i < 0 {
		return
	}
	reopen := slices.Clone(t.stack[i+1:])
	for j := len(t.stack) - 1; j >= i; j-- {
		t.sb.WriteString("</" + string(t.stack[j]) + ">")
	}
	t.stack = t.stack[:i]
	for _, r := range reopen {
		t.open(r)
	}
}

func (t *textBuilder) closeAll() {
	for j := len(t.stack) - 1; j >= 0; j-- {
		t.sb.WriteString("</" + string(t.stack[j]) + ">")
	}
	t.stack = t.stack[:0]
}

// lines finishes the cue text and splits it into non-empty lines (a blank
// line would terminate a WebVTT cue).
func (t *textBuilder) lines() []string {
	t.closeAll()
	var out []string
	carry := "" // tags from text-free lines, moved onto the next line to keep nesting intact
	for _, l := range strings.Split(t.sb.String(), "\n") {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(stripTags(l)) == "" && !strings.Contains(l, " ") {
			carry += strings.TrimSpace(l)
			continue
		}
		out = append(out, carry+l)
		carry = ""
	}
	if carry != "" && len(out) > 0 {
		out[len(out)-1] += carry
	}
	return out
}

var reTag = regexp.MustCompile(`</?[biu]>`)

func stripTags(s string) string { return reTag.ReplaceAllString(s, "") }

// ---------------------------------------------------------------------------
// SubRip

var reSRTTiming = regexp.MustCompile(`^\s*([0-9:.,]+)\s*-->\s*([0-9:.,]+)`)

// reHTMLTag matches the HTML-ish tags SubRip files use (<i>, </b>,
// <font color=…>). It is strict ("<" directly followed by a name) so text
// like "a < b > c" is escaped rather than swallowed.
var reHTMLTag = regexp.MustCompile(`(?i)^<(/?)([a-z]+)(?:\s[^<>]*)?>`)

// SRTToVTT converts SubRip to WebVTT. Cue numbers are dropped, ',' decimal
// separators become '.', <b>/<i>/<u> are kept (balanced), <font> and other
// tags and ASS-style "{\an8}" overrides are removed.
func SRTToVTT(r io.Reader, w io.Writer) error {
	lines, err := readText(r)
	if err != nil {
		return err
	}
	var cues []cue
	var cur *cue
	var payload []string
	flush := func(beforeTiming bool) {
		if cur == nil {
			return
		}
		// Drop trailing blank lines and, when another cue follows, its index
		// number (a lone number right before a timing line, or after a blank).
		for len(payload) > 0 && strings.TrimSpace(payload[len(payload)-1]) == "" {
			payload = payload[:len(payload)-1]
		}
		if n := len(payload); n > 0 && beforeTiming && isAllDigits(strings.TrimSpace(payload[n-1])) && (n == 1 || strings.TrimSpace(payload[n-2]) == "") {
			payload = payload[:n-1]
		}
		var tb textBuilder
		for i, l := range payload {
			if i > 0 {
				tb.sb.WriteByte('\n')
			}
			srtLine(&tb, l)
		}
		cur.lines = tb.lines()
		cues = append(cues, *cur)
		cur, payload = nil, nil
	}
	for _, l := range lines {
		if m := reSRTTiming.FindStringSubmatch(l); m != nil {
			start, ok1 := parseTimestamp(m[1])
			end, ok2 := parseTimestamp(m[2])
			if ok1 && ok2 {
				flush(true)
				if len(cues) >= maxCues {
					break
				}
				cur = &cue{start: start, end: end}
				continue
			}
		}
		if cur != nil {
			payload = append(payload, l)
		}
	}
	flush(false)
	if len(cues) == 0 && !onlyBlank(lines) {
		return fmt.Errorf("%w: no SubRip cues found", ErrInvalid)
	}
	return writeVTT(w, cues)
}

func srtLine(tb *textBuilder, l string) {
	for len(l) > 0 {
		switch l[0] {
		case '{':
			if end := strings.IndexByte(l, '}'); end > 0 && strings.HasPrefix(l, "{\\") {
				l = l[end+1:]
				continue
			}
		case '<':
			if m := reHTMLTag.FindStringSubmatch(l); m != nil {
				name := strings.ToLower(m[2])
				if name == "b" || name == "i" || name == "u" {
					if m[1] == "/" {
						tb.close(name[0])
					} else {
						tb.open(name[0])
					}
				}
				l = l[len(m[0]):]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(l)
		tb.text(l[:size])
		l = l[size:]
	}
}

func isAllDigits(s string) bool {
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

func onlyBlank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// SubStation Alpha / Advanced SubStation

// ASSToVTT converts ASS/SSA "Dialogue" events to WebVTT cues. Override
// blocks are removed except \i, \b, \u (mapped to tags) and \p (drawing
// commands are skipped); \N becomes a line break, \h a no-break space.
// Positioning, styles and karaoke are not carried over.
func ASSToVTT(r io.Reader, w io.Writer) error {
	lines, err := readText(r)
	if err != nil {
		return err
	}
	section := ""
	sawEvents := false
	// Default v4+ event format, used when the file has no Format line.
	format := []string{"layer", "start", "end", "style", "name", "marginl", "marginr", "marginv", "effect", "text"}
	var cues []cue
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.ToLower(t)
			if section == "[events]" {
				sawEvents = true
			}
			continue
		}
		if section != "[events]" {
			continue
		}
		key, val, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "format":
			var f []string
			for _, p := range strings.Split(val, ",") {
				f = append(f, strings.ToLower(strings.TrimSpace(p)))
			}
			if slices.Contains(f, "text") && slices.Contains(f, "start") && slices.Contains(f, "end") {
				format = f
			}
		case "dialogue":
			if len(cues) >= maxCues {
				continue
			}
			if c, ok := assDialogue(val, format); ok {
				cues = append(cues, c)
			}
		}
	}
	if !sawEvents {
		return fmt.Errorf("%w: no [Events] section", ErrInvalid)
	}
	return writeVTT(w, dedupe(cues))
}

func assDialogue(val string, format []string) (cue, bool) {
	fields := strings.SplitN(strings.TrimLeft(val, " "), ",", len(format))
	if len(fields) != len(format) {
		return cue{}, false
	}
	get := func(name string) string {
		if i := slices.Index(format, name); i >= 0 {
			return fields[i]
		}
		return ""
	}
	start, ok1 := parseTimestamp(get("start"))
	end, ok2 := parseTimestamp(get("end"))
	if !ok1 || !ok2 {
		return cue{}, false
	}
	var tb textBuilder
	assText(&tb, get("text"))
	lines := tb.lines()
	if len(lines) == 0 {
		return cue{}, false
	}
	return cue{start: start, end: end, lines: lines}, true
}

func assText(tb *textBuilder, s string) {
	drawing := false
	for len(s) > 0 {
		if s[0] == '{' {
			end := strings.IndexByte(s, '}')
			if end < 0 { // unterminated block: the rest of the line is overrides
				assOverrides(tb, s[1:], drawing)
				return
			}
			drawing = assOverrides(tb, s[1:end], drawing)
			s = s[end+1:]
			continue
		}
		if s[0] == '\\' && len(s) > 1 {
			switch s[1] {
			case 'N':
				if !drawing {
					tb.sb.WriteByte('\n')
				}
				s = s[2:]
				continue
			case 'n':
				if !drawing {
					tb.sb.WriteByte(' ')
				}
				s = s[2:]
				continue
			case 'h':
				if !drawing {
					tb.sb.WriteString(" ")
				}
				s = s[2:]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s)
		if !drawing {
			tb.text(s[:size])
		}
		s = s[size:]
	}
}

// assOverrides applies the tags inside one {...} block and returns the new
// drawing-mode state.
func assOverrides(tb *textBuilder, block string, drawing bool) bool {
	for _, tag := range strings.Split(block, `\`)[1:] {
		tag = strings.TrimSpace(tag)
		switch {
		case strings.HasPrefix(tag, "r"): // \r or \rStyle: reset to style defaults
			tb.closeAll()
		case len(tag) >= 2 && (tag[0] == 'i' || tag[0] == 'b' || tag[0] == 'u') && isAllDigits(tag[1:]):
			n, _ := strconv.Atoi(tag[1:])
			// \b accepts weights (\b700); anything ≥ 1 other than 0 is on.
			if n != 0 {
				tb.open(tag[0])
			} else {
				tb.close(tag[0])
			}
		case len(tag) >= 2 && tag[0] == 'p' && isAllDigits(tag[1:]):
			n, _ := strconv.Atoi(tag[1:])
			drawing = n > 0
		}
	}
	return drawing
}

// dedupe removes identical consecutive cues (layered ASS effects often
// repeat the same line).
func dedupe(cues []cue) []cue {
	out := cues[:0]
	for _, c := range cues {
		if n := len(out); n > 0 && out[n-1].start == c.start && out[n-1].end == c.end && slices.Equal(out[n-1].lines, c.lines) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// WebVTT

var reVTTSetting = regexp.MustCompile(`^(?:vertical|line|position|size|align|region):[A-Za-z0-9%.,:_-]{1,32}$`)

// NormalizeVTT validates WebVTT and re-emits it with normalised line endings
// and timestamps. Cues with unparsable timings are dropped, NOTE and STYLE
// blocks are removed (STYLE could pull remote resources via CSS), unknown cue
// settings are discarded, and "-->" in payloads is escaped. Cue payload
// markup is otherwise preserved.
func NormalizeVTT(r io.Reader, w io.Writer) error {
	lines, err := readText(r)
	if err != nil {
		return err
	}
	if len(lines) == 0 || !isVTTHeader(lines[0]) {
		return fmt.Errorf("%w: missing WEBVTT header", ErrInvalid)
	}
	var cues []cue
	var block []string
	flush := func() {
		if len(block) > 0 && len(cues) < maxCues {
			if c, ok := vttCue(block); ok {
				cues = append(cues, c)
			}
		}
		block = nil
	}
	// Skip the header block (header line plus optional metadata lines).
	i := 1
	for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
		i++
	}
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			flush()
			continue
		}
		block = append(block, lines[i])
	}
	flush()
	return writeVTT(w, cues)
}

func isVTTHeader(l string) bool {
	return l == "WEBVTT" || strings.HasPrefix(l, "WEBVTT ") || strings.HasPrefix(l, "WEBVTT\t")
}

func vttCue(block []string) (cue, bool) {
	first := strings.TrimSpace(block[0])
	if strings.HasPrefix(first, "NOTE") || first == "STYLE" || first == "REGION" {
		return cue{}, false
	}
	var c cue
	timing := 0
	if !strings.Contains(block[0], "-->") {
		if len(block) < 2 || !strings.Contains(block[1], "-->") {
			return cue{}, false
		}
		c.id = sanitizeID(block[0])
		timing = 1
	}
	startStr, rest, _ := strings.Cut(block[timing], "-->")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return cue{}, false
	}
	var ok1, ok2 bool
	c.start, ok1 = parseTimestamp(startStr)
	c.end, ok2 = parseTimestamp(fields[0])
	if !ok1 || !ok2 {
		return cue{}, false
	}
	var settings []string
	for _, s := range fields[1:] {
		if reVTTSetting.MatchString(s) {
			settings = append(settings, s)
		}
	}
	c.settings = strings.Join(settings, " ")
	for _, l := range block[timing+1:] {
		c.lines = append(c.lines, strings.ReplaceAll(l, "-->", "--&gt;"))
	}
	return c, len(c.lines) > 0
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "-->", ""))
	if len(s) > 256 {
		s = s[:256]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
