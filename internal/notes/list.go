package notes

import (
	"context"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// ListQuery are the filters of GET /notes.
type ListQuery struct {
	FolderID   string // "" = any folder, "none" = no folder, else a folder id
	Q          string
	Tag        string
	Pinned     *bool
	Trashed    bool
	Sort       string // updated | created | title | rank ("" = default)
	SortGiven  bool
	Order      string // asc | desc ("" = default for the sort)
	Cursor     string
	Limit      int
	matchQuery string
}

// listCursor is the keyset position after the last returned item.
type listCursor struct {
	M string  `json:"m"`           // sort mode
	O string  `json:"o"`           // order
	P int     `json:"p"`           // pinned (0/1)
	K string  `json:"k,omitempty"` // sort key value
	S float64 `json:"s,omitempty"` // bm25 score (rank mode; computed over the caller's own index only)
	I string  `json:"i"`           // id
}

var sortColumns = map[string]string{
	"updated": "n.updated_at",
	"created": "n.created_at",
	"title":   "n.title_sort",
}

// normalize validates the query and fills defaults.
func (q *ListQuery) normalize() error {
	switch q.Sort {
	case "":
		q.Sort = "updated"
		if strings.TrimSpace(q.Q) != "" {
			q.Sort = "rank"
		}
	case "updated", "created", "title":
		q.SortGiven = true
	default:
		return core.BadRequest("sort must be updated, created or title")
	}
	switch q.Order {
	case "":
		q.Order = "desc"
		if q.Sort == "title" || q.Sort == "rank" {
			q.Order = "asc"
		}
	case "asc", "desc":
		if q.Sort == "rank" {
			q.Order = "asc"
		}
	default:
		return core.BadRequest("order must be asc or desc")
	}
	if q.FolderID != "" && q.FolderID != "none" && !httpx.ValidID(q.FolderID) {
		return core.BadRequest("invalid folder_id")
	}
	if len(q.Q) > 500 {
		return core.BadRequest("q is too long")
	}
	if q.Tag != "" {
		t, problem := normalizeTag(q.Tag)
		if problem != "" {
			return core.BadRequest("invalid tag")
		}
		q.Tag = t
	}
	if strings.TrimSpace(q.Q) != "" {
		q.matchQuery = db.FTS5Query(q.Q)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	return nil
}

// List returns one page of note summaries and the next cursor ("" at the end).
func (s *Service) List(ctx context.Context, uid string, q ListQuery) ([]Note, string, error) {
	if err := q.normalize(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(q.Q) != "" && q.matchQuery == "" {
		return []Note{}, "", nil // nothing searchable in the query
	}
	var cur *listCursor
	if q.Cursor != "" {
		var c listCursor
		if err := httpx.DecodeCursor(q.Cursor, &c); err != nil {
			return nil, "", err
		}
		if c.M != q.Sort || c.O != q.Order {
			return nil, "", httpx.ErrInvalidCursor
		}
		cur = &c
	}
	// Searches only ever read the caller's own index (search.go): bm25's statistics, and
	// so the order and the cursor score, must not depend on other users' notes.
	var fts string
	if q.matchQuery != "" {
		tbl, ok, err := s.searchIndex(ctx, uid)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return []Note{}, "", nil // the user has no notes
		}
		fts = tbl
	}

	var (
		where []string
		args  []any
	)
	from := `notes n`
	cols := summaryCols
	if q.Sort == "rank" {
		from = `(SELECT rowid AS r, bm25(` + fts + `, 10.0, 1.0, 5.0) AS score FROM ` + fts + ` WHERE ` + fts + ` MATCH ?) m JOIN notes n ON n.n = m.r`
		args = append(args, q.matchQuery)
		cols += `, m.score`
	}
	where = append(where, `n.user_id = ?`)
	args = append(args, uid)
	if q.Trashed {
		where = append(where, `n.trashed_at IS NOT NULL`)
	} else {
		where = append(where, `n.trashed_at IS NULL`)
	}
	switch q.FolderID {
	case "":
	case "none":
		where = append(where, `n.folder_id IS NULL`)
	default:
		where = append(where, `n.folder_id = ?`)
		args = append(args, q.FolderID)
	}
	if q.Pinned != nil {
		where = append(where, `n.pinned = ?`)
		args = append(args, db.Bool(*q.Pinned))
	}
	if q.Tag != "" {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(n.tags) WHERE json_each.value = ?)`)
		args = append(args, q.Tag)
	}
	if q.matchQuery != "" && q.Sort != "rank" {
		where = append(where, `n.n IN (SELECT rowid FROM `+fts+` WHERE `+fts+` MATCH ?)`)
		args = append(args, q.matchQuery)
	}

	var order string
	if q.Sort == "rank" {
		order = `m.score, n.id`
		if cur != nil {
			where = append(where, `(m.score > ? OR (m.score = ? AND n.id > ?))`)
			args = append(args, cur.S, cur.S, cur.I)
		}
	} else {
		col := sortColumns[q.Sort]
		dir, cmp := "DESC", "<"
		if q.Order == "asc" {
			dir, cmp = "ASC", ">"
		}
		order = `n.pinned DESC, ` + col + ` ` + dir + `, n.id ` + dir
		if cur != nil {
			where = append(where, `(n.pinned < ? OR (n.pinned = ? AND (`+col+` `+cmp+` ? OR (`+col+` = ? AND n.id `+cmp+` ?))))`)
			args = append(args, cur.P, cur.P, cur.K, cur.K, cur.I)
		}
	}
	args = append(args, q.Limit+1)
	query := `SELECT ` + cols + `, n.title_sort FROM ` + from + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order + ` LIMIT ?`

	rows, err := s.d.DB.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	type keyed struct {
		note  *Note
		score float64
		tsort string
	}
	var items []keyed
	for rows.Next() {
		var k keyed
		extra := []any{}
		if q.Sort == "rank" {
			extra = append(extra, &k.score)
		}
		extra = append(extra, &k.tsort)
		nt, err := scanNote(rows, false, extra...)
		if err != nil {
			return nil, "", err
		}
		k.note = nt
		items = append(items, k)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	rows.Close()

	next := ""
	if len(items) > q.Limit {
		items = items[:q.Limit]
		last := items[len(items)-1]
		c := listCursor{M: q.Sort, O: q.Order, I: last.note.ID}
		if last.note.Pinned {
			c.P = 1
		}
		switch q.Sort {
		case "rank":
			c.S = last.score
		case "updated":
			c.K = last.note.UpdatedAt
		case "created":
			c.K = last.note.CreatedAt
		case "title":
			c.K = last.tsort
		}
		next = httpx.EncodeCursor(c)
	}
	out := make([]Note, len(items))
	for i, k := range items {
		out[i] = *k.note
	}
	if q.matchQuery != "" && len(out) > 0 {
		if err := s.addHighlights(ctx, fts, q.matchQuery, out); err != nil {
			return nil, "", err
		}
	}
	return out, next, nil
}

// addHighlights replaces the snippets of a search result page with the best-matching
// fragments and attaches highlight ranges for title and snippet. fts is the caller's
// search table.
func (s *Service) addHighlights(ctx context.Context, fts, match string, notes []Note) error {
	if err := checkFTSName(fts); err != nil {
		return err
	}
	byN := make(map[int64]*Note, len(notes))
	args := []any{match}
	for i := range notes {
		byN[notes[i].n] = &notes[i]
		args = append(args, notes[i].n)
	}
	rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT rowid, highlight(`+fts+`, 0, char(1), char(2)),
		snippet(`+fts+`, 1, char(1), char(2), '…', 24) FROM `+fts+` WHERE `+fts+` MATCH ? AND rowid IN (`+placeholders(len(notes))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		var title, snip string
		if err := rows.Scan(&n, &title, &snip); err != nil {
			return err
		}
		nt := byN[n]
		if nt == nil {
			continue
		}
		hl := &Highlights{Title: [][2]int{}, Snippet: [][2]int{}}
		if t, ranges := parseMarked(title); t == nt.Title && ranges != nil {
			hl.Title = ranges
		}
		text, ranges := parseMarked(snip)
		if len(ranges) > 0 || strings.TrimSpace(nt.Snippet) == "" {
			nt.Snippet, hl.Snippet = clipMarked(text, ranges, MaxSnippetRunes)
		}
		nt.Highlights = hl
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range notes {
		if notes[i].Highlights == nil {
			notes[i].Highlights = &Highlights{Title: [][2]int{}, Snippet: [][2]int{}}
		}
	}
	return nil
}
