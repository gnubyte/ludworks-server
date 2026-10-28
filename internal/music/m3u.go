package music

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// M3U/M3U8 import and export (API.md §12). Imported entries resolve by path relative to
// base_path in space_id (default: the caller's home root), then as "<space name>/<path>" (the
// export format), then by the "#EXTINF" "Artist - Title".

const (
	maxM3UBytes    = 5 << 20
	maxM3ULines    = 100_000
	maxM3UEntries  = 2 * maxPlaylistItems // entries looked up per import
	maxUnmatched   = 1000
	maxM3UEntryLen = 4096
)

// decodeText decodes a text file: UTF-8 (BOM stripped), UTF-16 with a BOM, else Windows
// Latin-1 (classic .m3u files are not UTF-8).
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(b[3:]), "")
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		be := b[0] == 0xFE
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			if be {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			} else {
				u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
			}
		}
		return string(utf16.Decode(u))
	case utf8.Valid(b):
		return string(b)
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// m3uEntry is one playlist entry with its #EXTINF display text.
type m3uEntry struct {
	location, title string
}

func parseM3U(text string) []m3uEntry {
	var (
		out     []m3uEntry
		pending string
		lines   int
	)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), maxM3UEntryLen*4)
	for sc.Scan() && lines < maxM3ULines {
		lines++
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXTINF:"):
			if _, t, ok := strings.Cut(line[len("#EXTINF:"):], ","); ok {
				pending = strings.TrimSpace(t)
			}
		case strings.HasPrefix(line, "#"):
		default:
			if len(line) <= maxM3UEntryLen {
				out = append(out, m3uEntry{location: line, title: pending})
			}
			pending = ""
		}
	}
	return out
}

// relPath cleans an entry relative to base ("" when it leaves the space or is not a path).
func relPath(base, loc string) string {
	if strings.Contains(loc, "://") || strings.HasPrefix(loc, "file:") {
		return ""
	}
	loc = strings.ReplaceAll(loc, `\`, "/")
	if strings.HasPrefix(loc, "/") || (len(loc) > 1 && loc[1] == ':') {
		return "" // absolute paths of another machine
	}
	joined := path.Join("/", base, loc)
	if !strings.HasPrefix(joined, "/") || strings.Contains(joined, "\x00") {
		return ""
	}
	return strings.Trim(joined, "/")
}

// importM3U creates a manual playlist from an M3U body; it returns the playlist and the
// entries that matched no readable track.
func (s *Service) importM3U(ctx context.Context, p *core.Principal, name, spaceID, basePath string, body io.Reader) (*Playlist, []string, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxM3UBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxM3UBytes {
		return nil, nil, core.ErrPayloadTooLarge
	}
	if spaceID == "" {
		if spaceID, err = s.files.HomeSpaceID(ctx, p.UserID); err != nil {
			return nil, nil, err
		}
	} else if !httpx.ValidID(spaceID) || !s.files.CanRead(ctx, p, spaceID) {
		return nil, nil, core.NotFound("Space not found")
	}
	base, ok := cleanDirPath(basePath)
	if !ok || len(basePath) > 4096 {
		return nil, nil, core.Validation(map[string]string{"base_path": "invalid path"})
	}
	if strings.TrimSpace(name) == "" {
		name = "Imported playlist"
	}
	readable, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	scopeSpaces, err := s.scope(ctx, p, "")
	if err != nil {
		return nil, nil, err
	}
	spaceNames, err := s.spaceNames(ctx, readable)
	if err != nil {
		return nil, nil, err
	}
	entries := parseM3U(decodeText(data))
	if len(entries) > maxM3UEntries {
		return nil, nil, core.Validation(map[string]string{"body": fmt.Sprintf("at most %d entries", maxM3UEntries)})
	}
	var (
		ids       []string
		unmatched = []string{}
	)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		id := ""
		if rel := relPath(base, e.location); rel != "" {
			id = s.musicFileAt(ctx, readable, spaceID, rel)
		}
		if id == "" {
			if first, rest, ok := strings.Cut(strings.ReplaceAll(e.location, `\`, "/"), "/"); ok && rest != "" {
				for _, sp := range spaceNames[fold(first)] {
					if id = s.musicFileAt(ctx, readable, sp, strings.Trim(path.Clean("/"+rest), "/")); id != "" {
						break
					}
				}
			}
		}
		if id == "" && e.title != "" {
			id = s.trackByDisplay(ctx, scopeSpaces, e.title)
		}
		if id == "" {
			if len(unmatched) < maxUnmatched {
				unmatched = append(unmatched, cleanText(e.location, maxM3UEntryLen))
			}
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) > maxPlaylistItems {
		return nil, nil, core.Validation(map[string]string{"body": fmt.Sprintf("a playlist holds at most %d items", maxPlaylistItems)})
	}
	if ids == nil {
		ids = []string{}
	}
	pl, err := s.createPlaylist(ctx, p, PlaylistCreate{Name: truncateRunes(cleanText(name, maxPlaylistName), maxPlaylistName), TrackIDs: ids})
	if err != nil {
		return nil, nil, err
	}
	return pl, unmatched, nil
}

// spaceNames maps folded space names to the readable spaces with that name (the caller's
// home first for "Home").
func (s *Service) spaceNames(ctx context.Context, readable []string) (map[string][]string, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id, name FROM spaces WHERE id IN (SELECT value FROM json_each(?))
		ORDER BY kind = 'library', id`, mustJSON(readable))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string][]string{}
	for rs.Next() {
		var id, name string
		if err := rs.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[fold(name)] = append(out[fold(name)], id)
	}
	return out, rs.Err()
}

// musicFileAt returns the id of the readable music file at a path in a space ("" if none).
func (s *Service) musicFileAt(ctx context.Context, readable []string, spaceID, p string) string {
	if p == "" || !containsStr(readable, spaceID) {
		return ""
	}
	var id string
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT f.id FROM files f WHERE f.space_id = ? AND f.path = ? AND `+eligibleSQL,
		spaceID, p).Scan(&id)
	if err != nil {
		return ""
	}
	if err := s.ensureRows(ctx, []string{id}); err != nil {
		return ""
	}
	return id
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// trackByDisplay finds a track by "Artist - Title" in the caller's library.
func (s *Service) trackByDisplay(ctx context.Context, spaces []string, display string) string {
	artist, title, ok := strings.Cut(display, " - ")
	if !ok {
		return ""
	}
	w := &whereBuilder{}
	visibleWhere(w, spaces)
	w.add(`t.title_key = ?`, fold(strings.TrimSpace(title)))
	w.add(`(t.artist_key = ? OR EXISTS (SELECT 1 FROM music_track_artists ta WHERE ta.file_id = t.file_id AND ta.name_key = ?))`,
		fold(strings.TrimSpace(artist)), fold(strings.TrimSpace(artist)))
	var id string
	if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT t.file_id FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id WHERE `+
		w.sql()+` ORDER BY t.file_id LIMIT 1`, w.args...).Scan(&id); err != nil {
		return ""
	}
	return id
}

// exportM3U writes the caller's view of a playlist as M3U8.
func (s *Service) exportM3U(w http.ResponseWriter, r *http.Request, p *core.Principal, id string) error {
	ctx := r.Context()
	row, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return err
	}
	v, err := s.view(ctx, s.d.DB.SQL, p, row)
	if err != nil {
		return err
	}
	trs, err := loadTracksByID(ctx, s.d.DB.SQL, p.UserID, uniq(v.ids))
	if err != nil {
		return err
	}
	byID := map[string]*track{}
	var spaceIDs []string
	for _, tr := range trs {
		byID[tr.FileID] = tr
		spaceIDs = append(spaceIDs, tr.SpaceID)
	}
	names := map[string]string{}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id, name FROM spaces WHERE id IN (SELECT value FROM json_each(?))`, mustJSON(uniq(spaceIDs)))
	if err != nil {
		return err
	}
	for rs.Next() {
		var sid, name string
		if err := rs.Scan(&sid, &name); err != nil {
			rs.Close()
			return err
		}
		names[sid] = name
	}
	rs.Close()
	var b bytes.Buffer
	b.WriteString("#EXTM3U\n")
	if n := m3uLine(row.Name); n != "" {
		b.WriteString("#PLAYLIST:" + n + "\n")
	}
	for _, fid := range v.ids {
		tr := byID[fid]
		if tr == nil {
			continue
		}
		secs := -1
		if tr.Duration.Valid {
			secs = int(math.Round(tr.Duration.Float64))
		}
		display := tr.Title
		if tr.Artist != "" {
			display = tr.Artist + " - " + tr.Title
		}
		b.WriteString("#EXTINF:" + strconv.Itoa(secs) + "," + m3uLine(display) + "\n")
		b.WriteString(m3uLine(names[tr.SpaceID]+"/"+tr.Path) + "\n")
	}
	h := w.Header()
	h.Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	h.Set("Content-Disposition", contentDisposition("attachment", safeFileName(row.Name)+".m3u8"))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(b.Bytes())
	return err
}

// m3uLine keeps a value on one line.
func m3uLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
}

// safeFileName makes a playlist name usable as a download name.
func safeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" || s == "." || s == ".." {
		return "playlist"
	}
	return truncateRunes(s, 120)
}
