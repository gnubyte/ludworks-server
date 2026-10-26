package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/sheets/engine"
)

// ---- workbooks -------------------------------------------------------------------------------

func (m *Module) listWorkbooks(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	q := r.URL.Query().Get("q")
	if utf8.RuneCountInString(q) > maxNameLen {
		return core.BadRequest("q is too long")
	}
	var after *listCursor
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur listCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil {
			return err
		}
		after = &cur
	}
	limit := httpx.ParseLimit(r, 50, 200)
	metas, sheets, next, err := m.st.list(r.Context(), p.UserID, q, after, limit)
	if err != nil {
		return err
	}
	items := make([]workbookSummary, 0, len(metas))
	for _, meta := range metas {
		items = append(items, summaryFromRows(meta, sheets[meta.ID]))
	}
	nc := ""
	if next != nil {
		nc = httpx.EncodeCursor(next)
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, nc))
}

func (m *Module) createWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	var in struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	name, err := validName(in.Name)
	if err != nil {
		return err
	}
	wb := &engine.Workbook{Sheets: []*engine.Sheet{engine.NewSheet(core.NewID(), "Sheet1", engine.DefaultRows, engine.DefaultCols)}}
	meta, err := m.insertWorkbook(r.Context(), p, name, wb, false)
	if err != nil {
		return err
	}
	body := appendWorkbook(nil, *meta, wb)
	return writeVersioned(w, r, http.StatusCreated, meta.Version, body)
}

// insertWorkbook stores a new workbook (fully recalculating it first when recalc is set,
// e.g. after an import) and caches its engine.
func (m *Module) insertWorkbook(ctx context.Context, p *core.Principal, name string, wb *engine.Workbook, recalc bool) (*wbMeta, error) {
	eng, err := engine.New(wb)
	if err != nil {
		return nil, codecError(err, "workbook")
	}
	if recalc {
		if err := m.evaluateIn(ctx, eng, m.transferBudget, func() error { eng.RecalcAll(); return nil }); err != nil {
			return nil, err
		}
	}
	now := core.Now()
	meta := &wbMeta{ID: core.NewID(), OwnerID: p.UserID, Name: name, Version: 1, CreatedAt: now, UpdatedAt: now}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitBudget)
	defer cancel()
	if err := m.st.create(cctx, meta, wb); err != nil {
		return nil, err
	}
	m.adopt(ctx, meta, eng)
	return meta, nil
}

// adopt puts a freshly created engine into the cache (best effort).
func (m *Module) adopt(ctx context.Context, meta *wbMeta, eng *engine.Engine) {
	e, found, err := m.cache.acquire(ctx, meta.ID, meta.OwnerID)
	if err != nil || !found {
		return
	}
	if e.eng == nil {
		e.eng, e.meta = eng, *meta
	}
	m.cache.release(e)
}

func (m *Module) getWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	var body []byte
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		body = appendWorkbook(make([]byte, 0, 256+64*e.eng.CellCount()), e.meta, e.eng.Workbook())
		version = e.meta.Version
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

func (m *Module) renameWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	name, err := validName(in.Name)
	if err != nil {
		return err
	}
	var body []byte
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		old := e.meta.Name
		e.meta.Name = name
		c := newChange()
		c.rename = old != name
		if _, err := m.mutate(r.Context(), e, m.editBudget, func(*engine.Engine) (*change, error) { return c, nil }); err != nil {
			e.meta.Name = old
			return err
		}
		version = e.meta.Version
		body = mustJSON(summaryOf(e.meta, e.eng.Workbook()))
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

func (m *Module) deleteWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		ok, err := m.st.delete(context.WithoutCancel(r.Context()), id, p.UserID)
		e.eng = nil // forget the cached copy either way
		if err != nil {
			return err
		}
		if !ok {
			return errWorkbookNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// ---- sheets ----------------------------------------------------------------------------------

func (m *Module) addSheet(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	var in struct {
		Name *string `json:"name"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return err
		}
	}
	var body []byte
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		name := ""
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
			if err := engine.ValidateSheetName(name); err != nil {
				return fieldError("name", capitalize(err.Error()))
			}
			if e.eng.Workbook().SheetByName(name) != nil {
				return core.Conflict("A sheet with that name already exists")
			}
		} else {
			name = nextSheetName(e.eng.Workbook())
		}
		var sh *engine.Sheet
		_, err := m.mutate(r.Context(), e, m.editBudget, func(eng *engine.Engine) (*change, error) {
			s, ch, err := eng.AddSheet(core.NewID(), name, engine.DefaultRows, engine.DefaultCols)
			if err != nil {
				return nil, engineError(err, "name")
			}
			sh = s
			c := newChange()
			c.addCells(ch)
			c.cells[s.ID] = true
			c.structure = true
			return c, nil
		})
		if err != nil {
			return err
		}
		version = e.meta.Version
		body = appendSheet(nil, newAPIEncoder(), sh, e.eng.Workbook().Index(sh.ID), true)
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

// nextSheetName returns the first "SheetN" (N ≥ number of sheets + 1) not in use.
func nextSheetName(wb *engine.Workbook) string {
	for n := len(wb.Sheets) + 1; ; n++ {
		name := "Sheet" + strconv.Itoa(n)
		if wb.SheetByName(name) == nil {
			return name
		}
	}
}

// sheetPatch is the PATCH /sheets/{id}/sheets/{sheet_id} body. Sizes are merged: a number
// sets a width/height, null resets it to the default.
type sheetPatch struct {
	Name       *string             `json:"name"`
	Index      *int                `json:"index"`
	ColWidths  map[string]*float64 `json:"col_widths"`
	RowHeights map[string]*float64 `json:"row_heights"`
	FrozenRows *int                `json:"frozen_rows"`
	FrozenCols *int                `json:"frozen_cols"`
	Rows       *int                `json:"rows"`
	Cols       *int                `json:"cols"`
}

func (m *Module) updateSheet(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var in sheetPatch
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if len(in.ColWidths) > engine.MaxCols || len(in.RowHeights) > 100_000 {
		return core.BadRequest("too many size entries")
	}
	var body []byte
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		sh, _, err := sheetOf(e, sid)
		if err != nil {
			return err
		}
		plan, err := planSheetPatch(e.eng.Workbook(), sh, &in)
		if err != nil {
			return err
		}
		_, err = m.mutate(r.Context(), e, m.editBudget, func(eng *engine.Engine) (*change, error) {
			return plan.apply(eng, sh)
		})
		if err != nil {
			return err
		}
		version = e.meta.Version
		body = appendSheet(nil, nil, sh, e.eng.Workbook().Index(sh.ID), false)
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

// sheetPlan is a fully validated sheet patch.
type sheetPlan struct {
	name       *string
	index      *int
	rows, cols int
	resize     bool
	colWidths  map[int]float64 // value < 0: reset
	rowHeights map[int]float64
	frozenRows *int
	frozenCols *int
}

func planSheetPatch(wb *engine.Workbook, sh *engine.Sheet, in *sheetPatch) (*sheetPlan, error) {
	fe := map[string]string{}
	pl := &sheetPlan{rows: sh.Rows, cols: sh.Cols}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if err := engine.ValidateSheetName(name); err != nil {
			fe["name"] = capitalize(err.Error())
		} else if o := wb.SheetByName(name); o != nil && o != sh {
			return nil, core.Conflict("A sheet with that name already exists")
		} else if name != sh.Name {
			pl.name = &name
		}
	}
	if in.Index != nil {
		if *in.Index < 0 {
			fe["index"] = "Index must be 0 or greater"
		} else {
			pl.index = in.Index
		}
	}
	if in.Rows != nil || in.Cols != nil {
		if in.Rows != nil {
			pl.rows = *in.Rows
		}
		if in.Cols != nil {
			pl.cols = *in.Cols
		}
		used, ok := sh.UsedRange()
		switch {
		case pl.rows < 1 || pl.rows > engine.MaxRows:
			fe["rows"] = fmt.Sprintf("Rows must be between 1 and %d", engine.MaxRows)
		case pl.cols < 1 || pl.cols > engine.MaxCols:
			fe["cols"] = fmt.Sprintf("Columns must be between 1 and %d", engine.MaxCols)
		case ok && (pl.rows <= used.To.Row || pl.cols <= used.To.Col):
			fe["rows"] = "The sheet has data beyond the requested size"
		default:
			pl.resize = pl.rows != sh.Rows || pl.cols != sh.Cols
		}
	}
	if len(in.ColWidths) > 0 {
		pl.colWidths = map[int]float64{}
		for k, v := range in.ColWidths {
			c, err := engine.ParseColName(k)
			if err != nil || c >= pl.cols {
				fe["col_widths"] = fmt.Sprintf("Unknown column %q", k)
				break
			}
			if v == nil || *v == 0 {
				pl.colWidths[c] = -1
				continue
			}
			if math.IsNaN(*v) || *v < minColWidth || *v > maxColWidth {
				fe["col_widths"] = fmt.Sprintf("Widths must be between %d and %d pixels", minColWidth, maxColWidth)
				break
			}
			pl.colWidths[c] = math.Round(*v*10) / 10
		}
	}
	if len(in.RowHeights) > 0 {
		pl.rowHeights = map[int]float64{}
		for k, v := range in.RowHeights {
			n, err := strconv.Atoi(k)
			if err != nil || n < 1 || n > pl.rows {
				fe["row_heights"] = fmt.Sprintf("Unknown row %q", k)
				break
			}
			if v == nil || *v == 0 {
				pl.rowHeights[n-1] = -1
				continue
			}
			if math.IsNaN(*v) || *v < minRowHeight || *v > maxRowHeight {
				fe["row_heights"] = fmt.Sprintf("Heights must be between %d and %d pixels", minRowHeight, maxRowHeight)
				break
			}
			pl.rowHeights[n-1] = math.Round(*v*10) / 10
		}
	}
	if in.FrozenRows != nil {
		if *in.FrozenRows < 0 || *in.FrozenRows > maxFrozen || *in.FrozenRows >= pl.rows {
			fe["frozen_rows"] = fmt.Sprintf("Frozen rows must be between 0 and %d", min(maxFrozen, pl.rows-1))
		}
		pl.frozenRows = in.FrozenRows
	}
	if in.FrozenCols != nil {
		if *in.FrozenCols < 0 || *in.FrozenCols > maxFrozen || *in.FrozenCols >= pl.cols {
			fe["frozen_cols"] = fmt.Sprintf("Frozen columns must be between 0 and %d", min(maxFrozen, pl.cols-1))
		}
		pl.frozenCols = in.FrozenCols
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	return pl, nil
}

func (pl *sheetPlan) apply(eng *engine.Engine, sh *engine.Sheet) (*change, error) {
	c := newChange()
	c.meta[sh.ID] = true
	if pl.resize {
		if err := eng.Resize(sh.ID, pl.rows, pl.cols); err != nil {
			return nil, engineError(err, "rows")
		}
	}
	// Validated above: from here on nothing can fail.
	if pl.name != nil {
		ch, err := eng.RenameSheet(sh.ID, *pl.name)
		if err != nil {
			return nil, engineError(err, "name")
		}
		c.addCells(ch)
		c.structure = true
	}
	if pl.index != nil && eng.Workbook().Index(sh.ID) != *pl.index {
		if err := eng.MoveSheet(sh.ID, *pl.index); err != nil {
			return nil, engineError(err, "index")
		}
		c.structure = true
	}
	for col, wv := range pl.colWidths {
		if wv < 0 {
			delete(sh.ColWidths, col)
		} else {
			sh.ColWidths[col] = wv
		}
	}
	for row, hv := range pl.rowHeights {
		if hv < 0 {
			delete(sh.RowHeights, row)
		} else {
			sh.RowHeights[row] = hv
		}
	}
	if pl.frozenRows != nil {
		sh.FrozenRows = *pl.frozenRows
	}
	if pl.frozenCols != nil {
		sh.FrozenCols = *pl.frozenCols
	}
	return c, nil
}

func (m *Module) deleteSheet(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		if _, _, err := sheetOf(e, sid); err != nil {
			return err
		}
		_, err := m.mutate(r.Context(), e, m.editBudget, func(eng *engine.Engine) (*change, error) {
			ch, err := eng.DeleteSheet(sid)
			if err != nil {
				return nil, engineError(err, "sheet_id")
			}
			c := newChange()
			c.addCells(ch)
			delete(c.cells, sid)
			c.structure = true
			return c, nil
		})
		version = e.meta.Version
		return err
	})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", formatVersion(version))
	return httpx.NoContent(w)
}

// ---- ranges and cells ------------------------------------------------------------------------

func (m *Module) getRange(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		return core.BadRequest("ref is required (e.g. A1:D20)")
	}
	rng, err := engine.ParseRange(ref)
	if err != nil {
		return core.BadRequest("Invalid range " + strconv.Quote(ref))
	}
	var body []byte
	var version int64
	err = m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		sh, _, err := sheetOf(e, sid)
		if err != nil {
			return err
		}
		data, err := engine.ReadRange(sh, rng)
		if err != nil {
			return core.BadRequest(capitalize(err.Error()))
		}
		formulas := make([][]*string, len(data.Formulas))
		for i, row := range data.Formulas {
			formulas[i] = make([]*string, len(row))
			for j := range row {
				if row[j] != "" {
					formulas[i][j] = &row[j]
				}
			}
		}
		version = e.meta.Version
		body = mustJSON(struct {
			Ref      string            `json:"ref"`
			Values   [][]any           `json:"values"`
			Formulas [][]*string       `json:"formulas"`
			Styles   [][]*engine.Style `json:"styles"`
		}{data.Ref, nonNil(data.Values), formulas, nonNilStyles(data.Styles)})
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

func nonNil(v [][]any) [][]any {
	if v == nil {
		return [][]any{}
	}
	return v
}

func nonNilStyles(v [][]*engine.Style) [][]*engine.Style {
	if v == nil {
		return [][]*engine.Style{}
	}
	return v
}

// decodeEditBody decodes a JSON body, using json.Number-free float decoding.
func (m *Module) putRange(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var in struct {
		Ref    string  `json:"ref"`
		Values [][]any `json:"values"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	rng, err := engine.ParseRange(in.Ref)
	if err != nil {
		return fieldError("ref", "Invalid reference "+strconv.Quote(in.Ref))
	}
	if in.Values == nil {
		return fieldError("values", "values is required (a 2-D array)")
	}
	ups, err := engine.UpdatesFromValues(rng.From, in.Values)
	if err != nil {
		return engineError(err, "values")
	}
	return m.applyUpdates(w, r, p, id, sid, ups, "values")
}

// cellUpdateIn is one PATCH .../cells update. Value distinguishes "absent" from null.
type cellUpdateIn struct {
	Ref     string             `json:"ref"`
	Value   json.RawMessage    `json:"value"`
	Formula *string            `json:"formula"`
	Input   *string            `json:"input"`
	Style   *engine.StylePatch `json:"style"`
	Clear   string             `json:"clear"`
}

func (m *Module) patchCells(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var in struct {
		Updates    []cellUpdateIn `json:"updates"`
		RangeStyle *struct {
			Ref   string             `json:"ref"`
			Style *engine.StylePatch `json:"style"`
		} `json:"range_style"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return engineError(err, "style")
	}
	if len(in.Updates) == 0 && in.RangeStyle == nil {
		return core.BadRequest("Nothing to update: send updates and/or range_style")
	}
	if len(in.Updates) > engine.MaxRangeCells {
		return fieldError("updates", fmt.Sprintf("At most %d updates per request", engine.MaxRangeCells))
	}
	ups := make([]engine.CellUpdate, 0, len(in.Updates))
	for i, u := range in.Updates {
		field := fmt.Sprintf("updates[%d]", i)
		ref, err := engine.ParseRef(u.Ref)
		if err != nil {
			return fieldError(field+".ref", "Invalid cell reference "+strconv.Quote(u.Ref))
		}
		cu := engine.CellUpdate{Ref: ref, Formula: u.Formula, Input: u.Input, Style: u.Style, Clear: u.Clear}
		if len(u.Value) > 0 {
			var raw any
			if err := json.Unmarshal(u.Value, &raw); err != nil {
				return fieldError(field+".value", "Invalid value")
			}
			if _, isObj := raw.(map[string]any); isObj {
				return fieldError(field+".value", "Value must be a number, string, boolean or null")
			}
			if _, isArr := raw.([]any); isArr {
				return fieldError(field+".value", "Value must be a number, string, boolean or null")
			}
			v, err := engine.ValueFromJSON(raw)
			if err != nil {
				return engineError(err, field+".value")
			}
			cu.Value = &v
		}
		switch u.Clear {
		case "", "all", "content", "style":
		default:
			return fieldError(field+".clear", `clear must be "all", "content" or "style"`)
		}
		ups = append(ups, cu)
	}
	var styleRange *engine.Range
	var stylePatch *engine.StylePatch
	if rs := in.RangeStyle; rs != nil {
		rng, err := engine.ParseRange(rs.Ref)
		if err != nil {
			return fieldError("range_style.ref", "Invalid range "+strconv.Quote(rs.Ref))
		}
		if rs.Style == nil {
			return fieldError("range_style.style", "style is required")
		}
		if _, err := (*engine.Style)(nil).Apply(rs.Style); err != nil {
			return engineError(err, "range_style.style")
		}
		styleRange, stylePatch = &rng, rs.Style
	}
	if styleRange == nil {
		return m.applyUpdates(w, r, p, id, sid, ups, "updates")
	}
	return m.applyUpdatesWith(w, r, p, id, sid, func(sh *engine.Sheet) ([]engine.CellUpdate, error) {
		rng := *styleRange
		rng.To.Row = min(rng.To.Row, max(sh.Rows-1, rng.From.Row))
		rng.To.Col = min(rng.To.Col, max(sh.Cols-1, rng.From.Col))
		if rng.Area() > engine.MaxStyleRangeCells {
			return nil, fieldError("range_style.ref", fmt.Sprintf("Style ranges are limited to %d cells", engine.MaxStyleRangeCells))
		}
		all := make([]engine.CellUpdate, 0, len(ups)+rng.Area())
		all = append(all, ups...)
		for row := rng.From.Row; row <= rng.To.Row; row++ {
			for col := rng.From.Col; col <= rng.To.Col; col++ {
				all = append(all, engine.CellUpdate{Ref: engine.Ref{Row: row, Col: col}, Style: stylePatch})
			}
		}
		return all, nil
	}, "updates")
}

func (m *Module) applyUpdates(w http.ResponseWriter, r *http.Request, p *core.Principal, id, sid string, ups []engine.CellUpdate, field string) error {
	return m.applyUpdatesWith(w, r, p, id, sid, func(*engine.Sheet) ([]engine.CellUpdate, error) { return ups, nil }, field)
}

// applyUpdatesWith applies cell updates (built against the locked sheet) all-or-nothing and
// answers {"changed":…,"version":n}.
func (m *Module) applyUpdatesWith(w http.ResponseWriter, r *http.Request, p *core.Principal, id, sid string,
	build func(*engine.Sheet) ([]engine.CellUpdate, error), field string) error {
	var body []byte
	var version int64
	err := m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		sh, _, err := sheetOf(e, sid)
		if err != nil {
			return err
		}
		ups, err := build(sh)
		if err != nil {
			return err
		}
		var changes engine.Changes
		_, err = m.mutate(r.Context(), e, m.editBudget, func(eng *engine.Engine) (*change, error) {
			ch, err := eng.Apply(sid, ups)
			if err != nil {
				return nil, engineError(err, field)
			}
			changes = ch
			c := newChange()
			c.addCells(ch)
			return c, nil
		})
		if err != nil {
			return err
		}
		version = e.meta.Version
		body = changedBody(changes, version)
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

// ---- structure -------------------------------------------------------------------------------

type axisOp struct {
	Axis  string `json:"axis"`
	At    *int   `json:"at"`
	Count *int   `json:"count"`
}

func (m *Module) insertAxis(w http.ResponseWriter, r *http.Request) error { return m.axis(w, r, true) }
func (m *Module) deleteAxis(w http.ResponseWriter, r *http.Request) error { return m.axis(w, r, false) }

func (m *Module) axis(w http.ResponseWriter, r *http.Request, insert bool) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var in axisOp
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := map[string]string{}
	if in.Axis != "row" && in.Axis != "col" {
		fe["axis"] = `axis must be "row" or "col"`
	}
	if in.At == nil || *in.At < 0 {
		fe["at"] = "at must be a 0-based index"
	}
	if in.Count == nil || *in.Count < 1 || *in.Count > maxAxisCount {
		fe["count"] = fmt.Sprintf("count must be between 1 and %d", maxAxisCount)
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	at, count := *in.At, *in.Count
	return m.structural(w, r, p, id, sid, func(eng *engine.Engine) (engine.Changes, error) {
		switch {
		case insert && in.Axis == "row":
			return eng.InsertRows(sid, at, count)
		case insert:
			return eng.InsertCols(sid, at, count)
		case in.Axis == "row":
			return eng.DeleteRows(sid, at, count)
		default:
			return eng.DeleteCols(sid, at, count)
		}
	}, "at")
}

func (m *Module) sortRange(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	sid, err := httpx.PathID(r, "sheet_id")
	if err != nil {
		return errSheetNotFound
	}
	var in struct {
		Ref   string `json:"ref"`
		ByCol string `json:"by_col"`
		Desc  bool   `json:"desc"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	rng, err := engine.ParseRange(in.Ref)
	if err != nil {
		return fieldError("ref", "Invalid range "+strconv.Quote(in.Ref))
	}
	col, err := engine.ParseColName(strings.TrimSpace(in.ByCol))
	if err != nil {
		return fieldError("by_col", "by_col must be a column letter such as B")
	}
	if col < rng.From.Col || col > rng.To.Col {
		return fieldError("by_col", "The sort column must lie inside the range")
	}
	return m.structural(w, r, p, id, sid, func(eng *engine.Engine) (engine.Changes, error) {
		return eng.SortRange(sid, rng, col, in.Desc)
	}, "ref")
}

// structural runs an insert/delete/sort and answers with the full Sheet.
func (m *Module) structural(w http.ResponseWriter, r *http.Request, p *core.Principal, id, sid string,
	op func(*engine.Engine) (engine.Changes, error), field string) error {
	var body []byte
	var version int64
	err := m.withWorkbook(r.Context(), p, id, func(e *entry) error {
		if err := checkIfMatch(r, e.meta.Version); err != nil {
			return err
		}
		sh, _, err := sheetOf(e, sid)
		if err != nil {
			return err
		}
		_, err = m.mutate(r.Context(), e, m.editBudget, func(eng *engine.Engine) (*change, error) {
			ch, err := op(eng)
			if err != nil {
				return nil, engineError(err, field)
			}
			c := newChange()
			c.addCells(ch)
			c.cells[sid] = true
			return c, nil
		})
		if err != nil {
			return err
		}
		version = e.meta.Version
		body = appendSheet(make([]byte, 0, 256+64*len(sh.Cells)), newAPIEncoder(), sh, e.eng.Workbook().Index(sid), true)
		return nil
	})
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusOK, version, body)
}

// ---- formulas --------------------------------------------------------------------------------

var (
	functionsOnce sync.Once
	functionsJSON []byte
)

func (m *Module) listFunctions(w http.ResponseWriter, r *http.Request) error {
	functionsOnce.Do(func() { functionsJSON = mustJSON(engine.Functions()) })
	w.Header().Set("Cache-Control", "private, max-age=3600")
	return writeJSONBytes(w, r, http.StatusOK, functionsJSON)
}

type evalResult struct {
	Value any    `json:"value"`
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

func (m *Module) evaluate(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	var in struct {
		Formula    string `json:"formula"`
		WorkbookID string `json:"workbook_id"`
		SheetID    string `json:"sheet_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f := strings.TrimSpace(in.Formula)
	if f == "" || f == "=" {
		return fieldError("formula", "formula is required")
	}
	if utf8.RuneCountInString(f) > engine.MaxCellChars {
		return fieldError("formula", "Formula is too long")
	}
	if !strings.HasPrefix(f, "=") {
		f = "=" + f
	}
	if in.SheetID != "" && in.WorkbookID == "" {
		return fieldError("sheet_id", "sheet_id requires workbook_id")
	}
	var res evalResult
	run := func(eng *engine.Engine, sheetID string) error {
		var v engine.Value
		var perr error
		err := m.evaluateIn(r.Context(), eng, m.editBudget, func() error {
			v, perr = eng.Evaluate(f, sheetID)
			return nil
		})
		if err != nil {
			return err
		}
		switch {
		case perr == nil:
			res = evalResult{Value: v.JSON(), Type: v.Kind.TypeCode(), Error: v.Err}
		case errors.Is(perr, engine.ErrSheetNotFound):
			return errSheetNotFound
		default:
			res = evalResult{Value: nil, Type: "e", Error: capitalize(perr.Error())}
		}
		return nil
	}
	if in.WorkbookID == "" {
		eng, _ := engine.New(nil)
		if err := run(eng, ""); err != nil {
			return err
		}
		return httpx.WriteJSON(w, http.StatusOK, res)
	}
	if !httpx.ValidID(in.WorkbookID) {
		return errWorkbookNotFound
	}
	if in.SheetID != "" && !httpx.ValidID(in.SheetID) {
		return errSheetNotFound
	}
	err := m.withWorkbook(r.Context(), p, in.WorkbookID, func(e *entry) error {
		if in.SheetID != "" {
			if _, _, err := sheetOf(e, in.SheetID); err != nil {
				return err
			}
		}
		return run(e.eng, in.SheetID)
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, res)
}
