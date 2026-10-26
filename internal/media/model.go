package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
)

// Meta sources (media_items.meta_source).
const (
	srcFilename = "filename" // guessed from the file name at creation
	srcAutotag  = "autotag"
	srcManual   = "manual"
	srcTMDB     = "tmdb"
)

// Probe states (media_items.probe_state).
const (
	probePending = 0
	probeOK      = 1
	probeFailed  = 2
)

// eligibleSQL selects files that are media items: category movie/tv or a video MIME type.
const eligibleSQL = `(f.category IN ('movie', 'tv') OR f.mime LIKE 'video/%')`

// storedSub is one subtitle track as stored in media_items.subtitles. Its index in the
// array is the API index.
type storedSub struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	Format   string `json:"format"` // srt | vtt | ass | embedded
	Codec    string `json:"codec,omitempty"`
	FileID   string `json:"file_id,omitempty"` // sidecar file
	Stream   *int   `json:"stream,omitempty"`  // embedded: ordinal among the subtitle streams
	Forced   bool   `json:"forced,omitempty"`
}

// item is a media_items row joined with its file and the caller's progress.
type item struct {
	FileID          string
	Title           string
	SortTitle       string
	Year            sql.NullInt64
	Description     string
	Genres          []string
	ContentRating   string
	ShowTitle       string
	ShowKey         string
	Season          sql.NullInt64
	Episode         sql.NullInt64
	EpisodeTitle    string
	Duration        sql.NullFloat64
	Width, Height   sql.NullInt64
	VideoCodec      string
	AudioCodec      string
	Container       string
	BrowserPlayable bool
	PosterPath      sql.NullString
	BackdropPath    sql.NullString
	TMDBID          sql.NullInt64
	IMDBID          string
	Subtitles       []storedSub
	ProbedAt        db.NullTime
	AddedAt         time.Time
	UpdatedAt       time.Time
	FileName        string
	ShowDescription string
	MetaSource      string
	ProbeState      int

	File       files.Entry
	ThumbState int

	Position   float64
	Completed  bool
	ProgressAt db.NullTime
}

// itemCols / itemFrom select an item; the first query argument is the caller's user id
// (for the progress join).
const itemCols = `m.file_id, m.title, m.sort_title, m.year, m.description, m.genres, m.content_rating, m.show_title,
	m.show_key, m.season, m.episode, m.episode_title, m.duration_seconds, m.width, m.height, m.video_codec,
	m.audio_codec, m.container, m.browser_playable, m.poster_path, m.backdrop_path, m.tmdb_id, m.imdb_id,
	m.subtitles, m.probed_at, m.added_at, m.updated_at, m.file_name, m.show_description, m.meta_source, m.probe_state,
	f.space_id, f.parent_id, f.path, f.name, f.is_dir, f.size, f.mime, f.mod_time, f.category, f.subcategory, f.tags,
	f.sha256, f.owner_id, f.created_at, f.updated_at, f.thumb_state,
	COALESCE(p.position_seconds, 0), COALESCE(p.completed, 0), p.updated_at`

const itemFrom = `FROM media_items m
	JOIN files f ON f.id = m.file_id
	LEFT JOIN media_progress p ON p.file_id = m.file_id AND p.user_id = ?`

type scanner interface{ Scan(dest ...any) error }

func scanItem(sc scanner) (*item, error) {
	var (
		it        item
		genres    db.JSON[[]string]
		subs      db.JSON[[]storedSub]
		tags      db.JSON[[]string]
		added     db.Time
		updated   db.Time
		parentID  sql.NullString
		modTime   db.Time
		fCreated  db.Time
		fUpdated  db.Time
		bp, isDir int64
		completed int64
	)
	err := sc.Scan(&it.FileID, &it.Title, &it.SortTitle, &it.Year, &it.Description, &genres, &it.ContentRating,
		&it.ShowTitle, &it.ShowKey, &it.Season, &it.Episode, &it.EpisodeTitle, &it.Duration, &it.Width, &it.Height,
		&it.VideoCodec, &it.AudioCodec, &it.Container, &bp, &it.PosterPath, &it.BackdropPath, &it.TMDBID, &it.IMDBID,
		&subs, &it.ProbedAt, &added, &updated, &it.FileName, &it.ShowDescription, &it.MetaSource, &it.ProbeState,
		&it.File.SpaceID, &parentID, &it.File.Path, &it.File.Name, &isDir, &it.File.Size, &it.File.Mime, &modTime,
		&it.File.Category, &it.File.Subcategory, &tags, &it.File.SHA256, &it.File.OwnerID, &fCreated, &fUpdated,
		&it.ThumbState, &it.Position, &completed, &it.ProgressAt)
	if err != nil {
		return nil, err
	}
	it.Genres = genres.V
	if it.Genres == nil {
		it.Genres = []string{}
	}
	it.Subtitles = subs.V
	it.BrowserPlayable = bp == 1
	it.AddedAt, it.UpdatedAt = added.Time, updated.Time
	it.Completed = completed == 1
	it.File.ID = it.FileID
	it.File.ParentID = parentID.String
	it.File.IsDir = isDir == 1
	it.File.ModTime = modTime.Time
	it.File.Tags = tags.V
	if it.File.Tags == nil {
		it.File.Tags = []string{}
	}
	it.File.CreatedAt, it.File.UpdatedAt = fCreated.Time, fUpdated.Time
	return &it, nil
}

func queryItems(ctx context.Context, q db.Querier, query string, args ...any) ([]*item, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*item
	for rs.Next() {
		it, err := scanItem(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rs.Err()
}

// loadItem loads one item for userID without any permission check (nil when absent).
func loadItem(ctx context.Context, q db.Querier, userID, fileID string) (*item, error) {
	items, err := queryItems(ctx, q, `SELECT `+itemCols+` `+itemFrom+` WHERE m.file_id = ?`, userID, fileID)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return items[0], nil
}

// access levels needed by an operation.
const (
	needRead = iota
	needWrite
)

// itemFor resolves a media item for p: the file must be readable (404 otherwise; 403 when
// write is needed and the space is read-only), live, and a media item.
func (s *Service) itemFor(ctx context.Context, p *core.Principal, fileID string, need int) (*item, error) {
	if !httpx.ValidID(fileID) {
		return nil, errNotMedia
	}
	e, err := s.files.Get(ctx, p, fileID)
	if err != nil {
		return nil, err
	}
	if e.IsDir {
		return nil, errNotMedia
	}
	if need == needWrite && !s.files.CanWrite(ctx, p, e.SpaceID) {
		return nil, core.Forbidden("You only have read access to this space")
	}
	if !isEligible(e) {
		return nil, errNotMedia
	}
	it, err := loadItem(ctx, s.d.DB.SQL, p.UserID, fileID)
	if err != nil {
		return nil, err
	}
	if it == nil {
		// The row is created asynchronously from file events; create it now.
		if err := s.ensureRows(ctx, []string{fileID}); err != nil {
			return nil, err
		}
		if it, err = loadItem(ctx, s.d.DB.SQL, p.UserID, fileID); err != nil {
			return nil, err
		}
		if it == nil {
			return nil, errNotMedia
		}
	}
	return it, nil
}

// isEligible reports whether a file is a media item.
func isEligible(e *files.Entry) bool {
	return !e.IsDir && (e.Category == "movie" || e.Category == "tv" || strings.HasPrefix(e.Mime, "video/"))
}

/* ------------------------------------------------------------------ JSON shapes */

// Subtitle is a subtitle track in the API.
type Subtitle struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Label    string `json:"label"`
	Format   string `json:"format"`
}

// Info is the "media" object of API.md §6.
type Info struct {
	Title           string     `json:"title"`
	SortTitle       string     `json:"sort_title"`
	Year            *int64     `json:"year"`
	Description     string     `json:"description"`
	Genres          []string   `json:"genres"`
	ContentRating   string     `json:"content_rating"`
	ShowTitle       string     `json:"show_title"`
	ShowKey         string     `json:"show_key"`
	Season          *int64     `json:"season"`
	Episode         *int64     `json:"episode"`
	EpisodeTitle    string     `json:"episode_title"`
	DurationSeconds *float64   `json:"duration_seconds"`
	Width           *int64     `json:"width"`
	Height          *int64     `json:"height"`
	VideoCodec      string     `json:"video_codec"`
	AudioCodec      string     `json:"audio_codec"`
	Container       string     `json:"container"`
	BrowserPlayable bool       `json:"browser_playable"`
	PosterURL       *string    `json:"poster_url"`
	BackdropURL     *string    `json:"backdrop_url"`
	TMDBID          *int64     `json:"tmdb_id"`
	IMDBID          string     `json:"imdb_id"`
	AddedAt         string     `json:"added_at"`
	Watched         bool       `json:"watched"`
	PositionSeconds float64    `json:"position_seconds"`
	LastWatchedAt   *string    `json:"last_watched_at"`
	ProbeStatus     string     `json:"probe_status"`
	Subtitles       []Subtitle `json:"subtitles"`
}

// Media is the Media object of API.md §6: Entry fields plus "media".
type Media struct {
	files.APIEntry
	Media Info `json:"media"`
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func strPtr(s string) *string { return &s }

// posterURL is the item's poster URL (custom poster, else thumbnail), or nil.
// assetVersion is a short cache-busting token for image URLs: it changes whenever the
// underlying image changes, so browsers never show a stale poster from their image cache.
func assetVersion(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:5])
}

// posterURL is the item's poster URL (custom poster, else thumbnail) with a version query,
// or nil when neither exists.
func posterURL(it *item, video bool) *string {
	switch {
	case it.PosterPath.Valid:
		return strPtr("/api/v1/media/" + it.FileID + "/poster?v=" + assetVersion(it.PosterPath.String))
	case hasThumbnail(&it.File, it.ThumbState, video):
		return strPtr("/api/v1/media/" + it.FileID + "/poster?v=" + assetVersion("t", db.FormatTime(it.File.ModTime), strconv.FormatInt(it.File.Size, 10)))
	}
	return nil
}

func (it *item) toAPI(video bool) Media {
	e := &it.File
	out := Media{APIEntry: files.APIEntry{
		ID: e.ID, SpaceID: e.SpaceID, Path: e.Path, Name: e.Name, IsDir: e.IsDir, Size: e.Size, Mime: e.Mime,
		ModTime: db.FormatTime(e.ModTime), Category: e.Category, Subcategory: e.Subcategory, Tags: e.Tags,
		SHA256: e.SHA256, OwnerID: e.OwnerID, CreatedAt: db.FormatTime(e.CreatedAt), UpdatedAt: db.FormatTime(e.UpdatedAt),
		HasThumbnail: hasThumbnail(e, it.ThumbState, video),
	}}
	if e.ParentID != "" {
		pid := e.ParentID
		out.ParentID = &pid
	}
	mi := Info{
		Title: it.Title, SortTitle: it.SortTitle, Year: nullInt(it.Year), Description: it.Description,
		Genres: it.Genres, ContentRating: it.ContentRating, ShowTitle: it.ShowTitle, ShowKey: it.ShowKey,
		Season: nullInt(it.Season), Episode: nullInt(it.Episode), EpisodeTitle: it.EpisodeTitle,
		Width: nullInt(it.Width), Height: nullInt(it.Height), VideoCodec: it.VideoCodec, AudioCodec: it.AudioCodec,
		Container: it.Container, BrowserPlayable: it.BrowserPlayable, PosterURL: posterURL(it, video),
		TMDBID: nullInt(it.TMDBID), IMDBID: it.IMDBID, AddedAt: db.FormatTime(it.AddedAt),
		Watched: it.Completed, PositionSeconds: it.Position, Subtitles: make([]Subtitle, 0, len(it.Subtitles)),
	}
	if it.Duration.Valid {
		d := it.Duration.Float64
		mi.DurationSeconds = &d
	}
	if it.BackdropPath.Valid {
		mi.BackdropURL = strPtr("/api/v1/media/" + it.FileID + "/backdrop?v=" + assetVersion(it.BackdropPath.String))
	}
	if it.ProgressAt.Valid {
		mi.LastWatchedAt = strPtr(db.FormatTime(it.ProgressAt.Time))
	}
	switch it.ProbeState {
	case probeOK:
		mi.ProbeStatus = "ok"
	case probeFailed:
		mi.ProbeStatus = "failed"
	default:
		mi.ProbeStatus = "pending"
	}
	for i, sub := range it.Subtitles {
		mi.Subtitles = append(mi.Subtitles, Subtitle{Index: i, Language: sub.Language, Label: sub.Label, Format: sub.Format})
	}
	out.Media = mi
	return out
}

// mediaJSON loads and renders one item for p (permission already checked by the caller).
func (s *Service) mediaJSON(ctx context.Context, p *core.Principal, fileID string) (Media, error) {
	it, err := loadItem(ctx, s.d.DB.SQL, p.UserID, fileID)
	if err != nil {
		return Media{}, err
	}
	if it == nil {
		return Media{}, errNotMedia
	}
	return it.toAPI(s.videoThumbs(ctx)), nil
}

func (s *Service) renderItems(ctx context.Context, items []*item) []Media {
	video := s.videoThumbs(ctx)
	out := make([]Media, 0, len(items))
	for _, it := range items {
		out = append(out, it.toAPI(video))
	}
	return out
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
