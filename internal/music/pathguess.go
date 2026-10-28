package music

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Path-based fallbacks (API.md §12): values a file's tags lack are guessed from its
// space-relative path — "Artist/Album/NN - Title.ext", "Artist - Album/NN Title.ext", disc
// folders ("CD1", "Disc 2") and leading track numbers. Without an artist folder a file name
// "NN - Artist - Title.ext" also gives the artist.

type pathGuess struct {
	Title, Artist, Album string
	Track, Disc, Year    int
}

var (
	// "1-03 Title", "2.05 - Title": disc and track.
	discTrackRe = regexp.MustCompile(`^(\d{1,2})[-.](\d{1,3})(?:\s*[-.)]\s*|\s+)(.+)$`)
	// "03 - Title", "03. Title", "03) Title", "03 Title".
	trackRe = regexp.MustCompile(`^(\d{1,3})(?:\s*[-.)]\s*|\s+)(.+)$`)
	// "CD1", "CD 2", "Disc 1", "disk-2", "Disc 1 - Bonus".
	discDirRe = regexp.MustCompile(`(?i)^(?:cd|dis[ck])[\s._-]*(\d{1,2})\b`)
	// "Album (1999)", "Album [1999]".
	yearSuffixRe = regexp.MustCompile(`^(.*?)\s*[(\[]((?:19|20)\d\d)[)\]]\s*$`)
)

// genericDirs are folder names that are never an artist or album.
var genericDirs = map[string]bool{
	"music": true, "musik": true, "musique": true, "musica": true, "audio": true, "mp3": true, "flac": true,
	"media": true, "downloads": true, "itunes": true, "itunes media": true, "library": true, "albums": true,
	"singles": true, "unsorted": true, "new folder": true, "home": true, "various": true,
}

func isGeneric(dir string) bool { return genericDirs[fold(dir)] }

// stripYear splits "Name (1999)" into "Name" and 1999.
func stripYear(s string) (string, int) {
	if m := yearSuffixRe.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		y, _ := strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), y
	}
	return s, 0
}

// guessFromPath derives values from a space-relative path.
func guessFromPath(p string) pathGuess {
	var g pathGuess
	if strings.HasPrefix(p, "/") { // trashed entries live under "/<id>/…"
		return g
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return g
	}
	dirs := strings.Split(path.Dir(p), "/")
	if len(dirs) == 1 && dirs[0] == "." {
		dirs = nil
	}
	// Disc folder: the album is one level up.
	if n := len(dirs); n > 0 {
		if m := discDirRe.FindStringSubmatch(dirs[n-1]); m != nil {
			g.Disc, _ = strconv.Atoi(m[1])
			dirs = dirs[:n-1]
		}
	}
	if n := len(dirs); n > 0 && !isGeneric(dirs[n-1]) {
		album, year := stripYear(dirs[n-1])
		if a, b, ok := strings.Cut(album, " - "); ok && strings.TrimSpace(a) != "" && strings.TrimSpace(b) != "" {
			g.Artist = strings.TrimSpace(a)
			var y int
			if album, y = stripYear(strings.TrimSpace(b)); y > 0 {
				year = y
			}
		} else if n >= 2 && !isGeneric(dirs[n-2]) {
			g.Artist = dirs[n-2]
		}
		g.Album, g.Year = cleanText(album, maxText), year
	}

	base := path.Base(p)
	stem := strings.TrimSpace(strings.TrimSuffix(base, path.Ext(base)))
	stem = strings.TrimSpace(strings.ReplaceAll(stem, "_", " "))
	title := stem
	if m := discTrackRe.FindStringSubmatch(stem); m != nil {
		if g.Disc == 0 {
			g.Disc, _ = strconv.Atoi(m[1])
		}
		g.Track, _ = strconv.Atoi(m[2])
		title = m[3]
	} else if m := trackRe.FindStringSubmatch(stem); m != nil {
		g.Track, _ = strconv.Atoi(m[1])
		title = m[2]
	}
	if g.Artist == "" {
		if a, t, ok := strings.Cut(title, " - "); ok && strings.TrimSpace(a) != "" && strings.TrimSpace(t) != "" {
			g.Artist, title = strings.TrimSpace(a), strings.TrimSpace(t)
		}
	}
	g.Title = cleanText(title, maxText)
	g.Artist = cleanText(g.Artist, maxText)
	return g
}
