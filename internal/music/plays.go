package music

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Plays (API.md §12): a play counts when seconds_played ≥ min(50% of the duration, 240 s) or
// the track completed; repeats of (track, played_at), future times (> 5 min) and plays older
// than a year are ignored, so offline clients can replay their queue safely.

const (
	playMaxSeconds  = 240.0
	playFraction    = 0.5
	playFutureSlack = 5 * time.Minute
	playMaxAge      = 365 * 24 * time.Hour
	maxContextID    = 200
)

var contextKinds = map[string]bool{"album": true, "artist": true, "playlist": true, "genre": true, "search": true, "queue": true}

// PlayContext is where a play came from.
type PlayContext struct {
	Kind string  `json:"kind"`
	ID   *string `json:"id,omitempty"`
}

// PlayInput is one entry of POST /music/plays.
type PlayInput struct {
	TrackID       string       `json:"track_id"`
	PlayedAt      string       `json:"played_at"`
	SecondsPlayed *float64     `json:"seconds_played"`
	Completed     bool         `json:"completed"`
	Context       *PlayContext `json:"context"`
}

type play struct {
	trackID     string
	at          time.Time
	seconds     float64
	completed   bool
	kind, ctxID sql.NullString
}

// parsePlays validates the request shape (bad shapes are 422; plays that do not count are
// only "ignored" later).
func parsePlays(in []PlayInput) ([]play, error) {
	if len(in) > maxPlays {
		return nil, core.Validation(map[string]string{"plays": fmt.Sprintf("at most %d plays", maxPlays)})
	}
	out := make([]play, 0, len(in))
	for i, p := range in {
		field := fmt.Sprintf("plays[%d].", i)
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(p.PlayedAt))
		if err != nil {
			return nil, core.Validation(map[string]string{field + "played_at": "must be an RFC 3339 timestamp"})
		}
		pl := play{trackID: p.TrackID, at: at.UTC().Truncate(time.Millisecond), completed: p.Completed}
		if p.SecondsPlayed != nil {
			v := *p.SecondsPlayed
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e7 {
				return nil, core.Validation(map[string]string{field + "seconds_played": "must be a number of seconds >= 0"})
			}
			pl.seconds = v
		}
		if c := p.Context; c != nil {
			if !contextKinds[c.Kind] {
				return nil, core.Validation(map[string]string{field + "context.kind": "must be album, artist, playlist, genre, search or queue"})
			}
			pl.kind = sql.NullString{String: c.Kind, Valid: true}
			if c.ID != nil {
				id := cleanText(*c.ID, maxContextID)
				if id != "" {
					pl.ctxID = sql.NullString{String: id, Valid: true}
				}
			}
		}
		out = append(out, pl)
	}
	return out, nil
}

// counts reports whether a play of a track with the given duration is recorded.
func (pl *play) counts(duration sql.NullFloat64) bool {
	if pl.completed {
		return true
	}
	need := playMaxSeconds
	if duration.Valid && duration.Float64 > 0 {
		need = min(playFraction*duration.Float64, playMaxSeconds)
	}
	return pl.seconds >= need
}

// recordPlays records the plays that count and returns (recorded, ignored).
func (s *Service) recordPlays(ctx context.Context, p *core.Principal, plays []play) (int, int, error) {
	if len(plays) == 0 {
		return 0, 0, nil
	}
	ids := make([]string, 0, len(plays))
	for _, pl := range plays {
		if httpx.ValidID(pl.trackID) {
			ids = append(ids, pl.trackID)
		}
	}
	trs, err := s.visibleTracks(ctx, p, ids)
	if err != nil {
		return 0, 0, err
	}
	byID := map[string]*track{}
	for _, tr := range trs {
		byID[tr.FileID] = tr
	}
	now := s.now()
	recorded := 0
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		recorded = 0
		for i := range plays {
			pl := &plays[i]
			tr := byID[pl.trackID]
			if tr == nil || pl.at.After(now.Add(playFutureSlack)) || pl.at.Before(now.Add(-playMaxAge)) || !pl.counts(tr.Duration) {
				continue
			}
			ok, err := s.insertPlay(ctx, tx, p.UserID, pl, now)
			if err != nil {
				return err
			}
			if ok {
				recorded++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return recorded, len(plays) - recorded, nil
}

// insertPlay stores one play (ignored when (user, track, played_at) exists) and updates the
// counters.
func (s *Service) insertPlay(ctx context.Context, tx *sql.Tx, userID string, pl *play, now time.Time) (bool, error) {
	at := db.FormatTime(pl.at)
	res, err := tx.ExecContext(ctx, `INSERT INTO music_plays (id, user_id, file_id, played_at, seconds_played, completed,
		context_kind, context_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (user_id, file_id, played_at) DO NOTHING`,
		core.NewID(), userID, pl.trackID, at, pl.seconds, db.Bool(pl.completed), pl.kind, pl.ctxID, db.FormatTime(now))
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return false, nil
		}
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO music_play_stats (user_id, file_id, play_count, last_played_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (user_id, file_id) DO UPDATE SET play_count = play_count + 1,
		last_played_at = MAX(last_played_at, excluded.last_played_at)`, userID, pl.trackID, at)
	return err == nil, err
}

// HistoryEntry is one entry of GET /music/history.
type HistoryEntry struct {
	ID            string       `json:"id"`
	PlayedAt      string       `json:"played_at"`
	SecondsPlayed float64      `json:"seconds_played"`
	Context       *PlayContext `json:"context"`
	Track         Track        `json:"track"`
}

type historyCursor struct {
	At string `json:"t"`
	ID string `json:"id"`
}

// history pages the caller's plays of tracks they can still read, newest first.
func (s *Service) history(ctx context.Context, p *core.Principal, cursor string, limit int) ([]HistoryEntry, string, error) {
	spaces, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, "", err
	}
	w := &whereBuilder{args: []any{p.UserID}}
	visibleWhere(w, spaces)
	w.add(`pl.user_id = ?`, p.UserID)
	if cursor != "" {
		var c historyCursor
		if err := httpx.DecodeCursor(cursor, &c); err != nil {
			return nil, "", httpx.ErrInvalidCursor
		}
		w.add(`(pl.played_at, pl.id) < (?, ?)`, c.At, c.ID)
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT `+trackCols+`, pl.id, pl.played_at, pl.seconds_played, pl.context_kind, pl.context_id
		`+trackFrom+` JOIN music_plays pl ON pl.file_id = t.file_id WHERE `+w.sql()+`
		ORDER BY pl.played_at DESC, pl.id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rs.Close()
	folderArt := s.settings(ctx).FolderArt
	var out []HistoryEntry
	for rs.Next() {
		var (
			e         HistoryEntry
			at        db.Time
			kind, cid sql.NullString
		)
		tr, err := scanTrack(rs, &e.ID, &at, &e.SecondsPlayed, &kind, &cid)
		if err != nil {
			return nil, "", err
		}
		e.PlayedAt = db.FormatTime(at.Time)
		if kind.Valid {
			e.Context = &PlayContext{Kind: kind.String}
			if cid.Valid {
				id := cid.String
				e.Context.ID = &id
			}
		}
		e.Track = tr.toAPI(folderArt)
		out = append(out, e)
	}
	if err := rs.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = httpx.EncodeCursor(historyCursor{At: last.PlayedAt, ID: last.ID})
	}
	if out == nil {
		out = []HistoryEntry{}
	}
	return out, next, nil
}

// clearHistory deletes the caller's plays and play counters.
func (s *Service) clearHistory(ctx context.Context, p *core.Principal) error {
	return s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM music_plays WHERE user_id = ?`, p.UserID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM music_play_stats WHERE user_id = ?`, p.UserID)
		return err
	})
}
