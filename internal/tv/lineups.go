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

type lineupCreate struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Items       []itemInput `json:"items"`
}

type lineupPatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type itemsReplace struct {
	Items *[]itemInput `json:"items"`
}

type itemsInsert struct {
	Items    []itemInput `json:"items"`
	Position *int        `json:"position"`
}

type lineupCursor struct {
	N string `json:"n"`
	I string `json:"i"`
}

// lineupView renders a lineup with its items and expansion summary.
func (m *Module) lineupView(ctx context.Context, l *lineupRow) (Lineup, error) {
	items, err := loadItems(ctx, m.d.DB.SQL, l.ID)
	if err != nil {
		return Lineup{}, err
	}
	exp, err := m.expansionFor(ctx, l.ID, l.OwnerID)
	if err != nil {
		return Lineup{}, err
	}
	return Lineup{ID: l.ID, Name: l.Name, Description: l.Description, OwnerID: l.OwnerID, Items: items,
		ItemCount: exp.count(), TotalDurationSeconds: exp.total,
		CreatedAt: db.FormatTime(l.CreatedAt), UpdatedAt: db.FormatTime(l.UpdatedAt)}, nil
}

func (m *Module) listLineups(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	limit := httpx.ParseLimit(r, 50, 200)
	args := []any{p.UserID}
	where := `WHERE (owner_id = ?1 OR id IN (SELECT lineup_id FROM tv_channels WHERE visibility = 'shared'))`
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur lineupCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil {
			return err
		}
		where += ` AND (name COLLATE NOCASE > ?2 OR (name COLLATE NOCASE = ?2 AND id > ?3))`
		args = append(args, cur.N, cur.I)
	}
	rs, err := m.d.DB.SQL.QueryContext(r.Context(), `SELECT `+lineupCols+` FROM tv_lineups `+where+`
		ORDER BY name COLLATE NOCASE, id LIMIT `+fmt.Sprint(limit+1), args...)
	if err != nil {
		return err
	}
	var rows []*lineupRow
	for rs.Next() {
		l, err := scanLineup(rs)
		if err != nil {
			rs.Close()
			return err
		}
		rows = append(rows, l)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = httpx.EncodeCursor(lineupCursor{N: last.Name, I: last.ID})
	}
	out := make([]Lineup, 0, len(rows))
	for _, l := range rows {
		v, err := m.lineupView(r.Context(), l)
		if err != nil {
			return err
		}
		out = append(out, v)
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(out, next))
}

func (m *Module) createLineup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in lineupCreate
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	errs := fieldErrs{}
	name := checkText(errs, "name", in.Name, true, nameMax, false)
	desc := checkText(errs, "description", in.Description, false, descriptionMax, true)
	if len(in.Items) > MaxLineupItems {
		errs.set("items", fmt.Sprintf("at most %d items", MaxLineupItems))
	}
	items := normalizeItems(errs, "items", in.Items, nil)
	if err := errs.err(); err != nil {
		return err
	}
	now := m.now()
	l := &lineupRow{ID: core.NewID(), OwnerID: p.UserID, Name: name, Description: desc, CreatedAt: now, UpdatedAt: now}
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		n, err := countRows(ctx, tx, `SELECT count(*) FROM tv_lineups WHERE owner_id = ?`, p.UserID)
		if err != nil {
			return err
		}
		if n >= MaxLineupsPerUser {
			return core.Conflict(fmt.Sprintf("Too many lineups (maximum %d)", MaxLineupsPerUser))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tv_lineups (id, owner_id, name, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, l.ID, l.OwnerID, l.Name, l.Description, db.FormatTime(now), db.FormatTime(now)); err != nil {
			return err
		}
		return writeItems(ctx, tx, l.ID, items)
	})
	if err != nil {
		return err
	}
	v, err := m.lineupView(ctx, l)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, v)
}

func (m *Module) getLineup(w http.ResponseWriter, r *http.Request) error {
	l, err := m.lineupFromPath(r, false)
	if err != nil {
		return err
	}
	v, err := m.lineupView(r.Context(), l)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, v)
}

func (m *Module) lineupFromPath(r *http.Request, write bool) (*lineupRow, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, errLineupNotFound
	}
	return lineupFor(r.Context(), m.d.DB.SQL, id, core.MustPrincipal(r.Context()).UserID, write)
}

func (m *Module) updateLineup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	l, err := m.lineupFromPath(r, true)
	if err != nil {
		return err
	}
	var in lineupPatch
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	errs := fieldErrs{}
	if in.Name != nil {
		l.Name = checkText(errs, "name", *in.Name, true, nameMax, false)
	}
	if in.Description != nil {
		l.Description = checkText(errs, "description", *in.Description, false, descriptionMax, true)
	}
	if err := errs.err(); err != nil {
		return err
	}
	l.UpdatedAt = m.now()
	if _, err := m.d.DB.SQL.ExecContext(ctx, `UPDATE tv_lineups SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		l.Name, l.Description, db.FormatTime(l.UpdatedAt), l.ID); err != nil {
		return err
	}
	v, err := m.lineupView(ctx, l)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, v)
}

func (m *Module) deleteLineup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	l, err := m.lineupFromPath(r, true)
	if err != nil {
		return err
	}
	force := httpx.QueryBool(r, "force")
	var removed []string
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		removed = nil
		rs, err := tx.QueryContext(ctx, `SELECT id FROM tv_channels WHERE lineup_id = ? ORDER BY number`, l.ID)
		if err != nil {
			return err
		}
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				rs.Close()
				return err
			}
			removed = append(removed, id)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		if len(removed) > 0 && !force {
			return core.Conflict(fmt.Sprintf("This lineup is used by %d channel(s); delete them first or pass force=1", len(removed))).
				WithDetails(map[string]any{"channels": removed})
		}
		// Channels cascade with their lineup (and their positions with the channels).
		_, err = tx.ExecContext(ctx, `DELETE FROM tv_lineups WHERE id = ?`, l.ID)
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range removed {
		m.removeLogoFile(id)
	}
	m.cache.invalidate(l.ID)
	return httpx.NoContent(w)
}

func (m *Module) replaceItems(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	l, err := m.lineupFromPath(r, true)
	if err != nil {
		return err
	}
	var in itemsReplace
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Items == nil {
		return core.Validation(map[string]string{"items": "is required"})
	}
	if len(*in.Items) > MaxLineupItems {
		return core.Validation(map[string]string{"items": fmt.Sprintf("at most %d items", MaxLineupItems)})
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := loadItems(ctx, tx, l.ID)
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for _, it := range cur {
			existing[it.ID] = true
		}
		errs := fieldErrs{}
		items := normalizeItems(errs, "items", *in.Items, existing)
		if err := errs.err(); err != nil {
			return err
		}
		if err := writeItems(ctx, tx, l.ID, items); err != nil {
			return err
		}
		return touchLineup(ctx, tx, l.ID, m.now())
	})
	if err != nil {
		return err
	}
	return m.respondLineup(w, r, l.ID)
}

func (m *Module) insertItems(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	l, err := m.lineupFromPath(r, true)
	if err != nil {
		return err
	}
	var in itemsInsert
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if len(in.Items) == 0 {
		return core.Validation(map[string]string{"items": "at least one item is required"})
	}
	if in.Position != nil && *in.Position < 0 {
		return core.Validation(map[string]string{"position": "must be 0 or more"})
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := loadItems(ctx, tx, l.ID)
		if err != nil {
			return err
		}
		if len(cur)+len(in.Items) > MaxLineupItems {
			return core.Validation(map[string]string{"items": fmt.Sprintf("a lineup holds at most %d items", MaxLineupItems)})
		}
		errs := fieldErrs{}
		add := normalizeItems(errs, "items", in.Items, nil)
		if err := errs.err(); err != nil {
			return err
		}
		pos := len(cur)
		if in.Position != nil && *in.Position < pos {
			pos = *in.Position
		}
		items := make([]LineupItem, 0, len(cur)+len(add))
		items = append(items, cur[:pos]...)
		items = append(items, add...)
		items = append(items, cur[pos:]...)
		if err := writeItems(ctx, tx, l.ID, items); err != nil {
			return err
		}
		return touchLineup(ctx, tx, l.ID, m.now())
	})
	if err != nil {
		return err
	}
	return m.respondLineup(w, r, l.ID)
}

func (m *Module) deleteItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	l, err := m.lineupFromPath(r, true)
	if err != nil {
		return err
	}
	itemID := r.PathValue("item_id")
	if !httpx.ValidID(itemID) {
		return core.NotFound("Lineup item not found")
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := loadItems(ctx, tx, l.ID)
		if err != nil {
			return err
		}
		items := make([]LineupItem, 0, len(cur))
		for _, it := range cur {
			if it.ID != itemID {
				items = append(items, it)
			}
		}
		if len(items) == len(cur) {
			return core.NotFound("Lineup item not found")
		}
		if err := writeItems(ctx, tx, l.ID, items); err != nil {
			return err
		}
		return touchLineup(ctx, tx, l.ID, m.now())
	})
	if err != nil {
		return err
	}
	return m.respondLineup(w, r, l.ID)
}

// respondLineup drops the cached expansion and writes the fresh lineup.
func (m *Module) respondLineup(w http.ResponseWriter, r *http.Request, id string) error {
	m.cache.invalidate(id)
	l, err := loadLineup(r.Context(), m.d.DB.SQL, id)
	if err != nil {
		return err
	}
	if l == nil {
		return errLineupNotFound
	}
	v, err := m.lineupView(r.Context(), l)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, v)
}

func (m *Module) expandedLineup(w http.ResponseWriter, r *http.Request) error {
	l, err := m.lineupFromPath(r, false)
	if err != nil {
		return err
	}
	exp, err := m.expansionFor(r.Context(), l.ID, l.OwnerID)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, exp.expandedView())
}
