// Package csvio imports and exports single sheets as CSV/TSV.
//
// Import sniffs the delimiter (, ; tab |), strips a UTF-8 BOM, decodes
// UTF-16 (BOM) and Windows-1252 input, and infers types conservatively:
// plain decimal numbers without leading zeros (≤ 15 significant digits)
// and TRUE/FALSE are converted, and so are the displays this package's
// own export writes for formatted cells — "$1,234.50", "-$5.00", "12%",
// "(3)" and ISO dates such as "2024-01-15" — which keep the implied number
// format, so an exported sheet re-imports as numbers. Thousands separators
// are only recognised in comma-delimited files (elsewhere "1,5" is a
// decimal comma). Everything else, including text that looks like a
// formula, stays text — CSV never creates formulas.
//
// Export writes RFC 4180 output (CRLF by default) and by default guards
// against CSV/formula injection by prefixing an apostrophe to text that a
// spreadsheet would treat as a formula (= + - @ tab CR).
package csvio

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"ludworks/internal/sheets/engine"
)

// DefaultMaxBytes caps the input size (API import limit is 50 MiB).
const DefaultMaxBytes = 50 << 20

// Errors.
var (
	ErrTooLarge = errors.New("csv: input exceeds limits")
	ErrInvalid  = errors.New("csv: invalid input")
)

// ReadOptions configures Read. The zero value sniffs the delimiter and
// infers types.
type ReadOptions struct {
	Delimiter rune   // 0 = sniff among , ; \t |
	MaxCells  int    // 0 = engine.MaxCellsPerWorkbook
	MaxBytes  int64  // 0 = DefaultMaxBytes
	NoInfer   bool   // keep every field as text
	SheetID   string // "" = engine.NewSheetID()
	SheetName string // "" = "Sheet1"
}

// Candidates for delimiter sniffing, in tie-break order.
var candidates = []rune{',', ';', '\t', '|'}

// Read parses CSV text into a new sheet.
func Read(r io.Reader, opts ReadOptions) (*engine.Sheet, error) {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	maxCells := opts.MaxCells
	if maxCells <= 0 {
		maxCells = engine.MaxCellsPerWorkbook
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, maxBytes)
	}
	text := decodeText(data)
	delim := opts.Delimiter
	if delim == 0 {
		delim = Sniff(text)
	}
	if !validDelim(delim) {
		return nil, fmt.Errorf("%w: delimiter %q", ErrInvalid, delim)
	}
	id, name := opts.SheetID, opts.SheetName
	if id == "" {
		id = engine.NewSheetID()
	}
	if name == "" {
		name = "Sheet1"
	}
	if err := engine.ValidateSheetName(name); err != nil {
		return nil, err
	}
	s := engine.NewSheet(id, name, 0, 0)
	cr := csv.NewReader(strings.NewReader(text))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	cells := 0
	for row := 0; ; row++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if row >= engine.MaxRows {
			return nil, fmt.Errorf("%w: more than %d rows", ErrTooLarge, engine.MaxRows)
		}
		for col, field := range rec {
			if field == "" {
				continue
			}
			if col >= engine.MaxCols {
				return nil, fmt.Errorf("%w: more than %d columns", ErrTooLarge, engine.MaxCols)
			}
			if len(field) > engine.MaxCellChars && utf8.RuneCountInString(field) > engine.MaxCellChars {
				return nil, fmt.Errorf("%w: cell %s longer than %d characters", ErrTooLarge, engine.Ref{Row: row, Col: col}, engine.MaxCellChars)
			}
			cells++
			if cells > maxCells {
				return nil, fmt.Errorf("%w: more than %d cells", ErrTooLarge, maxCells)
			}
			cell := &engine.Cell{Value: engine.StringValue(field)}
			if !opts.NoInfer {
				v, format := InferFormatted(field, delim)
				cell.Value = v
				if format != "" {
					cell.Style = &engine.Style{Fmt: format}
				}
			}
			s.SetCell(engine.Ref{Row: row, Col: col}, cell)
		}
	}
	return s, nil
}

func validDelim(r rune) bool {
	return r != 0 && r != '"' && r != '\r' && r != '\n' && utf8.ValidRune(r) && r != utf8.RuneError
}

// decodeText strips BOMs and converts UTF-16 / Windows-1252 input to UTF-8.
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		le := b[0] == 0xFF
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			if le {
				u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
			} else {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			}
		}
		return string(utf16.Decode(u))
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return engine.DecodeCP1252(b)
}

// Sniff guesses the delimiter from (the start of) the text: the candidate
// whose per-line count is non-zero and most consistent across lines wins;
// ties prefer more columns, then , ; tab |. Quoted sections are ignored.
func Sniff(text string) rune {
	const maxSample, maxLines = 64 << 10, 50
	truncated := false
	if len(text) > maxSample {
		text = text[:maxSample]
		truncated = true
	}
	var lines [][4]int
	var cur [4]int
	inQuote := false
	for _, r := range text {
		switch {
		case r == '"':
			inQuote = !inQuote
		case inQuote:
		case r == '\n':
			lines = append(lines, cur)
			cur = [4]int{}
		default:
			for i, c := range candidates {
				if r == c {
					cur[i]++
				}
			}
		}
		if len(lines) >= maxLines {
			break
		}
	}
	if !truncated && len(lines) < maxLines && cur != ([4]int{}) {
		lines = append(lines, cur) // final line without newline
	}
	best, bestCons, bestCols := ',', 0, 0
	for i, c := range candidates {
		freq := map[int]int{}
		for _, l := range lines {
			if l[i] > 0 {
				freq[l[i]]++
			}
		}
		mode, cons := 0, 0
		for n, f := range freq {
			if f > cons || (f == cons && n > mode) {
				mode, cons = n, f
			}
		}
		if cons > bestCons || (cons == bestCons && cons > 0 && mode > bestCols) {
			best, bestCons, bestCols = c, cons, mode
		}
	}
	return best
}

// Infer converts a CSV field conservatively: plain decimal numbers (no
// leading zeros, no thousands separators, ≤ 15 significant digits) and
// TRUE/FALSE. Everything else stays text.
func Infer(s string) engine.Value {
	switch strings.ToUpper(s) {
	case "TRUE":
		return engine.BoolValue(true)
	case "FALSE":
		return engine.BoolValue(false)
	}
	if isPlainNumber(s) {
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) {
			return engine.NumberValue(f)
		}
	}
	return engine.StringValue(s)
}

var (
	// Number displays: "-$1,234.50", "$5", "12.5%", "1,234".
	reFormattedNumber = regexp.MustCompile(`^-?\$?[0-9][0-9,]*(\.[0-9]+)?%?$`)
	// Accounting-style negatives: "(3)", "($1,234.50)".
	reParenNumber = regexp.MustCompile(`^\(\$?[0-9][0-9,]*(\.[0-9]+)?\)$`)
	// ISO dates and date-times as the yyyy-mm-dd formats display them.
	reISODateTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}( [0-9]{1,2}:[0-9]{2}(:[0-9]{2})?)?$`)
)

// InferFormatted is Infer plus the formatted displays a spreadsheet writes
// to CSV for number-formatted cells (this package's export included):
// currency ("$1,234.50", "-$5.00"), percentages ("12%"), thousands
// separators ("1,234" — only when delim is ',', since with other
// delimiters a comma is usually a decimal comma), accounting negatives
// ("(3)") and ISO dates/date-times ("2024-01-15", "2024-01-15 9:30"). It
// returns the value and the number format the text implies ("" = General).
// Leading zeros, more than 15 significant digits, surrounding spaces and
// anything else keep the field as text.
func InferFormatted(s string, delim rune) (engine.Value, string) {
	v := Infer(s)
	if v.Kind != engine.KindString {
		return v, ""
	}
	date := false
	switch {
	case reFormattedNumber.MatchString(s) || reParenNumber.MatchString(s):
		if strings.Contains(s, ",") && delim != ',' {
			return v, ""
		}
		digits := strings.TrimLeft(s, "-($")
		if len(digits) > 1 && digits[0] == '0' && !strings.ContainsRune(".%)", rune(digits[1])) {
			return v, "" // leading zeros: identifiers, zip codes, phone numbers
		}
		if len(strings.TrimLeft(strings.Map(keepDigits, s), "0")) > 15 {
			return v, ""
		}
	case reISODateTime.MatchString(s):
		date = true
	default:
		return v, ""
	}
	in := engine.ParseInput(s)
	if in.Formula != "" || in.Value.Kind != engine.KindNumber {
		return v, ""
	}
	isDateFmt := strings.HasPrefix(in.Fmt, "yyyy-mm-dd")
	if date != isDateFmt {
		return v, ""
	}
	return in.Value, in.Fmt
}

func keepDigits(r rune) rune {
	if r >= '0' && r <= '9' {
		return r
	}
	return -1
}

func isPlainNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	intStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intDigits := s[intStart:i]
	if intDigits == "" {
		return false
	}
	if len(intDigits) > 1 && intDigits[0] == '0' {
		return false // leading zeros: identifiers, zip codes, phone numbers
	}
	fracDigits := ""
	if i < len(s) && s[i] == '.' {
		i++
		fs := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		fracDigits = s[fs:i]
		if fracDigits == "" {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		es := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == es || i-es > 3 {
			return false
		}
	}
	if i != len(s) {
		return false
	}
	sig := strings.TrimLeft(intDigits+fracDigits, "0")
	return len(strings.TrimRight(sig, "0")) <= 15 && len(intDigits) <= 15
}

// WriteOptions configures Write. The zero value writes comma-separated,
// CRLF-terminated, display-formatted values with injection protection on.
type WriteOptions struct {
	Delimiter     rune // 0 = ','
	Raw           bool // write raw values instead of number-formatted display text
	AllowFormulas bool // disable the formula-injection guard (not recommended)
	UseLF         bool // "\n" line endings instead of RFC 4180 "\r\n"
	BOM           bool // prefix a UTF-8 BOM (helps Excel detect UTF-8)
}

// Write exports the sheet's used range (content cells; style-only cells
// are ignored). Formula cells export their computed values.
func Write(w io.Writer, s *engine.Sheet, opts WriteOptions) error {
	delim := opts.Delimiter
	if delim == 0 {
		delim = ','
	}
	if !validDelim(delim) {
		return fmt.Errorf("%w: delimiter %q", ErrInvalid, delim)
	}
	if opts.BOM {
		if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			return err
		}
	}
	used, ok := s.ContentRange()
	if !ok {
		return nil
	}
	cw := csv.NewWriter(w)
	cw.Comma = delim
	cw.UseCRLF = !opts.UseLF
	formats := map[string]*engine.NumberFormat{}
	rec := make([]string, used.To.Col+1)
	for r := 0; r <= used.To.Row; r++ {
		for c := range rec {
			rec[c] = ""
			cell := s.Cells[engine.Ref{Row: r, Col: c}]
			if cell == nil {
				continue
			}
			rec[c] = cellText(cell, opts, formats)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func cellText(c *engine.Cell, opts WriteOptions, formats map[string]*engine.NumberFormat) string {
	v := c.Value
	var out string
	switch {
	case v.Kind == engine.KindNumber && opts.Raw:
		out = rawNumber(v.Num)
	case v.Kind == engine.KindNumber && c.Style != nil && c.Style.Fmt != "":
		nf := formats[c.Style.Fmt]
		if nf == nil {
			nf = engine.CompileFormat(c.Style.Fmt)
			formats[c.Style.Fmt] = nf
		}
		out, _ = nf.Format(v)
	default:
		out = v.Text()
	}
	if opts.AllowFormulas || out == "" {
		return out
	}
	if needsGuard(out, v.Kind == engine.KindNumber) {
		return "'" + out
	}
	return out
}

// needsGuard reports whether a spreadsheet application could interpret the
// field as a formula. Plain numeric displays such as "-1,234.50" or "-5%"
// are left alone; anything else starting with = + - @ tab or CR is guarded.
func needsGuard(s string, number bool) bool {
	switch s[0] {
	case '=', '@', '\t', '\r':
		return true
	case '+', '-':
		if !number {
			return true
		}
		return strings.TrimLeft(s[1:], "0123456789.,%$€£¥ E()+-") != ""
	}
	return false
}

func rawNumber(f float64) string {
	a := math.Abs(f)
	if f == 0 || (a >= 1e-7 && a < 1e21) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
