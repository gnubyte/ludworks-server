package media

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"ludworks/internal/core"
)

// Cross-module API for the cast module (API.md §14 "media" items). Every method resolves the
// item with the given principal, so unreadable files and files that are not media items are
// 404 exactly as on the HTTP routes.

// PlaybackInfo is what a cast session needs to play a media item.
type PlaybackInfo struct {
	FileID string
	// Title is the item title, else the file name without its extension.
	Title           string
	ShowTitle       string
	EpisodeTitle    string
	Season, Episode int // 0 = unknown
	Year            int // 0 = unknown
	DurationSeconds float64
	Mime            string
	VideoCodec      string
	AudioCodec      string
	Container       string
	BrowserPlayable bool
	// PosterURL is the browser URL of the poster (an API path with a cache-busting ?v=; append
	// &size=), "" when the item has neither a poster nor a thumbnail.
	PosterURL string
	// PositionSeconds and Watched are the principal's own progress.
	PositionSeconds float64
	Watched         bool
	// Subtitles are the item's subtitle tracks; Format "embedded" tracks need ffmpeg.
	Subtitles []Subtitle
}

// PlaybackInfo returns the playback metadata of media item fileID for p.
func (s *Service) PlaybackInfo(ctx context.Context, p *core.Principal, fileID string) (*PlaybackInfo, error) {
	it, err := s.itemFor(ctx, p, fileID, needRead)
	if err != nil {
		return nil, err
	}
	m := it.toAPI(s.videoThumbs(ctx)).Media
	out := &PlaybackInfo{
		FileID: it.FileID, Title: strings.TrimSpace(it.Title), ShowTitle: it.ShowTitle, EpisodeTitle: it.EpisodeTitle,
		Mime: it.File.Mime, VideoCodec: it.VideoCodec, AudioCodec: it.AudioCodec, Container: it.Container,
		BrowserPlayable: it.BrowserPlayable, PositionSeconds: it.Position, Watched: it.Completed, Subtitles: m.Subtitles,
	}
	if out.Title == "" {
		out.Title = strings.TrimSuffix(it.File.Name, path.Ext(it.File.Name))
		if out.Title == "" {
			out.Title = it.File.Name
		}
	}
	if it.Season.Valid {
		out.Season = int(it.Season.Int64)
	}
	if it.Episode.Valid {
		out.Episode = int(it.Episode.Int64)
	}
	if it.Year.Valid {
		out.Year = int(it.Year.Int64)
	}
	if it.Duration.Valid && it.Duration.Float64 > 0 {
		out.DurationSeconds = it.Duration.Float64
	}
	if m.PosterURL != nil {
		out.PosterURL = *m.PosterURL
	}
	return out, nil
}

// RecordProgress records p's playback position of fileID with the PUT /media/{id}/progress
// rules: completed true marks the item watched (and resets the position), false unmarks it,
// nil lets the position decide (watched from 90% of the duration on).
func (s *Service) RecordProgress(ctx context.Context, p *core.Principal, fileID string, positionSeconds float64, completed *bool) error {
	return s.setProgress(ctx, p, fileID, ProgressUpdate{PositionSeconds: &positionSeconds, Completed: completed})
}

// maxPosterJPEG bounds the poster bytes PosterJPEG returns (posters are re-encoded JPEGs of at
// most 640 px, far below this).
const maxPosterJPEG = 4 << 20

// PosterJPEG returns the poster (else the thumbnail) of media item fileID at size sm|md|lg as
// JPEG bytes, for senders that push artwork themselves; nil, nil when the item has none.
func (s *Service) PosterJPEG(ctx context.Context, p *core.Principal, fileID, size string) ([]byte, error) {
	if size == "" {
		size = "md"
	}
	if _, ok := posterSizes[size]; !ok {
		return nil, core.Validation(map[string]string{"size": "must be sm, md or lg"})
	}
	it, err := s.itemFor(ctx, p, fileID, needRead)
	if err != nil {
		return nil, err
	}
	var f *os.File
	if it.PosterPath.Valid {
		f, _, err = s.imageVariant(ctx, it.PosterPath.String, kindPoster, size)
	}
	if f == nil {
		var abs string
		if abs, _, err = s.files.ThumbnailFile(ctx, p, fileID, size); err == nil {
			f, err = os.Open(abs)
		}
	}
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status == 404 || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPosterJPEG+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPosterJPEG {
		return nil, nil
	}
	return b, nil
}
