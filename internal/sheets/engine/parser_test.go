package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sexpr renders an AST unambiguously so tests can assert structure.
func sexpr(e Expr) string {
	switch n := e.(type) {
	case *NumberLit:
		return n.Raw
	case *StringLit:
		return fmt.Sprintf("%q", n.Val)
	case *BoolLit:
		return fmt.Sprint(n.Val)
	case *ErrorLit:
		return n.Code
	case *CellRef:
		var b strings.Builder
		writeExpr(&b, n, &FormatOptions{})
		return b.String()
	case *RangeRef:
		var b strings.Builder
		writeExpr(&b, n, &FormatOptions{})
		return b.String()
	case *UnaryOp:
		return "(" + string(n.Op) + " " + sexpr(n.X) + ")"
	case *PercentOp:
		return "(% " + sexpr(n.X) + ")"
	case *BinaryOp:
		return "(" + n.Op.String() + " " + sexpr(n.L) + " " + sexpr(n.R) + ")"
	case *ParenExpr:
		return sexpr(n.X)
	case *CallExpr:
		parts := []string{n.Name}
		for _, a := range n.Args {
			parts = append(parts, sexpr(a))
		}
		return "(" + strings.Join(parts, " ") + ")"
	case *ArrayLit:
		var rows []string
		for _, r := range n.Rows {
			var els []string
			for _, x := range r {
				els = append(els, sexpr(x))
			}
			rows = append(rows, strings.Join(els, ","))
		}
		return "{" + strings.Join(rows, ";") + "}"
	case *NameExpr:
		return "name:" + n.Name
	case *MissingArg:
		return "_"
	}
	return "?"
}

func TestRefs(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
		ok   bool
	}{
		{"A1", Ref{0, 0}, true}, {"$A$1", Ref{0, 0}, true}, {"b3", Ref{2, 1}, true},
		{"Z1", Ref{0, 25}, true}, {"AA100", Ref{99, 26}, true}, {"XFD1048576", Ref{1048575, 16383}, true},
		{"XFE1", Ref{}, false}, {"A0", Ref{}, false}, {"A1048577", Ref{}, false}, {"1A", Ref{}, false},
		{"A01", Ref{}, false}, {"", Ref{}, false}, {"A$", Ref{}, false}, {"ABCD1", Ref{}, false},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseRef(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
		if c.ok && !strings.Contains(c.in, "$") && strings.ToUpper(c.in) != got.String() {
			t.Errorf("String(%v) = %q", got, got.String())
		}
	}
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA", 16383: "XFD"} {
		if ColName(i) != want {
			t.Errorf("ColName(%d) = %q want %q", i, ColName(i), want)
		}
		if c, err := ParseColName(want); err != nil || c != i {
			t.Errorf("ParseColName(%q) = %d, %v", want, c, err)
		}
	}
	rngs := map[string]string{"A1:B2": "A1:B2", "B2:A1": "A1:B2", "A:C": "A:C", "3:5": "3:5", "C3": "C3", "$A$1:$B$9": "A1:B9"}
	for in, want := range rngs {
		r, err := ParseRange(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseRange(%q) = %v %v want %q", in, r, err, want)
		}
	}
	for _, bad := range []string{"A1:", ":B2", "A1:B", "A:1", "1:B", "foo"} {
		if _, err := ParseRange(bad); err == nil {
			t.Errorf("ParseRange(%q) should fail", bad)
		}
	}
}

func TestParserPrecedence(t *testing.T) {
	cases := map[string]string{
		"1+2*3":               "(+ 1 (* 2 3))",
		"(1+2)*3":             "(* (+ 1 2) 3)",
		"-2^2":                "(^ (- 2) 2)",
		"2^-2":                "(^ 2 (- 2))",
		"2^3^2":               "(^ (^ 2 3) 2)",
		"-50%":                "(% (- 50))",
		"50%^2":               "(^ (% 50) 2)",
		"1-2-3":               "(- (- 1 2) 3)",
		"8/4/2":               "(/ (/ 8 4) 2)",
		"1&2=12":              "(= (& 1 2) 12)",
		"1+1&2":               "(& (+ 1 1) 2)",
		"A1<>B1":              "(<> A1 B1)",
		"1<=2":                "(<= 1 2)",
		"--A1":                "(- (- A1))",
		"+A1":                 "(+ A1)",
		"\"a\"\"b\"":          `"a\"b"`,
		"SUM(A1:B2,C3)":       "(SUM A1:B2 C3)",
		"IF(A1,,1)":           "(IF A1 _ 1)",
		"F()":                 "(F)",
		"Sheet2!A1":           "Sheet2!A1",
		"'My Sheet'!A1:B2":    "'My Sheet'!A1:B2",
		"'It''s'!$A$1":        "'It''s'!$A$1",
		"A:A":                 "A:A",
		"$B:D":                "$B:D",
		"1:3":                 "1:3",
		"Sheet2!2:$4":         "Sheet2!2:$4",
		"#N/A":                "#N/A",
		"#div/0!+1":           "(+ #DIV/0! 1)",
		"true":                "true",
		"FALSE()":             "(FALSE)",
		"_xlfn.CONCAT(1)":     "(CONCAT 1)",
		"sum(a1)":             "(SUM A1)",
		"{1,2;3,-4}":          "{1,2;3,-4}",
		"{\"a\",TRUE,#N/A}":   `{"a",true,#N/A}`,
		"TaxRate*2":           "(* name:TaxRate 2)",
		"B5:A1":               "A1:B5",
		"1e3+.5":              "(+ 1e3 .5)",
		"Sheet1!A1:Sheet1!B2": "Sheet1!A1:B2",
		"A1 + B1":             "(+ A1 B1)",
	}
	for in, want := range cases {
		f, err := ParseFormula("=" + in)
		if err != nil {
			t.Errorf("parse %q: %v", in, err)
			continue
		}
		if got := sexpr(f.Root); got != want {
			t.Errorf("parse %q = %s want %s", in, got, want)
		}
	}
}

func TestParserPrintRoundTrip(t *testing.T) {
	cases := map[string]string{
		"=sum( a1 , b2 )":                 "=SUM(A1,B2)",
		"=A1+'Other Sheet'!$B$2":          "=A1+'Other Sheet'!$B$2",
		"=IF(A1>0,\"yes\",\"no\"\"\")":    "=IF(A1>0,\"yes\",\"no\"\"\")",
		"=-(1+2)%":                        "=-(1+2)%",
		"={1,2;3,4}":                      "={1,2;3,4}",
		"=_xlfn.TEXTJOIN(\",\",TRUE,A:A)": "=TEXTJOIN(\",\",TRUE,A:A)",
		"=Data!1:1":                       "=Data!1:1",
		"='2024'!A1":                      "='2024'!A1",
		"='AB1'!A1":                       "='AB1'!A1",
	}
	for in, want := range cases {
		f, err := ParseFormula(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := f.String(); got != want {
			t.Errorf("print(%q) = %q want %q", in, got, want)
		}
		// printing is a fixed point
		f2, err := ParseFormula(f.String())
		if err != nil || f2.String() != f.String() {
			t.Errorf("reparse of %q gave %v %v", f.String(), f2, err)
		}
	}
}

func TestParserErrors(t *testing.T) {
	bad := []string{"=", "=1+", "=(1", "=1)", "=SUM(1,", "=\"abc", "='Sheet", "='S'A1", "=#FOO!", "=A1:", "=A1:B",
		"=1 2", "={1,2;3}", "={A1}", "=Sheet1!", "=Sheet1!foo", "=A1:Sheet2!B2", "=1:A", "=@A1", "=1..2", "=$A$"}
	for _, in := range bad {
		if _, err := ParseFormula(in); err == nil {
			t.Errorf("ParseFormula(%q) should fail", in)
		} else if !errors.Is(err, ErrFormulaSyntax) && !errors.Is(err, ErrFormulaTooDeep) {
			t.Errorf("ParseFormula(%q) error %v not a syntax error", in, err)
		}
	}
	deep := "=" + strings.Repeat("(", 65) + "1" + strings.Repeat(")", 65)
	if _, err := ParseFormula(deep); !errors.Is(err, ErrFormulaTooDeep) {
		t.Errorf("deep nesting: %v", err)
	}
	ok := "=" + strings.Repeat("(", 63) + "1" + strings.Repeat(")", 63)
	if _, err := ParseFormula(ok); err != nil {
		t.Errorf("63 levels should parse: %v", err)
	}
	if _, err := ParseFormula("=" + strings.Repeat("-", 100) + "1"); !errors.Is(err, ErrFormulaTooDeep) {
		t.Errorf("unary chain should hit depth limit: %v", err)
	}
	if _, err := ParseFormula("=" + strings.Repeat("1+", 20000) + "1"); !errors.Is(err, ErrCellTooLong) {
		t.Errorf("over-long formula: %v", err)
	}
}

func TestShiftFormula(t *testing.T) {
	cases := []struct {
		in     string
		dr, dc int
		want   string
	}{
		{"A1+$B$2+C$3+$D4", 1, 1, "B2+$B$2+D$3+$D5"},
		{"SUM(A1:A3)", 2, 0, "SUM(A3:A5)"},
		{"SUM($A1:A$3)", 1, 1, "SUM($A2:B$3)"},
		{"A1", -1, 0, "#REF!"},
		{"=Sheet2!A1*2", 0, 3, "=Sheet2!D1*2"},
		{"SUM(A:A)", 5, 1, "SUM(B:B)"},
		{"SUM(1:1)", 5, 1, "SUM(6:6)"},
	}
	for _, c := range cases {
		got, err := ShiftFormula(c.in, c.dr, c.dc)
		if err != nil || got != c.want {
			t.Errorf("ShiftFormula(%q,%d,%d) = %q, %v want %q", c.in, c.dr, c.dc, got, err, c.want)
		}
	}
}

func TestFormulaSupported(t *testing.T) {
	for f, want := range map[string]bool{
		"=SUM(A1:A3)":                        true,
		"=_xlfn.CONCAT(A1)":                  true,
		"=FOOBAR(1)":                         false,
		"=TaxRate*2":                         false,
		"=Table1[Col]":                       false,
		"=ROUND(1)":                          false,
		"=IF(A1,1,2)+VLOOKUP(1,A:B,2,FALSE)": true,
	} {
		if got := FormulaSupported(f); got != want {
			t.Errorf("FormulaSupported(%q) = %v", f, got)
		}
	}
}

func FuzzParseFormula(f *testing.F) {
	for _, s := range []string{"=SUM(A1:B2)", "=-2^2%", "='a b'!A:A", "={1,2;3,4}", "=IF(A1,,\"x\"\"y\")", "=#N/A&1:3", "=_xlfn.X(1,,2)"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		fm, err := ParseFormula(s)
		if err != nil {
			return
		}
		out := fm.String()
		fm2, err := ParseFormula(out)
		if err != nil {
			t.Fatalf("re-parse of printed %q (from %q) failed: %v", out, s, err)
		}
		if fm2.String() != out {
			t.Fatalf("print not stable: %q -> %q", out, fm2.String())
		}
		// evaluation of arbitrary formulas must never panic
		e, _ := New(&Workbook{Sheets: []*Sheet{NewSheet("s1", "Sheet1", 0, 0)}}, Options{Rand: func() float64 { return 0.5 }})
		e.Evaluate(s, "s1")
	})
}
