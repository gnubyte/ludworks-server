package music

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path"
	"strings"
	"unicode"

	"ludworks/internal/db"
)

// Descriptive metadata: values read from the file (fileTags), guessed from the path, and
// manual edits; computeEffective combines them into the stored effective values.

// Field limits (mirrored in openapi.json).
const (
	maxText       = 500 // title, artist, album, album artist, composer (runes)
	maxComment    = 4000
	maxArtists    = 50
	maxGenres     = 20
	maxGenreRunes = 100
)

// variousArtists is the album artist of compilations without an album artist tag.
const variousArtists = "Various Artists"

// fileTags are the descriptive values read from a file (music_tracks.file_tags).
type fileTags struct {
	Title           string   `json:"title,omitempty"`
	Artist          string   `json:"artist,omitempty"`
	Artists         []string `json:"artists,omitempty"`
	Album           string   `json:"album,omitempty"`
	AlbumArtist     string   `json:"album_artist,omitempty"`
	TrackNumber     int      `json:"track_number,omitempty"`
	TrackTotal      int      `json:"track_total,omitempty"`
	DiscNumber      int      `json:"disc_number,omitempty"`
	DiscTotal       int      `json:"disc_total,omitempty"`
	Year            int      `json:"year,omitempty"`
	Genres          []string `json:"genres,omitempty"`
	Composer        string   `json:"composer,omitempty"`
	Comment         string   `json:"comment,omitempty"`
	Compilation     bool     `json:"compilation,omitempty"`
	SortTitle       string   `json:"sort_title,omitempty"`
	SortArtist      string   `json:"sort_artist,omitempty"`
	SortAlbum       string   `json:"sort_album,omitempty"`
	SortAlbumArtist string   `json:"sort_album_artist,omitempty"`
}

// effective is a track's stored descriptive values.
type effective struct {
	Title, SortTitle, TitleKey                                  string
	Artist, ArtistKey, SortArtist, ArtistID                     string
	Artists                                                     []string
	Album, AlbumKey, SortAlbum, AlbumID                         string
	AlbumArtist, AlbumArtistKey, SortAlbumArtist, AlbumArtistID string
	TrackNumber, TrackTotal, DiscNumber, DiscTotal, Year        sql.NullInt64
	Genres                                                      []string
	Composer, ComposerKey, Comment                              string
	Compilation                                                 bool
	FileName                                                    string
}

// manualFields are the PATCH fields stored as manual edits.
var manualFields = []string{"title", "artist", "artists", "album", "album_artist", "track_number", "track_total",
	"disc_number", "disc_total", "year", "genres", "composer", "compilation"}

func nullInt(v int) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// computeEffective combines file tags, path guesses (for missing values) and manual edits.
// spacePath is the file's space-relative path, name its file name.
func computeEffective(ft fileTags, manual map[string]json.RawMessage, spacePath, name string) effective {
	g := guessFromPath(spacePath)
	d := ft
	if d.Title == "" {
		d.Title = g.Title
	}
	if d.Artist == "" && len(d.Artists) == 0 {
		d.Artist = g.Artist
	}
	if d.Album == "" {
		d.Album = g.Album
	}
	if d.TrackNumber == 0 {
		d.TrackNumber = g.Track
	}
	if d.DiscNumber == 0 {
		d.DiscNumber = g.Disc
	}
	if d.Year == 0 {
		d.Year = g.Year
	}
	// Manual edits override; an explicit null clears.
	str := func(key string, dst *string) bool {
		v, ok := manual[key]
		if !ok {
			return false
		}
		var s string
		if !isNull(v) {
			_ = json.Unmarshal(v, &s)
		}
		*dst = s
		return true
	}
	num := func(key string, dst *int) {
		if v, ok := manual[key]; ok {
			n := 0
			if !isNull(v) {
				_ = json.Unmarshal(v, &n)
			}
			*dst = n
		}
	}
	list := func(key string, dst *[]string) bool {
		v, ok := manual[key]
		if !ok {
			return false
		}
		var l []string
		if !isNull(v) {
			_ = json.Unmarshal(v, &l)
		}
		*dst = l
		return true
	}
	manTitle := str("title", &d.Title)
	manArtist := str("artist", &d.Artist)
	manArtists := list("artists", &d.Artists)
	manAlbum := str("album", &d.Album)
	manAA := str("album_artist", &d.AlbumArtist)
	str("composer", &d.Composer)
	num("track_number", &d.TrackNumber)
	num("track_total", &d.TrackTotal)
	num("disc_number", &d.DiscNumber)
	num("disc_total", &d.DiscTotal)
	num("year", &d.Year)
	list("genres", &d.Genres)
	if v, ok := manual["compilation"]; ok {
		var b bool
		_ = json.Unmarshal(v, &b)
		d.Compilation = b
	}
	if manArtist && !manArtists {
		d.Artists = nil // the tag's individual artists belong to the replaced display string
	}

	var e effective
	e.FileName = name
	e.Title = cleanText(d.Title, maxText)
	if e.Title == "" {
		e.Title = cleanText(strings.TrimSuffix(name, path.Ext(name)), maxText)
	}
	if e.Title == "" {
		e.Title = cleanText(name, maxText)
	}
	e.Artists = normalizeList(d.Artists, maxArtists, maxText, false)
	e.Artist = cleanText(d.Artist, maxText)
	switch {
	case e.Artist == "" && len(e.Artists) > 0:
		e.Artist = truncateRunes(strings.Join(e.Artists, "; "), maxText)
	case len(e.Artists) == 0 && e.Artist != "":
		e.Artists = []string{e.Artist}
	}
	if e.Artists == nil {
		e.Artists = []string{}
	}
	e.Album = cleanText(d.Album, maxText)
	e.Compilation = d.Compilation
	e.AlbumArtist = cleanText(d.AlbumArtist, maxText)
	if e.AlbumArtist == "" {
		if e.Compilation {
			e.AlbumArtist = variousArtists
		} else {
			e.AlbumArtist = e.Artist
		}
	}
	e.Composer = cleanText(d.Composer, maxText)
	e.Comment = cleanMultiline(d.Comment, maxComment)
	e.Genres = normalizeList(d.Genres, maxGenres, maxGenreRunes, true)
	if e.Genres == nil {
		e.Genres = []string{}
	}
	e.TrackNumber, e.TrackTotal = nullInt(d.TrackNumber), nullInt(d.TrackTotal)
	e.DiscNumber, e.DiscTotal = nullInt(d.DiscNumber), nullInt(d.DiscTotal)
	e.Year = nullInt(d.Year)

	// Keys, sort keys (explicit sort tags unless the value itself was edited) and ids.
	e.TitleKey, e.ArtistKey, e.AlbumKey = fold(e.Title), fold(e.Artist), fold(e.Album)
	e.AlbumArtistKey, e.ComposerKey = fold(e.AlbumArtist), fold(e.Composer)
	sortOf := func(value, tag string, edited bool) string {
		if tag != "" && !edited {
			return explicitSortKey(cleanText(tag, maxText))
		}
		return sortKey(value)
	}
	e.SortTitle = sortOf(e.Title, ft.SortTitle, manTitle)
	e.SortArtist = sortOf(e.Artist, ft.SortArtist, manArtist)
	e.SortAlbum = sortOf(e.Album, ft.SortAlbum, manAlbum)
	aaTag := ft.SortAlbumArtist
	if aaTag == "" && ft.AlbumArtist == "" && !ft.Compilation {
		aaTag = ft.SortArtist // the album artist is the track artist
	}
	e.SortAlbumArtist = sortOf(e.AlbumArtist, aaTag, manAA || (manArtist && ft.AlbumArtist == ""))
	if len(e.Artists) > 0 {
		e.ArtistID = artistIDFor(e.Artists[0])
	} else {
		e.ArtistID = artistIDFor(e.Artist)
	}
	e.AlbumArtistID = artistIDFor(e.AlbumArtist)
	e.AlbumID = albumIDFor(e.AlbumArtist, e.Album)
	return e
}

// normalizeList cleans values, drops empties and case-insensitive duplicates, caps the count
// and the length of each value. Genres are also split on ";".
func normalizeList(in []string, maxItems, maxRunes int, splitSemicolons bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		v = cleanText(v, maxRunes)
		if v == "" || len(out) >= maxItems {
			return
		}
		k := fold(v)
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

// cleanMultiline is cleanText for long text: newlines are kept (CRLF normalised).
func cleanMultiline(s string, max int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), max)
}

// storeEffective writes the effective values of a track with its artist and genre rows.
func storeEffective(ctx context.Context, q db.Querier, fileID string, e effective, updatedAt string) error {
	_, err := q.ExecContext(ctx, `UPDATE music_tracks SET title = ?, sort_title = ?, title_key = ?, artist = ?, artists = ?,
		artist_key = ?, sort_artist = ?, artist_id = ?, album = ?, album_key = ?, sort_album = ?, album_id = ?,
		album_artist = ?, album_artist_key = ?, sort_album_artist = ?, album_artist_id = ?, track_number = ?,
		track_total = ?, disc_number = ?, disc_total = ?, year = ?, genres = ?, composer = ?, composer_key = ?,
		comment = ?, compilation = ?, file_name = ?, updated_at = ? WHERE file_id = ?`,
		e.Title, e.SortTitle, e.TitleKey, e.Artist, db.JSON[[]string]{V: e.Artists}, e.ArtistKey, e.SortArtist, e.ArtistID,
		e.Album, e.AlbumKey, e.SortAlbum, e.AlbumID, e.AlbumArtist, e.AlbumArtistKey, e.SortAlbumArtist, e.AlbumArtistID,
		e.TrackNumber, e.TrackTotal, e.DiscNumber, e.DiscTotal, e.Year, db.JSON[[]string]{V: e.Genres}, e.Composer,
		e.ComposerKey, e.Comment, db.Bool(e.Compilation), e.FileName, updatedAt, fileID)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM music_track_artists WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	type artistRow struct {
		name, sortName string
		role           int
	}
	rows := map[string]*artistRow{}
	var order []string
	addArtist := func(name, sortName string, role int) {
		id := artistIDFor(name)
		if id == "" {
			return
		}
		if r, ok := rows[id]; ok {
			r.role |= role
			return
		}
		rows[id] = &artistRow{name: name, sortName: sortName, role: role}
		order = append(order, id)
	}
	addArtist(e.AlbumArtist, e.SortAlbumArtist, 1)
	for i, a := range e.Artists {
		sk := sortKey(a)
		if i == 0 && len(e.Artists) == 1 {
			sk = e.SortArtist
		}
		addArtist(a, sk, 2)
	}
	for _, id := range order {
		r := rows[id]
		if _, err := q.ExecContext(ctx, `INSERT INTO music_track_artists (file_id, artist_id, name, name_key, sort_name, role)
			VALUES (?, ?, ?, ?, ?, ?)`, fileID, id, r.name, fold(r.name), r.sortName, r.role); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM music_track_genres WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	for i, g := range e.Genres {
		if _, err := q.ExecContext(ctx, `INSERT INTO music_track_genres (file_id, genre_key, name, position) VALUES (?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, fileID, fold(g), g, i); err != nil {
			return err
		}
	}
	return nil
}

// recompute re-derives and stores a track's effective values from its stored file tags,
// manual edits and current path (after a move/rename, a probe or a PATCH).
func (s *Service) recompute(ctx context.Context, q db.Querier, fileID string) error {
	var (
		ft     db.JSON[fileTags]
		manual db.JSON[map[string]json.RawMessage]
		p, n   string
	)
	err := q.QueryRowContext(ctx, `SELECT t.file_tags, t.manual, f.path, f.name FROM music_tracks t CROSS JOIN files f ON f.id = t.file_id
		WHERE t.file_id = ?`, fileID).Scan(&ft, &manual, &p, &n)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return storeEffective(ctx, q, fileID, computeEffective(ft.V, manual.V, p, n), db.FormatTime(s.now()))
}
