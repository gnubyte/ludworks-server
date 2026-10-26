package media

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ludworks/internal/core"
)

// Resource limits of the playback and probe endpoints.
const (
	// maxTranscodesPerUser bounds the ffmpeg transcodes one user (the requesting principal,
	// not the owner of the file — tv programs run as the channel owner) may run at once, so
	// a single account cannot hold every one of the maxTranscodes server-wide slots.
	maxTranscodesPerUser = 1
	// transcodeSlotWait is how long a transcode request waits for a slot before 429: a
	// seeking player opens the new stream while the old one is still being torn down.
	transcodeSlotWait = 5 * time.Second
	// transcodeWriteTimeout drops transcode clients that stop reading (a stalled or paused
	// player): ffmpeg is killed and the slot freed. Players resume with ?start=<position>.
	transcodeWriteTimeout = 60 * time.Second

	// Manual POST /media/{id}/probe requests: per user and server-wide.
	maxManualProbesPerUser = 1
	maxManualProbes        = 4

	// builtinProbeTimeout bounds one pure-Go probe (they are also bounded by their own
	// read and work budgets).
	builtinProbeTimeout = 60 * time.Second
)

var errTranscodeBusy = core.NewError(http.StatusTooManyRequests, "rate_limited", "Too many transcodes are running; try again shortly")

var errProbeBusy = core.NewError(http.StatusTooManyRequests, "rate_limited", "A probe is already running; try again shortly")

// slots is a counting limiter with a server-wide and a per-key maximum.
type slots struct {
	mu      sync.Mutex
	total   int
	max     int
	perKey  int
	byKey   map[string]int
	changed chan struct{} // closed (and replaced) whenever a slot is released
}

func newSlots(max, perKey int) *slots {
	return &slots{max: max, perKey: perKey, byKey: map[string]int{}, changed: make(chan struct{})}
}

// tryAcquire takes a slot for key without waiting.
func (s *slots) tryAcquire(key string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeLocked(key)
}

func (s *slots) takeLocked(key string) (func(), bool) {
	if s.total >= s.max || s.byKey[key] >= s.perKey {
		return nil, false
	}
	s.total++
	s.byKey[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.total--
			if s.byKey[key]--; s.byKey[key] <= 0 {
				delete(s.byKey, key)
			}
			close(s.changed)
			s.changed = make(chan struct{})
			s.mu.Unlock()
		})
	}, true
}

// acquire takes a slot for key, waiting up to wait for one to be released. It returns
// ctx.Err() when ctx ends first and ok=false when the wait times out.
func (s *slots) acquire(ctx context.Context, key string, wait time.Duration) (func(), bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		release, ok := s.takeLocked(key)
		changed := s.changed
		s.mu.Unlock()
		if ok {
			return release, true, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// inUse reports the slots held server-wide and by key (tests).
func (s *slots) inUse(key string) (total, byKey int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.byKey[key]
}

// requester is the user a request's resource limits are charged to: the authenticated
// caller, falling back to the principal the work runs as.
func requester(r *http.Request, fallback *core.Principal) string {
	if p, ok := core.PrincipalFrom(r.Context()); ok && p.UserID != "" {
		return p.UserID
	}
	if fallback != nil {
		return fallback.UserID
	}
	return ""
}
