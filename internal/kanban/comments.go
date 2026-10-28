package kanban

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

const commentBoardQuery = `SELECT c.board_id, m.role FROM kanban_comments k JOIN kanban_cards c ON c.id = k.card_id
	LEFT JOIN kanban_members m ON m.board_id = c.board_id AND m.user_id = ? WHERE k.id = ?`

func commentBody(s string) (string, error) {
	body, problem := cleanText(s, maxCommentLen)
	if problem == "" && strings.TrimSpace(body) == "" {
		problem = "is required"
	}
	if problem != "" {
		return "", validation("body", problem)
	}
	return body, nil
}

type commentsCursor struct {
	C string `json:"c"`
	N int64  `json:"n"`
}

func (m *Module) listComments(w http.ResponseWriter, r *http.Request) error {
	cardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	limit := httpx.ParseLimit(r, 50, 200)
	after := int64(0)
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur commentsCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil || cur.C != cardID {
			return httpx.ErrInvalidCursor
		}
		after = cur.N
	}
	if _, _, err := cardFor(ctx, m.d.DB.SQL, cardID, uidOf(r)); err != nil {
		return err
	}
	items, err := queryComments(ctx, m.d.DB.SQL, `SELECT `+commentCols+commentFrom+` WHERE k.card_id = ? AND k.n > ? ORDER BY k.n LIMIT ?`,
		cardID, after, limit+1)
	if err != nil {
		return err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = httpx.EncodeCursor(commentsCursor{C: cardID, N: items[len(items)-1].n})
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// createComment adds a comment; any member may comment, viewers included.
func (m *Module) createComment(w http.ResponseWriter, r *http.Request) error {
	cardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	body, err := commentBody(in.Body)
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	id := core.NewID()
	var out *Comment
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		card, _, err := cardFor(ctx, tx, cardID, uid)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kanban_comments WHERE card_id = ?`, cardID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxComments {
			return tooMany("comments on this card", MaxComments)
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_comments (id, card_id, author_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, cardID, uid, body, ts, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, card.BoardID, cardID, uid, "comment.created",
			map[string]any{"comment_id": id, "title": card.Title, "excerpt": excerpt(body, 140)}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, card.BoardID, ts); err != nil {
			return err
		}
		out, err = getComment(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

// patchComment edits a comment (its author only).
func (m *Module) patchComment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	body, err := commentBody(in.Body)
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Comment
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		boardID, _, err := lookupBoard(ctx, tx, commentBoardQuery, id, uid, errCommentNotFound)
		if err != nil {
			return err
		}
		cur, err := getComment(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.AuthorID != uid {
			return core.Forbidden("Only the author can edit a comment")
		}
		if body != cur.Body {
			ts := now()
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_comments SET body = ?, edited = 1, updated_at = ? WHERE id = ?`, body, ts, id); err != nil {
				return err
			}
			if err := touch(ctx, tx, boardID, ts); err != nil {
				return err
			}
		}
		out, err = getComment(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// deleteComment removes a comment (its author or a board admin).
func (m *Module) deleteComment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		boardID, rl, err := lookupBoard(ctx, tx, commentBoardQuery, id, uid, errCommentNotFound)
		if err != nil {
			return err
		}
		cur, err := getComment(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.AuthorID != uid && rl < roleAdmin {
			return core.Forbidden("Only the author or a board admin can delete a comment")
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM kanban_comments WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("kanban: comment vanished")
		}
		ts := now()
		if err := logActivity(ctx, tx, boardID, cur.CardID, uid, "comment.deleted",
			map[string]any{"comment_id": id, "author_id": cur.AuthorID}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, boardID, ts)
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}
