package naming

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxNFOBytes is the largest .nfo file ParseNFO accepts. Real Kodi NFOs are
// a few KiB; anything larger is not worth parsing.
const MaxNFOBytes = 1 << 20

// NFO kinds.
const (
	NFOMovie   = "movie"
	NFOTVShow  = "tvshow"
	NFOEpisode = "episode"
)

var (
	// ErrNFOTooLarge is returned for inputs over MaxNFOBytes.
	ErrNFOTooLarge = errors.New("naming: nfo too large")
	// ErrNotNFO is returned when the input is neither Kodi XML nor a URL
	// line naming an IMDb/TMDB title.
	ErrNotNFO = errors.New("naming: not a recognised nfo")
)

// NFO is the subset of Kodi movie / tvshow / episodedetails metadata
// Ludworks uses.
type NFO struct {
	Kind          string // NFOMovie, NFOTVShow, NFOEpisode, or "" for a URL-only nfo
	Title         string
	OriginalTitle string
	SortTitle     string
	ShowTitle     string
	Year          int // from <year>, else <premiered>/<aired>
	Plot          string
	Genres        []string
	MPAA          string // "Rated " and "US:" style prefixes removed
	// Season and Episode are nil when absent (0 is valid: specials).
	Season, Episode *int
	RuntimeMinutes  int
	Aired           string // YYYY-MM-DD when given
	TMDBID          int64
	IMDBID          string // "tt" + digits
}

// Bounds for hostile input.
const (
	maxNFOTokens  = 200_000
	maxNFODepth   = 64
	maxFieldBytes = 4096
	maxPlotBytes  = 64 << 10
	maxGenres     = 32
)

var (
	reIMDbURL = regexp.MustCompile(`imdb\.com/(?:[a-z]{2}/)?title/(tt\d{5,10})`)
	reTMDBURL = regexp.MustCompile(`themoviedb\.org/(?:movie|tv)/(\d{1,10})`)
	reIMDbID  = regexp.MustCompile(`^tt\d{5,10}$`)
	reDate    = regexp.MustCompile(`^(\d{4})-\d{2}-\d{2}`)
	reLeadNum = regexp.MustCompile(`^\s*(\d{1,5})`)
)

// ParseNFO parses a Kodi .nfo file: <movie>, <tvshow> or <episodedetails>
// XML (only the first root element is used; a trailing scraper URL line is
// tolerated and mined for IDs), or a bare IMDb/TMDB URL. External entities
// are never resolved (encoding/xml does not support them).
func ParseNFO(r io.Reader) (*NFO, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxNFOBytes+1))
	if err != nil {
		return nil, fmt.Errorf("naming: read nfo: %w", err)
	}
	if len(data) > MaxNFOBytes {
		return nil, ErrNFOTooLarge
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	// Legacy NFOs are often Windows-1252 without declaring it.
	converted := false
	if !utf8.Valid(data) {
		data = []byte(decodeWindows1252(data))
		converted = true
	}

	n := &NFO{}
	root := bytes.IndexByte(data, '<')
	if root >= 0 {
		end, err := parseNFOXML(data, n, converted)
		if err != nil && n.Kind == "" {
			return nil, err
		}
		if end > 0 && end <= int64(len(data)) {
			idsFromText(string(data[end:]), n)
		}
	}
	if n.Kind == "" {
		idsFromText(string(data), n)
		if n.IMDBID == "" && n.TMDBID == 0 {
			return nil, ErrNotNFO
		}
	}
	return n, nil
}

func idsFromText(s string, n *NFO) {
	if n.IMDBID == "" {
		if m := reIMDbURL.FindStringSubmatch(s); m != nil {
			n.IMDBID = m[1]
		}
	}
	if n.TMDBID == 0 {
		if m := reTMDBURL.FindStringSubmatch(s); m != nil {
			n.TMDBID, _ = strconv.ParseInt(m[1], 10, 64)
		}
	}
}

// parseNFOXML fills n from the first recognised root element and returns the
// byte offset just after it.
func parseNFOXML(data []byte, n *NFO, converted bool) (int64, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	if converted {
		// Already transcoded to UTF-8: ignore the declared charset.
		dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	} else {
		dec.CharsetReader = charsetReader
	}

	depth := 0
	inRoot := false
	var field string
	var attrType string
	var text strings.Builder
	tokens := 0
	for {
		tokens++
		if tokens > maxNFOTokens {
			return dec.InputOffset(), errors.New("naming: nfo too complex")
		}
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return dec.InputOffset(), nil
			}
			return dec.InputOffset(), fmt.Errorf("naming: nfo xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxNFODepth {
				return dec.InputOffset(), errors.New("naming: nfo nested too deeply")
			}
			if !inRoot {
				if depth != 1 {
					continue
				}
				switch strings.ToLower(t.Name.Local) {
				case "movie":
					n.Kind = NFOMovie
				case "tvshow":
					n.Kind = NFOTVShow
				case "episodedetails":
					n.Kind = NFOEpisode
				default:
					continue
				}
				inRoot = true
				continue
			}
			if depth == 2 {
				field = strings.ToLower(t.Name.Local)
				attrType = ""
				for _, a := range t.Attr {
					if strings.EqualFold(a.Name.Local, "type") {
						attrType = strings.ToLower(strings.TrimSpace(a.Value))
					}
				}
				text.Reset()
			}
		case xml.CharData:
			if inRoot && depth == 2 && text.Len() < maxPlotBytes {
				text.Write(t)
			}
		case xml.EndElement:
			if inRoot && depth == 2 && field != "" {
				applyNFOField(n, field, attrType, strings.TrimSpace(text.String()))
				field = ""
			}
			depth--
			if inRoot && depth == 0 {
				finishNFO(n)
				return dec.InputOffset(), nil
			}
		}
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func applyNFOField(n *NFO, field, typ, v string) {
	if v == "" {
		return
	}
	short := clip(v, maxFieldBytes)
	switch field {
	case "title":
		n.Title = short
	case "originaltitle":
		n.OriginalTitle = short
	case "sorttitle":
		n.SortTitle = short
	case "showtitle":
		n.ShowTitle = short
	case "year":
		if y, err := strconv.Atoi(v); err == nil && y >= minYear && y <= maxYear {
			n.Year = y
		}
	case "premiered", "aired":
		if m := reDate.FindStringSubmatch(v); m != nil {
			if n.Aired == "" {
				n.Aired = v[:10]
			}
			if n.Year == 0 {
				if y := atoi(m[1]); y >= minYear && y <= maxYear {
					n.Year = y
				}
			}
		}
	case "plot":
		n.Plot = clip(v, maxPlotBytes)
	case "outline":
		if n.Plot == "" {
			n.Plot = clip(v, maxPlotBytes)
		}
	case "genre":
		for _, g := range strings.Split(short, "/") {
			addGenre(n, strings.TrimSpace(g))
		}
	case "mpaa", "certification":
		if n.MPAA == "" {
			n.MPAA = normRating(short)
		}
	case "season":
		if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < 100000 {
			n.Season = &i
		}
	case "episode":
		if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < 100000 {
			n.Episode = &i
		}
	case "runtime":
		if m := reLeadNum.FindStringSubmatch(v); m != nil {
			n.RuntimeMinutes = atoi(m[1])
		}
	case "uniqueid":
		switch typ {
		case "tmdb", "themoviedb":
			if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
				n.TMDBID = id
			}
		case "imdb":
			if reIMDbID.MatchString(v) {
				n.IMDBID = v
			}
		}
	case "tmdbid", "tmdb":
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && n.TMDBID == 0 {
			n.TMDBID = id
		}
	case "imdbid", "imdb_id", "imdb", "id":
		if reIMDbID.MatchString(v) && n.IMDBID == "" {
			n.IMDBID = v
		}
	}
}

func addGenre(n *NFO, g string) {
	if g == "" || len(n.Genres) >= maxGenres {
		return
	}
	for _, e := range n.Genres {
		if strings.EqualFold(e, g) {
			return
		}
	}
	n.Genres = append(n.Genres, g)
}

func normRating(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 6 && strings.EqualFold(s[:6], "rated ") {
		s = strings.TrimSpace(s[6:])
	}
	// "US:PG-13" / "gb:15" → keep the rating only. Multiple countries are
	// separated by " / "; the first wins.
	if i := strings.Index(s, " / "); i > 0 {
		s = s[:i]
	}
	if len(s) > 3 && s[2] == ':' && isLetter(s[0]) && isLetter(s[1]) {
		s = strings.TrimSpace(s[3:])
	}
	return s
}

func finishNFO(n *NFO) {
	if n.Kind == NFOMovie && n.Title == "" && n.OriginalTitle != "" {
		n.Title = n.OriginalTitle
	}
}

// charsetReader supports the encodings found in real NFOs besides UTF-8.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1", "windows-1252", "cp1252":
		b, err := io.ReadAll(io.LimitReader(input, MaxNFOBytes))
		if err != nil {
			return nil, err
		}
		return strings.NewReader(decodeWindows1252(b)), nil
	}
	return nil, fmt.Errorf("naming: unsupported nfo charset %q", label)
}

// win1252 maps bytes 0x80–0x9F; other bytes equal their Latin-1 code point.
var win1252 = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

// decodeWindows1252 converts Windows-1252 (a superset of printable Latin-1)
// bytes to a UTF-8 string.
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
