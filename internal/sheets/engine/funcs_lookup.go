package engine

import (
	"strings"
)

func init() {
	register(
		&funcDef{name: "VLOOKUP", syntax: "VLOOKUP(lookup_value, table_array, col_index_num, [range_lookup])", desc: "Looks up a value in the first column and returns a value from another column (range_lookup TRUE = approximate, sorted ascending).", min: 3, max: 4, eager: lookupHV(true)},
		&funcDef{name: "HLOOKUP", syntax: "HLOOKUP(lookup_value, table_array, row_index_num, [range_lookup])", desc: "Looks up a value in the first row and returns a value from another row.", min: 3, max: 4, eager: lookupHV(false)},
		&funcDef{name: "INDEX", syntax: "INDEX(array, row_num, [column_num])", desc: "Value (reference) at a row/column position; 0 selects a whole row or column.", min: 2, max: 3, eager: fnIndex},
		&funcDef{name: "MATCH", syntax: "MATCH(lookup_value, lookup_array, [match_type])", desc: "Relative position of a value (1: largest ≤, 0: exact, -1: smallest ≥).", min: 2, max: 3, eager: fnMatch},
		&funcDef{name: "XLOOKUP", syntax: "XLOOKUP(lookup_value, lookup_array, return_array, [if_not_found], [match_mode], [search_mode])", desc: "Searches a range and returns the matching item from another range.", min: 3, max: 6, eager: fnXLookup},
	)
}

// matcher implements exact (optionally wildcard) equality for lookups.
type matcher struct {
	v    Value
	pat  []wildTok
	wild bool
}

func newMatcher(v Value, wildcards bool) matcher {
	m := matcher{v: v}
	if wildcards && v.Kind == KindString && hasWildcards(v.Str) {
		m.wild = true
		m.pat = compileWild(strings.ToLower(v.Str))
	}
	return m
}

func (m *matcher) eq(x Value) bool {
	if m.wild {
		return x.Kind == KindString && wildMatch(m.pat, strings.ToLower(x.Str))
	}
	if x.Kind != m.v.Kind || x.Kind == KindError {
		return false
	}
	return compareValues(x, m.v) == 0
}

// line is a 1-D view (a row or column) of a grid.
type line struct {
	g        grid
	vertical bool
	idx      int // fixed column (vertical) or row (horizontal)
	n        int // logical length
	scan     int // positions that may hold data
}

func (c *evalCtx) lineOf(g grid, vertical bool, idx int) line {
	br, bc := c.bound(g)
	if vertical {
		return line{g: g, vertical: true, idx: idx, n: g.rows, scan: br}
	}
	return line{g: g, idx: idx, n: g.cols, scan: bc}
}

func (l line) at(i int) Value {
	if l.vertical {
		return l.g.at(i, l.idx)
	}
	return l.g.at(l.idx, i)
}

// findExact returns the first (or last when reverse) exact match or -1.
func (l line) findExact(m matcher, reverse bool) int {
	if !reverse {
		for i := 0; i < l.scan; i++ {
			if m.eq(l.at(i)) {
				return i
			}
		}
		return -1
	}
	for i := l.scan - 1; i >= 0; i-- {
		if m.eq(l.at(i)) {
			return i
		}
	}
	return -1
}

// findSorted implements approximate matching on sorted data: for asc, the
// last position whose value ≤ v (stopping at the first larger value of
// the same type); for desc, the last position whose value ≥ v. Values of a
// different type and blanks are skipped, like Excel.
func (l line) findSorted(v Value, asc bool) int {
	found := -1
	for i := 0; i < l.scan; i++ {
		x := l.at(i)
		if x.Kind != v.Kind {
			continue
		}
		c := compareValues(x, v)
		if (asc && c <= 0) || (!asc && c >= 0) {
			found = i
			if c == 0 {
				// keep scanning: Excel returns the last of equal values
				continue
			}
		} else {
			break
		}
	}
	return found
}

func lookupHV(vertical bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		lv := c.scalar(args[0])
		if lv.IsError() {
			return valOp(lv)
		}
		if args[1].k == opVal {
			if args[1].v.IsError() {
				return args[1]
			}
			return errOp(CodeNA)
		}
		g := c.gridOf(args[1])
		idxF, e := toNum(c.scalar(args[2]))
		if e != "" {
			return errOp(e)
		}
		idx := clampInt(idxF, -1, MaxCols+1)
		approx := true
		if len(args) > 3 {
			b, e := toBool(c.scalar(args[3]))
			if e != "" {
				return errOp(e)
			}
			approx = b
		}
		span := g.cols
		if !vertical {
			span = g.rows
		}
		if idx < 1 {
			return errOp(CodeValue)
		}
		if idx > span {
			return errOp(CodeRef)
		}
		if lv.IsEmpty() {
			return errOp(CodeNA)
		}
		l := c.lineOf(g, vertical, 0)
		var pos int
		if approx {
			pos = l.findSorted(lv, true)
		} else {
			pos = l.findExact(newMatcher(lv, true), false)
		}
		if pos < 0 {
			return errOp(CodeNA)
		}
		if vertical {
			return g.sub(pos, idx-1, 1, 1)
		}
		return g.sub(idx-1, pos, 1, 1)
	}
}

func fnIndex(c *evalCtx, args []operand) operand {
	if args[0].k == opVal && args[0].v.IsError() {
		return args[0]
	}
	g := c.gridOf(args[0])
	num := func(i int) (int, string) {
		if i >= len(args) {
			return 0, ""
		}
		f, e := toNum(c.scalar(args[i]))
		if e != "" {
			return 0, e
		}
		if f < 0 {
			return 0, CodeValue
		}
		return clampInt(f, 0, MaxRows+1), ""
	}
	r, e := num(1)
	if e != "" {
		return errOp(e)
	}
	col, e := num(2)
	if e != "" {
		return errOp(e)
	}
	if len(args) == 2 && g.rows == 1 && g.cols > 1 {
		r, col = 1, r // single row: the one index selects a column
	} else if len(args) == 2 && g.cols == 1 {
		col = 1
	}
	if r > g.rows || col > g.cols {
		return errOp(CodeRef)
	}
	r0, nr := r-1, 1
	if r == 0 {
		r0, nr = 0, g.rows
	}
	c0, nc := col-1, 1
	if col == 0 {
		c0, nc = 0, g.cols
	}
	return g.sub(r0, c0, nr, nc)
}

func fnMatch(c *evalCtx, args []operand) operand {
	lv := c.scalar(args[0])
	if lv.IsError() {
		return valOp(lv)
	}
	if args[1].k == opVal {
		if args[1].v.IsError() {
			return args[1]
		}
		return errOp(CodeNA)
	}
	g := c.gridOf(args[1])
	mt := 1.0
	if len(args) > 2 {
		f, e := toNum(c.scalar(args[2]))
		if e != "" {
			return errOp(e)
		}
		mt = f
	}
	if g.rows != 1 && g.cols != 1 {
		return errOp(CodeNA)
	}
	if lv.IsEmpty() {
		return errOp(CodeNA)
	}
	l := c.lineOf(g, g.cols == 1, 0)
	var pos int
	switch {
	case mt == 0:
		pos = l.findExact(newMatcher(lv, true), false)
	case mt > 0:
		pos = l.findSorted(lv, true)
	default:
		pos = l.findSorted(lv, false)
	}
	if pos < 0 {
		return errOp(CodeNA)
	}
	return numOp(float64(pos + 1))
}

func fnXLookup(c *evalCtx, args []operand) operand {
	lv := c.scalar(args[0])
	if lv.IsError() {
		return valOp(lv)
	}
	if args[1].k == opVal || args[2].k == opVal {
		return errOp(CodeValue)
	}
	lg, rg := c.gridOf(args[1]), c.gridOf(args[2])
	if lg.rows != 1 && lg.cols != 1 {
		return errOp(CodeValue)
	}
	vertical := lg.cols == 1 && !(lg.rows == 1 && rg.rows == 1 && rg.cols > 1)
	if vertical && rg.rows != lg.rows || !vertical && rg.cols != lg.cols {
		return errOp(CodeValue)
	}
	optInt := func(i int, def float64) (float64, string) {
		if i >= len(args) || (args[i].k == opVal && args[i].v.IsEmpty()) {
			return def, ""
		}
		return toNum(c.scalar(args[i]))
	}
	mode, e := optInt(4, 0)
	if e != "" {
		return errOp(e)
	}
	search, e := optInt(5, 1)
	if e != "" {
		return errOp(e)
	}
	reverse := search < 0
	l := c.lineOf(lg, vertical, 0)
	pos := -1
	switch mode {
	case 0, 2:
		pos = l.findExact(newMatcher(lv, mode == 2), reverse)
	case -1, 1:
		if pos = l.findExact(newMatcher(lv, false), reverse); pos < 0 {
			best := Value{}
			for k := 0; k < l.scan; k++ {
				i := k
				if reverse {
					i = l.scan - 1 - k
				}
				x := l.at(i)
				if x.Kind != lv.Kind || x.Kind == KindEmpty {
					continue
				}
				cmp := compareValues(x, lv)
				if (mode == -1 && cmp < 0 && (pos < 0 || compareValues(x, best) > 0)) ||
					(mode == 1 && cmp > 0 && (pos < 0 || compareValues(x, best) < 0)) {
					pos, best = i, x
				}
			}
		}
	default:
		return errOp(CodeValue)
	}
	if pos < 0 {
		if len(args) > 3 && !(args[3].k == opVal && args[3].v.IsEmpty()) {
			return args[3]
		}
		return errOp(CodeNA)
	}
	if vertical {
		return rg.sub(pos, 0, 1, rg.cols)
	}
	return rg.sub(0, pos, rg.rows, 1)
}
