package xls

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"ludworks/internal/sheets/engine"
)

// Excel 97-2003 limits on distinct formatting records.
const (
	maxXFs   = 4000
	maxFonts = 500
)

const (
	defaultFontHeight = 200 // Arial 10pt, in twips
	firstCellXF       = 16  // 15 style XFs + the default cell XF (15)
	defaultCellXF     = 15
	icvAuto           = 0x7FFF
)

// Write serialises the workbook as BIFF8 in an OLE2 compound file.
// Formulas are written as FORMULA records (BIFF8 tokens plus the cached
// result, recalculated by Excel on load); a formula BIFF8 cannot express
// (see ptgwrite.go) is exported as its cached value only. #CIRC! is written
// as #VALUE!. Sheets larger than the BIFF8 grid (65536 rows × 256 columns)
// are rejected with ErrTooLarge.
func Write(w io.Writer, wb *engine.Workbook) error {
	if wb == nil || len(wb.Sheets) == 0 {
		return errors.New("xls: workbook has no sheets")
	}
	for _, s := range wb.Sheets {
		if r, ok := s.UsedRange(); ok && (r.To.Row >= MaxRows || r.To.Col >= MaxCols) {
			return fmt.Errorf("%w: sheet %q exceeds %d rows × %d columns", ErrTooLarge, s.Name, MaxRows, MaxCols)
		}
	}
	bw := newBookWriter()
	bw.ptg = newPtgEncoder(wb)
	if err := bw.collectStyles(wb); err != nil {
		return err
	}
	stream := bw.build(wb)
	return writeCFB(w, "Workbook", stream)
}

type fontKey struct {
	bold, italic, underline, strike bool
	color                           uint16
	height                          uint16
}

type xfKey struct {
	font, ifmt int
	alc, alcV  byte
	wrap       bool
	dg         [4]byte // top, bottom, left, right
	bg         int     // palette index or -1
}

type bookWriter struct {
	palette  [56]uint32
	colorIdx map[string]int
	fonts    []fontKey
	fontIdx  map[fontKey]int
	formats  []string
	fmtIdx   map[string]int
	xfs      []xfKey
	xfIdx    map[xfKey]int
	styleXF  map[*engine.Style]int
	sst      []string
	sstIdx   map[string]int
	sstTotal int
	ptg      *ptgEncoder
}

func newBookWriter() *bookWriter {
	return &bookWriter{palette: defaultPalette, colorIdx: map[string]int{}, fontIdx: map[fontKey]int{},
		fmtIdx: map[string]int{}, xfIdx: map[xfKey]int{}, styleXF: map[*engine.Style]int{}, sstIdx: map[string]int{}}
}

func parseHex(c string) (uint32, bool) {
	if len(c) != 7 || c[0] != '#' {
		return 0, false
	}
	v, err := strconv.ParseUint(c[1:], 16, 32)
	return uint32(v), err == nil
}

// assignColors maps every colour used by the workbook's styles to a palette
// index, reusing default entries and overwriting unused slots for custom
// colours (nearest match once all 56 slots are taken).
func (bw *bookWriter) assignColors(colors []string) {
	sort.Strings(colors)
	used := map[int]bool{}
	var custom []string
	for _, c := range colors {
		v, ok := parseHex(c)
		if !ok {
			continue
		}
		found := -1
		for i, p := range defaultPalette {
			if p == v {
				found = i
				break
			}
		}
		if found >= 0 && !used[found] {
			used[found] = true
			bw.colorIdx[c] = found + 8
		} else if found >= 0 {
			bw.colorIdx[c] = found + 8
		} else {
			custom = append(custom, c)
		}
	}
	slot := 55
	for _, c := range custom {
		v, _ := parseHex(c)
		for slot >= 0 && used[slot] {
			slot--
		}
		if slot >= 0 {
			bw.palette[slot] = v
			used[slot] = true
			bw.colorIdx[c] = slot + 8
			continue
		}
		best, bestD := 0, math.MaxFloat64
		for i, p := range bw.palette {
			dr := float64(int(p>>16&0xff) - int(v>>16&0xff))
			dg := float64(int(p>>8&0xff) - int(v>>8&0xff))
			db := float64(int(p&0xff) - int(v&0xff))
			if d := dr*dr + dg*dg + db*db; d < bestD {
				best, bestD = i, d
			}
		}
		bw.colorIdx[c] = best + 8
	}
}

// thinLine returns the BIFF line style for an edge (1 = thin, 0 = none).
func thinLine(on bool) byte {
	if on {
		return 1
	}
	return 0
}

func (bw *bookWriter) collectStyles(wb *engine.Workbook) error {
	seen := map[*engine.Style]bool{}
	var styles []*engine.Style
	colorSet := map[string]bool{}
	for _, s := range wb.Sheets {
		for _, r := range s.SortedRefs() {
			st := s.Cells[r].Style
			if st.IsZero() || seen[st] {
				continue
			}
			seen[st] = true
			styles = append(styles, st)
			if st.Color != "" {
				colorSet[st.Color] = true
			}
			if st.Bg != "" {
				colorSet[st.Bg] = true
			}
		}
	}
	colors := make([]string, 0, len(colorSet))
	for c := range colorSet {
		colors = append(colors, c)
	}
	bw.assignColors(colors)
	for _, st := range styles {
		fk := fontKey{bold: st.Bold, italic: st.Italic, underline: st.Underline, strike: st.Strike, color: icvAuto, height: defaultFontHeight}
		if st.Color != "" {
			fk.color = uint16(bw.colorIdx[st.Color])
		}
		if st.Size > 0 {
			fk.height = uint16(min(st.Size, 409) * 20)
		}
		font := 0
		if fk != (fontKey{color: icvAuto, height: defaultFontHeight}) {
			i, ok := bw.fontIdx[fk]
			if !ok {
				i = len(bw.fonts)
				bw.fontIdx[fk] = i
				bw.fonts = append(bw.fonts, fk)
				if len(bw.fonts) > maxFonts {
					return fmt.Errorf("%w: more than %d distinct fonts", ErrTooLarge, maxFonts)
				}
			}
			font = i + 5 // indexes 0-3 are the defaults, 4 is never used
		}
		ifmt := 0
		if st.Fmt != "" {
			if id, ok := engine.BuiltinNumFmtID(st.Fmt); ok {
				ifmt = id
			} else if id, ok := bw.fmtIdx[st.Fmt]; ok {
				ifmt = id
			} else {
				ifmt = 164 + len(bw.formats)
				bw.fmtIdx[st.Fmt] = ifmt
				bw.formats = append(bw.formats, st.Fmt)
			}
		}
		k := xfKey{font: font, ifmt: ifmt, alcV: 2, wrap: st.Wrap, bg: -1}
		switch st.Align {
		case "left":
			k.alc = 1
		case "center":
			k.alc = 2
		case "right":
			k.alc = 3
		}
		switch st.VAlign {
		case "top":
			k.alcV = 0
		case "middle":
			k.alcV = 1
		}
		if b := st.Border; b != nil {
			k.dg = [4]byte{thinLine(b.Top), thinLine(b.Bottom), thinLine(b.Left), thinLine(b.Right)}
		}
		if st.Bg != "" {
			k.bg = bw.colorIdx[st.Bg]
		}
		id, ok := bw.xfIdx[k]
		if !ok {
			id = firstCellXF + len(bw.xfs)
			bw.xfIdx[k] = id
			bw.xfs = append(bw.xfs, k)
			if len(bw.xfs) > maxXFs {
				return fmt.Errorf("%w: more than %d distinct cell styles", ErrTooLarge, maxXFs)
			}
		}
		bw.styleXF[st] = id
	}
	return nil
}

func (bw *bookWriter) xfFor(st *engine.Style) uint16 {
	if st.IsZero() {
		return defaultCellXF
	}
	return uint16(bw.styleXF[st])
}

func (bw *bookWriter) sstAdd(s string) uint32 {
	bw.sstTotal++
	if i, ok := bw.sstIdx[s]; ok {
		return uint32(i)
	}
	i := len(bw.sst)
	bw.sstIdx[s] = i
	bw.sst = append(bw.sst, s)
	return uint32(i)
}

func bofPayload(dt uint16) payload {
	return payload{}.u16(0x0600).u16(dt).u16(0x0DBB).u16(0x07CC).u32(0).u32(0x06)
}

func fontPayload(k fontKey) payload {
	p := payload{}.u16(k.height)
	var grbit uint16
	if k.italic {
		grbit |= 0x02
	}
	if k.strike {
		grbit |= 0x08
	}
	p = p.u16(grbit).u16(k.color)
	if k.bold {
		p = p.u16(700)
	} else {
		p = p.u16(400)
	}
	p = p.u16(0)
	if k.underline {
		p = p.u8(1)
	} else {
		p = p.u8(0)
	}
	p = p.u8(2).u8(0).u8(0) // family swiss, ANSI charset, reserved
	return shortXLString(p, "Arial")
}

func xfPayload(font, ifmt int, style bool, alc, alcV byte, wrap bool, used byte, dg [4]byte, bg int) payload {
	p := payload{}.u16(uint16(font)).u16(uint16(ifmt))
	typeProt := uint16(0x0001) // locked
	if style {
		typeProt |= 0x0004 | 0xFFF0 // style XF, no parent
	}
	p = p.u16(typeProt)
	align := alc&0x07 | (alcV&0x07)<<4
	if wrap {
		align |= 0x08
	}
	p = p.u8(align).u8(0).u8(0).u8(used << 2)
	icv := func(d byte) uint32 {
		if d != 0 {
			return 64
		}
		return 0
	}
	top, bottom, left, right := dg[0], dg[1], dg[2], dg[3]
	b1 := uint32(left) | uint32(right)<<4 | uint32(top)<<8 | uint32(bottom)<<12 | icv(left)<<16 | icv(right)<<23
	b2 := icv(top) | icv(bottom)<<7
	fore, back := uint16(64), uint16(65)
	if bg >= 0 {
		b2 |= 1 << 26 // solid fill
		fore = uint16(bg)
	}
	return p.u32(b1).u32(b2).u16(fore | back<<7)
}

// build produces the complete Workbook stream.
func (bw *bookWriter) build(wb *engine.Workbook) []byte {
	// Sheets first: they fill the shared string table.
	sheets := make([][]byte, len(wb.Sheets))
	for i, s := range wb.Sheets {
		sheets[i] = bw.sheetStream(s, i == 0)
	}
	g := &recWriter{}
	g.rec(rBOF, bofPayload(0x0005))
	g.rec(rInterfaceHdr, payload{}.u16(1200))
	g.rec(0x00C1, payload{}.u16(0)) // MMS
	g.rec(0x00E2, nil)              // INTERFACEEND
	g.rec(rCodePage, payload{}.u16(1200))
	g.rec(rWindow1, payload{}.u16(0).u16(0).u16(0x4000).u16(0x2000).u16(0x0038).u16(0).u16(0).u16(1).u16(0x0258))
	g.rec(rDateMode, payload{}.u16(0))
	g.rec(rPrecision, payload{}.u16(1))
	for i := 0; i < 4; i++ {
		g.rec(rFont, fontPayload(fontKey{color: icvAuto, height: defaultFontHeight}))
	}
	for _, k := range bw.fonts {
		g.rec(rFont, fontPayload(k))
	}
	for i, code := range bw.formats {
		g.rec(rFormat, xlString(payload{}.u16(uint16(164+i)), code))
	}
	g.rec(rXF, xfPayload(0, 0, true, 0, 2, false, 0x00, [4]byte{}, -1))
	for i := 1; i < 15; i++ {
		g.rec(rXF, xfPayload(0, 0, true, 0, 2, false, 0x3D, [4]byte{}, -1))
	}
	g.rec(rXF, xfPayload(0, 0, false, 0, 2, false, 0x00, [4]byte{}, -1))
	for _, k := range bw.xfs {
		g.rec(rXF, xfPayload(k.font, k.ifmt, false, k.alc, k.alcV, k.wrap, 0x3F, k.dg, k.bg))
	}
	g.rec(rStyle, payload{}.u16(0x8000).u8(0).u8(0xFF))
	if bw.palette != defaultPalette {
		p := payload{}.u16(56)
		for _, c := range bw.palette {
			p = p.u8(byte(c >> 16)).u8(byte(c >> 8)).u8(byte(c)).u8(0)
		}
		g.rec(rPalette, p)
	}
	sheetPosFields := make([]int, len(wb.Sheets))
	for i, s := range wb.Sheets {
		sheetPosFields[i] = g.pos() + 4
		g.rec(rBoundSheet, shortXLString(payload{}.u32(0).u8(0).u8(0), s.Name))
	}
	if xtis := bw.ptg.xtis; len(xtis) > 0 {
		// 3-D references in formulas point into this workbook through EXTERNSHEET.
		g.rec(rSupBook, payload{}.u16(uint16(len(wb.Sheets))).u16(0x0401))
		p := payload{}.u16(uint16(len(xtis)))
		for _, sheet := range xtis {
			p = p.u16(0).u16(uint16(sheet)).u16(uint16(sheet))
		}
		g.rec(rExternSheet, p)
	}
	bw.writeSST(g)
	g.rec(rEOF, nil)
	off := len(g.buf)
	for i, sh := range sheets {
		binary.LittleEndian.PutUint32(g.buf[sheetPosFields[i]:], uint32(off))
		off += len(sh)
	}
	out := g.buf
	for _, sh := range sheets {
		out = append(out, sh...)
	}
	return out
}

// writeSST emits SST (+ CONTINUE) and EXTSST. Strings never split inside
// their header; character data split across records restarts with an
// option byte, as BIFF8 requires.
func (bw *bookWriter) writeSST(g *recWriter) {
	dsst := max(8, (len(bw.sst)+127)/128)
	cur := payload{}.u32(uint32(bw.sstTotal)).u32(uint32(len(bw.sst)))
	curType := uint16(rSST)
	curStart := g.pos()
	flush := func() {
		g.rec(curType, cur)
		curType, cur, curStart = rContinue, payload{}, g.pos()
	}
	var buckets payload
	for i, s := range bw.sst {
		b, high := encodeChars(s)
		size := 1
		var grbit byte
		if high {
			size, grbit = 2, 1
		}
		if maxRecLen-len(cur) < 3+min(size, len(b)) {
			flush()
		}
		if i%dsst == 0 {
			buckets = buckets.u32(uint32(curStart + 4 + len(cur))).u16(uint16(4 + len(cur))).u16(0)
		}
		cur = cur.u16(uint16(len(b) / size)).u8(grbit)
		for rest := b; len(rest) > 0; {
			space := maxRecLen - len(cur)
			space -= space % size
			if space <= 0 {
				flush()
				cur = cur.u8(grbit)
				continue
			}
			n := min(len(rest), space)
			cur = append(cur, rest[:n]...)
			rest = rest[n:]
		}
	}
	flush()
	g.rec(rExtSST, append(payload{}.u16(uint16(dsst)), buckets...))
}

func (bw *bookWriter) sheetStream(s *engine.Sheet, selected bool) []byte {
	w := &recWriter{}
	w.rec(rBOF, bofPayload(0x0010))
	cols := make([]int, 0, len(s.ColWidths))
	for c := range s.ColWidths {
		if c >= 0 && c < MaxCols {
			cols = append(cols, c)
		}
	}
	sort.Ints(cols)
	for _, c := range cols {
		coldx := math.Round(s.ColWidths[c] / 7 * 256)
		coldx = math.Max(0, math.Min(coldx, 65535))
		w.rec(rColInfo, payload{}.u16(uint16(c)).u16(uint16(c)).u16(uint16(coldx)).u16(defaultCellXF).u16(0x0002).u16(0))
	}
	refs := s.SortedRefs()
	dim := payload{}.u32(0).u32(0).u16(0).u16(0).u16(0)
	if len(refs) > 0 {
		r, _ := s.UsedRange()
		dim = payload{}.u32(uint32(r.From.Row)).u32(uint32(r.To.Row + 1)).u16(uint16(r.From.Col)).u16(uint16(r.To.Col + 1)).u16(0)
	}
	w.rec(rDimensions, dim)

	// Rows: blocks of up to 32 ROW records followed by their cells.
	rowSet := map[int]bool{}
	for _, r := range refs {
		rowSet[r.Row] = true
	}
	for r := range s.RowHeights {
		if r >= 0 && r < MaxRows {
			rowSet[r] = true
		}
	}
	rows := make([]int, 0, len(rowSet))
	for r := range rowSet {
		rows = append(rows, r)
	}
	sort.Ints(rows)
	byRow := map[int][]engine.Ref{}
	for _, r := range refs {
		byRow[r.Row] = append(byRow[r.Row], r)
	}
	for start := 0; start < len(rows); {
		end := start
		block := rows[start] / 32
		for end < len(rows) && rows[end]/32 == block {
			end++
		}
		for _, row := range rows[start:end] {
			colMic, colMac := 0, 0
			if rc := byRow[row]; len(rc) > 0 {
				colMic, colMac = rc[0].Col, rc[len(rc)-1].Col+1
			}
			miy, flags := uint16(0x00FF), uint16(0x0100)
			if h, ok := s.RowHeights[row]; ok && h > 0 {
				miy = uint16(math.Max(1, math.Min(math.Round(h*15), 8192)))
				flags |= 0x0040
			}
			w.rec(rRow, payload{}.u16(uint16(row)).u16(uint16(colMic)).u16(uint16(colMac)).u16(miy).u16(0).u16(0).u16(flags).u16(defaultCellXF))
		}
		for _, row := range rows[start:end] {
			for _, ref := range byRow[row] {
				bw.cellRecord(w, ref, s.Cells[ref])
			}
		}
		start = end
	}
	grbit := uint16(0x0002 | 0x0004 | 0x0010 | 0x0020 | 0x0080)
	frozen := s.FrozenRows > 0 || s.FrozenCols > 0
	if frozen {
		grbit |= 0x0008 | 0x0100
	}
	if selected {
		grbit |= 0x0200 | 0x0400
	}
	w.rec(rWindow2, payload{}.u16(grbit).u16(0).u16(0).u16(0x40).u16(0).u16(0).u16(0).u16(0).u16(0))
	if frozen {
		fr, fc := min(s.FrozenRows, MaxRows-1), min(s.FrozenCols, MaxCols-1)
		var pane byte
		switch {
		case fr > 0 && fc > 0:
			pane = 0
		case fc > 0:
			pane = 1
		default:
			pane = 2
		}
		w.rec(rPane, payload{}.u16(uint16(fc)).u16(uint16(fr)).u16(uint16(fr)).u16(uint16(fc)).u8(pane).u8(0))
	}
	w.rec(rEOF, nil)
	return w.buf
}

// formulaRecord writes c as FORMULA (+ STRING for a text result) and reports
// whether it could; otherwise the caller writes the cached value alone.
func (bw *bookWriter) formulaRecord(w *recWriter, head payload, c *engine.Cell) bool {
	rgce, err := bw.ptg.encode(c.Formula)
	if err != nil {
		return false
	}
	var result payload
	var text []byte
	textHigh := false
	switch v := c.Value; v.Kind {
	case engine.KindNumber:
		result = payload{}.f64(v.Num)
	case engine.KindString:
		if v.Str == "" {
			result = payload{3, 0, 0, 0, 0, 0, 0xFF, 0xFF}
			break
		}
		text, textHigh = encodeChars(v.Str)
		if 3+len(text) > maxRecLen {
			return false // would need CONTINUE records: keep the value-only export
		}
		result = payload{0, 0, 0, 0, 0, 0, 0xFF, 0xFF}
	case engine.KindBool:
		var b byte
		if v.Bool {
			b = 1
		}
		result = payload{1, 0, b, 0, 0, 0, 0xFF, 0xFF}
	case engine.KindError:
		result = payload{2, 0, biffErrorCode(v.Err), 0, 0, 0, 0xFF, 0xFF}
	default:
		result = payload{}.f64(0)
	}
	// grbit fAlwaysCalc: Excel recalculates instead of trusting the cached result.
	w.rec(rFormula, append(append(head, result...).u16(0x0001).u32(0).u16(uint16(len(rgce))), rgce...))
	if text != nil {
		n := len(text)
		var flags byte
		if textHigh {
			n /= 2
			flags = 1
		}
		w.rec(rString, append(payload{}.u16(uint16(n)).u8(flags), text...))
	}
	return true
}

func (bw *bookWriter) cellRecord(w *recWriter, r engine.Ref, c *engine.Cell) {
	xf := bw.xfFor(c.Style)
	head := payload{}.u16(uint16(r.Row)).u16(uint16(r.Col)).u16(xf)
	if c.Formula != "" && bw.formulaRecord(w, head, c) {
		return
	}
	switch v := c.Value; v.Kind {
	case engine.KindNumber:
		w.rec(rNumber, head.f64(v.Num))
	case engine.KindString:
		w.rec(rLabelSST, head.u32(bw.sstAdd(v.Str)))
	case engine.KindBool:
		b := byte(0)
		if v.Bool {
			b = 1
		}
		w.rec(rBoolErr, head.u8(b).u8(0))
	case engine.KindError:
		w.rec(rBoolErr, head.u8(biffErrorCode(v.Err)).u8(1))
	default:
		if !c.Style.IsZero() {
			w.rec(rBlank, head)
		}
	}
}
