package xlsx

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"ludworks/internal/sheets/engine"
)

func ref(t testing.TB, a1 string) engine.Ref {
	t.Helper()
	r, err := engine.ParseRef(a1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func buildZip(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readBytes(t testing.TB, b []byte) (*engine.Workbook, error) {
	t.Helper()
	return Read(bytes.NewReader(b), int64(len(b)))
}

func sampleWorkbook(t testing.TB) *engine.Workbook {
	s1 := engine.NewSheet("a", "Data & 'Stuff' x", 0, 0)
	s2 := engine.NewSheet("b", "Second", 0, 0)
	put := func(s *engine.Sheet, a1 string, c engine.Cell) { s.SetCell(ref(t, a1), &c) }
	put(s1, "A1", engine.Cell{Value: engine.NumberValue(1)})
	put(s1, "A2", engine.Cell{Value: engine.NumberValue(-0.5)})
	put(s1, "A3", engine.Cell{Value: engine.NumberValue(1e300)})
	put(s1, "A4", engine.Cell{Value: engine.NumberValue(0.1 + 0.2)})
	put(s1, "B1", engine.Cell{Value: engine.StringValue("  padded  ")})
	put(s1, "B2", engine.Cell{Value: engine.StringValue("line1\nline2\tTab")})
	put(s1, "B3", engine.Cell{Value: engine.StringValue("ctl\x01char _x0041_ literal <&> \"q\"")})
	put(s1, "B4", engine.Cell{Value: engine.StringValue("日本語 ☃ 🎉")})
	put(s1, "B5", engine.Cell{Value: engine.StringValue("  padded  ")}) // shared string reuse
	put(s1, "C1", engine.Cell{Value: engine.BoolValue(true)})
	put(s1, "C2", engine.Cell{Value: engine.BoolValue(false)})
	put(s1, "C3", engine.Cell{Value: engine.ErrorValue(engine.CodeNA)})
	put(s1, "C4", engine.Cell{Value: engine.ErrorValue(engine.CodeDiv0)})
	put(s1, "D1", engine.Cell{Formula: "=SUM(A1:A2)", Value: engine.NumberValue(0.5)})
	put(s1, "D2", engine.Cell{Formula: `=CONCAT("a",B4)`, Value: engine.StringValue("a日本語 ☃ 🎉")})
	put(s1, "D3", engine.Cell{Formula: "=A1>0", Value: engine.BoolValue(true)})
	put(s1, "D4", engine.Cell{Formula: "=1/0", Value: engine.ErrorValue(engine.CodeDiv0)})
	put(s1, "D5", engine.Cell{Formula: "=Second!A1*2", Value: engine.NumberValue(84)})
	put(s1, "D6", engine.Cell{Formula: `=IFERROR(XLOOKUP(1,A1:A4,B1:B4),"none")`, Value: engine.StringValue("  padded  ")})
	full := &engine.Style{Bold: true, Italic: true, Underline: true, Strike: true, Color: "#ff0000", Bg: "#00ff00",
		Align: "center", VAlign: "middle", Wrap: true, Fmt: "yyyy-mm-dd", Size: 14,
		Border: &engine.Border{Top: true, Bottom: true, Left: true, Right: true}}
	put(s1, "E1", engine.Cell{Value: engine.NumberValue(45306), Style: full})
	put(s1, "E2", engine.Cell{Value: engine.NumberValue(0.25), Style: &engine.Style{Fmt: "0.00%"}})
	put(s1, "E3", engine.Cell{Value: engine.NumberValue(1234.5), Style: &engine.Style{Fmt: "$#,##0.00", Align: "right", VAlign: "bottom"}})
	put(s1, "E4", engine.Cell{Style: &engine.Style{Bg: "#123456", Border: &engine.Border{Bottom: true}}})
	put(s1, "E5", engine.Cell{Value: engine.NumberValue(7), Style: &engine.Style{Bold: true}})
	put(s1, "Z200", engine.Cell{Value: engine.StringValue("far")})
	s1.ColWidths[0] = 120
	s1.ColWidths[4] = 63
	s1.RowHeights[0] = 24
	s1.RowHeights[50] = 40 // row with height but no cells
	s1.FrozenRows, s1.FrozenCols = 1, 2
	put(s2, "A1", engine.Cell{Value: engine.NumberValue(42)})
	s2.FrozenRows = 3
	return &engine.Workbook{Sheets: []*engine.Sheet{s1, s2}}
}

func TestRoundTrip(t *testing.T) {
	wb := sampleWorkbook(t)
	var buf bytes.Buffer
	if err := Write(&buf, wb); err != nil {
		t.Fatal(err)
	}
	// the package must mention future functions with their _xlfn. prefix
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	for _, f := range zr.File {
		want := map[string][]string{
			"xl/worksheets/sheet1.xml": {"_xlfn.CONCAT(", "_xlfn.XLOOKUP(", `<pane xSplit="2" ySplit="1" topLeftCell="C2" activePane="bottomRight" state="frozen"/>`},
			"xl/sharedStrings.xml":     {`_x0001_`, `_x005F_x0041_`, `xml:space="preserve"`},
		}[f.Name]
		if len(want) > 0 {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			for _, want := range want {
				if !bytes.Contains(b, []byte(want)) {
					t.Errorf("sheet1.xml missing %q", want)
				}
			}
		}
	}
	back, err := readBytes(t, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Sheets) != 2 || back.Sheets[0].Name != "Data & 'Stuff' x" || back.Sheets[1].Name != "Second" {
		t.Fatalf("sheets: %+v", back.Sheets)
	}
	for i, s := range wb.Sheets {
		got := back.Sheets[i]
		if len(got.Cells) != len(s.Cells) {
			t.Errorf("sheet %d: %d cells, want %d", i, len(got.Cells), len(s.Cells))
		}
		for r, c := range s.Cells {
			gc := got.Cell(r)
			if gc == nil {
				t.Errorf("%s!%s missing", s.Name, r)
				continue
			}
			if !gc.Value.Equal(c.Value) || gc.Formula != c.Formula || !gc.Style.Equal(c.Style) {
				t.Errorf("%s!%s = %+v (style %+v), want %+v (style %+v)", s.Name, r, *gc, gc.Style, *c, c.Style)
			}
		}
		if got.FrozenRows != s.FrozenRows || got.FrozenCols != s.FrozenCols {
			t.Errorf("frozen %d/%d", got.FrozenRows, got.FrozenCols)
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
	// the result is usable by the engine and recalculates consistently
	e, err := engine.New(back)
	if err != nil {
		t.Fatal(err)
	}
	e.RecalcAll()
	if v := back.Sheets[0].Value(ref(t, "D5")); v.Num != 84 {
		t.Errorf("cross-sheet formula after recalc: %v", v)
	}
	// deterministic output
	var buf2 bytes.Buffer
	Write(&buf2, wb)
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Errorf("output is not deterministic")
	}
}

func TestCircWrittenAsValue(t *testing.T) {
	s := engine.NewSheet("a", "S", 0, 0)
	s.SetCell(engine.Ref{}, &engine.Cell{Formula: "=A1", Value: engine.ErrorValue(engine.CodeCirc)})
	var buf bytes.Buffer
	if err := Write(&buf, &engine.Workbook{Sheets: []*engine.Sheet{s}}); err != nil {
		t.Fatal(err)
	}
	back, err := readBytes(t, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if v := back.Sheets[0].Value(engine.Ref{}); v.Err != engine.CodeValue {
		t.Errorf("CIRC cached value = %v", v)
	}
}

const ctypes = `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`
const rootRels = `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`

func handcrafted(date1904 string) map[string]string {
	return map[string]string{
		"[Content_Types].xml": ctypes,
		"_rels/.rels":         rootRels,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?>
<x:workbook xmlns:x="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:rel="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<x:workbookPr date1904="` + date1904 + `"/>
<x:sheets><x:sheet name="Main" sheetId="1" rel:id="rId1"/><x:sheet name="Chart" sheetId="3" rel:id="rId9"/><x:sheet name="Other" sheetId="2" rel:id="rId2"/></x:sheets>
<x:definedNames><x:definedName name="Rate">Main!$A$1</x:definedName></x:definedNames></x:workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="/xl/worksheets/sheet2.xml"/>
<Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chartsheet" Target="chartsheets/sheet1.xml"/>
<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>
<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
<Relationship Id="rId5" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="theme/theme1.xml"/>
<Relationship Id="rId6" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="http://example.com" TargetMode="External"/>
</Relationships>`,
		"xl/chartsheets/sheet1.xml": `<chartsheet/>`,
		"xl/theme/theme1.xml": `<?xml version="1.0"?><a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:themeElements><a:clrScheme name="T">
<a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1>
<a:dk2><a:srgbClr val="1F497D"/></a:dk2><a:lt2><a:srgbClr val="EEECE1"/></a:lt2><a:accent1><a:srgbClr val="4F81BD"/></a:accent1>
<a:accent2><a:srgbClr val="C0504D"/></a:accent2></a:clrScheme></a:themeElements></a:theme>`,
		"xl/sharedStrings.xml": `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="4" uniqueCount="4">
<si><t>plain</t></si>
<si><r><rPr><b/></rPr><t>Rich </t></r><r><t xml:space="preserve">text </t></r><rPh sb="0" eb="1"><t>PHONETIC</t></rPh></si>
<si><t>CR_x000D_LF</t></si>
<si><t/></si></sst>`,
		"xl/styles.xml": `<?xml version="1.0"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<numFmts count="2"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd"/><numFmt numFmtId="165" formatCode="&quot;$&quot;#,##0.00"/></numFmts>
<fonts count="5"><font><sz val="11"/><color theme="1"/><name val="Calibri"/></font>
<font><b/><sz val="11"/><color theme="1"/></font>
<font><b val="0"/><i/><u val="double"/><sz val="16"/><color theme="4" tint="0.39997558519241921"/></font>
<font><strike/><u val="none"/><color indexed="10"/></font>
<font><color rgb="FF0070C0"/></font></fonts>
<fills count="4"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFFFFF00"/><bgColor indexed="64"/></patternFill></fill>
<fill><patternFill patternType="solid"><fgColor theme="5"/></patternFill></fill></fills>
<borders count="2"><border><left/><right/><top/><bottom/><diagonal/></border>
<border><left style="hair"/><right style="mediumDashed"/><top style="thick"/><bottom style="double"/><diagonal/></border></borders>
<cellXfs count="7"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>
<xf numFmtId="164" fontId="1" fillId="2" borderId="1" applyNumberFormat="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf>
<xf numFmtId="14" fontId="2" fillId="3" borderId="0"/>
<xf numFmtId="165" fontId="3" fillId="0" borderId="0"><alignment horizontal="right" vertical="top"/></xf>
<xf numFmtId="10" fontId="4" fillId="0" borderId="0"/>
<xf numFmtId="22" fontId="0" fillId="0" borderId="0"/>
<xf numFmtId="49" fontId="0" fillId="0" borderId="0"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<sheetViews><sheetView workbookViewId="0"><pane xSplit="1" ySplit="2" topLeftCell="B3" activePane="bottomRight" state="frozen"/></sheetView></sheetViews>
<cols><col min="1" max="2" width="20.7109375" customWidth="1"/><col min="3" max="16384" width="9" customWidth="1"/><col min="4" max="4" width="30" /></cols>
<sheetData>
<row r="1" ht="30" customHeight="1">
 <c r="A1" t="s"><v>0</v></c><c r="B1" t="s" s="1"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c><c r="E1" t="s"><v>99</v></c>
</row>
<row r="2"><c r="A2"><v>10</v></c><c r="B2" s="2"><v>45306</v></c><c r="C2" t="b"><v>1</v></c><c r="D2" t="e"><v>#DIV/0!</v></c><c r="E2" t="e"><v>#SPILL!</v></c></row>
<row r="3"><c r="A3"><v>20</v></c><c r="B3"><f>SUM(A2:A3)</f><v>30</v></c><c r="C3" t="str"><f>_xlfn.CONCAT("x",A2)</f><v>x10</v></c>
 <c r="D3" t="inlineStr"><is><t>inline</t><rPh><t>skip</t></rPh></is></c><c r="E3" s="3"><v>1234.5</v></c></row>
<row r="4"><c r="A4"><f t="shared" ref="A4:A6" si="0">A2*2</f><v>20</v></c><c r="B4"><f>Rate*2</f><v>99</v></c><c r="C4"><f>Table1[[#This Row],[x]]</f><v>7</v></c>
 <c r="D4" s="4"><v>0.125</v></c><c r="E4"><f t="array" ref="E4:E5">A2:A3*2</f><v>20</v></c></row>
<row r="5"><c r="A5"><f t="shared" si="0"/><v>40</v></c><c r="E5"><v>40</v></c></row>
<row r="6"><c r="A6"><f t="shared" si="0"/><v>99</v></c><c r="B6" t="d"><v>2024-01-15T06:00:00</v></c><c r="C6" s="5"><v>45306.5</v></c></row>
<row><c><v>1</v></c><c><v>2</v></c><c r="E7" s="6" t="s"><v>0</v></c></row>
<row r="9"><c r="XFE9"><v>1</v></c><c r="A9" t="n"><v></v></c><c r="B9" t="s"><v>-1</v></c></row>
</sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1"><v>5</v></c></row></sheetData></worksheet>`,
	}
}

func TestReadHandcrafted(t *testing.T) {
	wb, err := readBytes(t, buildZip(t, handcrafted("0")))
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.Sheets) != 2 || wb.Sheets[0].Name != "Main" || wb.Sheets[1].Name != "Other" {
		t.Fatalf("sheets %v", len(wb.Sheets))
	}
	s := wb.Sheets[0]
	type want struct {
		v       any
		formula string
	}
	cases := map[string]want{
		"A1": {"plain", ""}, "B1": {"Rich text ", ""}, "C1": {"CR\rLF", ""}, "D1": {"", ""},
		"A2": {10.0, ""}, "B2": {45306.0, ""}, "C2": {true, ""}, "D2": {engine.CodeDiv0, ""}, "E2": {engine.CodeValue, ""},
		"A3": {20.0, ""}, "B3": {30.0, "=SUM(A2:A3)"}, "C3": {"x10", `=CONCAT("x",A2)`}, "D3": {"inline", ""}, "E3": {1234.5, ""},
		"A4": {20.0, "=A2*2"}, "A5": {40.0, "=A3*2"}, "A6": {99.0, "=A4*2"},
		"B4": {99.0, ""}, "C4": {7.0, ""}, // unsupported formulas fall back to cached values
		"D4": {0.125, ""}, "E4": {20.0, "=A2:A3*2"}, "E5": {40.0, ""},
		"B6": {45306.25, ""}, "C6": {45306.5, ""},
		"A7": {1.0, ""}, "B7": {2.0, ""}, "E7": {"plain", ""},
	}
	for a1, w := range cases {
		c := s.Cell(ref(t, a1))
		if c == nil {
			t.Errorf("%s missing", a1)
			continue
		}
		switch x := w.v.(type) {
		case string:
			if a1 == "D2" || a1 == "E2" {
				if c.Value.Kind != engine.KindError || c.Value.Err != x {
					t.Errorf("%s = %v want error %s", a1, c.Value, x)
				}
			} else if c.Value.Kind != engine.KindString || c.Value.Str != x {
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
		}
		if c.Formula != w.formula {
			t.Errorf("%s formula %q want %q", a1, c.Formula, w.formula)
		}
	}
	for _, a1 := range []string{"A9", "B9", "E1"} {
		if c := s.Cell(ref(t, a1)); c != nil {
			t.Errorf("%s should be empty, got %+v", a1, c)
		}
	}
	// styles
	st := s.Cell(ref(t, "B1")).Style
	wantSt := &engine.Style{Bold: true, Bg: "#ffff00", Align: "center", VAlign: "middle", Wrap: true, Fmt: `yyyy\-mm\-dd`,
		Border: &engine.Border{Left: true, Right: true, Top: true, Bottom: true}}
	if !st.Equal(wantSt) {
		t.Errorf("B1 style %+v %+v", st, st.Border)
	}
	st = s.Cell(ref(t, "B2")).Style
	if st == nil || st.Bold || !st.Italic || !st.Underline || st.Size != 16 || st.Fmt != "m/d/yyyy" || st.Color != "#95b3d7" || st.Bg != "#c0504d" {
		t.Errorf("B2 style %+v", st)
	}
	st = s.Cell(ref(t, "E3")).Style
	if st == nil || !st.Strike || st.Underline || st.Color != "#ff0000" || st.Fmt != `"$"#,##0.00` || st.Align != "right" || st.VAlign != "top" {
		t.Errorf("E3 style %+v", st)
	}
	if st := s.Cell(ref(t, "D4")).Style; st == nil || st.Fmt != "0.00%" || st.Color != "#0070c0" {
		t.Errorf("D4 style %+v", st)
	}
	if st := s.Cell(ref(t, "E7")).Style; st == nil || st.Fmt != "@" {
		t.Errorf("E7 style %+v", st)
	}
	if engine.FormatValue(s.Value(ref(t, "B2")), s.Cell(ref(t, "B2")).Style.Fmt) != "1/15/2024" {
		t.Errorf("date display")
	}
	if s.FrozenRows != 2 || s.FrozenCols != 1 {
		t.Errorf("frozen %d %d", s.FrozenRows, s.FrozenCols)
	}
	if s.ColWidths[0] != 145 || s.ColWidths[1] != 145 || len(s.ColWidths) != 2 {
		t.Errorf("col widths %v", s.ColWidths)
	}
	if s.RowHeights[0] != 40 {
		t.Errorf("row heights %v", s.RowHeights)
	}
	if wb.Sheets[1].Value(engine.Ref{}).Num != 5 {
		t.Errorf("second sheet")
	}
	if wb.Sheets[0].ID == "" || wb.Sheets[0].ID == wb.Sheets[1].ID {
		t.Errorf("sheet ids %q %q", wb.Sheets[0].ID, wb.Sheets[1].ID)
	}
}

func TestRead1904(t *testing.T) {
	wb, err := readBytes(t, buildZip(t, handcrafted("1")))
	if err != nil {
		t.Fatal(err)
	}
	s := wb.Sheets[0]
	if v := s.Value(ref(t, "B2")).Num; v != 45306+engine.Date1904Offset {
		t.Errorf("1904 date not shifted: %v", v)
	}
	if v := s.Value(ref(t, "A2")).Num; v != 10 {
		t.Errorf("non-date shifted: %v", v)
	}
}

func TestReadRejectsHostileInput(t *testing.T) {
	base := handcrafted("0")
	withExtra := func(name, body string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		m[name] = body
		return m
	}
	if _, err := readBytes(t, []byte("not a zip")); !errors.Is(err, ErrInvalid) {
		t.Errorf("not zip: %v", err)
	}
	for _, bad := range []string{"../evil.xml", "/abs.xml", "a/../../b.xml", "dir\\x.xml", "a/./b"} {
		if _, err := readBytes(t, buildZip(t, withExtra(bad, "x"))); !errors.Is(err, ErrInvalid) {
			t.Errorf("entry %q: %v", bad, err)
		}
	}
	// duplicate (case-insensitive) part names
	if _, err := readBytes(t, buildZip(t, withExtra("XL/Workbook.xml", "x"))); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate parts: %v", err)
	}
	// deep nesting
	deep := strings.Repeat("<a>", 100) + strings.Repeat("</a>", 100)
	m := withExtra("xl/worksheets/sheet2.xml", `<worksheet>`+deep+`</worksheet>`)
	if _, err := readBytes(t, buildZip(t, m)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("deep xml: %v", err)
	}
	m = withExtra("xl/styles.xml", `<styleSheet>`+deep+`</styleSheet>`)
	if _, err := readBytes(t, buildZip(t, m)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("deep styles: %v", err)
	}
	// entity expansion is refused
	bomb := `<?xml version="1.0"?><!DOCTYPE lol [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;">]><sst xmlns="x"><si><t>&lol2;</t></si></sst>`
	if _, err := readBytes(t, buildZip(t, withExtra("xl/sharedStrings.xml", bomb))); !errors.Is(err, ErrInvalid) {
		t.Errorf("entity bomb: %v", err)
	}
	// too many entries
	many := map[string]string{}
	for i := 0; i <= MaxEntries; i++ {
		many["f"+strconv.Itoa(i)] = ""
	}
	if _, err := readBytes(t, buildZip(t, many)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("entries: %v", err)
	}
	// declared uncompressed size above the limit
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateRaw(&zip.FileHeader{Name: "huge.bin", Method: zip.Store, CompressedSize64: 1, UncompressedSize64: MaxUncompressed + 1})
	w.Write([]byte{0})
	zw.Close()
	if _, err := readBytes(t, buf.Bytes()); !errors.Is(err, ErrTooLarge) {
		t.Errorf("declared size: %v", err)
	}
	// no workbook
	if _, err := readBytes(t, buildZip(t, map[string]string{"a.txt": "x"})); !errors.Is(err, ErrInvalid) {
		t.Errorf("no workbook: %v", err)
	}
	// relationship pointing outside the package is ignored (sheet skipped → no worksheets)
	m = withExtra("xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="../../../etc/passwd"/></Relationships>`)
	if _, err := readBytes(t, buildZip(t, m)); !errors.Is(err, ErrInvalid) {
		t.Errorf("escaping rel: %v", err)
	}
}

func TestReadDecompressionBudget(t *testing.T) {
	// A highly compressible sheet slightly above the budget is rejected while
	// streaming even though it is tiny on disk.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range handcrafted("0") {
		if name == "xl/worksheets/sheet2.xml" {
			continue
		}
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	w, _ := zw.Create("xl/worksheets/sheet2.xml")
	io.WriteString(w, `<worksheet><sheetData>`)
	pad := bytes.Repeat([]byte(" "), 1<<20)
	for i := 0; i < MaxUncompressed>>20+1; i++ {
		w.Write(pad)
	}
	io.WriteString(w, `</sheetData></worksheet>`)
	zw.Close()
	if buf.Len() > 2<<20 {
		t.Fatalf("test zip unexpectedly large: %d", buf.Len())
	}
	if _, err := readBytes(t, buf.Bytes()); !errors.Is(err, ErrTooLarge) {
		t.Errorf("budget: %v", err)
	}
}

func TestStreamingBudget(t *testing.T) {
	// Declared sizes can lie; the streaming budget is enforced independently.
	b := buildZip(t, map[string]string{"p.xml": strings.Repeat("x", 5000)})
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	p := &pkgReader{files: map[string]*zip.File{"p.xml": zr.File[0]}, budget: 4096}
	if _, err := p.readAll("p.xml", maxSmallPart); !errors.Is(err, ErrTooLarge) {
		t.Errorf("package budget: %v", err)
	}
	p.budget = MaxUncompressed
	if _, err := p.readAll("p.xml", 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("part limit: %v", err)
	}
	p.budget = MaxUncompressed
	if got, err := p.readAll("p.xml", maxSmallPart); err != nil || len(got) != 5000 {
		t.Errorf("within budget: %d %v", len(got), err)
	}
}

func TestSanitizeSheetNames(t *testing.T) {
	used := map[string]bool{}
	for in, want := range map[string]string{"a/b": "a_b"} {
		if got := sanitizeSheetName(in, 1, used); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	if got := sanitizeSheetName("A_B", 2, used); got != "A_B (2)" {
		t.Errorf("dup → %q", got)
	}
	if got := sanitizeSheetName("", 3, used); got != "Sheet3" {
		t.Errorf("empty → %q", got)
	}
	if got := sanitizeSheetName(strings.Repeat("x", 40), 4, used); len([]rune(got)) != 31 {
		t.Errorf("long → %q", got)
	}
}

func TestEscapes(t *testing.T) {
	for _, s := range []string{"plain", "a\x01b", "_x0041_", "x_x00", "tab\there", " nbsp", "emoji 🎉"} {
		esc := escapeText(s, false)
		if got := decodeST(esc); got != s {
			t.Errorf("escape/decode %q → %q → %q", s, esc, got)
		}
	}
	if applyTint("4F81BD", 0.39997558519241921) != "95B3D7" {
		t.Errorf("tint %s", applyTint("4F81BD", 0.39997558519241921))
	}
	if applyTint("FFFFFF", -0.5) != "808080" {
		t.Errorf("darken %s", applyTint("FFFFFF", -0.5))
	}
}

func FuzzRead(f *testing.F) {
	var buf bytes.Buffer
	Write(&buf, sampleWorkbook(f))
	f.Add(buf.Bytes())
	f.Add(buildZip(f, handcrafted("0")))
	f.Fuzz(func(t *testing.T, b []byte) {
		wb, err := readBytes(t, b)
		if err != nil {
			return
		}
		if _, err := engine.New(wb); err != nil {
			t.Fatalf("reader produced a workbook the engine rejects: %v", err)
		}
	})
}
