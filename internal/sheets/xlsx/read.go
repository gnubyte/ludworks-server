package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/sheets/engine"
)

const (
	relOfficeDoc = "/officeDocument"
	relWorksheet = "/worksheet"
	relShared    = "/sharedStrings"
	relStyles    = "/styles"
	relTheme     = "/theme"
)

type pkgReader struct {
	files  map[string]*zip.File // lower-cased part name → entry
	budget int64
}

// Read parses an .xlsx package. Formulas the engine cannot evaluate
// faithfully (defined names, unknown functions, structured references…)
// are dropped and their cached values kept. Sheet IDs are freshly generated.
func Read(r io.ReaderAt, size int64) (*engine.Workbook, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, invalidf("not a zip package: %v", err)
	}
	if len(zr.File) > MaxEntries {
		return nil, fmt.Errorf("%w: %d zip entries", ErrTooLarge, len(zr.File))
	}
	p := &pkgReader{files: map[string]*zip.File{}, budget: MaxUncompressed}
	var declared uint64
	for _, f := range zr.File {
		name, err := cleanPartName(f.Name)
		if err != nil {
			return nil, err
		}
		if f.FileInfo().IsDir() {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := p.files[key]; dup {
			return nil, invalidf("duplicate part %q", name)
		}
		p.files[key] = f
		declared += f.UncompressedSize64
	}
	if declared > MaxUncompressed {
		return nil, fmt.Errorf("%w: %d bytes uncompressed", ErrTooLarge, declared)
	}
	rd := &reader{p: p}
	return rd.read()
}

// cleanPartName validates a zip entry name: no absolute paths, no "..",
// no NUL/backslash tricks.
func cleanPartName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\x00\\") || strings.HasPrefix(name, "/") || len(name) > 1024 {
		return "", invalidf("bad zip entry name %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." {
			return "", invalidf("bad zip entry name %q", name)
		}
	}
	return name, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// open returns a reader over a part that draws from the package budget.
func (p *pkgReader) open(name string, limit int64) (io.ReadCloser, error) {
	f := p.files[strings.ToLower(name)]
	if f == nil {
		return nil, invalidf("missing part %q", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, invalidf("open %q: %v", name, err)
	}
	return limitedReadCloser{Reader: &budgetReader{r: rc, p: p, limit: limit}, Closer: rc}, nil
}

func (p *pkgReader) has(name string) bool { return p.files[strings.ToLower(name)] != nil }

type budgetReader struct {
	r     io.Reader
	p     *pkgReader
	limit int64
}

func (b *budgetReader) Read(buf []byte) (int, error) {
	n, err := b.r.Read(buf)
	b.p.budget -= int64(n)
	b.limit -= int64(n)
	if b.p.budget < 0 || b.limit < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

func (p *pkgReader) readAll(name string, limit int64) ([]byte, error) {
	rc, err := p.open(name, limit)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("%w: part %q", ErrTooLarge, name)
		}
		return nil, invalidf("read %q: %v", name, err)
	}
	return b, nil
}

// newDecoder returns a strict decoder; Go's decoder never expands external
// entities or DTDs, so XXE/billion-laughs are not possible.
func newDecoder(r io.Reader) *xml.Decoder {
	d := xml.NewDecoder(r)
	d.Strict = true
	return d
}

// unmarshalSmall decodes a small part after checking its nesting depth.
func (p *pkgReader) unmarshalSmall(name string, v any) error {
	b, err := p.readAll(name, maxSmallPart)
	if err != nil {
		return err
	}
	if err := checkDepth(b); err != nil {
		return fmt.Errorf("%w (%s)", err, name)
	}
	if err := newDecoder(bytes.NewReader(b)).Decode(v); err != nil {
		return invalidf("parse %q: %v", name, err)
	}
	return nil
}

func checkDepth(b []byte) error {
	d := newDecoder(bytes.NewReader(b))
	depth := 0
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalidf("xml: %v", err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
			if depth > MaxXMLDepth {
				return fmt.Errorf("%w: XML nesting deeper than %d", ErrTooLarge, MaxXMLDepth)
			}
		case xml.EndElement:
			depth--
		}
	}
}

type xRels struct {
	Rels []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
		Mode   string `xml:"TargetMode,attr"`
	} `xml:"Relationship"`
}

// rels returns relationship targets of a part, resolved to part names.
func (p *pkgReader) rels(part string) (map[string]string, map[string][]string, error) {
	dir, base := path.Split(part)
	relPath := dir + "_rels/" + base + ".rels"
	byID, byType := map[string]string{}, map[string][]string{}
	if !p.has(relPath) {
		return byID, byType, nil
	}
	var x xRels
	if err := p.unmarshalSmall(relPath, &x); err != nil {
		return nil, nil, err
	}
	for _, r := range x.Rels {
		if strings.EqualFold(r.Mode, "External") || r.Target == "" {
			continue
		}
		var target string
		if strings.HasPrefix(r.Target, "/") {
			target = path.Clean(r.Target[1:])
		} else {
			target = path.Clean(path.Join(dir, r.Target))
		}
		if target == "." || strings.HasPrefix(target, "../") || target == ".." || strings.HasPrefix(target, "/") {
			continue // escapes the package root
		}
		byID[r.ID] = target
		byType[relKind(r.Type)] = append(byType[relKind(r.Type)], target)
	}
	return byID, byType, nil
}

func relKind(t string) string {
	if i := strings.LastIndexByte(t, '/'); i >= 0 {
		return t[i:]
	}
	return t
}

type xWorkbook struct {
	Pr struct {
		Date1904 string `xml:"date1904,attr"`
	} `xml:"workbookPr"`
	Sheets []struct {
		Name  string     `xml:"name,attr"`
		Attrs []xml.Attr `xml:",any,attr"`
	} `xml:"sheets>sheet"`
}

type reader struct {
	p        *pkgReader
	sst      []string
	styles   []*engine.Style
	isDate   []bool
	date1904 bool
	cells    int
}

func xmlBool(s string) bool { return s == "1" || strings.EqualFold(s, "true") }

func (rd *reader) read() (*engine.Workbook, error) {
	_, rootTypes, err := rd.p.rels("")
	if err != nil {
		return nil, err
	}
	wbPart := "xl/workbook.xml"
	if t := rootTypes[relOfficeDoc]; len(t) > 0 {
		wbPart = t[0]
	}
	if !rd.p.has(wbPart) {
		return nil, invalidf("no workbook part")
	}
	relIDs, relTypes, err := rd.p.rels(wbPart)
	if err != nil {
		return nil, err
	}
	var wbx xWorkbook
	if err := rd.p.unmarshalSmall(wbPart, &wbx); err != nil {
		return nil, err
	}
	rd.date1904 = xmlBool(wbx.Pr.Date1904)
	if len(wbx.Sheets) > engine.MaxSheets {
		return nil, fmt.Errorf("%w: %d sheets", ErrTooLarge, len(wbx.Sheets))
	}
	theme := defaultTheme
	if t := relTypes[relTheme]; len(t) > 0 && rd.p.has(t[0]) {
		if th, err := rd.readTheme(t[0]); err == nil {
			theme = th
		}
	}
	if t := relTypes[relShared]; len(t) > 0 && rd.p.has(t[0]) {
		if err := rd.readSST(t[0]); err != nil {
			return nil, err
		}
	}
	if t := relTypes[relStyles]; len(t) > 0 && rd.p.has(t[0]) {
		if err := rd.readStyles(t[0], theme); err != nil {
			return nil, err
		}
	}
	wb := engine.NewWorkbook()
	used := map[string]bool{}
	for i, sh := range wbx.Sheets {
		rid := ""
		for _, a := range sh.Attrs {
			if a.Name.Local == "id" {
				rid = a.Value
			}
		}
		target, ok := relIDs[rid]
		if !ok || !rd.p.has(target) {
			continue // chartsheets, dialog sheets or dangling entries
		}
		isWorksheet := false
		for _, w := range relTypes[relWorksheet] {
			if w == target {
				isWorksheet = true
			}
		}
		if !isWorksheet {
			continue
		}
		name := sanitizeSheetName(sh.Name, i+1, used)
		s := engine.NewSheet(engine.NewSheetID(), name, 0, 0)
		if err := rd.readSheet(target, s); err != nil {
			return nil, err
		}
		wb.Sheets = append(wb.Sheets, s)
	}
	if len(wb.Sheets) == 0 {
		return nil, invalidf("no worksheets")
	}
	return wb, nil
}

// sanitizeSheetName makes an imported name valid and unique.
func sanitizeSheetName(name string, n int, used map[string]bool) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(":\\/?*[]", r) || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, "'")
	name = truncateRunes(name, engine.MaxSheetNameLen)
	if engine.ValidateSheetName(name) != nil {
		name = "Sheet" + strconv.Itoa(n)
	}
	base := name
	for i := 2; used[strings.ToLower(name)]; i++ {
		suffix := " (" + strconv.Itoa(i) + ")"
		name = truncateRunes(base, engine.MaxSheetNameLen-len(suffix)) + suffix
	}
	used[strings.ToLower(name)] = true
	return name
}

// readSST streams the shared string table.
func (rd *reader) readSST(part string) error {
	rc, err := rd.p.open(part, MaxUncompressed)
	if err != nil {
		return err
	}
	defer rc.Close()
	d := newDecoder(rc)
	var stack []string
	var cur strings.Builder
	inSI, skip := false, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return wrapXMLErr(part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if len(stack) > MaxXMLDepth {
				return fmt.Errorf("%w: XML nesting", ErrTooLarge)
			}
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "rPh", "phoneticPr":
				skip++
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			switch t.Name.Local {
			case "si":
				if len(rd.sst) >= MaxSharedStrings {
					return fmt.Errorf("%w: too many shared strings", ErrTooLarge)
				}
				rd.sst = append(rd.sst, truncateRunes(decodeST(cur.String()), engine.MaxCellChars))
				inSI = false
			case "rPh", "phoneticPr":
				skip--
			}
		case xml.CharData:
			if inSI && skip == 0 && len(stack) > 0 && stack[len(stack)-1] == "t" && cur.Len() < 4*engine.MaxCellChars {
				cur.Write(t)
			}
		}
	}
}

func wrapXMLErr(part string, err error) error {
	if errors.Is(err, ErrTooLarge) {
		return fmt.Errorf("%w: part %q", ErrTooLarge, part)
	}
	return invalidf("parse %q: %v", part, err)
}

// readTheme extracts the colour scheme in theme-index order.
func (rd *reader) readTheme(part string) ([]string, error) {
	b, err := rd.p.readAll(part, maxSmallPart)
	if err != nil {
		return nil, err
	}
	d := newDecoder(bytes.NewReader(b))
	colors := map[string]string{}
	inScheme, current, depth := false, "", 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > MaxXMLDepth {
				return nil, ErrTooLarge
			}
			switch t.Name.Local {
			case "clrScheme":
				inScheme = true
			case "dk1", "lt1", "dk2", "lt2", "accent1", "accent2", "accent3", "accent4", "accent5", "accent6", "hlink", "folHlink":
				if inScheme {
					current = t.Name.Local
				}
			case "srgbClr", "sysClr":
				if inScheme && current != "" {
					for _, a := range t.Attr {
						if (t.Name.Local == "srgbClr" && a.Name.Local == "val") || (t.Name.Local == "sysClr" && a.Name.Local == "lastClr") {
							colors[current] = strings.ToUpper(a.Value)
						}
					}
				}
			}
		case xml.EndElement:
			depth--
			if t.Name.Local == "clrScheme" {
				inScheme = false
			}
		}
	}
	out := append([]string(nil), defaultTheme...)
	for i, k := range []string{"lt1", "dk1", "lt2", "dk2", "accent1", "accent2", "accent3", "accent4", "accent5", "accent6", "hlink", "folHlink"} {
		if c, ok := colors[k]; ok && len(c) == 6 {
			out[i] = c
		}
	}
	return out, nil
}

type xVal struct {
	Val *string `xml:"val,attr"`
}

func (v *xVal) on() bool { return v != nil && (v.Val == nil || xmlBool(*v.Val)) }

type xColor struct {
	RGB     string  `xml:"rgb,attr"`
	Theme   *int    `xml:"theme,attr"`
	Indexed *int    `xml:"indexed,attr"`
	Tint    float64 `xml:"tint,attr"`
	Auto    string  `xml:"auto,attr"`
}

type xFont struct {
	B      *xVal   `xml:"b"`
	I      *xVal   `xml:"i"`
	Strike *xVal   `xml:"strike"`
	U      *xVal   `xml:"u"`
	Sz     *xVal   `xml:"sz"`
	Color  *xColor `xml:"color"`
}

type xFill struct {
	Pattern *struct {
		Type string  `xml:"patternType,attr"`
		Fg   *xColor `xml:"fgColor"`
		Bg   *xColor `xml:"bgColor"`
	} `xml:"patternFill"`
}

type xSide struct {
	Style string `xml:"style,attr"`
}

type xBorder struct {
	Left   *xSide `xml:"left"`
	Right  *xSide `xml:"right"`
	Top    *xSide `xml:"top"`
	Bottom *xSide `xml:"bottom"`
	Start  *xSide `xml:"start"`
	End    *xSide `xml:"end"`
}

type xXf struct {
	NumFmtID  int `xml:"numFmtId,attr"`
	FontID    int `xml:"fontId,attr"`
	FillID    int `xml:"fillId,attr"`
	BorderID  int `xml:"borderId,attr"`
	Alignment *struct {
		Horizontal string `xml:"horizontal,attr"`
		Vertical   string `xml:"vertical,attr"`
		Wrap       string `xml:"wrapText,attr"`
	} `xml:"alignment"`
}

type xStyles struct {
	NumFmts []struct {
		ID   int    `xml:"numFmtId,attr"`
		Code string `xml:"formatCode,attr"`
	} `xml:"numFmts>numFmt"`
	Fonts   []xFont   `xml:"fonts>font"`
	Fills   []xFill   `xml:"fills>fill"`
	Borders []xBorder `xml:"borders>border"`
	CellXfs []xXf     `xml:"cellXfs>xf"`
}

func resolveColor(c *xColor, theme []string) string {
	if c == nil || xmlBool(c.Auto) {
		return ""
	}
	var rgb string
	switch {
	case len(c.RGB) == 8:
		rgb = strings.ToUpper(c.RGB[2:])
	case len(c.RGB) == 6:
		rgb = strings.ToUpper(c.RGB)
	case c.Theme != nil && *c.Theme >= 0 && *c.Theme < len(theme):
		rgb = theme[*c.Theme]
	case c.Indexed != nil && *c.Indexed >= 0 && *c.Indexed < len(indexedPalette):
		rgb = indexedPalette[*c.Indexed]
	default:
		return ""
	}
	if _, err := strconv.ParseUint(rgb, 16, 32); err != nil {
		return ""
	}
	return "#" + strings.ToLower(applyTint(rgb, c.Tint))
}

// borderOn reports whether an edge has any line; the engine models borders
// as on/off (line styles collapse to a thin line).
func borderOn(s *xSide) bool {
	return s != nil && s.Style != "" && s.Style != "none"
}

func (rd *reader) readStyles(part string, theme []string) error {
	var x xStyles
	if err := rd.p.unmarshalSmall(part, &x); err != nil {
		return err
	}
	custom := map[int]string{}
	for _, nf := range x.NumFmts {
		custom[nf.ID] = nf.Code
	}
	defSize, defColor := 0.0, ""
	if len(x.Fonts) > 0 {
		if x.Fonts[0].Sz != nil && x.Fonts[0].Sz.Val != nil {
			defSize, _ = strconv.ParseFloat(*x.Fonts[0].Sz.Val, 64)
		}
		defColor = resolveColor(x.Fonts[0].Color, theme)
	}
	rd.styles = make([]*engine.Style, len(x.CellXfs))
	rd.isDate = make([]bool, len(x.CellXfs))
	for i, xf := range x.CellXfs {
		st := &engine.Style{}
		code, ok := custom[xf.NumFmtID]
		if !ok {
			code, _ = engine.BuiltinNumFmt(xf.NumFmtID)
		}
		if code != "" && !strings.EqualFold(code, "general") {
			st.Fmt = truncateRunes(code, engine.MaxFormatLen)
			rd.isDate[i] = engine.IsDateFormat(code)
		}
		if xf.FontID >= 0 && xf.FontID < len(x.Fonts) {
			f := x.Fonts[xf.FontID]
			st.Bold, st.Italic, st.Strike = f.B.on(), f.I.on(), f.Strike.on()
			if f.U != nil && (f.U.Val == nil || *f.U.Val != "none") {
				st.Underline = true
			}
			if f.Sz != nil && f.Sz.Val != nil {
				if sz, err := strconv.ParseFloat(*f.Sz.Val, 64); err == nil && sz != defSize && sz >= 1 && sz <= 409 {
					st.Size = int(math.Round(sz))
				}
			}
			if c := resolveColor(f.Color, theme); c != "" && c != defColor && c != "#000000" {
				st.Color = c
			}
		}
		if xf.FillID >= 0 && xf.FillID < len(x.Fills) {
			if pf := x.Fills[xf.FillID].Pattern; pf != nil && pf.Type != "" && pf.Type != "none" && pf.Type != "gray125" {
				c := resolveColor(pf.Fg, theme)
				if c == "" && pf.Type != "solid" {
					c = resolveColor(pf.Bg, theme)
				}
				st.Bg = c
			}
		}
		if xf.BorderID >= 0 && xf.BorderID < len(x.Borders) {
			b := x.Borders[xf.BorderID]
			left, right := b.Left, b.Right
			if left == nil {
				left = b.Start
			}
			if right == nil {
				right = b.End
			}
			bd := engine.Border{Top: borderOn(b.Top), Bottom: borderOn(b.Bottom), Left: borderOn(left), Right: borderOn(right)}
			if bd != (engine.Border{}) {
				st.Border = &bd
			}
		}
		if a := xf.Alignment; a != nil {
			switch a.Horizontal {
			case "left", "right", "center":
				st.Align = a.Horizontal
			case "centerContinuous", "distributed", "justify":
				st.Align = "center"
				if a.Horizontal == "justify" {
					st.Align = "left"
				}
			}
			switch a.Vertical {
			case "top", "bottom":
				st.VAlign = a.Vertical
			case "center", "distributed", "justify":
				st.VAlign = "middle"
			}
			st.Wrap = xmlBool(a.Wrap)
		}
		norm, err := st.Normalize()
		if err == nil {
			rd.styles[i] = norm
		}
	}
	return nil
}

type sharedFormula struct {
	text     string
	row, col int
}

// readSheet streams one worksheet into s.
func (rd *reader) readSheet(part string, s *engine.Sheet) error {
	rc, err := rd.p.open(part, MaxUncompressed)
	if err != nil {
		return err
	}
	defer rc.Close()
	d := newDecoder(rc)
	shared := map[string]sharedFormula{}
	depth := 0
	// Rows/cells may omit their r= attribute; positions then continue from
	// the previous row/cell.
	row, nextRow, col := 0, 0, -1
	paneDone := false

	type cellState struct {
		ref              engine.Ref
		style            int
		typ              string
		v, f, is         strings.Builder
		hasV, hasF       bool
		fType, fSI, fRef string
	}
	var c *cellState
	var textTarget *strings.Builder
	inIS, isT := false, false
	addText := func(b *strings.Builder, data []byte) {
		if b.Len() < 4*engine.MaxCellChars {
			b.Write(data)
		}
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return wrapXMLErr(part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > MaxXMLDepth {
				return fmt.Errorf("%w: XML nesting deeper than %d", ErrTooLarge, MaxXMLDepth)
			}
			switch t.Name.Local {
			case "pane":
				if paneDone {
					break
				}
				paneDone = true
				var xs, ys float64
				frozen := false
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "xSplit":
						xs, _ = strconv.ParseFloat(a.Value, 64)
					case "ySplit":
						ys, _ = strconv.ParseFloat(a.Value, 64)
					case "state":
						frozen = a.Value == "frozen" || a.Value == "frozenSplit"
					}
				}
				if frozen {
					s.FrozenCols = clampInt(xs, 0, engine.MaxCols-1)
					s.FrozenRows = clampInt(ys, 0, engine.MaxRows-1)
				}
			case "col":
				rd.readCol(t, s)
			case "row":
				col = -1
				row = min(nextRow, engine.MaxRows-1)
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						if n, err := strconv.Atoi(a.Value); err == nil && n >= 1 && n <= engine.MaxRows {
							row = n - 1
						}
					}
				}
				var ht float64
				custom := false
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "ht":
						ht, _ = strconv.ParseFloat(a.Value, 64)
					case "customHeight":
						custom = xmlBool(a.Value)
					}
				}
				if custom && ht > 0 && ht <= 409.5 {
					s.RowHeights[row] = ptToPx(ht)
				}
			case "c":
				c = &cellState{style: 0}
				col++
				c.ref = engine.Ref{Row: row, Col: col}
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						if ref, err := engine.ParseRef(a.Value); err == nil {
							c.ref = ref
							row, col = ref.Row, ref.Col
						} else {
							c.ref = engine.Ref{Row: -1, Col: -1} // off-grid: dropped
						}
					case "s":
						c.style, _ = strconv.Atoi(a.Value)
					case "t":
						c.typ = a.Value
					}
				}
			case "v":
				if c != nil {
					c.hasV = true
					textTarget = &c.v
				}
			case "f":
				if c != nil {
					c.hasF = true
					textTarget = &c.f
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "t":
							c.fType = a.Value
						case "si":
							c.fSI = a.Value
						case "ref":
							c.fRef = a.Value
						}
					}
				}
			case "is":
				inIS = true
			case "t":
				if inIS && c != nil {
					isT = true
				}
			case "rPh":
				if inIS {
					isT = false
					inIS = false // phonetic runs are not part of the text
				}
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "v", "f":
				textTarget = nil
			case "t":
				isT = false
			case "is":
				inIS = false
			case "row":
				nextRow = row + 1
				col = -1
			case "c":
				if c != nil {
					if err := rd.finishCell(s, c.ref, c.style, c.typ, c.v.String(), c.hasV, c.f.String(), c.hasF, c.fType, c.fSI, c.is.String(), shared); err != nil {
						return err
					}
				}
				c = nil
			}
		case xml.CharData:
			switch {
			case textTarget != nil:
				addText(textTarget, t)
			case isT && c != nil:
				addText(&c.is, t)
			}
		}
	}
	return nil
}

func clampInt(f float64, lo, hi int) int {
	if math.IsNaN(f) || f < float64(lo) {
		return lo
	}
	if f > float64(hi) {
		return hi
	}
	return int(f)
}

func (rd *reader) readCol(t xml.StartElement, s *engine.Sheet) {
	var mn, mx int
	var width float64
	custom, hasWidth := false, false
	for _, a := range t.Attr {
		switch a.Name.Local {
		case "min":
			mn, _ = strconv.Atoi(a.Value)
		case "max":
			mx, _ = strconv.Atoi(a.Value)
		case "width":
			if w, err := strconv.ParseFloat(a.Value, 64); err == nil && w >= 0 && w <= 255 {
				width, hasWidth = w, true
			}
		case "customWidth":
			custom = xmlBool(a.Value)
		}
	}
	if !hasWidth || !custom || mn < 1 || mx < mn || mx > engine.MaxCols {
		return
	}
	// Spans covering most of the sheet are "default width for the rest" hints.
	if mx-mn > 1024 {
		return
	}
	for c := mn - 1; c < mx; c++ {
		s.ColWidths[c] = widthToPx(width)
	}
}

func (rd *reader) finishCell(s *engine.Sheet, ref engine.Ref, style int, typ, v string, hasV bool, f string, hasF bool, fType, fSI, inline string, shared map[string]sharedFormula) error {
	if !ref.Valid() {
		return nil
	}
	cell := &engine.Cell{}
	if style > 0 && style < len(rd.styles) {
		cell.Style = rd.styles[style]
	}
	switch typ {
	case "s":
		if idx, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && idx >= 0 && idx < len(rd.sst) {
			cell.Value = engine.StringValue(rd.sst[idx])
		}
	case "str":
		if hasV {
			cell.Value = engine.StringValue(truncateRunes(decodeST(v), engine.MaxCellChars))
		}
	case "inlineStr":
		cell.Value = engine.StringValue(truncateRunes(decodeST(inline), engine.MaxCellChars))
	case "b":
		if hasV {
			cell.Value = engine.BoolValue(xmlBool(strings.TrimSpace(v)))
		}
	case "e":
		code, ok := engine.CanonicalErrorCode(strings.TrimSpace(v))
		if !ok {
			code = engine.CodeValue
		}
		cell.Value = engine.ErrorValue(code)
	case "d":
		if tm, ok := parseISODate(strings.TrimSpace(v)); ok {
			cell.Value = engine.NumberValue(engine.TimeToSerial(tm))
		}
	default:
		if hasV && strings.TrimSpace(v) != "" {
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && !math.IsInf(n, 0) {
				if rd.date1904 && style > 0 && style < len(rd.isDate) && rd.isDate[style] {
					n += engine.Date1904Offset
				}
				cell.Value = engine.NumberValue(n)
			}
		}
	}
	if hasF {
		text := ""
		switch fType {
		case "shared":
			if strings.TrimSpace(f) != "" {
				shared[fSI] = sharedFormula{text: f, row: ref.Row, col: ref.Col}
				text = f
			} else if m, ok := shared[fSI]; ok {
				if sh, err := engine.ShiftFormula(m.text, ref.Row-m.row, ref.Col-m.col); err == nil {
					text = sh
				}
			}
		case "dataTable":
		default:
			text = f
		}
		if text = strings.TrimSpace(text); text != "" {
			full := "=" + text
			if engine.FormulaSupported(full) {
				if canon, err := engine.RewriteFormula(full, engine.FormatOptions{}); err == nil && len(canon) <= 4*engine.MaxCellChars {
					cell.Formula = canon
				}
			}
		}
	}
	if cell.IsEmpty() {
		return nil
	}
	rd.cells++
	if rd.cells > engine.MaxCellsPerWorkbook {
		return fmt.Errorf("%w: more than %d cells", ErrTooLarge, engine.MaxCellsPerWorkbook)
	}
	s.SetCell(ref, cell)
	return nil
}

func parseISODate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02", "15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "15:04:05" {
				t = time.Date(1899, 12, 31, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			}
			if t.Year() < 1899 || t.Year() > 9999 {
				return time.Time{}, false
			}
			return t, true
		}
	}
	return time.Time{}, false
}
