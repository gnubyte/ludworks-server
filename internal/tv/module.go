// Package tv implements lineups, channels, the schedule engine, the guide and
// M3U/XMLTV/HDHomeRun feeds (API.md §7).
//
// A lineup is an ordered list of program sources (files, shows, seasons, folders, library
// queries) owned by one user. A channel plays a lineup on the deterministic schedule of
// API.md §7.1 (internal/tv/schedule): the lineup is expanded to files with the channel
// owner's read permissions, and viewers of shared channels stream programs through
// channel-scoped URLs that are authorised by channel visibility plus membership of the file
// in the channel's expansion — they need no access to the owner's files.
package tv

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/media"
	"ludworks/internal/settings"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Settings is the "tv" settings namespace (API.md §3).
type Settings struct {
	HDHREnabled    bool   `json:"hdhr_enabled"`
	HDHRDeviceName string `json:"hdhr_device_name"`
	HDHRTunerCount int    `json:"hdhr_tuner_count"`
	GuideDays      int    `json:"guide_days"`
}

// DefaultSettings are the namespace defaults.
var DefaultSettings = Settings{HDHRDeviceName: "Ludworks TV", HDHRTunerCount: 2, GuideDays: 3}

func validateSettings(s *Settings) error {
	errs := settings.FieldErrors{}
	settings.CheckText(errs, "hdhr_device_name", s.HDHRDeviceName, true, 64)
	settings.CheckRange(errs, "hdhr_tuner_count", int64(s.HDHRTunerCount), 1, 16)
	settings.CheckRange(errs, "guide_days", int64(s.GuideDays), 1, 14)
	return errs.Err()
}

// mediaService is the part of *media.Service the tv module uses (an interface so tests can
// observe the delegation). Every method applies the given principal's file permissions.
type mediaService interface {
	ServeStream(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string) error
	ServeTranscode(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, startSeconds float64, height int) error
	ServePoster(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID, size string) error
	ServeSubtitle(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, index int) error
	FFmpegPath(ctx context.Context) string
}

// Module is the tv module.
type Module struct {
	d     *core.Deps
	log   *slog.Logger
	files *files.Service
	media mediaService // nil when no media service was given

	cache      *expCache
	mediaTable atomic.Bool // media_items exists

	now func() time.Time

	// ffmpegPath resolves the ffmpeg binary ("" = unavailable); tests replace it.
	ffmpegPath func(ctx context.Context) string
	ffMu       sync.Mutex
	ffValue    string
	ffChecked  time.Time

	// live.ts tuners: global and per-user concurrency.
	liveMu    sync.Mutex
	liveTotal int
	liveUsers map[string]int

	feedMu sync.Mutex // serialises feed-token creation/rotation
}

// New constructs the module and registers the "tv" settings namespace.
func New(d *core.Deps, f *files.Service, m *media.Service) *Module {
	d.Settings.Register("tv", DefaultSettings, nil, settings.Validator(validateSettings))
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mod := &Module{d: d, log: log.With("module", "tv"), files: f, cache: newExpCache(), now: core.Now,
		liveUsers: map[string]int{}}
	if m != nil {
		mod.media = m
	}
	mod.ffmpegPath = func(ctx context.Context) string {
		if mod.media == nil {
			return ""
		}
		return mod.media.FFmpegPath(ctx)
	}
	return mod
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "tv" }

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

// Routes implements httpx.Module.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("tv:read"), httpx.Scope("tv:write")
	qt := read.WithQueryToken()

	r.Handle("GET /api/v1/tv/lineups", read, m.listLineups)
	r.Handle("POST /api/v1/tv/lineups", write, m.createLineup)
	r.Handle("GET /api/v1/tv/lineups/{id}", read, m.getLineup)
	r.Handle("PATCH /api/v1/tv/lineups/{id}", write, m.updateLineup)
	r.Handle("DELETE /api/v1/tv/lineups/{id}", write, m.deleteLineup)
	r.Handle("PUT /api/v1/tv/lineups/{id}/items", write, m.replaceItems)
	r.Handle("POST /api/v1/tv/lineups/{id}/items", write, m.insertItems)
	r.Handle("DELETE /api/v1/tv/lineups/{id}/items/{item_id}", write, m.deleteItem)
	r.Handle("GET /api/v1/tv/lineups/{id}/expanded", read, m.expandedLineup)

	r.Handle("GET /api/v1/tv/channels", read, m.listChannels)
	r.Handle("POST /api/v1/tv/channels", write, m.createChannel)
	r.Handle("POST /api/v1/tv/channels/auto-generate", write, m.autoGenerate)
	r.Handle("GET /api/v1/tv/channels/{id}", read, m.getChannel)
	r.Handle("PATCH /api/v1/tv/channels/{id}", write, m.updateChannel)
	r.Handle("DELETE /api/v1/tv/channels/{id}", write, m.deleteChannel)
	r.Handle("PUT /api/v1/tv/channels/{id}/logo", write.WithMaxBody(MaxLogoBytes), m.putLogo)
	r.Handle("DELETE /api/v1/tv/channels/{id}/logo", write, m.deleteLogo)
	r.Handle("GET /api/v1/tv/channels/{id}/logo", qt, m.getLogo)
	r.Handle("POST /api/v1/tv/channels/{id}/reshuffle", write, m.reshuffle)
	r.Handle("POST /api/v1/tv/channels/{id}/restart", write, m.restart)
	r.Handle("GET /api/v1/tv/channels/{id}/now", read, m.nowPlaying)
	r.Handle("PUT /api/v1/tv/channels/{id}/position", read, m.setPosition)
	r.Handle("GET /api/v1/tv/channels/{id}/programs/{file_id}/stream", qt, m.programStream)
	r.Handle("GET /api/v1/tv/channels/{id}/programs/{file_id}/transcode", qt, m.programTranscode)
	r.Handle("GET /api/v1/tv/channels/{id}/programs/{file_id}/poster", qt, m.programPoster)
	r.Handle("GET /api/v1/tv/channels/{id}/programs/{file_id}/subtitles/{name}", qt, m.programSubtitle)
	r.Handle("GET /api/v1/tv/channels/{id}/live.ts", qt, m.liveTS)
	r.Handle("GET /api/v1/tv/channels/{id}/play", qt, m.play)
	r.Handle("GET /api/v1/tv/guide", read, m.guide)

	r.Handle("GET /api/v1/tv/feeds", read, m.getFeeds)
	r.Handle("POST /api/v1/tv/feeds/rotate", write, m.rotateFeeds)
	r.Handle("GET /api/v1/tv/channels.m3u", qt, m.m3u)
	r.Handle("GET /api/v1/tv/xmltv.xml", qt, m.xmltv)

	// HDHomeRun emulation: public routes authorised by the feed token in the path.
	pub := httpx.Public()
	r.Handle("GET /api/v1/hdhr/{feed_token}/discover.json", pub, m.hdhrDiscover)
	r.Handle("GET /api/v1/hdhr/{feed_token}/lineup.json", pub, m.hdhrLineup)
	r.Handle("GET /api/v1/hdhr/{feed_token}/lineup_status.json", pub, m.hdhrLineupStatus)
	r.Handle("GET /api/v1/hdhr/{feed_token}/device.xml", pub, m.hdhrDeviceXML)
	r.Handle("POST /api/v1/hdhr/{feed_token}/lineup.post", pub, m.hdhrLineupPost)
}

// Start implements httpx.Module: cache invalidation on file and user events.
func (m *Module) Start(ctx context.Context) error {
	if err := ensurePrivateDir(m.logosDir()); err != nil {
		return err
	}
	inv := func(any) { m.cache.invalidateAll() }
	for _, t := range []string{events.FileCreated, events.FileUpdated, events.FileDeleted, events.FileMoved,
		events.UserDeleted, TopicMediaUpdated} {
		m.d.Events.Subscribe(t, inv)
	}
	// Logos of channels deleted together with their owner (cascade) are swept up.
	m.d.Events.Subscribe(events.UserDeleted, func(any) { m.d.Jobs.GoOnce("tv.logos-gc", m.sweepLogos) })
	m.d.Jobs.Every("tv.logos-gc", 6*time.Hour, m.sweepLogos)
	m.d.Settings.OnChange(func(ns string) {
		if ns == "media" {
			m.ffMu.Lock()
			m.ffChecked = time.Time{}
			m.ffMu.Unlock()
		}
	})
	return nil
}

// TopicMediaUpdated is the event the media module publishes after media metadata
// (durations, titles, posters) changes; tv drops its cached expansions on it.
const TopicMediaUpdated = media.TopicUpdated

// logosDir is <data>/tv/logos.
func (m *Module) logosDir() string { return filepath.Join(m.d.Config.DataDir, "tv", "logos") }

// ffmpeg returns the ffmpeg path ("" = unavailable), cached for 15 s.
func (m *Module) ffmpeg(ctx context.Context) string {
	m.ffMu.Lock()
	defer m.ffMu.Unlock()
	if !m.ffChecked.IsZero() && time.Since(m.ffChecked) < 15*time.Second {
		return m.ffValue
	}
	m.ffValue = m.ffmpegPath(ctx)
	m.ffChecked = time.Now()
	return m.ffValue
}
