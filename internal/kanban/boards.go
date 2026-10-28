package kanban

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

func uidOf(r *http.Request) string { return core.MustPrincipal(r.Context()).UserID }

/* ------------------------------------------------------------------ list */

func (m *Module) listBoards(w http.ResponseWriter, r *http.Request) error {
	archived, err := queryBoolPtr(r, "archived")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	ts := now()
	var out []BoardSummary
	err = m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT b.id, b.name, substr(b.description, 1, 200), b.color, b.owner_id, b.archived,
			b.created_at, b.updated_at, b.version, me.role,
			(SELECT count(*) FROM kanban_members x WHERE x.board_id = b.id),
			(SELECT count(*) FROM kanban_cards c WHERE c.board_id = b.id AND c.archived = 0),
			(SELECT count(*) FROM kanban_cards c WHERE c.board_id = b.id AND c.archived = 0 AND c.completed = 0),
			(SELECT count(*) FROM kanban_cards c WHERE c.board_id = b.id AND c.archived = 0 AND c.completed = 0
				AND c.due_at IS NOT NULL AND c.due_at < ?)
			FROM kanban_members me JOIN kanban_boards b ON b.id = me.board_id
			WHERE me.user_id = ? AND b.archived = ? ORDER BY lower(b.name), b.id`, ts, uid, db.Bool(archived != nil && *archived))
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []BoardSummary{}
		for rows.Next() {
			var b BoardSummary
			if err := rows.Scan(&b.ID, &b.Name, &b.Description, &b.Color, &b.OwnerID, &b.Archived, &b.CreatedAt, &b.UpdatedAt,
				&b.Version, &b.Role, &b.MemberCount, &b.CardCount, &b.OpenCardCount, &b.OverdueCount); err != nil {
				return err
			}
			b.Members = []Member{}
			out = append(out, b)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if len(out) == 0 {
			return nil
		}
		idx := make(map[string]int, len(out))
		for i, b := range out {
			idx[b.ID] = i
		}
		mrows, err := tx.QueryContext(ctx, `SELECT m.board_id, m.user_id, u.username, COALESCE(u.display_name, ''), m.role
			FROM kanban_members m JOIN users u ON u.id = m.user_id
			WHERE m.board_id IN (SELECT board_id FROM kanban_members WHERE user_id = ?) ORDER BY m.board_id, `+memberOrder, uid)
		if err != nil {
			return err
		}
		defer mrows.Close()
		for mrows.Next() {
			var boardID string
			var mb Member
			if err := mrows.Scan(&boardID, &mb.UserID, &mb.Username, &mb.DisplayName, &mb.Role); err != nil {
				return err
			}
			if i, ok := idx[boardID]; ok {
				out[i].Members = append(out[i].Members, mb)
			}
		}
		return mrows.Err()
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* ---------------------------------------------------------------- create */

var templates = map[string][]string{
	"basic": {"To do", "In progress", "Done"},
	"blank": nil,
}

func (m *Module) createBoard(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name        string           `json:"name"`
		Description string           `json:"description"`
		Color       optional[string] `json:"color"`
		Template    optional[string] `json:"template"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	name, problem := cleanLine(in.Name, maxNameLen, true)
	if problem != "" {
		fe.add("name", problem)
	}
	desc, problem := cleanText(in.Description, maxDescriptionLen)
	if problem != "" {
		fe.add("description", problem)
	}
	color := "blue"
	if in.Color.Set && !in.Color.Null && in.Color.V != "" {
		if !colors[in.Color.V] {
			fe.add("color", "must be one of "+colorList)
		}
		color = in.Color.V
	}
	tpl := "basic"
	if in.Template.Set && !in.Template.Null && in.Template.V != "" {
		tpl = in.Template.V
	}
	columns, ok := templates[tpl]
	if !ok {
		fe.add("template", "must be basic or blank")
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Board
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kanban_boards WHERE owner_id = ?`, uid).Scan(&n); err != nil {
			return err
		}
		if n >= MaxBoardsPerUser {
			return tooMany("boards", MaxBoardsPerUser)
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_boards (id, owner_id, name, description, color, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, uid, name, desc, color, ts, ts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_members (board_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)`,
			id, uid, ts); err != nil {
			return err
		}
		for i, c := range columns {
			if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_columns (id, board_id, name, position, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)`, core.NewID(), id, c, i, ts, ts); err != nil {
				return err
			}
		}
		if err := logActivity(ctx, tx, id, "", uid, "board.created", map[string]any{"name": name, "template": tpl}, ts); err != nil {
			return err
		}
		var err error
		out, err = loadBoard(ctx, tx, id, uid, false)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

/* ------------------------------------------------------------------- get */

// etagMatches reports whether an If-None-Match header lists etag (or is "*"); weak
// validators compare equal to strong ones (RFC 9110 weak comparison).
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || strings.TrimPrefix(p, "W/") == etag {
			return true
		}
	}
	return false
}

func (m *Module) getBoard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	includeArchived := httpx.QueryBool(r, "include_archived")
	inm := r.Header.Get("If-None-Match")
	var out *Board
	notModified := ""
	err = m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		if inm != "" {
			// Cheap path for polling clients: answer 304 before assembling anything.
			var version int64
			err := tx.QueryRowContext(ctx, `SELECT b.version FROM kanban_boards b
				JOIN kanban_members m ON m.board_id = b.id AND m.user_id = ? WHERE b.id = ?`, uid, id).Scan(&version)
			if errors.Is(err, sql.ErrNoRows) {
				return errBoardNotFound
			}
			if err != nil {
				return err
			}
			if etag := boardETag(version); etagMatches(inm, etag) {
				notModified = etag
				return nil
			}
		}
		var err error
		out, err = loadBoard(ctx, tx, id, uid, includeArchived)
		return err
	})
	if err != nil {
		return err
	}
	if notModified != "" {
		w.Header().Set("ETag", notModified)
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	w.Header().Set("ETag", boardETag(out.Version))
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func boardETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// writeBoard answers a board write with the board as the caller now sees it.
func (m *Module) writeBoard(w http.ResponseWriter, status int, b *Board) error {
	w.Header().Set("ETag", boardETag(b.Version))
	return httpx.WriteJSON(w, status, b)
}

/* ----------------------------------------------------------------- patch */

func (m *Module) patchBoard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name        optional[string] `json:"name"`
		Description optional[string] `json:"description"`
		Color       optional[string] `json:"color"`
		Archived    optional[bool]   `json:"archived"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := fieldErrors{}
	var name, desc, color string
	if in.Name.Set {
		var problem string
		if name, problem = cleanLine(in.Name.V, maxNameLen, true); problem != "" {
			fe.add("name", problem)
		}
	}
	if in.Description.Set {
		var problem string
		if desc, problem = cleanText(in.Description.V, maxDescriptionLen); problem != "" {
			fe.add("description", problem)
		}
	}
	if in.Color.Set {
		color = in.Color.V
		if !colors[color] {
			fe.add("color", "must be one of "+colorList)
		}
	}
	if in.Archived.Set && in.Archived.Null {
		fe.add("archived", "must be true or false")
	}
	if err := fe.err(); err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Board
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleAdmin); err != nil {
			return err
		}
		var curName, curDesc, curColor string
		var curArchived bool
		if err := tx.QueryRowContext(ctx, `SELECT name, description, color, archived FROM kanban_boards WHERE id = ?`, id).
			Scan(&curName, &curDesc, &curColor, &curArchived); err != nil {
			return err
		}
		var fields []string
		ts := now()
		if in.Name.Set && name != curName {
			fields = append(fields, "name")
			curName = name
		}
		if in.Description.Set && desc != curDesc {
			fields = append(fields, "description")
			curDesc = desc
		}
		if in.Color.Set && color != curColor {
			fields = append(fields, "color")
			curColor = color
		}
		archivedChanged := in.Archived.Set && in.Archived.V != curArchived
		if archivedChanged {
			curArchived = in.Archived.V
		}
		if len(fields) > 0 || archivedChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_boards SET name = ?, description = ?, color = ?, archived = ? WHERE id = ?`,
				curName, curDesc, curColor, db.Bool(curArchived), id); err != nil {
				return err
			}
			if err := touch(ctx, tx, id, ts); err != nil {
				return err
			}
		}
		if len(fields) > 0 {
			if err := logActivity(ctx, tx, id, "", uid, "board.updated", map[string]any{"fields": fields, "name": curName}, ts); err != nil {
				return err
			}
		}
		if archivedChanged {
			action := "board.restored"
			if in.Archived.V {
				action = "board.archived"
			}
			if err := logActivity(ctx, tx, id, "", uid, action, nil, ts); err != nil {
				return err
			}
		}
		out, err = loadBoard(ctx, tx, id, uid, false)
		return err
	})
	if err != nil {
		return err
	}
	return m.writeBoard(w, http.StatusOK, out)
}

/* ---------------------------------------------------------------- delete */

func (m *Module) deleteBoard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleOwner); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM kanban_boards WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return err
	}
	m.kickGC()
	return httpx.NoContent(w)
}

/* --------------------------------------------------------------- members */

// unassign removes uid from every card and subtask of the board (a member who leaves or is
// removed cannot stay assigned). The cards' versions move on: assignees are a PATCH field.
func unassign(ctx context.Context, tx *sql.Tx, boardID, uid, ts string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE kanban_cards SET version = version + 1, updated_at = ?
		WHERE board_id = ? AND id IN (SELECT card_id FROM kanban_card_assignees WHERE user_id = ?)`, ts, boardID, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_card_assignees
		WHERE user_id = ? AND card_id IN (SELECT id FROM kanban_cards WHERE board_id = ?)`, uid, boardID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE kanban_subtasks SET assignee_id = NULL, updated_at = ?
		WHERE assignee_id = ? AND card_id IN (SELECT id FROM kanban_cards WHERE board_id = ?)`, ts, uid, boardID)
	return err
}

func (m *Module) putMembers(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Members *[]struct {
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		} `json:"members"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Members == nil {
		return validation("members", "is required")
	}
	want := map[string]string{}
	var order []string
	for _, e := range *in.Members {
		if !httpx.ValidID(e.UserID) {
			return validation("members", "contains an invalid user id")
		}
		if e.Role != "admin" && e.Role != "editor" && e.Role != "viewer" {
			return validation("members", "role must be admin, editor or viewer")
		}
		if _, dup := want[e.UserID]; dup {
			return validation("members", "lists a user twice")
		}
		want[e.UserID] = e.Role
		order = append(order, e.UserID)
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Board
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleAdmin); err != nil {
			return err
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM kanban_boards WHERE id = ?`, id).Scan(&owner); err != nil {
			return err
		}
		delete(want, owner) // the owner stays the owner
		if len(want)+1 > MaxMembers {
			return validation("members", "a board has at most "+itoa(MaxMembers)+" members")
		}
		if _, ok := want[uid]; !ok && uid != owner {
			return validation("members", "you cannot remove yourself; leave the board instead")
		}
		current := map[string]string{}
		rows, err := tx.QueryContext(ctx, `SELECT user_id, role FROM kanban_members WHERE board_id = ? AND role <> 'owner'`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u, rr string
			if err := rows.Scan(&u, &rr); err != nil {
				rows.Close()
				return err
			}
			current[u] = rr
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ts := now()
		changed := false
		for u, oldRole := range current {
			if _, keep := want[u]; keep {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_members WHERE board_id = ? AND user_id = ?`, id, u); err != nil {
				return err
			}
			if err := unassign(ctx, tx, id, u, ts); err != nil {
				return err
			}
			if err := logActivity(ctx, tx, id, "", uid, "member.removed", map[string]any{"user_id": u, "role": oldRole}, ts); err != nil {
				return err
			}
			changed = true
		}
		for _, u := range order {
			newRole, ok := want[u]
			if !ok {
				continue // the owner
			}
			oldRole, isMember := current[u]
			switch {
			case !isMember:
				var exists int
				err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, u).Scan(&exists)
				if errors.Is(err, sql.ErrNoRows) {
					return validation("members", "lists an unknown user")
				}
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_members (board_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
					id, u, newRole, ts); err != nil {
					return err
				}
				if err := logActivity(ctx, tx, id, "", uid, "member.added", map[string]any{"user_id": u, "role": newRole}, ts); err != nil {
					return err
				}
				changed = true
			case oldRole != newRole:
				if _, err := tx.ExecContext(ctx, `UPDATE kanban_members SET role = ? WHERE board_id = ? AND user_id = ?`, newRole, id, u); err != nil {
					return err
				}
				if err := logActivity(ctx, tx, id, "", uid, "member.role_changed",
					map[string]any{"user_id": u, "role": newRole, "old_role": oldRole}, ts); err != nil {
					return err
				}
				changed = true
			}
		}
		if changed {
			if err := touch(ctx, tx, id, ts); err != nil {
				return err
			}
		}
		out, err = loadBoard(ctx, tx, id, uid, false)
		return err
	})
	if err != nil {
		return err
	}
	return m.writeBoard(w, http.StatusOK, out)
}

func (m *Module) leaveBoard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if rl == roleOwner {
			return core.Conflict("The owner cannot leave the board; transfer it to another member first")
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_members WHERE board_id = ? AND user_id = ?`, id, uid); err != nil {
			return err
		}
		if err := unassign(ctx, tx, id, uid, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, id, "", uid, "member.left", map[string]any{"user_id": uid, "role": rl.String()}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, id, ts)
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) transferBoard(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		UserID string `json:"user_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if !httpx.ValidID(in.UserID) {
		return validation("user_id", "is required")
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Board
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		rl, err := memberRole(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleOwner); err != nil {
			return err
		}
		if in.UserID == uid {
			return validation("user_id", "you already own this board")
		}
		target, err := memberRole(ctx, tx, id, in.UserID)
		if errors.Is(err, errBoardNotFound) {
			return validation("user_id", "must be a member of the board")
		}
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kanban_boards WHERE owner_id = ?`, in.UserID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxBoardsPerUser {
			return core.Conflict("That member already owns the maximum number of boards (" + itoa(MaxBoardsPerUser) + ")")
		}
		ts := now()
		// Demote first: the partial unique index allows one owner row per board.
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_members SET role = 'admin' WHERE board_id = ? AND user_id = ?`, id, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_members SET role = 'owner' WHERE board_id = ? AND user_id = ?`, id, in.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kanban_boards SET owner_id = ? WHERE id = ?`, in.UserID, id); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, id, "", uid, "board.transferred",
			map[string]any{"from": uid, "to": in.UserID, "old_role": target.String()}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, id, ts); err != nil {
			return err
		}
		out, err = loadBoard(ctx, tx, id, uid, false)
		return err
	})
	if err != nil {
		return err
	}
	return m.writeBoard(w, http.StatusOK, out)
}

/* -------------------------------------------------------------- activity */

type activityCursor struct {
	B string `json:"b"`
	C string `json:"c,omitempty"`
	N int64  `json:"n"`
}

func (m *Module) boardActivity(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cardID := r.URL.Query().Get("card_id")
	if cardID != "" && !httpx.ValidID(cardID) {
		return validation("card_id", "is not a valid id")
	}
	limit := httpx.ParseLimit(r, 50, 200)
	before := int64(-1)
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur activityCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil || cur.B != id || cur.C != cardID {
			return httpx.ErrInvalidCursor
		}
		before = cur.N
	}
	if _, err := memberRole(ctx, m.d.DB.SQL, id, uidOf(r)); err != nil {
		return err
	}
	query := `SELECT ` + activityCols + activityFrom + ` WHERE a.board_id = ?`
	args := []any{id}
	if cardID != "" {
		query += ` AND a.card_id = ?`
		args = append(args, cardID)
	}
	if before >= 0 {
		query += ` AND a.n < ?`
		args = append(args, before)
	}
	items, err := queryActivity(ctx, m.d.DB.SQL, query+` ORDER BY a.n DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = httpx.EncodeCursor(activityCursor{B: id, C: cardID, N: items[len(items)-1].n})
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// pruneActivity keeps the newest maxActivityPerBoard entries of every board.
func (m *Module) pruneActivity(ctx context.Context) error {
	rows, err := m.d.DB.SQL.QueryContext(ctx, `SELECT board_id FROM kanban_activity GROUP BY board_id HAVING count(*) > ?`, maxActivityPerBoard)
	if err != nil {
		return err
	}
	var boards []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		boards = append(boards, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range boards {
		if _, err := m.d.DB.SQL.ExecContext(ctx, `DELETE FROM kanban_activity WHERE board_id = ? AND n <
			(SELECT n FROM kanban_activity WHERE board_id = ? ORDER BY n DESC LIMIT 1 OFFSET ?)`, b, b, maxActivityPerBoard-1); err != nil {
			return err
		}
	}
	return nil
}

/* ----------------------------------------------------------------- users */

func (m *Module) searchUsers(w http.ResponseWriter, r *http.Request) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []UserResult{}
	if utf8.RuneCountInString(q) < 2 || len(q) > 200 {
		return httpx.WriteJSON(w, http.StatusOK, out)
	}
	pattern := db.EscapeLike(q) + "%"
	rows, err := m.d.DB.SQL.QueryContext(r.Context(), `SELECT id, username, COALESCE(display_name, '') FROM users
		WHERE disabled = 0 AND (username LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\')
		ORDER BY lower(username), id LIMIT ?`, pattern, pattern, maxUserResults)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u UserResult
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName); err != nil {
			return err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}
