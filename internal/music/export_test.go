package music

import (
	"context"
	"encoding/json"
	"time"
)

// Test hooks (compiled only into this package's tests).

// DrainProbes runs the tag-reading queue synchronously.
func (s *Service) DrainProbes(ctx context.Context) error { return s.drainProbes(ctx) }

// DrainFolderArt runs the folder image queue synchronously.
func (s *Service) DrainFolderArt(ctx context.Context) error { return s.drainFolderArt(ctx) }

// Reconcile runs the startup reconciliation synchronously.
func (s *Service) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }

// GC runs the garbage collector.
func (s *Service) GC(ctx context.Context) error { return s.gc(ctx) }

// RefreshTools re-detects ffprobe synchronously.
func (s *Service) RefreshTools(ctx context.Context) { s.refreshTools(ctx) }

// SetClock replaces the clock.
func (s *Service) SetClock(fn func() time.Time) { s.now = fn }

// SetTranscodeTiming shortens the transcode slot wait and the stalled-client write timeout.
func (s *Service) SetTranscodeTiming(wait, writeTimeout time.Duration) {
	s.transcodeWait, s.transcodeWriteTimeout = wait, writeTimeout
}

// TranscodesInUse reports the transcode slots held server-wide and by userID.
func (s *Service) TranscodesInUse(userID string) (total, byUser int) {
	return s.transcodes.inUse(userID)
}

// HoldProbe takes userID's manual-probe slot.
func (s *Service) HoldProbe(userID string) (release func(), ok bool) {
	return s.probes.tryAcquire(userID)
}

// Dir is <data>/music.
func (s *Service) Dir() string { return s.dir }

// Exported pure helpers for unit tests.
var (
	Fold             = fold
	SortKey          = sortKey
	AlbumIDFor       = albumIDFor
	ArtistIDFor      = artistIDFor
	GuessFromPath    = guessFromPath
	MatchesTerms     = matchesTerms
	ChooseFolderName = func(names ...string) string {
		var sib []sibling
		for i, n := range names {
			sib = append(sib, sibling{id: string(rune('a' + i)), name: n})
		}
		id := chooseFolderImage(sib)
		for i, n := range names {
			if string(rune('a'+i)) == id {
				return n
			}
		}
		return ""
	}
	TargetBitrate = targetBitrate
	DecodeText    = decodeText
	RelPath       = relPath
)

// PathGuess exposes guessFromPath's result.
type PathGuess = pathGuess

// Effective computes the effective values of file tags, a manual-edit JSON object and a path.
func Effective(tagsJSON, manualJSON, spacePath, name string) map[string]any {
	var ft fileTags
	_ = json.Unmarshal([]byte(tagsJSON), &ft)
	var man map[string]json.RawMessage
	_ = json.Unmarshal([]byte(manualJSON), &man)
	e := computeEffective(ft, man, spacePath, name)
	return map[string]any{"title": e.Title, "artist": e.Artist, "artists": e.Artists, "album": e.Album,
		"album_artist": e.AlbumArtist, "album_id": e.AlbumID, "artist_id": e.ArtistID, "album_artist_id": e.AlbumArtistID,
		"track_number": e.TrackNumber.Int64, "disc_number": e.DiscNumber.Int64, "year": e.Year.Int64, "genres": e.Genres,
		"sort_title": e.SortTitle, "sort_artist": e.SortArtist, "sort_album_artist": e.SortAlbumArtist, "compilation": e.Compilation}
}

// ValidateRules validates smart rules JSON.
func ValidateRules(raw string) error {
	_, err := validateRules([]byte(raw))
	return err
}
