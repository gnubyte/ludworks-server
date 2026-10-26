package engine

import (
	"math"
	"testing"
	"time"
)

// errv marks an expected error value in test tables.
type errv string

func mustRef(t testing.TB, a1 string) Ref {
	t.Helper()
	r, err := ParseRef(a1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var fixedNow = time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	return Options{Now: func() time.Time { return fixedNow }, Rand: func() float64 { return 0.25 }}
}

// fixture builds a two-sheet workbook used by the function tests.
//
//	   A  B           C      D       E   F        G
//	1  1  apple       TRUE   "5"     10  ten      45306 (2024-01-15)
//	2  2  Banana      FALSE  ""      20  twenty   0.5
//	3  3  cherry      text   #N/A    30  thirty
//	4  4  date        (blank) -2.5
//	5  5  apple pie   0      10
func fixture(t testing.TB) *Engine {
	t.Helper()
	s1 := NewSheet("s1", "Sheet1", 0, 0)
	s2 := NewSheet("s2", "Sheet2", 0, 0)
	set := func(s *Sheet, a1 string, v Value) { s.SetCell(mustRef(t, a1), &Cell{Value: v}) }
	for i := 1; i <= 5; i++ {
		set(s1, "A"+itoa(i), NumberValue(float64(i)))
	}
	for i, v := range []string{"apple", "Banana", "cherry", "date", "apple pie"} {
		set(s1, "B"+itoa(i+1), StringValue(v))
	}
	set(s1, "C1", BoolValue(true))
	set(s1, "C2", BoolValue(false))
	set(s1, "C3", StringValue("text"))
	set(s1, "C5", NumberValue(0))
	set(s1, "D1", StringValue("5"))
	set(s1, "D2", StringValue(""))
	set(s1, "D3", ErrorValue(CodeNA))
	set(s1, "D4", NumberValue(-2.5))
	set(s1, "D5", NumberValue(10))
	set(s1, "E1", NumberValue(10))
	set(s1, "E2", NumberValue(20))
	set(s1, "E3", NumberValue(30))
	set(s1, "F1", StringValue("ten"))
	set(s1, "F2", StringValue("twenty"))
	set(s1, "F3", StringValue("thirty"))
	set(s1, "G1", NumberValue(45306))
	set(s1, "G2", NumberValue(0.5))
	set(s2, "A1", NumberValue(100))
	set(s2, "B1", StringValue("x"))
	e, err := New(&Workbook{Sheets: []*Sheet{s1, s2}}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func checkValue(t *testing.T, label string, got Value, want any) {
	t.Helper()
	switch w := want.(type) {
	case errv:
		if got.Kind != KindError || got.Err != string(w) {
			t.Errorf("%s = %v (%v), want error %s", label, got, got.Kind, w)
		}
	case int:
		checkValue(t, label, got, float64(w))
	case float64:
		if got.Kind != KindNumber || math.Abs(got.Num-w) > 1e-9*math.Max(1, math.Abs(w)) {
			t.Errorf("%s = %v (%v), want %v", label, got, got.Kind, w)
		}
	case string:
		if got.Kind != KindString || got.Str != w {
			t.Errorf("%s = %q (%v), want %q", label, got.Text(), got.Kind, w)
		}
	case bool:
		if got.Kind != KindBool || got.Bool != w {
			t.Errorf("%s = %v (%v), want %v", label, got, got.Kind, w)
		}
	default:
		t.Fatalf("bad expectation type %T", want)
	}
}

type evalCase struct {
	f    string
	want any
}

func runEvalCases(t *testing.T, e *Engine, cases []evalCase) {
	t.Helper()
	for _, c := range cases {
		got, err := e.Evaluate(c.f, "s1")
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.f, err)
			continue
		}
		checkValue(t, c.f, got, c.want)
	}
}

func TestOperatorsAndCoercion(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{"=1+2", 3}, {"=A1+A2", 3}, {`="3"+1`, 4}, {`="abc"+1`, errv(CodeValue)}, {"=TRUE+1", 2},
		{"=C4+1", 1}, {"=C4", 0}, {"=1/0", errv(CodeDiv0)}, {"=D3+1", errv(CodeNA)}, {"=1/0+D3", errv(CodeDiv0)},
		{"=2^10", 1024}, {"=-2^2", 4}, {"=2^-1", 0.5}, {"=0^0", errv(CodeNum)}, {"=0^-1", errv(CodeDiv0)},
		{"=(-8)^(1/3)", errv(CodeNum)}, {"=50%", 0.5}, {"=10*10%", 1}, {"=1-2-3", -4}, {"=2*3+4", 10},
		{`="a"&1&TRUE`, "a1TRUE"}, {`=1/3&""`, "0.333333333333333"}, {`=1E+15&""`, "1E+15"},
		{`=0.1+0.2&""`, "0.3"}, {`=123456789012345678&""`, "1.23456789012346E+17"}, {`=1E-10&""`, "1E-10"},
		{`=0.0001&""`, "0.0001"}, {`=-0.5&""`, "-0.5"}, {`=C4&"x"`, "x"}, {`=D3&"x"`, errv(CodeNA)},
		{"=1<2", true}, {`="a"<"B"`, true}, {`="a"="A"`, true}, {`=1<"a"`, true}, {`="z"<TRUE`, true},
		{"=C4=0", true}, {`=C4=""`, true}, {"=C4=FALSE", true}, {"=D3=1", errv(CodeNA)}, {"=2>=2", true},
		{"=1<>1", false}, {`=D1=5`, false}, {"=+A1", 1}, {"=--C1", 1}, {"=1E308*10", errv(CodeNum)},
		{"=Sheet2!A1*2", 200}, {"='Sheet2'!B1", "x"}, {"=sheet2!a1", 100}, {"=Missing!A1", errv(CodeRef)},
		{"=Foo", errv(CodeName)}, {"=NOSUCHFN(1)", errv(CodeName)}, {"=#REF!", errv(CodeRef)},
		{"=SUM({1,2;3,4})", 10}, {"=SUM({1,2}*{3,4})", 11}, {"=SUM(A1:A3*{1;2;3})", 14}, {"={1,2}+{1;2}", 2},
		{"=SUM(A1:A2+A1:A3)", errv(CodeNA)}, {"=A1:A3", 1}, {"=SUM(A:A*2)", 30}, {"=SUM(A:A+1)", 1048591},
		{"=SUMPRODUCT((A:A>2)*1)", 3}, {`=SUMPRODUCT((B:B="apple")*(A:A))`, 1}, {`=COUNTA(A:A&"")`, 1048576},
		{`=REPT("x",30000)&REPT("y",3000)`, errv(CodeValue)},
	})
}

func TestMathFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{"=SUM(A1:A5)", 15}, {"=SUM(A1:A5,10)", 25}, {"=SUM(B1:B5)", 0}, {`=SUM("3",2)`, 5}, {`=SUM("x")`, errv(CodeValue)},
		{"=SUM(A1:D5)", errv(CodeNA)}, {"=SUM(C1:C2)", 0}, {"=SUM(TRUE,1)", 2}, {"=SUM(A:A)", 15}, {"=SUM(1:1)", 45317},
		{"=SUM(,)", 0}, {"=PRODUCT(A1:A5)", 120}, {"=PRODUCT(B1:B2)", 0}, {"=SUMSQ(1,2,3)", 14},
		{"=ROUND(2.675,2)", 2.68}, {"=ROUND(-2.5,0)", -3}, {"=ROUND(1234.5678,-2)", 1200}, {"=ROUND(0.5,0)", 1},
		{"=ROUND(1.005,2)", 1.01}, {"=ROUND(1E-20,25)", 1e-20}, {"=ROUND(5,-400)", 0}, {"=ROUNDUP(3.2,0)", 4},
		{"=ROUNDUP(-3.2,0)", -4}, {"=ROUNDUP(3.14159,3)", 3.142}, {"=ROUNDDOWN(3.9,0)", 3}, {"=ROUNDDOWN(-3.9,0)", -3},
		{"=INT(-3.5)", -4}, {"=TRUNC(-3.5)", -3}, {"=TRUNC(3.14159,2)", 3.14}, {"=ABS(-3)", 3}, {"=SQRT(16)", 4},
		{"=SQRT(-1)", errv(CodeNum)}, {"=POWER(2,3)", 8}, {"=MOD(10,3)", 1}, {"=MOD(-10,3)", 2}, {"=MOD(10,-3)", -2},
		{"=MOD(5,0)", errv(CodeDiv0)}, {"=MOD(5.5,1)", 0.5}, {"=QUOTIENT(7,2)", 3}, {"=QUOTIENT(-7,2)", -3},
		{"=PI()", math.Pi}, {"=SIGN(-5)", -1}, {"=SIGN(0)", 0}, {"=EXP(0)", 1}, {"=LN(1)", 0}, {"=LN(0)", errv(CodeNum)},
		{"=LOG(100)", 2}, {"=LOG(8,2)", 3}, {"=LOG(8,1)", errv(CodeDiv0)}, {"=LOG10(1000)", 3},
		{"=CEILING(2.5,1)", 3}, {"=CEILING(-2.5,-2)", -4}, {"=CEILING(-2.5,2)", -2}, {"=CEILING(2.5,-1)", errv(CodeNum)},
		{"=CEILING(0.3,0.1)", 0.3}, {"=FLOOR(2.5,1)", 2}, {"=FLOOR(-2.5,-2)", -2}, {"=FLOOR(-2.5,2)", -4},
		{"=FLOOR(1,0)", errv(CodeDiv0)}, {"=MROUND(10,3)", 9}, {"=MROUND(11,3)", 12}, {"=MROUND(-10,3)", errv(CodeNum)},
		{"=EVEN(1.5)", 2}, {"=EVEN(3)", 4}, {"=EVEN(-1)", -2}, {"=ODD(1.5)", 3}, {"=ODD(2)", 3}, {"=ODD(-2)", -3},
		{"=ODD(0)", 1}, {"=FACT(5)", 120}, {"=FACT(-1)", errv(CodeNum)}, {"=SUMPRODUCT(A1:A3,E1:E3)", 140},
		{"=SUMPRODUCT((A1:A5>2)*A1:A5)", 12}, {"=SUMPRODUCT(A1:A2,A1:A3)", errv(CodeValue)}, {"=SUMPRODUCT(B1:B3)", 0},
		{"=SIN(0)", 0}, {"=COS(0)", 1}, {"=TAN(0)", 0}, {"=ASIN(1)", math.Pi / 2}, {"=ACOS(1)", 0}, {"=ATAN(1)", math.Pi / 4},
		{"=ASIN(2)", errv(CodeNum)}, {"=ATAN2(1,1)", math.Pi / 4}, {"=ATAN2(0,0)", errv(CodeDiv0)},
		{"=DEGREES(PI())", 180}, {"=RADIANS(180)", math.Pi}, {"=RAND()", 0.25}, {"=RANDBETWEEN(1,10)", 3},
		{"=RANDBETWEEN(5,1)", errv(CodeNum)}, {"=ABS(A1:A3)", 1}, {"=SUM(ABS(D4:D5))", 12.5},
	})
}

func TestStatFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{"=AVERAGE(A1:A5)", 3}, {"=AVERAGE(B1:B5)", errv(CodeDiv0)}, {"=AVERAGE(A1:A5,C1)", 3}, {"=AVERAGE(1,TRUE)", 1},
		{"=MIN(A1:A5)", 1}, {"=MAX(A1:A5,7)", 7}, {"=MIN(B1:B2)", 0}, {"=MAX(D4:D5)", 10}, {"=MIN(D4:D5)", -2.5},
		{"=MAX(D1:D5)", errv(CodeNA)}, {"=COUNT(A1:D5)", 8}, {`=COUNT(1,"2","x",TRUE)`, 3}, {"=COUNT(D3)", 0},
		{"=COUNTA(A1:D5)", 19}, {"=COUNTA(1,,\"\")", 2}, {"=COUNTBLANK(C1:D5)", 2}, {"=COUNTBLANK(A6:A10)", 5},
		{"=COUNTBLANK(A:A)", 1048571}, {"=MEDIAN(A1:A4)", 2.5}, {"=MEDIAN(5,1,3)", 3}, {"=MEDIAN(B1:B2)", errv(CodeNum)},
		{"=MODE(1,2,2,3)", 2}, {"=MODE(1,2,3)", errv(CodeNA)}, {"=MODE.SNGL(3,3,1,1)", 3},
		{"=STDEV(A1:A5)", 1.5811388300841898}, {"=STDEV.S(A1:A5)", 1.5811388300841898}, {"=STDEV.P(A1:A5)", 1.4142135623730951},
		{"=STDEVP(A1:A5)", 1.4142135623730951}, {"=VAR(A1:A5)", 2.5}, {"=VAR.S(A1:A5)", 2.5}, {"=VAR.P(A1:A5)", 2}, {"=VARP(A1:A5)", 2},
		{"=STDEV(1)", errv(CodeDiv0)}, {"=LARGE(A1:A5,2)", 4}, {"=SMALL(A1:A5,2)", 2}, {"=LARGE(A1:A5,6)", errv(CodeNum)},
		{"=SMALL({3,1,2},1)", 1}, {"=RANK(4,A1:A5)", 2}, {"=RANK(4,A1:A5,1)", 4}, {"=RANK.EQ(1,A1:A5)", 5},
		{"=RANK(9,A1:A5)", errv(CodeNA)},
	})
}

func TestCriteriaFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{`=COUNTIF(A1:A5,">2")`, 3}, {"=COUNTIF(A1:A5,3)", 1}, {`=COUNTIF(A1:A5,"<>3")`, 4}, {`=COUNTIF(A1:A5,">=2")`, 4},
		{`=COUNTIF(A1:A5,"<=2")`, 2}, {`=COUNTIF(A1:A5,"=3")`, 1}, {`=COUNTIF(B1:B5,"apple")`, 1},
		{`=COUNTIF(B1:B5,"APPLE*")`, 2}, {`=COUNTIF(B1:B5,"?anana")`, 1}, {`=COUNTIF(B1:B5,"<>apple")`, 4},
		{`=COUNTIF(B1:B5,"*e")`, 3}, {`=COUNTIF(B1:B5,"~*")`, 0}, {`=COUNTIF(B1:B5,">c")`, 2}, {"=COUNTIF(C1:C5,TRUE)", 1},
		{`=COUNTIF(C1:C5,"true")`, 1}, {`=COUNTIF(C1:D5,"")`, 2}, {`=COUNTIF(C1:D5,"=")`, 1}, {`=COUNTIF(C1:D5,"<>")`, 9},
		{"=COUNTIF(D1:D5,5)", 1}, {`=COUNTIF(A1:A10,"")`, 5}, {`=COUNTIF(A:A,"<>")`, 5}, {`=COUNTIF(A:A,"")`, 1048571},
		{`=COUNTIF(D1:D5,"#N/A")`, 1}, {`=COUNTIF(G1:G2,">2024-01-01")`, 1}, {`=COUNTIF(A1:A5,">"&A3)`, 2},
		{"=COUNTIF(A1:A5,D3)", errv(CodeNA)}, {`=COUNTIF(5,">1")`, errv(CodeValue)},
		{`=COUNTIFS(A1:A5,">1",A1:A5,"<5")`, 3}, {`=COUNTIFS(A1:A5,">1",B1:B4,"x")`, errv(CodeValue)},
		{`=SUMIF(A1:A5,">2")`, 12}, {`=SUMIF(B1:B5,"apple*",A1:A5)`, 6}, {`=SUMIF(B1:B5,"apple*",A1)`, 6},
		{`=SUMIF(C1:D5,"",A1:B5)`, 4},
		{`=SUMIFS(A1:A5,B1:B5,"*a*",A1:A5,">1")`, 11}, {`=SUMIFS(A1:A5,B1:B4,"x")`, errv(CodeValue)},
		{`=AVERAGEIF(A1:A5,">2")`, 4}, {`=AVERAGEIF(A1:A5,">10")`, errv(CodeDiv0)}, {`=AVERAGEIF(B1:B5,"apple*",A1:A5)`, 3},
		{`=AVERAGEIFS(A1:A5,A1:A5,">=2",A1:A5,"<=4")`, 3}, {`=MAXIFS(A1:A5,B1:B5,"apple*")`, 5},
		{`=MINIFS(A1:A5,B1:B5,"apple*")`, 1}, {`=MAXIFS(A1:A5,B1:B5,"zzz")`, 0}, {`=SUMIF(D1:D5,">0",D1:D5)`, 10}, {`=SUMIF(D1:D5,"<>1",D1:D5)`, errv(CodeNA)},
	})
}

func TestLogicalAndInfoFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{`=IF(A1>0,"pos","neg")`, "pos"}, {"=IF(FALSE,1)", false}, {"=IF(TRUE,,1)", 0}, {`=IF("x",1,2)`, errv(CodeValue)},
		{"=IF(D3,1,2)", errv(CodeNA)}, {"=IF(C4,1,2)", 2}, {`=IF("true",1,2)`, 1}, {"=SUM(IF(A1:A5>2,A1:A5,0))", 12},
		{"=IF(TRUE,1,1/0)", 1}, {"=IF(FALSE,1/0,2)", 2},
		{`=IFS(A1>5,"a",A1>0,"b")`, "b"}, {"=IFS(FALSE,1)", errv(CodeNA)}, {"=IFS(TRUE,1,FALSE)", errv(CodeValue)},
		{`=IFERROR(1/0,"err")`, "err"}, {`=IFERROR(5,"err")`, 5}, {`=IFNA(D3,"na")`, "na"}, {`=IFNA(1/0,"na")`, errv(CodeDiv0)},
		{`=SUM(IFERROR(D1:D5*1,0))`, 12.5},
		{`=SWITCH(2,1,"one",2,"two","other")`, "two"}, {`=SWITCH(9,1,"one","other")`, "other"}, {`=SWITCH(9,1,"one")`, errv(CodeNA)},
		{`=SWITCH("B","a",1,"b",2)`, 2},
		{`=CHOOSE(2,"a","b","c")`, "b"}, {`=CHOOSE(4,"a")`, errv(CodeValue)}, {"=SUM(CHOOSE(1,A1:A5,E1:E3))", 15},
		{"=AND(TRUE,1)", true}, {"=AND(TRUE,0)", false}, {"=AND(A1:A5)", true}, {"=AND(B1:B2)", errv(CodeValue)},
		{`=AND("x")`, errv(CodeValue)}, {`=AND("true",1)`, true}, {"=AND(TRUE,D3)", errv(CodeNA)},
		{"=OR(FALSE,C1)", true}, {"=OR(C2,0)", false}, {"=XOR(TRUE,TRUE,TRUE)", true}, {"=XOR(TRUE,TRUE)", false},
		{"=NOT(0)", true}, {"=NOT(A1)", false}, {"=TRUE()", true}, {"=FALSE()", false},
		{"=ISBLANK(C4)", true}, {"=ISBLANK(D2)", false}, {"=ISNUMBER(A1)", true}, {"=ISNUMBER(D1)", false},
		{"=ISTEXT(B1)", true}, {"=ISNONTEXT(A1)", true}, {"=ISNONTEXT(C4)", true}, {"=ISLOGICAL(C1)", true},
		{"=ISERROR(D3)", true}, {"=ISERR(D3)", false}, {"=ISERR(1/0)", true}, {"=ISNA(D3)", true}, {"=ISEVEN(4)", true},
		{"=ISODD(3.7)", true}, {`=ISEVEN("x")`, errv(CodeValue)}, {"=ISREF(A1)", true}, {"=ISREF(1)", false},
		{"=NA()", errv(CodeNA)}, {"=ERROR.TYPE(1/0)", 2}, {"=ERROR.TYPE(1)", errv(CodeNA)}, {`=TYPE("a")`, 2},
		{"=TYPE(A1:A2)", 64}, {"=TYPE(C1)", 4}, {"=TYPE(D3)", 16}, {"=TYPE(1)", 1},
		{"=N(TRUE)", 1}, {`=N("a")`, 0}, {"=T(1)", ""}, {`=T("a")`, "a"},
		{"=ROW()", 1}, {"=ROW(C5)", 5}, {"=COLUMN(C5)", 3}, {"=COLUMN()", 1}, {"=ROW(1)", errv(CodeValue)},
		{"=ROWS(A1:B5)", 5}, {"=COLUMNS(A1:B5)", 2}, {"=ROWS({1,2;3,4})", 2}, {"=COLUMN(B:C)", 2}, {"=ROWS(A:A)", 1048576},
	})
}

func TestTextFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{`=CONCAT(B1:B2,"!")`, "appleBanana!"}, {`=CONCATENATE("a",1,TRUE)`, "a1TRUE"}, {`=CONCAT(D2:D3)`, errv(CodeNA)},
		{`=TEXTJOIN("-",TRUE,A1:A3)`, "1-2-3"}, {`=TEXTJOIN(",",FALSE,C3:C5)`, "text,,0"}, {`=TEXTJOIN(",",TRUE,C3:C5)`, "text,0"},
		{`=TEXTJOIN(",",FALSE,A1:A2,"x",{1,2})`, "1,2,x,1,2"}, {`=TEXTJOIN(",",FALSE,A:A)`, errv(CodeValue)},
		{`=LEN("héllo")`, 5}, {"=LEN(A1)", 1}, {"=LEN(C4)", 0}, {`=UPPER("abc")`, "ABC"}, {`=LOWER("ÀBC")`, "àbc"},
		{`=PROPER("hello wORLD-foo")`, "Hello World-Foo"}, {`=TRIM("  a   b  ")`, "a b"},
		{`=LEFT("hello",2)`, "he"}, {`=LEFT("hello")`, "h"}, {`=LEFT("hello",-1)`, errv(CodeValue)}, {`=RIGHT("hello",3)`, "llo"},
		{`=RIGHT("hi",10)`, "hi"}, {`=MID("hello",2,3)`, "ell"}, {`=MID("hello",10,2)`, ""}, {`=MID("hello",0,1)`, errv(CodeValue)},
		{`=MID("日本語テキスト",2,2)`, "本語"},
		{`=SUBSTITUTE("a-b-c","-","+")`, "a+b+c"}, {`=SUBSTITUTE("a-b-c","-","+",2)`, "a-b+c"}, {`=SUBSTITUTE("a-b-c","-","+",5)`, "a-b-c"},
		{`=SUBSTITUTE("abc","","x")`, "abc"}, {`=REPLACE("abcdef",2,3,"XY")`, "aXYef"},
		{`=FIND("b","abcb")`, 2}, {`=FIND("B","abc")`, errv(CodeValue)}, {`=FIND("b","abcb",3)`, 4}, {`=FIND("","abc")`, 1},
		{`=FIND("語","日本語")`, 3}, {`=SEARCH("B","abc")`, 2}, {`=SEARCH("c?","abcde")`, 3}, {`=SEARCH("x","abc")`, errv(CodeValue)},
		{`=SEARCH("b*d","abcde")`, 2},
		{`=TEXT(1234.567,"#,##0.00")`, "1,234.57"}, {`=TEXT(0.256,"0.0%")`, "25.6%"}, {`=TEXT(45306,"yyyy-mm-dd")`, "2024-01-15"},
		{`=TEXT(45306,"dddd, mmmm d")`, "Monday, January 15"}, {`=TEXT("abc","@")`, "abc"}, {`=TEXT(-1,"yyyy")`, errv(CodeValue)},
		{`=TEXT("12","0.00")`, "12.00"}, {`=TEXT(TRUE,"0")`, "TRUE"},
		{`=VALUE("1,234.5")`, 1234.5}, {`=VALUE("50%")`, 0.5}, {`=VALUE("abc")`, errv(CodeValue)}, {`=VALUE("2024-01-15")`, 45306},
		{`=VALUE("($5)")`, -5}, {`=VALUE("1e3")`, 1000}, {`=VALUE(TRUE)`, errv(CodeValue)}, {`=VALUE("12:00")`, 0.5},
		{`=REPT("ab",3)`, "ababab"}, {`=REPT("a",40000)`, errv(CodeValue)}, {`=REPT("a",0)`, ""},
		{`=EXACT("a","A")`, false}, {`=EXACT("a","a")`, true}, {"=CHAR(65)", "A"}, {"=CHAR(128)", "€"}, {"=CHAR(0)", errv(CodeValue)},
		{`=CODE("A")`, 65}, {`=CODE("€")`, 128}, {`=CODE("")`, errv(CodeValue)}, {"=UNICHAR(9731)", "☃"}, {`=UNICODE("☃")`, 9731},
		{`=CLEAN("a"&CHAR(9)&"b")`, "ab"},
	})
}

func TestDateFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{"=DATE(2024,1,15)", 45306}, {"=DATE(2024,13,1)", 45658}, {"=DATE(2024,3,0)", 45351}, {"=DATE(1900,2,29)", 60},
		{"=DATE(1900,3,1)", 61}, {"=DATE(1900,1,1)", 1}, {"=DATE(99,1,1)", 36161}, {"=DATE(10000,1,1)", errv(CodeNum)},
		{"=DATE(1899,12,31)=DATE(3799,12,31)", true}, {"=DATE(-1,1,1)", errv(CodeNum)}, {"=DATE(1900,1,0)", 0},
		{"=YEAR(45306)", 2024}, {"=MONTH(45306)", 1}, {"=DAY(45306)", 15}, {"=DAY(60)", 29}, {"=MONTH(60)", 2},
		{"=DAY(61)", 1}, {"=MONTH(61)", 3}, {"=YEAR(0)", 1900}, {"=DAY(0)", 0}, {"=YEAR(-1)", errv(CodeNum)},
		{`=YEAR("2024-06-01")`, 2024}, {"=WEEKDAY(45306)", 2}, {"=WEEKDAY(45306,2)", 1}, {"=WEEKDAY(45306,3)", 0},
		{"=WEEKDAY(1)", 1}, {"=WEEKDAY(45306,11)", 1}, {"=WEEKDAY(45306,9)", errv(CodeNum)},
		{"=TIME(12,30,0)", 0.5208333333333334}, {"=TIME(25,0,0)", 1.0 / 24}, {"=HOUR(0.75)", 18}, {"=MINUTE(TIME(1,2,3))", 2},
		{"=SECOND(TIME(1,2,3))", 3}, {"=HOUR(G2)", 12},
		{"=EDATE(DATE(2024,1,31),1)", 45351}, {"=EDATE(DATE(2024,3,31),-1)", 45351}, {"=EOMONTH(DATE(2024,1,15),0)", 45322},
		{"=EOMONTH(DATE(2024,1,15),-1)", 45291}, {"=DAYS(DATE(2024,3,1),DATE(2024,2,1))", 29},
		{`=DATEDIF(DATE(2020,1,15),DATE(2024,1,14),"Y")`, 3}, {`=DATEDIF(DATE(2020,1,15),DATE(2024,1,14),"M")`, 47},
		{`=DATEDIF(DATE(2020,1,15),DATE(2024,1,14),"D")`, 1460}, {`=DATEDIF(DATE(2024,1,10),DATE(2024,3,15),"MD")`, 5},
		{`=DATEDIF(DATE(2024,1,10),DATE(2024,3,15),"YM")`, 2}, {`=DATEDIF(DATE(2024,1,10),DATE(2025,3,15),"YD")`, 64},
		{`=DATEDIF(DATE(2024,1,10),DATE(2023,3,15),"D")`, errv(CodeNum)}, {`=DATEDIF(1,2,"X")`, errv(CodeNum)},
		{`=DATEVALUE("2024-01-15")`, 45306}, {`=DATEVALUE("1/15/2024")`, 45306}, {`=DATEVALUE("15-Jan-2024")`, 45306},
		{`=DATEVALUE("Jan 15, 2024")`, 45306}, {`=DATEVALUE("2024-02-30")`, errv(CodeValue)}, {`=DATEVALUE(5)`, errv(CodeValue)},
		{`=TIMEVALUE("6:00 PM")`, 0.75}, {`=TIMEVALUE("2024-01-15 06:00")`, 0.25},
		{"=WEEKNUM(DATE(2024,1,1))", 1}, {"=WEEKNUM(DATE(2024,1,7))", 2}, {"=WEEKNUM(DATE(2024,1,7),2)", 1},
		{"=NOW()", 45306.5}, {"=TODAY()", 45306},
	})
}

func TestLookupFunctions(t *testing.T) {
	runEvalCases(t, fixture(t), []evalCase{
		{"=VLOOKUP(20,E1:F3,2,FALSE)", "twenty"}, {"=VLOOKUP(25,E1:F3,2)", "twenty"}, {"=VLOOKUP(25,E1:F3,2,TRUE)", "twenty"},
		{"=VLOOKUP(5,E1:F3,2)", errv(CodeNA)}, {"=VLOOKUP(20,E1:F3,3,FALSE)", errv(CodeRef)}, {"=VLOOKUP(20,E1:F3,0)", errv(CodeValue)},
		{`=VLOOKUP("TW*",F1:F3,1,FALSE)`, "twenty"}, {"=VLOOKUP(99,E1:F3,2)", "thirty"}, {"=VLOOKUP(20,E1:F3,2,)", "twenty"},
		{"=VLOOKUP(D3,E1:F3,2)", errv(CodeNA)}, {"=VLOOKUP(10,E:F,2,FALSE)", "ten"}, {"=VLOOKUP(C4,E1:F3,2,FALSE)", errv(CodeNA)},
		{`=HLOOKUP(2,{1,2,3;"a","b","c"},2,FALSE)`, "b"}, {`=HLOOKUP(2.5,{1,2,3;"a","b","c"},2)`, "b"},
		{`=HLOOKUP(2,{1,2,3;"a","b","c"},3)`, errv(CodeRef)},
		{"=INDEX(E1:F3,2,2)", "twenty"}, {"=INDEX(A1:A5,3)", 3}, {"=INDEX({1,2,3},2)", 2}, {"=SUM(INDEX(E1:F3,0,1))", 60},
		{"=INDEX(A1:A5,9)", errv(CodeRef)}, {"=INDEX(A1:A5,-1)", errv(CodeValue)}, {"=SUM(INDEX(A1:E2,2,0))", 2 + 20},
		{"=INDEX(E1:F3,MATCH(30,E1:E3,0),2)", "thirty"},
		{"=MATCH(30,E1:E3,0)", 3}, {"=MATCH(25,E1:E3)", 2}, {"=MATCH(25,{30,20,10},-1)", 1}, {`=MATCH("b*",B1:B5,0)`, 2},
		{"=MATCH(99,E1:E3,0)", errv(CodeNA)}, {"=MATCH(5,E1:F3,0)", errv(CodeNA)}, {"=MATCH(1,{1,1,1,2})", 3},
		{`=MATCH("BANANA",B:B,0)`, 2},
		{"=XLOOKUP(20,E1:E3,F1:F3)", "twenty"}, {`=XLOOKUP(25,E1:E3,F1:F3,"none")`, "none"}, {"=XLOOKUP(25,E1:E3,F1:F3)", errv(CodeNA)},
		{"=XLOOKUP(25,E1:E3,F1:F3,,-1)", "twenty"}, {"=XLOOKUP(25,E1:E3,F1:F3,,1)", "thirty"}, {`=XLOOKUP("t*y",F1:F3,E1:E3,,2)`, 20},
		{`=XLOOKUP("t*y",F1:F3,E1:E3,,2,-1)`, 30}, {"=XLOOKUP(2,A1:A5,A1:A5,,0,-1)", 2}, {"=XLOOKUP(2,A1:A5,E1:E3)", errv(CodeValue)},
		{"=SUM(XLOOKUP(2,A1:A5,A1:B5))", 2},
	})
}

func TestArgCountValidation(t *testing.T) {
	e := fixture(t)
	for _, f := range []string{"=ROUND(1)", "=IF()", "=PI(1)", "=SUM()"} {
		if _, err := e.Evaluate(f, "s1"); err == nil {
			t.Errorf("%s: expected argument error", f)
		}
	}
}

func FuzzEvaluate(f *testing.F) {
	for _, s := range []string{"=SUM(A1:B9)*2", "=VLOOKUP(A1,A:C,2)", "=TEXT(A1,\"#,##0.00;[Red](0)\")", "=MID(B1,2,3)&REPT(\"x\",9)",
		"=COUNTIF(A:A,\"a*?~*\")", "=INDEX(A1:C3,0,2)", "=SUMPRODUCT((A:A>1)*B:B)", "=DATE(A1,B1,C1)"} {
		f.Add(s)
	}
	e := fixture(f)
	f.Fuzz(func(t *testing.T, s string) {
		fm, err := ParseFormula(s)
		if err != nil {
			return
		}
		ctx := &evalCtx{e: e, sheet: e.wb.Sheets[0]}
		v := finalValue(ctx.scalar(ctx.eval(fm.Root)))
		if v.Kind == KindNumber && (math.IsNaN(v.Num) || math.IsInf(v.Num, 0)) {
			t.Fatalf("%q produced non-finite %v", s, v.Num)
		}
	})
}

func TestPathologicalWildcardsAreBounded(t *testing.T) {
	e := fixture(t)
	start := time.Now()
	v, err := e.Evaluate(`=SEARCH("*a*a*a*a*a*a*b",REPT("a",30000))`, "s1")
	if err != nil || v.Kind != KindError {
		t.Errorf("got %v %v", v, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("pathological SEARCH took %v", time.Since(start))
	}
	if !wildMatch(compileWild("a*b*c"), "aXXbYYc") || wildMatch(compileWild("a*b*c"), "aXXbYY") {
		t.Errorf("wildMatch basics")
	}
}
