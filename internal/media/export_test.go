package media

import (
	"context"
	"net/http"
	"time"
)

// Test hooks (compiled only into this package's tests).

// SetTMDBEndpoints points the TMDB client at test servers.
func (s *Service) SetTMDBEndpoints(api, img string, c *http.Client) {
	s.tmdb.apiBase, s.tmdb.imageBase = api, img
	if c != nil {
		s.tmdb.client = c
	}
}

// RefreshTools re-detects ffmpeg/ffprobe synchronously.
func (s *Service) RefreshTools(ctx context.Context) { s.refreshTools(ctx) }

// DrainProbes runs the probe queue synchronously.
func (s *Service) DrainProbes(ctx context.Context) error { return s.drainProbes(ctx) }

// Reconcile runs the startup reconciliation synchronously.
func (s *Service) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }

// GC runs the image garbage collector.
func (s *Service) GC(ctx context.Context) error { return s.gc(ctx) }

// System returns the internal principal.
func (s *Service) System() any { return s.sys }

// Exported pure helpers for unit tests.
var (
	SortTitleFor     = sortTitleFor
	CleanText        = cleanText
	CleanMultiline   = cleanMultiline
	NormalizeGenres  = normalizeGenres
	TrimVTT          = trimVTT
	CleanDirPath     = cleanDirPath
	ValidStoredImage = validStoredImage
	LooksLikeWebP    = looksLikeWebP
)

// SetTranscodeTiming shortens the transcode slot wait and the stalled-client write timeout.
func (s *Service) SetTranscodeTiming(wait, writeTimeout time.Duration) {
	s.transcodeWait, s.transcodeWriteTimeout = wait, writeTimeout
}

// TranscodesInUse reports the transcode slots held server-wide and by userID.
func (s *Service) TranscodesInUse(userID string) (total, byUser int) {
	return s.transcodes.inUse(userID)
}

// HoldProbe takes userID's manual-probe slot (as a running POST /probe would).
func (s *Service) HoldProbe(userID string) (release func(), ok bool) {
	return s.probes.tryAcquire(userID)
}
