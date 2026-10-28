package kanban

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

/* ----------------------------------------------------------------- columns */

// columnColor validates an optional column color (null or "" clears it).
func columnColor(o optional[string]) (*string, string) {
	if !o.Set || o.Null || o.V == "" {
		return nil, ""
	}
	if !colors[o.V] {
		return nil, "must be one of " + colorList
	}
	v := o.V
	return &v, ""
}

// wipLimit validates an optional WIP limit (null or 0 clears it).
func wipLimit(o optional[int]) (*int, string) {
	if !o.Set || o.Null || o.V == 0 {
		return nil, ""
	}
	if o.V < 0 || o.V > maxWIPLimit {
		return nil, "must be between 1 and " + itoa(maxWIPLimit)
	}
	v := o.V
	return &v, ""
}

func (m *Module) createColumn(w http.ResponseWriter, r *http.Request) error {
	boardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name     string           `json:"name"`
		Position optional[int]    `json:"position"`
		Color    optional[string] `json:"color"`
		WIPLimit optional[int]    `json:"wip_limit"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	name, problem := cleanLine(in.Name, maxNameLen, true)
	if problem != "" {
		fe.add("name", problem)
	}
	color, problem := columnColor(in.Color)
	if problem != "" {
		fe.add("color", problem)
	}
	wip, problem := wipLimit(in.WIPLimit)
	if problem != "" {
		fe.add("wip_limit", problem)
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Column
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, boardID, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		n, err := columnSeq.count(ctx, tx, boardID)
		if err != nil {
			return err
		}
		if n >= MaxColumns {
			return tooMany("columns", MaxColumns)
		}
		pos := n
		if in.Position.Set && !in.Position.Null {
			pos = clamp(in.Position.V, n)
		}
		if err := columnSeq.openGap(ctx, tx, boardID, pos); err != nil {
			return err
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_columns (id, board_id, name, position, color, wip_limit, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, boardID, name, pos, color, wip, ts, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, boardID, "", uid, "column.created", map[string]any{"column_id": id, "name": name}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, boardID, ts); err != nil {
			return err
		}
		out, err = getColumn(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

func (m *Module) patchColumn(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name      optional[string] `json:"name"`
		Color     optional[string] `json:"color"`
		WIPLimit  optional[int]    `json:"wip_limit"`
		Collapsed optional[bool]   `json:"collapsed"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	var name string
	if in.Name.Set {
		var problem string
		if name, problem = cleanLine(in.Name.V, maxNameLen, true); problem != "" {
			fe.add("name", problem)
		}
	}
	color, problem := columnColor(in.Color)
	if problem != "" {
		fe.add("color", problem)
	}
	wip, problem := wipLimit(in.WIPLimit)
	if problem != "" {
		fe.add("wip_limit", problem)
	}
	if in.Collapsed.Set && in.Collapsed.Null {
		fe.add("collapsed", "must be true or false")
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Column
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := columnFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		cur, err := getColumn(ctx, tx, id)
		if err != nil {
			return err
		}
		var fields []string
		if in.Name.Set && name != cur.Name {
			fields = append(fields, "name")
			cur.Name = name
		}
		if in.Color.Set && !sameStr(color, cur.Color) {
			fields = append(fields, "color")
			cur.Color = color
		}
		if in.WIPLimit.Set && !sameInt(wip, cur.WIPLimit) {
			fields = append(fields, "wip_limit")
			cur.WIPLimit = wip
		}
		if in.Collapsed.Set && in.Collapsed.V != cur.Collapsed {
			fields = append(fields, "collapsed")
			cur.Collapsed = in.Collapsed.V
		}
		if len(fields) > 0 {
			ts := now()
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_columns SET name = ?, color = ?, wip_limit = ?, collapsed = ?, updated_at = ? WHERE id = ?`,
				cur.Name, cur.Color, cur.WIPLimit, db.Bool(cur.Collapsed), ts, id); err != nil {
				return err
			}
			if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
				return err
			}
			// Collapsing is a view preference; it is not worth a log entry.
			if logged := withoutField(fields, "collapsed"); len(logged) > 0 {
				meta := map[string]any{"column_id": id, "name": cur.Name, "fields": logged}
				if in.Name.Set && ref.Name != cur.Name {
					meta["old_name"] = ref.Name
				}
				if err := logActivity(ctx, tx, ref.BoardID, "", uid, "column.updated", meta, ts); err != nil {
					return err
				}
			}
		}
		out, err = getColumn(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) deleteColumn(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	target := strings.TrimSpace(r.URL.Query().Get("move_cards_to"))
	if target != "" && !httpx.ValidID(target) {
		return validation("move_cards_to", "is not a column of this board")
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := columnFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		n, err := cardSeq.count(ctx, tx, id)
		if err != nil {
			return err
		}
		ts := now()
		meta := map[string]any{"column_id": id, "name": ref.Name}
		if n > 0 {
			if target == "" {
				return core.Conflict("The column still holds cards; move them to another column first (move_cards_to)").
					WithDetails(map[string]any{"card_count": n})
			}
			var targetBoard, targetName string
			err := tx.QueryRowContext(ctx, `SELECT board_id, name FROM kanban_columns WHERE id = ?`, target).Scan(&targetBoard, &targetName)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (targetBoard != ref.BoardID || target == id)) {
				return validation("move_cards_to", "must be another column of this board")
			}
			if err != nil {
				return err
			}
			base, err := cardSeq.count(ctx, tx, target)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET column_id = ?, position = position + ?, updated_at = ? WHERE column_id = ?`,
				target, base, ts, id); err != nil {
				return err
			}
			meta["moved_cards"] = n
			meta["to_column_id"] = target
			meta["to_column"] = targetName
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_columns WHERE id = ?`, id); err != nil {
			return err
		}
		if err := columnSeq.closeGap(ctx, tx, ref.BoardID, ref.Position); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, ref.BoardID, "", uid, "column.deleted", meta, ts); err != nil {
			return err
		}
		return touch(ctx, tx, ref.BoardID, ts)
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) moveColumn(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Position *int `json:"position"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Position == nil {
		return validation("position", "is required")
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out []Column
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := columnFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		n, err := columnSeq.count(ctx, tx, ref.BoardID)
		if err != nil {
			return err
		}
		to := clamp(*in.Position, n-1)
		if to != ref.Position {
			if err := columnSeq.moveWithin(ctx, tx, ref.BoardID, id, ref.Position, to); err != nil {
				return err
			}
			ts := now()
			if err := logActivity(ctx, tx, ref.BoardID, "", uid, "column.moved",
				map[string]any{"column_id": id, "name": ref.Name, "from": ref.Position, "to": to}, ts); err != nil {
				return err
			}
			if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
				return err
			}
		}
		out, err = boardColumns(ctx, tx, ref.BoardID)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func sameStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func sameInt(a, b *int) bool    { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func withoutField(fields []string, drop string) []string {
	out := fields[:0:0]
	for _, f := range fields {
		if f != drop {
			out = append(out, f)
		}
	}
	return out
}

/* ------------------------------------------------------------------ labels */

func (m *Module) createLabel(w http.ResponseWriter, r *http.Request) error {
	boardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	name, problem := cleanLine(in.Name, maxNameLen, true)
	if problem != "" {
		fe.add("name", problem)
	}
	if !colors[in.Color] {
		fe.add("color", "must be one of "+colorList)
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Label
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, boardID, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kanban_labels WHERE board_id = ?`, boardID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxLabels {
			return tooMany("labels", MaxLabels)
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_labels (id, board_id, name, name_key, color, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, boardID, name, strings.ToLower(name), in.Color, ts); err != nil {
			if db.IsUniqueViolation(err) {
				return core.Conflict("A label with that name already exists on this board")
			}
			return err
		}
		if err := logActivity(ctx, tx, boardID, "", uid, "label.created", map[string]any{"label_id": id, "name": name, "color": in.Color}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, boardID, ts); err != nil {
			return err
		}
		out, err = getLabel(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

const labelBoardQuery = `SELECT l.board_id, m.role FROM kanban_labels l
	LEFT JOIN kanban_members m ON m.board_id = l.board_id AND m.user_id = ? WHERE l.id = ?`

func (m *Module) patchLabel(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name  optional[string] `json:"name"`
		Color optional[string] `json:"color"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	var name string
	if in.Name.Set {
		var problem string
		if name, problem = cleanLine(in.Name.V, maxNameLen, true); problem != "" {
			fe.add("name", problem)
		}
	}
	if in.Color.Set && !colors[in.Color.V] {
		fe.add("color", "must be one of "+colorList)
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Label
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		boardID, rl, err := lookupBoard(ctx, tx, labelBoardQuery, id, uid, errLabelNotFound)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		cur, err := getLabel(ctx, tx, id)
		if err != nil {
			return err
		}
		var fields []string
		oldName := cur.Name
		if in.Name.Set && name != cur.Name {
			fields = append(fields, "name")
			cur.Name = name
		}
		if in.Color.Set && in.Color.V != cur.Color {
			fields = append(fields, "color")
			cur.Color = in.Color.V
		}
		if len(fields) > 0 {
			ts := now()
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_labels SET name = ?, name_key = ?, color = ? WHERE id = ?`,
				cur.Name, strings.ToLower(cur.Name), cur.Color, id); err != nil {
				if db.IsUniqueViolation(err) {
					return core.Conflict("A label with that name already exists on this board")
				}
				return err
			}
			meta := map[string]any{"label_id": id, "name": cur.Name, "fields": fields}
			if oldName != cur.Name {
				meta["old_name"] = oldName
			}
			if err := logActivity(ctx, tx, boardID, "", uid, "label.updated", meta, ts); err != nil {
				return err
			}
			if err := touch(ctx, tx, boardID, ts); err != nil {
				return err
			}
		}
		out = cur
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) deleteLabel(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		boardID, rl, err := lookupBoard(ctx, tx, labelBoardQuery, id, uid, errLabelNotFound)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		l, err := getLabel(ctx, tx, id)
		if err != nil {
			return err
		}
		ts := now()
		// The label disappears from every card (cascade); their versions move on.
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET version = version + 1, updated_at = ?
			WHERE id IN (SELECT card_id FROM kanban_card_labels WHERE label_id = ?)`, ts, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_labels WHERE id = ?`, id); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, boardID, "", uid, "label.deleted", map[string]any{"label_id": id, "name": l.Name, "color": l.Color}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, boardID, ts)
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}
