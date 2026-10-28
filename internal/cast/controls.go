package cast

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
)

// controlRequest is the POST /cast/sessions/{id}/control body.
type controlRequest struct {
	Action          string   `json:"action"`
	PositionSeconds *float64 `json:"position_seconds"`
	Index           *int     `json:"index"`
	Volume          *float64 `json:"volume"`
	Muted           *bool    `json:"muted"`
	Repeat          *string  `json:"repeat"`
	Shuffle         *bool    `json:"shuffle"`
}

// restartThreshold: "previous" restarts the current item when it has played longer than this.
const restartThreshold = 3.0

func validSeconds(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1e7 }

// validate checks the parameters an action needs (422 otherwise).
func (c *controlRequest) validate() error {
	fe := map[string]string{}
	switch c.Action {
	case "play", "pause", "toggle", "stop", "next", "previous":
	case "seek":
		if c.PositionSeconds == nil || !validSeconds(*c.PositionSeconds) {
			fe["position_seconds"] = "must be a number of seconds of 0 or more"
		}
	case "jump":
		if c.Index == nil || *c.Index < 0 {
			fe["index"] = "must be the index of a queued item"
		}
	case "volume":
		if c.Volume == nil || math.IsNaN(*c.Volume) || *c.Volume < 0 || *c.Volume > 1 {
			fe["volume"] = "must be between 0 and 1"
		}
	case "mute":
		if c.Muted == nil {
			fe["muted"] = "is required"
		}
	case "repeat":
		if c.Repeat == nil || (*c.Repeat != RepeatOff && *c.Repeat != RepeatAll && *c.Repeat != RepeatOne) {
			fe["repeat"] = "must be off, all or one"
		}
	case "shuffle":
		if c.Shuffle == nil {
			fe["shuffle"] = "is required"
		}
	default:
		fe["action"] = "must be play, pause, toggle, stop, seek, next, previous, jump, volume, mute, repeat or shuffle"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	return nil
}

// deviceError turns a renderer failure of a control into an API error.
func deviceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, renderer.ErrNotSeekable):
		return errNotSeekable
	case errors.Is(err, context.Canceled):
		return errSessionEnded
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	return core.NewError(http.StatusBadGateway, codeDeviceUnavailable, "The device did not accept the command")
}

// controlCtx bounds one device command.
func (s *session) controlCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, s.m.loadTimeout)
}

// control runs one action on the loop.
func (s *session) control(c controlRequest) error {
	switch c.Action {
	case "toggle":
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if s.cur != nil && (state == StatePlaying || state == StateBuffering || state == StateLoading) {
			return s.pause()
		}
		return s.play()
	case "play":
		return s.play()
	case "pause":
		return s.pause()
	case "stop":
		return s.stopPlayback()
	case "seek":
		return s.seek(*c.PositionSeconds)
	case "next":
		s.mu.Lock()
		err := s.q.next()
		if err == nil {
			s.bumpLocked()
		}
		s.mu.Unlock()
		if err != nil {
			return errNoNextItem
		}
		s.recordFinal()
		s.failures = 0
		s.schedule(func() { s.loadCurrent(0, nil) })
		return nil
	case "previous":
		s.mu.Lock()
		pos := s.positionAtLocked(s.m.now())
		s.mu.Unlock()
		if s.cur != nil && !s.cur.live && pos > restartThreshold {
			return s.seek(0)
		}
		s.mu.Lock()
		s.q.previous()
		s.bumpLocked()
		s.mu.Unlock()
		s.recordFinal()
		s.failures = 0
		s.schedule(func() { s.loadCurrent(0, nil) })
		return nil
	case "jump":
		s.mu.Lock()
		if *c.Index >= len(s.q.items) {
			s.mu.Unlock()
			return core.Validation(map[string]string{"index": fmt.Sprintf("must be below %d", len(s.q.items))})
		}
		s.q.jump(*c.Index)
		s.bumpLocked()
		s.mu.Unlock()
		s.recordFinal()
		s.failures = 0
		s.schedule(func() { s.loadCurrent(0, nil) })
		return nil
	case "volume":
		if err := s.ensureRenderer(); err != nil {
			return err
		}
		ctx, cancel := s.controlCtx()
		defer cancel()
		if err := s.rend.SetVolume(ctx, *c.Volume); err != nil {
			return deviceError(err)
		}
		s.mu.Lock()
		s.volume = *c.Volume
		s.bumpLocked()
		s.mu.Unlock()
		s.syncVolume()
		return nil
	case "mute":
		if err := s.ensureRenderer(); err != nil {
			return err
		}
		ctx, cancel := s.controlCtx()
		defer cancel()
		if err := s.rend.SetMuted(ctx, *c.Muted); err != nil {
			return deviceError(err)
		}
		s.mu.Lock()
		s.muted = *c.Muted
		s.bumpLocked()
		s.mu.Unlock()
		s.syncVolume()
		return nil
	case "repeat":
		s.mu.Lock()
		s.q.repeat = *c.Repeat
		s.bumpLocked()
		s.mu.Unlock()
		return nil
	case "shuffle":
		s.mu.Lock()
		s.q.setShuffle(*c.Shuffle)
		s.bumpLocked()
		s.mu.Unlock()
		return nil
	}
	return core.Validation(map[string]string{"action": "unknown action"})
}

// play resumes playback; with nothing loaded (stopped, failed, the queue ended) it loads the
// current item again, and a live channel paused for a while re-joins the broadcast.
func (s *session) play() error {
	if err := s.ensureRenderer(); err != nil {
		return err
	}
	s.mu.Lock()
	state := s.state
	at := s.positionAtLocked(s.m.now())
	s.mu.Unlock()
	if s.cur == nil {
		if state == StateIdle {
			at = 0 // the queue ended (or playback was stopped): start the item again
		}
		s.failures = 0
		s.schedule(func() { s.loadCurrent(at, nil) })
		s.setState(StateLoading, nil)
		return nil
	}
	if l := s.cur; l.live && !l.pausedAt.IsZero() && s.m.now().Sub(l.pausedAt) > liveResyncAfter {
		s.recordFinal()
		s.schedule(func() { s.loadCurrent(0, nil) })
		s.setState(StateLoading, nil)
		return nil
	}
	ctx, cancel := s.controlCtx()
	defer cancel()
	if err := s.rend.Play(ctx); err != nil {
		if state != StateIdle {
			return deviceError(err)
		}
		// The device dropped the item (it reported idle): load it again where it was.
		s.cur = nil
		s.schedule(func() { s.loadCurrent(at, nil) })
		s.setState(StateLoading, nil)
		return nil
	}
	if s.cur != nil {
		s.cur.pausedAt = time.Time{}
	}
	s.onStatus()
	return nil
}

// pause pauses the device.
func (s *session) pause() error {
	if s.cur == nil || s.rend == nil {
		return errNothingToSet
	}
	ctx, cancel := s.controlCtx()
	defer cancel()
	if err := s.rend.Pause(ctx); err != nil {
		return deviceError(err)
	}
	if s.cur != nil && s.cur.live && s.cur.pausedAt.IsZero() {
		s.cur.pausedAt = s.m.now()
	}
	s.onStatus()
	s.recordCurrent(false)
	return nil
}

// stopPlayback stops the item but keeps the session (and the device connection).
func (s *session) stopPlayback() error {
	if s.cur == nil {
		s.setState(StateIdle, nil)
		return nil
	}
	s.recordFinal()
	if s.rend != nil {
		ctx, cancel := s.controlCtx()
		err := s.rend.Stop(ctx)
		cancel()
		if err != nil {
			return deviceError(err)
		}
	}
	s.setState(StateIdle, nil)
	return nil
}

// seek moves within the current item: the device seeks in the file itself, a transcode is
// restarted at the position, live channels cannot seek (409).
func (s *session) seek(to float64) error {
	l := s.cur
	if l == nil || s.rend == nil {
		return errNothingToSet
	}
	if l.live {
		return errNotSeekable
	}
	if l.duration > 0 && to > l.duration {
		to = math.Max(0, l.duration-1)
	}
	if !l.direct {
		// The same playback continues from elsewhere: progress so far is kept, and a track's
		// play counter carries over to the reloaded transcode.
		s.recordCurrent(false)
		s.cur, s.carry = nil, l
		if l.item.Kind == KindChannel && l.program != nil {
			it, index := l.item, l.program.Index
			s.schedule(func() { s.loadProgramAt(it, index, to) })
		} else {
			s.schedule(func() { s.loadCurrent(to, nil) })
		}
		s.setState(StateLoading, nil)
		return nil
	}
	ctx, cancel := s.controlCtx()
	defer cancel()
	if err := s.rend.Seek(ctx, to-l.base); err != nil {
		return deviceError(err)
	}
	now := s.m.now()
	s.mu.Lock()
	s.pos, s.posAt = to, now
	s.bumpLocked()
	s.mu.Unlock()
	return nil
}

// replace swaps the queue and plays items[index] from position.
func (s *session) replace(items []*castItem, index int, position float64) error {
	s.recordFinal()
	s.mu.Lock()
	s.q.replace(items, index)
	s.reindexLocked()
	s.state, s.err = StateLoading, nil
	s.bumpLocked()
	s.mu.Unlock()
	s.failures = 0
	s.schedule(func() { s.loadCurrent(position, nil) })
	return nil
}

// insert adds items at position (-1 = append); when the queue had finished they play now.
func (s *session) insert(items []*castItem, at int) error {
	s.mu.Lock()
	if len(s.q.items)+len(items) > MaxQueue {
		s.mu.Unlock()
		return core.Validation(map[string]string{"items": fmt.Sprintf("the queue holds at most %d items", MaxQueue)})
	}
	if at < 0 || at > len(s.q.items) {
		at = len(s.q.items)
	}
	finished := s.cur == nil && s.state == StateIdle
	s.q.insert(items, at)
	s.reindexLocked()
	if finished {
		s.q.jump(at)
	}
	s.bumpLocked()
	s.mu.Unlock()
	if finished {
		s.failures = 0
		s.schedule(func() { s.loadCurrent(0, nil) })
	}
	return nil
}
