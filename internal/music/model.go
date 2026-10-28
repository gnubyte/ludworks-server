package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
)

// Probe states (music_tracks.probe_state).
const (
	probePending = 0
	probeOK      = 1
	probeFailed  = 2
)

// eligibleSQL selects live files that are music items (table alias f): audio (or Ogg) files
// whose category is empty or music.
const eligibleSQL = `f.trashed_at IS NULL AND f.is_dir = 0 AND (f.mime LIKE 'audio/%' OR f.mime = 'application/ogg')
	AND f.category IN ('', 'music')`

// isMusicFile reports whether a file is a music item (API.md §12).
func isMusicFile(e *files.Entry) bool {
	return !e.IsDir && isMusicMime(e.Mime) && (e.Category == "" || e.Category == "music")
}

func isMusicMime(m string) bool { return strings.HasPrefix(m, "audio/") || m == "application/ogg" }

// track is a music_tracks row joined with its file and the caller's state.
type track struct {
	FileID, SpaceID, Path, Name, Mime, Subcategory string
	Size                                           int64
	ModTime, FileCreated                           db.Time
	Title, Artist                                  string
	Artists                                        []string
	Album, AlbumID, AlbumArtist, ArtistID          string
	TrackNumber, TrackTotal, DiscNumber, DiscTotal sql.NullInt64
	Year                                           sql.NullInt64
	Genres                                         []string
	Composer, Comment                              string
	Compilation                                    bool
	Duration                                       sql.NullFloat64
	Codec, Container                               string
	Bitrate, SampleRate, BitDepth, Channels        sql.NullInt64
	RGTrackGain, RGTrackPeak                       sql.NullFloat64
	RGAlbumGain, RGAlbumPeak                       sql.NullFloat64
	ArtID, CustomArtID, FolderArtID                sql.NullString
	HasLyrics                                      bool
	ProbeState                                     int
	AddedAt, UpdatedAt                             db.Time
	Favorite                                       bool
	Rating                                         sql.NullInt64
	PlayCount                                      int64
	LastPlayed                                     db.NullTime
}

// trackCols / trackFrom select tracks; the first query argument is the caller's user id. The
// joins add the caller's favorite, rating and play counters, the custom album art of the
// track's album in its space and the decoded folder image. CROSS JOIN fixes the join order:
// queries are driven by music_tracks (indexes for album lookups and title order), never by a
// scan of every uncategorized file.
const trackCols = `t.file_id, f.space_id, f.path, f.name, f.mime, f.subcategory, f.size, f.mod_time, f.created_at,
	t.title, t.artist, t.artists, t.album, t.album_id, t.album_artist, t.artist_id, t.track_number, t.track_total,
	t.disc_number, t.disc_total, t.year, t.genres, t.composer, t.comment, t.compilation, t.duration_seconds, t.codec,
	t.container, t.bitrate_kbps, t.sample_rate, t.bit_depth, t.channels, t.rg_track_gain, t.rg_track_peak,
	t.rg_album_gain, t.rg_album_peak, t.art_id, aa.art_id, fa.art_id,
	(t.lyrics IS NOT NULL OR EXISTS (SELECT 1 FROM files lf WHERE lf.id = t.lrc_file_id AND lf.trashed_at IS NULL)),
	t.probe_state, t.added_at, t.updated_at, fv.file_id IS NOT NULL, ra.rating, COALESCE(ps.play_count, 0), ps.last_played_at`

const trackFrom = `FROM music_tracks t
	CROSS JOIN files f ON f.id = t.file_id
	CROSS JOIN (SELECT ? AS uid) me
	LEFT JOIN music_favorite_tracks fv ON fv.user_id = me.uid AND fv.file_id = t.file_id
	LEFT JOIN music_ratings ra ON ra.user_id = me.uid AND ra.file_id = t.file_id
	LEFT JOIN music_play_stats ps ON ps.user_id = me.uid AND ps.file_id = t.file_id
	LEFT JOIN music_album_art aa ON aa.album_id = t.album_id AND aa.space_id = f.space_id
	LEFT JOIN music_folder_art fa ON fa.file_id = t.folder_image_id AND fa.state = 1`

type scanner interface{ Scan(dest ...any) error }

func scanTrack(sc scanner, extra ...any) (*track, error) {
	var (
		tr                  track
		artists, genres     db.JSON[[]string]
		compilation, hasLyr int64
		favorite            int64
	)
	dest := []any{&tr.FileID, &tr.SpaceID, &tr.Path, &tr.Name, &tr.Mime, &tr.Subcategory, &tr.Size, &tr.ModTime,
		&tr.FileCreated, &tr.Title, &tr.Artist, &artists, &tr.Album, &tr.AlbumID, &tr.AlbumArtist, &tr.ArtistID,
		&tr.TrackNumber, &tr.TrackTotal, &tr.DiscNumber, &tr.DiscTotal, &tr.Year, &genres, &tr.Composer, &tr.Comment,
		&compilation, &tr.Duration, &tr.Codec, &tr.Container, &tr.Bitrate, &tr.SampleRate, &tr.BitDepth, &tr.Channels,
		&tr.RGTrackGain, &tr.RGTrackPeak, &tr.RGAlbumGain, &tr.RGAlbumPeak, &tr.ArtID, &tr.CustomArtID, &tr.FolderArtID,
		&hasLyr, &tr.ProbeState, &tr.AddedAt, &tr.UpdatedAt, &favorite, &tr.Rating, &tr.PlayCount, &tr.LastPlayed}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	tr.Artists, tr.Genres = artists.V, genres.V
	if tr.Artists == nil {
		tr.Artists = []string{}
	}
	if tr.Genres == nil {
		tr.Genres = []string{}
	}
	tr.Compilation, tr.HasLyrics, tr.Favorite = compilation == 1, hasLyr == 1, favorite == 1
	return &tr, nil
}

func queryTracks(ctx context.Context, q db.Querier, query string, args ...any) ([]*track, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*track
	for rs.Next() {
		tr, err := scanTrack(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rs.Err()
}

// loadTrack loads one track for userID without any permission check (nil when absent).
func loadTrack(ctx context.Context, q db.Querier, userID, fileID string) (*track, error) {
	trs, err := queryTracks(ctx, q, `SELECT `+trackCols+` `+trackFrom+` WHERE t.file_id = ?`, userID, fileID)
	if err != nil || len(trs) == 0 {
		return nil, err
	}
	return trs[0], nil
}

// loadTracksByID loads tracks in the given order (missing ids are skipped), without
// permission checks.
func loadTracksByID(ctx context.Context, q db.Querier, userID string, ids []string) ([]*track, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(ids)
	trs, err := queryTracks(ctx, q, `SELECT `+trackCols+` `+trackFrom+` WHERE t.file_id IN (SELECT value FROM json_each(?))`,
		userID, string(b))
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*track, len(trs))
	for _, tr := range trs {
		byID[tr.FileID] = tr
	}
	out := make([]*track, 0, len(ids))
	for _, id := range ids {
		if tr, ok := byID[id]; ok {
			out = append(out, tr)
		}
	}
	return out, nil
}

// access levels needed by an operation.
const (
	needRead = iota
	needWrite
)

// trackFor resolves a track for p: the file must be readable (404 otherwise; 403 when write is
// needed and the space is read-only), live, and a music item.
func (s *Service) trackFor(ctx context.Context, p *core.Principal, fileID string, need int) (*track, error) {
	if !httpx.ValidID(fileID) {
		return nil, errNotTrack
	}
	e, err := s.files.Get(ctx, p, fileID)
	if err != nil {
		return nil, err
	}
	if !isMusicFile(e) {
		return nil, errNotTrack
	}
	if need == needWrite && !s.files.CanWrite(ctx, p, e.SpaceID) {
		return nil, core.Forbidden("You only have read access to this space")
	}
	tr, err := loadTrack(ctx, s.d.DB.SQL, p.UserID, fileID)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		// Rows are created asynchronously from file events; create it now.
		if err := s.ensureRows(ctx, []string{fileID}); err != nil {
			return nil, err
		}
		if tr, err = loadTrack(ctx, s.d.DB.SQL, p.UserID, fileID); err != nil {
			return nil, err
		}
		if tr == nil {
			return nil, errNotTrack
		}
	}
	return tr, nil
}

/* ------------------------------------------------------------------ JSON shapes */

// ReplayGain is the replaygain object of a Track.
type ReplayGain struct {
	TrackGainDB float64  `json:"track_gain_db"`
	TrackPeak   float64  `json:"track_peak"`
	AlbumGainDB *float64 `json:"album_gain_db"`
	AlbumPeak   *float64 `json:"album_peak"`
}

// Track is the Track object of API.md §12.
type Track struct {
	ID              string      `json:"id"`
	SpaceID         string      `json:"space_id"`
	Path            string      `json:"path"`
	Name            string      `json:"name"`
	Size            int64       `json:"size"`
	Mime            string      `json:"mime"`
	Title           string      `json:"title"`
	Artist          string      `json:"artist"`
	Artists         []string    `json:"artists"`
	Album           string      `json:"album"`
	AlbumID         string      `json:"album_id"`
	AlbumArtist     string      `json:"album_artist"`
	ArtistID        string      `json:"artist_id"`
	TrackNumber     *int64      `json:"track_number"`
	TrackTotal      *int64      `json:"track_total"`
	DiscNumber      *int64      `json:"disc_number"`
	DiscTotal       *int64      `json:"disc_total"`
	Year            *int64      `json:"year"`
	Genres          []string    `json:"genres"`
	Composer        string      `json:"composer"`
	Comment         string      `json:"comment"`
	Compilation     bool        `json:"compilation"`
	Subcategory     string      `json:"subcategory"`
	DurationSeconds *float64    `json:"duration_seconds"`
	Codec           string      `json:"codec"`
	Container       string      `json:"container"`
	BitrateKbps     *int64      `json:"bitrate_kbps"`
	SampleRate      *int64      `json:"sample_rate"`
	BitDepth        *int64      `json:"bit_depth"`
	Channels        *int64      `json:"channels"`
	ReplayGain      *ReplayGain `json:"replaygain"`
	HasArt          bool        `json:"has_art"`
	ArtURL          *string     `json:"art_url"`
	HasLyrics       bool        `json:"has_lyrics"`
	Favorite        bool        `json:"favorite"`
	Rating          *int64      `json:"rating"`
	PlayCount       int64       `json:"play_count"`
	LastPlayedAt    *string     `json:"last_played_at"`
	AddedAt         string      `json:"added_at"`
	UpdatedAt       string      `json:"updated_at"`
	ProbeStatus     string      `json:"probe_status"`
}

func intPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func floatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}

func strPtr(s string) *string { return &s }

// effectiveArt returns the art id a track shows: custom album art, else the embedded picture,
// else (folderArt) the folder image.
func (tr *track) effectiveArt(folderArt bool) string {
	switch {
	case tr.CustomArtID.Valid && tr.CustomArtID.String != "":
		return tr.CustomArtID.String
	case tr.ArtID.Valid && tr.ArtID.String != "":
		return tr.ArtID.String
	case folderArt && tr.FolderArtID.Valid && tr.FolderArtID.String != "":
		return tr.FolderArtID.String
	}
	return ""
}

// artVersion is the cache-busting ?v= token of an art id.
func artVersion(artID string) string {
	if len(artID) > 12 {
		return artID[:12]
	}
	return artID
}

func (tr *track) toAPI(folderArt bool) Track {
	out := Track{
		ID: tr.FileID, SpaceID: tr.SpaceID, Path: tr.Path, Name: tr.Name, Size: tr.Size, Mime: tr.Mime,
		Title: tr.Title, Artist: tr.Artist, Artists: tr.Artists, Album: tr.Album, AlbumID: tr.AlbumID,
		AlbumArtist: tr.AlbumArtist, ArtistID: tr.ArtistID, TrackNumber: intPtr(tr.TrackNumber),
		TrackTotal: intPtr(tr.TrackTotal), DiscNumber: intPtr(tr.DiscNumber), DiscTotal: intPtr(tr.DiscTotal),
		Year: intPtr(tr.Year), Genres: tr.Genres, Composer: tr.Composer, Comment: tr.Comment, Compilation: tr.Compilation,
		Subcategory: tr.Subcategory, DurationSeconds: floatPtr(tr.Duration), Codec: tr.Codec, Container: tr.Container,
		BitrateKbps: intPtr(tr.Bitrate), SampleRate: intPtr(tr.SampleRate), BitDepth: intPtr(tr.BitDepth),
		Channels: intPtr(tr.Channels), HasLyrics: tr.HasLyrics, Favorite: tr.Favorite, Rating: intPtr(tr.Rating),
		PlayCount: tr.PlayCount, AddedAt: db.FormatTime(tr.AddedAt.Time), UpdatedAt: db.FormatTime(tr.UpdatedAt.Time),
	}
	if tr.RGTrackGain.Valid {
		rg := &ReplayGain{TrackGainDB: tr.RGTrackGain.Float64, TrackPeak: 1, AlbumGainDB: floatPtr(tr.RGAlbumGain),
			AlbumPeak: floatPtr(tr.RGAlbumPeak)}
		if tr.RGTrackPeak.Valid {
			rg.TrackPeak = tr.RGTrackPeak.Float64
		}
		out.ReplayGain = rg
	}
	if art := tr.effectiveArt(folderArt); art != "" {
		out.HasArt = true
		out.ArtURL = strPtr("/api/v1/music/tracks/" + tr.FileID + "/art?v=" + artVersion(art))
	}
	if tr.LastPlayed.Valid {
		out.LastPlayedAt = strPtr(db.FormatTime(tr.LastPlayed.Time))
	}
	switch tr.ProbeState {
	case probeOK:
		out.ProbeStatus = "ok"
	case probeFailed:
		out.ProbeStatus = "failed"
	default:
		out.ProbeStatus = "pending"
	}
	return out
}

// renderTracks converts rows to API objects.
func (s *Service) renderTracks(ctx context.Context, trs []*track) []Track {
	folderArt := s.settings(ctx).FolderArt
	out := make([]Track, 0, len(trs))
	for _, tr := range trs {
		out = append(out, tr.toAPI(folderArt))
	}
	return out
}

// trackJSON loads and renders one track for p (permission already checked by the caller).
func (s *Service) trackJSON(ctx context.Context, p *core.Principal, fileID string) (Track, error) {
	tr, err := loadTrack(ctx, s.d.DB.SQL, p.UserID, fileID)
	if err != nil {
		return Track{}, err
	}
	if tr == nil {
		return Track{}, errNotTrack
	}
	return tr.toAPI(s.settings(ctx).FolderArt), nil
}
