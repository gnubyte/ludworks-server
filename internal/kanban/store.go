package kanban

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

/* ------------------------------------------------------------------ access */

// memberRole returns uid's role on a board. A missing board and a board uid is not a member of
// are both 404: boards are visible only to their members (server admins included).
func memberRole(ctx context.Context, q db.Querier, boardID, uid string) (role, error) {
	var r string
	err := q.QueryRowContext(ctx, `SELECT role FROM kanban_members WHERE board_id = ? AND user_id = ?`, boardID, uid).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return roleNone, errBoardNotFound
	}
	if err != nil {
		return roleNone, err
	}
	return parseRole(r), nil
}

// cardRef is the part of a card the write paths need.
type cardRef struct {
	ID, BoardID, ColumnID, Title string
	Position, Number             int
	Version                      int64
	Archived                     bool
}

// cardFor loads a card and uid's role on its board (404 unless uid is a member).
func cardFor(ctx context.Context, q db.Querier, cardID, uid string) (*cardRef, role, error) {
	var c cardRef
	var r sql.NullString
	err := q.QueryRowContext(ctx, `SELECT c.id, c.board_id, c.column_id, c.title, c.position, c.number, c.version, c.archived, m.role
		FROM kanban_cards c LEFT JOIN kanban_members m ON m.board_id = c.board_id AND m.user_id = ?
		WHERE c.id = ?`, uid, cardID).Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Position, &c.Number, &c.Version, &c.Archived, &r)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.Valid) {
		return nil, roleNone, errCardNotFound
	}
	if err != nil {
		return nil, roleNone, err
	}
	return &c, parseRole(r.String), nil
}

// columnRef is the part of a column the write paths need.
type columnRef struct {
	ID, BoardID, Name string
	Position          int
}

func columnFor(ctx context.Context, q db.Querier, columnID, uid string) (*columnRef, role, error) {
	var c columnRef
	var r sql.NullString
	err := q.QueryRowContext(ctx, `SELECT c.id, c.board_id, c.name, c.position, m.role
		FROM kanban_columns c LEFT JOIN kanban_members m ON m.board_id = c.board_id AND m.user_id = ?
		WHERE c.id = ?`, uid, columnID).Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &r)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.Valid) {
		return nil, roleNone, errColumnNotFound
	}
	if err != nil {
		return nil, roleNone, err
	}
	return &c, parseRole(r.String), nil
}

// lookupBoard runs query (args: uid, id; it selects the board id and uid's member role) to find
// the board an object belongs to. notFound is returned for missing objects and for boards uid
// is not a member of. The SQL is a constant of each call site, never user input.
func lookupBoard(ctx context.Context, q db.Querier, query, id, uid string, notFound error) (string, role, error) {
	var boardID string
	var r sql.NullString
	err := q.QueryRowContext(ctx, query, uid, id).Scan(&boardID, &r)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.Valid) {
		return "", roleNone, notFound
	}
	if err != nil {
		return "", roleNone, err
	}
	return boardID, parseRole(r.String), nil
}

/* --------------------------------------------------------- versions & log */

// touch bumps a board's version (its ETag) and updated_at. Every change to a board or anything
// on it calls it inside the write transaction.
func touch(ctx context.Context, tx *sql.Tx, boardID, ts string) error {
	_, err := tx.ExecContext(ctx, `UPDATE kanban_boards SET version = version + 1, updated_at = ? WHERE id = ?`, ts, boardID)
	return err
}

// logActivity appends an entry to the board's activity log.
func logActivity(ctx context.Context, tx *sql.Tx, boardID, cardID, actor, action string, meta map[string]any, ts string) error {
	if meta == nil {
		meta = map[string]any{}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var card any
	if cardID != "" {
		card = cardID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kanban_activity (id, board_id, card_id, actor_id, action, meta, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, core.NewID(), boardID, card, actor, action, string(b), ts)
	return err
}

/* ------------------------------------------------------------------- cards */

// cardSelect is the SELECT list of a card with its links and counters. short cuts the
// description for lists (substr counts characters).
func cardSelect(short bool) string {
	desc := "c.description"
	if short {
		desc = "substr(c.description, 1, 200)"
	}
	return `SELECT c.id, c.board_id, c.column_id, c.number, c.title, ` + desc + `, c.position, c.priority,
		c.start_at, c.due_at, c.all_day, c.completed, c.completed_at, c.cover_attachment_id, c.archived, c.archived_at,
		c.created_by, c.created_at, c.updated_at, c.version,
		(SELECT group_concat(l.id, ',' ORDER BY l.name_key, l.id) FROM kanban_card_labels cl
			JOIN kanban_labels l ON l.id = cl.label_id WHERE cl.card_id = c.id),
		(SELECT group_concat(a.user_id, ',' ORDER BY a.ord, a.user_id) FROM kanban_card_assignees a WHERE a.card_id = c.id),
		(SELECT count(*) FROM kanban_subtasks s WHERE s.card_id = c.id),
		(SELECT count(*) FROM kanban_subtasks s WHERE s.card_id = c.id AND s.done = 1),
		(SELECT count(*) FROM kanban_comments k WHERE k.card_id = c.id),
		(SELECT count(*) FROM kanban_attachments t WHERE t.card_id = c.id)`
}

type scanner interface{ Scan(dest ...any) error }

func scanCard(sc scanner, extra ...any) (*Card, error) {
	var c Card
	var start, due, completedAt, cover, archivedAt, labels, assignees sql.NullString
	dest := []any{&c.ID, &c.BoardID, &c.ColumnID, &c.Number, &c.Title, &c.Description, &c.Position, &c.Priority,
		&start, &due, &c.AllDay, &c.Completed, &completedAt, &cover, &c.Archived, &archivedAt,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.Version,
		&labels, &assignees, &c.SubtasksTotal, &c.SubtasksDone, &c.CommentCount, &c.AttachmentCount}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	c.StartAt, c.DueAt, c.CompletedAt = strPtr(start), strPtr(due), strPtr(completedAt)
	c.CoverAttachmentID, c.ArchivedAt = strPtr(cover), strPtr(archivedAt)
	c.Labels, c.Assignees = splitIDs(labels), splitIDs(assignees)
	return &c, nil
}

func strPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func splitIDs(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return []string{}
	}
	return strings.Split(s.String, ",")
}

// getCard loads one card in full.
func getCard(ctx context.Context, q db.Querier, id string) (*Card, error) {
	c, err := scanCard(q.QueryRowContext(ctx, cardSelect(false)+` FROM kanban_cards c WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errCardNotFound
	}
	return c, err
}

// boardCards lists a board's cards by column and position (descriptions cut).
func boardCards(ctx context.Context, q db.Querier, boardID string, includeArchived bool) ([]Card, error) {
	query := cardSelect(true) + ` FROM kanban_cards c JOIN kanban_columns col ON col.id = c.column_id WHERE c.board_id = ?`
	if !includeArchived {
		query += ` AND c.archived = 0`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY col.position, c.position, c.id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

/* ------------------------------------------------- columns, labels, members */

const columnCols = `c.id, c.board_id, c.name, c.position, c.color, c.wip_limit, c.collapsed, c.created_at, c.updated_at,
	(SELECT count(*) FROM kanban_cards k WHERE k.column_id = c.id AND k.archived = 0)`

func scanColumn(sc scanner) (*Column, error) {
	var c Column
	var color sql.NullString
	var wip sql.NullInt64
	if err := sc.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &color, &wip, &c.Collapsed, &c.CreatedAt, &c.UpdatedAt, &c.CardCount); err != nil {
		return nil, err
	}
	c.Color = strPtr(color)
	if wip.Valid {
		n := int(wip.Int64)
		c.WIPLimit = &n
	}
	return &c, nil
}

func getColumn(ctx context.Context, q db.Querier, id string) (*Column, error) {
	c, err := scanColumn(q.QueryRowContext(ctx, `SELECT `+columnCols+` FROM kanban_columns c WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errColumnNotFound
	}
	return c, err
}

func boardColumns(ctx context.Context, q db.Querier, boardID string) ([]Column, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+columnCols+` FROM kanban_columns c WHERE c.board_id = ? ORDER BY c.position, c.id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Column{}
	for rows.Next() {
		c, err := scanColumn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func boardLabels(ctx context.Context, q db.Querier, boardID string) ([]Label, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, color FROM kanban_labels WHERE board_id = ? ORDER BY name_key, id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func getLabel(ctx context.Context, q db.Querier, id string) (*Label, error) {
	var l Label
	err := q.QueryRowContext(ctx, `SELECT id, name, color FROM kanban_labels WHERE id = ?`, id).Scan(&l.ID, &l.Name, &l.Color)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errLabelNotFound
	}
	return &l, err
}

// memberOrder sorts the owner first, then admins, editors and viewers, by name.
const memberOrder = `CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'editor' THEN 2 ELSE 3 END,
	lower(COALESCE(NULLIF(u.display_name, ''), u.username)), u.id`

func boardMembers(ctx context.Context, q db.Querier, boardID string) ([]Member, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.user_id, u.username, COALESCE(u.display_name, ''), m.role
		FROM kanban_members m JOIN users u ON u.id = m.user_id WHERE m.board_id = ? ORDER BY `+memberOrder, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var mb Member
		if err := rows.Scan(&mb.UserID, &mb.Username, &mb.DisplayName, &mb.Role); err != nil {
			return nil, err
		}
		out = append(out, mb)
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------------- board */

// loadBoard assembles GET /kanban/boards/{id} for uid. Callers run it in one transaction so
// the version matches the content.
func loadBoard(ctx context.Context, q db.Querier, boardID, uid string, includeArchived bool) (*Board, error) {
	b := &Board{}
	var r sql.NullString
	err := q.QueryRowContext(ctx, `SELECT b.id, b.name, b.description, b.color, b.owner_id, b.archived, b.created_at, b.updated_at, b.version, m.role,
		(SELECT count(*) FROM kanban_cards c WHERE c.board_id = b.id AND c.archived = 0)
		FROM kanban_boards b LEFT JOIN kanban_members m ON m.board_id = b.id AND m.user_id = ? WHERE b.id = ?`, uid, boardID).
		Scan(&b.ID, &b.Name, &b.Description, &b.Color, &b.OwnerID, &b.Archived, &b.CreatedAt, &b.UpdatedAt, &b.Version, &r, &b.CardCount)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.Valid) {
		return nil, errBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Role = r.String
	if b.Members, err = boardMembers(ctx, q, boardID); err != nil {
		return nil, err
	}
	if b.Labels, err = boardLabels(ctx, q, boardID); err != nil {
		return nil, err
	}
	if b.Columns, err = boardColumns(ctx, q, boardID); err != nil {
		return nil, err
	}
	if b.Cards, err = boardCards(ctx, q, boardID, includeArchived); err != nil {
		return nil, err
	}
	return b, nil
}

/* ---------------------------------------------------------------- subtasks */

const subtaskCols = `id, card_id, title, done, position, due_at, assignee_id, completed_at, created_at, updated_at`

func scanSubtask(sc scanner) (*Subtask, error) {
	var s Subtask
	var due, assignee, completed sql.NullString
	if err := sc.Scan(&s.ID, &s.CardID, &s.Title, &s.Done, &s.Position, &due, &assignee, &completed, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.DueAt, s.AssigneeID, s.CompletedAt = strPtr(due), strPtr(assignee), strPtr(completed)
	return &s, nil
}

func cardSubtasks(ctx context.Context, q db.Querier, cardID string) ([]Subtask, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+subtaskCols+` FROM kanban_subtasks WHERE card_id = ? ORDER BY position, id`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subtask{}
	for rows.Next() {
		s, err := scanSubtask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func getSubtask(ctx context.Context, q db.Querier, id string) (*Subtask, error) {
	s, err := scanSubtask(q.QueryRowContext(ctx, `SELECT `+subtaskCols+` FROM kanban_subtasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSubtaskNotFound
	}
	return s, err
}

/* ---------------------------------------------------------------- comments */

const commentCols = `k.n, k.id, k.card_id, k.author_id, u.username, COALESCE(u.display_name, ''), k.body, k.created_at, k.updated_at, k.edited`

const commentFrom = ` FROM kanban_comments k LEFT JOIN users u ON u.id = k.author_id`

func scanComment(sc scanner) (*Comment, error) {
	var c Comment
	var username, display sql.NullString
	if err := sc.Scan(&c.n, &c.ID, &c.CardID, &c.AuthorID, &username, &display, &c.Body, &c.CreatedAt, &c.UpdatedAt, &c.Edited); err != nil {
		return nil, err
	}
	c.AuthorName = deletedUser
	if username.Valid {
		c.AuthorName = displayName(username.String, display.String)
	}
	return &c, nil
}

func queryComments(ctx context.Context, q db.Querier, query string, args ...any) ([]Comment, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func getComment(ctx context.Context, q db.Querier, id string) (*Comment, error) {
	c, err := scanComment(q.QueryRowContext(ctx, `SELECT `+commentCols+commentFrom+` WHERE k.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errCommentNotFound
	}
	return c, err
}

/* ------------------------------------------------------------- attachments */

const attachmentCols = `id, card_id, board_id, filename, mime, size, thumb_state, uploaded_by, created_at`

func scanAttachment(sc scanner) (*Attachment, error) {
	var a Attachment
	if err := sc.Scan(&a.ID, &a.CardID, &a.boardID, &a.Filename, &a.Mime, &a.Size, &a.thumbState, &a.UploadedBy, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.fill()
	return &a, nil
}

// fill derives the URLs and the image flag.
func (a *Attachment) fill() {
	a.URL = "/api/v1/kanban/attachments/" + a.ID
	a.IsImage = inlineImage(a.Mime)
	a.ThumbnailURL = nil
	if thumbable(a.Mime) && a.thumbState != thumbFailed {
		u := a.URL + "/thumbnail"
		a.ThumbnailURL = &u
	}
}

func cardAttachments(ctx context.Context, q db.Querier, cardID string) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+attachmentCols+` FROM kanban_attachments WHERE card_id = ? ORDER BY created_at, rowid`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func getAttachment(ctx context.Context, q db.Querier, id string) (*Attachment, error) {
	a, err := scanAttachment(q.QueryRowContext(ctx, `SELECT `+attachmentCols+` FROM kanban_attachments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errAttachmentNotFound
	}
	return a, err
}

/* ---------------------------------------------------------------- activity */

const activityCols = `a.n, a.id, a.board_id, a.card_id, a.actor_id, u.username, COALESCE(u.display_name, ''), a.action, a.meta, a.created_at`

const activityFrom = ` FROM kanban_activity a LEFT JOIN users u ON u.id = a.actor_id`

func queryActivity(ctx context.Context, q db.Querier, query string, args ...any) ([]Activity, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		var card, username, display sql.NullString
		var meta string
		if err := rows.Scan(&a.n, &a.ID, &a.BoardID, &card, &a.ActorID, &username, &display, &a.Action, &meta, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.CardID = strPtr(card)
		a.ActorName = deletedUser
		if username.Valid {
			a.ActorName = displayName(username.String, display.String)
		}
		if err := json.Unmarshal([]byte(meta), &a.Meta); err != nil || a.Meta == nil {
			a.Meta = map[string]any{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

/* --------------------------------------------------------------- positions */

// seq is a dense 0-based ordering of rows of one table under a parent column. Table and
// column names are constants, never user input.
type seq struct{ table, parent string }

var (
	cardSeq    = seq{"kanban_cards", "column_id"}
	columnSeq  = seq{"kanban_columns", "board_id"}
	subtaskSeq = seq{"kanban_subtasks", "card_id"}
)

func (s seq) count(ctx context.Context, q db.Querier, parent string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+s.table+` WHERE `+s.parent+` = ?`, parent).Scan(&n)
	return n, err
}

// openGap makes room at position at (rows at or after it move down one).
func (s seq) openGap(ctx context.Context, tx *sql.Tx, parent string, at int) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+s.table+` SET position = position + 1 WHERE `+s.parent+` = ? AND position >= ?`, parent, at)
	return err
}

// closeGap closes the hole left at position at (rows after it move up one).
func (s seq) closeGap(ctx context.Context, tx *sql.Tx, parent string, at int) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+s.table+` SET position = position - 1 WHERE `+s.parent+` = ? AND position > ?`, parent, at)
	return err
}

// moveWithin moves row id from position from to position to inside parent, shifting the rows
// in between.
func (s seq) moveWithin(ctx context.Context, tx *sql.Tx, parent, id string, from, to int) error {
	var err error
	switch {
	case to < from:
		_, err = tx.ExecContext(ctx, `UPDATE `+s.table+` SET position = position + 1 WHERE `+s.parent+` = ? AND position >= ? AND position < ?`, parent, to, from)
	case to > from:
		_, err = tx.ExecContext(ctx, `UPDATE `+s.table+` SET position = position - 1 WHERE `+s.parent+` = ? AND position > ? AND position <= ?`, parent, from, to)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.table+` SET position = ? WHERE id = ?`, to, id)
	return err
}

// clamp limits a requested position to [0, n].
func clamp(p, n int) int {
	if p < 0 {
		return 0
	}
	if p > n {
		return n
	}
	return p
}
