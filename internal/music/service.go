package music

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/media"
	"ludworks/internal/media/probe"
)

// Service is the music service: it backs the HTTP handlers and the cross-module API in
// export.go (used by the cast module).
type Service struct {
	d     *core.Deps
	files *files.Service
	media *media.Service // ffmpeg discovery (media settings)
	log   *slog.Logger
	dir   string // <data>/music

	rootOnce sync.Once
	root     *os.Root
	rootErr  error

	toolsMu   sync.RWMutex
	ffprobe   string
	refreshMu sync.Mutex // serialises refreshTools so the last run sees the latest settings
	// detect resolves ffmpeg/ffprobe from the media settings (replaceable in tests).
	detect func(ffmpegSetting, ffprobeSetting string) (ffmpeg, ffprobe, version string)

	transcodes *slots // live audio transcodes: maxTranscodes server-wide, maxTranscodesPerUser each
	probes     *slots // manual POST /probe requests (the §6 limits)
	// transcodeWait and transcodeWriteTimeout are fields so tests can shorten them.
	transcodeWait         time.Duration
	transcodeWriteTimeout time.Duration
	imgSem                chan struct{} // concurrent picture decodes started by this module

	probeDirty  atomic.Bool
	folderDirty atomic.Bool
	eventMu     sync.Mutex // serialises file-event handling and reconciliation

	render renderQueue

	now func() time.Time
	// sys is the internal principal of background work (probing, ingest). It has the admin
	// role so files.Service grants it access to every space; it never reaches HTTP.
	sys *core.Principal
}

func newService(d *core.Deps, f *files.Service, m *media.Service) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		d: d, files: f, media: m, log: log.With("module", "music"),
		dir:                   filepath.Join(d.Config.DataDir, "music"),
		detect:                probe.DetectTools,
		transcodes:            newSlots(maxTranscodes, maxTranscodesPerUser),
		probes:                newSlots(maxManualProbes, maxManualProbesPerUser),
		transcodeWait:         transcodeSlotWait,
		transcodeWriteTimeout: transcodeWriteTimeout,
		imgSem:                make(chan struct{}, maxImageWorkers),
		render:                newRenderQueue(),
		now:                   core.Now,
		sys: &core.Principal{UserID: "music-system", Username: "system", Role: core.RoleAdmin,
			Scopes: []string{core.ScopeAll}, Kind: "system"},
	}
}

// ensureDirs creates <data>/music and its subdirectories (0700).
func (s *Service) ensureDirs() error {
	for _, sub := range []string{"art", filepath.Join("art", "cache"), filepath.Join("art", "mosaic"), "cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(s.dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// musicRoot is an os.Root on <data>/music; all art and cache file access goes through it.
func (s *Service) musicRoot() (*os.Root, error) {
	s.rootOnce.Do(func() {
		if err := s.ensureDirs(); err != nil {
			s.rootErr = err
			return
		}
		s.root, s.rootErr = os.OpenRoot(s.dir)
	})
	return s.root, s.rootErr
}

// settings returns the current music settings (defaults on error).
func (s *Service) settings(ctx context.Context) Settings {
	st := DefaultSettings
	if err := s.d.Settings.Get(ctx, "music", &st); err != nil {
		s.log.Warn("read music settings", "err", err)
		return DefaultSettings
	}
	return st
}

// mediaTools are the tool paths of the "media" namespace (the music module has none of its own).
type mediaTools struct {
	FFmpegPath  string `json:"ffmpeg_path"`
	FFprobePath string `json:"ffprobe_path"`
}

// refreshTools re-detects ffprobe from the media settings (or PATH), like the media module.
func (s *Service) refreshTools(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	var mt mediaTools
	if err := s.d.Settings.Get(ctx, "media", &mt); err != nil {
		mt = mediaTools{}
	}
	_, fp, _ := s.detect(mt.FFmpegPath, mt.FFprobePath)
	s.toolsMu.Lock()
	s.ffprobe = fp
	s.toolsMu.Unlock()
}

// ffprobePath returns the detected ffprobe binary ("" = none).
func (s *Service) ffprobePath() string {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.ffprobe
}

// ffmpegPath returns ffmpeg when it is available and transcoding is enabled ("" otherwise).
func (s *Service) ffmpegPath(ctx context.Context) string {
	if s.media == nil {
		return ""
	}
	return s.media.FFmpegPath(ctx)
}

// errFFmpegUnavailable is the API error for ffmpeg-dependent features without ffmpeg.
var errFFmpegUnavailable = core.NewError(http.StatusNotImplemented, "ffmpeg_unavailable",
	"ffmpeg is not available on this server (install it or set media.ffmpeg_path)")

// errNotTrack is returned for files that exist but are not music items (indistinguishable
// from a missing file on purpose).
var errNotTrack = core.NotFound("Track not found")
