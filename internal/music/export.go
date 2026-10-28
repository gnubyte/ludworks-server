package music

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ludworks/internal/core"
)

// Cross-module API used by the cast module (API.md §14). Every method resolves the track with
// the given principal: unreadable files and files that are not music items are 404.

// TrackInfo is the playback metadata of a track.
type TrackInfo struct {
	ID, Title, Artist, Album string
	Mime, Codec, Container   string
	DurationSeconds          float64 // 0 = unknown
	HasArt                   bool
}

// TrackInfo returns the playback metadata of track id.
func (s *Service) TrackInfo(ctx context.Context, p *core.Principal, id string) (*TrackInfo, error) {
	tr, err := s.trackFor(ctx, p, id, needRead)
	if err != nil {
		return nil, err
	}
	info := &TrackInfo{ID: tr.FileID, Title: tr.Title, Artist: tr.Artist, Album: tr.Album, Mime: tr.Mime, Codec: tr.Codec,
		Container: tr.Container, HasArt: tr.effectiveArt(s.settings(ctx).FolderArt) != ""}
	if tr.Duration.Valid {
		info.DurationSeconds = tr.Duration.Float64
	}
	return info, nil
}

// ServeStream writes the track's bytes (Range/If-Range/ETag, inline).
func (s *Service) ServeStream(w http.ResponseWriter, r *http.Request, p *core.Principal, id string) error {
	tr, err := s.trackFor(r.Context(), p, id, needRead)
	if err != nil {
		return err
	}
	return s.serveFile(w, r, p, tr, true, "")
}

// ServeTranscode streams a live ffmpeg transcode (format "aac" | "mp3" | "opus", bitrate in
// kbps, 0 = default) from startSeconds, charged to the requesting principal p.
func (s *Service) ServeTranscode(w http.ResponseWriter, r *http.Request, p *core.Principal, id, format string, bitrateKbps int, startSeconds float64) error {
	format, bitrateKbps, err := parseTranscode(format, bitrateKbps, startSeconds)
	if err != nil {
		return err
	}
	tr, err := s.trackFor(r.Context(), p, id, needRead)
	if err != nil {
		return err
	}
	return s.serveTranscode(w, r, p, tr, format, bitrateKbps, startSeconds)
}

// ServeArt writes the track's cover art (size "sm" | "md" | "lg" | "xl"); 404 when it has none.
func (s *Service) ServeArt(w http.ResponseWriter, r *http.Request, p *core.Principal, id, size string) error {
	if _, _, err := parseArtSize(size); err != nil {
		return err
	}
	tr, err := s.trackFor(r.Context(), p, id, needRead)
	if err != nil {
		return err
	}
	return s.serveArt(w, r, tr.effectiveArt(s.settings(r.Context()).FolderArt), size)
}

// ArtJPEG returns the track's cover art as JPEG bytes (for senders that push art themselves,
// e.g. AirPlay); nil, nil when it has none.
func (s *Service) ArtJPEG(ctx context.Context, p *core.Principal, id, size string) ([]byte, error) {
	if _, _, err := parseArtSize(size); err != nil {
		return nil, err
	}
	tr, err := s.trackFor(ctx, p, id, needRead)
	if err != nil {
		return nil, err
	}
	art := tr.effectiveArt(s.settings(ctx).FolderArt)
	if art == "" {
		return nil, nil
	}
	b, err := s.readArt(ctx, art, size)
	if errors.Is(err, errNoArt) {
		return nil, nil
	}
	return b, err
}

// RecordPlay records a play of track id for p with the POST /music/plays rules.
func (s *Service) RecordPlay(ctx context.Context, p *core.Principal, id string, playedAt time.Time, secondsPlayed float64, completed bool) error {
	if _, err := s.trackFor(ctx, p, id, needRead); err != nil {
		return err
	}
	if secondsPlayed < 0 || secondsPlayed != secondsPlayed || secondsPlayed > 1e7 {
		secondsPlayed = 0
	}
	_, _, err := s.recordPlays(ctx, p, []play{{trackID: id, at: playedAt.UTC().Truncate(time.Millisecond),
		seconds: secondsPlayed, completed: completed}})
	return err
}
