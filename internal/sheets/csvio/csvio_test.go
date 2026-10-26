package csvio

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"ludworks/internal/sheets/engine"
)

func TestSniff(t *testing.T) {
	cases := map[string]rune{
		"a,b,c\n1,2,3\n":                       ',',
		"a;b;c\n1,5;2,5;3\n":                   ';',
		"a\tb\tc\n1\t2\t3":                     '\t',
		"a|b|c\n1|2|3\n":                       '|',
		"\"x,y,z\";b;c\n\"1,2\";3;4\n":         ';',
		"single":                               ',',
		"name;age, city;x\nbob;3, x;y\n":       ';',
		"a,b\nc,d\ne;f;g;h\n":                  ',',
		"only one;line;here,with,commas,extra": ',',
	}
	for in, want := range cases {
		if got := Sniff(in); got != want {
			t.Errorf("Sniff(%q) = %q want %q", in, got, want)
		}
	}
}

func TestInfer(t *testing.T) {
	cases := map[string]engine.Value{
		"42": engine.NumberValue(42), "-1.5": engine.NumberValue(-1.5), "0": engine.NumberValue(0), "0.25": engine.NumberValue(0.25),
		"1e3": engine.NumberValue(1000), "-0": engine.NumberValue(0), "TRUE": engine.BoolValue(true), "false": engine.BoolValue(false),
		"007": engine.StringValue("007"), "00.5": engine.StringValue("00.5"), "+5": engine.StringValue("+5"), "1,234": engine.StringValue("1,234"),
		" 5": engine.StringValue(" 5"), "5 ": engine.StringValue("5 "), "1.": engine.StringValue("1."), ".5": engine.StringValue(".5"),
		"50%": engine.StringValue("50%"), "$5": engine.StringValue("$5"), "2024-01-15": engine.StringValue("2024-01-15"),
		"12345678901234567890": engine.StringValue("12345678901234567890"), "123456789012345": engine.NumberValue(123456789012345),
		"1e999": engine.StringValue("1e999"), "=SUM(A1)": engine.StringValue("=SUM(A1)"), "NaN": engine.StringValue("NaN"),
		"Inf": engine.StringValue("Inf"), "0x10": engine.StringValue("0x10"), "1_000": engine.StringValue("1_000"),
	}
	for in, want := range cases {
		if got := Infer(in); !got.Equal(want) {
			t.Errorf("Infer(%q) = %v (%v) want %v", in, got, got.Kind, want)
		}
	}
}

func TestInferFormatted(t *testing.T) {
	type want struct {
		v   engine.Value
		fmt string
	}
	num := func(f float64, format string) want { return want{engine.NumberValue(f), format} }
	txt := func(s string) want { return want{engine.StringValue(s), ""} }
	cases := map[string]want{
		// what the exporter writes for formatted numbers
		"$1,234.50": num(1234.5, "$#,##0.00"), "-$1,234.50": num(-1234.5, "$#,##0.00"), "$5": num(5, "$#,##0.00"),
		"12%": num(0.12, "0%"), "12.5%": num(0.125, "0.00%"), "-5%": num(-0.05, "0%"), "0.5%": num(0.005, "0.00%"),
		"1,234": num(1234, "#,##0"), "1,234,567.25": num(1234567.25, "#,##0.00"), "(3)": num(-3, ""), "($1,234.50)": num(-1234.5, "$#,##0.00"),
		"$0.99":      num(0.99, "$#,##0.00"),
		"2024-01-15": num(45306, "yyyy-mm-dd"), "2024-01-15 9:30": num(45306+9.5/24, "yyyy-mm-dd h:mm"),
		// plain values keep the plain path
		"42": num(42, ""), "TRUE": {engine.BoolValue(true), ""},
		// still text
		"007": txt("007"), "$007": txt("$007"), "0,123": txt("0,123"), "1,23": txt("1,23"), "1,2,3": txt("1,2,3"),
		"+5%": txt("+5%"), " 12%": txt(" 12%"), "12% ": txt("12% "), "5%%": txt("5%%"), "$": txt("$"), "%": txt("%"),
		"$1,234,567,890,123,456.78": txt("$1,234,567,890,123,456.78"), "2024-13-45": txt("2024-13-45"),
		"15/01/2024": txt("15/01/2024"), "=1+1": txt("=1+1"), "'=1+1": txt("'=1+1"), "€5": txt("€5"), "(abc)": txt("(abc)"),
	}
	for in, w := range cases {
		v, format := InferFormatted(in, ',')
		if !v.Equal(w.v) || format != w.fmt {
			t.Errorf("InferFormatted(%q) = %v %q want %v %q", in, v, format, w.v, w.fmt)
		}
	}
	// Outside comma-delimited files a comma is a decimal comma: leave it alone.
	for _, d := range []rune{';', '\t', '|'} {
		if v, f := InferFormatted("1,234", d); v.Kind != engine.KindString || f != "" {
			t.Errorf("InferFormatted(1,234, %q) = %v %q", d, v, f)
		}
		if v, f := InferFormatted("$5", d); !v.Equal(engine.NumberValue(5)) || f != "$#,##0.00" {
			t.Errorf("InferFormatted($5, %q) = %v %q", d, v, f)
		}
	}
}

// Regression: the default (display-formatted) export of currency, percent,
// thousands and date cells must re-import as numbers with their formats,
// not as text that SUM() ignores.
func TestFormattedRoundTrip(t *testing.T) {
	src := sheetOf(t, map[string]engine.Cell{
		"A1": {Value: engine.StringValue("Item")},
		"B1": {Value: engine.NumberValue(1200), Style: &engine.Style{Fmt: "$#,##0.00"}},
		"C1": {Value: engine.NumberValue(-89.99), Style: &engine.Style{Fmt: "$#,##0.00"}},
		"D1": {Value: engine.NumberValue(0.12), Style: &engine.Style{Fmt: "0%"}},
		"E1": {Value: engine.NumberValue(0.125), Style: &engine.Style{Fmt: "0.00%"}},
		"F1": {Value: engine.NumberValue(1234567), Style: &engine.Style{Fmt: "#,##0"}},
		"G1": {Value: engine.NumberValue(45306), Style: &engine.Style{Fmt: "yyyy-mm-dd"}},
		"H1": {Value: engine.StringValue("00123")},
	})
	var buf bytes.Buffer
	if err := Write(&buf, src, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	got := readStr(t, buf.String(), ReadOptions{})
	for ref, c := range src.Cells {
		g := got.Cell(ref)
		if g == nil || !g.Value.Equal(c.Value) {
			t.Errorf("%s: %+v after round trip of %q, want %v", ref, g, buf.String(), c.Value)
			continue
		}
		wantFmt := ""
		if c.Style != nil {
			wantFmt = c.Style.Fmt
		}
		gotFmt := ""
		if g.Style != nil {
			gotFmt = g.Style.Fmt
		}
		if gotFmt != wantFmt {
			t.Errorf("%s: format %q want %q", ref, gotFmt, wantFmt)
		}
	}
}

func readStr(t *testing.T, s string, o ReadOptions) *engine.Sheet {
	t.Helper()
	sh, err := Read(strings.NewReader(s), o)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestRead(t *testing.T) {
	in := "\xEF\xBB\xBFname;qty;note\r\n\"Smith; J\";007;\"multi\nline \"\"quoted\"\"\"\r\nx;=1+1;TRUE\nragged\n;;;4.5\n"
	s := readStr(t, in, ReadOptions{SheetName: "Imported", SheetID: "id1"})
	if s.Name != "Imported" || s.ID != "id1" {
		t.Errorf("name/id %q %q", s.Name, s.ID)
	}
	want := map[string]engine.Value{
		"A1": engine.StringValue("name"), "B1": engine.StringValue("qty"), "A2": engine.StringValue("Smith; J"),
		"B2": engine.StringValue("007"), "C2": engine.StringValue("multi\nline \"quoted\""), "A3": engine.StringValue("x"),
		"B3": engine.StringValue("=1+1"), "C3": engine.BoolValue(true), "A4": engine.StringValue("ragged"), "D5": engine.NumberValue(4.5),
	}
	for a1, v := range want {
		r, _ := engine.ParseRef(a1)
		c := s.Cell(r)
		if c == nil || !c.Value.Equal(v) || c.Formula != "" {
			t.Errorf("%s = %+v want %v", a1, c, v)
		}
	}
	if len(s.Cells) != len(want)+1 { // + C1 "note"
		t.Errorf("cell count %d", len(s.Cells))
	}
	// NoInfer keeps text
	s = readStr(t, "1,TRUE\n", ReadOptions{NoInfer: true})
	if v := s.Value(engine.Ref{}); v.Kind != engine.KindString {
		t.Errorf("NoInfer: %v", v.Kind)
	}
	// UTF-16LE with BOM, tab separated
	u16 := []byte{0xFF, 0xFE}
	for _, r := range "a\tb\r\n1\t€\r\n" {
		u16 = append(u16, byte(r), byte(r>>8))
	}
	s = readStr(t, string(u16), ReadOptions{})
	if s.Value(engine.Ref{Row: 1, Col: 1}).Str != "€" || s.Value(engine.Ref{Col: 1}).Str != "b" {
		t.Errorf("utf16: %v", s.Cells)
	}
	// Windows-1252 fallback
	s = readStr(t, "price,\x80 5\n", ReadOptions{})
	if s.Value(engine.Ref{Col: 1}).Str != "€ 5" {
		t.Errorf("cp1252: %q", s.Value(engine.Ref{Col: 1}).Str)
	}
	// explicit delimiter
	s = readStr(t, "a|b,c\n", ReadOptions{Delimiter: '|'})
	if s.Value(engine.Ref{Col: 1}).Str != "b,c" {
		t.Errorf("explicit delimiter")
	}
	// lazy quotes are tolerated
	s = readStr(t, "a \"quoted\" word,b\n", ReadOptions{})
	if s.Value(engine.Ref{}).Str != `a "quoted" word` {
		t.Errorf("lazy quotes: %q", s.Value(engine.Ref{}).Str)
	}
}

func TestReadLimits(t *testing.T) {
	if _, err := Read(strings.NewReader("1,2,3\n4,5,6\n"), ReadOptions{MaxCells: 5}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("max cells: %v", err)
	}
	if _, err := Read(strings.NewReader(strings.Repeat("x", 100)), ReadOptions{MaxBytes: 50}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("max bytes: %v", err)
	}
	if _, err := Read(strings.NewReader(strings.Repeat("y", engine.MaxCellChars+1)), ReadOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("long cell: %v", err)
	}
	if _, err := Read(strings.NewReader(strings.Repeat(",", engine.MaxCols)+"x"), ReadOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too many columns: %v", err)
	}
	if _, err := Read(strings.NewReader("a"), ReadOptions{Delimiter: '"'}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad delimiter: %v", err)
	}
	if _, err := Read(strings.NewReader("a"), ReadOptions{SheetName: "bad/name"}); !errors.Is(err, engine.ErrInvalidSheetName) {
		t.Errorf("bad sheet name: %v", err)
	}
}

func sheetOf(t *testing.T, cells map[string]engine.Cell) *engine.Sheet {
	s := engine.NewSheet("s", "S", 0, 0)
	for a1, c := range cells {
		r, _ := engine.ParseRef(a1)
		cc := c
		s.SetCell(r, &cc)
	}
	return s
}

func TestWrite(t *testing.T) {
	s := sheetOf(t, map[string]engine.Cell{
		"A1": {Value: engine.StringValue("plain")},
		"B1": {Value: engine.StringValue("has,comma")},
		"C1": {Value: engine.StringValue("quote\"inside")},
		"D1": {Value: engine.StringValue("multi\nline")},
		"A2": {Value: engine.StringValue("=HYPERLINK(\"http://evil\")")},
		"B2": {Value: engine.StringValue("+cmd|' /C calc'!A0")},
		"C2": {Value: engine.StringValue("-2+3")},
		"D2": {Value: engine.StringValue("@SUM(1)")},
		"A3": {Value: engine.NumberValue(-5)},
		"B3": {Value: engine.NumberValue(1.5), Style: &engine.Style{Fmt: "0.00"}},
		"C3": {Value: engine.NumberValue(-1234.5), Style: &engine.Style{Fmt: "$#,##0.00"}},
		"D3": {Value: engine.NumberValue(7), Formula: "=3+4"},
		"A4": {Value: engine.BoolValue(true)},
		"B4": {Value: engine.ErrorValue(engine.CodeNA)},
		"C4": {Value: engine.NumberValue(5), Style: &engine.Style{Fmt: `"=1+"0`}},
		"D4": {Value: engine.NumberValue(45306), Style: &engine.Style{Fmt: "yyyy-mm-dd"}},
		"F9": {Style: &engine.Style{Bold: true}}, // style-only: not part of the export extent
		"A5": {Value: engine.NumberValue(0.1 + 0.2)},
		"B5": {Value: engine.NumberValue(-1.2e-10)},
	})
	var buf bytes.Buffer
	if err := Write(&buf, s, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// encoding/csv writes embedded newlines as CRLF when UseCRLF is set.
	want := "plain,\"has,comma\",\"quote\"\"inside\",\"multi\r\nline\"\r\n" +
		"\"'=HYPERLINK(\"\"http://evil\"\")\",'+cmd|' /C calc'!A0,'-2+3,'@SUM(1)\r\n" +
		"-5,1.50,\"-$1,234.50\",7\r\n" +
		"TRUE,#N/A,'=1+5,2024-01-15\r\n" +
		"0.3,-1.2E-10,,\r\n"
	if buf.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", buf.String(), want)
	}
	// raw values, LF, tab, BOM, guard off
	buf.Reset()
	if err := Write(&buf, s, WriteOptions{Raw: true, UseLF: true, Delimiter: '\t', BOM: true, AllowFormulas: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\xEF\xBB\xBFplain\thas,comma\t") {
		t.Errorf("raw prefix %q", out[:20])
	}
	for _, frag := range []string{"\n\"=HYPERLINK(\"\"http://evil\"\")\"\t+cmd", "-5\t1.5\t-1234.5\t7\n", "TRUE\t#N/A\t5\t45306\n", "0.3\t-1.2e-10\t\t\n"} {
		if !strings.Contains(out, frag) {
			t.Errorf("raw output missing %q in %q", frag, out)
		}
	}
	// empty sheet
	buf.Reset()
	if err := Write(&buf, engine.NewSheet("e", "E", 0, 0), WriteOptions{}); err != nil || buf.Len() != 0 {
		t.Errorf("empty sheet: %q %v", buf.String(), err)
	}
	if err := Write(&buf, s, WriteOptions{Delimiter: '\n'}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad delimiter: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	src := "id,name,score,flag\n001,\"Doe, Jane\",98.5,TRUE\n2,\"line\nbreak\",-3,false\n"
	s := readStr(t, src, ReadOptions{})
	var buf bytes.Buffer
	if err := Write(&buf, s, WriteOptions{Raw: true, UseLF: true}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "id,name,score,flag\n001,\"Doe, Jane\",98.5,TRUE\n2,\"line\nbreak\",-3,FALSE\n" {
		t.Errorf("round trip %q", buf.String())
	}
	s2 := readStr(t, buf.String(), ReadOptions{})
	for r, c := range s.Cells {
		if !s2.Value(r).Equal(c.Value) {
			t.Errorf("%s: %v != %v", r, s2.Value(r), c.Value)
		}
	}
}

func FuzzRead(f *testing.F) {
	f.Add([]byte("a,b\n1,2\n"))
	f.Add([]byte("\"x\"\"y\";z\r\n"))
	f.Add([]byte{0xFF, 0xFE, 'a', 0, ',', 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Read(bytes.NewReader(b), ReadOptions{MaxCells: 10000})
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := Write(&out, s, WriteOptions{}); err != nil {
			t.Fatalf("write after read: %v", err)
		}
	})
}
