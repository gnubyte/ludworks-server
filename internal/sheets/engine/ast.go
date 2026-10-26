package engine

import (
	"strconv"
	"strings"
)

// Expr is a node of a parsed formula.
type Expr interface{ exprNode() }

// NumberLit is a numeric literal; Raw preserves the source spelling.
type NumberLit struct {
	Val float64
	Raw string
}

// StringLit is a "quoted" text literal (unescaped).
type StringLit struct{ Val string }

// BoolLit is TRUE / FALSE.
type BoolLit struct{ Val bool }

// ErrorLit is an error literal such as #N/A (also used for #REF! after edits).
type ErrorLit struct{ Code string }

// CellRef is a single-cell reference. Sheet "" means the formula's own sheet.
type CellRef struct {
	Sheet          string
	Row, Col       int
	AbsRow, AbsCol bool
}

// RangeKind distinguishes A1:B2, A:B and 1:2 ranges.
type RangeKind uint8

// Range kinds.
const (
	RangeCells RangeKind = iota
	RangeCols            // whole columns; row fields unused
	RangeRows            // whole rows; column fields unused
)

// RangeRef is a rectangular reference, always normalised (R1≤R2, C1≤C2).
type RangeRef struct {
	Sheet                      string
	Kind                       RangeKind
	R1, C1, R2, C2             int
	AbsR1, AbsC1, AbsR2, AbsC2 bool
}

// Bounds returns the covered rectangle on the grid.
func (r *RangeRef) Bounds() Range {
	switch r.Kind {
	case RangeCols:
		return Range{From: Ref{0, r.C1}, To: Ref{MaxRows - 1, r.C2}}
	case RangeRows:
		return Range{From: Ref{r.R1, 0}, To: Ref{r.R2, MaxCols - 1}}
	}
	return Range{From: Ref{r.R1, r.C1}, To: Ref{r.R2, r.C2}}
}

// UnaryOp is a prefix operator.
type UnaryOp struct {
	Op byte // '-' or '+'
	X  Expr
}

// PercentOp is the postfix % operator.
type PercentOp struct{ X Expr }

// BinOp identifies a binary operator.
type BinOp uint8

// Binary operators.
const (
	OpAdd BinOp = iota
	OpSub
	OpMul
	OpDiv
	OpPow
	OpConcat
	OpEq
	OpNe
	OpLt
	OpGt
	OpLe
	OpGe
)

var binOpText = [...]string{"+", "-", "*", "/", "^", "&", "=", "<>", "<", ">", "<=", ">="}

func (o BinOp) String() string { return binOpText[o] }

// BinaryOp is L op R.
type BinaryOp struct {
	Op   BinOp
	L, R Expr
}

// ParenExpr preserves explicit parentheses for faithful re-printing.
type ParenExpr struct{ X Expr }

// CallExpr is a function call; Name is upper-case without _xlfn. prefixes.
type CallExpr struct {
	Name string
	Args []Expr
}

// ArrayLit is an array constant {1,2;3,4}; elements are literals.
type ArrayLit struct{ Rows [][]Expr }

// NameExpr is an undefined name (evaluates to #NAME?).
type NameExpr struct{ Name string }

// MissingArg is an omitted function argument, e.g. the middle of IF(A1,,1).
type MissingArg struct{}

func (*NumberLit) exprNode()  {}
func (*StringLit) exprNode()  {}
func (*BoolLit) exprNode()    {}
func (*ErrorLit) exprNode()   {}
func (*CellRef) exprNode()    {}
func (*RangeRef) exprNode()   {}
func (*UnaryOp) exprNode()    {}
func (*PercentOp) exprNode()  {}
func (*BinaryOp) exprNode()   {}
func (*ParenExpr) exprNode()  {}
func (*CallExpr) exprNode()   {}
func (*ArrayLit) exprNode()   {}
func (*NameExpr) exprNode()   {}
func (*MissingArg) exprNode() {}

// Walk visits e and its children depth-first; fn returning false skips children.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch n := e.(type) {
	case *UnaryOp:
		Walk(n.X, fn)
	case *PercentOp:
		Walk(n.X, fn)
	case *BinaryOp:
		Walk(n.L, fn)
		Walk(n.R, fn)
	case *ParenExpr:
		Walk(n.X, fn)
	case *CallExpr:
		for _, a := range n.Args {
			Walk(a, fn)
		}
	case *ArrayLit:
		for _, row := range n.Rows {
			for _, x := range row {
				Walk(x, fn)
			}
		}
	}
}

// Rewrite replaces nodes bottom-up: fn receives each node (children already
// rewritten) and returns its replacement (or the node itself).
func Rewrite(e Expr, fn func(Expr) Expr) Expr {
	switch n := e.(type) {
	case *UnaryOp:
		n.X = Rewrite(n.X, fn)
	case *PercentOp:
		n.X = Rewrite(n.X, fn)
	case *BinaryOp:
		n.L = Rewrite(n.L, fn)
		n.R = Rewrite(n.R, fn)
	case *ParenExpr:
		n.X = Rewrite(n.X, fn)
	case *CallExpr:
		for i, a := range n.Args {
			n.Args[i] = Rewrite(a, fn)
		}
	}
	return fn(e)
}

// FormatOptions controls formula printing.
type FormatOptions struct {
	NoEquals bool                     // omit the leading "=" (OOXML storage form)
	FuncName func(name string) string // optional function-name mapper (e.g. add "_xlfn.")
}

// Formula is a parsed formula.
type Formula struct {
	Root Expr
}

// String prints the canonical formula with a leading "=".
func (f *Formula) String() string { return f.Format(FormatOptions{}) }

// Format prints the formula with options.
func (f *Formula) Format(o FormatOptions) string {
	var b strings.Builder
	if !o.NoEquals {
		b.WriteByte('=')
	}
	writeExpr(&b, f.Root, &o)
	return b.String()
}

// QuoteSheetName returns the name as it must appear before "!" in a formula.
func QuoteSheetName(name string) string {
	if !sheetNeedsQuotes(name) {
		return name
	}
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

func sheetNeedsQuotes(name string) bool {
	if name == "" {
		return true
	}
	// Conservative: only plain ASCII identifiers stay unquoted; quoting is
	// always valid, so anything unusual gets quotes.
	for i := 0; i < len(name); i++ {
		c := name[i]
		isLetter := c == '_' || isASCIILetter(c)
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isLetter {
			return true
		}
		if !isLetter && !isDigit {
			return true
		}
	}
	up := strings.ToUpper(name)
	if up == "TRUE" || up == "FALSE" {
		return true
	}
	if _, _, _, _, ok := refParts(name); ok {
		return true
	}
	if _, _, ok := colPart(name); ok {
		return true // e.g. "AB" would read as a column
	}
	if looksLikeR1C1(up) {
		return true
	}
	return false
}

func looksLikeR1C1(s string) bool {
	if s == "" || (s[0] != 'R' && s[0] != 'C') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && c != 'R' && c != 'C' {
			return false
		}
	}
	return true
}

func writeSheet(b *strings.Builder, sheet string) {
	if sheet != "" {
		b.WriteString(QuoteSheetName(sheet))
		b.WriteByte('!')
	}
}

func writeCol(b *strings.Builder, c int, abs bool) {
	if abs {
		b.WriteByte('$')
	}
	b.WriteString(ColName(c))
}

func writeRow(b *strings.Builder, r int, abs bool) {
	if abs {
		b.WriteByte('$')
	}
	b.WriteString(strconv.Itoa(r + 1))
}

func writeExpr(b *strings.Builder, e Expr, o *FormatOptions) {
	switch n := e.(type) {
	case *NumberLit:
		if n.Raw != "" {
			b.WriteString(n.Raw)
		} else {
			b.WriteString(strconv.FormatFloat(n.Val, 'G', -1, 64))
		}
	case *StringLit:
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(n.Val, `"`, `""`))
		b.WriteByte('"')
	case *BoolLit:
		if n.Val {
			b.WriteString("TRUE")
		} else {
			b.WriteString("FALSE")
		}
	case *ErrorLit:
		b.WriteString(n.Code)
	case *CellRef:
		writeSheet(b, n.Sheet)
		writeCol(b, n.Col, n.AbsCol)
		writeRow(b, n.Row, n.AbsRow)
	case *RangeRef:
		writeSheet(b, n.Sheet)
		switch n.Kind {
		case RangeCols:
			writeCol(b, n.C1, n.AbsC1)
			b.WriteByte(':')
			writeCol(b, n.C2, n.AbsC2)
		case RangeRows:
			writeRow(b, n.R1, n.AbsR1)
			b.WriteByte(':')
			writeRow(b, n.R2, n.AbsR2)
		default:
			writeCol(b, n.C1, n.AbsC1)
			writeRow(b, n.R1, n.AbsR1)
			b.WriteByte(':')
			writeCol(b, n.C2, n.AbsC2)
			writeRow(b, n.R2, n.AbsR2)
		}
	case *UnaryOp:
		b.WriteByte(n.Op)
		writeExpr(b, n.X, o)
	case *PercentOp:
		writeExpr(b, n.X, o)
		b.WriteByte('%')
	case *BinaryOp:
		writeExpr(b, n.L, o)
		b.WriteString(n.Op.String())
		writeExpr(b, n.R, o)
	case *ParenExpr:
		b.WriteByte('(')
		writeExpr(b, n.X, o)
		b.WriteByte(')')
	case *CallExpr:
		name := n.Name
		if o.FuncName != nil {
			name = o.FuncName(name)
		}
		b.WriteString(name)
		b.WriteByte('(')
		for i, a := range n.Args {
			if i > 0 {
				b.WriteByte(',')
			}
			writeExpr(b, a, o)
		}
		b.WriteByte(')')
	case *ArrayLit:
		b.WriteByte('{')
		for i, row := range n.Rows {
			if i > 0 {
				b.WriteByte(';')
			}
			for j, x := range row {
				if j > 0 {
					b.WriteByte(',')
				}
				writeExpr(b, x, o)
			}
		}
		b.WriteByte('}')
	case *NameExpr:
		b.WriteString(n.Name)
	case *MissingArg:
	}
}
