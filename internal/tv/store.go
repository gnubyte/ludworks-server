package tv

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

type scanner interface{ Scan(dest ...any) error }

var (
	errLineupNotFound  = core.NotFound("Lineup not found")
	errChannelNotFound = core.NotFound("Channel not found")
	errNotOwner        = core.Forbidden("Only the owner can change this")
)

// newSeed returns a fresh random shuffle seed (128 bits, base64url).
func newSeed() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

/* ------------------------------------------------------------------ lineups */

const lineupCols = `id, owner_id, name, description, created_at, updated_at`

func scanLineup(sc scanner) (*lineupRow, error) {
	var (
		r        lineupRow
		cat, uat db.Time
	)
	if err := sc.Scan(&r.ID, &r.OwnerID, &r.Name, &r.Description, &cat, &uat); err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = cat.Time, uat.Time
	return &r, nil
}

// loadLineup returns the lineup row or nil when it does not exist.
func loadLineup(ctx context.Context, q db.Querier, id string) (*lineupRow, error) {
	if !httpx.ValidID(id) {
		return nil, nil
	}
	r, err := scanLineup(q.QueryRowContext(ctx, `SELECT `+lineupCols+` FROM tv_lineups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// lineupVisible reports whether user may read the lineup: owner, or the lineup is used by a
// shared channel.
func lineupVisible(ctx context.Context, q db.Querier, l *lineupRow, userID string) (bool, error) {
	if l.OwnerID == userID {
		return true, nil
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM tv_channels WHERE lineup_id = ? AND visibility = 'shared' LIMIT 1`, l.ID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// lineupFor loads a lineup the user may read (404 otherwise); write additionally requires
// ownership (403 for a visible lineup of someone else).
func lineupFor(ctx context.Context, q db.Querier, id, userID string, write bool) (*lineupRow, error) {
	l, err := loadLineup(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errLineupNotFound
	}
	if l.OwnerID == userID {
		return l, nil
	}
	ok, err := lineupVisible(ctx, q, l, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errLineupNotFound
	}
	if write {
		return nil, errNotOwner
	}
	return l, nil
}

func loadItems(ctx context.Context, q db.Querier, lineupID string) ([]LineupItem, error) {
	rs, err := q.QueryContext(ctx, `SELECT id, kind, params, item_order, item_limit, label
		FROM tv_lineup_items WHERE lineup_id = ? ORDER BY position, id`, lineupID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []LineupItem{}
	for rs.Next() {
		var (
			it    LineupItem
			p     db.JSON[itemParams]
			limit sql.NullInt64
		)
		if err := rs.Scan(&it.ID, &it.Kind, &p, &it.Order, &limit, &it.Label); err != nil {
			return nil, err
		}
		it.FileID, it.ShowTitle, it.Season = p.V.FileID, p.V.ShowTitle, p.V.Season
		it.SpaceID, it.Path, it.Recursive, it.Query = p.V.SpaceID, p.V.Path, p.V.Recursive, p.V.Query
		if limit.Valid {
			n := int(limit.Int64)
			it.Limit = &n
		}
		out = append(out, it)
	}
	return out, rs.Err()
}

// writeItems replaces every item of a lineup with items (in order).
func writeItems(ctx context.Context, tx *sql.Tx, lineupID string, items []LineupItem) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM tv_lineup_items WHERE lineup_id = ?`, lineupID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tv_lineup_items (id, lineup_id, position, kind, params,
		item_order, item_limit, label) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, it := range items {
		p := itemParams{FileID: it.FileID, ShowTitle: it.ShowTitle, Season: it.Season, SpaceID: it.SpaceID,
			Path: it.Path, Recursive: it.Recursive, Query: it.Query}
		var limit any
		if it.Limit != nil {
			limit = int64(*it.Limit)
		}
		if _, err := stmt.ExecContext(ctx, it.ID, lineupID, i, it.Kind, db.JSON[itemParams]{V: p}, it.Order, limit, it.Label); err != nil {
			return err
		}
	}
	return nil
}

func touchLineup(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE tv_lineups SET updated_at = ? WHERE id = ?`, db.FormatTime(now), id)
	return err
}

/* ----------------------------------------------------------------- channels */

const channelCols = `c.id, c.owner_id, c.number, c.name, c.description, c.grp, c.lineup_id, l.name,
	c.shuffle, c.shuffle_mode, c.shuffle_seed, c.playback_mode, c.visibility, c.anchor_at,
	COALESCE(c.logo_version, ''), c.created_at, c.updated_at`

const channelFrom = ` FROM tv_channels c JOIN tv_lineups l ON l.id = c.lineup_id `

func scanChannel(sc scanner) (*channelRow, error) {
	var (
		c             channelRow
		anchor, ct, u db.Time
	)
	if err := sc.Scan(&c.ID, &c.OwnerID, &c.Number, &c.Name, &c.Description, &c.Group, &c.LineupID, &c.LineupName,
		&c.Shuffle, &c.ShuffleMode, &c.Seed, &c.PlaybackMode, &c.Visibility, &anchor, &c.LogoVersion, &ct, &u); err != nil {
		return nil, err
	}
	c.AnchorAt, c.CreatedAt, c.UpdatedAt = anchor.Time, ct.Time, u.Time
	return &c, nil
}

func queryChannels(ctx context.Context, q db.Querier, where string, args ...any) ([]*channelRow, error) {
	rs, err := q.QueryContext(ctx, `SELECT `+channelCols+channelFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*channelRow
	for rs.Next() {
		c, err := scanChannel(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// loadChannel returns a channel row or nil.
func loadChannel(ctx context.Context, q db.Querier, id string) (*channelRow, error) {
	if !httpx.ValidID(id) {
		return nil, nil
	}
	c, err := scanChannel(q.QueryRowContext(ctx, `SELECT `+channelCols+channelFrom+`WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// canSee reports whether userID may view (and play) the channel.
func (c *channelRow) canSee(userID string) bool {
	return c.OwnerID == userID || c.Visibility == VisibilityShared
}

// channelFor loads a channel the user can see (404 otherwise); write needs ownership (403 for
// a visible shared channel of someone else).
func channelFor(ctx context.Context, q db.Querier, id, userID string, write bool) (*channelRow, error) {
	c, err := loadChannel(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if c == nil || !c.canSee(userID) {
		return nil, errChannelNotFound
	}
	if write && c.OwnerID != userID {
		return nil, errNotOwner
	}
	return c, nil
}

// visibleChannels lists the channels a user can see, sorted by number (own first on ties).
func visibleChannels(ctx context.Context, q db.Querier, userID string) ([]*channelRow, error) {
	return queryChannels(ctx, q, `WHERE c.owner_id = ?1 OR c.visibility = 'shared'
		ORDER BY c.number, c.owner_id <> ?1, c.name COLLATE NOCASE, c.id`, userID)
}

// nextChannelNumber returns max+1 for the owner, or the smallest free number when max is
// 9999; 0 when every number is taken.
func nextChannelNumber(ctx context.Context, q db.Querier, ownerID string) (int, error) {
	var mx sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT max(number) FROM tv_channels WHERE owner_id = ?`, ownerID).Scan(&mx); err != nil {
		return 0, err
	}
	if !mx.Valid {
		return 1, nil
	}
	if mx.Int64 < MaxChannelNumber {
		return int(mx.Int64) + 1, nil
	}
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		SELECT min(n) FROM seq WHERE n NOT IN (SELECT number FROM tv_channels WHERE owner_id = ?)`,
		MaxChannelNumber, ownerID).Scan(&n)
	if err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

func countRows(ctx context.Context, q db.Querier, query string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

/* ---------------------------------------------------------------- positions */

// loadPosition returns the user's on-demand position (0, 0 when none).
func loadPosition(ctx context.Context, q db.Querier, userID, channelID string) (int64, float64, error) {
	var (
		seq int64
		off float64
	)
	err := q.QueryRowContext(ctx, `SELECT seq, offset_seconds FROM tv_positions WHERE user_id = ? AND channel_id = ?`,
		userID, channelID).Scan(&seq, &off)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return seq, off, err
}

func savePosition(ctx context.Context, q db.Querier, userID, channelID string, seq int64, off float64, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO tv_positions (user_id, channel_id, seq, offset_seconds, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (user_id, channel_id) DO UPDATE SET seq = excluded.seq,
		offset_seconds = excluded.offset_seconds, updated_at = excluded.updated_at`,
		userID, channelID, seq, off, db.FormatTime(now))
	return err
}
