// Package engine is the pure-Go spreadsheet core used by the sheets module:
// cell model, A1 references, formula lexer/parser/evaluator, dependency graph
// with incremental recalculation, structural edits and number formatting.
//
// It has no I/O, HTTP or database dependencies. An Engine is NOT safe for
// concurrent use; callers serialise access per workbook.
package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Grid and content limits (API.md §10, Excel-compatible bounds).
const (
	MaxRows             = 1048576
	MaxCols             = 16384 // column XFD
	MaxCellsPerWorkbook = 1_000_000
	MaxCellChars        = 32767
	MaxFormulaDepth     = 64
	MaxSheets           = 1024
	MaxSheetNameLen     = 31
	MaxFormatLen        = 255
)

// Ref is a 0-based cell position. Row 0 / Col 0 is A1.
type Ref struct {
	Row, Col int
}

// Valid reports whether r lies inside the maximum grid.
func (r Ref) Valid() bool {
	return r.Row >= 0 && r.Row < MaxRows && r.Col >= 0 && r.Col < MaxCols
}

// String returns the A1 form ("B3"). Invalid refs render as "#REF!".
func (r Ref) String() string {
	if !r.Valid() {
		return CodeRef
	}
	return ColName(r.Col) + strconv.Itoa(r.Row+1)
}

// MarshalText makes Ref usable as a JSON object key ({"A1": ...}).
func (r Ref) MarshalText() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("%w: %d,%d", ErrInvalidRef, r.Row, r.Col)
	}
	return []byte(r.String()), nil
}

// UnmarshalText parses an A1 reference.
func (r *Ref) UnmarshalText(b []byte) error {
	v, err := ParseRef(string(b))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// ColName converts a 0-based column index to letters (0 → "A", 26 → "AA").
func ColName(c int) string {
	if c < 0 {
		return "?"
	}
	var buf [8]byte
	i := len(buf)
	c++
	for c > 0 {
		c--
		i--
		buf[i] = byte('A' + c%26)
		c /= 26
	}
	return string(buf[i:])
}

// ParseColName converts column letters (case-insensitive) to a 0-based index.
func ParseColName(s string) (int, error) {
	if s == "" || len(s) > 3 {
		return 0, fmt.Errorf("%w: column %q", ErrInvalidRef, s)
	}
	n := 0
	for i := 0; i < len(s); i++ {
		ch := s[i] | 0x20
		if ch < 'a' || ch > 'z' {
			return 0, fmt.Errorf("%w: column %q", ErrInvalidRef, s)
		}
		n = n*26 + int(ch-'a'+1)
	}
	if n > MaxCols {
		return 0, fmt.Errorf("%w: column %q beyond XFD", ErrInvalidRef, s)
	}
	return n - 1, nil
}

// refParts splits "$A$1"-style text. It accepts optional "$" markers and
// returns ok=false for anything that is not exactly letters+digits in range.
func refParts(s string) (row, col int, rowAbs, colAbs, ok bool) {
	i := 0
	if i < len(s) && s[i] == '$' {
		colAbs = true
		i++
	}
	ls := i
	for i < len(s) && isASCIILetter(s[i]) {
		i++
	}
	if i == ls || i-ls > 3 {
		return 0, 0, false, false, false
	}
	letters := s[ls:i]
	if i < len(s) && s[i] == '$' {
		rowAbs = true
		i++
	}
	ds := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == ds || i != len(s) || i-ds > 7 || s[ds] == '0' {
		return 0, 0, false, false, false
	}
	c, err := ParseColName(letters)
	if err != nil {
		return 0, 0, false, false, false
	}
	r, _ := strconv.Atoi(s[ds:])
	if r < 1 || r > MaxRows {
		return 0, 0, false, false, false
	}
	return r - 1, c, rowAbs, colAbs, true
}

// colPart parses "$A" / "AB" (column-only, used in A:B ranges).
func colPart(s string) (col int, abs, ok bool) {
	if strings.HasPrefix(s, "$") {
		abs = true
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if !isASCIILetter(s[i]) {
			return 0, false, false
		}
	}
	c, err := ParseColName(s)
	if err != nil {
		return 0, false, false
	}
	return c, abs, true
}

// rowPart parses "$5" / "12" (row-only, used in 1:3 ranges).
func rowPart(s string) (row int, abs, ok bool) {
	if strings.HasPrefix(s, "$") {
		abs = true
		s = s[1:]
	}
	if s == "" || len(s) > 7 || s[0] == '0' {
		return 0, false, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false, false
		}
	}
	r, _ := strconv.Atoi(s)
	if r < 1 || r > MaxRows {
		return 0, false, false
	}
	return r - 1, abs, true
}

func isASCIILetter(b byte) bool { return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') }

// ParseRef parses "A1", "$A$1", "aa100" (case-insensitive, "$" ignored).
func ParseRef(s string) (Ref, error) {
	row, col, _, _, ok := refParts(strings.TrimSpace(s))
	if !ok {
		return Ref{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
	}
	return Ref{Row: row, Col: col}, nil
}

// Range is an inclusive rectangle; From is the top-left, To the bottom-right.
type Range struct {
	From, To Ref
}

// NewRange builds a normalised range from any two corners.
func NewRange(a, b Ref) Range {
	r := Range{From: a, To: b}
	if r.From.Row > r.To.Row {
		r.From.Row, r.To.Row = r.To.Row, r.From.Row
	}
	if r.From.Col > r.To.Col {
		r.From.Col, r.To.Col = r.To.Col, r.From.Col
	}
	return r
}

// CellRange is the 1×1 range of a single cell.
func CellRange(r Ref) Range { return Range{From: r, To: r} }

// ParseRange parses "A1", "A1:C3", "$A$1:B2", "A:C" (whole columns) and "2:5" (whole rows).
func ParseRange(s string) (Range, error) {
	s = strings.TrimSpace(s)
	a, b, found := strings.Cut(s, ":")
	if !found {
		r, err := ParseRef(a)
		if err != nil {
			return Range{}, err
		}
		return CellRange(r), nil
	}
	if r1, c1, _, _, ok := refParts(a); ok {
		r2, c2, _, _, ok2 := refParts(b)
		if !ok2 {
			return Range{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
		}
		return NewRange(Ref{r1, c1}, Ref{r2, c2}), nil
	}
	if c1, _, ok := colPart(a); ok {
		c2, _, ok2 := colPart(b)
		if !ok2 {
			return Range{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
		}
		return NewRange(Ref{0, c1}, Ref{MaxRows - 1, c2}), nil
	}
	if r1, _, ok := rowPart(a); ok {
		r2, _, ok2 := rowPart(b)
		if !ok2 {
			return Range{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
		}
		return NewRange(Ref{r1, 0}, Ref{r2, MaxCols - 1}), nil
	}
	return Range{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
}

// String renders "A1" for single cells, "A:B" / "1:3" for whole columns/rows, else "A1:B2".
func (r Range) String() string {
	if r.From == r.To {
		return r.From.String()
	}
	if r.From.Row == 0 && r.To.Row == MaxRows-1 && !(r.From.Col == 0 && r.To.Col == MaxCols-1) {
		return ColName(r.From.Col) + ":" + ColName(r.To.Col)
	}
	if r.From.Col == 0 && r.To.Col == MaxCols-1 {
		return strconv.Itoa(r.From.Row+1) + ":" + strconv.Itoa(r.To.Row+1)
	}
	return r.From.String() + ":" + r.To.String()
}

// Rows is the number of rows covered.
func (r Range) Rows() int { return r.To.Row - r.From.Row + 1 }

// Cols is the number of columns covered.
func (r Range) Cols() int { return r.To.Col - r.From.Col + 1 }

// Area is Rows*Cols.
func (r Range) Area() int { return r.Rows() * r.Cols() }

// Contains reports whether ref lies inside r.
func (r Range) Contains(ref Ref) bool {
	return ref.Row >= r.From.Row && ref.Row <= r.To.Row && ref.Col >= r.From.Col && ref.Col <= r.To.Col
}

// Intersects reports whether the two ranges overlap.
func (r Range) Intersects(o Range) bool {
	return r.From.Row <= o.To.Row && o.From.Row <= r.To.Row && r.From.Col <= o.To.Col && o.From.Col <= r.To.Col
}

// Valid reports whether both corners are on the grid and ordered.
func (r Range) Valid() bool {
	return r.From.Valid() && r.To.Valid() && r.From.Row <= r.To.Row && r.From.Col <= r.To.Col
}

// Sentinel errors. Callers map them to HTTP 400/422 (see integration notes).
var (
	ErrInvalidRef       = errors.New("invalid cell reference")
	ErrSheetNotFound    = errors.New("sheet not found")
	ErrInvalidSheetName = errors.New("invalid sheet name")
	ErrDuplicateSheet   = errors.New("a sheet with that name already exists")
	ErrLastSheet        = errors.New("cannot delete the last sheet")
	ErrTooManySheets    = errors.New("too many sheets")
	ErrTooManyCells     = errors.New("workbook cell limit exceeded")
	ErrCellTooLong      = errors.New("cell content exceeds 32767 characters")
	ErrFormulaSyntax    = errors.New("formula syntax error")
	ErrFormulaTooDeep   = errors.New("formula nesting too deep")
	ErrFormulaArgs      = errors.New("wrong number of function arguments")
	ErrInvalidStyle     = errors.New("invalid style")
	ErrOutOfBounds      = errors.New("operation would move data beyond the sheet bounds")
	ErrInvalidArgument  = errors.New("invalid argument")
)
