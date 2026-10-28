// Package music implements the music library (API.md §12): tracks from audio tags, albums,
// artists, cover art, lyrics, manual and smart playlists, favorites, ratings and plays, audio
// transcodes and offline-download renditions.
//
// Data layout under <data>/music (all names are server generated):
//
//	art/<id>.jpg              cover art: every distinct picture once, re-encoded JPEG ≤ 1200 px
//	                          (id = hash of the source picture)
//	art/cache/<id>-<size>.jpg resized variants (sm 160, md 320, lg 640)
//	art/mosaic/<key>.jpg      generated playlist mosaics
//	cache/<file id>-<quality>-<key>.m4a  download renditions (LRU, music.download_cache_bytes)
//	tmp/                      renditions being encoded
//
// Every /music/tracks/{id} endpoint resolves the file through files.Service with the caller's
// principal (missing and unreadable are both 404) and additionally requires the file to be a
// music item, so a music:read token can never be used to read arbitrary files. Listings cover
// the caller's home plus the libraries they can read.
package music

import (
	"context"
	"embed"
	"io/fs"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/media"
	"ludworks/internal/settings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed openapi.json
var openAPI []byte

// Settings is the "music" settings namespace (API.md §3).
type Settings struct {
	// DownloadCacheBytes bounds the rendition cache; 0 disables transcoded downloads.
	DownloadCacheBytes int64 `json:"download_cache_bytes"`
	// FolderArt uses cover images next to the tracks (cover.jpg, folder.png, …) as album art.
	FolderArt bool `json:"folder_art"`
}

// DefaultSettings are the namespace defaults.
var DefaultSettings = Settings{DownloadCacheBytes: 10 << 30, FolderArt: true}

// maxCacheBytes is the largest accepted download_cache_bytes (16 TiB).
const maxCacheBytes = 1 << 44

func validateSettings(s *Settings) error {
	errs := settings.FieldErrors{}
	settings.CheckRange(errs, "download_cache_bytes", s.DownloadCacheBytes, 0, maxCacheBytes)
	return errs.Err()
}

// Module is the music module.
type Module struct {
	d   *core.Deps
	svc *Service
}

// New constructs the module and registers the "music" settings namespace.
func New(d *core.Deps, f *files.Service, m *media.Service) *Module {
	d.Settings.Register("music", DefaultSettings, nil, settings.Validator(validateSettings))
	return &Module{d: d, svc: newService(d, f, m)}
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "music" }

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

// Service returns the music service (used by the cast module).
func (m *Module) Service() *Service { return m.svc }

// Routes implements httpx.Module.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("music:read"), httpx.Scope("music:write")
	qt := read.WithQueryToken()
	img := write.WithMaxBody(maxArtUpload)

	r.Handle("GET /api/v1/music/tracks", read, m.listTracks)
	r.Handle("POST /api/v1/music/tracks/lookup", read, m.lookupTracks)
	r.Handle("POST /api/v1/music/tracks/bulk", write, m.bulkTracks)
	r.Handle("GET /api/v1/music/tracks/{id}", read, m.getTrack)
	r.Handle("PATCH /api/v1/music/tracks/{id}", write, m.patchTrack)
	r.Handle("POST /api/v1/music/tracks/{id}/probe", write, m.probeTrack)
	r.Handle("GET /api/v1/music/tracks/{id}/stream", qt, m.streamTrack)
	r.Handle("GET /api/v1/music/tracks/{id}/transcode", qt, m.transcodeTrack)
	r.Handle("GET /api/v1/music/tracks/{id}/download", qt, m.downloadTrack)
	r.Handle("GET /api/v1/music/tracks/{id}/art", qt, m.trackArt)
	r.Handle("GET /api/v1/music/tracks/{id}/lyrics", read, m.trackLyrics)
	r.Handle("PUT /api/v1/music/tracks/{id}/rating", read, m.rateTrack)
	r.Handle("PUT /api/v1/music/favorites", read, m.setFavorite)

	r.Handle("GET /api/v1/music/albums", read, m.listAlbums)
	r.Handle("GET /api/v1/music/albums/{id}", read, m.getAlbum)
	r.Handle("GET /api/v1/music/albums/{id}/art", qt, m.albumArt)
	r.Handle("PUT /api/v1/music/albums/{id}/art", img, m.putAlbumArt)
	r.Handle("DELETE /api/v1/music/albums/{id}/art", write, m.deleteAlbumArt)
	r.Handle("GET /api/v1/music/artists", read, m.listArtists)
	r.Handle("GET /api/v1/music/artists/{id}", read, m.getArtist)
	r.Handle("GET /api/v1/music/artists/{id}/art", qt, m.artistArt)
	r.Handle("GET /api/v1/music/genres", read, m.genres)
	r.Handle("GET /api/v1/music/facets", read, m.facets)
	r.Handle("GET /api/v1/music/search", read, m.search)
	r.Handle("GET /api/v1/music/home", read, m.home)

	r.Handle("POST /api/v1/music/plays", read, m.recordPlays)
	r.Handle("GET /api/v1/music/history", read, m.history)
	r.Handle("DELETE /api/v1/music/history", read, m.clearHistory)

	r.Handle("GET /api/v1/music/playlists", read, m.listPlaylists)
	r.Handle("POST /api/v1/music/playlists", write, m.createPlaylist)
	r.Handle("POST /api/v1/music/playlists/import", write.WithMaxBody(maxM3UBytes), m.importPlaylist)
	r.Handle("GET /api/v1/music/playlists/{id}", read, m.getPlaylist)
	r.Handle("PATCH /api/v1/music/playlists/{id}", write, m.patchPlaylist)
	r.Handle("DELETE /api/v1/music/playlists/{id}", write, m.deletePlaylist)
	r.Handle("POST /api/v1/music/playlists/{id}/items", write, m.insertItems)
	r.Handle("PUT /api/v1/music/playlists/{id}/items", write, m.replaceItems)
	r.Handle("POST /api/v1/music/playlists/{id}/items/remove", write, m.removeItems)
	r.Handle("POST /api/v1/music/playlists/{id}/items/move", write, m.moveItems)
	r.Handle("GET /api/v1/music/playlists/{id}/art", qt, m.playlistArt)
	r.Handle("PUT /api/v1/music/playlists/{id}/art", img, m.putPlaylistArt)
	r.Handle("DELETE /api/v1/music/playlists/{id}/art", write, m.deletePlaylistArt)
	r.Handle("POST /api/v1/music/playlists/{id}/duplicate", write, m.duplicatePlaylist)
	r.Handle("GET /api/v1/music/playlists/{id}/export.m3u8", read, m.exportPlaylist)

	r.Handle("POST /api/v1/music/rescan", write, m.rescan)
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
	// Per-user rows go with the user (ON DELETE CASCADE); their images are collected here.
	m.d.Events.Subscribe(events.UserDeleted, func(any) { m.d.Jobs.GoOnce("music.gc", s.gc) })
	m.d.Settings.OnChange(func(ns string) {
		switch ns {
		case "media":
			// Not GoOnce: a refresh already in flight may have read the old settings.
			m.d.Jobs.Go("music.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
		case "music":
			m.d.Jobs.GoOnce("music.cache-trim", func(ctx context.Context) error { return s.trimCache(ctx, 0) })
		}
	})
	m.d.Jobs.GoOnce("music.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.Every("music.tools", 10*time.Minute, func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.GoOnce("music.reconcile", s.reconcile)
	m.d.Jobs.Every("music.probe-sweep", 5*time.Minute, func(context.Context) error {
		s.kickProbe()
		s.kickFolderArt()
		return nil
	})
	m.d.Jobs.Every("music.gc", 6*time.Hour, s.gc)
	return nil
}
