package xls

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"ludworks/internal/sheets/engine"
)

type fontRec struct {
	height    uint16
	italic    bool
	strike    bool
	bold      bool
	underline bool
	color     uint16
}

type xfRec struct {
	font, format int
	alc, alcV    byte
	wrap         bool
	dg           [4]byte // top, bottom, left, right
	fls          byte
	icvFore      int
}

type boundSheet struct {
	pos  uint32
	typ  byte
	name string
}

type xti struct {
	supbook, first, last int
}

type reader struct {
	stream   []byte
	biff8    bool
	codepage uint16
	date1904 bool
	fonts    []fontRec
	formats  map[int]string
	xfs      []xfRec
	palette  [56]uint32
	sheets   []boundSheet
	sst      []string
	supbooks []bool // true = this workbook (internal references)
	xtis     []xti
	styles   []*engine.Style
	isDate   []bool
	cells    int
}

// Read parses an .xls file (BIFF8, or BIFF5/7 without formula text).
// Sheet IDs are freshly generated.
func Read(r io.ReaderAt, size int64) (*engine.Workbook, error) {
	if size <= 0 {
		return nil, invalidf("empty file")
	}
	if size > MaxFileSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, size)
	}
	data := make([]byte, size)
	if n, err := r.ReadAt(data, 0); n != int(size) && err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	} else if n != int(size) {
		return nil, invalidf("short read")
	}
	f, err := parseCFB(data)
	if err != nil {
		return nil, err
	}
	stream, _, err := f.stream(MaxFileSize, "Workbook", "Book")
	if err != nil {
		return nil, err
	}
	rd := &reader{stream: stream, formats: map[int]string{}, palette: defaultPalette}
	return rd.read()
}

func (rd *reader) text8(b []byte) string {
	// BIFF5 8-bit text in the workbook code page; Windows-1252 covers the
	// overwhelmingly common Western case and is a safe fallback elsewhere.
	return engine.DecodeCP1252(b)
}

// unicodeString reads an XLUnicodeString (BIFF8) or a byte string (BIFF5)
// with a 16-bit length.
func (rd *reader) unicodeString(c *cursor) string {
	n := int(c.u16())
	if !rd.biff8 {
		return rd.text8(c.bytes(n))
	}
	flags := c.u8()
	runs, ext := 0, 0
	if flags&0x08 != 0 {
		runs = int(c.u16())
	}
	if flags&0x04 != 0 {
		ext = int(c.u32())
	}
	s := c.chars(n, flags&0x01 != 0)
	c.bytes(4 * runs)
	if ext > 0 && ext <= c.rest() {
		c.bytes(ext)
	}
	return s
}

func (rd *reader) read() (*engine.Workbook, error) {
	rec, pos, err := nextRecord(rd.stream, 0)
	if err != nil {
		return nil, err
	}
	if rec.typ != rBOF || len(rec.data) < 4 {
		return nil, invalidf("workbook stream does not start with BOF")
	}
	switch le16(rec.data, 0) {
	case 0x0600:
		rd.biff8 = true
	case 0x0500:
	default:
		return nil, fmt.Errorf("%w: BOF version %#04x", ErrUnsupported, le16(rec.data, 0))
	}
	if le16(rec.data, 2) != 0x0005 {
		return nil, invalidf("first substream is not workbook globals")
	}
	for {
		rec, next, err := nextRecord(rd.stream, pos)
		if err != nil {
			return nil, err
		}
		c := &cursor{b: rec.data}
		switch rec.typ {
		case rEOF:
			return rd.readSheets()
		case rFilePass:
			return nil, ErrEncrypted
		case rCodePage:
			rd.codepage = c.u16()
		case rDateMode:
			rd.date1904 = c.u16() == 1
		case rFont:
			var f fontRec
			f.height = c.u16()
			grbit := c.u16()
			f.italic, f.strike = grbit&0x02 != 0, grbit&0x08 != 0
			f.color = c.u16()
			f.bold = c.u16() >= 700
			c.u16()
			f.underline = c.u8() != 0
			if !c.bad {
				rd.fonts = append(rd.fonts, f)
			}
		case rFormat:
			ifmt := int(c.u16())
			var code string
			if rd.biff8 {
				code = rd.unicodeString(c)
			} else {
				code = rd.text8(c.bytes(int(c.u8())))
			}
			if !c.bad {
				rd.formats[ifmt] = code
			}
		case rXF:
			if x, ok := rd.parseXF(c); ok {
				rd.xfs = append(rd.xfs, x)
			}
		case rPalette:
			n := int(c.u16())
			for i := 0; i < n && i < 56; i++ {
				r, g, b := c.u8(), c.u8(), c.u8()
				c.u8()
				if !c.bad {
					rd.palette[i] = uint32(r)<<16 | uint32(g)<<8 | uint32(b)
				}
			}
		case rBoundSheet:
			bs := boundSheet{pos: c.u32()}
			c.u8()
			bs.typ = c.u8()
			n := int(c.u8())
			if rd.biff8 {
				flags := c.u8()
				bs.name = c.chars(n, flags&0x01 != 0)
			} else {
				bs.name = rd.text8(c.bytes(n))
			}
			if !c.bad {
				rd.sheets = append(rd.sheets, bs)
			}
			if len(rd.sheets) > engine.MaxSheets {
				return nil, fmt.Errorf("%w: too many sheets", ErrTooLarge)
			}
		case rSST:
			segs := [][]byte{rec.data}
			for {
				nr, nn, err := nextRecord(rd.stream, next)
				if err != nil || nr.typ != rContinue {
					break
				}
				segs = append(segs, nr.data)
				next = nn
			}
			if err := rd.parseSST(segs); err != nil {
				return nil, err
			}
		case rSupBook:
			c.u16()
			rd.supbooks = append(rd.supbooks, c.u16() == 0x0401)
		case rExternSheet:
			if rd.biff8 {
				n := int(c.u16())
				for i := 0; i < n && !c.bad; i++ {
					x := xti{supbook: int(c.u16()), first: int(int16(c.u16())), last: int(int16(c.u16()))}
					if !c.bad {
						rd.xtis = append(rd.xtis, x)
					}
				}
			}
		}
		pos = next
	}
}

func (rd *reader) parseXF(c *cursor) (xfRec, bool) {
	var x xfRec
	x.font = int(c.u16())
	x.format = int(c.u16())
	c.u16() // type/protection/parent
	align := c.u8()
	x.alc, x.wrap, x.alcV = align&0x07, align&0x08 != 0, (align>>4)&0x07
	if rd.biff8 {
		c.u8() // rotation
		c.u8() // indent etc.
		c.u8() // used attributes
		b1 := c.u32()
		b2 := c.u32()
		colors := c.u16()
		x.dg = [4]byte{byte(b1>>8) & 0x0F, byte(b1>>12) & 0x0F, byte(b1) & 0x0F, byte(b1>>4) & 0x0F}
		x.fls = byte(b2>>26) & 0x3F
		x.icvFore = int(colors & 0x7F)
	} else {
		c.u8() // orientation / used attributes
		b1 := c.u32()
		b2 := c.u32()
		x.icvFore = int(b1 & 0x7F)
		x.fls = byte(b1>>16) & 0x3F
		x.dg = [4]byte{byte(b2) & 0x07, byte(b1>>22) & 0x07, byte(b2>>3) & 0x07, byte(b2>>6) & 0x07}
	}
	return x, !c.bad
}

// segReader walks SST data spread over SST + CONTINUE records. Character
// data that crosses a record boundary restarts with a fresh option byte.
type segReader struct {
	segs [][]byte
	i    int
	off  int
}

func (s *segReader) avail() int {
	for s.i < len(s.segs) && s.off >= len(s.segs[s.i]) {
		s.i++
		s.off = 0
	}
	if s.i >= len(s.segs) {
		return 0
	}
	return len(s.segs[s.i]) - s.off
}

func (s *segReader) byte1() (byte, bool) {
	if s.avail() == 0 {
		return 0, false
	}
	b := s.segs[s.i][s.off]
	s.off++
	return b, true
}

func (s *segReader) uint(n int) (uint32, bool) {
	var v uint32
	for k := 0; k < n; k++ {
		b, ok := s.byte1()
		if !ok {
			return 0, false
		}
		v |= uint32(b) << (8 * k)
	}
	return v, true
}

func (s *segReader) skip(n int) bool {
	for n > 0 {
		a := s.avail()
		if a == 0 {
			return false
		}
		k := min(a, n)
		s.off += k
		n -= k
	}
	return true
}

func (s *segReader) chars(n int, high bool) ([]uint16, bool) {
	out := make([]uint16, 0, min(n, 1<<16))
	for len(out) < n {
		if s.i >= len(s.segs) {
			return out, false
		}
		seg := s.segs[s.i]
		if s.off >= len(seg) {
			// Character data continues in the next record, which starts
			// with a fresh option byte (the encoding may change).
			s.i++
			s.off = 0
			if s.i >= len(s.segs) || len(s.segs[s.i]) == 0 {
				return out, false
			}
			high = s.segs[s.i][0]&0x01 != 0
			s.off = 1
			continue
		}
		if high {
			if s.off+2 > len(seg) {
				s.off = len(seg) // malformed half character
				continue
			}
			out = append(out, le16(seg, s.off))
			s.off += 2
		} else {
			out = append(out, uint16(seg[s.off]))
			s.off++
		}
	}
	return out, true
}

func (rd *reader) parseSST(segs [][]byte) error {
	s := &segReader{segs: segs}
	s.uint(4) // total references
	unique, ok := s.uint(4)
	if !ok {
		return invalidf("truncated SST")
	}
	for i := uint32(0); i < unique; i++ {
		if len(rd.sst) >= engine.MaxCellsPerWorkbook*2 {
			return fmt.Errorf("%w: shared string table", ErrTooLarge)
		}
		n, ok1 := s.uint(2)
		flags, ok2 := s.byte1()
		if !ok1 || !ok2 {
			break // tolerate a unique count larger than the data
		}
		runs, ext := uint32(0), uint32(0)
		if flags&0x08 != 0 {
			runs, _ = s.uint(2)
		}
		if flags&0x04 != 0 {
			ext, _ = s.uint(4)
		}
		u, ok := s.chars(int(n), flags&0x01 != 0)
		rd.sst = append(rd.sst, truncate(decodeUTF16(u), engine.MaxCellChars))
		if !ok || !s.skip(int(runs)*4) || !s.skip(int(ext)) {
			break
		}
	}
	return nil
}

func (rd *reader) font(i int) *fontRec {
	switch {
	case i == 4:
		return nil // index 4 is never written by Excel
	case i > 4:
		i--
	}
	if i < 0 || i >= len(rd.fonts) {
		return nil
	}
	return &rd.fonts[i]
}

func (rd *reader) buildStyles() {
	rd.styles = make([]*engine.Style, len(rd.xfs))
	rd.isDate = make([]bool, len(rd.xfs))
	var def fontRec
	if len(rd.fonts) > 0 {
		def = rd.fonts[0]
	}
	for i, x := range rd.xfs {
		st := &engine.Style{}
		if f := rd.font(x.font); f != nil {
			st.Bold, st.Italic, st.Strike, st.Underline = f.bold, f.italic, f.strike, f.underline
			if f.height != def.height && f.height >= 20 {
				st.Size = int(math.Round(float64(f.height) / 20))
			}
			if c, ok := paletteColor(&rd.palette, int(f.color)); ok && c != "#000000" {
				st.Color = c
			}
		}
		code, ok := rd.formats[x.format]
		if !ok {
			code, _ = engine.BuiltinNumFmt(x.format)
		}
		if code != "" && code != "General" {
			st.Fmt = truncate(code, engine.MaxFormatLen)
			rd.isDate[i] = engine.IsDateFormat(code)
		}
		switch x.alc {
		case 1:
			st.Align = "left"
		case 2, 6:
			st.Align = "center"
		case 3:
			st.Align = "right"
		case 5, 7:
			st.Align = "left"
		}
		switch x.alcV {
		case 0:
			st.VAlign = "top"
		case 1, 4:
			st.VAlign = "middle"
		}
		st.Wrap = x.wrap
		// any line style counts as a border (the engine models on/off edges)
		b := engine.Border{Top: x.dg[0] != 0, Bottom: x.dg[1] != 0, Left: x.dg[2] != 0, Right: x.dg[3] != 0}
		if b != (engine.Border{}) {
			st.Border = &b
		}
		if x.fls != 0 {
			if c, ok := paletteColor(&rd.palette, x.icvFore); ok {
				st.Bg = c
			}
		}
		if norm, err := st.Normalize(); err == nil {
			rd.styles[i] = norm
		}
	}
}

func (rd *reader) readSheets() (*engine.Workbook, error) {
	rd.buildStyles()
	wb := engine.NewWorkbook()
	used := map[string]bool{}
	for i, bs := range rd.sheets {
		if bs.typ != 0 { // charts, macro sheets, VBA modules
			continue
		}
		s := engine.NewSheet(engine.NewSheetID(), uniqueName(bs.name, i+1, used), 0, 0)
		if err := rd.readSheet(bs, s); err != nil {
			return nil, fmt.Errorf("sheet %q: %w", bs.name, err)
		}
		wb.Sheets = append(wb.Sheets, s)
	}
	if len(wb.Sheets) == 0 {
		return nil, invalidf("no worksheets")
	}
	return wb, nil
}

type formulaCell struct {
	ref  engine.Ref
	rgce []byte
}

type sharedDef struct {
	first, last engine.Ref
	rgce        []byte
}

func (rd *reader) readSheet(bs boundSheet, s *engine.Sheet) error {
	pos := int(bs.pos)
	if pos < 0 || pos >= len(rd.stream) {
		return invalidf("sheet offset %d out of range", pos)
	}
	rec, pos, err := nextRecord(rd.stream, pos)
	if err != nil {
		return err
	}
	if rec.typ != rBOF {
		return invalidf("sheet substream does not start with BOF")
	}
	var (
		formulas []formulaCell
		shared   = map[engine.Ref]sharedDef{}
		arrays   = map[engine.Ref][]byte{}
		pending  *engine.Ref // formula awaiting its STRING result
		frozen   bool
		depth    int
	)
	put := func(r engine.Ref, ixfe int, v engine.Value) error {
		if !r.Valid() {
			return nil
		}
		cell := &engine.Cell{Value: v}
		if ixfe > 0 && ixfe < len(rd.styles) {
			cell.Style = rd.styles[ixfe]
			if rd.date1904 && v.Kind == engine.KindNumber && rd.isDate[ixfe] {
				cell.Value = engine.NumberValue(v.Num + engine.Date1904Offset)
			}
		}
		if cell.IsEmpty() {
			delete(s.Cells, r)
			return nil
		}
		if s.Cells[r] == nil {
			rd.cells++
			if rd.cells > engine.MaxCellsPerWorkbook {
				return fmt.Errorf("%w: more than %d cells", ErrTooLarge, engine.MaxCellsPerWorkbook)
			}
		}
		s.SetCell(r, cell)
		return nil
	}
	for {
		rec, next, err := nextRecord(rd.stream, pos)
		if err != nil {
			return err
		}
		pos = next
		if rec.typ == rBOF {
			depth++ // embedded chart substream
			continue
		}
		if rec.typ == rEOF {
			if depth == 0 {
				break
			}
			depth--
			continue
		}
		if depth > 0 {
			continue
		}
		c := &cursor{b: rec.data}
		switch rec.typ {
		case rLabelSST:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			idx := int(c.u32())
			if !c.bad && idx >= 0 && idx < len(rd.sst) {
				err = put(r, ixfe, engine.StringValue(rd.sst[idx]))
			}
		case rLabel, rRString:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			str := rd.unicodeString(c)
			if !c.bad {
				err = put(r, ixfe, engine.StringValue(truncate(str, engine.MaxCellChars)))
			}
		case rNumber:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			v := c.f64()
			if !c.bad {
				err = put(r, ixfe, engine.NumberValue(v))
			}
		case rRK:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			v := c.u32()
			if !c.bad {
				err = put(r, ixfe, engine.NumberValue(rkValue(v)))
			}
		case rMulRK:
			row := int(c.u16())
			col := int(c.u16())
			n := (len(rec.data) - 6) / 6
			for i := 0; i < n && err == nil && !c.bad; i++ {
				ixfe := int(c.u16())
				v := c.u32()
				if !c.bad {
					err = put(engine.Ref{Row: row, Col: col + i}, ixfe, engine.NumberValue(rkValue(v)))
				}
			}
		case rBoolErr:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			v, isErr := c.u8(), c.u8()
			if !c.bad {
				err = put(r, ixfe, boolErrValue(v, isErr != 0))
			}
		case rBlank:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			if !c.bad {
				err = put(r, ixfe, engine.Value{})
			}
		case rMulBlank:
			row := int(c.u16())
			col := int(c.u16())
			n := (len(rec.data) - 6) / 2
			for i := 0; i < n && err == nil && !c.bad; i++ {
				ixfe := int(c.u16())
				if !c.bad {
					err = put(engine.Ref{Row: row, Col: col + i}, ixfe, engine.Value{})
				}
			}
		case rFormula:
			r := engine.Ref{Row: int(c.u16()), Col: int(c.u16())}
			ixfe := int(c.u16())
			val := c.bytes(8)
			c.u16() // grbit
			c.u32() // chn
			cce := int(c.u16())
			rgce := c.bytes(min(cce, c.rest()))
			if c.bad || val == nil {
				break
			}
			pending = nil
			var v engine.Value
			if le16(val, 6) == 0xFFFF {
				switch val[0] {
				case 0:
					p := r
					pending = &p
					v = engine.StringValue("")
				case 1:
					v = engine.BoolValue(val[2] != 0)
				case 2:
					v = boolErrValue(val[2], true)
				case 3:
					v = engine.StringValue("")
				}
			} else {
				v = engine.NumberValue(math.Float64frombits(uint64(le32(val, 0)) | uint64(le32(val, 4))<<32))
			}
			err = put(r, ixfe, v)
			if rd.biff8 && len(rgce) > 0 {
				formulas = append(formulas, formulaCell{ref: r, rgce: rgce})
			}
		case rString:
			if pending != nil {
				str := truncate(rd.unicodeString(c), engine.MaxCellChars)
				if cell := s.Cell(*pending); cell != nil {
					cell.Value = engine.StringValue(str)
				} else {
					err = put(*pending, 0, engine.StringValue(str))
				}
				pending = nil
			}
		case rShrFmla:
			first := engine.Ref{Row: int(c.u16())}
			last := engine.Ref{Row: int(c.u16())}
			first.Col, last.Col = int(c.u8()), int(c.u8())
			c.u8()
			c.u8()
			cce := int(c.u16())
			rgce := c.bytes(min(cce, c.rest()))
			if !c.bad {
				// ptgExp in member cells points at the range's top-left cell
				shared[first] = sharedDef{first: first, last: last, rgce: rgce}
			}
		case rArray:
			first := engine.Ref{Row: int(c.u16())}
			c.u16()
			first.Col = int(c.u8())
			c.u8()
			c.u16()
			c.u32()
			cce := int(c.u16())
			rgce := c.bytes(min(cce, c.rest()))
			if !c.bad {
				arrays[first] = rgce
			}
		case rColInfo:
			first, last := int(c.u16()), int(c.u16())
			coldx := c.u16()
			c.u16()
			grbit := c.u16()
			if !c.bad && grbit&0x0002 != 0 && last >= first && last-first < MaxCols {
				for col := first; col <= last && col < engine.MaxCols; col++ {
					s.ColWidths[col] = math.Round(float64(coldx) / 256 * 7)
				}
			}
		case rRow:
			row := int(c.u16())
			c.u16()
			c.u16()
			miy := c.u16()
			c.u16()
			c.u16()
			flags := c.u16()
			if !c.bad && flags&0x0040 != 0 && miy&0x7FFF > 0 {
				s.RowHeights[row] = math.Round(float64(miy&0x7FFF)/15*100) / 100
			}
		case rWindow2:
			frozen = c.u16()&0x0008 != 0
		case rPane:
			x, y := int(c.u16()), int(c.u16())
			if !c.bad && frozen {
				s.FrozenCols, s.FrozenRows = min(x, MaxCols-1), min(y, MaxRows-1)
			}
		}
		if err != nil {
			return err
		}
	}
	for _, fc := range formulas {
		text, err := rd.decodeFormula(fc, shared, arrays)
		if err != nil || text == "" {
			continue // keep the cached value
		}
		if engine.FormulaSupported(text) {
			if cell := s.Cell(fc.ref); cell != nil {
				cell.Formula = text
			}
		}
	}
	return nil
}

func boolErrValue(v byte, isErr bool) engine.Value {
	if !isErr {
		return engine.BoolValue(v != 0)
	}
	if code, ok := biffErrors[v]; ok {
		return engine.ErrorValue(code)
	}
	return engine.ErrorValue(engine.CodeValue)
}

func decodeUTF16(u []uint16) string {
	r := make([]rune, 0, len(u))
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c >= 0xD800 && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
			r = append(r, (rune(c)-0xD800)<<10+(rune(u[i+1])-0xDC00)+0x10000)
			i++
			continue
		}
		if c >= 0xD800 && c < 0xE000 {
			r = append(r, 0xFFFD)
			continue
		}
		r = append(r, rune(c))
	}
	return string(r)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func uniqueName(name string, n int, used map[string]bool) string {
	clean := []rune{}
	for _, r := range name {
		if r < 0x20 || r == ':' || r == '\\' || r == '/' || r == '?' || r == '*' || r == '[' || r == ']' {
			r = '_'
		}
		clean = append(clean, r)
	}
	for len(clean) > 0 && clean[0] == '\'' {
		clean = clean[1:]
	}
	for len(clean) > 0 && clean[len(clean)-1] == '\'' {
		clean = clean[:len(clean)-1]
	}
	if len(clean) > engine.MaxSheetNameLen {
		clean = clean[:engine.MaxSheetNameLen]
	}
	name = string(clean)
	if engine.ValidateSheetName(name) != nil {
		name = fmt.Sprintf("Sheet%d", n)
	}
	base := []rune(name)
	for i := 2; used[strings.ToLower(name)]; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		b := base
		if len(b)+len(suffix) > engine.MaxSheetNameLen {
			b = b[:engine.MaxSheetNameLen-len(suffix)]
		}
		name = string(b) + suffix
	}
	used[strings.ToLower(name)] = true
	return name
}
