package kanban

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// cardInput is the body of card create/update.
type cardInput struct {
	Title             optional[string]   `json:"title"`
	Description       optional[string]   `json:"description"`
	Position          optional[int]      `json:"position"`
	Labels            optional[[]string] `json:"labels"`
	Assignees         optional[[]string] `json:"assignees"`
	Priority          optional[string]   `json:"priority"`
	StartAt           optional[string]   `json:"start_at"`
	DueAt             optional[string]   `json:"due_at"`
	AllDay            optional[bool]     `json:"all_day"`
	Completed         optional[bool]     `json:"completed"`
	CoverAttachmentID optional[string]   `json:"cover_attachment_id"`
	Archived          optional[bool]     `json:"archived"`
	BaseVersion       optional[int64]    `json:"base_version"`
}

// cardFields is a validated cardInput.
type cardFields struct {
	title, description, priority string
	labels, assignees            []string
	startAt, dueAt, cover        *string
}

func (in *cardInput) validate(create bool) (*cardFields, error) {
	fe := fieldErrors{}
	f := &cardFields{priority: "none"}
	if create || in.Title.Set {
		var problem string
		if f.title, problem = cleanLine(in.Title.V, maxTitleLen, true); problem != "" {
			fe.add("title", problem)
		}
	}
	if in.Description.Set && !in.Description.Null {
		var problem string
		if f.description, problem = cleanText(in.Description.V, maxDescriptionLen); problem != "" {
			fe.add("description", problem)
		}
	}
	if in.Labels.Set {
		var problem string
		if f.labels, problem = validIDs(in.Labels.V, MaxLabels); problem != "" {
			fe.add("labels", problem)
		}
	}
	if in.Assignees.Set {
		var problem string
		if f.assignees, problem = validIDs(in.Assignees.V, MaxMembers); problem != "" {
			fe.add("assignees", problem)
		}
	}
	if in.Priority.Set && !in.Priority.Null {
		if !priorities[in.Priority.V] {
			fe.add("priority", "must be none, low, medium, high or urgent")
		}
		f.priority = in.Priority.V
	}
	var problem string
	if f.startAt, problem = optTime(in.StartAt); problem != "" {
		fe.add("start_at", problem)
	}
	if f.dueAt, problem = optTime(in.DueAt); problem != "" {
		fe.add("due_at", problem)
	}
	if in.CoverAttachmentID.Set && !in.CoverAttachmentID.Null && in.CoverAttachmentID.V != "" {
		if !httpx.ValidID(in.CoverAttachmentID.V) {
			fe.add("cover_attachment_id", "must be an image attachment of this card")
		}
		v := in.CoverAttachmentID.V
		f.cover = &v
	}
	for name, o := range map[string]optional[bool]{"all_day": in.AllDay, "completed": in.Completed, "archived": in.Archived} {
		if o.Set && o.Null {
			fe.add(name, "must be true or false")
		}
	}
	return f, fe.err()
}

// checkLinks verifies that labels belong to the board and assignees are members.
func checkLinks(ctx context.Context, tx *sql.Tx, boardID string, labels, assignees []string) error {
	for _, l := range labels {
		var x int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM kanban_labels WHERE id = ? AND board_id = ?`, l, boardID).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			return validation("labels", "contains a label that is not on this board")
		}
		if err != nil {
			return err
		}
	}
	for _, u := range assignees {
		var x int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM kanban_members WHERE board_id = ? AND user_id = ?`, boardID, u).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			return validation("assignees", "contains a user who is not a member of this board")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func setLabels(ctx context.Context, tx *sql.Tx, cardID string, labels []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_card_labels WHERE card_id = ?`, cardID); err != nil {
		return err
	}
	for _, l := range labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_card_labels (card_id, label_id) VALUES (?, ?)`, cardID, l); err != nil {
			return err
		}
	}
	return nil
}

func setAssignees(ctx context.Context, tx *sql.Tx, cardID string, users []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_card_assignees WHERE card_id = ?`, cardID); err != nil {
		return err
	}
	for i, u := range users {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_card_assignees (card_id, user_id, ord) VALUES (?, ?, ?)`, cardID, u, i); err != nil {
			return err
		}
	}
	return nil
}

func checkDates(start, due *string) error {
	if start != nil && due != nil && *due < *start {
		return validation("due_at", "must not be before start_at")
	}
	return nil
}

/* ------------------------------------------------------------------ create */

// insertCard adds a card at position pos of a column (the caller checked access and limits).
type newCard struct {
	ID, BoardID, ColumnID, Title, Description, Priority, CreatedBy string
	Position                                                       int
	Labels, Assignees                                              []string
	StartAt, DueAt                                                 *string
	AllDay                                                         bool
}

func insertCard(ctx context.Context, tx *sql.Tx, c newCard, ts string) error {
	var cards, number int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kanban_cards WHERE board_id = ?`, c.BoardID).Scan(&cards); err != nil {
		return err
	}
	if cards >= MaxCards {
		return tooMany("cards on this board", MaxCards)
	}
	if err := tx.QueryRowContext(ctx, `UPDATE kanban_boards SET next_number = next_number + 1 WHERE id = ? RETURNING next_number - 1`,
		c.BoardID).Scan(&number); err != nil {
		return err
	}
	if err := cardSeq.openGap(ctx, tx, c.ColumnID, c.Position); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_cards (id, board_id, column_id, number, title, description, position, priority,
		start_at, due_at, all_day, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.BoardID, c.ColumnID, number, c.Title, c.Description, c.Position, c.Priority, c.StartAt, c.DueAt,
		db.Bool(c.AllDay), c.CreatedBy, ts, ts); err != nil {
		return err
	}
	if err := setLabels(ctx, tx, c.ID, c.Labels); err != nil {
		return err
	}
	return setAssignees(ctx, tx, c.ID, c.Assignees)
}

func (m *Module) createCard(w http.ResponseWriter, r *http.Request) error {
	columnID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in cardInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f, err := in.validate(true)
	if err != nil {
		return err
	}
	if err := checkDates(f.startAt, f.dueAt); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Card
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		col, rl, err := columnFor(ctx, tx, columnID, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		if err := checkLinks(ctx, tx, col.BoardID, f.labels, f.assignees); err != nil {
			return err
		}
		n, err := cardSeq.count(ctx, tx, columnID)
		if err != nil {
			return err
		}
		pos := n
		if in.Position.Set && !in.Position.Null {
			pos = clamp(in.Position.V, n)
		}
		ts := now()
		if err := insertCard(ctx, tx, newCard{ID: id, BoardID: col.BoardID, ColumnID: columnID, Title: f.title,
			Description: f.description, Priority: f.priority, CreatedBy: uid, Position: pos, Labels: f.labels,
			Assignees: f.assignees, StartAt: f.startAt, DueAt: f.dueAt, AllDay: in.AllDay.V}, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, col.BoardID, id, uid, "card.created",
			map[string]any{"title": f.title, "column_id": columnID, "column": col.Name}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, col.BoardID, ts); err != nil {
			return err
		}
		out, err = getCard(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

/* ------------------------------------------------------------------ detail */

func (m *Module) getCardDetail(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out CardDetail
	err = m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		if _, _, err := cardFor(ctx, tx, id, uid); err != nil {
			return err
		}
		c, err := getCard(ctx, tx, id)
		if err != nil {
			return err
		}
		out.Card = *c
		if out.Subtasks, err = cardSubtasks(ctx, tx, id); err != nil {
			return err
		}
		if out.Attachments, err = cardAttachments(ctx, tx, id); err != nil {
			return err
		}
		comments, err := queryComments(ctx, tx, `SELECT `+commentCols+commentFrom+` WHERE k.card_id = ? ORDER BY k.n DESC LIMIT ?`, id, detailComments)
		if err != nil {
			return err
		}
		slices.Reverse(comments)
		out.Comments = comments
		out.Activity, err = queryActivity(ctx, tx, `SELECT `+activityCols+activityFrom+` WHERE a.board_id = ? AND a.card_id = ?
			ORDER BY a.n DESC LIMIT ?`, c.BoardID, id, detailActivity)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* ------------------------------------------------------------------- patch */

func (m *Module) patchCard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in cardInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f, err := in.validate(false)
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Card
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := cardFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		cur, err := getCard(ctx, tx, id)
		if err != nil {
			return err
		}
		if in.BaseVersion.Set && !in.BaseVersion.Null && in.BaseVersion.V != cur.Version {
			return core.Conflict("The card was changed by someone else").WithDetails(map[string]any{"current": cur})
		}
		if err := checkLinks(ctx, tx, ref.BoardID, f.labels, f.assignees); err != nil {
			return err
		}
		next := *cur
		var fields []string
		mark := func(name string, changed bool) {
			if changed {
				fields = append(fields, name)
			}
		}
		ts := now()
		if in.Title.Set {
			mark("title", f.title != cur.Title)
			next.Title = f.title
		}
		if in.Description.Set {
			mark("description", f.description != cur.Description)
			next.Description = f.description
		}
		if in.Labels.Set {
			mark("labels", !sameSet(f.labels, cur.Labels))
			next.Labels = f.labels
		}
		if in.Assignees.Set {
			mark("assignees", !slices.Equal(f.assignees, cur.Assignees))
			next.Assignees = f.assignees
		}
		if in.Priority.Set {
			mark("priority", f.priority != cur.Priority)
			next.Priority = f.priority
		}
		if in.StartAt.Set {
			mark("start_at", !sameStr(f.startAt, cur.StartAt))
			next.StartAt = f.startAt
		}
		if in.DueAt.Set {
			mark("due_at", !sameStr(f.dueAt, cur.DueAt))
			next.DueAt = f.dueAt
		}
		if in.AllDay.Set {
			mark("all_day", in.AllDay.V != cur.AllDay)
			next.AllDay = in.AllDay.V
		}
		if in.Completed.Set && in.Completed.V != cur.Completed {
			fields = append(fields, "completed")
			next.Completed = in.Completed.V
			next.CompletedAt = nil
			if next.Completed {
				next.CompletedAt = &ts
			}
		}
		if in.CoverAttachmentID.Set {
			if f.cover != nil {
				var mime string
				err := tx.QueryRowContext(ctx, `SELECT mime FROM kanban_attachments WHERE id = ? AND card_id = ?`, *f.cover, id).Scan(&mime)
				if errors.Is(err, sql.ErrNoRows) || (err == nil && !inlineImage(mime)) {
					return validation("cover_attachment_id", "must be an image attachment of this card")
				}
				if err != nil {
					return err
				}
			}
			mark("cover_attachment_id", !sameStr(f.cover, cur.CoverAttachmentID))
			next.CoverAttachmentID = f.cover
		}
		if in.Archived.Set && in.Archived.V != cur.Archived {
			fields = append(fields, "archived")
			next.Archived = in.Archived.V
			next.ArchivedAt = nil
			if next.Archived {
				next.ArchivedAt = &ts
			}
		}
		if err := checkDates(next.StartAt, next.DueAt); err != nil {
			return err
		}
		if len(fields) == 0 {
			out = cur
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET title = ?, description = ?, priority = ?, start_at = ?, due_at = ?,
			all_day = ?, completed = ?, completed_at = ?, cover_attachment_id = ?, archived = ?, archived_at = ?,
			version = version + 1, updated_at = ? WHERE id = ?`,
			next.Title, next.Description, next.Priority, next.StartAt, next.DueAt, db.Bool(next.AllDay), db.Bool(next.Completed),
			next.CompletedAt, next.CoverAttachmentID, db.Bool(next.Archived), next.ArchivedAt, ts, id); err != nil {
			return err
		}
		if slices.Contains(fields, "labels") {
			if err := setLabels(ctx, tx, id, next.Labels); err != nil {
				return err
			}
		}
		if slices.Contains(fields, "assignees") {
			if err := setAssignees(ctx, tx, id, next.Assignees); err != nil {
				return err
			}
		}
		if err := logCardUpdate(ctx, tx, ref.BoardID, id, uid, cur, &next, fields, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
			return err
		}
		out, err = getCard(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// logCardUpdate records a card change: completing/reopening and archiving/restoring get their
// own entries, everything else one card.updated with meta.fields.
func logCardUpdate(ctx context.Context, tx *sql.Tx, boardID, cardID, uid string, cur, next *Card, fields []string, ts string) error {
	var rest []string
	for _, f := range fields {
		switch f {
		case "completed":
			action := "card.reopened"
			if next.Completed {
				action = "card.completed"
			}
			if err := logActivity(ctx, tx, boardID, cardID, uid, action, map[string]any{"title": next.Title}, ts); err != nil {
				return err
			}
		case "archived":
			action := "card.restored"
			if next.Archived {
				action = "card.archived"
			}
			if err := logActivity(ctx, tx, boardID, cardID, uid, action, map[string]any{"title": next.Title}, ts); err != nil {
				return err
			}
		default:
			rest = append(rest, f)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	meta := map[string]any{"title": next.Title, "fields": rest}
	if cur.Title != next.Title {
		meta["old_title"] = cur.Title
	}
	return logActivity(ctx, tx, boardID, cardID, uid, "card.updated", meta, ts)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

/* ------------------------------------------------------------- delete/move */

func (m *Module) deleteCard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := cardFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_cards WHERE id = ?`, id); err != nil {
			return err
		}
		if err := cardSeq.closeGap(ctx, tx, ref.ColumnID, ref.Position); err != nil {
			return err
		}
		ts := now()
		if err := logActivity(ctx, tx, ref.BoardID, id, uid, "card.deleted", map[string]any{"title": ref.Title, "number": ref.Number}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, ref.BoardID, ts)
	})
	if err != nil {
		return err
	}
	m.kickGC()
	return httpx.NoContent(w)
}

func (m *Module) moveCard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		ColumnID string        `json:"column_id"`
		Position optional[int] `json:"position"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if !httpx.ValidID(in.ColumnID) {
		return validation("column_id", "is required")
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Card
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, rl, err := cardFor(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		var toName, fromName string
		var toBoard string
		err = tx.QueryRowContext(ctx, `SELECT board_id, name FROM kanban_columns WHERE id = ?`, in.ColumnID).Scan(&toBoard, &toName)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && toBoard != ref.BoardID) {
			return validation("column_id", "must be a column of the card's board")
		}
		if err != nil {
			return err
		}
		ts := now()
		if in.ColumnID == ref.ColumnID {
			n, err := cardSeq.count(ctx, tx, ref.ColumnID)
			if err != nil {
				return err
			}
			to := n - 1
			if in.Position.Set && !in.Position.Null {
				to = clamp(in.Position.V, n-1)
			}
			if to != ref.Position {
				if err := cardSeq.moveWithin(ctx, tx, ref.ColumnID, id, ref.Position, to); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET updated_at = ? WHERE id = ?`, ts, id); err != nil {
					return err
				}
				if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
					return err
				}
			}
		} else {
			if err := tx.QueryRowContext(ctx, `SELECT name FROM kanban_columns WHERE id = ?`, ref.ColumnID).Scan(&fromName); err != nil {
				return err
			}
			n, err := cardSeq.count(ctx, tx, in.ColumnID)
			if err != nil {
				return err
			}
			to := n
			if in.Position.Set && !in.Position.Null {
				to = clamp(in.Position.V, n)
			}
			if err := cardSeq.closeGap(ctx, tx, ref.ColumnID, ref.Position); err != nil {
				return err
			}
			if err := cardSeq.openGap(ctx, tx, in.ColumnID, to); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET column_id = ?, position = ?, updated_at = ? WHERE id = ?`,
				in.ColumnID, to, ts, id); err != nil {
				return err
			}
			if err := logActivity(ctx, tx, ref.BoardID, id, uid, "card.moved", map[string]any{
				"title": ref.Title, "from_column": fromName, "to_column": toName,
				"from_column_id": ref.ColumnID, "to_column_id": in.ColumnID,
			}, ts); err != nil {
				return err
			}
			if err := touch(ctx, tx, ref.BoardID, ts); err != nil {
				return err
			}
		}
		out, err = getCard(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* -------------------------------------------------------------------- list */

// cardsCursor is the keyset of GET /kanban/cards (the sort key of the last item) plus a hash
// of the filter, so a cursor cannot be replayed against another query.
type cardsCursor struct {
	F       string `json:"f"`
	NoDue   int    `json:"nd"`
	Due     string `json:"d"`
	Board   string `json:"bn"`
	BoardID string `json:"b"`
	ColPos  int    `json:"cp"`
	Pos     int    `json:"p"`
	ID      string `json:"i"`
}

var numberQuery = regexp.MustCompile(`^#?(\d{1,9})$`)

func (m *Module) listCards(w http.ResponseWriter, r *http.Request) error {
	qv := r.URL.Query()
	ctx := r.Context()
	uid := uidOf(r)
	limit := httpx.ParseLimit(r, 50, 200)
	where := []string{"me.user_id = ?"}
	args := []any{uid}
	boardID := qv.Get("board_id")
	if boardID != "" {
		if !httpx.ValidID(boardID) {
			return errBoardNotFound
		}
		if _, err := memberRole(ctx, m.d.DB.SQL, boardID, uid); err != nil {
			return err
		}
		where = append(where, "c.board_id = ?")
		args = append(args, boardID)
	} else {
		where = append(where, "b.archived = 0") // cards of archived boards only show when asked for by board
	}
	if q := strings.TrimSpace(qv.Get("q")); q != "" {
		if len(q) > 200 {
			return validation("q", "must be at most 200 characters")
		}
		pat := "%" + db.EscapeLike(q) + "%"
		cond := `(c.title LIKE ? ESCAPE '\' OR c.description LIKE ? ESCAPE '\'`
		args = append(args, pat, pat)
		if mm := numberQuery.FindStringSubmatch(q); mm != nil {
			n, _ := strconv.Atoi(mm[1])
			cond += ` OR c.number = ?`
			args = append(args, n)
		}
		where = append(where, cond+")")
	}
	if l := qv.Get("label"); l != "" {
		if !httpx.ValidID(l) {
			return validation("label", "is not a valid id")
		}
		where = append(where, "EXISTS (SELECT 1 FROM kanban_card_labels cl WHERE cl.card_id = c.id AND cl.label_id = ?)")
		args = append(args, l)
	}
	if a := qv.Get("assignee"); a != "" {
		if a == "me" {
			a = uid
		} else if !httpx.ValidID(a) {
			return validation("assignee", "must be me or a user id")
		}
		where = append(where, "EXISTS (SELECT 1 FROM kanban_card_assignees ca WHERE ca.card_id = c.id AND ca.user_id = ?)")
		args = append(args, a)
	}
	t := core.Now()
	ts := db.FormatTime(t)
	switch qv.Get("due") {
	case "":
	case "overdue":
		where = append(where, "c.due_at IS NOT NULL AND c.due_at < ? AND c.completed = 0")
		args = append(args, ts)
	case "today":
		where = append(where, "c.due_at >= ? AND c.due_at < ?")
		args = append(args, ts, db.FormatTime(t.Add(24*time.Hour)))
	case "week":
		where = append(where, "c.due_at >= ? AND c.due_at < ?")
		args = append(args, ts, db.FormatTime(t.Add(7*24*time.Hour)))
	case "none":
		where = append(where, "c.due_at IS NULL")
	case "any":
		where = append(where, "c.due_at IS NOT NULL")
	default:
		return validation("due", "must be overdue, today, week, none or any")
	}
	completed, err := queryBoolPtr(r, "completed")
	if err != nil {
		return err
	}
	if completed != nil {
		where = append(where, "c.completed = ?")
		args = append(args, db.Bool(*completed))
	}
	archived, err := queryBoolPtr(r, "archived")
	if err != nil {
		return err
	}
	where = append(where, "c.archived = ?")
	args = append(args, db.Bool(archived != nil && *archived))

	// The filter fingerprint binds cursors to this query (the due windows move with the
	// clock, so they are keyed by name, not by their bounds).
	filter := strings.Join([]string{boardID, qv.Get("q"), qv.Get("label"), qv.Get("assignee"), qv.Get("due"),
		qv.Get("completed"), qv.Get("archived")}, "\x1f")
	key := `(CASE WHEN c.due_at IS NULL THEN 1 ELSE 0 END), COALESCE(c.due_at, ''), lower(b.name), c.board_id, col.position, c.position, c.id`
	if cur := qv.Get("cursor"); cur != "" {
		var cc cardsCursor
		if err := httpx.DecodeCursor(cur, &cc); err != nil || cc.F != filter {
			return httpx.ErrInvalidCursor
		}
		where = append(where, "("+key+") > (?, ?, ?, ?, ?, ?, ?)")
		args = append(args, cc.NoDue, cc.Due, cc.Board, cc.BoardID, cc.ColPos, cc.Pos, cc.ID)
	}
	query := cardSelect(true) + `, b.name, col.name, (CASE WHEN c.due_at IS NULL THEN 1 ELSE 0 END), COALESCE(c.due_at, ''), lower(b.name), col.position
		FROM kanban_cards c
		JOIN kanban_boards b ON b.id = c.board_id
		JOIN kanban_members me ON me.board_id = c.board_id
		JOIN kanban_columns col ON col.id = c.column_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + key + ` LIMIT ?`
	rows, err := m.d.DB.SQL.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []CardListItem{}
	var keys []cardsCursor
	for rows.Next() {
		var it CardListItem
		var k cardsCursor
		c, err := scanCard(rows, &it.BoardName, &it.ColumnName, &k.NoDue, &k.Due, &k.Board, &k.ColPos)
		if err != nil {
			return err
		}
		it.Card = *c
		k.F, k.BoardID, k.Pos, k.ID = filter, c.BoardID, c.Position, c.ID
		items = append(items, it)
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = httpx.EncodeCursor(keys[limit-1])
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}
