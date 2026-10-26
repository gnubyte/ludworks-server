package tv

import (
	"errors"
	"io"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/media/probe"
	"ludworks/internal/tv/schedule"
)

const (
	liveMaxTotal     = 16               // live.ts streams server-wide
	liveHeight       = 720              // output height of live.ts
	liveWriteTimeout = 30 * time.Second // a client that stops reading is dropped
	liveMaxFailures  = 3                // consecutive failed programs before giving up
	liveMaxFastExits = 20               // consecutive "instant" programs before giving up
)

var errFFmpegUnavailable = core.NewError(http.StatusNotImplemented, "ffmpeg_unavailable",
	"Live MPEG-TS streams need ffmpeg on the server")

var errTunersBusy = core.NewError(http.StatusTooManyRequests, "rate_limited", "All tuners are busy; try again shortly")

// acquireTuner reserves a live.ts slot for userID (per-user limit = tv.hdhr_tuner_count).
func (m *Module) acquireTuner(userID string, perUser int) (func(), bool) {
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	if m.liveTotal >= liveMaxTotal || m.liveUsers[userID] >= perUser {
		return nil, false
	}
	m.liveTotal++
	m.liveUsers[userID]++
	return func() {
		m.liveMu.Lock()
		m.liveTotal--
		if m.liveUsers[userID]--; m.liveUsers[userID] <= 0 {
			delete(m.liveUsers, userID)
		}
		m.liveMu.Unlock()
	}, true
}

// deadlineWriter flushes every write and drops clients that stop reading.
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d deadlineWriter) Write(b []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
	n, err := d.w.Write(b)
	if err == nil {
		err = d.rc.Flush()
	}
	return n, err
}

// liveTS streams the channel as one continuous MPEG-TS: the program on air (from its live
// offset), then the following ones, each transcoded by ffmpeg with a growing
// -output_ts_offset so timestamps keep increasing. It runs until the client disconnects or
// the channel disappears. On-demand channels use their live schedule here.
func (m *Module) liveTS(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	ff := m.ffmpeg(ctx)
	if ff == "" {
		return errFFmpegUnavailable
	}
	owner, err := m.ownerPrincipal(ctx, c.OwnerID)
	if err != nil {
		return err
	}
	if owner == nil || m.files == nil {
		return errChannelNotFound
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return err
	}
	if cur, _ := schedule.Now(exp.items, scheduleChannel(c), m.now()); cur == nil {
		return core.NotFound("Nothing is on air on this channel")
	}
	release, ok := m.acquireTuner(p.UserID, m.tvSettings(ctx).HDHRTunerCount)
	if !ok {
		w.Header().Set("Retry-After", "10")
		return errTunersBusy
	}
	defer release()

	h := w.Header()
	h.Set("Content-Type", "video/mp2t")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	out := deadlineWriter{w: w, rc: rc}

	sent := 0.0         // seconds of output already produced (next -output_ts_offset)
	var after time.Time // never start a program before the previous one's scheduled end
	failures, fast := 0, 0
	for ctx.Err() == nil {
		c, err = loadChannel(ctx, m.d.DB.SQL, c.ID)
		if err != nil || c == nil || !c.canSee(p.UserID) {
			return nil
		}
		if exp, err = m.expansionFor(ctx, c.LineupID, c.OwnerID); err != nil {
			return nil
		}
		at := m.now()
		if at.Before(after) {
			at = after
		}
		cur, _ := schedule.Now(exp.items, scheduleChannel(c), at)
		if cur == nil {
			return nil
		}
		after = cur.End
		path, err := m.files.LocalPath(ctx, owner, cur.FileID)
		if err != nil {
			if failures++; failures >= liveMaxFailures {
				return nil
			}
			continue
		}
		cmd, err := probe.LiveTSCommand(ctx, ff, path, cur.Offset, liveHeight, sent)
		if err != nil {
			m.log.Warn("live.ts: building ffmpeg command failed", "channel", c.ID, "err", err)
			return nil
		}
		cmd.Stdout = out
		started := time.Now()
		runErr := cmd.Run()
		if ctx.Err() != nil {
			return nil
		}
		elapsed := time.Since(started)
		if runErr != nil {
			var ee interface{ ExitCode() int }
			if !errors.As(runErr, &ee) {
				return nil // write failure: the client is gone
			}
			m.log.Warn("live.ts: ffmpeg failed", "channel", c.ID, "file", cur.FileID, "err", runErr)
			if failures++; failures >= liveMaxFailures {
				return nil
			}
		} else {
			failures = 0
		}
		if elapsed < time.Second {
			if fast++; fast >= liveMaxFastExits {
				return nil
			}
		} else {
			fast = 0
		}
		// -re paces ffmpeg in real time, so wall time ≥ media time written: using it keeps the
		// next program's timestamps strictly after this one's.
		sent += max(elapsed.Seconds(), 0.001)
	}
	return nil
}
