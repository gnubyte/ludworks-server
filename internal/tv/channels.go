package tv

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// channelInput is the POST/PATCH /tv/channels body (absent fields are nil).
type channelInput struct {
	Number       *int    `json:"number"`
	Name         *string `json:"name"`
	LineupID     *string `json:"lineup_id"`
	Shuffle      *bool   `json:"shuffle"`
	ShuffleMode  *string `json:"shuffle_mode"`
	PlaybackMode *string `json:"playback_mode"`
	Visibility   *string `json:"visibility"`
	Group        *string `json:"group"`
	Description  *string `json:"description"`
}

// apply validates in and copies the given fields onto c.
func (in *channelInput) apply(errs fieldErrs, c *channelRow, create bool) {
	if create && in.Name == nil {
		errs.set("name", "is required")
	}
	if in.Name != nil {
		c.Name = checkText(errs, "name", *in.Name, true, nameMax, false)
	}
	if in.Description != nil {
		c.Description = checkText(errs, "description", *in.Description, false, descriptionMax, true)
	}
	if in.Group != nil {
		c.Group = checkText(errs, "group", *in.Group, false, groupMax, false)
	}
	if in.Number != nil {
		if *in.Number < 1 || *in.Number > MaxChannelNumber {
			errs.set("number", fmt.Sprintf("must be between 1 and %d", MaxChannelNumber))
		}
		c.Number = *in.Number
	}
	if in.Shuffle != nil {
		c.Shuffle = *in.Shuffle
	}
	if in.ShuffleMode != nil {
		if *in.ShuffleMode != "per_cycle" && *in.ShuffleMode != "fixed" {
			errs.set("shuffle_mode", "must be per_cycle or fixed")
		}
		c.ShuffleMode = *in.ShuffleMode
	}
	if in.PlaybackMode != nil {
		if *in.PlaybackMode != PlaybackLive && *in.PlaybackMode != PlaybackOnDemand {
			errs.set("playback_mode", "must be live or on_demand")
		}
		c.PlaybackMode = *in.PlaybackMode
	}
	if in.Visibility != nil {
		if *in.Visibility != VisibilityPrivate && *in.Visibility != VisibilityShared {
			errs.set("visibility", "must be private or shared")
		}
		c.Visibility = *in.Visibility
	}
	if create && in.LineupID == nil {
		errs.set("lineup_id", "is required")
	}
	if in.LineupID != nil {
		if !httpx.ValidID(*in.LineupID) {
			errs.set("lineup_id", "is not a valid id")
		}
		c.LineupID = *in.LineupID
	}
}

func (m *Module) listChannels(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	rows, err := visibleChannels(r.Context(), m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	out := make([]Channel, 0, len(rows))
	for _, c := range rows {
		out = append(out, c.api())
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) channelFromPath(r *http.Request, write bool) (*channelRow, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, errChannelNotFound
	}
	return channelFor(r.Context(), m.d.DB.SQL, id, core.MustPrincipal(r.Context()).UserID, write)
}

func (m *Module) getChannel(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, c.api())
}

// ownLineup checks that lineupID exists and belongs to userID (404 otherwise).
func ownLineup(ctx context.Context, q db.Querier, lineupID, userID string) error {
	l, err := loadLineup(ctx, q, lineupID)
	if err != nil {
		return err
	}
	if l == nil || l.OwnerID != userID {
		return errLineupNotFound
	}
	return nil
}

var errNumberTaken = core.Conflict("That channel number is already in use")

// insertChannel stores a new channel (number auto-assigned when 0) inside tx.
func (m *Module) insertChannel(ctx context.Context, tx *sql.Tx, c *channelRow) error {
	n, err := countRows(ctx, tx, `SELECT count(*) FROM tv_channels WHERE owner_id = ?`, c.OwnerID)
	if err != nil {
		return err
	}
	if n >= MaxChannelsPerUser {
		return core.Conflict(fmt.Sprintf("Too many channels (maximum %d)", MaxChannelsPerUser))
	}
	if c.Number == 0 {
		if c.Number, err = nextChannelNumber(ctx, tx, c.OwnerID); err != nil {
			return err
		}
		if c.Number == 0 {
			return core.Conflict("Every channel number is in use")
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tv_channels (id, owner_id, number, name, description, grp, lineup_id,
		shuffle, shuffle_mode, shuffle_seed, playback_mode, visibility, anchor_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OwnerID, c.Number, c.Name, c.Description, c.Group, c.LineupID, db.Bool(c.Shuffle), c.ShuffleMode, c.Seed,
		c.PlaybackMode, c.Visibility, db.FormatTime(c.AnchorAt), db.FormatTime(c.CreatedAt), db.FormatTime(c.UpdatedAt))
	if db.IsUniqueViolation(err) {
		return errNumberTaken
	}
	return err
}

func (m *Module) createChannel(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in channelInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	now := m.now()
	c := &channelRow{ID: core.NewID(), OwnerID: p.UserID, ShuffleMode: "per_cycle", PlaybackMode: PlaybackLive,
		Visibility: VisibilityPrivate, Seed: newSeed(), AnchorAt: now, CreatedAt: now, UpdatedAt: now}
	errs := fieldErrs{}
	in.apply(errs, c, true)
	if err := errs.err(); err != nil {
		return err
	}
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := ownLineup(ctx, tx, c.LineupID, p.UserID); err != nil {
			return err
		}
		if in.Number == nil {
			c.Number = 0
		}
		return m.insertChannel(ctx, tx, c)
	})
	if err != nil {
		return err
	}
	return m.respondChannel(w, r, c.ID, http.StatusCreated)
}

func (m *Module) respondChannel(w http.ResponseWriter, r *http.Request, id string, status int) error {
	c, err := loadChannel(r.Context(), m.d.DB.SQL, id)
	if err != nil {
		return err
	}
	if c == nil {
		return errChannelNotFound
	}
	return httpx.WriteJSON(w, status, c.api())
}

func (m *Module) updateChannel(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	var in channelInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	errs := fieldErrs{}
	in.apply(errs, c, false)
	if err := errs.err(); err != nil {
		return err
	}
	c.UpdatedAt = m.now()
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if in.LineupID != nil {
			if err := ownLineup(ctx, tx, c.LineupID, p.UserID); err != nil {
				return core.Validation(map[string]string{"lineup_id": "lineup not found"})
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE tv_channels SET number = ?, name = ?, description = ?, grp = ?, lineup_id = ?,
			shuffle = ?, shuffle_mode = ?, playback_mode = ?, visibility = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
			c.Number, c.Name, c.Description, c.Group, c.LineupID, db.Bool(c.Shuffle), c.ShuffleMode, c.PlaybackMode,
			c.Visibility, db.FormatTime(c.UpdatedAt), c.ID, p.UserID)
		if db.IsUniqueViolation(err) {
			return errNumberTaken
		}
		return err
	})
	if err != nil {
		return err
	}
	return m.respondChannel(w, r, c.ID, http.StatusOK)
}

func (m *Module) deleteChannel(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	if _, err := m.d.DB.SQL.ExecContext(r.Context(), `DELETE FROM tv_channels WHERE id = ? AND owner_id = ?`, c.ID, c.OwnerID); err != nil {
		return err
	}
	m.removeLogoFile(c.ID)
	return httpx.NoContent(w)
}

func (m *Module) reshuffle(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	if _, err := m.d.DB.SQL.ExecContext(r.Context(), `UPDATE tv_channels SET shuffle_seed = ?, updated_at = ? WHERE id = ?`,
		newSeed(), db.FormatTime(m.now()), c.ID); err != nil {
		return err
	}
	return m.respondChannel(w, r, c.ID, http.StatusOK)
}

func (m *Module) restart(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	now := db.FormatTime(m.now())
	if _, err := m.d.DB.SQL.ExecContext(r.Context(), `UPDATE tv_channels SET anchor_at = ?, updated_at = ? WHERE id = ?`,
		now, now, c.ID); err != nil {
		return err
	}
	return m.respondChannel(w, r, c.ID, http.StatusOK)
}
