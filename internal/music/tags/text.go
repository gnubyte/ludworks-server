package tags

import (
	"encoding/binary"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Text helpers shared by the parsers: decoding, cleaning and the parsing of numbers, years,
// "n/m" pairs, ReplayGain values and genres.

// latin1 decodes ISO-8859-1.
func latin1(b []byte) string {
	if isASCII(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// utf8OrLatin1 decodes UTF-8, falling back to ISO-8859-1 for invalid input (some taggers
// write Latin-1 into fields declared UTF-8).
func utf8OrLatin1(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return latin1(b)
}

// utf16Decode decodes UTF-16 in the given byte order (an odd trailing byte is dropped).
func utf16Decode(b []byte, bigEndian bool) string {
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		if bigEndian {
			u[i] = binary.BigEndian.Uint16(b[2*i:])
		} else {
			u[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
	}
	return string(utf16.Decode(u))
}

// utf16WithBOM decodes UTF-16 starting with an optional BOM (little-endian without one, as
// written by most tools that omit it).
func utf16WithBOM(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		return utf16Decode(b[2:], false)
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		return utf16Decode(b[2:], true)
	}
	return utf16Decode(b, false)
}

// cleanValue makes a single-line value safe to store: invalid UTF-8 and control characters
// are removed (newlines and tabs become spaces), whitespace is trimmed and the value is cut to
// maxValueRunes.
func cleanValue(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError:
			return -1
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r), r == 0xFEFF:
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	return truncateRunes(s, maxValueRunes)
}

// cleanMultiline keeps newlines (CRLF normalised) but removes other control characters.
func cleanMultiline(s string, maxRunes int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || r == 0xFEFF {
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxRunes)
}

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

// cleanList cleans values, drops empties and (case-insensitive) duplicates and caps the
// count. Genres are additionally split on ";".
func cleanList(in []string, splitSemicolons bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		v = cleanValue(v)
		if v == "" || len(out) >= maxValues {
			return
		}
		k := strings.ToLower(v)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, v)
	}
	for _, v := range in {
		if splitSemicolons {
			for _, part := range strings.Split(v, ";") {
				add(part)
			}
		} else {
			add(v)
		}
	}
	return out
}

// appendValues adds values to a list without exceeding maxValues.
func appendValues(dst []string, vals ...string) []string {
	for _, v := range vals {
		if len(dst) >= maxValues {
			break
		}
		dst = append(dst, v)
	}
	return dst
}

// parseNumberPair parses "3", "3/12", " 03 / 12 " (and "3 of 12"); invalid parts are 0.
func parseNumberPair(s string) (n, total int) {
	s = strings.TrimSpace(s)
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		a, b, _ = strings.Cut(s, " of ")
	}
	return atoiPrefix(a), atoiPrefix(b)
}

// atoiPrefix parses the leading decimal digits of s (after spaces); 0 when there are none or
// the number is implausibly large.
func atoiPrefix(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && end < 9 && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}

// parseYear extracts a year from "2004", "2004-05-12", "2004-05-12T10:00:00", "05/12/2004"
// or "12 May 2004": the first run of exactly four digits between 1000 and 2999.
func parseYear(s string) int {
	for i := 0; i+4 <= len(s); i++ {
		if !isDigits(s[i:i+4]) || (i > 0 && isDigit(s[i-1])) || (i+4 < len(s) && isDigit(s[i+4])) {
			continue
		}
		if y, _ := strconv.Atoi(s[i : i+4]); y >= 1000 && y <= 2999 {
			return y
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// parseGain parses a ReplayGain value such as "-6.54 dB", "+1.2dB" or "0.98765".
func parseGain(s string) *float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "dB"), "db"))
	s = strings.TrimSpace(strings.TrimSuffix(s, " DB"))
	s = strings.ReplaceAll(s, ",", ".")
	if s == "" || len(s) > 32 {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// parseBool reads "1", "true", "yes" (case-insensitive) as true.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// resolveGenre turns an ID3 TCON value into genre names: "(17)" and "17" are ID3v1 genre
// numbers, "(17)(6)" lists several, "(4)Eurodisco" refines 4 to the text after the
// references, "(RX)"/"(CR)" are Remix/Cover and "((" escapes a literal parenthesis.
func resolveGenre(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		if name := genreName(n); name != "" {
			return []string{name}
		}
		return nil
	}
	var refs []string
	rest := v
	for strings.HasPrefix(rest, "(") && !strings.HasPrefix(rest, "((") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			break
		}
		ref := rest[1:end]
		switch {
		case ref == "RX":
			refs = append(refs, "Remix")
		case ref == "CR":
			refs = append(refs, "Cover")
		default:
			n, err := strconv.Atoi(ref)
			if err != nil {
				return []string{v} // not a reference after all
			}
			if name := genreName(n); name != "" {
				refs = append(refs, name)
			}
		}
		rest = rest[end+1:]
	}
	rest = strings.TrimSpace(strings.ReplaceAll(rest, "((", "("))
	if rest != "" {
		return []string{rest}
	}
	return refs
}

// genreName returns the ID3v1 (with Winamp extensions) genre name for n ("" when unknown).
func genreName(n int) string {
	if n < 0 || n >= len(id3v1Genres) {
		return ""
	}
	return id3v1Genres[n]
}

// id3v1Genres are the ID3v1 genres 0–79 plus the Winamp extensions 80–191.
var id3v1Genres = [...]string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge", "Hip-Hop", "Jazz", "Metal",
	"New Age", "Oldies", "Other", "Pop", "R&B", "Rap", "Reggae", "Rock", "Techno", "Industrial",
	"Alternative", "Ska", "Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient", "Trip-Hop", "Vocal", "Jazz+Funk",
	"Fusion", "Trance", "Classical", "Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel", "Noise",
	"Alternative Rock", "Bass", "Soul", "Punk", "Space", "Meditative", "Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic",
	"Darkwave", "Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream", "Southern Rock", "Comedy", "Cult", "Gangsta",
	"Top 40", "Christian Rap", "Pop/Funk", "Jungle", "Native American", "Cabaret", "New Wave", "Psychedelic", "Rave", "Showtunes",
	"Trailer", "Lo-Fi", "Tribal", "Acid Punk", "Acid Jazz", "Polka", "Retro", "Musical", "Rock & Roll", "Hard Rock",
	"Folk", "Folk-Rock", "National Folk", "Swing", "Fast Fusion", "Bebop", "Latin", "Revival", "Celtic", "Bluegrass",
	"Avantgarde", "Gothic Rock", "Progressive Rock", "Psychedelic Rock", "Symphonic Rock", "Slow Rock", "Big Band", "Chorus", "Easy Listening", "Acoustic",
	"Humour", "Speech", "Chanson", "Opera", "Chamber Music", "Sonata", "Symphony", "Booty Bass", "Primus", "Porn Groove",
	"Satire", "Slow Jam", "Club", "Tango", "Samba", "Folklore", "Ballad", "Power Ballad", "Rhythmic Soul", "Freestyle",
	"Duet", "Punk Rock", "Drum Solo", "A Cappella", "Euro-House", "Dance Hall", "Goa", "Drum & Bass", "Club-House", "Hardcore Techno",
	"Terror", "Indie", "BritPop", "Afro-Punk", "Polsk Punk", "Beat", "Christian Gangsta Rap", "Heavy Metal", "Black Metal", "Crossover",
	"Contemporary Christian", "Christian Rock", "Merengue", "Salsa", "Thrash Metal", "Anime", "JPop", "Synthpop", "Abstract", "Art Rock",
	"Baroque", "Bhangra", "Big Beat", "Breakbeat", "Chillout", "Downtempo", "Dub", "EBM", "Eclectic", "Electro",
	"Electroclash", "Emo", "Experimental", "Garage", "Global", "IDM", "Illbient", "Industro-Goth", "Jam Band", "Krautrock",
	"Leftfield", "Lounge", "Math Rock", "New Romantic", "Nu-Breakz", "Post-Punk", "Post-Rock", "Psytrance", "Shoegaze", "Space Rock",
	"Trop Rock", "World Music", "Neoclassical", "Audiobook", "Audio Theatre", "Neue Deutsche Welle", "Podcast", "Indie Rock", "G-Funk", "Dubstep",
	"Garage Rock", "Psybient",
}
