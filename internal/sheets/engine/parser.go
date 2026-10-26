package engine

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SyntaxError describes a formula that cannot be parsed. It unwraps to
// ErrFormulaSyntax (or ErrFormulaTooDeep for nesting violations).
type SyntaxError struct {
	Pos  int // byte offset in the formula body (after "=")
	Msg  string
	kind error
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("formula error at position %d: %s", e.Pos+1, e.Msg)
}

// Unwrap returns the sentinel category.
func (e *SyntaxError) Unwrap() error {
	if e.kind != nil {
		return e.kind
	}
	return ErrFormulaSyntax
}

type tokKind uint8

const (
	tEOF tokKind = iota
	tNumber
	tString
	tError
	tIdent
	tSheet
	tOp
	tLParen
	tRParen
	tComma
	tColon
	tLBrace
	tRBrace
	tSemicolon
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '.' || r == '$' || r == '\\' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func lex(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			st := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if i < len(s) && s[i] == '.' {
				i++
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
			}
			if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
				j := i + 1
				if j < len(s) && (s[j] == '+' || s[j] == '-') {
					j++
				}
				if j < len(s) && s[j] >= '0' && s[j] <= '9' {
					for j < len(s) && s[j] >= '0' && s[j] <= '9' {
						j++
					}
					i = j
				}
			}
			toks = append(toks, token{tNumber, s[st:i], st})
		case c == '"':
			st := i
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, &SyntaxError{Pos: st, Msg: "unterminated string"}
			}
			toks = append(toks, token{tString, b.String(), st})
		case c == '\'':
			st := i
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed || i >= len(s) || s[i] != '!' {
				return nil, &SyntaxError{Pos: st, Msg: "quoted sheet name must be followed by '!'"}
			}
			i++
			if b.Len() == 0 {
				return nil, &SyntaxError{Pos: st, Msg: "empty sheet name"}
			}
			toks = append(toks, token{tSheet, b.String(), st})
		case c == '#':
			st := i
			matched := ""
			for _, code := range errorCodes {
				if len(s)-i >= len(code) && strings.EqualFold(s[i:i+len(code)], code) {
					if len(code) > len(matched) {
						matched = code
					}
				}
			}
			if matched == "" {
				return nil, &SyntaxError{Pos: st, Msg: "unknown error literal"}
			}
			i += len(matched)
			toks = append(toks, token{tError, matched, st})
		case c == '(':
			toks = append(toks, token{tLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")", i})
			i++
		case c == ',':
			toks = append(toks, token{tComma, ",", i})
			i++
		case c == ':':
			toks = append(toks, token{tColon, ":", i})
			i++
		case c == '{':
			toks = append(toks, token{tLBrace, "{", i})
			i++
		case c == '}':
			toks = append(toks, token{tRBrace, "}", i})
			i++
		case c == ';':
			toks = append(toks, token{tSemicolon, ";", i})
			i++
		case c == '<' || c == '>':
			if i+1 < len(s) && (s[i+1] == '=' || (c == '<' && s[i+1] == '>')) {
				toks = append(toks, token{tOp, s[i : i+2], i})
				i += 2
			} else {
				toks = append(toks, token{tOp, s[i : i+1], i})
				i++
			}
		case strings.IndexByte("+-*/^&=%", c) >= 0:
			toks = append(toks, token{tOp, s[i : i+1], i})
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				return nil, &SyntaxError{Pos: i, Msg: "invalid UTF-8"}
			}
			if !isIdentRune(r) {
				return nil, &SyntaxError{Pos: i, Msg: fmt.Sprintf("unexpected character %q", r)}
			}
			st := i
			for i < len(s) {
				r, size := utf8.DecodeRuneInString(s[i:])
				if !isIdentRune(r) {
					break
				}
				i += size
			}
			if i < len(s) && s[i] == '!' {
				toks = append(toks, token{tSheet, s[st:i], st})
				i++
			} else {
				toks = append(toks, token{tIdent, s[st:i], st})
			}
		}
	}
	toks = append(toks, token{tEOF, "", len(s)})
	return toks, nil
}

type parser struct {
	toks  []token
	pos   int
	depth int
}

// ParseFormula parses a formula with or without the leading "=".
func ParseFormula(src string) (*Formula, error) {
	if utf8.RuneCountInString(src) > MaxCellChars {
		return nil, ErrCellTooLong
	}
	body := strings.TrimPrefix(src, "=")
	toks, err := lex(body)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tEOF {
		return nil, &SyntaxError{Pos: 0, Msg: "empty formula"}
	}
	root, err := p.expr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, &SyntaxError{Pos: t.pos, Msg: fmt.Sprintf("unexpected %q", t.text)}
	}
	return &Formula{Root: root}, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > MaxFormulaDepth {
		return &SyntaxError{Pos: pos, Msg: fmt.Sprintf("nesting deeper than %d levels", MaxFormulaDepth), kind: ErrFormulaTooDeep}
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) isOp(ops ...string) (string, bool) {
	t := p.peek()
	if t.kind != tOp {
		return "", false
	}
	for _, o := range ops {
		if t.text == o {
			return o, true
		}
	}
	return "", false
}

var binOps = map[string]BinOp{
	"+": OpAdd, "-": OpSub, "*": OpMul, "/": OpDiv, "^": OpPow, "&": OpConcat,
	"=": OpEq, "<>": OpNe, "<": OpLt, ">": OpGt, "<=": OpLe, ">=": OpGe,
}

// Precedence (low → high): comparison, &, + -, * /, ^, %, unary -, primary.
func (p *parser) expr() (Expr, error) { return p.binary(0) }

var precLevels = [][]string{
	{"=", "<>", "<", ">", "<=", ">="},
	{"&"},
	{"+", "-"},
	{"*", "/"},
	{"^"},
}

func (p *parser) binary(level int) (Expr, error) {
	if level == len(precLevels) {
		return p.percent()
	}
	l, err := p.binary(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.isOp(precLevels[level]...)
		if !ok {
			return l, nil
		}
		p.next()
		r, err := p.binary(level + 1)
		if err != nil {
			return nil, err
		}
		l = &BinaryOp{Op: binOps[op], L: l, R: r}
	}
}

func (p *parser) percent() (Expr, error) {
	x, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		if _, ok := p.isOp("%"); !ok {
			return x, nil
		}
		p.next()
		x = &PercentOp{X: x}
	}
}

func (p *parser) unary() (Expr, error) {
	if op, ok := p.isOp("-", "+"); ok {
		t := p.next()
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		x, err := p.unary()
		p.leave()
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Op: op[0], X: x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (Expr, error) {
	t := p.next()
	switch t.kind {
	case tNumber:
		if p.peek().kind == tColon {
			return p.rowRange("", t)
		}
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "number out of range"}
		}
		return &NumberLit{Val: f, Raw: t.text}, nil
	case tString:
		return &StringLit{Val: t.text}, nil
	case tError:
		return &ErrorLit{Code: t.text}, nil
	case tSheet:
		n := p.next()
		switch n.kind {
		case tIdent:
			return p.refOrName(t.text, n)
		case tNumber:
			return p.rowRange(t.text, n)
		case tError:
			if n.text == CodeRef {
				return &ErrorLit{Code: CodeRef}, nil
			}
		}
		return nil, &SyntaxError{Pos: n.pos, Msg: "expected a reference after sheet name"}
	case tIdent:
		if p.peek().kind == tLParen {
			return p.call(t)
		}
		return p.refOrName("", t)
	case tLParen:
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		x, err := p.expr()
		p.leave()
		if err != nil {
			return nil, err
		}
		if c := p.next(); c.kind != tRParen {
			return nil, &SyntaxError{Pos: c.pos, Msg: "expected ')'"}
		}
		return &ParenExpr{X: x}, nil
	case tLBrace:
		return p.array(t)
	case tEOF:
		return nil, &SyntaxError{Pos: t.pos, Msg: "unexpected end of formula"}
	}
	return nil, &SyntaxError{Pos: t.pos, Msg: fmt.Sprintf("unexpected %q", t.text)}
}

// stripFuncPrefix removes OOXML future-function prefixes (_xlfn., _xlws.).
func stripFuncPrefix(name string) string {
	for {
		u := strings.ToUpper(name)
		switch {
		case strings.HasPrefix(u, "_XLFN."):
			name = name[6:]
		case strings.HasPrefix(u, "_XLWS."):
			name = name[6:]
		default:
			return strings.ToUpper(name)
		}
	}
}

func validName(s string) bool {
	if s == "" || strings.Contains(s, "$") {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return r == '_' || r == '\\' || unicode.IsLetter(r)
}

func (p *parser) call(nameTok token) (Expr, error) {
	name := stripFuncPrefix(nameTok.text)
	if !validName(name) {
		return nil, &SyntaxError{Pos: nameTok.pos, Msg: fmt.Sprintf("invalid function name %q", nameTok.text)}
	}
	open := p.next() // '('
	if err := p.enter(open.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	call := &CallExpr{Name: name}
	if p.peek().kind == tRParen {
		p.next()
		return call, nil
	}
	for {
		var arg Expr
		if k := p.peek().kind; k == tComma || k == tRParen {
			arg = &MissingArg{}
		} else {
			var err error
			if arg, err = p.expr(); err != nil {
				return nil, err
			}
		}
		call.Args = append(call.Args, arg)
		if len(call.Args) > 255 {
			return nil, &SyntaxError{Pos: nameTok.pos, Msg: "too many arguments (max 255)"}
		}
		t := p.next()
		switch t.kind {
		case tComma:
			continue
		case tRParen:
			return call, nil
		}
		return nil, &SyntaxError{Pos: t.pos, Msg: "expected ',' or ')'"}
	}
}

func (p *parser) refOrName(sheet string, t token) (Expr, error) {
	text := t.text
	colon := p.peek().kind == tColon
	if row, col, ra, ca, ok := refParts(text); ok {
		ref := &CellRef{Sheet: sheet, Row: row, Col: col, AbsRow: ra, AbsCol: ca}
		if !colon {
			return ref, nil
		}
		p.next()
		t2 := p.secondPart(sheet)
		row2, col2, ra2, ca2, ok2 := refParts(t2.text)
		if t2.kind != tIdent || !ok2 {
			return nil, &SyntaxError{Pos: t2.pos, Msg: "invalid range end"}
		}
		return normRange(&RangeRef{Sheet: sheet, Kind: RangeCells, R1: row, C1: col, R2: row2, C2: col2,
			AbsR1: ra, AbsC1: ca, AbsR2: ra2, AbsC2: ca2}), nil
	}
	if colon {
		if c1, a1, ok := colPart(text); ok {
			p.next()
			t2 := p.secondPart(sheet)
			c2, a2, ok2 := colPart(t2.text)
			if t2.kind != tIdent || !ok2 {
				return nil, &SyntaxError{Pos: t2.pos, Msg: "invalid column range"}
			}
			return normRange(&RangeRef{Sheet: sheet, Kind: RangeCols, C1: c1, C2: c2, AbsC1: a1, AbsC2: a2}), nil
		}
		if _, _, ok := rowPart(text); ok {
			return p.rowRange(sheet, t)
		}
	}
	if sheet != "" {
		return nil, &SyntaxError{Pos: t.pos, Msg: fmt.Sprintf("invalid reference %q", text)}
	}
	switch strings.ToUpper(text) {
	case "TRUE":
		return &BoolLit{Val: true}, nil
	case "FALSE":
		return &BoolLit{Val: false}, nil
	}
	if !validName(text) {
		return nil, &SyntaxError{Pos: t.pos, Msg: fmt.Sprintf("invalid name %q", text)}
	}
	return &NameExpr{Name: text}, nil
}

// secondPart reads the token after ':' allowing a repeated sheet prefix
// (Sheet1!A1:Sheet1!B2). A different sheet (3-D reference) is rejected by
// returning a token that fails validation.
func (p *parser) secondPart(sheet string) token {
	t := p.next()
	if t.kind == tSheet {
		if !strings.EqualFold(t.text, sheet) {
			return token{kind: tEOF, pos: t.pos}
		}
		t = p.next()
	}
	return t
}

func (p *parser) rowRange(sheet string, t token) (Expr, error) {
	r1, a1, ok := rowPart(t.text)
	if !ok {
		return nil, &SyntaxError{Pos: t.pos, Msg: "invalid row reference"}
	}
	if p.next().kind != tColon {
		return nil, &SyntaxError{Pos: t.pos, Msg: "expected ':'"}
	}
	t2 := p.secondPart(sheet)
	if t2.kind != tNumber && t2.kind != tIdent {
		return nil, &SyntaxError{Pos: t2.pos, Msg: "invalid row range"}
	}
	r2, a2, ok := rowPart(t2.text)
	if !ok {
		return nil, &SyntaxError{Pos: t2.pos, Msg: "invalid row range"}
	}
	return normRange(&RangeRef{Sheet: sheet, Kind: RangeRows, R1: r1, R2: r2, AbsR1: a1, AbsR2: a2}), nil
}

func normRange(r *RangeRef) *RangeRef {
	if r.R1 > r.R2 {
		r.R1, r.R2 = r.R2, r.R1
		r.AbsR1, r.AbsR2 = r.AbsR2, r.AbsR1
	}
	if r.C1 > r.C2 {
		r.C1, r.C2 = r.C2, r.C1
		r.AbsC1, r.AbsC2 = r.AbsC2, r.AbsC1
	}
	return r
}

func (p *parser) array(open token) (Expr, error) {
	if err := p.enter(open.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	arr := &ArrayLit{}
	row := []Expr{}
	total := 0
	for {
		el, err := p.arrayElem()
		if err != nil {
			return nil, err
		}
		row = append(row, el)
		total++
		if total > 65536 {
			return nil, &SyntaxError{Pos: open.pos, Msg: "array constant too large"}
		}
		t := p.next()
		switch t.kind {
		case tComma:
			continue
		case tSemicolon, tRBrace:
			if len(arr.Rows) > 0 && len(row) != len(arr.Rows[0]) {
				return nil, &SyntaxError{Pos: t.pos, Msg: "array rows must have equal length"}
			}
			arr.Rows = append(arr.Rows, row)
			row = []Expr{}
			if t.kind == tRBrace {
				return arr, nil
			}
		default:
			return nil, &SyntaxError{Pos: t.pos, Msg: "expected ',', ';' or '}'"}
		}
	}
}

func (p *parser) arrayElem() (Expr, error) {
	t := p.next()
	sign := ""
	if t.kind == tOp && (t.text == "-" || t.text == "+") {
		sign = t.text
		t = p.next()
		if t.kind != tNumber {
			return nil, &SyntaxError{Pos: t.pos, Msg: "expected number in array"}
		}
	}
	switch t.kind {
	case tNumber:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "number out of range"}
		}
		if sign == "-" {
			f = -f
		}
		raw := t.text
		if sign == "-" {
			raw = "-" + raw
		}
		return &NumberLit{Val: f, Raw: raw}, nil
	case tString:
		return &StringLit{Val: t.text}, nil
	case tError:
		return &ErrorLit{Code: t.text}, nil
	case tIdent:
		switch strings.ToUpper(t.text) {
		case "TRUE":
			return &BoolLit{Val: true}, nil
		case "FALSE":
			return &BoolLit{Val: false}, nil
		}
	}
	return nil, &SyntaxError{Pos: t.pos, Msg: "array constants may only contain literals"}
}
