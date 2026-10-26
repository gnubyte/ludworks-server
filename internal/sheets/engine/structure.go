package engine

import (
	"fmt"
	"sort"
	"strings"
)

// Axis selects rows or columns for structural edits.
type Axis uint8

// Axes.
const (
	AxisRow Axis = iota
	AxisCol
)

// ---- formula rewriting helpers (also used by the codecs) -----------------

// ShiftFormula returns the formula as if copied by (dRow, dCol): relative
// row/column components move, absolute ($) ones stay; references pushed off
// the grid become #REF!. Used to expand OOXML shared formulas and for fills.
func ShiftFormula(formula string, dRow, dCol int) (string, error) {
	f, err := ParseFormula(formula)
	if err != nil {
		return "", err
	}
	f.Root = shiftCopy(f.Root, dRow, dCol)
	return f.Format(FormatOptions{NoEquals: !strings.HasPrefix(formula, "=")}), nil
}

// RewriteFormula re-prints a formula with options (e.g. without "=" and
// with "_xlfn." function prefixes for OOXML).
func RewriteFormula(formula string, o FormatOptions) (string, error) {
	f, err := ParseFormula(formula)
	if err != nil {
		return "", err
	}
	return f.Format(o), nil
}

// FormulaSupported reports whether the engine can evaluate the formula
// faithfully: it parses, uses only known functions with valid argument
// counts and no defined names. Importers keep only supported formulas and
// fall back to the cached value otherwise.
func FormulaSupported(formula string) bool {
	f, err := ParseFormula(formula)
	if err != nil {
		return false
	}
	ok := true
	Walk(f.Root, func(x Expr) bool {
		switch n := x.(type) {
		case *NameExpr:
			ok = false
		case *CallExpr:
			if funcRegistry[n.Name] == nil {
				ok = false
			}
		}
		return ok
	})
	return ok && checkCalls(f.Root) == nil
}

func refErr() Expr { return &ErrorLit{Code: CodeRef} }

func shiftCopy(root Expr, dRow, dCol int) Expr {
	return Rewrite(root, func(x Expr) Expr {
		switch n := x.(type) {
		case *CellRef:
			if !n.AbsRow {
				n.Row += dRow
			}
			if !n.AbsCol {
				n.Col += dCol
			}
			if !(Ref{n.Row, n.Col}).Valid() {
				return refErr()
			}
		case *RangeRef:
			if n.Kind != RangeCols {
				if !n.AbsR1 {
					n.R1 += dRow
				}
				if !n.AbsR2 {
					n.R2 += dRow
				}
				if n.R1 < 0 || n.R2 >= MaxRows {
					return refErr()
				}
			}
			if n.Kind != RangeRows {
				if !n.AbsC1 {
					n.C1 += dCol
				}
				if !n.AbsC2 {
					n.C2 += dCol
				}
				if n.C1 < 0 || n.C2 >= MaxCols {
					return refErr()
				}
			}
			normRange(n)
		}
		return x
	})
}

// shiftLine moves an index for an insert (count > 0) or delete (count < 0)
// at `at`. For deletes, ok=false means the index was inside the deleted band.
func shiftIndex(i, at, count, limit int) (int, bool) {
	if count > 0 {
		if i >= at {
			i += count
			if i >= limit {
				return 0, false
			}
		}
		return i, true
	}
	n := -count
	switch {
	case i >= at+n:
		return i - n, true
	case i >= at:
		return 0, false
	}
	return i, true
}

// shiftSpan adjusts an inclusive [a,b] span; ok=false when it vanishes.
func shiftSpan(a, b, at, count, limit int) (int, int, bool) {
	if count > 0 {
		if a >= at {
			a += count
		}
		if b >= at {
			b += count
		}
		if a >= limit {
			return 0, 0, false
		}
		return a, min(b, limit-1), true
	}
	n := -count
	end := at + n // first index after the deleted band
	switch {
	case a >= end:
		a -= n
	case a >= at:
		a = at
	}
	switch {
	case b >= end:
		b -= n
	case b >= at:
		b = at - 1
	}
	return a, b, a <= b
}

// shiftStructural rewrites references into target for an insert/delete.
func (e *Engine) shiftStructural(root Expr, own, target *Sheet, axis Axis, at, count int) (Expr, bool) {
	changed := false
	resolve := func(name string) *Sheet {
		if name == "" {
			return own
		}
		return e.sheetByName(name)
	}
	out := Rewrite(root, func(x Expr) Expr {
		switch n := x.(type) {
		case *CellRef:
			if resolve(n.Sheet) != target {
				return x
			}
			if axis == AxisRow {
				v, ok := shiftIndex(n.Row, at, count, MaxRows)
				if !ok {
					changed = true
					return refErr()
				}
				changed = changed || v != n.Row
				n.Row = v
			} else {
				v, ok := shiftIndex(n.Col, at, count, MaxCols)
				if !ok {
					changed = true
					return refErr()
				}
				changed = changed || v != n.Col
				n.Col = v
			}
		case *RangeRef:
			if resolve(n.Sheet) != target {
				return x
			}
			if axis == AxisRow && n.Kind != RangeCols {
				a, b, ok := shiftSpan(n.R1, n.R2, at, count, MaxRows)
				if !ok {
					changed = true
					return refErr()
				}
				changed = changed || a != n.R1 || b != n.R2
				n.R1, n.R2 = a, b
			}
			if axis == AxisCol && n.Kind != RangeRows {
				a, b, ok := shiftSpan(n.C1, n.C2, at, count, MaxCols)
				if !ok {
					changed = true
					return refErr()
				}
				changed = changed || a != n.C1 || b != n.C2
				n.C1, n.C2 = a, b
			}
		}
		return x
	})
	return out, changed
}

// snapshotFormulas parses every formula in the workbook (positions as keys).
func (e *Engine) parsedFormulas() map[cellKey]*Formula {
	out := map[cellKey]*Formula{}
	for _, s := range e.wb.Sheets {
		for r, c := range s.Cells {
			if c.Formula == "" {
				continue
			}
			e.poll()
			if f, err := ParseFormula(c.Formula); err == nil {
				out[cellKey{s, r}] = f
			}
		}
	}
	return out
}

// InsertRows inserts count blank rows before row index at (0-based).
func (e *Engine) InsertRows(sheetID string, at, count int) (Changes, error) {
	return e.structural(sheetID, AxisRow, at, count)
}

// InsertCols inserts count blank columns before column index at.
func (e *Engine) InsertCols(sheetID string, at, count int) (Changes, error) {
	return e.structural(sheetID, AxisCol, at, count)
}

// DeleteRows removes count rows starting at row index at; references into
// the removed rows become #REF!.
func (e *Engine) DeleteRows(sheetID string, at, count int) (Changes, error) {
	return e.structural(sheetID, AxisRow, at, -count)
}

// DeleteCols removes count columns starting at column index at.
func (e *Engine) DeleteCols(sheetID string, at, count int) (Changes, error) {
	return e.structural(sheetID, AxisCol, at, -count)
}

func (e *Engine) structural(sheetID string, axis Axis, at, count int) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	limit := MaxRows
	if axis == AxisCol {
		limit = MaxCols
	}
	n := count
	if n < 0 {
		n = -n
	}
	if n < 1 || at < 0 || at >= limit || at+n > limit {
		return nil, fmt.Errorf("%w: position %d count %d", ErrInvalidArgument, at, n)
	}
	if count > 0 {
		for r := range s.Cells {
			i := r.Row
			if axis == AxisCol {
				i = r.Col
			}
			if i >= at && i+count >= limit {
				return nil, ErrOutOfBounds
			}
		}
	}
	ch := Changes{}
	// 1. Rewrite references in every formula of every sheet.
	parsed := e.parsedFormulas()
	for k, f := range parsed {
		root, changed := e.shiftStructural(f.Root, k.s, s, axis, at, count)
		if changed {
			f.Root = root
			k.s.Cells[k.r].Formula = f.String()
			if k.s != s {
				ch.set(k.s, k.r)
			}
		}
	}
	// 2. Move the cells of the target sheet.
	old := s.Cells
	s.Cells = make(map[Ref]*Cell, len(old))
	for r, c := range old {
		nr := r
		var ok bool
		if axis == AxisRow {
			nr.Row, ok = shiftIndex(r.Row, at, count, limit)
		} else {
			nr.Col, ok = shiftIndex(r.Col, at, count, limit)
		}
		if ok {
			s.Cells[nr] = c
		}
	}
	// 3. Sizes, frozen panes and grid dimensions.
	shiftSizes := func(m map[int]float64) map[int]float64 {
		out := make(map[int]float64, len(m))
		for i, v := range m {
			if ni, ok := shiftIndex(i, at, count, limit); ok {
				out[ni] = v
			}
		}
		return out
	}
	frozen := func(f int) int {
		if f == 0 || at >= f {
			return f
		}
		if count > 0 {
			return min(f+count, limit-1)
		}
		return max(at, f-n)
	}
	if axis == AxisRow {
		s.RowHeights = shiftSizes(s.RowHeights)
		s.FrozenRows = frozen(s.FrozenRows)
		s.Rows = max(1, min(limit, s.Rows+count))
	} else {
		s.ColWidths = shiftSizes(s.ColWidths)
		s.FrozenCols = frozen(s.FrozenCols)
		s.Cols = max(1, min(limit, s.Cols+count))
	}
	e.rebuild()
	for r := range old {
		ch.set(s, r)
	}
	for r := range s.Cells {
		ch.set(s, r)
	}
	ch.merge(e.RecalcAll())
	return ch, nil
}

// SortRange sorts the rows of rng by the values in column byCol (absolute
// 0-based column inside rng). Blanks sort last in both directions; numbers
// < text < logicals < errors; text is case-insensitive; the sort is stable.
// Formulas in moved rows have their relative row references adjusted like a
// copy (Excel behaviour); references elsewhere are left untouched.
func (e *Engine) SortRange(sheetID string, rng Range, byCol int, desc bool) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	if !rng.Valid() {
		return nil, ErrInvalidRef
	}
	if byCol < rng.From.Col || byCol > rng.To.Col {
		return nil, fmt.Errorf("%w: sort column outside range", ErrInvalidArgument)
	}
	ext := e.extentOf(s)
	rng.To.Row = min(rng.To.Row, max(ext.Row-1, rng.From.Row))
	rng.To.Col = min(rng.To.Col, max(ext.Col-1, rng.From.Col))
	type row struct {
		src   int
		key   Value
		cells map[int]*Cell
	}
	rows := make([]row, rng.Rows())
	for i := range rows {
		rows[i] = row{src: rng.From.Row + i, cells: map[int]*Cell{}}
	}
	e.eachCell(s, rng, false, func(r Ref, c *Cell) bool {
		rows[r.Row-rng.From.Row].cells[r.Col] = c
		return true
	})
	for i := range rows {
		if c := rows[i].cells[byCol]; c != nil {
			rows[i].key = c.Value
		}
	}
	rank := func(v Value) int {
		switch v.Kind {
		case KindNumber:
			return 0
		case KindString:
			return 1
		case KindBool:
			return 2
		case KindError:
			return 3
		}
		return 4
	}
	sort.SliceStable(rows, func(i, j int) bool {
		e.poll()
		a, b := rows[i].key, rows[j].key
		ra, rb := rank(a), rank(b)
		if ra == 4 || rb == 4 { // blanks last regardless of direction
			return ra < rb
		}
		if ra != rb {
			if desc {
				return ra > rb
			}
			return ra < rb
		}
		var c int
		if a.Kind == KindError {
			c = strings.Compare(a.Err, b.Err)
		} else {
			c = compareValues(a, b)
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
	ch := Changes{}
	for col := rng.From.Col; col <= rng.To.Col; col++ {
		for r := rng.From.Row; r <= rng.To.Row; r++ {
			if s.Cells[Ref{r, col}] != nil {
				ch.set(s, Ref{r, col})
				delete(s.Cells, Ref{r, col})
			}
		}
	}
	for i, rw := range rows {
		dst := rng.From.Row + i
		for col, c := range rw.cells {
			nc := *c
			if nc.Formula != "" && dst != rw.src {
				if f, err := ParseFormula(nc.Formula); err == nil {
					f.Root = shiftCopy(f.Root, dst-rw.src, 0)
					nc.Formula = f.String()
				}
			}
			s.Cells[Ref{dst, col}] = &nc
		}
	}
	e.rebuild()
	for r := range s.Cells {
		if rng.Contains(r) {
			ch.set(s, r)
		}
	}
	ch.merge(e.RecalcAll())
	return ch, nil
}

// ---- sheet management -----------------------------------------------------

// AddSheet appends a new sheet (id must be unique; rows/cols ≤ 0 → 100×26).
// Formulas that referenced the name before it existed are recalculated.
func (e *Engine) AddSheet(id, name string, rows, cols int) (*Sheet, Changes, error) {
	if id == "" || e.byID[id] != nil {
		return nil, nil, fmt.Errorf("%w: sheet id", ErrInvalidArgument)
	}
	if err := ValidateSheetName(name); err != nil {
		return nil, nil, err
	}
	if e.sheetByName(name) != nil {
		return nil, nil, ErrDuplicateSheet
	}
	if len(e.wb.Sheets) >= MaxSheets {
		return nil, nil, ErrTooManySheets
	}
	s := NewSheet(id, name, rows, cols)
	e.wb.Sheets = append(e.wb.Sheets, s)
	waiting := e.missing[strings.ToLower(name)]
	ch := Changes{}
	if len(waiting) == 0 {
		e.byName[strings.ToLower(name)] = s
		e.byID[id] = s
		e.rangeIdx[s] = newRangeIndex()
		e.ext[s] = Ref{}
		return s, ch, nil
	}
	seeds := make([]cellKey, 0, len(waiting))
	for k := range waiting {
		seeds = append(seeds, k)
	}
	e.rebuild()
	e.recalc(seeds, ch)
	return s, ch, nil
}

// UniqueSheetName returns base, or base followed by " (2)", " (3)" … so it
// does not clash with an existing sheet.
func (e *Engine) UniqueSheetName(base string) string {
	if base == "" {
		base = "Sheet"
	}
	if e.sheetByName(base) == nil {
		return base
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		b := []rune(base)
		if len(b)+len([]rune(suffix)) > MaxSheetNameLen {
			b = b[:MaxSheetNameLen-len([]rune(suffix))]
		}
		cand := string(b) + suffix
		if e.sheetByName(cand) == nil {
			return cand
		}
	}
}

// RenameSheet renames a sheet and rewrites formulas that reference it.
func (e *Engine) RenameSheet(sheetID, newName string) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	if err := ValidateSheetName(newName); err != nil {
		return nil, err
	}
	if o := e.sheetByName(newName); o != nil && o != s {
		return nil, ErrDuplicateSheet
	}
	oldName := s.Name
	ch := Changes{}
	var seeds []cellKey
	for k := range e.missing[strings.ToLower(newName)] {
		seeds = append(seeds, k)
	}
	for k, f := range e.parsedFormulas() {
		changed := false
		Walk(f.Root, func(x Expr) bool {
			switch n := x.(type) {
			case *CellRef:
				if n.Sheet != "" && strings.EqualFold(n.Sheet, oldName) {
					n.Sheet, changed = newName, true
				}
			case *RangeRef:
				if n.Sheet != "" && strings.EqualFold(n.Sheet, oldName) {
					n.Sheet, changed = newName, true
				}
			}
			return true
		})
		if changed {
			k.s.Cells[k.r].Formula = f.String()
			ch.set(k.s, k.r)
		}
	}
	s.Name = newName
	e.rebuild()
	e.recalc(seeds, ch)
	return ch, nil
}

// DeleteSheet removes a sheet; references to it in other sheets become #REF!.
func (e *Engine) DeleteSheet(sheetID string) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	if len(e.wb.Sheets) == 1 {
		return nil, ErrLastSheet
	}
	ch := Changes{}
	var seeds []cellKey
	for k, f := range e.parsedFormulas() {
		if k.s == s {
			continue
		}
		changed := false
		f.Root = Rewrite(f.Root, func(x Expr) Expr {
			var name string
			switch n := x.(type) {
			case *CellRef:
				name = n.Sheet
			case *RangeRef:
				name = n.Sheet
			default:
				return x
			}
			if name != "" && strings.EqualFold(name, s.Name) {
				changed = true
				return refErr()
			}
			return x
		})
		if changed {
			k.s.Cells[k.r].Formula = f.String()
			seeds = append(seeds, k)
		}
	}
	idx := e.wb.Index(sheetID)
	e.wb.Sheets = append(e.wb.Sheets[:idx], e.wb.Sheets[idx+1:]...)
	e.rebuild()
	e.recalc(seeds, ch)
	for _, k := range seeds {
		ch.set(k.s, k.r)
	}
	return ch, nil
}

// MoveSheet changes a sheet's position (0-based, clamped).
func (e *Engine) MoveSheet(sheetID string, index int) error {
	i := e.wb.Index(sheetID)
	if i < 0 {
		return ErrSheetNotFound
	}
	s := e.wb.Sheets[i]
	rest := append(append([]*Sheet{}, e.wb.Sheets[:i]...), e.wb.Sheets[i+1:]...)
	index = max(0, min(index, len(rest)))
	e.wb.Sheets = append(rest[:index], append([]*Sheet{s}, rest[index:]...)...)
	return nil
}

// Resize changes the visible grid size; shrinking below stored cells fails.
func (e *Engine) Resize(sheetID string, rows, cols int) error {
	s := e.byID[sheetID]
	if s == nil {
		return ErrSheetNotFound
	}
	if rows < 1 || rows > MaxRows || cols < 1 || cols > MaxCols {
		return fmt.Errorf("%w: grid size", ErrInvalidArgument)
	}
	var ext Ref
	for r := range s.Cells {
		ext.Row, ext.Col = max(ext.Row, r.Row+1), max(ext.Col, r.Col+1)
	}
	if rows < ext.Row || cols < ext.Col {
		return fmt.Errorf("%w: data exists beyond %dx%d", ErrOutOfBounds, rows, cols)
	}
	s.Rows, s.Cols = rows, cols
	return nil
}
