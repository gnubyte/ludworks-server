package xls

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"

	"ludworks/internal/sheets/engine"
)

func mref(t testing.TB, a1 string) engine.Ref {
	t.Helper()
	r, err := engine.ParseRef(a1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readBytes(b []byte) (*engine.Workbook, error) { return Read(bytes.NewReader(b), int64(len(b))) }

func roundTripWorkbook(t testing.TB) *engine.Workbook {
	s1 := engine.NewSheet("a", "Données 1", 0, 0)
	s2 := engine.NewSheet("b", "Other", 0, 0)
	put := func(s *engine.Sheet, a1 string, c engine.Cell) { s.SetCell(mref(t, a1), &c) }
	put(s1, "A1", engine.Cell{Value: engine.NumberValue(1)})
	put(s1, "A2", engine.Cell{Value: engine.NumberValue(-1234.5678)})
	put(s1, "A3", engine.Cell{Value: engine.NumberValue(1e300)})
	put(s1, "A4", engine.Cell{Value: engine.NumberValue(0.1)})
	put(s1, "B1", engine.Cell{Value: engine.StringValue("plain")})
	put(s1, "B2", engine.Cell{Value: engine.StringValue("Ünïcødé ☃ 🎉 日本")})
	put(s1, "B3", engine.Cell{Value: engine.StringValue("")})
	put(s1, "B4", engine.Cell{Value: engine.StringValue(strings.Repeat("latin-1 é ", 1500))}) // > 8224 bytes: CONTINUE
	put(s1, "B5", engine.Cell{Value: engine.StringValue(strings.Repeat("ユニコード", 1500))})      // UTF-16 across records
	put(s1, "B6", engine.Cell{Value: engine.StringValue("plain")})                            // shared string reuse
	put(s1, "C1", engine.Cell{Value: engine.BoolValue(true)})
	put(s1, "C2", engine.Cell{Value: engine.BoolValue(false)})
	put(s1, "C3", engine.Cell{Value: engine.ErrorValue(engine.CodeNA)})
	put(s1, "C4", engine.Cell{Value: engine.ErrorValue(engine.CodeDiv0)})
	full := &engine.Style{Bold: true, Italic: true, Underline: true, Strike: true, Color: "#123456", Bg: "#fedcba",
		Align: "center", VAlign: "middle", Wrap: true, Fmt: "yyyy-mm-dd", Size: 14,
		Border: &engine.Border{Top: true, Bottom: true, Left: true, Right: true}}
	put(s1, "D1", engine.Cell{Value: engine.NumberValue(45306), Style: full})
	put(s1, "D2", engine.Cell{Value: engine.NumberValue(0.25), Style: &engine.Style{Fmt: "0.00%", Color: "#ff0000"}})
	put(s1, "D3", engine.Cell{Value: engine.NumberValue(5), Style: &engine.Style{Fmt: `#,##0.0" units"`, Align: "right", VAlign: "top"}})
	put(s1, "D4", engine.Cell{Style: &engine.Style{Bg: "#00ff00", Border: &engine.Border{Bottom: true}}})
	put(s1, "D5", engine.Cell{Value: engine.StringValue("x"), Style: &engine.Style{Align: "left", Border: &engine.Border{Top: true}}})
	put(s1, "IV100", engine.Cell{Value: engine.NumberValue(7)})
	s1.ColWidths[0] = 120
	s1.ColWidths[3] = 63
	s1.RowHeights[0] = 24
	s1.RowHeights[40] = 40
	s1.FrozenRows, s1.FrozenCols = 1, 2
	for i := 0; i < 2000; i++ { // many unique strings span several CONTINUE records
		put(s2, "A"+itoa(i+1), engine.Cell{Value: engine.StringValue("row " + itoa(i) + " " + strings.Repeat("·", i%7))})
	}
	put(s2, "B1", engine.Cell{Formula: "=1+1", Value: engine.NumberValue(2)})
	put(s2, "B2", engine.Cell{Formula: "=A1", Value: engine.ErrorValue(engine.CodeCirc)})
	s2.FrozenCols = 1
	return &engine.Workbook{Sheets: []*engine.Sheet{s1, s2}}
}

func itoa(i int) string {
	return strings.TrimLeft(string([]byte{byte('0' + i/1000%10), byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)}), "0") + map[bool]string{true: "0", false: ""}[i == 0]
}

func TestRoundTrip(t *testing.T) {
	wb := roundTripWorkbook(t)
	var buf bytes.Buffer
	if err := Write(&buf, wb); err != nil {
		t.Fatal(err)
	}
	back, err := readBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Sheets) != 2 || back.Sheets[0].Name != "Données 1" || back.Sheets[1].Name != "Other" {
		t.Fatalf("sheets %d", len(back.Sheets))
	}
	for i, s := range wb.Sheets {
		got := back.Sheets[i]
		if len(got.Cells) != len(s.Cells) {
			t.Errorf("sheet %d: %d cells want %d", i, len(got.Cells), len(s.Cells))
		}
		for r, c := range s.Cells {
			want := *c
			if want.Value.Kind == engine.KindError && want.Value.Err == engine.CodeCirc {
				want.Value = engine.ErrorValue(engine.CodeValue)
			}
			gc := got.Cell(r)
			if gc == nil {
				t.Errorf("%s!%s missing", s.Name, r)
				continue
			}
			if !gc.Value.Equal(want.Value) || gc.Formula != want.Formula || !gc.Style.Equal(want.Style) {
				t.Errorf("%s!%s = %v %+v, want %v %+v", s.Name, r, gc.Value, gc.Style, want.Value, want.Style)
			}
		}
		if got.FrozenRows != s.FrozenRows || got.FrozenCols != s.FrozenCols {
			t.Errorf("frozen %d %d", got.FrozenRows, got.FrozenCols)
		}
		if len(got.ColWidths) != len(s.ColWidths) || len(got.RowHeights) != len(s.RowHeights) {
			t.Errorf("sizes %v %v", got.ColWidths, got.RowHeights)
		}
		for c, w := range s.ColWidths {
			if got.ColWidths[c] != w {
				t.Errorf("col %d width %v want %v", c, got.ColWidths[c], w)
			}
		}
		for r, h := range s.RowHeights {
			if got.RowHeights[r] != h {
				t.Errorf("row %d height %v want %v", r, got.RowHeights[r], h)
			}
		}
	}
	// byte-identical output for identical input
	var buf2 bytes.Buffer
	Write(&buf2, wb)
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Errorf("output not deterministic")
	}
}

// Regression (e2e): an .xls export dropped every formula (only cached values
// were written), so an exported workbook re-imported without formulas.
func TestFormulaRoundTrip(t *testing.T) {
	data := engine.NewSheet("d", "Data sheet ✓", 0, 0)
	calc := engine.NewSheet("c", "Calc", 0, 0)
	put := func(s *engine.Sheet, a1 string, c engine.Cell) { s.SetCell(mref(t, a1), &c) }
	num, str := engine.NumberValue, engine.StringValue
	put(data, "A1", engine.Cell{Value: num(10)})
	put(data, "A2", engine.Cell{Value: num(20)})
	put(data, "B1", engine.Cell{Value: str("x")})
	formulas := map[string]struct {
		f string
		v engine.Value
	}{
		"A1":  {"=SUM('Data sheet ✓'!A1:A2)", num(30)},
		"A2":  {"='Data sheet ✓'!$A$1*2", num(20)},
		"A3":  {"=A1+A2-(B1/4)^2", num(50)},
		"A4":  {"=-A1%", num(-0.3)},
		"A5":  {"=IF(A1>10,\"big\",\"small\")", str("big")},
		"A6":  {"=CONCATENATE(\"a\",\"b\",UPPER('Data sheet ✓'!B1))", str("abX")},
		"A7":  {"=AND(TRUE,NOT(FALSE))", engine.BoolValue(true)},
		"A8":  {"=1/0", engine.ErrorValue(engine.CodeDiv0)},
		"A9":  {"=ROUND(PI(),2)", num(3.14)},
		"A10": {"=SUM(Calc!$B$1:B2,70000,0.5)", num(70000.5)},
		"A11": {"=COUNT(B:B)", num(0)},
		"A12": {"=IF(A1=30,,1)", num(0)},
		"A13": {"=\"\"", str("")},
		"A14": {"=#N/A", engine.ErrorValue(engine.CodeNA)},
		"A15": {"=VLOOKUP(\"x\",'Data sheet ✓'!B1:B1,1,FALSE)&\"ü日本\"", str("xü日本")},
	}
	for a1, f := range formulas {
		put(calc, a1, engine.Cell{Formula: f.f, Value: f.v})
	}
	// Not expressible in BIFF8: kept as cached values.
	valueOnly := map[string]engine.Cell{
		"C1": {Formula: "=IFERROR(1/0,0)", Value: num(0)}, // Excel 2007 function
		"C2": {Formula: "=SUM({1,2})", Value: num(3)},     // array constant
		"C3": {Formula: "=\"" + strings.Repeat("y", 300) + "\"", Value: str(strings.Repeat("y", 300))},
		"C4": {Formula: "=A1", Value: str(strings.Repeat("z", 9000))}, // result too long for one STRING record
	}
	for a1, c := range valueOnly {
		put(calc, a1, c)
	}
	var buf bytes.Buffer
	if err := Write(&buf, &engine.Workbook{Sheets: []*engine.Sheet{data, calc}}); err != nil {
		t.Fatal(err)
	}
	back, err := readBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got := back.Sheets[1]
	for a1, f := range formulas {
		c := got.Cell(mref(t, a1))
		if c == nil {
			t.Errorf("%s missing", a1)
			continue
		}
		want := f.f
		if a1 == "A11" {
			want = "=COUNT(B1:B65536)" // whole columns are the BIFF8 grid's rows
		}
		if c.Formula != want || !c.Value.Equal(f.v) {
			t.Errorf("%s = %q %v, want %q %v", a1, c.Formula, c.Value, want, f.v)
		}
	}
	for a1, w := range valueOnly {
		c := got.Cell(mref(t, a1))
		if c == nil || c.Formula != "" || !c.Value.Equal(w.Value) {
			t.Errorf("%s = %+v, want value-only %v", a1, c, w.Value)
		}
	}
	if c := back.Sheets[0].Cell(mref(t, "A2")); c == nil || c.Formula != "" || c.Value.Num != 20 {
		t.Errorf("plain values unaffected: %+v", c)
	}
}

func TestWriteLimits(t *testing.T) {
	s := engine.NewSheet("a", "S", 0, 0)
	s.SetCell(engine.Ref{Row: 0, Col: 256}, &engine.Cell{Value: engine.NumberValue(1)})
	if err := Write(&bytes.Buffer{}, &engine.Workbook{Sheets: []*engine.Sheet{s}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("256 columns: %v", err)
	}
	s = engine.NewSheet("a", "S", 0, 0)
	s.SetCell(engine.Ref{Row: 65536}, &engine.Cell{Value: engine.NumberValue(1)})
	if err := Write(&bytes.Buffer{}, &engine.Workbook{Sheets: []*engine.Sheet{s}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("65537 rows: %v", err)
	}
	// more custom colours than palette slots fall back to the nearest colour
	s = engine.NewSheet("a", "S", 0, 0)
	for i := 0; i < 80; i++ {
		c := "#" + strings.Repeat("0", 4) + []string{"1", "3", "5", "7", "9", "b", "d", "f"}[i%8] + []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "a"}[i/8]
		s.SetCell(engine.Ref{Row: i}, &engine.Cell{Value: engine.NumberValue(1), Style: &engine.Style{Bg: c}})
	}
	var buf bytes.Buffer
	if err := Write(&buf, &engine.Workbook{Sheets: []*engine.Sheet{s}}); err != nil {
		t.Fatal(err)
	}
	if _, err := readBytes(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestCFBLargeStreamWithDIFAT(t *testing.T) {
	// > 109 FAT sectors forces DIFAT sectors.
	stream := make([]byte, 8<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range stream {
		stream[i] = byte(rng.IntN(256))
	}
	var buf bytes.Buffer
	if err := writeCFB(&buf, "Workbook", stream); err != nil {
		t.Fatal(err)
	}
	f, err := parseCFB(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, name, err := f.stream(MaxFileSize, "Workbook")
	if err != nil || name != "Workbook" || !bytes.Equal(got, stream) {
		t.Fatalf("stream mismatch: %v %q %d", err, name, len(got))
	}
	if le32(buf.Bytes(), 72) == 0 {
		t.Errorf("expected DIFAT sectors")
	}
	// small streams are padded to the mini-stream cutoff
	buf.Reset()
	writeCFB(&buf, "Workbook", []byte("tiny"))
	f, err = parseCFB(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = f.stream(MaxFileSize, "Workbook")
	if err != nil || len(got) != 4096 || string(got[:4]) != "tiny" {
		t.Errorf("tiny stream: %v %d", err, len(got))
	}
}

// ---- hand-assembled fixtures ---------------------------------------------

func u16s(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// miniCFB builds a version-3 compound file whose Workbook stream lives in
// the mini stream (exercises the mini FAT path of the reader).
func miniCFB(name string, stream []byte) []byte {
	const ss = 512
	miniSecs := (len(stream) + 63) / 64
	container := make([]byte, miniSecs*64)
	copy(container, stream)
	contSecs := (len(container) + ss - 1) / ss
	out := make([]byte, ss*(1+3+contSecs))
	h := out[:ss]
	copy(h, cfbSignature)
	binary.LittleEndian.PutUint16(h[24:], 0x3E)
	binary.LittleEndian.PutUint16(h[26:], 3)
	binary.LittleEndian.PutUint16(h[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(h[30:], 9)
	binary.LittleEndian.PutUint16(h[32:], 6)
	binary.LittleEndian.PutUint32(h[44:], 1) // FAT sectors
	binary.LittleEndian.PutUint32(h[48:], 1) // directory
	binary.LittleEndian.PutUint32(h[56:], 4096)
	binary.LittleEndian.PutUint32(h[60:], 2) // mini FAT
	binary.LittleEndian.PutUint32(h[64:], 1)
	binary.LittleEndian.PutUint32(h[68:], cfbEndChain)
	for i := 0; i < 109; i++ {
		binary.LittleEndian.PutUint32(h[76+4*i:], cfbFreeSect)
	}
	binary.LittleEndian.PutUint32(h[76:], 0)
	sec := func(n int) []byte { return out[(n+1)*ss : (n+2)*ss] }
	fat := sec(0)
	for i := 0; i < 128; i++ {
		binary.LittleEndian.PutUint32(fat[4*i:], cfbFreeSect)
	}
	binary.LittleEndian.PutUint32(fat[0:], cfbFATSect)
	binary.LittleEndian.PutUint32(fat[4:], cfbEndChain)
	binary.LittleEndian.PutUint32(fat[8:], cfbEndChain)
	for i := 0; i < contSecs; i++ {
		next := uint32(3 + i + 1)
		if i == contSecs-1 {
			next = cfbEndChain
		}
		binary.LittleEndian.PutUint32(fat[4*(3+i):], next)
	}
	dir := sec(1)
	entry := func(i int, n string, typ byte, child, start uint32, size uint64) {
		e := dir[i*128 : (i+1)*128]
		copy(e, u16s(n))
		binary.LittleEndian.PutUint16(e[64:], uint16(2*(len(utf16.Encode([]rune(n)))+1)))
		e[66], e[67] = typ, 1
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[76:], child)
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint64(e[120:], size)
	}
	entry(0, "Root Entry", 5, 1, 3, uint64(len(container)))
	entry(1, name, 2, cfbNoStream, 0, uint64(len(stream)))
	for i := 2; i < 4; i++ {
		e := dir[i*128:]
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[76:], cfbNoStream)
	}
	mf := sec(2)
	for i := 0; i < 128; i++ {
		v := uint32(cfbFreeSect)
		if i < miniSecs-1 {
			v = uint32(i + 1)
		} else if i == miniSecs-1 {
			v = cfbEndChain
		}
		binary.LittleEndian.PutUint32(mf[4*i:], v)
	}
	copy(out[4*ss:], container)
	return out
}

func xfRaw(font, ifmt uint16, style bool, align byte, b1, b2 uint32, colors uint16) payload {
	tp := uint16(0x0001)
	if style {
		tp |= 0xFFF4
	}
	return payload{}.u16(font).u16(ifmt).u16(tp).u8(align).u8(0).u8(0).u8(0xFC).u32(b1).u32(b2).u16(colors)
}

func formulaRec(r, c, xf uint16, cached []byte, rgce []byte) payload {
	return payload{}.u16(r).u16(c).u16(xf).append(cached).u16(0).u32(0).u16(uint16(len(rgce))).append(rgce)
}

func (p payload) append(b []byte) payload { return append(p, b...) }

func numBytes(f float64) []byte { return payload{}.f64(f) }

var strResult = []byte{0, 0, 0, 0, 0, 0, 0xFF, 0xFF}

const relRC = 0xC000 // row and column relative

func biff8Fixture(t testing.TB, extraGlobals ...[]byte) []byte {
	g := &recWriter{}
	g.rec(rBOF, bofPayload(0x0005))
	for _, x := range extraGlobals {
		g.buf = append(g.buf, x...)
	}
	g.rec(rCodePage, payload{}.u16(1200))
	g.rec(rDateMode, payload{}.u16(1))
	for i := 0; i < 4; i++ {
		g.rec(rFont, fontPayload(fontKey{color: icvAuto, height: 200}))
	}
	g.rec(rFont, fontPayload(fontKey{bold: true, italic: true, underline: true, color: 10, height: 280}))
	g.rec(rFormat, xlString(payload{}.u16(164), "0.000"))
	for i := 0; i < 15; i++ {
		g.rec(rXF, xfRaw(0, 0, true, 0x20, 0, 0, 0x20C0))
	}
	g.rec(rXF, xfRaw(0, 0, false, 0x20, 0, 0, 0x20C0)) // 15 default
	// 16: font 5 (index 4 in file), fmt 164, right/top/wrap, thin top, double bottom, solid fill colour 12
	g.rec(rXF, xfRaw(5, 164, false, 0x03|0x08|0x00<<4, 1<<8|6<<12, 1<<26, 12|65<<7))
	g.rec(rXF, xfRaw(0, 14, false, 0x20, 0, 0, 0x20C0)) // 17: date
	pal := payload{}.u16(56)
	for i, c := range defaultPalette {
		if i == 2 {
			c = 0x123456
		}
		pal = pal.u8(byte(c >> 16)).u8(byte(c >> 8)).u8(byte(c)).u8(0)
	}
	g.rec(rPalette, pal)
	sheetPos := g.pos() + 4
	g.rec(rBoundSheet, shortXLString(payload{}.u32(0).u8(0).u8(0), "Data"))
	g.rec(rBoundSheet, shortXLString(payload{}.u32(0).u8(0).u8(2), "Chart1"))
	g.rec(rSupBook, payload{}.u16(2).u16(0x0401))
	g.rec(rExternSheet, payload{}.u16(1).u16(0).u16(0).u16(0))
	sst := payload{}.u32(4).u32(3).u16(5).u8(0).append([]byte("hello")).u16(8).u8(1).append(u16s("ünïcode✓")).u16(6).u8(0).append([]byte("abc"))
	g.rec(rSST, sst)
	g.rec(rContinue, payload{}.u8(1).append(u16s("déf")))
	g.rec(rEOF, nil)
	binary.LittleEndian.PutUint32(g.buf[sheetPos:], uint32(len(g.buf)))

	w := g
	w.rec(rBOF, bofPayload(0x0010))
	w.rec(rColInfo, payload{}.u16(1).u16(2).u16(4389).u16(15).u16(0x0002).u16(0))
	w.rec(rColInfo, payload{}.u16(5).u16(5).u16(9999).u16(15).u16(0).u16(0)) // not user-set: ignored
	w.rec(rRow, payload{}.u16(0).u16(0).u16(4).u16(600).u16(0).u16(0).u16(0x0140).u16(15))
	w.rec(rLabelSST, payload{}.u16(0).u16(0).u16(15).u32(0))
	w.rec(rLabelSST, payload{}.u16(0).u16(1).u16(16).u32(1))
	w.rec(rLabelSST, payload{}.u16(0).u16(2).u16(15).u32(2))
	w.rec(rLabelSST, payload{}.u16(0).u16(5).u16(15).u32(99)) // bad index: ignored
	w.rec(rLabel, xlString(payload{}.u16(0).u16(3).u16(15), "lbl"))
	w.rec(rNumber, payload{}.u16(1).u16(0).u16(15).f64(1.5))
	w.rec(rRK, payload{}.u16(1).u16(1).u16(15).u32(42<<2|2))
	w.rec(rRK, payload{}.u16(1).u16(2).u16(15).u32(123<<2|3))
	w.rec(rRK, payload{}.u16(1).u16(3).u16(15).u32(uint32(math.Float64bits(2.5)>>32)))
	w.rec(rMulRK, payload{}.u16(2).u16(0).u16(15).u32(10<<2|2).u16(17).u32(100<<2|2).u16(1))
	w.rec(rBoolErr, payload{}.u16(3).u16(0).u16(15).u8(1).u8(0))
	w.rec(rBoolErr, payload{}.u16(3).u16(1).u16(15).u8(0x07).u8(1))
	w.rec(rBlank, payload{}.u16(3).u16(2).u16(16))
	w.rec(rMulBlank, payload{}.u16(3).u16(3).u16(16).u16(16).u16(4))
	// A5 = A2+1
	w.rec(rFormula, formulaRec(4, 0, 15, numBytes(2.5), payload{}.u8(0x44).u16(1).u16(0|relRC).u8(0x1E).u16(1).u8(0x03)))
	// B5 = "x"&D1 (string result + STRING record)
	w.rec(rFormula, formulaRec(4, 1, 15, strResult, payload{}.u8(0x17).u8(1).u8(0).append([]byte("x")).u8(0x44).u16(0).u16(3|relRC).u8(0x08)))
	w.rec(rString, xlString(payload{}, "xlbl"))
	// C5 = SUM(A2:B2) via ptgFuncVar
	w.rec(rFormula, formulaRec(4, 2, 15, numBytes(43.5), payload{}.u8(0x25).u16(1).u16(1).u16(0|relRC).u16(1|relRC).u8(0x42).u8(1).u16(4)))
	// D5 = ROUND(A2,2) via ptgFunc
	w.rec(rFormula, formulaRec(4, 3, 15, numBytes(1.5), payload{}.u8(0x44).u16(1).u16(0|relRC).u8(0x1E).u16(2).u8(0x41).u16(27)))
	// E5 = SUM(A2:C2) via tAttrSum, plus a space attribute
	w.rec(rFormula, formulaRec(4, 4, 15, numBytes(44.73), payload{}.u8(0x19).u8(0x40).u16(0x0100).u8(0x25).u16(1).u16(1).u16(0|relRC).u16(2|relRC).u8(0x19).u8(0x10).u16(0)))
	// A6/B6 shared formula =A5*10 / =B5*10 (ptgRefN: row -1, col +0)
	w.rec(rFormula, formulaRec(5, 0, 15, numBytes(25), payload{}.u8(0x01).u16(5).u16(0)))
	w.rec(rShrFmla, payload{}.u16(5).u16(5).u8(0).u8(1).u8(0).u8(2).u16(9).u8(0x4C).u16(0xFFFF).u16(0|relRC).u8(0x1E).u16(10).u8(0x05))
	w.rec(rFormula, formulaRec(5, 1, 15, []byte{2, 0, 0x0F, 0, 0, 0, 0xFF, 0xFF}, payload{}.u8(0x01).u16(5).u16(0)))
	// C6 = Data!$A$1 (ptgRef3d through EXTERNSHEET/SUPBOOK)
	w.rec(rFormula, formulaRec(5, 2, 15, strResult, payload{}.u8(0x5A).u16(0).u16(0).u16(0)))
	w.rec(rString, xlString(payload{}, "hello"))
	// D6 uses a defined name (unsupported): cached value kept
	w.rec(rFormula, formulaRec(5, 3, 15, numBytes(7), payload{}.u8(0x43).u16(1).u16(0)))
	// E6 calls an add-in function (iftab 255): cached value kept
	w.rec(rFormula, formulaRec(5, 4, 15, []byte{1, 0, 1, 0, 0, 0, 0xFF, 0xFF}, payload{}.u8(0x22).u8(1).u16(255)))
	// embedded chart substream is skipped
	w.rec(rBOF, bofPayload(0x0020))
	w.rec(rNumber, payload{}.u16(9).u16(0).u16(15).f64(99))
	w.rec(rEOF, nil)
	w.rec(rWindow2, payload{}.u16(0x06B6|0x0008).u16(0).u16(0).u16(0x40).u16(0).u16(0).u16(0).u16(0).u16(0))
	w.rec(rPane, payload{}.u16(1).u16(2).u16(2).u16(1).u8(0).u8(0))
	w.rec(rEOF, nil)
	return w.buf
}

func TestReadHandcraftedBIFF8(t *testing.T) {
	stream := biff8Fixture(t)
	if len(stream) >= 4096 {
		t.Fatalf("fixture should fit in the mini stream, got %d bytes", len(stream))
	}
	wb, err := readBytes(miniCFB("Workbook", stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.Sheets) != 1 || wb.Sheets[0].Name != "Data" {
		t.Fatalf("sheets: %d", len(wb.Sheets))
	}
	s := wb.Sheets[0]
	type want struct {
		v       any
		formula string
	}
	cases := map[string]want{
		"A1": {"hello", ""}, "B1": {"ünïcode✓", ""}, "C1": {"abcdéf", ""}, "D1": {"lbl", ""},
		"A2": {1.5, ""}, "B2": {42.0, ""}, "C2": {1.23, ""}, "D2": {2.5, ""}, "A3": {10.0, ""}, "B3": {100.0 + engine.Date1904Offset, ""},
		"A4": {true, ""}, "B4": {errv(engine.CodeDiv0), ""},
		"A5": {2.5, "=A2+1"}, "B5": {"xlbl", `="x"&D1`}, "C5": {43.5, "=SUM(A2:B2)"}, "D5": {1.5, "=ROUND(A2,2)"},
		"E5": {44.73, "=SUM(A2:C2)"}, "A6": {25.0, "=A5*10"}, "B6": {errv(engine.CodeValue), "=B5*10"},
		"C6": {"hello", "=Data!$A$1"}, "D6": {7.0, ""}, "E6": {true, ""},
	}
	for a1, w := range cases {
		c := s.Cell(mref(t, a1))
		if c == nil {
			t.Errorf("%s missing", a1)
			continue
		}
		switch x := w.v.(type) {
		case string:
			if c.Value.Kind != engine.KindString || c.Value.Str != x {
				t.Errorf("%s = %q (%v) want %q", a1, c.Value.Text(), c.Value.Kind, x)
			}
		case float64:
			if c.Value.Kind != engine.KindNumber || math.Abs(c.Value.Num-x) > 1e-9 {
				t.Errorf("%s = %v want %v", a1, c.Value, x)
			}
		case bool:
			if c.Value.Kind != engine.KindBool || c.Value.Bool != x {
				t.Errorf("%s = %v want %v", a1, c.Value, x)
			}
		case errv:
			if c.Value.Kind != engine.KindError || c.Value.Err != string(x) {
				t.Errorf("%s = %v want %v", a1, c.Value, x)
			}
		}
		if c.Formula != w.formula {
			t.Errorf("%s formula %q want %q", a1, c.Formula, w.formula)
		}
	}
	for _, a1 := range []string{"A10", "F1"} {
		if s.Cell(mref(t, a1)) != nil {
			t.Errorf("%s should not exist", a1)
		}
	}
	wantSt := &engine.Style{Bold: true, Italic: true, Underline: true, Color: "#123456", Size: 14, Fmt: "0.000", Align: "right",
		VAlign: "top", Wrap: true, Bg: "#0000ff", Border: &engine.Border{Top: true, Bottom: true}}
	if st := s.Cell(mref(t, "B1")).Style; !st.Equal(wantSt) {
		t.Errorf("B1 style %+v / %+v", st, st.Border)
	}
	for _, a1 := range []string{"C4", "D4", "E4"} {
		if c := s.Cell(mref(t, a1)); c == nil || !c.Style.Equal(wantSt) || !c.Value.IsEmpty() {
			t.Errorf("%s blank styled cell %+v", a1, c)
		}
	}
	if st := s.Cell(mref(t, "B3")).Style; st == nil || st.Fmt != "m/d/yyyy" {
		t.Errorf("B3 style %+v", st)
	}
	if s.ColWidths[1] != 120 || s.ColWidths[2] != 120 || len(s.ColWidths) != 2 {
		t.Errorf("col widths %v", s.ColWidths)
	}
	if s.RowHeights[0] != 40 {
		t.Errorf("row heights %v", s.RowHeights)
	}
	if s.FrozenRows != 2 || s.FrozenCols != 1 {
		t.Errorf("frozen %d/%d", s.FrozenRows, s.FrozenCols)
	}
	// the engine accepts the imported workbook and recalculates it
	e, err := engine.New(wb)
	if err != nil {
		t.Fatal(err)
	}
	e.RecalcAll()
	if v := s.Value(mref(t, "A6")); v.Num != 25 {
		t.Errorf("recalc A6 = %v", v)
	}
}

type errv string

func TestReadBIFF5(t *testing.T) {
	g := &recWriter{}
	g.rec(rBOF, payload{}.u16(0x0500).u16(0x0005).u16(0).u16(0))
	g.rec(rCodePage, payload{}.u16(1252))
	font5 := payload{}.u16(200).u16(0).u16(icvAuto).u16(400).u16(0).u8(0).u8(0).u8(0).u8(0).u8(5).append([]byte("Arial"))
	g.rec(rFont, font5)
	g.rec(rFormat, payload{}.u16(164).u8(3).append([]byte("0.0")))
	xf5 := func(ifmt uint16, b1 uint32) payload {
		return payload{}.u16(0).u16(ifmt).u16(0x0001).u8(0x20).u8(0).u32(b1).u32(0)
	}
	for i := 0; i < 16; i++ {
		g.rec(rXF, xf5(0, 0x20C0))
	}
	g.rec(rXF, xf5(164, 10|1<<16|1<<22)) // 16: fill red (index 10), thin bottom border
	pos := g.pos() + 4
	g.rec(rBoundSheet, payload{}.u32(0).u8(0).u8(0).u8(4).append([]byte("Alt\x80")))
	g.rec(rEOF, nil)
	binary.LittleEndian.PutUint32(g.buf[pos:], uint32(len(g.buf)))
	g.rec(rBOF, payload{}.u16(0x0500).u16(0x0010).u16(0).u16(0))
	g.rec(rLabel, payload{}.u16(0).u16(0).u16(15).u16(4).append([]byte("caf\xe9")))
	g.rec(rNumber, payload{}.u16(1).u16(0).u16(16).f64(3.25))
	g.rec(rFormula, payload{}.u16(2).u16(0).u16(15).f64(9).u16(0).u32(0).u16(3).u8(0x1E).u16(9))
	g.rec(rEOF, nil)
	var buf bytes.Buffer
	if err := writeCFB(&buf, "Book", g.buf); err != nil {
		t.Fatal(err)
	}
	wb, err := readBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	s := wb.Sheets[0]
	if s.Name != "Alt€" {
		t.Errorf("name %q", s.Name)
	}
	if v := s.Value(engine.Ref{}); v.Str != "café" {
		t.Errorf("label %q", v.Str)
	}
	c := s.Cell(engine.Ref{Row: 1})
	if c == nil || c.Value.Num != 3.25 || c.Style == nil || c.Style.Fmt != "0.0" || c.Style.Bg != "#ff0000" || c.Style.Border == nil || !c.Style.Border.Bottom {
		t.Errorf("styled number %+v %+v", c, c.Style)
	}
	if c := s.Cell(engine.Ref{Row: 2}); c == nil || c.Value.Num != 9 || c.Formula != "" {
		t.Errorf("BIFF5 formula keeps cached value only: %+v", c)
	}
}

func TestReadRejectsHostileInput(t *testing.T) {
	if _, err := readBytes([]byte("definitely not an xls file, but long enough................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................")); !errors.Is(err, ErrInvalid) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := readBytes(nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty: %v", err)
	}
	var good bytes.Buffer
	writeCFB(&good, "Workbook", biff8Fixture(t))
	// directory chain loop
	b := append([]byte(nil), good.Bytes()...)
	dirStart := le32(b, 48)
	fatSec := le32(b, 76)
	binary.LittleEndian.PutUint32(b[(int(fatSec)+1)*512+4*int(dirStart):], dirStart)
	if _, err := readBytes(b); !errors.Is(err, ErrInvalid) {
		t.Errorf("FAT loop: %v", err)
	}
	// FAT sector pointing outside the file
	b = append([]byte(nil), good.Bytes()...)
	binary.LittleEndian.PutUint32(b[76:], 999999)
	if _, err := readBytes(b); !errors.Is(err, ErrInvalid) {
		t.Errorf("FAT out of range: %v", err)
	}
	// declared stream size larger than the file
	b = append([]byte(nil), good.Bytes()...)
	dirOff := (int(dirStart) + 1) * 512
	binary.LittleEndian.PutUint32(b[dirOff+128+120:], 0x7FFFFFFF)
	if _, err := readBytes(b); !errors.Is(err, ErrTooLarge) {
		t.Errorf("huge stream: %v", err)
	}
	// encrypted workbook
	var enc bytes.Buffer
	writeCFB(&enc, "Workbook", biff8Fixture(t, (&recWriter{}).recBytes(rFilePass, payload{}.u16(0))))
	if _, err := readBytes(enc.Bytes()); !errors.Is(err, ErrEncrypted) {
		t.Errorf("encrypted: %v", err)
	}
	// unsupported BIFF version
	var old bytes.Buffer
	writeCFB(&old, "Workbook", (&recWriter{}).recBytes(rBOF, payload{}.u16(0x0400).u16(5)))
	if _, err := readBytes(old.Bytes()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BIFF4: %v", err)
	}
	// record overrunning the stream
	stream := biff8Fixture(t)
	for _, cut := range []int{3, 7, 40} {
		rd := &reader{stream: stream[:len(stream)-cut], formats: map[int]string{}, palette: defaultPalette}
		if _, err := rd.read(); !errors.Is(err, ErrInvalid) {
			t.Errorf("truncated by %d: %v", cut, err)
		}
	}
	// sheet offset out of range
	g := &recWriter{}
	g.rec(rBOF, bofPayload(5))
	g.rec(rBoundSheet, shortXLString(payload{}.u32(1<<30).u8(0).u8(0), "X"))
	g.rec(rEOF, nil)
	var bad bytes.Buffer
	writeCFB(&bad, "Workbook", g.buf)
	if _, err := readBytes(bad.Bytes()); !errors.Is(err, ErrInvalid) {
		t.Errorf("sheet offset: %v", err)
	}
	// SST claiming billions of strings with no data must not allocate or hang
	g = &recWriter{}
	g.rec(rBOF, bofPayload(5))
	g.rec(rSST, payload{}.u32(0xFFFFFFFF).u32(0xFFFFFFFF))
	g.rec(rEOF, nil)
	var huge bytes.Buffer
	writeCFB(&huge, "Workbook", g.buf)
	if _, err := readBytes(huge.Bytes()); !errors.Is(err, ErrInvalid) { // no worksheets
		t.Errorf("huge SST: %v", err)
	}
	// file size limit
	if _, err := Read(bytes.NewReader(nil), MaxFileSize+1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("size limit: %v", err)
	}
}

func (w *recWriter) recBytes(typ uint16, data []byte) []byte {
	w.rec(typ, data)
	return w.buf
}

func FuzzRead(f *testing.F) {
	var buf bytes.Buffer
	Write(&buf, roundTripWorkbook(f))
	f.Add(buf.Bytes())
	f.Add(miniCFB("Workbook", biff8Fixture(f)))
	f.Fuzz(func(t *testing.T, b []byte) {
		wb, err := readBytes(b)
		if err != nil {
			return
		}
		if _, err := engine.New(wb); err != nil {
			t.Fatalf("reader produced a workbook the engine rejects: %v", err)
		}
	})
}

// FuzzBIFF fuzzes the record stream directly (bypassing the container) so
// the fuzzer spends its effort on BIFF parsing.
func FuzzBIFF(f *testing.F) {
	f.Add(biff8Fixture(f))
	f.Fuzz(func(t *testing.T, stream []byte) {
		rd := &reader{stream: stream, formats: map[int]string{}, palette: defaultPalette}
		wb, err := rd.read()
		if err != nil {
			return
		}
		if _, err := engine.New(wb); err != nil {
			t.Fatalf("engine rejects parsed workbook: %v", err)
		}
	})
}
