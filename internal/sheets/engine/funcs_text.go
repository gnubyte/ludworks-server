package engine

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

func init() {
	register(
		&funcDef{name: "CONCAT", syntax: "CONCAT(text1, [text2], ...)", desc: "Joins text from ranges and values.", min: 1, max: -1, eager: fnConcat},
		&funcDef{name: "CONCATENATE", syntax: "CONCATENATE(text1, [text2], ...)", desc: "Joins text values.", min: 1, max: -1, scalar: fnConcatenate},
		&funcDef{name: "TEXTJOIN", syntax: "TEXTJOIN(delimiter, ignore_empty, text1, [text2], ...)", desc: "Joins text with a delimiter.", min: 3, max: -1, eager: fnTextJoin},
		&funcDef{name: "LEN", syntax: "LEN(text)", desc: "Number of characters.", min: 1, max: 1, scalar: text1(func(s string) Value { return NumberValue(float64(textLen(s))) })},
		&funcDef{name: "UPPER", syntax: "UPPER(text)", desc: "Converts text to upper case.", min: 1, max: 1, scalar: text1(func(s string) Value { return StringValue(strings.ToUpper(s)) })},
		&funcDef{name: "LOWER", syntax: "LOWER(text)", desc: "Converts text to lower case.", min: 1, max: 1, scalar: text1(func(s string) Value { return StringValue(strings.ToLower(s)) })},
		&funcDef{name: "PROPER", syntax: "PROPER(text)", desc: "Capitalises the first letter of each word.", min: 1, max: 1, scalar: text1(func(s string) Value { return StringValue(proper(s)) })},
		&funcDef{name: "TRIM", syntax: "TRIM(text)", desc: "Removes leading/trailing spaces and collapses inner runs of spaces.", min: 1, max: 1, scalar: text1(func(s string) Value {
			return StringValue(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' }), " "))
		})},
		&funcDef{name: "CLEAN", syntax: "CLEAN(text)", desc: "Removes non-printable characters (codes 0-31).", min: 1, max: 1, scalar: text1(func(s string) Value {
			return StringValue(strings.Map(func(r rune) rune {
				if r < 32 {
					return -1
				}
				return r
			}, s))
		})},
		&funcDef{name: "LEFT", syntax: "LEFT(text, [num_chars])", desc: "Leftmost characters.", min: 1, max: 2, scalar: leftRight(true)},
		&funcDef{name: "RIGHT", syntax: "RIGHT(text, [num_chars])", desc: "Rightmost characters.", min: 1, max: 2, scalar: leftRight(false)},
		&funcDef{name: "MID", syntax: "MID(text, start_num, num_chars)", desc: "Characters from the middle of text.", min: 3, max: 3, scalar: fnMid},
		&funcDef{name: "SUBSTITUTE", syntax: "SUBSTITUTE(text, old_text, new_text, [instance_num])", desc: "Replaces occurrences of old_text.", min: 3, max: 4, scalar: fnSubstitute},
		&funcDef{name: "REPLACE", syntax: "REPLACE(old_text, start_num, num_chars, new_text)", desc: "Replaces characters by position.", min: 4, max: 4, scalar: fnReplace},
		&funcDef{name: "FIND", syntax: "FIND(find_text, within_text, [start_num])", desc: "Case-sensitive position of text (no wildcards).", min: 2, max: 3, scalar: findFn(false)},
		&funcDef{name: "SEARCH", syntax: "SEARCH(find_text, within_text, [start_num])", desc: "Case-insensitive position of text (wildcards * ?).", min: 2, max: 3, scalar: findFn(true)},
		&funcDef{name: "TEXT", syntax: "TEXT(value, format_text)", desc: "Formats a number as text.", min: 2, max: 2, scalar: fnText},
		&funcDef{name: "VALUE", syntax: "VALUE(text)", desc: "Converts text to a number.", min: 1, max: 1, scalar: fnValue},
		&funcDef{name: "REPT", syntax: "REPT(text, number_times)", desc: "Repeats text.", min: 2, max: 2, scalar: fnRept},
		&funcDef{name: "EXACT", syntax: "EXACT(text1, text2)", desc: "Case-sensitive text comparison.", min: 2, max: 2, scalar: func(_ *evalCtx, a []Value) Value {
			x, e := toText(a[0])
			if e != "" {
				return ErrorValue(e)
			}
			y, e := toText(a[1])
			if e != "" {
				return ErrorValue(e)
			}
			return BoolValue(x == y)
		}},
		&funcDef{name: "CHAR", syntax: "CHAR(number)", desc: "Character for a code 1-255 (Windows-1252).", min: 1, max: 1, scalar: num1(func(x float64) Value {
			n := int(math.Trunc(x))
			if n < 1 || n > 255 {
				return ErrorValue(CodeValue)
			}
			return StringValue(string(cp1252ToRune(byte(n))))
		})},
		&funcDef{name: "CODE", syntax: "CODE(text)", desc: "Code of the first character (Windows-1252).", min: 1, max: 1, scalar: text1(func(s string) Value {
			if s == "" {
				return ErrorValue(CodeValue)
			}
			r, _ := utf8.DecodeRuneInString(s)
			if b, ok := runeToCP1252(r); ok {
				return NumberValue(float64(b))
			}
			return NumberValue(63) // '?', as Excel does for unmappable characters
		})},
		&funcDef{name: "UNICHAR", syntax: "UNICHAR(number)", desc: "Unicode character for a code point.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			n := int(math.Trunc(math.Min(x, 0x10FFFF+1)))
			if n < 1 || n > 0x10FFFF || (n >= 0xD800 && n <= 0xDFFF) {
				return ErrorValue(CodeValue)
			}
			return StringValue(string(rune(n)))
		})},
		&funcDef{name: "UNICODE", syntax: "UNICODE(text)", desc: "Code point of the first character.", min: 1, max: 1, scalar: text1(func(s string) Value {
			if s == "" {
				return ErrorValue(CodeValue)
			}
			r, _ := utf8.DecodeRuneInString(s)
			return NumberValue(float64(r))
		})},
	)
}

func text1(f func(string) Value) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		s, e := toText(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		return f(s)
	}
}

// textResult enforces the 32767-character cell limit on produced text.
func textResult(s string) Value {
	if len(s) > MaxCellChars && textLen(s) > MaxCellChars {
		return ErrorValue(CodeValue)
	}
	return StringValue(s)
}

func proper(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevLetter {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			prevLetter = true
		} else {
			b.WriteRune(r)
			prevLetter = false
		}
	}
	return b.String()
}

type textBuilder struct {
	b     strings.Builder
	n     int
	over  bool
	parts int
}

func (t *textBuilder) add(s string) {
	if t.over {
		return
	}
	t.n += textLen(s)
	if t.n > MaxCellChars {
		t.over = true
		return
	}
	t.b.WriteString(s)
}

func fnConcat(c *evalCtx, args []operand) operand {
	var tb textBuilder
	for _, a := range args {
		switch a.k {
		case opVal:
			s, e := toText(a.v)
			if e != "" {
				return errOp(e)
			}
			tb.add(s)
		case opRef:
			errCode := ""
			c.e.eachCell(a.s, a.r, true, func(_ Ref, cell *Cell) bool {
				if cell.Value.IsError() {
					errCode = cell.Value.Err
					return false
				}
				tb.add(cell.Value.Text())
				return !tb.over
			})
			if errCode != "" {
				return errOp(errCode)
			}
		case opArr:
			for r := 0; r < a.arr.rows && !tb.over; r++ {
				for col := 0; col < a.arr.cols && !tb.over; col++ {
					v := a.arr.at(r, col)
					if v.IsError() {
						return valOp(v)
					}
					tb.add(v.Text())
				}
			}
		}
		if tb.over {
			return errOp(CodeValue)
		}
	}
	return valOp(StringValue(tb.b.String()))
}

func fnConcatenate(_ *evalCtx, a []Value) Value {
	var tb textBuilder
	for _, v := range a {
		s, e := toText(v)
		if e != "" {
			return ErrorValue(e)
		}
		tb.add(s)
	}
	if tb.over {
		return ErrorValue(CodeValue)
	}
	return StringValue(tb.b.String())
}

func fnTextJoin(c *evalCtx, args []operand) operand {
	delim, e := toText(c.scalar(args[0]))
	if e != "" {
		return errOp(e)
	}
	ignore, e := toBool(c.scalar(args[1]))
	if e != "" {
		return errOp(e)
	}
	var tb textBuilder
	first := true
	push := func(v Value) string {
		if v.IsError() {
			return v.Err
		}
		s := v.Text()
		if ignore && s == "" {
			return ""
		}
		if !first {
			tb.add(delim)
		}
		first = false
		tb.add(s)
		return ""
	}
	for _, a := range args[2:] {
		switch a.k {
		case opVal:
			if e := push(a.v); e != "" {
				return errOp(e)
			}
		case opRef:
			if ignore || delim == "" {
				errCode := ""
				c.e.eachCell(a.s, a.r, true, func(_ Ref, cell *Cell) bool {
					errCode = push(cell.Value)
					return errCode == "" && !tb.over
				})
				if errCode != "" {
					return errOp(errCode)
				}
				break
			}
			// Blanks contribute delimiters, so walk every position (the
			// 32767-character cap ends runaway loops quickly).
			for r := a.r.From.Row; r <= a.r.To.Row && !tb.over; r++ {
				for col := a.r.From.Col; col <= a.r.To.Col && !tb.over; col++ {
					if e := push(a.s.Value(Ref{r, col})); e != "" {
						return errOp(e)
					}
				}
			}
		case opArr:
			for r := 0; r < a.arr.rows && !tb.over; r++ {
				for col := 0; col < a.arr.cols && !tb.over; col++ {
					if e := push(a.arr.at(r, col)); e != "" {
						return errOp(e)
					}
				}
			}
		}
		if tb.over {
			return errOp(CodeValue)
		}
	}
	return valOp(StringValue(tb.b.String()))
}

func leftRight(left bool) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		s, e := toText(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		nf, e := optNum(a, 1, 1)
		if e != "" {
			return ErrorValue(e)
		}
		if nf < 0 {
			return ErrorValue(CodeValue)
		}
		rs := []rune(s)
		n := clampInt(nf, 0, len(rs))
		if left {
			return StringValue(string(rs[:n]))
		}
		return StringValue(string(rs[len(rs)-n:]))
	}
}

func fnMid(_ *evalCtx, a []Value) Value {
	s, e := toText(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	st, e := toNum(a[1])
	if e != "" {
		return ErrorValue(e)
	}
	nf, e := toNum(a[2])
	if e != "" {
		return ErrorValue(e)
	}
	if st < 1 || nf < 0 {
		return ErrorValue(CodeValue)
	}
	rs := []rune(s)
	start := clampInt(st, 1, len(rs)+1) - 1
	n := clampInt(nf, 0, len(rs)-start)
	return StringValue(string(rs[start : start+n]))
}

func fnSubstitute(_ *evalCtx, a []Value) Value {
	var t [3]string
	for i := 0; i < 3; i++ {
		s, e := toText(a[i])
		if e != "" {
			return ErrorValue(e)
		}
		t[i] = s
	}
	text, old, repl := t[0], t[1], t[2]
	if old == "" {
		return StringValue(text)
	}
	if len(a) < 4 {
		// bound the result before building it
		if cnt := strings.Count(text, old); cnt > 0 && len(text)+cnt*(len(repl)-len(old)) > 4*MaxCellChars {
			return ErrorValue(CodeValue)
		}
		return textResult(strings.ReplaceAll(text, old, repl))
	}
	nf, e := toNum(a[3])
	if e != "" {
		return ErrorValue(e)
	}
	if nf < 1 {
		return ErrorValue(CodeValue)
	}
	inst := clampInt(nf, 1, len(text)+1)
	idx := -1
	from := 0
	for i := 0; i < inst; i++ {
		j := strings.Index(text[from:], old)
		if j < 0 {
			return StringValue(text)
		}
		idx = from + j
		from = idx + len(old)
	}
	return textResult(text[:idx] + repl + text[idx+len(old):])
}

func fnReplace(_ *evalCtx, a []Value) Value {
	s, e := toText(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	st, e := toNum(a[1])
	if e != "" {
		return ErrorValue(e)
	}
	nf, e := toNum(a[2])
	if e != "" {
		return ErrorValue(e)
	}
	repl, e := toText(a[3])
	if e != "" {
		return ErrorValue(e)
	}
	if st < 1 || nf < 0 {
		return ErrorValue(CodeValue)
	}
	rs := []rune(s)
	start := clampInt(st, 1, len(rs)+1) - 1
	n := clampInt(nf, 0, len(rs)-start)
	return textResult(string(rs[:start]) + repl + string(rs[start+n:]))
}

func findFn(search bool) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		needle, e := toText(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		hay, e := toText(a[1])
		if e != "" {
			return ErrorValue(e)
		}
		st, e := optNum(a, 2, 1)
		if e != "" {
			return ErrorValue(e)
		}
		hr := []rune(hay)
		if st < 1 || st > float64(len(hr)+1) {
			return ErrorValue(CodeValue)
		}
		start := int(st) - 1
		if needle == "" {
			return NumberValue(float64(start + 1))
		}
		if !search {
			if i := strings.Index(string(hr[start:]), needle); i >= 0 {
				return NumberValue(float64(start + utf8.RuneCountInString(string(hr[start:])[:i]) + 1))
			}
			return ErrorValue(CodeValue)
		}
		lowHay := []rune(strings.ToLower(hay))
		if len(lowHay) != len(hr) { // case mapping changed length; fall back to original runes
			lowHay = hr
		}
		lowNeedle := strings.ToLower(needle)
		if !strings.ContainsAny(lowNeedle, "*?~") {
			if i := strings.Index(string(lowHay[start:]), lowNeedle); i >= 0 {
				return NumberValue(float64(start + utf8.RuneCountInString(string(lowHay[start:])[:i]) + 1))
			}
			return ErrorValue(CodeValue)
		}
		pat := compileWild(lowNeedle + "*")
		budget := 4 * wildStepBudget // shared across all start positions
		for i := start; i < len(lowHay) && budget > 0; i++ {
			if wildMatchRunes(pat, lowHay[i:], &budget) {
				return NumberValue(float64(i + 1))
			}
		}
		return ErrorValue(CodeValue)
	}
}

func fnText(_ *evalCtx, a []Value) Value {
	f, e := toText(a[1])
	if e != "" {
		return ErrorValue(e)
	}
	v := a[0]
	switch v.Kind {
	case KindError:
		return v
	case KindString:
		if n, ok := ParseNumberText(v.Str); ok {
			v = NumberValue(n)
		}
	case KindEmpty:
		v = NumberValue(0)
	case KindBool:
		return StringValue(v.Text())
	}
	s, ok := CompileFormat(f).Format(v)
	if !ok {
		return ErrorValue(CodeValue)
	}
	return textResult(s)
}

func fnValue(_ *evalCtx, a []Value) Value {
	switch a[0].Kind {
	case KindNumber, KindError:
		return a[0]
	case KindEmpty:
		return NumberValue(0)
	case KindString:
		if f, ok := ParseNumberText(a[0].Str); ok {
			return NumberValue(f)
		}
	}
	return ErrorValue(CodeValue)
}

func fnRept(_ *evalCtx, a []Value) Value {
	s, e := toText(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	nf, e := toNum(a[1])
	if e != "" {
		return ErrorValue(e)
	}
	if nf < 0 {
		return ErrorValue(CodeValue)
	}
	n := clampInt(nf, 0, MaxCellChars+1)
	if textLen(s)*n > MaxCellChars {
		return ErrorValue(CodeValue)
	}
	return StringValue(strings.Repeat(s, n))
}

// Windows-1252 differs from Latin-1 in 0x80-0x9F.
var cp1252High = [32]rune{
	0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F,
	0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178,
}

func cp1252ToRune(b byte) rune {
	if b >= 0x80 && b < 0xA0 {
		return cp1252High[b-0x80]
	}
	return rune(b)
}

func runeToCP1252(r rune) (byte, bool) {
	if r < 0x80 || (r >= 0xA0 && r <= 0xFF) {
		return byte(r), true
	}
	for i, x := range cp1252High {
		if x == r {
			return byte(0x80 + i), true
		}
	}
	return 0, false
}

// DecodeCP1252 converts Windows-1252 bytes to a UTF-8 string (used by
// importers for legacy 8-bit text).
func DecodeCP1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(cp1252ToRune(c))
	}
	return sb.String()
}
