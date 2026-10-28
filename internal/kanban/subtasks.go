package kanban

import (
	"database/sql"
	"errors"
	"net/http"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// subtaskRef locates a subtask with its card and uid's role on the board.
type subtaskRef struct {
	ID, CardID, BoardID, ColumnID, Title string
	Position, CardPosition               int
	Done                                 bool
}

func subtaskFor(tx *sql.Tx, r *http.Request, id, uid string) (*subtaskRef, role, error) {
	var s subtaskRef
	var rl sql.NullString
	err := tx.QueryRowContext(r.Context(), `SELECT s.id, s.card_id, c.board_id, c.column_id, s.title, s.position, c.position, s.done, m.role
		FROM kanban_subtasks s JOIN kanban_cards c ON c.id = s.card_id
		LEFT JOIN kanban_members m ON m.board_id = c.board_id AND m.user_id = ? WHERE s.id = ?`, uid, id).
		Scan(&s.ID, &s.CardID, &s.BoardID, &s.ColumnID, &s.Title, &s.Position, &s.CardPosition, &s.Done, &rl)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !rl.Valid) {
		return nil, roleNone, errSubtaskNotFound
	}
	if err != nil {
		return nil, roleNone, err
	}
	return &s, parseRole(rl.String), nil
}

// checkAssignee verifies that a subtask assignee is a member of the board.
func checkAssignee(tx *sql.Tx, r *http.Request, boardID string, uid *string) error {
	if uid == nil {
		return nil
	}
	var x int
	err := tx.QueryRowContext(r.Context(), `SELECT 1 FROM kanban_members WHERE board_id = ? AND user_id = ?`, boardID, *uid).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return validation("assignee_id", "must be a member of this board")
	}
	return err
}

func optID(o optional[string], field string) (*string, error) {
	if !o.Set || o.Null || o.V == "" {
		return nil, nil
	}
	if !httpx.ValidID(o.V) {
		return nil, validation(field, "is not a valid id")
	}
	v := o.V
	return &v, nil
}

func (m *Module) createSubtask(w http.ResponseWriter, r *http.Request) error {
	cardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Title      string           `json:"title"`
		Position   optional[int]    `json:"position"`
		DueAt      optional[string] `json:"due_at"`
		AssigneeID optional[string] `json:"assignee_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	title, problem := cleanLine(in.Title, maxTitleLen, true)
	if problem != "" {
		fe.add("title", problem)
	}
	due, problem := optTime(in.DueAt)
	if problem != "" {
		fe.add("due_at", problem)
	}
	if err := fe.err(); err != nil {
		return err
	}
	assignee, err := optID(in.AssigneeID, "assignee_id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Subtask
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		card, rl, err := cardFor(ctx, tx, cardID, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		if err := checkAssignee(tx, r, card.BoardID, assignee); err != nil {
			return err
		}
		n, err := subtaskSeq.count(ctx, tx, cardID)
		if err != nil {
			return err
		}
		if n >= MaxSubtasks {
			return tooMany("subtasks on this card", MaxSubtasks)
		}
		pos := n
		if in.Position.Set && !in.Position.Null {
			pos = clamp(in.Position.V, n)
		}
		if err := subtaskSeq.openGap(ctx, tx, cardID, pos); err != nil {
			return err
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_subtasks (id, card_id, title, position, due_at, assignee_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, cardID, title, pos, due, assignee, ts, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, card.BoardID, cardID, uid, "subtask.created", map[string]any{"subtask_id": id, "title": title}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, card.BoardID, ts); err != nil {
			return err
		}
		out, err = getSubtask(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

func (m *Module) patchSubtask(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Title      optional[string] `json:"title"`
		Done       optional[bool]   `json:"done"`
		DueAt      optional[string] `json:"due_at"`
		AssigneeID optional[string] `json:"assignee_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	var title string
	if in.Title.Set {
		var problem string
		if title, problem = cleanLine(in.Title.V, maxTitleLen, true); problem != "" {
			fe.add("title", problem)
		}
	}
	due, problem := optTime(in.DueAt)
	if problem != "" {
		fe.add("due_at", problem)
	}
	if in.Done.Set && in.Done.Null {
		fe.add("done", "must be true or false")
	}
	if err := fe.err(); err != nil {
		return err
	}
	assignee, err := optID(in.AssigneeID, "assignee_id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Subtask
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := subtaskFor(tx, r, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		cur, err := getSubtask(ctx, tx, id)
		if err != nil {
			return err
		}
		next := *cur
		var fields []string
		ts := now()
		if in.Title.Set && title != cur.Title {
			fields = append(fields, "title")
			next.Title = title
		}
		if in.Done.Set && in.Done.V != cur.Done {
			fields = append(fields, "done")
			next.Done = in.Done.V
			next.CompletedAt = nil
			if next.Done {
				next.CompletedAt = &ts
			}
		}
		if in.DueAt.Set && !sameStr(due, cur.DueAt) {
			fields = append(fields, "due_at")
			next.DueAt = due
		}
		if in.AssigneeID.Set && !sameStr(assignee, cur.AssigneeID) {
			if err := checkAssignee(tx, r, ref.BoardID, assignee); err != nil {
				return err
			}
			fields = append(fields, "assignee_id")
			next.AssigneeID = assignee
		}
		if len(fields) == 0 {
			out = cur
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_subtasks SET title = ?, done = ?, completed_at = ?, due_at = ?, assignee_id = ?, updated_at = ?
			WHERE id = ?`, next.Title, db.Bool(next.Done), next.CompletedAt, next.DueAt, next.AssigneeID, ts, id); err != nil {
			return err
		}
		action, meta := "subtask.updated", map[string]any{"subtask_id": id, "title": next.Title, "fields": fields}
		if len(fields) == 1 && fields[0] == "done" {
			action, meta = "subtask.reopened", map[string]any{"subtask_id": id, "title": next.Title}
			if next.Done {
				action = "subtask.completed"
			}
		}
		if err := logActivity(ctx, tx, ref.BoardID, ref.CardID, uid, action, meta, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
			return err
		}
		out, err = getSubtask(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) deleteSubtask(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := subtaskFor(tx, r, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_subtasks WHERE id = ?`, id); err != nil {
			return err
		}
		if err := subtaskSeq.closeGap(ctx, tx, ref.CardID, ref.Position); err != nil {
			return err
		}
		ts := now()
		if err := logActivity(ctx, tx, ref.BoardID, ref.CardID, uid, "subtask.deleted", map[string]any{"subtask_id": id, "title": ref.Title}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, ref.BoardID, ts)
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) moveSubtask(w http.ResponseWriter, r *http.Request) error {
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
	var out []Subtask
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := subtaskFor(tx, r, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		n, err := subtaskSeq.count(ctx, tx, ref.CardID)
		if err != nil {
			return err
		}
		if to := clamp(*in.Position, n-1); to != ref.Position {
			if err := subtaskSeq.moveWithin(ctx, tx, ref.CardID, id, ref.Position, to); err != nil {
				return err
			}
			if err := touch(ctx, tx, ref.BoardID, now()); err != nil {
				return err
			}
		}
		out, err = cardSubtasks(ctx, tx, ref.CardID)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// promoteSubtask turns a subtask into a card right below its card (same column, title, due
// date and assignee) and deletes the subtask.
func (m *Module) promoteSubtask(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	cardID := core.NewID()
	var out *Card
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := subtaskFor(tx, r, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		s, err := getSubtask(ctx, tx, id)
		if err != nil {
			return err
		}
		var assignees []string
		if s.AssigneeID != nil {
			assignees = []string{*s.AssigneeID}
		}
		ts := now()
		if err := insertCard(ctx, tx, newCard{ID: cardID, BoardID: ref.BoardID, ColumnID: ref.ColumnID, Title: s.Title,
			Priority: "none", CreatedBy: uid, Position: ref.CardPosition + 1, Assignees: assignees, DueAt: s.DueAt}, ts); err != nil {
			return err
		}
		if s.Done {
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET completed = 1, completed_at = ? WHERE id = ?`, s.CompletedAt, cardID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_subtasks WHERE id = ?`, id); err != nil {
			return err
		}
		if err := subtaskSeq.closeGap(ctx, tx, ref.CardID, ref.Position); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, ref.BoardID, ref.CardID, uid, "subtask.promoted",
			map[string]any{"subtask_id": id, "title": s.Title, "new_card_id": cardID}, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, ref.BoardID, cardID, uid, "card.created",
			map[string]any{"title": s.Title, "column_id": ref.ColumnID, "from_card_id": ref.CardID}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
			return err
		}
		out, err = getCard(ctx, tx, cardID)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}
