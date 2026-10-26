package sheets

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"ludworks/internal/sheets/engine"
)

// Storage format of a sheet's cells (column workbook_sheets.cells):
//
//	{"v":1,"styles":[Style,...],"cells":[[row,col,"t",value(,"=formula"(,styleIndex))],...]}
//
// row/col are 0-based, "t" is the API type code (n s b e z), value is the cached value
// (number, string, bool, error code string or null). Styles are de-duplicated by value and
// referenced by index; a cell without formula but with a style stores "" as the formula.
// Cells are written in row-major order, which keeps the blob deterministic.

const blobVersion = 1

var errBlob = errors.New("sheets: corrupt cell data")

// styleTable de-duplicates styles while encoding.
type styleTable struct {
	byPtr  map[*engine.Style]int
	byJSON map[string]int
	list   [][]byte
}

func newStyleTable() *styleTable {
	return &styleTable{byPtr: map[*engine.Style]int{}, byJSON: map[string]int{}}
}

func (t *styleTable) index(s *engine.Style) int {
	if i, ok := t.byPtr[s]; ok {
		return i
	}
	b, err := json.Marshal(s)
	if err != nil { // cannot happen: Style has only plain fields
		b = []byte("{}")
	}
	i, ok := t.byJSON[string(b)]
	if !ok {
		i = len(t.list)
		t.list = append(t.list, b)
		t.byJSON[string(b)] = i
	}
	t.byPtr[s] = i
	return i
}

// encodeCells renders a sheet's cells in the storage format.
func encodeCells(s *engine.Sheet) []byte {
	cells := sortedCells(s.Cells)
	st := newStyleTable()
	body := make([]byte, 0, 24*len(cells)+16)
	body = append(body, '[')
	n := 0
	for _, rc := range cells {
		r, c := rc.ref, rc.cell
		if c == nil || c.IsEmpty() {
			continue
		}
		if n > 0 {
			body = append(body, ',')
		}
		n++
		body = append(body, '[')
		body = strconv.AppendInt(body, int64(r.Row), 10)
		body = append(body, ',')
		body = strconv.AppendInt(body, int64(r.Col), 10)
		body = append(body, ',', '"')
		body = append(body, c.Value.Kind.TypeCode()...)
		body = append(body, '"', ',')
		body = appendValue(body, c.Value)
		hasStyle := !c.Style.IsZero()
		if c.Formula != "" || hasStyle {
			body = append(body, ',')
			body = appendJSONString(body, c.Formula)
		}
		if hasStyle {
			body = append(body, ',')
			body = strconv.AppendInt(body, int64(st.index(c.Style)), 10)
		}
		body = append(body, ']')
	}
	body = append(body, ']')
	out := make([]byte, 0, len(body)+64+32*len(st.list))
	out = append(out, `{"v":1,"styles":[`...)
	for i, b := range st.list {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	out = append(out, `],"cells":`...)
	out = append(out, body...)
	out = append(out, '}')
	return out
}

// decodeCells parses the storage format. Every value is validated so a damaged row can
// never inject out-of-range references or invalid styles into an engine.
func decodeCells(data []byte) (map[engine.Ref]*engine.Cell, error) {
	var top struct {
		V      int               `json:"v"`
		Styles []json.RawMessage `json:"styles"`
		Cells  json.RawMessage   `json:"cells"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%w: %v", errBlob, err)
	}
	if top.V != blobVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", errBlob, top.V)
	}
	styles := make([]*engine.Style, len(top.Styles))
	for i, raw := range top.Styles {
		var s engine.Style
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%w: style %d: %v", errBlob, i, err)
		}
		ns, err := s.Normalize()
		if err != nil {
			return nil, fmt.Errorf("%w: style %d: %v", errBlob, i, err)
		}
		styles[i] = ns
	}
	cells := map[engine.Ref]*engine.Cell{}
	if len(top.Cells) == 0 {
		return cells, nil
	}
	d := &scanner{b: top.Cells}
	if err := d.cells(styles, cells); err != nil {
		return nil, fmt.Errorf("%w: %v (offset %d)", errBlob, err, d.i)
	}
	return cells, nil
}

// scanner is a small hand-written parser for the cells array: decoding hundreds of
// thousands of cells through encoding/json's generic path would allocate heavily.
type scanner struct {
	b []byte
	i int
}

func (d *scanner) ws() {
	for d.i < len(d.b) {
		switch d.b[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *scanner) peek() byte {
	d.ws()
	if d.i >= len(d.b) {
		return 0
	}
	return d.b[d.i]
}

func (d *scanner) expect(c byte) error {
	if d.peek() != c {
		return fmt.Errorf("expected %q", c)
	}
	d.i++
	return nil
}

func (d *scanner) number() (float64, error) {
	d.ws()
	start := d.i
	for d.i < len(d.b) {
		c := d.b[d.i]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			d.i++
			continue
		}
		break
	}
	if start == d.i {
		return 0, errors.New("expected number")
	}
	f, err := strconv.ParseFloat(string(d.b[start:d.i]), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("bad number")
	}
	return f, nil
}

func (d *scanner) integer(maxExcl int) (int, error) {
	f, err := d.number()
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) || f < 0 || f >= float64(maxExcl) {
		return 0, errors.New("integer out of range")
	}
	return int(f), nil
}

func (d *scanner) str() (string, error) {
	if d.peek() != '"' {
		return "", errors.New("expected string")
	}
	start := d.i
	d.i++
	escaped := false
	for d.i < len(d.b) {
		c := d.b[d.i]
		switch {
		case c == '\\':
			escaped = true
			d.i += 2
			continue
		case c == '"':
			d.i++
			if !escaped {
				s := d.b[start+1 : d.i-1]
				if !utf8.Valid(s) {
					return "", errors.New("invalid UTF-8")
				}
				return string(s), nil
			}
			var s string
			if err := json.Unmarshal(d.b[start:d.i], &s); err != nil {
				return "", err
			}
			return s, nil
		case c < 0x20:
			return "", errors.New("control character in string")
		}
		d.i++
	}
	return "", errors.New("unterminated string")
}

func (d *scanner) literal(word string) bool {
	d.ws()
	if strings.HasPrefix(string(d.b[d.i:min(len(d.b), d.i+len(word))]), word) {
		d.i += len(word)
		return true
	}
	return false
}

func (d *scanner) cells(styles []*engine.Style, out map[engine.Ref]*engine.Cell) error {
	if err := d.expect('['); err != nil {
		return err
	}
	if d.peek() == ']' {
		d.i++
		return nil
	}
	for {
		if err := d.cell(styles, out); err != nil {
			return err
		}
		switch d.peek() {
		case ',':
			d.i++
		case ']':
			d.i++
			if d.peek() != 0 {
				return errors.New("trailing data")
			}
			return nil
		default:
			return errors.New("expected , or ]")
		}
	}
}

func (d *scanner) cell(styles []*engine.Style, out map[engine.Ref]*engine.Cell) error {
	if err := d.expect('['); err != nil {
		return err
	}
	row, err := d.integer(engine.MaxRows)
	if err != nil {
		return err
	}
	if err := d.expect(','); err != nil {
		return err
	}
	col, err := d.integer(engine.MaxCols)
	if err != nil {
		return err
	}
	if err := d.expect(','); err != nil {
		return err
	}
	t, err := d.str()
	if err != nil {
		return err
	}
	if err := d.expect(','); err != nil {
		return err
	}
	var v engine.Value
	switch t {
	case "n":
		f, err := d.number()
		if err != nil {
			return err
		}
		v = engine.NumberValue(f)
	case "s":
		s, err := d.str()
		if err != nil {
			return err
		}
		if utf8.RuneCountInString(s) > engine.MaxCellChars {
			return engine.ErrCellTooLong
		}
		v = engine.StringValue(s)
	case "b":
		switch {
		case d.literal("true"):
			v = engine.BoolValue(true)
		case d.literal("false"):
			v = engine.BoolValue(false)
		default:
			return errors.New("expected boolean")
		}
	case "e":
		s, err := d.str()
		if err != nil {
			return err
		}
		code, ok := engine.CanonicalErrorCode(s)
		if !ok {
			return fmt.Errorf("unknown error code %q", s)
		}
		v = engine.ErrorValue(code)
	case "z":
		if !d.literal("null") {
			return errors.New("expected null")
		}
	default:
		return fmt.Errorf("unknown type %q", t)
	}
	c := &engine.Cell{Value: v}
	if d.peek() == ',' {
		d.i++
		f, err := d.str()
		if err != nil {
			return err
		}
		if f != "" && (!strings.HasPrefix(f, "=") || utf8.RuneCountInString(f) > engine.MaxCellChars) {
			return errors.New("invalid formula")
		}
		c.Formula = f
		if d.peek() == ',' {
			d.i++
			si, err := d.integer(len(styles))
			if err != nil {
				return err
			}
			c.Style = styles[si]
		}
	}
	if err := d.expect(']'); err != nil {
		return err
	}
	if !c.IsEmpty() {
		out[engine.Ref{Row: row, Col: col}] = c
	}
	return nil
}

// ---- fast API JSON encoding ---------------------------------------------------------------

// apiEncoder renders cells in the API shape {"v","f","t","e","s"} without reflection; styles
// (immutable, shared) are marshalled once per pointer.
type apiEncoder struct {
	styles map[*engine.Style][]byte
}

func newAPIEncoder() *apiEncoder { return &apiEncoder{styles: map[*engine.Style][]byte{}} }

func (a *apiEncoder) cell(b []byte, c *engine.Cell) []byte {
	b = append(b, `{"v":`...)
	b = appendValue(b, c.Value)
	if c.Formula != "" {
		b = append(b, `,"f":`...)
		b = appendJSONString(b, c.Formula)
	}
	b = append(b, `,"t":"`...)
	b = append(b, c.Value.Kind.TypeCode()...)
	b = append(b, '"')
	if c.Value.Kind == engine.KindError {
		b = append(b, `,"e":`...)
		b = appendJSONString(b, c.Value.Err)
	}
	if !c.Style.IsZero() {
		sb, ok := a.styles[c.Style]
		if !ok {
			sb, _ = json.Marshal(c.Style)
			a.styles[c.Style] = sb
		}
		b = append(b, `,"s":`...)
		b = append(b, sb...)
	}
	return append(b, '}')
}

// cellMap renders {"A1":Cell,...} in row-major order.
func (a *apiEncoder) cellMap(b []byte, cells map[engine.Ref]*engine.Cell) []byte {
	b = append(b, '{')
	n := 0
	for _, rc := range sortedCells(cells) {
		if rc.cell == nil {
			continue
		}
		if n > 0 {
			b = append(b, ',')
		}
		n++
		b = append(b, '"')
		b = appendRef(b, rc.ref)
		b = append(b, '"', ':')
		b = a.cell(b, rc.cell)
	}
	return append(b, '}')
}

// changes renders engine.Changes as {"<sheet_id>":{"A1":Cell}} (sheets and cells sorted).
func (a *apiEncoder) changes(b []byte, ch engine.Changes) []byte {
	ids := make([]string, 0, len(ch))
	for id := range ch {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b = append(b, '{')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, id)
		b = append(b, ':')
		m := ch[id]
		refs := make([]engine.Ref, 0, len(m))
		for r := range m {
			refs = append(refs, r)
		}
		sortRefs(refs)
		b = append(b, '{')
		for j, r := range refs {
			if j > 0 {
				b = append(b, ',')
			}
			b = append(b, '"')
			b = appendRef(b, r)
			b = append(b, '"', ':')
			c := m[r]
			b = a.cell(b, &c)
		}
		b = append(b, '}')
	}
	return append(b, '}')
}

func sortRefs(refs []engine.Ref) {
	slices.SortFunc(refs, func(a, b engine.Ref) int {
		if a.Row != b.Row {
			return a.Row - b.Row
		}
		return a.Col - b.Col
	})
}

// sortedRefs returns the stored positions of cells in row-major order.
func sortedRefs(cells map[engine.Ref]*engine.Cell) []engine.Ref {
	refs := make([]engine.Ref, 0, len(cells))
	for r := range cells {
		refs = append(refs, r)
	}
	sortRefs(refs)
	return refs
}

// appendValue renders a cell value: number, string, bool, the error code string, or null.
func appendValue(b []byte, v engine.Value) []byte {
	switch v.Kind {
	case engine.KindNumber:
		return appendFloat(b, v.Num)
	case engine.KindString:
		return appendJSONString(b, v.Str)
	case engine.KindBool:
		return strconv.AppendBool(b, v.Bool)
	case engine.KindError:
		return appendJSONString(b, v.Err)
	}
	return append(b, "null"...)
}

// appendFloat formats like encoding/json (shortest round-trip representation).
func appendFloat(b []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

const hexDigits = "0123456789abcdef"

// appendJSONString quotes s like encoding/json (HTML-safe, U+2028/2029 escaped, invalid
// UTF-8 replaced by U+FFFD).
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i += size
			start = i
			continue
		}
		if r == ' ' || r == ' ' {
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', '2', '0', '2', hexDigits[r&0xf])
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

type refCell struct {
	ref  engine.Ref
	cell *engine.Cell
}

// sortedCells returns the cells in row-major order (one pass over the map: sorting refs and
// looking each one up again costs a random map access per cell).
func sortedCells(cells map[engine.Ref]*engine.Cell) []refCell {
	out := make([]refCell, 0, len(cells))
	for r, c := range cells {
		out = append(out, refCell{r, c})
	}
	slices.SortFunc(out, func(a, b refCell) int {
		if a.ref.Row != b.ref.Row {
			return a.ref.Row - b.ref.Row
		}
		return a.ref.Col - b.ref.Col
	})
	return out
}

// appendRef appends the A1 form of r without allocating.
func appendRef(b []byte, r engine.Ref) []byte {
	var col [4]byte
	i := len(col)
	for c := r.Col + 1; c > 0; {
		c--
		i--
		col[i] = byte('A' + c%26)
		c /= 26
	}
	b = append(b, col[i:]...)
	return strconv.AppendInt(b, int64(r.Row+1), 10)
}
