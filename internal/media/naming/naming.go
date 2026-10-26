// Package naming infers media metadata from file and folder names (scene
// releases, Plex/Kodi/Jellyfin-style libraries, anime fansub names) and from
// Kodi .nfo sidecars. It never touches the filesystem: callers pass
// space-relative paths and readers. All functions are pure and safe on
// arbitrary (hostile) input.
package naming

import (
	"fmt"
	"regexp"
	"strings"
)

// Kinds reported in Parsed.Kind.
const (
	KindMovie   = "movie"
	KindTV      = "tv"
	KindUnknown = "unknown"
)

// maxPathLen bounds the work done on absurd inputs.
const maxPathLen = 4096

// Parsed is what Parse could infer from a relative path.
type Parsed struct {
	Kind string // KindMovie, KindTV or KindUnknown
	// Title is the best display title: the movie title; for TV the episode
	// title, or "Show S01E02" when the file names none; for unknown files a
	// cleaned file name.
	Title string
	// Year is the movie's release year, or the show's year when the name
	// disambiguates it ("Doctor Who (2005)"). 0 = unknown.
	Year      int
	ShowTitle string // TV only, without the year
	// Season and Episode are valid when Kind == KindTV. Season 0 = specials.
	// Daily shows use Season = year and Episode = month*100+day (sortable).
	Season, Episode int
	// EpisodeEnd is the last episode of a multi-episode file (S01E01E02,
	// S01E01-E03); 0 for single episodes.
	EpisodeEnd   int
	EpisodeTitle string
	// AbsoluteEpisode is set for anime-style absolute numbering
	// ("Show - 137"); Season is then 1 and Episode == AbsoluteEpisode.
	AbsoluteEpisode int
	// AirDate is "YYYY-MM-DD" for date-based (daily) episodes.
	AirDate string
	// Quality is a canonical resolution tag ("2160p", "1080p", "720p"…) and
	// Source a canonical source ("bluray", "web-dl", "webrip", "web",
	// "hdtv", "dvd", "remux"…), when present in the name.
	Quality, Source string
	ReleaseGroup    string
}

var mediaExt = map[string]bool{
	"mkv": true, "mp4": true, "m4v": true, "mov": true, "avi": true, "webm": true, "wmv": true, "mpg": true,
	"mpeg": true, "ts": true, "m2ts": true, "mts": true, "vob": true, "flv": true, "ogv": true, "3gp": true,
	"divx": true, "xvid": true, "iso": true, "nfo": true, "srt": true, "vtt": true, "ass": true, "ssa": true,
	"sub": true, "idx": true, "mp3": true, "m4a": true, "flac": true, "ogg": true, "opus": true, "wav": true,
	"jpg": true, "jpeg": true, "png": true, "gif": true, "rmvb": true, "rm": true, "asf": true, "f4v": true,
}

// stripExt removes a file extension: any known media extension, or a short
// alphanumeric extension containing a letter (so "Movie.2010" keeps 2010).
func stripExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return name
	}
	ext := strings.ToLower(name[i+1:])
	if mediaExt[ext] {
		return name[:i]
	}
	if len(ext) > 4 || isDigits(ext) {
		return name
	}
	for j := 0; j < len(ext); j++ {
		c := ext[j]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return name
		}
	}
	// Unknown short extensions on names with spaces are probably real
	// extensions; on scene names "Name.ext" could be a word, but a
	// four-letter tail is far more often an extension.
	return name[:i]
}

var (
	reLeadGroup = regexp.MustCompile(`^\s*[\[【]([^\]】]{1,64})[\]】]\s*`)
	reWebsite   = regexp.MustCompile(`(?i)^\s*(?:www\.)[a-z0-9-]+\.[a-z]{2,6}\s*-\s*`)

	reSxEy     = regexp.MustCompile(`^s(\d{1,4})[ ._-]?e(\d{1,4})((?:[-_]?e\d{1,4})*)(?:-(\d{1,4}))?(?:v\d)?$`)
	reSeason   = regexp.MustCompile(`^s(\d{1,4})$`)
	reEp       = regexp.MustCompile(`^(?:e|ep|episode)(\d{1,4})((?:[-_]?e\d{1,4})*)(?:-(\d{1,4}))?(?:v\d)?$`)
	reNxM      = regexp.MustCompile(`^(\d{1,2})x(\d{1,3})(?:[-_](?:\d{1,2}x)?(\d{1,3})|((?:x\d{1,3})+))?$`)
	reMoreEps  = regexp.MustCompile(`e(\d{1,4})`)
	reISODate  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	reNumber   = regexp.MustCompile(`^(\d{1,4})(?:v\d)?$`)
	reSeasonFn = regexp.MustCompile(`^(?:season|series|saison|staffel|temporada|stagione|seizoen)[ ._-]*(\d{1,4})\b`)
	reSpecials = regexp.MustCompile(`^(?:specials?|extras? ?season ?0|season[ ._-]*0+)$`)
)

var genericDirs = map[string]bool{
	"tv": true, "tv shows": true, "tvshows": true, "shows": true, "series": true, "anime": true, "downloads": true,
	"download": true, "media": true, "videos": true, "video": true, "movies": true, "films": true, "complete": true,
	"library": true, "tv series": true, "television": true, "new": true, "incoming": true, "torrents": true,
}

// Parse infers metadata from a space-relative path ("/" or "\" separated).
// Folder names are used for context: "Show/Season 2/05 - Title.mkv",
// "Movies/Name (2010)/movie.mkv".
func Parse(relPath string) Parsed {
	if len(relPath) > maxPathLen {
		relPath = relPath[len(relPath)-maxPathLen:]
	}
	relPath = strings.ReplaceAll(relPath, `\`, "/")
	var segs []string
	for _, s := range strings.Split(relPath, "/") {
		if s = strings.TrimSpace(s); s != "" && s != "." && s != ".." {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return Parsed{Kind: KindUnknown}
	}
	file := stripExt(segs[len(segs)-1])
	dirs := segs[:len(segs)-1]

	var p Parsed
	name, group := cleanName(file)
	p.ReleaseGroup = group
	toks := tokenize(name)

	ctx := folderContext(dirs)
	if parseTV(&p, toks, ctx) {
		return finishTV(p)
	}
	parseMovie(&p, toks, ctx)
	return p
}

// cleanName removes a leading "[Group]" tag and "www.site.tld - " prefixes.
func cleanName(s string) (string, string) {
	group := ""
	for range 3 {
		m := reLeadGroup.FindStringSubmatchIndex(s)
		if m == nil {
			break
		}
		inner := s[m[2]:m[3]]
		// Keep bracketed years/qualities at the very start (rare but valid).
		if yearOf(makeToken(inner)) != 0 {
			break
		}
		if group == "" && classify([]token{makeToken("[" + inner + "]")}, 0).class == tagNone {
			group = strings.TrimSpace(inner)
		}
		s = s[m[1]:]
	}
	if m := reWebsite.FindStringIndex(s); m != nil {
		s = s[m[1]:]
	}
	return s, group
}

// folderCtx is what the parent folders say about a file.
type folderCtx struct {
	season     int
	hasSeason  bool
	show       string // cleaned show folder title (may be "")
	showYear   int
	parent     string // cleaned parent folder title (non-season)
	parentYear int
	parentTags bool // parent folder name carries release tags
}

func folderContext(dirs []string) folderCtx {
	var c folderCtx
	if len(dirs) == 0 {
		return c
	}
	parent := dirs[len(dirs)-1]
	if s, ok := seasonFolder(parent); ok {
		c.season, c.hasSeason = s, true
		for i := len(dirs) - 2; i >= 0; i-- {
			if _, ok := seasonFolder(dirs[i]); ok {
				continue
			}
			if genericDirs[strings.ToLower(dirs[i])] {
				break
			}
			c.show, c.showYear, _, _ = folderTitle(dirs[i])
			break
		}
		return c
	}
	if genericDirs[strings.ToLower(parent)] {
		return c
	}
	var fs int
	var fsOK bool
	c.parent, c.parentYear, fs, fsOK = folderTitle(parent)
	if fsOK {
		// A season-pack folder such as "Show.Name.S02.1080p.WEB-DL".
		c.season, c.hasSeason = fs, true
		c.show, c.showYear = c.parent, c.parentYear
	}
	ptoks := tokenize(parent)
	for i := range ptoks {
		if classify(ptoks, i).class == tagStrong {
			c.parentTags = true
			break
		}
	}
	return c
}

// seasonFolder recognises "Season 1", "Season 01 (2008)", "S01", "Series 2",
// "Specials".
func seasonFolder(name string) (int, bool) {
	l := strings.ToLower(strings.TrimSpace(name))
	if reSpecials.MatchString(l) {
		return 0, true
	}
	if m := reSeasonFn.FindStringSubmatch(l); m != nil {
		return atoi(m[1]), true
	}
	if m := reSeason.FindStringSubmatch(l); m != nil {
		return atoi(m[1]), true
	}
	return 0, false
}

// folderTitle cleans a folder name into (title, year, season-if-any).
func folderTitle(name string) (string, int, int, bool) {
	n, _ := cleanName(name)
	toks := tokenize(n)
	end := len(toks)
	year, season, hasSeason := 0, 0, false
	for i := range toks {
		if i > 0 && yearOf(toks[i]) != 0 && year == 0 {
			year = yearOf(toks[i])
			end = min(end, i)
		}
		if m := reSeason.FindStringSubmatch(toks[i].low); m != nil && i > 0 {
			season, hasSeason = atoi(m[1]), true
			end = min(end, i)
		}
		if (toks[i].low == "season" || toks[i].low == "series") && i > 0 && i+1 < len(toks) && isDigits(toks[i+1].low) {
			season, hasSeason = atoi(toks[i+1].low), true
			end = min(end, i)
		}
		if classify(toks, i).class == tagStrong {
			end = min(end, i)
		}
	}
	return joinTitle(toks[:end]), year, season, hasSeason
}

// marker describes an episode designation found in the token list.
type marker struct {
	start, end      int // token span [start, end)
	season          int
	hasSeason       bool
	episode, epEnd  int
	hasEpisode      bool
	absolute        bool
	airDate         string
	dailyY, dailyMD int
}

func parseTV(p *Parsed, toks []token, ctx folderCtx) bool {
	m, ok := findMarker(toks, ctx)
	if !ok {
		return false
	}
	p.Kind = KindTV

	// Show title: tokens before the marker, a trailing year removed.
	showToks := toks[:m.start]
	year := 0
	if n := len(showToks); n >= 2 {
		if y := yearOf(showToks[n-1]); y != 0 {
			year = y
			showToks = showToks[:n-1]
		} else if n >= 3 && isDash(showToks[n-1]) {
			if y := yearOf(showToks[n-2]); y != 0 {
				year = y
				showToks = showToks[:n-2]
			}
		}
	}
	// Strip tags that crept into the show part ("[1080p] Show - 01").
	cut := len(showToks)
	for i := range showToks {
		if classify(showToks, i).class == tagStrong {
			cut = i
			break
		}
	}
	fileShow := joinTitle(showToks[:cut])

	switch {
	case ctx.show != "" && (fileShow == "" || ctx.hasSeason):
		p.ShowTitle, p.Year = ctx.show, ctx.showYear
	case fileShow == "" && ctx.parent != "" && !ctx.parentTags:
		p.ShowTitle, p.Year = ctx.parent, ctx.parentYear
	case fileShow != "" && ctx.parent != "" && ShowKey(ctx.parent) == ShowKey(fileShow):
		p.ShowTitle, p.Year = ctx.parent, ctx.parentYear // nicer punctuation/casing from the folder
	default:
		p.ShowTitle = fileShow
	}
	if year != 0 {
		p.Year = year
	}

	switch {
	case m.hasSeason:
		p.Season = m.season
	case ctx.hasSeason:
		p.Season = ctx.season
	default:
		p.Season = 1
	}
	p.Episode, p.EpisodeEnd = m.episode, m.epEnd
	if m.absolute && !ctx.hasSeason {
		p.AbsoluteEpisode = m.episode
	}
	if m.airDate != "" {
		p.AirDate = m.airDate
		p.Season, p.Episode = m.dailyY, m.dailyMD
	}

	// Episode title: after the marker up to the first strong tag.
	rest := toks[m.end:]
	end := len(rest)
	for i := range rest {
		if classify(rest, i).class == tagStrong {
			end = i
			break
		}
	}
	titleToks := rest[:end]
	for len(titleToks) > 0 {
		last := titleToks[len(titleToks)-1]
		if classify(titleToks, len(titleToks)-1).class == tagWeak && last.raw == strings.ToUpper(last.raw) && len(last.raw) > 1 {
			titleToks = titleToks[:len(titleToks)-1]
			continue
		}
		break
	}
	p.EpisodeTitle = joinTitle(titleToks)
	fillTags(p, toks, m.end)
	return true
}

func finishTV(p Parsed) Parsed {
	switch {
	case p.EpisodeTitle != "":
		p.Title = p.EpisodeTitle
	case p.AirDate != "":
		p.Title = strings.TrimSpace(p.ShowTitle + " " + p.AirDate)
	default:
		code := fmt.Sprintf("S%02dE%02d", p.Season, p.Episode)
		if p.EpisodeEnd > p.Episode {
			code += fmt.Sprintf("-E%02d", p.EpisodeEnd)
		}
		p.Title = strings.TrimSpace(p.ShowTitle + " " + code)
	}
	return p
}

// findMarker locates the episode designation, trying the most explicit forms
// first across the whole name before falling back to weaker ones.
func findMarker(toks []token, ctx folderCtx) (marker, bool) {
	// 1. S01E02 / S01.E02 / 1x02 / "Season 1 Episode 2".
	for i := range toks {
		if m, ok := strongMarker(toks, i); ok {
			return m, true
		}
	}
	// 2. Daily: 2026.10.26 or 2026-10-26 (needs a show name or folder).
	for i := range toks {
		if m, ok := dailyMarker(toks, i); ok && (i > 0 || ctx.show != "" || ctx.parent != "") {
			return m, true
		}
	}
	// 3. Season-only token followed by an episode ("Show S2 - 05", "Show S01 E05").
	for i := range toks {
		sm := reSeason.FindStringSubmatch(toks[i].low)
		if sm == nil || i == 0 && ctx.show == "" {
			continue
		}
		j := i + 1
		if j < len(toks) && isDash(toks[j]) {
			j++
		}
		if j < len(toks) {
			if n := reNumber.FindStringSubmatch(toks[j].low); n != nil && yearOf(toks[j]) == 0 {
				return marker{start: i, end: j + 1, season: atoi(sm[1]), hasSeason: true, episode: atoi(n[1]), hasEpisode: true}, true
			}
		}
	}
	// 4. Episode-only: E05, Ep05, "Episode 5", "Ep 5".
	// Skipped outside season folders when a release year follows, so
	// "Star.Wars.Episode.5.The.Empire.Strikes.Back.1980" stays a movie.
	for i := range toks {
		if m, ok := episodeOnly(toks, i); ok && (ctx.hasSeason || !hasYearFrom(toks, m.end)) {
			return m, true
		}
	}
	// 5. Inside a season folder: leading number "05 - Title", "105 Title".
	if ctx.hasSeason && len(toks) > 0 {
		if n := reNumber.FindStringSubmatch(toks[0].low); n != nil && !toks[0].bracket {
			num := n[1]
			ep := atoi(num)
			// "105" in "Season 1" → episode 5 (S/EE compact numbering).
			if len(num) >= 3 && ctx.season > 0 {
				sp := fmt.Sprint(ctx.season)
				if strings.HasPrefix(num, sp) && len(num)-len(sp) == 2 {
					ep = atoi(num[len(sp):])
				}
			}
			if yearOf(toks[0]) == 0 || len(toks) > 1 && isDash(toks[1]) {
				return marker{start: 0, end: 1, episode: ep, hasEpisode: true}, true
			}
		}
		// Anime-style "Show - 05" inside a season folder.
		if m, ok := absoluteMarker(toks); ok {
			m.absolute = false
			return m, true
		}
	}
	// 6. Anime absolute numbering: "Show - 137", "Show - 05v2".
	if m, ok := absoluteMarker(toks); ok {
		return m, true
	}
	return marker{}, false
}

func strongMarker(toks []token, i int) (marker, bool) {
	t := toks[i].low
	if m := reSxEy.FindStringSubmatch(t); m != nil {
		mk := marker{start: i, end: i + 1, season: atoi(m[1]), hasSeason: true, episode: atoi(m[2]), hasEpisode: true}
		mk.epEnd = moreEpisodes(m[3], m[4], mk.episode)
		return mk, true
	}
	if m := reNxM.FindStringSubmatch(t); m != nil && !toks[i].bracket {
		mk := marker{start: i, end: i + 1, season: atoi(m[1]), hasSeason: true, episode: atoi(m[2]), hasEpisode: true}
		if m[3] != "" {
			mk.epEnd = endIfGreater(atoi(m[3]), mk.episode)
		} else if m[4] != "" {
			xs := strings.Split(strings.TrimPrefix(m[4], "x"), "x")
			mk.epEnd = endIfGreater(atoi(xs[len(xs)-1]), mk.episode)
		}
		return mk, true
	}
	// "S01" "E02" split by a separator.
	if sm := reSeason.FindStringSubmatch(t); sm != nil && i+1 < len(toks) {
		if em := reEp.FindStringSubmatch(toks[i+1].low); em != nil && strings.HasPrefix(toks[i+1].low, "e") {
			mk := marker{start: i, end: i + 2, season: atoi(sm[1]), hasSeason: true, episode: atoi(em[1]), hasEpisode: true}
			mk.epEnd = moreEpisodes(em[2], em[3], mk.episode)
			return mk, true
		}
	}
	// "Season 1 Episode 2" / "Season 1 Ep 2" / "Season 1 E2".
	if (t == "season" || t == "series") && i+2 < len(toks) && isDigits(toks[i+1].low) && len(toks[i+1].low) <= 4 {
		j := i + 2
		if isDash(toks[j]) && j+1 < len(toks) {
			j++
		}
		w := toks[j].low
		if (w == "episode" || w == "ep" || w == "e") && j+1 < len(toks) && isDigits(toks[j+1].low) {
			return marker{start: i, end: j + 2, season: atoi(toks[i+1].low), hasSeason: true, episode: atoi(toks[j+1].low), hasEpisode: true}, true
		}
		if em := reEp.FindStringSubmatch(w); em != nil {
			return marker{start: i, end: j + 1, season: atoi(toks[i+1].low), hasSeason: true, episode: atoi(em[1]), hasEpisode: true, epEnd: moreEpisodes(em[2], em[3], atoi(em[1]))}, true
		}
	}
	return marker{}, false
}

func episodeOnly(toks []token, i int) (marker, bool) {
	t := toks[i].low
	if i == 0 && len(toks) == 1 && !strings.HasPrefix(t, "e") {
		return marker{}, false
	}
	if m := reEp.FindStringSubmatch(t); m != nil {
		mk := marker{start: i, end: i + 1, episode: atoi(m[1]), hasEpisode: true}
		mk.epEnd = moreEpisodes(m[2], m[3], mk.episode)
		return mk, true
	}
	if (t == "episode" || t == "ep" || t == "ep.") && i+1 < len(toks) {
		if n := reNumber.FindStringSubmatch(toks[i+1].low); n != nil {
			return marker{start: i, end: i + 2, episode: atoi(n[1]), hasEpisode: true}, true
		}
	}
	return marker{}, false
}

func absoluteMarker(toks []token) (marker, bool) {
	for i := 1; i+1 < len(toks); i++ {
		if !isDash(toks[i]) {
			continue
		}
		n := reNumber.FindStringSubmatch(toks[i+1].low)
		if n == nil || toks[i+1].bracket || yearOf(toks[i+1]) != 0 {
			continue
		}
		// Must be followed by nothing, a dash, or tags — not more title words
		// with a number in the middle of a movie name ("Ocean's - 11 Men").
		if j := i + 2; j < len(toks) && !isDash(toks[j]) && classify(toks, j).class == tagNone && !startsBracket(toks[j]) {
			continue
		}
		ep := atoi(n[1])
		return marker{start: i, end: i + 2, episode: ep, hasEpisode: true, absolute: true}, true
	}
	return marker{}, false
}

func dailyMarker(toks []token, i int) (marker, bool) {
	if m := reISODate.FindStringSubmatch(toks[i].low); m != nil {
		return dateMarker(i, i+1, atoi(m[1]), atoi(m[2]), atoi(m[3]))
	}
	if i+2 < len(toks) && len(toks[i].low) == 4 && len(toks[i+1].low) == 2 && len(toks[i+2].low) == 2 &&
		isDigits(toks[i].low) && isDigits(toks[i+1].low) && isDigits(toks[i+2].low) {
		return dateMarker(i, i+3, atoi(toks[i].low), atoi(toks[i+1].low), atoi(toks[i+2].low))
	}
	return marker{}, false
}

func dateMarker(start, end, y, mo, d int) (marker, bool) {
	if y < 1950 || y > maxYear || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return marker{}, false
	}
	return marker{
		start: start, end: end, hasSeason: true, season: y, hasEpisode: true,
		airDate: fmt.Sprintf("%04d-%02d-%02d", y, mo, d), dailyY: y, dailyMD: mo*100 + d,
	}, true
}

// moreEpisodes interprets the tail of S01E02E03 / S01E02-E03 / S01E02-03.
func moreEpisodes(chain, dashNum string, first int) int {
	end := 0
	for _, m := range reMoreEps.FindAllStringSubmatch(chain, -1) {
		end = atoi(m[1])
	}
	if dashNum != "" {
		end = atoi(dashNum)
	}
	return endIfGreater(end, first)
}

func endIfGreater(end, first int) int {
	if end > first {
		return end
	}
	return 0
}

func parseMovie(p *Parsed, toks []token, ctx folderCtx) {
	firstStrong := len(toks)
	for i := range toks {
		if classify(toks, i).class == tagStrong {
			firstStrong = i
			break
		}
	}
	// Year: prefer a bracketed year, else the last year before the first
	// tag ("Blade.Runner.2049.2017.1080p" → 2017), else the first after.
	yearIdx := -1
	for i := 1; i < len(toks); i++ {
		if yearOf(toks[i]) != 0 && toks[i].bracket {
			yearIdx = i
		}
	}
	if yearIdx < 0 {
		for i := 1; i < firstStrong; i++ {
			if yearOf(toks[i]) != 0 {
				yearIdx = i
			}
		}
	}
	if yearIdx < 0 {
		for i := max(firstStrong, 1); i < len(toks); i++ {
			if yearOf(toks[i]) != 0 {
				yearIdx = i
				break
			}
		}
	}
	end := firstStrong
	if yearIdx >= 0 {
		end = min(end, yearIdx)
		p.Year = yearOf(toks[yearIdx])
	}
	p.Title = joinTitle(toks[:end])
	tagFrom := end
	fillTags(p, toks, tagFrom)

	// "Movies/Name (2010)/movie.mkv": a bare file name inside a titled folder.
	if p.Year == 0 && ctx.parentYear != 0 && !ctx.hasSeason {
		p.Title, p.Year = ctx.parent, ctx.parentYear
	}
	if p.Title == "" {
		p.Title = joinTitle(toks)
	}
	if p.Year != 0 || p.Quality != "" || p.Source != "" || firstStrong < len(toks) {
		p.Kind = KindMovie
	} else {
		p.Kind = KindUnknown
	}
}

// fillTags records quality/source/release group from tokens at or after from.
func fillTags(p *Parsed, toks []token, from int) {
	for i := max(from, 0); i < len(toks); i++ {
		ti := classify(toks, i)
		if p.Quality == "" && ti.quality != "" {
			p.Quality = ti.quality
		}
		if p.Source == "" && ti.source != "" {
			p.Source = ti.source
		}
	}
	if p.ReleaseGroup != "" || from >= len(toks) {
		return
	}
	// Scene group: "…x264-GRP" as the last token, but only inside the tag
	// region (so "Spider-Man" is not a group) and not when the whole token is
	// itself a tag ("WEB-DL").
	last := toks[len(toks)-1]
	k := strings.LastIndexByte(last.raw, '-')
	if k <= 0 || k == len(last.raw)-1 || last.bracket {
		return
	}
	if _, whole := tagTable[normTag(last.low)]; whole {
		return
	}
	for i := max(from, 0); i < len(toks); i++ {
		if classify(toks, i).class == tagStrong {
			p.ReleaseGroup = last.raw[k+1:]
			return
		}
	}
}

func hasYearFrom(toks []token, from int) bool {
	for i := from; i < len(toks); i++ {
		if yearOf(toks[i]) != 0 {
			return true
		}
	}
	return false
}

func startsBracket(t token) bool {
	return t.raw != "" && (t.raw[0] == '[' || t.raw[0] == '(')
}
