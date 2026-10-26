package sheets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/sheets/engine"
)

// wbMeta is a workbook row (without sheets).
type wbMeta struct {
	ID        string
	OwnerID   string
	Name      string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// sheetMeta is a sheet row without its cells.
type sheetMeta struct {
	ID         string
	WorkbookID string
	Name       string
	Position   int
	Rows, Cols int
	ColWidths  map[string]float64
	RowHeights map[string]float64
	FrozenRows int
	FrozenCols int
}

func (s sheetMeta) toEngine() (*engine.Sheet, error) {
	j := engine.SheetJSON{
		ID: s.ID, Name: s.Name, Rows: s.Rows, Cols: s.Cols, ColWidths: s.ColWidths, RowHeights: s.RowHeights,
		FrozenRows: s.FrozenRows, FrozenCols: s.FrozenCols,
	}
	return j.ToSheet()
}

// sizesJSON renders col_widths / row_heights maps for storage.
func sizesJSON(s *engine.Sheet) (cols, rows string) {
	j := s.ToJSON(0, false)
	cb, _ := json.Marshal(j.ColWidths)
	rb, _ := json.Marshal(j.RowHeights)
	return string(cb), string(rb)
}

// errWorkbookGone reports that the workbook row vanished or changed underneath a cached
// engine (deleted concurrently, or the optimistic version check failed).
var errWorkbookGone = errors.New("sheets: workbook changed concurrently")

type store struct {
	db *db.DB
}

// loadMeta reads one workbook row owned by owner (nil, nil when absent).
func (s *store) loadMeta(ctx context.Context, q queryer, id, owner string) (*wbMeta, error) {
	var m wbMeta
	var created, updated db.Time
	err := q.QueryRowContext(ctx,
		`SELECT id, owner_id, name, version, created_at, updated_at FROM workbooks WHERE id = ? AND owner_id = ?`,
		id, owner).Scan(&m.ID, &m.OwnerID, &m.Name, &m.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, m.UpdatedAt = created.Time, updated.Time
	return &m, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// load reads a whole workbook (metadata and all cells) owned by owner.
func (s *store) load(ctx context.Context, id, owner string) (*wbMeta, *engine.Workbook, error) {
	var meta *wbMeta
	wb := &engine.Workbook{}
	err := s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		wb.Sheets = nil
		var err error
		if meta, err = s.loadMeta(ctx, tx, id, owner); err != nil || meta == nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, name, position, rows, cols, col_widths, row_heights,
			frozen_rows, frozen_cols, cells FROM workbook_sheets WHERE workbook_id = ? ORDER BY position, id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sm sheetMeta
			var cw, rh db.JSON[map[string]float64]
			var cells []byte
			if err := rows.Scan(&sm.ID, &sm.Name, &sm.Position, &sm.Rows, &sm.Cols, &cw, &rh,
				&sm.FrozenRows, &sm.FrozenCols, &cells); err != nil {
				return err
			}
			sm.ColWidths, sm.RowHeights = cw.V, rh.V
			sh, err := sm.toEngine()
			if err != nil {
				return fmt.Errorf("sheets: workbook %s sheet %s: %w", id, sm.ID, err)
			}
			cm, err := decodeCells(cells)
			if err != nil {
				return fmt.Errorf("sheets: workbook %s sheet %s: %w", id, sm.ID, err)
			}
			for r, c := range cm {
				sh.SetCell(r, c)
			}
			wb.Sheets = append(wb.Sheets, sh)
		}
		return rows.Err()
	})
	if err != nil || meta == nil {
		return nil, nil, err
	}
	if len(wb.Sheets) == 0 {
		return nil, nil, fmt.Errorf("sheets: workbook %s has no sheets", id)
	}
	return meta, wb, nil
}

// sheetRow is a prepared sheet row for writing (cells encoded outside the transaction,
// because Tx may run its function more than once).
type sheetRow struct {
	sheet      *engine.Sheet
	position   int
	colWidths  string
	rowHeights string
	cells      []byte // nil: metadata-only update
	cellCount  int
}

func prepareSheet(s *engine.Sheet, pos int, withCells bool) sheetRow {
	cw, rh := sizesJSON(s)
	r := sheetRow{sheet: s, position: pos, colWidths: cw, rowHeights: rh}
	if withCells {
		r.cells = encodeCells(s)
		r.cellCount = len(s.Cells)
	}
	return r
}

func (r sheetRow) upsert(ctx context.Context, tx *sql.Tx, wbID string) error {
	s := r.sheet
	if r.cells == nil {
		res, err := tx.ExecContext(ctx, `UPDATE workbook_sheets SET name = ?, position = ?, rows = ?, cols = ?,
			col_widths = ?, row_heights = ?, frozen_rows = ?, frozen_cols = ? WHERE id = ? AND workbook_id = ?`,
			s.Name, r.position, s.Rows, s.Cols, r.colWidths, r.rowHeights, s.FrozenRows, s.FrozenCols, s.ID, wbID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errWorkbookGone
		}
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workbook_sheets (id, workbook_id, name, position, rows, cols,
			col_widths, row_heights, frozen_rows, frozen_cols, cells, cell_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, position = excluded.position, rows = excluded.rows,
			cols = excluded.cols, col_widths = excluded.col_widths, row_heights = excluded.row_heights,
			frozen_rows = excluded.frozen_rows, frozen_cols = excluded.frozen_cols, cells = excluded.cells,
			cell_count = excluded.cell_count
		WHERE workbook_sheets.workbook_id = excluded.workbook_id`,
		s.ID, wbID, s.Name, r.position, s.Rows, s.Cols, r.colWidths, r.rowHeights, s.FrozenRows, s.FrozenCols,
		string(r.cells), r.cellCount)
	return err
}

// create inserts a new workbook with all its sheets.
func (s *store) create(ctx context.Context, meta *wbMeta, wb *engine.Workbook) error {
	rows := make([]sheetRow, len(wb.Sheets))
	for i, sh := range wb.Sheets {
		rows[i] = prepareSheet(sh, i, true)
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workbooks (id, owner_id, name, version, cell_count, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, meta.ID, meta.OwnerID, meta.Name, meta.Version, wb.CellCount(),
			db.FormatTime(meta.CreatedAt), db.FormatTime(meta.UpdatedAt)); err != nil {
			return err
		}
		for _, r := range rows {
			if err := r.upsert(ctx, tx, meta.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// change describes what a mutation touched, so only the necessary rows are rewritten.
type change struct {
	cells     map[string]bool // sheet IDs whose cells (and metadata) must be rewritten
	meta      map[string]bool // sheet IDs whose metadata only changed
	structure bool            // sheets added, removed or reordered: rewrite every position
	rename    bool            // workbook name changed
}

func newChange() *change { return &change{cells: map[string]bool{}, meta: map[string]bool{}} }

func (c *change) addCells(ch engine.Changes) {
	for id := range ch {
		c.cells[id] = true
	}
}

func (c *change) empty() bool {
	return len(c.cells) == 0 && len(c.meta) == 0 && !c.structure && !c.rename
}

// commit persists a mutation in one transaction, bumping the workbook version only when it
// still equals meta.Version (optimistic concurrency). It returns the new version and time.
func (s *store) commit(ctx context.Context, meta *wbMeta, wb *engine.Workbook, c *change) (int64, time.Time, error) {
	var rows []sheetRow
	ids := make([]string, 0, len(wb.Sheets))
	for i, sh := range wb.Sheets {
		ids = append(ids, sh.ID)
		switch {
		case c.cells[sh.ID]:
			rows = append(rows, prepareSheet(sh, i, true))
		case c.meta[sh.ID] || c.structure:
			rows = append(rows, prepareSheet(sh, i, false))
		}
	}
	now := core.Now()
	if !now.After(meta.UpdatedAt) {
		now = meta.UpdatedAt.Add(time.Millisecond) // keep updated_at strictly increasing
	}
	newVersion := meta.Version + 1
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workbooks SET name = ?, version = ?, cell_count = ?, updated_at = ?
			WHERE id = ? AND owner_id = ? AND version = ?`,
			meta.Name, newVersion, wb.CellCount(), db.FormatTime(now), meta.ID, meta.OwnerID, meta.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errWorkbookGone
		}
		for _, r := range rows {
			if err := r.upsert(ctx, tx, meta.ID); err != nil {
				return err
			}
		}
		if c.structure {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
			args := make([]any, 0, len(ids)+1)
			args = append(args, meta.ID)
			for _, id := range ids {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM workbook_sheets WHERE workbook_id = ? AND id NOT IN (`+ph+`)`, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return newVersion, now, nil
}

// delete removes a workbook (sheets cascade). It reports whether a row was deleted.
func (s *store) delete(ctx context.Context, id, owner string) (bool, error) {
	var n int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM workbooks WHERE id = ? AND owner_id = ?`, id, owner)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n == 1, err
}

// listCursor is the keyset position of the workbook list (updated_at DESC, id DESC).
type listCursor struct {
	U string `json:"u"`
	I string `json:"i"`
}

// list returns one page of the owner's workbooks with sheet metadata.
func (s *store) list(ctx context.Context, owner, q string, after *listCursor, limit int) ([]wbMeta, map[string][]sheetMeta, *listCursor, error) {
	var metas []wbMeta
	sheets := map[string][]sheetMeta{}
	var next *listCursor
	err := s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		metas, next = nil, nil
		clear(sheets)
		query := `SELECT id, owner_id, name, version, created_at, updated_at FROM workbooks WHERE owner_id = ?`
		args := []any{owner}
		if q = strings.TrimSpace(q); q != "" {
			query += ` AND name LIKE ? ESCAPE '\'`
			args = append(args, "%"+db.EscapeLike(q)+"%")
		}
		if after != nil {
			query += ` AND (updated_at < ? OR (updated_at = ? AND id < ?))`
			args = append(args, after.U, after.U, after.I)
		}
		query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m wbMeta
			var created, updated db.Time
			if err := rows.Scan(&m.ID, &m.OwnerID, &m.Name, &m.Version, &created, &updated); err != nil {
				rows.Close()
				return err
			}
			m.CreatedAt, m.UpdatedAt = created.Time, updated.Time
			metas = append(metas, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(metas) > limit {
			metas = metas[:limit]
			last := metas[len(metas)-1]
			next = &listCursor{U: db.FormatTime(last.UpdatedAt), I: last.ID}
		}
		if len(metas) == 0 {
			return nil
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(metas)), ",")
		args = args[:0]
		for _, m := range metas {
			args = append(args, m.ID)
		}
		srows, err := tx.QueryContext(ctx, `SELECT id, workbook_id, name, position, rows, cols, col_widths, row_heights,
			frozen_rows, frozen_cols FROM workbook_sheets WHERE workbook_id IN (`+ph+`) ORDER BY workbook_id, position, id`, args...)
		if err != nil {
			return err
		}
		defer srows.Close()
		for srows.Next() {
			var sm sheetMeta
			var cw, rh db.JSON[map[string]float64]
			if err := srows.Scan(&sm.ID, &sm.WorkbookID, &sm.Name, &sm.Position, &sm.Rows, &sm.Cols, &cw, &rh,
				&sm.FrozenRows, &sm.FrozenCols); err != nil {
				return err
			}
			sm.ColWidths, sm.RowHeights = cw.V, rh.V
			sheets[sm.WorkbookID] = append(sheets[sm.WorkbookID], sm)
		}
		return srows.Err()
	})
	return metas, sheets, next, err
}

// countSheets is used by tests and diagnostics.
func (s *store) countSheets(ctx context.Context, wbID string) (int, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM workbook_sheets WHERE workbook_id = ?`, wbID).Scan(&n)
	return n, err
}

// formatVersion renders a version as an ETag.
func formatVersion(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }
