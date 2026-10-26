// Package media implements the media library (API.md §6): media metadata attached to
// files, probing (pure-Go parsers and ffprobe), filename/NFO auto-tagging, TMDB
// identification, posters, direct streaming, ffmpeg transcoding, subtitles and per-user
// playback progress.
//
// Data layout under <data>/media (all names are server generated):
//
//	posters/<file id>-<rand>.jpg     normalized custom posters (JPEG, ≤ 1280 px)
//	backdrops/<file id>-<rand>.jpg   backdrops from TMDB (JPEG, ≤ 1920 px)
//	cache/<name>-<size>.jpg          resized poster/backdrop variants
//	subs/<file id>-<stream>-<key>.vtt embedded subtitle tracks extracted by ffmpeg
//
// Every media endpoint resolves the file through files.Service with the caller's principal
// (missing and unreadable are both 404) and additionally requires the file to be a media
// item, so a media:read token can never be used to read arbitrary files.
package media

import (
	"context"
	"embed"
	"io/fs"
	"path/filepath"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed openapi.json
var openAPI []byte

// Settings is the "media" settings namespace (API.md §3). TMDBAPIKey is secret.
type Settings struct {
	FFmpegPath       string `json:"ffmpeg_path"`  // "" = look up on PATH
	FFprobePath      string `json:"ffprobe_path"` // "" = look up on PATH
	TMDBAPIKey       string `json:"tmdb_api_key"`
	AutoProbe        bool   `json:"auto_probe"`
	AutoThumbnails   bool   `json:"auto_thumbnails"`
	TranscodeEnabled bool   `json:"transcode_enabled"` // effective only when ffmpeg is found
}

// DefaultSettings are the namespace defaults.
var DefaultSettings = Settings{AutoProbe: true, AutoThumbnails: true, TranscodeEnabled: true}

func validateSettings(s *Settings) error {
	errs := settings.FieldErrors{}
	for field, p := range map[string]string{"ffmpeg_path": s.FFmpegPath, "ffprobe_path": s.FFprobePath} {
		settings.CheckText(errs, field, p, false, 4096)
		if p != "" && !filepath.IsAbs(p) {
			errs[field] = "must be an absolute path"
		}
	}
	settings.CheckText(errs, "tmdb_api_key", s.TMDBAPIKey, false, 1024)
	return errs.Err()
}

// Module is the media module.
type Module struct {
	d     *core.Deps
	files *files.Service
	svc   *Service
}

// New constructs the module and registers the "media" settings namespace.
func New(d *core.Deps, f *files.Service) *Module {
	d.Settings.Register("media", DefaultSettings, []string{"tmdb_api_key"}, settings.Validator(validateSettings))
	return &Module{d: d, files: f, svc: newService(d, f)}
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "media" }

// Migrations implements httpx.Module.
func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// OpenAPI implements httpx.Module.
func (m *Module) OpenAPI() []byte { return openAPI }

// Service returns the media service (used by the tv module).
func (m *Module) Service() *Service { return m.svc }

// Routes implements httpx.Module.
//
// GET /media/shows/{show_key} and GET /media/{file_id}/poster|stream|transcode would overlap
// in Go's ServeMux ("/media/shows/stream" matches both), so the per-file GET assets share one
// "{file_id}/{asset}" route; the literal "shows" route is more specific and wins.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("media:read"), httpx.Scope("media:write")
	qt := read.WithQueryToken()

	r.Handle("GET /api/v1/media", read, m.list)
	r.Handle("GET /api/v1/media/facets", read, m.facets)
	r.Handle("GET /api/v1/media/shows", read, m.shows)
	r.Handle("GET /api/v1/media/shows/{show_key}", read, m.show)
	r.Handle("GET /api/v1/media/tmdb/search", write, m.tmdbSearch)
	r.Handle("POST /api/v1/media/bulk", write, m.bulk)
	r.Handle("POST /api/v1/media/auto-tag", write, m.autoTag)

	r.Handle("GET /api/v1/media/{file_id}", read, m.get)
	r.Handle("PATCH /api/v1/media/{file_id}", write, m.patch)
	r.Handle("POST /api/v1/media/{file_id}/probe", write, m.probe)
	r.Handle("POST /api/v1/media/{file_id}/identify", write, m.identify)
	r.Handle("PUT /api/v1/media/{file_id}/poster", write.WithMaxBody(maxPosterUpload), m.putPoster)
	r.Handle("DELETE /api/v1/media/{file_id}/poster", write, m.deletePoster)
	r.Handle("DELETE /api/v1/media/{file_id}/backdrop", write, m.deleteBackdrop)
	r.Handle("PUT /api/v1/media/{file_id}/progress", read, m.progress)
	r.Handle("GET /api/v1/media/{file_id}/{asset}", qt, m.asset) // poster | backdrop | stream | transcode
	r.Handle("GET /api/v1/media/{file_id}/subtitles/{name}", qt, m.subtitle)
}

// Start implements httpx.Module: event subscriptions and background jobs.
func (m *Module) Start(ctx context.Context) error {
	s := m.svc
	if err := s.ensureDirs(); err != nil {
		return err
	}
	for _, topic := range []string{events.FileCreated, events.FileUpdated, events.FileDeleted, events.FileMoved} {
		m.d.Events.Subscribe(topic, s.onFileEvent)
	}
	m.d.Settings.OnChange(func(ns string) {
		if ns == "media" {
			// Not GoOnce: a refresh already in flight may have read the old settings.
			m.d.Jobs.Go("media.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
			s.kickProbe()
		}
	})
	m.d.Jobs.GoOnce("media.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.Every("media.tools", 10*time.Minute, func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.GoOnce("media.reconcile", s.reconcile)
	m.d.Jobs.Every("media.probe-sweep", 5*time.Minute, func(context.Context) error { s.kickProbe(); return nil })
	m.d.Jobs.Every("media.gc", 6*time.Hour, s.gc)
	return nil
}
