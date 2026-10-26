package xlsx

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/sheets/engine"
)

const (
	nsMain = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkg  = "http://schemas.openxmlformats.org/package/2006/relationships"
	xmlHdr = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
)

// futureFuncs need the "_xlfn." prefix in OOXML (functions newer than Excel 2007).
var futureFuncs = map[string]bool{
	"CONCAT": true, "TEXTJOIN": true, "IFS": true, "SWITCH": true, "MAXIFS": true, "MINIFS": true, "XLOOKUP": true,
	"STDEV.S": true, "STDEV.P": true, "VAR.S": true, "VAR.P": true, "MODE.SNGL": true, "RANK.EQ": true, "DAYS": true,
	"IFNA": true, "XOR": true, "UNICHAR": true, "UNICODE": true,
}

func excelFuncName(name string) string {
	if futureFuncs[name] {
		return "_xlfn." + name
	}
	return name
}

// fixedTime keeps output byte-for-byte deterministic.
var fixedTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Write serialises the workbook as an .xlsx package. Formulas are written
// with their cached values and the workbook asks Excel to recalculate on
// open. #CIRC! (not an Excel error) is written as #VALUE!.
func Write(w io.Writer, wb *engine.Workbook) error {
	if wb == nil || len(wb.Sheets) == 0 {
		return errors.New("xlsx: workbook has no sheets")
	}
	zw := zip.NewWriter(w)
	sw := &styleWriter{fonts: map[fontKey]int{}, fills: map[string]int{}, borders: map[engine.Border]int{},
		numFmts: map[string]int{}, xfs: map[xfKey]int{}, byStyle: map[*engine.Style]int{}}
	sw.init()
	sst := &sstWriter{index: map[string]int{}}

	create := func(name string) (*bufio.Writer, func() error, error) {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: fixedTime})
		if err != nil {
			return nil, nil, err
		}
		bw := bufio.NewWriterSize(fw, 64<<10)
		return bw, bw.Flush, nil
	}
	writePart := func(name, content string) error {
		bw, flush, err := create(name)
		if err != nil {
			return err
		}
		if _, err := bw.WriteString(content); err != nil {
			return err
		}
		return flush()
	}

	// Worksheets first: they populate the shared strings and styles tables.
	for i, s := range wb.Sheets {
		bw, flush, err := create(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1))
		if err != nil {
			return err
		}
		if err := writeSheet(bw, s, i == 0, sw, sst); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
	}
	n := len(wb.Sheets)
	var ct strings.Builder
	ct.WriteString(xmlHdr + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	ct.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
		`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>` +
		`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>` +
		`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>` +
		`</Types>`)

	rootRels := xmlHdr + `<Relationships xmlns="` + nsPkg + `">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>` +
		`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>` +
		`</Relationships>`

	var wbx strings.Builder
	wbx.WriteString(xmlHdr + `<workbook xmlns="` + nsMain + `" xmlns:r="` + nsRel + `">` +
		`<workbookPr/><bookViews><workbookView activeTab="0"/></bookViews><sheets>`)
	for i, s := range wb.Sheets {
		fmt.Fprintf(&wbx, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, escapeText(s.Name, true), i+1, i+1)
	}
	wbx.WriteString(`</sheets><calcPr calcId="191029" fullCalcOnLoad="1"/></workbook>`)

	var wbRels strings.Builder
	wbRels.WriteString(xmlHdr + `<Relationships xmlns="` + nsPkg + `">`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, n+1)
	fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>`, n+2)
	wbRels.WriteString(`</Relationships>`)

	var titles strings.Builder
	for _, s := range wb.Sheets {
		fmt.Fprintf(&titles, "<vt:lpstr>%s</vt:lpstr>", escapeText(s.Name, false))
	}
	app := xmlHdr + `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">` +
		`<Application>Ludworks</Application>` +
		`<HeadingPairs><vt:vector size="2" baseType="variant"><vt:variant><vt:lpstr>Worksheets</vt:lpstr></vt:variant><vt:variant><vt:i4>` + strconv.Itoa(n) + `</vt:i4></vt:variant></vt:vector></HeadingPairs>` +
		`<TitlesOfParts><vt:vector size="` + strconv.Itoa(n) + `" baseType="lpstr">` + titles.String() + `</vt:vector></TitlesOfParts></Properties>`
	core := xmlHdr + `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		`<dc:creator>Ludworks</dc:creator></cp:coreProperties>`

	for _, part := range []struct{ name, body string }{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", rootRels},
		{"docProps/app.xml", app},
		{"docProps/core.xml", core},
		{"xl/workbook.xml", wbx.String()},
		{"xl/_rels/workbook.xml.rels", wbRels.String()},
		{"xl/styles.xml", sw.xml()},
	} {
		if err := writePart(part.name, part.body); err != nil {
			return err
		}
	}
	bw, flush, err := create("xl/sharedStrings.xml")
	if err != nil {
		return err
	}
	if err := sst.write(bw); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return zw.Close()
}

func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func excelErr(code string) string {
	if code == engine.CodeCirc {
		return engine.CodeValue
	}
	return code
}

func writeSheet(w *bufio.Writer, s *engine.Sheet, selected bool, sw *styleWriter, sst *sstWriter) error {
	w.WriteString(xmlHdr + `<worksheet xmlns="` + nsMain + `" xmlns:r="` + nsRel + `">`)
	if used, ok := s.UsedRange(); ok {
		fmt.Fprintf(w, `<dimension ref="%s"/>`, engine.NewRange(used.From, used.To).String())
	} else {
		w.WriteString(`<dimension ref="A1"/>`)
	}
	w.WriteString(`<sheetViews><sheetView workbookViewId="0"`)
	if selected {
		w.WriteString(` tabSelected="1"`)
	}
	w.WriteString(`>`)
	if s.FrozenRows > 0 || s.FrozenCols > 0 {
		pane := "bottomRight"
		switch {
		case s.FrozenCols == 0:
			pane = "bottomLeft"
		case s.FrozenRows == 0:
			pane = "topRight"
		}
		w.WriteString(`<pane`)
		if s.FrozenCols > 0 {
			fmt.Fprintf(w, ` xSplit="%d"`, s.FrozenCols)
		}
		if s.FrozenRows > 0 {
			fmt.Fprintf(w, ` ySplit="%d"`, s.FrozenRows)
		}
		fmt.Fprintf(w, ` topLeftCell="%s" activePane="%s" state="frozen"/><selection pane="%s"/>`,
			engine.Ref{Row: s.FrozenRows, Col: s.FrozenCols}.String(), pane, pane)
	}
	w.WriteString(`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/>`)
	if len(s.ColWidths) > 0 {
		cols := make([]int, 0, len(s.ColWidths))
		for c := range s.ColWidths {
			if c >= 0 && c < engine.MaxCols {
				cols = append(cols, c)
			}
		}
		sort.Ints(cols)
		w.WriteString(`<cols>`)
		for _, c := range cols {
			fmt.Fprintf(w, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, c+1, c+1, formatNum(pxToWidth(s.ColWidths[c])))
		}
		w.WriteString(`</cols>`)
	}
	w.WriteString(`<sheetData>`)
	refs := s.SortedRefs()
	// rows that only carry a custom height
	heightRows := make([]int, 0, len(s.RowHeights))
	for r := range s.RowHeights {
		if r >= 0 && r < engine.MaxRows {
			heightRows = append(heightRows, r)
		}
	}
	sort.Ints(heightRows)
	hi := 0
	openRow := func(r int) {
		fmt.Fprintf(w, `<row r="%d"`, r+1)
		if h, ok := s.RowHeights[r]; ok && h > 0 {
			fmt.Fprintf(w, ` ht="%s" customHeight="1"`, formatNum(math.Min(pxToPt(h), 409)))
		}
		w.WriteString(`>`)
	}
	cur := -1
	for _, ref := range refs {
		for hi < len(heightRows) && heightRows[hi] < ref.Row {
			if heightRows[hi] > cur {
				if cur >= 0 {
					w.WriteString(`</row>`)
				}
				openRow(heightRows[hi])
				cur = heightRows[hi]
			}
			hi++
		}
		if ref.Row != cur {
			if cur >= 0 {
				w.WriteString(`</row>`)
			}
			openRow(ref.Row)
			cur = ref.Row
		}
		writeCell(w, ref, s.Cells[ref], sw, sst)
	}
	for ; hi < len(heightRows); hi++ {
		if heightRows[hi] > cur {
			if cur >= 0 {
				w.WriteString(`</row>`)
			}
			openRow(heightRows[hi])
			cur = heightRows[hi]
		}
	}
	if cur >= 0 {
		w.WriteString(`</row>`)
	}
	_, err := w.WriteString(`</sheetData></worksheet>`)
	return err
}

func writeCell(w *bufio.Writer, ref engine.Ref, c *engine.Cell, sw *styleWriter, sst *sstWriter) {
	fmt.Fprintf(w, `<c r="%s"`, ref.String())
	if xf := sw.xfFor(c.Style); xf > 0 {
		fmt.Fprintf(w, ` s="%d"`, xf)
	}
	formula := ""
	if c.Formula != "" {
		if f, err := engine.RewriteFormula(c.Formula, engine.FormatOptions{NoEquals: true, FuncName: excelFuncName}); err == nil {
			formula = f
		}
	}
	v := c.Value
	if formula != "" {
		switch v.Kind {
		case engine.KindString:
			w.WriteString(` t="str"`)
		case engine.KindBool:
			w.WriteString(` t="b"`)
		case engine.KindError:
			w.WriteString(` t="e"`)
		}
		w.WriteString(`><f>` + escapeText(formula, false) + `</f>`)
		switch v.Kind {
		case engine.KindNumber:
			w.WriteString(`<v>` + formatNum(v.Num) + `</v>`)
		case engine.KindString:
			w.WriteString(`<v>` + escapeText(v.Str, false) + `</v>`)
		case engine.KindBool:
			w.WriteString(`<v>` + boolStr(v.Bool) + `</v>`)
		case engine.KindError:
			w.WriteString(`<v>` + excelErr(v.Err) + `</v>`)
		}
		w.WriteString(`</c>`)
		return
	}
	switch v.Kind {
	case engine.KindNumber:
		w.WriteString(`><v>` + formatNum(v.Num) + `</v></c>`)
	case engine.KindString:
		fmt.Fprintf(w, ` t="s"><v>%d</v></c>`, sst.add(v.Str))
	case engine.KindBool:
		w.WriteString(` t="b"><v>` + boolStr(v.Bool) + `</v></c>`)
	case engine.KindError:
		w.WriteString(` t="e"><v>` + excelErr(v.Err) + `</v></c>`)
	default:
		w.WriteString(`/>`)
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

type sstWriter struct {
	index map[string]int
	list  []string
	count int
}

func (s *sstWriter) add(str string) int {
	s.count++
	if i, ok := s.index[str]; ok {
		return i
	}
	i := len(s.list)
	s.index[str] = i
	s.list = append(s.list, str)
	return i
}

func (s *sstWriter) write(w *bufio.Writer) error {
	fmt.Fprintf(w, xmlHdr+`<sst xmlns="%s" count="%d" uniqueCount="%d">`, nsMain, s.count, len(s.list))
	for _, str := range s.list {
		if str != strings.TrimSpace(str) || strings.ContainsAny(str, "\n\t") {
			w.WriteString(`<si><t xml:space="preserve">` + escapeText(str, false) + `</t></si>`)
		} else {
			w.WriteString(`<si><t>` + escapeText(str, false) + `</t></si>`)
		}
	}
	_, err := w.WriteString(`</sst>`)
	return err
}

// ---- styles ---------------------------------------------------------------

type fontKey struct {
	b, i, u, strike bool
	color           string
	size            int
}

type xfKey struct {
	numFmt, font, fill, border int
	align, valign              string
	wrap                       bool
}

type styleWriter struct {
	fontList   []fontKey
	fonts      map[fontKey]int
	fillList   []string
	fills      map[string]int
	borderList []engine.Border
	borders    map[engine.Border]int
	numFmts    map[string]int
	numFmtList []string
	xfList     []xfKey
	xfs        map[xfKey]int
	byStyle    map[*engine.Style]int
}

func (sw *styleWriter) init() {
	sw.fontList = []fontKey{{}}
	sw.fonts[fontKey{}] = 0
	sw.fillList = []string{"none", "gray125"}
	sw.borderList = []engine.Border{{}}
	sw.borders[engine.Border{}] = 0
	sw.xfList = []xfKey{{}}
	sw.xfs[xfKey{}] = 0
}

func (sw *styleWriter) xfFor(st *engine.Style) int {
	if st.IsZero() {
		return 0
	}
	if id, ok := sw.byStyle[st]; ok {
		return id
	}
	fk := fontKey{b: st.Bold, i: st.Italic, u: st.Underline, strike: st.Strike, color: st.Color, size: st.Size}
	font, ok := sw.fonts[fk]
	if !ok {
		font = len(sw.fontList)
		sw.fonts[fk] = font
		sw.fontList = append(sw.fontList, fk)
	}
	fill := 0
	if st.Bg != "" {
		var ok bool
		if fill, ok = sw.fills[st.Bg]; !ok {
			fill = len(sw.fillList)
			sw.fills[st.Bg] = fill
			sw.fillList = append(sw.fillList, st.Bg)
		}
	}
	border := 0
	if st.Border != nil {
		var ok bool
		if border, ok = sw.borders[*st.Border]; !ok {
			border = len(sw.borderList)
			sw.borders[*st.Border] = border
			sw.borderList = append(sw.borderList, *st.Border)
		}
	}
	numFmt := 0
	if st.Fmt != "" {
		if id, ok := engine.BuiltinNumFmtID(st.Fmt); ok {
			numFmt = id
		} else if id, ok := sw.numFmts[st.Fmt]; ok {
			numFmt = id
		} else {
			numFmt = 164 + len(sw.numFmtList)
			sw.numFmts[st.Fmt] = numFmt
			sw.numFmtList = append(sw.numFmtList, st.Fmt)
		}
	}
	k := xfKey{numFmt: numFmt, font: font, fill: fill, border: border, align: st.Align, valign: st.VAlign, wrap: st.Wrap}
	id, ok := sw.xfs[k]
	if !ok {
		id = len(sw.xfList)
		sw.xfs[k] = id
		sw.xfList = append(sw.xfList, k)
	}
	sw.byStyle[st] = id
	return id
}

func argb(c string) string { return "FF" + strings.ToUpper(strings.TrimPrefix(c, "#")) }

func (sw *styleWriter) xml() string {
	var b strings.Builder
	b.WriteString(xmlHdr + `<styleSheet xmlns="` + nsMain + `">`)
	if len(sw.numFmtList) > 0 {
		fmt.Fprintf(&b, `<numFmts count="%d">`, len(sw.numFmtList))
		for i, code := range sw.numFmtList {
			fmt.Fprintf(&b, `<numFmt numFmtId="%d" formatCode="%s"/>`, 164+i, escapeText(code, true))
		}
		b.WriteString(`</numFmts>`)
	}
	fmt.Fprintf(&b, `<fonts count="%d">`, len(sw.fontList))
	for _, f := range sw.fontList {
		b.WriteString(`<font>`)
		if f.b {
			b.WriteString(`<b/>`)
		}
		if f.i {
			b.WriteString(`<i/>`)
		}
		if f.strike {
			b.WriteString(`<strike/>`)
		}
		if f.u {
			b.WriteString(`<u/>`)
		}
		size := f.size
		if size == 0 {
			size = 11
		}
		fmt.Fprintf(&b, `<sz val="%d"/>`, size)
		if f.color != "" {
			fmt.Fprintf(&b, `<color rgb="%s"/>`, argb(f.color))
		} else {
			b.WriteString(`<color theme="1"/>`)
		}
		b.WriteString(`<name val="Calibri"/><family val="2"/><scheme val="minor"/></font>`)
	}
	b.WriteString(`</fonts>`)
	fmt.Fprintf(&b, `<fills count="%d">`, len(sw.fillList))
	for i, f := range sw.fillList {
		switch i {
		case 0:
			b.WriteString(`<fill><patternFill patternType="none"/></fill>`)
		case 1:
			b.WriteString(`<fill><patternFill patternType="gray125"/></fill>`)
		default:
			fmt.Fprintf(&b, `<fill><patternFill patternType="solid"><fgColor rgb="%s"/><bgColor indexed="64"/></patternFill></fill>`, argb(f))
		}
	}
	b.WriteString(`</fills>`)
	fmt.Fprintf(&b, `<borders count="%d">`, len(sw.borderList))
	side := func(tag string, on bool) {
		if !on {
			b.WriteString(`<` + tag + `/>`)
			return
		}
		fmt.Fprintf(&b, `<%s style="thin"><color indexed="64"/></%s>`, tag, tag)
	}
	for _, bd := range sw.borderList {
		b.WriteString(`<border>`)
		side("left", bd.Left)
		side("right", bd.Right)
		side("top", bd.Top)
		side("bottom", bd.Bottom)
		b.WriteString(`<diagonal/></border>`)
	}
	b.WriteString(`</borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`)
	fmt.Fprintf(&b, `<cellXfs count="%d">`, len(sw.xfList))
	for _, x := range sw.xfList {
		fmt.Fprintf(&b, `<xf numFmtId="%d" fontId="%d" fillId="%d" borderId="%d" xfId="0"`, x.numFmt, x.font, x.fill, x.border)
		if x.numFmt != 0 {
			b.WriteString(` applyNumberFormat="1"`)
		}
		if x.font != 0 {
			b.WriteString(` applyFont="1"`)
		}
		if x.fill != 0 {
			b.WriteString(` applyFill="1"`)
		}
		if x.border != 0 {
			b.WriteString(` applyBorder="1"`)
		}
		if x.align == "" && x.valign == "" && !x.wrap {
			b.WriteString(`/>`)
			continue
		}
		b.WriteString(` applyAlignment="1"><alignment`)
		if x.align != "" {
			fmt.Fprintf(&b, ` horizontal="%s"`, x.align)
		}
		switch x.valign {
		case "middle":
			b.WriteString(` vertical="center"`)
		case "top", "bottom":
			fmt.Fprintf(&b, ` vertical="%s"`, x.valign)
		}
		if x.wrap {
			b.WriteString(` wrapText="1"`)
		}
		b.WriteString(`/></xf>`)
	}
	b.WriteString(`</cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
		`<dxfs count="0"/><tableStyles count="0" defaultTableStyle="TableStyleMedium2" defaultPivotStyle="PivotStyleLight16"/></styleSheet>`)
	return b.String()
}
