package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Workbook is an ordered list of sheets.
type Workbook struct {
	Sheets []*Sheet
}

// NewWorkbook returns an empty workbook.
func NewWorkbook() *Workbook { return &Workbook{} }

// Sheet returns the sheet with the given ID or nil.
func (wb *Workbook) Sheet(id string) *Sheet {
	for _, s := range wb.Sheets {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// SheetByName returns the sheet with the given name (case-insensitive) or nil.
func (wb *Workbook) SheetByName(name string) *Sheet {
	for _, s := range wb.Sheets {
		if strings.EqualFold(s.Name, name) {
			return s
		}
	}
	return nil
}

// Index returns the position of the sheet with the given ID or -1.
func (wb *Workbook) Index(id string) int {
	for i, s := range wb.Sheets {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// CellCount returns the number of stored cells across all sheets.
func (wb *Workbook) CellCount() int {
	n := 0
	for _, s := range wb.Sheets {
		n += len(s.Cells)
	}
	return n
}

// Sheet is one grid. Rows/Cols are the visible grid size (≥ used extent).
// ColWidths/RowHeights are keyed by 0-based index and measured in CSS pixels.
type Sheet struct {
	ID         string
	Name       string
	Rows, Cols int
	ColWidths  map[int]float64
	RowHeights map[int]float64
	FrozenRows int
	FrozenCols int
	Cells      map[Ref]*Cell
}

// Default grid size of a new sheet (API.md: 100×26).
const (
	DefaultRows = 100
	DefaultCols = 26
)

// NewSheet allocates an empty sheet. rows/cols ≤ 0 use the defaults.
func NewSheet(id, name string, rows, cols int) *Sheet {
	if rows <= 0 {
		rows = DefaultRows
	}
	if cols <= 0 {
		cols = DefaultCols
	}
	return &Sheet{
		ID: id, Name: name,
		Rows: min(rows, MaxRows), Cols: min(cols, MaxCols),
		ColWidths:  map[int]float64{},
		RowHeights: map[int]float64{},
		Cells:      map[Ref]*Cell{},
	}
}

// NewSheetID returns a random 26-char lowercase base32 ID (same shape as
// core.NewID) for sheets created by importers.
func NewSheetID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("engine: crypto/rand failed: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// Cell returns the stored cell or nil.
func (s *Sheet) Cell(r Ref) *Cell { return s.Cells[r] }

// Value returns the value at r (empty when absent).
func (s *Sheet) Value(r Ref) Value {
	if c := s.Cells[r]; c != nil {
		return c.Value
	}
	return Value{}
}

// SetCell stores c at r (deleting when c is nil or empty) and grows the grid.
// This is a raw setter for codecs; use Engine.Apply for edits that must
// trigger recalculation.
func (s *Sheet) SetCell(r Ref, c *Cell) {
	if c == nil || c.IsEmpty() {
		delete(s.Cells, r)
		return
	}
	if s.Cells == nil {
		s.Cells = map[Ref]*Cell{}
	}
	s.Cells[r] = c
	s.grow(r)
}

func (s *Sheet) grow(r Ref) {
	if r.Row >= s.Rows {
		s.Rows = min(r.Row+1, MaxRows)
	}
	if r.Col >= s.Cols {
		s.Cols = min(r.Col+1, MaxCols)
	}
}

// UsedRange returns the bounding box of stored cells (content or style).
// ok=false for an empty sheet.
func (s *Sheet) UsedRange() (Range, bool) {
	return s.extent(func(*Cell) bool { return true })
}

// ContentRange is like UsedRange but ignores style-only cells.
func (s *Sheet) ContentRange() (Range, bool) {
	return s.extent(func(c *Cell) bool { return !c.Value.IsEmpty() || c.Formula != "" })
}

func (s *Sheet) extent(keep func(*Cell) bool) (Range, bool) {
	first := true
	var r Range
	for ref, c := range s.Cells {
		if !keep(c) {
			continue
		}
		if first {
			r = CellRange(ref)
			first = false
			continue
		}
		r.From.Row = min(r.From.Row, ref.Row)
		r.From.Col = min(r.From.Col, ref.Col)
		r.To.Row = max(r.To.Row, ref.Row)
		r.To.Col = max(r.To.Col, ref.Col)
	}
	return r, !first
}

// SortedRefs returns the stored cell positions in row-major order.
func (s *Sheet) SortedRefs() []Ref {
	refs := make([]Ref, 0, len(s.Cells))
	for r := range s.Cells {
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Row != refs[j].Row {
			return refs[i].Row < refs[j].Row
		}
		return refs[i].Col < refs[j].Col
	})
	return refs
}

// Clone deep-copies the sheet (styles are shared: they are immutable).
func (s *Sheet) Clone() *Sheet {
	c := *s
	c.ColWidths = make(map[int]float64, len(s.ColWidths))
	for k, v := range s.ColWidths {
		c.ColWidths[k] = v
	}
	c.RowHeights = make(map[int]float64, len(s.RowHeights))
	for k, v := range s.RowHeights {
		c.RowHeights[k] = v
	}
	c.Cells = make(map[Ref]*Cell, len(s.Cells))
	for k, v := range s.Cells {
		cc := *v
		c.Cells[k] = &cc
	}
	return &c
}

// ValidateSheetName enforces Excel's rules: 1..31 characters, none of
// : \ / ? * [ ], no leading/trailing apostrophe, no control characters.
func ValidateSheetName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 || n > MaxSheetNameLen || !utf8.ValidString(name) {
		return fmt.Errorf("%w: must be 1..%d characters", ErrInvalidSheetName, MaxSheetNameLen)
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: blank", ErrInvalidSheetName)
	}
	if strings.ContainsAny(name, ":\\/?*[]") {
		return fmt.Errorf("%w: contains one of : \\ / ? * [ ]", ErrInvalidSheetName)
	}
	if strings.HasPrefix(name, "'") || strings.HasSuffix(name, "'") {
		return fmt.Errorf("%w: cannot start or end with an apostrophe", ErrInvalidSheetName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrInvalidSheetName)
		}
	}
	return nil
}

// Cell is a stored cell. Formula, when set, includes the leading "=" and is
// in canonical form; Value then holds the last computed result.
type Cell struct {
	Value   Value
	Formula string
	Style   *Style
}

// IsEmpty reports whether the cell carries nothing worth storing.
func (c *Cell) IsEmpty() bool {
	return c == nil || (c.Value.IsEmpty() && c.Formula == "" && c.Style.IsZero())
}

// CellJSON is the API wire shape {"v","f","t","e","s"}.
type CellJSON struct {
	V any    `json:"v"`
	F string `json:"f,omitempty"`
	T string `json:"t"`
	E string `json:"e,omitempty"`
	S *Style `json:"s,omitempty"`
}

// JSON converts the cell to its API shape. Error values carry the code in
// both "v" and "e" so naive clients still display something meaningful.
func (c Cell) JSON() CellJSON {
	j := CellJSON{V: c.Value.JSON(), F: c.Formula, T: c.Value.Kind.TypeCode()}
	if c.Value.Kind == KindError {
		j.E = c.Value.Err
	}
	if !c.Style.IsZero() {
		j.S = c.Style
	}
	return j
}

// MarshalJSON implements json.Marshaler.
func (c Cell) MarshalJSON() ([]byte, error) { return json.Marshal(c.JSON()) }

// UnmarshalJSON parses the API cell shape (used for persisted workbooks).
func (c *Cell) UnmarshalJSON(b []byte) error {
	var j struct {
		V json.RawMessage `json:"v"`
		F string          `json:"f"`
		T string          `json:"t"`
		E string          `json:"e"`
		S *Style          `json:"s"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&j); err != nil {
		return err
	}
	var raw any
	if len(j.V) > 0 {
		d := json.NewDecoder(bytes.NewReader(j.V))
		d.UseNumber()
		if err := d.Decode(&raw); err != nil {
			return err
		}
	}
	var v Value
	switch j.T {
	case "e":
		code := j.E
		if code == "" {
			code, _ = raw.(string)
		}
		cc, ok := CanonicalErrorCode(code)
		if !ok {
			return fmt.Errorf("%w: unknown error code %q", ErrInvalidArgument, code)
		}
		v = ErrorValue(cc)
	case "z":
	default:
		var err error
		if v, err = ValueFromJSON(raw); err != nil {
			return err
		}
	}
	st, err := j.S.Normalize()
	if err != nil {
		return err
	}
	if j.F != "" && !strings.HasPrefix(j.F, "=") {
		j.F = "=" + j.F
	}
	*c = Cell{Value: v, Formula: j.F, Style: st}
	return nil
}

// ValueFromJSON converts a JSON-decoded literal (nil, bool, float64,
// json.Number, string) into a Value. Strings are always text (no formula or
// number interpretation).
func ValueFromJSON(raw any) (Value, error) {
	switch x := raw.(type) {
	case nil:
		return Value{}, nil
	case bool:
		return BoolValue(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return Value{}, fmt.Errorf("%w: non-finite number", ErrInvalidArgument)
		}
		return NumberValue(x), nil
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil || math.IsInf(f, 0) {
			return Value{}, fmt.Errorf("%w: number %q", ErrInvalidArgument, string(x))
		}
		return NumberValue(f), nil
	case int:
		return NumberValue(float64(x)), nil
	case int64:
		return NumberValue(float64(x)), nil
	case string:
		if utf8.RuneCountInString(x) > MaxCellChars {
			return Value{}, ErrCellTooLong
		}
		return StringValue(x), nil
	}
	return Value{}, fmt.Errorf("%w: unsupported value type %T", ErrInvalidArgument, raw)
}

// SheetJSON is the API "Sheet" wire shape. Cells is omitted when nil
// (summaries) and rendered as {} when empty.
type SheetJSON struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Index      int                `json:"index"`
	Rows       int                `json:"rows"`
	Cols       int                `json:"cols"`
	ColWidths  map[string]float64 `json:"col_widths"`
	RowHeights map[string]float64 `json:"row_heights"`
	FrozenRows int                `json:"frozen_rows"`
	FrozenCols int                `json:"frozen_cols"`
	Cells      map[Ref]*Cell      `json:"cells,omitzero"`
}

// ToJSON builds the wire shape. Cells are shared, not copied.
func (s *Sheet) ToJSON(index int, withCells bool) SheetJSON {
	j := SheetJSON{
		ID: s.ID, Name: s.Name, Index: index, Rows: s.Rows, Cols: s.Cols,
		ColWidths:  make(map[string]float64, len(s.ColWidths)),
		RowHeights: make(map[string]float64, len(s.RowHeights)),
		FrozenRows: s.FrozenRows, FrozenCols: s.FrozenCols,
	}
	for c, w := range s.ColWidths {
		j.ColWidths[ColName(c)] = w
	}
	for r, h := range s.RowHeights {
		j.RowHeights[strconv.Itoa(r+1)] = h
	}
	if withCells {
		j.Cells = s.Cells
		if j.Cells == nil {
			j.Cells = map[Ref]*Cell{}
		}
	}
	return j
}

// ToSheet validates the wire shape and converts it back to a Sheet.
func (j *SheetJSON) ToSheet() (*Sheet, error) {
	if err := ValidateSheetName(j.Name); err != nil {
		return nil, err
	}
	if j.Rows < 0 || j.Rows > MaxRows || j.Cols < 0 || j.Cols > MaxCols {
		return nil, fmt.Errorf("%w: grid size %dx%d", ErrInvalidArgument, j.Rows, j.Cols)
	}
	s := NewSheet(j.ID, j.Name, j.Rows, j.Cols)
	if j.FrozenRows < 0 || j.FrozenRows >= MaxRows || j.FrozenCols < 0 || j.FrozenCols >= MaxCols {
		return nil, fmt.Errorf("%w: frozen panes", ErrInvalidArgument)
	}
	s.FrozenRows, s.FrozenCols = j.FrozenRows, j.FrozenCols
	for k, w := range j.ColWidths {
		c, err := ParseColName(k)
		if err != nil || w < 0 || w > 10000 || math.IsNaN(w) {
			return nil, fmt.Errorf("%w: col_widths[%q]", ErrInvalidArgument, k)
		}
		s.ColWidths[c] = w
	}
	for k, h := range j.RowHeights {
		r, err := strconv.Atoi(k)
		if err != nil || r < 1 || r > MaxRows || h < 0 || h > 10000 || math.IsNaN(h) {
			return nil, fmt.Errorf("%w: row_heights[%q]", ErrInvalidArgument, k)
		}
		s.RowHeights[r-1] = h
	}
	if len(j.Cells) > MaxCellsPerWorkbook {
		return nil, ErrTooManyCells
	}
	for ref, c := range j.Cells {
		if c == nil || !ref.Valid() {
			continue
		}
		if c.Value.Kind == KindString && utf8.RuneCountInString(c.Value.Str) > MaxCellChars {
			return nil, ErrCellTooLong
		}
		if utf8.RuneCountInString(c.Formula) > MaxCellChars {
			return nil, ErrCellTooLong
		}
		cc := *c
		s.SetCell(ref, &cc)
	}
	return s, nil
}
