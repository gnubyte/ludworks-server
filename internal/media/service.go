package media

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/imaging"
	"ludworks/internal/media/probe"
)

// Concurrency limits.
const (
	maxTranscodes   = 2 // concurrent ffmpeg transcodes (429 beyond)
	maxSubExtracts  = 2 // concurrent embedded-subtitle extractions
	maxImageWorkers = 2 // concurrent poster resizes / downloads
)

// Service is the media service. The tv module uses the Serve* methods (with the channel
// owner's principal) and FFmpegPath; everything else backs the HTTP handlers.
// TopicUpdated is published after metadata that other modules cache changes: a new media
// row, probe results (duration, codecs), descriptive fields and posters. The payload is an
// UpdatedEvent. The tv module drops its cached lineup expansions on it, so a freshly
// uploaded episode is scheduled as soon as its duration is known.
const TopicUpdated = "media.updated"

// UpdatedEvent is the payload of TopicUpdated.
type UpdatedEvent struct {
	FileIDs []string
}

// publishUpdated announces changed media rows (see TopicUpdated).
func (s *Service) publishUpdated(ids ...string) {
	if len(ids) == 0 || s.d == nil || s.d.Events == nil {
		return
	}
	s.d.Events.Publish(TopicUpdated, UpdatedEvent{FileIDs: ids})
}

type Service struct {
	d     *core.Deps
	files *files.Service
	log   *slog.Logger
	dir   string // <data>/media

	toolsMu   sync.RWMutex
	tools     toolInfo
	refreshMu sync.Mutex // serialises refreshTools so the last run sees the latest settings

	transcodes *slots // ffmpeg transcodes: maxTranscodes server-wide, maxTranscodesPerUser each
	probes     *slots // manual POST /probe requests
	// transcodeWait and transcodeWriteTimeout (see limits.go) are fields so tests can shorten them.
	transcodeWait         time.Duration
	transcodeWriteTimeout time.Duration
	subSem                chan struct{}
	imgSem                chan struct{}

	probeDirty atomic.Bool
	eventMu    sync.Mutex // serialises file-event handling

	tmdb tmdbConfig

	rootOnce sync.Once
	root     *os.Root
	rootErr  error

	now func() time.Time
	// detect resolves ffmpeg/ffprobe (replaceable in tests).
	detect func(ffmpegSetting, ffprobeSetting string) (ffmpeg, ffprobe, version string)
	// sys is the internal principal used by background work (probing, ingest). It has the
	// admin role so files.Service grants it access to every space; it never reaches HTTP.
	sys *core.Principal
}

type toolInfo struct {
	ffmpeg, ffprobe, version string
	checked                  bool
}

func newService(d *core.Deps, f *files.Service) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		d: d, files: f, log: log.With("module", "media"),
		dir:                   filepath.Join(d.Config.DataDir, "media"),
		transcodes:            newSlots(maxTranscodes, maxTranscodesPerUser),
		probes:                newSlots(maxManualProbes, maxManualProbesPerUser),
		transcodeWait:         transcodeSlotWait,
		transcodeWriteTimeout: transcodeWriteTimeout,
		subSem:                make(chan struct{}, maxSubExtracts),
		imgSem:                make(chan struct{}, maxImageWorkers),
		tmdb:                  defaultTMDB(),
		now:                   core.Now,
		detect:                probe.DetectTools,
		sys: &core.Principal{UserID: "media-system", Username: "system", Role: core.RoleAdmin,
			Scopes: []string{core.ScopeAll}, Kind: "system"},
	}
}

// ensureDirs creates <data>/media and its subdirectories (0700).
func (s *Service) ensureDirs() error {
	for _, sub := range []string{"posters", "backdrops", "cache", "subs"} {
		if err := os.MkdirAll(filepath.Join(s.dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// mediaRoot is an os.Root on <data>/media; all poster/subtitle file access goes through it.
func (s *Service) mediaRoot() (*os.Root, error) {
	s.rootOnce.Do(func() {
		if err := s.ensureDirs(); err != nil {
			s.rootErr = err
			return
		}
		s.root, s.rootErr = os.OpenRoot(s.dir)
	})
	return s.root, s.rootErr
}

// settings returns the current media settings (defaults on error).
func (s *Service) settings(ctx context.Context) Settings {
	st := DefaultSettings
	if err := s.d.Settings.Get(ctx, "media", &st); err != nil {
		s.log.Warn("read media settings", "err", err)
		return DefaultSettings
	}
	return st
}

// refreshTools re-detects ffmpeg/ffprobe from the settings (or PATH).
func (s *Service) refreshTools(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	st := s.settings(ctx)
	ff, fp, ver := s.detect(st.FFmpegPath, st.FFprobePath)
	s.toolsMu.Lock()
	s.tools = toolInfo{ffmpeg: ff, ffprobe: fp, version: ver, checked: true}
	s.toolsMu.Unlock()
}

func (s *Service) toolSnapshot() toolInfo {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.tools
}

// ffmpeg returns the detected ffmpeg binary regardless of transcode_enabled ("" = none).
func (s *Service) ffmpeg() string { return s.toolSnapshot().ffmpeg }

// ffprobe returns the detected ffprobe binary ("" = none).
func (s *Service) ffprobe() string { return s.toolSnapshot().ffprobe }

// FFmpegPath returns the resolved ffmpeg binary, or "" when ffmpeg is unavailable or
// transcoding is disabled in the media settings.
func (s *Service) FFmpegPath(ctx context.Context) string {
	ff := s.ffmpeg()
	if ff == "" || !s.settings(ctx).TranscodeEnabled {
		return ""
	}
	return ff
}

// videoThumbs reports whether video thumbnails can be produced (mirrors the files module:
// ffmpeg present and auto_thumbnails on).
func (s *Service) videoThumbs(ctx context.Context) bool {
	return s.ffmpeg() != "" && s.settings(ctx).AutoThumbnails
}

// hasThumbnail mirrors files' has_thumbnail rule for an entry.
func hasThumbnail(e *files.Entry, thumbState int, video bool) bool {
	if e.IsDir || thumbState == 2 || e.Size <= 0 {
		return false
	}
	switch {
	case e.Mime == "image/jpeg" || e.Mime == "image/png" || e.Mime == "image/gif":
		return e.Size <= imaging.MaxInputBytes
	case strings.HasPrefix(e.Mime, "video/"):
		return video
	}
	return false
}

// errFFmpegUnavailable is the API error for ffmpeg-dependent features without ffmpeg.
var errFFmpegUnavailable = core.NewError(http.StatusNotImplemented, "ffmpeg_unavailable",
	"ffmpeg is not available on this server (install it or set media.ffmpeg_path)")

// errNotMedia is returned for files that exist but are not media items (indistinguishable
// from a missing file on purpose).
var errNotMedia = core.NotFound("Media item not found")
