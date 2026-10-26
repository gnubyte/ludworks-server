package engine

import (
	"math"
	"strings"
)

// maxArrayElems bounds any materialised array (≈56 MiB of Values) so a
// formula cannot exhaust memory.
const maxArrayElems = 1 << 20

// trimArea: ranges larger than this are materialised only up to the used
// extent of their sheet; the remainder is represented by a fill value.
const trimArea = 1 << 16

type opKind uint8

const (
	opVal opKind = iota
	opRef
	opArr
)

// operand is an intermediate evaluation result: a scalar, a reference to a
// rectangle of cells, or an array.
type operand struct {
	k   opKind
	v   Value
	s   *Sheet
	r   Range
	arr *array
}

// array is a 2-D value block. Only the top-left srows×scols block is stored;
// every other logical position holds fill. This keeps whole-column array
// arithmetic (SUMPRODUCT((A:A="x")*B:B)) proportional to the used area.
type array struct {
	rows, cols   int
	srows, scols int
	vals         []Value
	fill         Value
}

func newArray(rows, cols int) *array {
	return &array{rows: rows, cols: cols, srows: rows, scols: cols, vals: make([]Value, rows*cols)}
}

func (a *array) at(r, c int) Value {
	if r < a.srows && c < a.scols {
		return a.vals[r*a.scols+c]
	}
	return a.fill
}

func (a *array) set(r, c int, v Value) { a.vals[r*a.scols+c] = v }

func (a *array) isScalar() bool { return a.rows == 1 && a.cols == 1 }

// fillCount is the number of logical positions represented by fill.
func (a *array) fillCount() int { return a.rows*a.cols - a.srows*a.scols }

func valOp(v Value) operand     { return operand{k: opVal, v: v} }
func errOp(code string) operand { return operand{k: opVal, v: ErrorValue(code)} }
func numOp(f float64) operand   { return valOp(NumberValue(f)) }
func arrOp(a *array) operand {
	if a.isScalar() {
		return valOp(a.at(0, 0))
	}
	return operand{k: opArr, arr: a}
}
func refOp(s *Sheet, r Range) operand { return operand{k: opRef, s: s, r: r} }

type evalCtx struct {
	e     *Engine
	sheet *Sheet
	cell  Ref
}

func (c *evalCtx) resolveSheet(name string) *Sheet {
	if name == "" {
		return c.sheet
	}
	return c.e.sheetByName(name)
}

func (c *evalCtx) eval(x Expr) operand {
	c.e.poll()
	switch n := x.(type) {
	case *NumberLit:
		return numOp(n.Val)
	case *StringLit:
		return valOp(StringValue(n.Val))
	case *BoolLit:
		return valOp(BoolValue(n.Val))
	case *ErrorLit:
		return errOp(n.Code)
	case *CellRef:
		s := c.resolveSheet(n.Sheet)
		if s == nil {
			return errOp(CodeRef)
		}
		return refOp(s, CellRange(Ref{n.Row, n.Col}))
	case *RangeRef:
		s := c.resolveSheet(n.Sheet)
		if s == nil {
			return errOp(CodeRef)
		}
		return refOp(s, n.Bounds())
	case *ParenExpr:
		return c.eval(n.X)
	case *UnaryOp:
		x := c.eval(n.X)
		if n.Op == '+' {
			return x // Excel's unary plus is a no-op
		}
		return c.mapOp([]operand{x}, func(vs []Value) Value { return negate(vs[0]) })
	case *PercentOp:
		x := c.eval(n.X)
		return c.mapOp([]operand{x}, func(vs []Value) Value {
			f, e := toNum(vs[0])
			if e != "" {
				return ErrorValue(e)
			}
			return NumberValue(f / 100)
		})
	case *BinaryOp:
		l := c.eval(n.L)
		r := c.eval(n.R)
		op := n.Op
		return c.mapOp([]operand{l, r}, func(vs []Value) Value { return binScalar(op, vs[0], vs[1]) })
	case *CallExpr:
		return c.call(n)
	case *ArrayLit:
		rows, cols := len(n.Rows), len(n.Rows[0])
		a := newArray(rows, cols)
		for i, row := range n.Rows {
			for j, el := range row {
				a.set(i, j, c.scalar(c.eval(el)))
			}
		}
		return arrOp(a)
	case *NameExpr:
		return errOp(CodeName)
	case *MissingArg:
		return valOp(Value{})
	}
	return errOp(CodeValue)
}

func negate(v Value) Value {
	f, e := toNum(v)
	if e != "" {
		return ErrorValue(e)
	}
	return NumberValue(-f)
}

// scalar collapses an operand to one value (top-left of ranges/arrays).
func (c *evalCtx) scalar(op operand) Value {
	switch op.k {
	case opRef:
		return op.s.Value(op.r.From)
	case opArr:
		return op.arr.at(0, 0)
	}
	return op.v
}

func isMulti(op operand) bool {
	switch op.k {
	case opRef:
		return op.r.From != op.r.To
	case opArr:
		return !op.arr.isScalar()
	}
	return false
}

// toArray materialises an operand. Large ranges are trimmed to the sheet's
// used extent with an empty fill.
func (c *evalCtx) toArray(op operand) (*array, *Value) {
	switch op.k {
	case opArr:
		return op.arr, nil
	case opVal:
		a := newArray(1, 1)
		a.vals[0] = op.v
		return a, nil
	}
	r := op.r
	rows, cols := r.Rows(), r.Cols()
	sr, sc := rows, cols
	if rows*cols > trimArea {
		ext := c.e.extentOf(op.s)
		sr = max(0, min(rows, ext.Row-r.From.Row))
		sc = max(0, min(cols, ext.Col-r.From.Col))
		if sr == 0 || sc == 0 {
			sr, sc = 0, 0
		}
	}
	if sr*sc > maxArrayElems {
		v := ErrorValue(CodeNum)
		return nil, &v
	}
	a := &array{rows: rows, cols: cols, srows: sr, scols: sc, vals: make([]Value, sr*sc)}
	if sr*sc > 0 {
		block := Range{From: r.From, To: Ref{r.From.Row + sr - 1, r.From.Col + sc - 1}}
		c.e.eachCell(op.s, block, false, func(ref Ref, cell *Cell) bool {
			a.vals[(ref.Row-r.From.Row)*sc+(ref.Col-r.From.Col)] = cell.Value
			return true
		})
	}
	return a, nil
}

// mapOp applies fn element-wise with Excel broadcasting (1×1 and single
// row/column operands stretch; mismatched sizes yield #N/A).
func (c *evalCtx) mapOp(args []operand, fn func([]Value) Value) operand {
	multi := false
	for _, a := range args {
		if isMulti(a) {
			multi = true
			break
		}
	}
	vs := make([]Value, len(args))
	if !multi {
		for i, a := range args {
			vs[i] = c.scalar(a)
		}
		return valOp(fn(vs))
	}
	arrs := make([]*array, len(args))
	for i, a := range args {
		arr, ev := c.toArray(a)
		if ev != nil {
			return valOp(*ev)
		}
		arrs[i] = arr
	}
	outR, outC := 1, 1
	for _, a := range arrs {
		outR, outC = max(outR, a.rows), max(outC, a.cols)
	}
	fast := true
	sr, sc := 0, 0
	for _, a := range arrs {
		if a.isScalar() {
			continue
		}
		if a.rows != outR || a.cols != outC {
			fast = false
			break
		}
		sr, sc = max(sr, a.srows), max(sc, a.scols)
	}
	if fast {
		res := &array{rows: outR, cols: outC, srows: sr, scols: sc, vals: make([]Value, sr*sc)}
		for r := 0; r < sr; r++ {
			c.e.poll()
			for col := 0; col < sc; col++ {
				for i, a := range arrs {
					if a.isScalar() {
						vs[i] = a.at(0, 0)
					} else {
						vs[i] = a.at(r, col)
					}
				}
				res.vals[r*sc+col] = fn(vs)
			}
		}
		if sr < outR || sc < outC {
			for i, a := range arrs {
				if a.isScalar() {
					vs[i] = a.at(0, 0)
				} else {
					vs[i] = a.fill
				}
			}
			res.fill = fn(vs)
		}
		return arrOp(res)
	}
	if outR*outC > maxArrayElems {
		return errOp(CodeNum)
	}
	res := newArray(outR, outC)
	for r := 0; r < outR; r++ {
		c.e.poll()
		for col := 0; col < outC; col++ {
			na := false
			for i, a := range arrs {
				ar, ac := r, col
				if a.rows == 1 {
					ar = 0
				}
				if a.cols == 1 {
					ac = 0
				}
				if ar >= a.rows || ac >= a.cols {
					na = true
					break
				}
				vs[i] = a.at(ar, ac)
			}
			if na {
				res.set(r, col, ErrorValue(CodeNA))
			} else {
				res.set(r, col, fn(vs))
			}
		}
	}
	return arrOp(res)
}

// ---- coercion -------------------------------------------------------------

// toNum coerces for arithmetic. Returns an error code ("" on success).
func toNum(v Value) (float64, string) {
	switch v.Kind {
	case KindNumber:
		return v.Num, ""
	case KindEmpty:
		return 0, ""
	case KindBool:
		if v.Bool {
			return 1, ""
		}
		return 0, ""
	case KindString:
		if f, ok := ParseNumberText(v.Str); ok {
			return f, ""
		}
		return 0, CodeValue
	}
	return 0, v.Err
}

// toText coerces for text operations.
func toText(v Value) (string, string) {
	if v.Kind == KindError {
		return "", v.Err
	}
	return v.Text(), ""
}

// toBool coerces for logical tests.
func toBool(v Value) (bool, string) {
	switch v.Kind {
	case KindBool:
		return v.Bool, ""
	case KindNumber:
		return v.Num != 0, ""
	case KindEmpty:
		return false, ""
	case KindString:
		switch strings.ToUpper(strings.TrimSpace(v.Str)) {
		case "TRUE":
			return true, ""
		case "FALSE":
			return false, ""
		}
		return false, CodeValue
	}
	return false, v.Err
}

func typeRank(k Kind) int {
	switch k {
	case KindNumber:
		return 0
	case KindString:
		return 1
	case KindBool:
		return 2
	}
	return 3
}

func zeroOf(k Kind) Value {
	switch k {
	case KindString:
		return StringValue("")
	case KindBool:
		return BoolValue(false)
	}
	return NumberValue(0)
}

// compareValues orders non-error values like Excel: numbers < text <
// logicals; blanks take the other side's zero; text is case-insensitive.
func compareValues(a, b Value) int {
	if a.Kind == KindEmpty && b.Kind == KindEmpty {
		return 0
	}
	if a.Kind == KindEmpty {
		a = zeroOf(b.Kind)
	}
	if b.Kind == KindEmpty {
		b = zeroOf(a.Kind)
	}
	ra, rb := typeRank(a.Kind), typeRank(b.Kind)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch a.Kind {
	case KindNumber:
		switch {
		case a.Num < b.Num:
			return -1
		case a.Num > b.Num:
			return 1
		}
		return 0
	case KindString:
		return strings.Compare(strings.ToLower(a.Str), strings.ToLower(b.Str))
	case KindBool:
		switch {
		case a.Bool == b.Bool:
			return 0
		case !a.Bool:
			return -1
		}
		return 1
	}
	return 0
}

func binScalar(op BinOp, a, b Value) Value {
	if a.Kind == KindError {
		return a
	}
	if b.Kind == KindError {
		return b
	}
	switch op {
	case OpConcat:
		s := a.Text() + b.Text()
		if len(s) > MaxCellChars && textLen(s) > MaxCellChars {
			return ErrorValue(CodeValue)
		}
		return StringValue(s)
	case OpEq, OpNe, OpLt, OpGt, OpLe, OpGe:
		c := compareValues(a, b)
		var r bool
		switch op {
		case OpEq:
			r = c == 0
		case OpNe:
			r = c != 0
		case OpLt:
			r = c < 0
		case OpGt:
			r = c > 0
		case OpLe:
			r = c <= 0
		case OpGe:
			r = c >= 0
		}
		return BoolValue(r)
	}
	x, e := toNum(a)
	if e != "" {
		return ErrorValue(e)
	}
	y, e := toNum(b)
	if e != "" {
		return ErrorValue(e)
	}
	switch op {
	case OpAdd:
		return NumberValue(x + y)
	case OpSub:
		return NumberValue(x - y)
	case OpMul:
		return NumberValue(x * y)
	case OpDiv:
		if y == 0 {
			return ErrorValue(CodeDiv0)
		}
		return NumberValue(x / y)
	case OpPow:
		return power(x, y)
	}
	return ErrorValue(CodeValue)
}

func power(x, y float64) Value {
	if x == 0 && y == 0 {
		return ErrorValue(CodeNum)
	}
	if x == 0 && y < 0 {
		return ErrorValue(CodeDiv0)
	}
	return NumberValue(math.Pow(x, y))
}

// ---- function dispatch ----------------------------------------------------

func (c *evalCtx) call(n *CallExpr) operand {
	def := funcRegistry[n.Name]
	if def == nil {
		return errOp(CodeName)
	}
	if len(n.Args) < def.min || (def.max >= 0 && len(n.Args) > def.max) {
		return errOp(CodeValue)
	}
	if def.lazy != nil {
		return def.lazy(c, n.Args)
	}
	ops := make([]operand, len(n.Args))
	for i, a := range n.Args {
		ops[i] = c.eval(a)
	}
	if def.eager != nil {
		return def.eager(c, ops)
	}
	return c.mapOp(ops, func(vs []Value) Value { return def.scalar(c, vs) })
}

// ---- iteration helpers ----------------------------------------------------

// visit walks function arguments for aggregate functions. direct=true marks
// values typed directly as arguments (coerced more leniently by Excel);
// n is a multiplicity for array fill values. Blank cells inside references
// are skipped. fn returns false to stop.
func (c *evalCtx) visit(args []operand, fn func(v Value, n int, direct bool) bool) {
	for _, a := range args {
		switch a.k {
		case opVal:
			if !fn(a.v, 1, true) {
				return
			}
		case opRef:
			stop := false
			c.e.eachCell(a.s, a.r, false, func(_ Ref, cell *Cell) bool {
				if cell.Value.IsEmpty() {
					return true
				}
				if !fn(cell.Value, 1, false) {
					stop = true
					return false
				}
				return true
			})
			if stop {
				return
			}
		case opArr:
			for _, v := range a.arr.vals {
				if !fn(v, 1, false) {
					return
				}
			}
			if n := a.arr.fillCount(); n > 0 && !fn(a.arr.fill, n, false) {
				return
			}
		}
	}
}

// numbers feeds numeric arguments to fn following SUM-style rules:
// numbers in references/arrays only; direct arguments may be logicals or
// numeric text; any error propagates.
func (c *evalCtx) numbers(args []operand, fn func(x float64, n int)) string {
	errCode := ""
	c.visit(args, func(v Value, n int, direct bool) bool {
		switch v.Kind {
		case KindNumber:
			fn(v.Num, n)
		case KindError:
			errCode = v.Err
			return false
		case KindBool:
			if direct {
				if v.Bool {
					fn(1, n)
				} else {
					fn(0, n)
				}
			}
		case KindString:
			if direct {
				f, ok := ParseNumberText(v.Str)
				if !ok {
					errCode = CodeValue
					return false
				}
				fn(f, n)
			}
		}
		return true
	})
	return errCode
}

// numberList collects numbers (bounded by maxArrayElems).
func (c *evalCtx) numberList(args []operand) ([]float64, string) {
	var xs []float64
	tooMany := false
	e := c.numbers(args, func(x float64, n int) {
		if len(xs)+n > maxArrayElems {
			tooMany = true
			return
		}
		for i := 0; i < n; i++ {
			xs = append(xs, x)
		}
	})
	if e == "" && tooMany {
		e = CodeNum
	}
	return xs, e
}

// grid is positional access to a reference or array argument.
type grid struct {
	s    *Sheet
	r    Range
	arr  *array
	rows int
	cols int
}

func (c *evalCtx) gridOf(op operand) grid {
	switch op.k {
	case opRef:
		return grid{s: op.s, r: op.r, rows: op.r.Rows(), cols: op.r.Cols()}
	case opArr:
		return grid{arr: op.arr, rows: op.arr.rows, cols: op.arr.cols}
	}
	a := newArray(1, 1)
	a.vals[0] = op.v
	return grid{arr: a, rows: 1, cols: 1}
}

func (g grid) at(r, c int) Value {
	if g.arr != nil {
		return g.arr.at(r, c)
	}
	return g.s.Value(Ref{g.r.From.Row + r, g.r.From.Col + c})
}

// bound returns how many rows/cols need scanning: beyond the used extent of
// a sheet every cell is blank.
func (c *evalCtx) bound(g grid) (int, int) {
	if g.arr != nil {
		return g.rows, g.cols
	}
	ext := c.e.extentOf(g.s)
	return max(0, min(g.rows, ext.Row-g.r.From.Row)), max(0, min(g.cols, ext.Col-g.r.From.Col))
}

// sub returns a reference/array operand for a sub-rectangle of g.
func (g grid) sub(r0, c0, rows, cols int) operand {
	if g.arr == nil {
		from := Ref{g.r.From.Row + r0, g.r.From.Col + c0}
		return refOp(g.s, Range{From: from, To: Ref{from.Row + rows - 1, from.Col + cols - 1}})
	}
	if rows*cols > maxArrayElems {
		return errOp(CodeNum)
	}
	a := newArray(rows, cols)
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			a.set(i, j, g.arr.at(r0+i, c0+j))
		}
	}
	return arrOp(a)
}
