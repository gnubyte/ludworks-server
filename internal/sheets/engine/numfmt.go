package engine

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// NumberFormat is a compiled Excel number-format code. Supported: up to 4
// sections (pos;neg;zero;text), simple [>n] conditions, General, 0 # ?
// placeholders, thousands separators and scaling commas, %, E+/E-
// scientific, quoted/escaped literals, _x spacing, *x fill (ignored),
// [Color] (ignored), [$sym-locale] currency, @ text, and date/time codes
// (y m d h s AM/PM A/P [h] [m] [s] and fractional seconds). Fractions
// ("# ?/?") are rendered as decimals.
type NumberFormat struct {
	src      string
	sections []*fmtSection
}

type ftKind uint8

const (
	ftLit   ftKind = iota
	ftDigit        // '0' '#' '?' (ch holds which)
	ftDecimal
	ftComma
	ftPercent
	ftExp // text "+" or "-"
	ftText
	ftGeneral
	ftYear
	ftMonth  // month or minute before resolution
	ftMinute // resolved
	ftDay
	ftHour
	ftSecond
	ftAMPM
	ftElapsedH
	ftElapsedM
	ftElapsedS
	ftFracSec
)

type ftok struct {
	kind ftKind
	text string
	ch   byte
	n    int
}

type secKind uint8

const (
	secNumber secKind = iota
	secDate
	secText
	secGeneral
)

type fmtCond struct {
	op  string
	val float64
}

func (c *fmtCond) match(v float64) bool {
	switch c.op {
	case "<":
		return v < c.val
	case "<=":
		return v <= c.val
	case ">":
		return v > c.val
	case ">=":
		return v >= c.val
	case "=":
		return v == c.val
	case "<>":
		return v != c.val
	}
	return false
}

type fmtSection struct {
	toks    []ftok
	kind    secKind
	cond    *fmtCond
	hasAMPM bool
	hasText bool
	// number layout
	percent   int
	scale     int
	thousands bool
	exp       bool
	frac      *fracLayout
}

// fracLayout describes a fraction format such as "# ?/?" or "?/16".
type fracLayout struct {
	slash    int  // index of the literal token starting with "/"
	numDigs  int  // numerator placeholders
	denDigs  int  // denominator placeholders (when not fixed)
	fixedDen int  // explicit denominator ("?/16"), 0 if none
	hasWhole bool // integer part placeholder(s) before the numerator
}

// detectFraction recognises "<whole> <num>/<den>" layouts in a number section.
func (sec *fmtSection) detectFraction() {
	for i, t := range sec.toks {
		if t.kind == ftDecimal || t.kind == ftExp {
			return
		}
		if t.kind != ftLit || !strings.HasPrefix(t.text, "/") || i == 0 || sec.toks[i-1].kind != ftDigit {
			continue
		}
		fl := &fracLayout{slash: i}
		j := i - 1
		for j >= 0 && sec.toks[j].kind == ftDigit {
			fl.numDigs++
			j--
		}
		for ; j >= 0; j-- {
			if sec.toks[j].kind == ftDigit {
				fl.hasWhole = true
			}
		}
		rest := t.text[1:]
		k := 0
		for k < len(rest) && rest[k] >= '0' && rest[k] <= '9' {
			k++
		}
		if k > 0 {
			n := 0
			for _, ch := range rest[:k] {
				n = n*10 + int(ch-'0')
				if n > 100000 {
					break
				}
			}
			fl.fixedDen = max(1, min(n, 100000))
		} else {
			for m := i + 1; m < len(sec.toks) && sec.toks[m].kind == ftDigit; m++ {
				fl.denDigs++
			}
			if fl.denDigs == 0 {
				continue
			}
		}
		sec.frac = fl
		return
	}
}

// bestFraction approximates x ≥ 0 with a denominator ≤ maxDen (or exactly
// fixedDen), preferring the smallest denominator on ties.
func bestFraction(x float64, maxDen, fixedDen int) (int, int) {
	if fixedDen > 0 {
		return int(math.Round(x * float64(fixedDen))), fixedDen
	}
	bestN, bestD, bestErr := int(math.Round(x)), 1, math.Abs(x-math.Round(x))
	for d := 2; d <= maxDen && bestErr > 1e-12; d++ {
		n := math.Round(x * float64(d))
		if err := math.Abs(x - n/float64(d)); err < bestErr-1e-15 {
			bestN, bestD, bestErr = int(n), d, err
		}
	}
	return bestN, bestD
}

func (sec *fmtSection) formatFraction(v float64) (string, bool) {
	fl := sec.frac
	maxDen := 1
	for i := 0; i < fl.denDigs && maxDen < 100000; i++ {
		maxDen *= 10
	}
	maxDen = max(1, maxDen-1)
	whole := 0.0
	x := v
	if fl.hasWhole {
		whole = math.Floor(v)
		x = v - whole
	}
	if v > 1e15 {
		return NumberToText(v), v == 0
	}
	num, den := bestFraction(x, maxDen, fl.fixedDen)
	if fl.hasWhole && num == den && fl.fixedDen == 0 {
		whole++
		num = 0
	}
	numStr := strconv.Itoa(num)
	if len(numStr) < fl.numDigs {
		numStr = strings.Repeat(" ", fl.numDigs-len(numStr)) + numStr
	}
	denStr := strconv.Itoa(den)
	if fl.denDigs > len(denStr) {
		denStr += strings.Repeat(" ", fl.denDigs-len(denStr))
	}
	var body string
	switch {
	case !fl.hasWhole:
		body = numStr + "/" + denStr
	case num == 0:
		body = strconv.FormatFloat(whole, 'f', 0, 64)
	case whole == 0:
		body = strings.TrimLeft(numStr, " ") + "/" + denStr
	default:
		body = strconv.FormatFloat(whole, 'f', 0, 64) + " " + numStr + "/" + denStr
	}
	// literals before the first and after the last placeholder are kept
	first, last := -1, -1
	for i, t := range sec.toks {
		if t.kind == ftDigit || i == fl.slash {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	var b strings.Builder
	for i := 0; i < first; i++ {
		if sec.toks[i].kind == ftLit {
			b.WriteString(sec.toks[i].text)
		}
	}
	b.WriteString(body)
	for i := last + 1; i < len(sec.toks); i++ {
		if t := sec.toks[i]; t.kind == ftLit {
			b.WriteString(t.text)
		}
	}
	return b.String(), num == 0 && whole == 0
}

// CompileFormat parses a format code. It never fails: unknown constructs
// are treated as literals. "" and "General" give the general format.
func CompileFormat(code string) *NumberFormat {
	if len(code) > MaxFormatLen {
		code = code[:MaxFormatLen]
	}
	nf := &NumberFormat{src: code}
	if code == "" || strings.EqualFold(code, "general") {
		nf.sections = []*fmtSection{{kind: secGeneral, toks: []ftok{{kind: ftGeneral}}}}
		return nf
	}
	for _, part := range splitSections(code) {
		nf.sections = append(nf.sections, parseSection(part))
		if len(nf.sections) == 4 {
			break
		}
	}
	return nf
}

// String returns the source code.
func (nf *NumberFormat) String() string { return nf.src }

// IsDate reports whether the (first) section formats dates/times.
func (nf *NumberFormat) IsDate() bool {
	return len(nf.sections) > 0 && nf.sections[0].kind == secDate
}

// IsDateFormat reports whether a format code displays dates or times.
func IsDateFormat(code string) bool { return CompileFormat(code).IsDate() }

func splitSections(s string) []string {
	var out []string
	st := 0
	inQuote, inBracket := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote:
			if c == '"' {
				inQuote = false
			}
		case inBracket:
			if c == ']' {
				inBracket = false
			}
		case c == '"':
			inQuote = true
		case c == '[':
			inBracket = true
		case c == '\\' || c == '_' || c == '*':
			i++ // skip escaped / spacing / fill char
		case c == ';':
			out = append(out, s[st:i])
			st = i + 1
		}
	}
	return append(out, s[st:])
}

func runLen(s string, i int, lower byte) int {
	n := 0
	for i+n < len(s) && s[i+n]|0x20 == lower {
		n++
	}
	return n
}

func parseSection(s string) *fmtSection {
	sec := &fmtSection{}
	lit := func(t string) {
		if n := len(sec.toks); n > 0 && sec.toks[n-1].kind == ftLit {
			sec.toks[n-1].text += t
			return
		}
		sec.toks = append(sec.toks, ftok{kind: ftLit, text: t})
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				lit(s[i+1:])
				i = len(s)
			} else {
				lit(s[i+1 : i+1+j])
				i += j + 2
			}
		case c == '\\':
			if i+1 < len(s) {
				_, size := utf8.DecodeRuneInString(s[i+1:])
				lit(s[i+1 : i+1+size])
				i += 1 + size
			} else {
				i++
			}
		case c == '_':
			lit(" ")
			if i+1 < len(s) {
				_, size := utf8.DecodeRuneInString(s[i+1:])
				i += 1 + size
			} else {
				i++
			}
		case c == '*':
			if i+1 < len(s) {
				_, size := utf8.DecodeRuneInString(s[i+1:])
				i += 1 + size
			} else {
				i++
			}
		case c == '[':
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				i = len(s)
				break
			}
			sec.bracket(s[i+1:i+j], lit)
			i += j + 1
		case (c == 'G' || c == 'g') && len(s)-i >= 7 && strings.EqualFold(s[i:i+7], "general"):
			sec.toks = append(sec.toks, ftok{kind: ftGeneral})
			i += 7
		case c == '0' || c == '#' || c == '?':
			sec.toks = append(sec.toks, ftok{kind: ftDigit, ch: c})
			i++
		case c == '.':
			sec.toks = append(sec.toks, ftok{kind: ftDecimal})
			i++
		case c == ',':
			sec.toks = append(sec.toks, ftok{kind: ftComma})
			i++
		case c == '%':
			sec.toks = append(sec.toks, ftok{kind: ftPercent})
			sec.percent++
			i++
		case (c == 'E' || c == 'e') && i+1 < len(s) && (s[i+1] == '+' || s[i+1] == '-'):
			sec.toks = append(sec.toks, ftok{kind: ftExp, text: s[i+1 : i+2]})
			sec.exp = true
			i += 2
		case c == '@':
			sec.toks = append(sec.toks, ftok{kind: ftText})
			sec.hasText = true
			i++
		case c|0x20 == 'y':
			n := runLen(s, i, 'y')
			sec.toks = append(sec.toks, ftok{kind: ftYear, n: n})
			i += n
		case c|0x20 == 'm':
			n := runLen(s, i, 'm')
			sec.toks = append(sec.toks, ftok{kind: ftMonth, n: n})
			i += n
		case c|0x20 == 'd':
			n := runLen(s, i, 'd')
			sec.toks = append(sec.toks, ftok{kind: ftDay, n: n})
			i += n
		case c|0x20 == 'h':
			n := runLen(s, i, 'h')
			sec.toks = append(sec.toks, ftok{kind: ftHour, n: n})
			i += n
		case c|0x20 == 's':
			n := runLen(s, i, 's')
			sec.toks = append(sec.toks, ftok{kind: ftSecond, n: n})
			i += n
		case (c == 'A' || c == 'a') && len(s)-i >= 5 && strings.EqualFold(s[i:i+5], "am/pm"):
			sec.toks = append(sec.toks, ftok{kind: ftAMPM, text: s[i : i+5]})
			sec.hasAMPM = true
			i += 5
		case (c == 'A' || c == 'a') && len(s)-i >= 3 && strings.EqualFold(s[i:i+3], "a/p"):
			sec.toks = append(sec.toks, ftok{kind: ftAMPM, text: s[i : i+3]})
			sec.hasAMPM = true
			i += 3
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			lit(s[i : i+size])
			i += size
		}
	}
	sec.finish()
	return sec
}

func (sec *fmtSection) bracket(content string, lit func(string)) {
	lc := strings.ToLower(content)
	switch {
	case lc == "":
	case strings.Trim(lc, "h") == "":
		sec.toks = append(sec.toks, ftok{kind: ftElapsedH, n: len(lc)})
	case strings.Trim(lc, "m") == "":
		sec.toks = append(sec.toks, ftok{kind: ftElapsedM, n: len(lc)})
	case strings.Trim(lc, "s") == "":
		sec.toks = append(sec.toks, ftok{kind: ftElapsedS, n: len(lc)})
	case lc[0] == '$':
		sym := content[1:]
		if k := strings.IndexByte(sym, '-'); k >= 0 {
			sym = sym[:k]
		}
		lit(sym)
	case lc[0] == '<' || lc[0] == '>' || lc[0] == '=':
		op := lc[:1]
		rest := lc[1:]
		if len(lc) > 1 && (lc[1] == '=' || (lc[0] == '<' && lc[1] == '>')) {
			op = lc[:2]
			rest = lc[2:]
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64); err == nil {
			sec.cond = &fmtCond{op: op, val: f}
		}
	default:
		// colours ([Red], [Color10]) and locale tags are display hints only.
	}
}

func (sec *fmtSection) finish() {
	isDateTok := func(k ftKind) bool {
		switch k {
		case ftYear, ftMonth, ftMinute, ftDay, ftHour, ftSecond, ftAMPM, ftElapsedH, ftElapsedM, ftElapsedS:
			return true
		}
		return false
	}
	date := false
	for _, t := range sec.toks {
		if isDateTok(t.kind) {
			date = true
			break
		}
	}
	if date {
		sec.kind = secDate
		// Resolve m → minute when it follows an hour or precedes a second.
		for i := range sec.toks {
			if sec.toks[i].kind != ftMonth {
				continue
			}
			prev, next := ftLit, ftLit
			for j := i - 1; j >= 0; j-- {
				if k := sec.toks[j].kind; isDateTok(k) {
					prev = k
					break
				}
			}
			for j := i + 1; j < len(sec.toks); j++ {
				if k := sec.toks[j].kind; isDateTok(k) {
					next = k
					break
				}
			}
			if (prev == ftHour || prev == ftElapsedH) && sec.toks[i].n <= 2 || next == ftSecond || next == ftElapsedS {
				if sec.toks[i].n <= 2 {
					sec.toks[i].kind = ftMinute
				}
			}
		}
		// ".0" after seconds → fractional seconds; other number tokens → literals.
		out := sec.toks[:0:0]
		for i := 0; i < len(sec.toks); i++ {
			t := sec.toks[i]
			switch t.kind {
			case ftDecimal:
				n := 0
				for i+1+n < len(sec.toks) && sec.toks[i+1+n].kind == ftDigit && sec.toks[i+1+n].ch == '0' {
					n++
				}
				if n > 0 {
					out = append(out, ftok{kind: ftFracSec, n: min(n, 3)})
					i += n
				} else {
					out = append(out, ftok{kind: ftLit, text: "."})
				}
			case ftDigit:
				out = append(out, ftok{kind: ftLit, text: string(t.ch)})
			case ftComma:
				out = append(out, ftok{kind: ftLit, text: ","})
			case ftPercent:
				out = append(out, ftok{kind: ftLit, text: "%"})
			case ftExp:
				out = append(out, ftok{kind: ftLit, text: "E" + t.text})
			default:
				out = append(out, t)
			}
		}
		sec.toks = out
		return
	}
	hasDigit, hasGeneral := false, false
	for _, t := range sec.toks {
		switch t.kind {
		case ftDigit:
			hasDigit = true
		case ftGeneral:
			hasGeneral = true
		}
	}
	switch {
	case hasGeneral:
		sec.kind = secGeneral
	case !hasDigit && sec.hasText:
		sec.kind = secText
	default:
		sec.kind = secNumber
		sec.detectFraction()
	}
	// Classify commas: thousands separator between integer placeholders,
	// scaling (÷1000) right after the last integer placeholder.
	inFrac, inExp := false, false
	for i, t := range sec.toks {
		switch t.kind {
		case ftDecimal:
			inFrac = true
		case ftExp:
			inExp = true
		case ftComma:
			prevDigit := false
			for j := i - 1; j >= 0; j-- {
				if sec.toks[j].kind == ftDigit {
					prevDigit = true
					break
				}
				if sec.toks[j].kind != ftComma {
					break
				}
			}
			nextDigit := false
			for j := i + 1; j < len(sec.toks); j++ {
				if sec.toks[j].kind == ftDigit {
					nextDigit = true
					break
				}
				if sec.toks[j].kind != ftComma {
					break
				}
			}
			switch {
			case inExp || !prevDigit:
				sec.toks[i] = ftok{kind: ftLit, text: ","}
			case nextDigit && !inFrac:
				sec.thousands = true
			case !nextDigit:
				sec.scale++ // trailing comma(s) after the last placeholder: ÷1000 each
			default:
				sec.toks[i] = ftok{kind: ftLit, text: ","}
			}
		}
	}
}

// Format renders a value. ok=false means the value cannot be shown in this
// format (e.g. a negative date); Format then returns a "#" filler.
func (nf *NumberFormat) Format(v Value) (string, bool) {
	switch v.Kind {
	case KindEmpty:
		return "", true
	case KindBool, KindError:
		return v.Text(), true
	case KindString:
		return nf.formatText(v.Str), true
	}
	return nf.formatNumber(v.Num)
}

func (nf *NumberFormat) formatText(s string) string {
	var sec *fmtSection
	if len(nf.sections) == 4 {
		sec = nf.sections[3]
	} else {
		for _, x := range nf.sections {
			if x.kind == secText {
				sec = x
			}
		}
	}
	if sec == nil {
		return s
	}
	var b strings.Builder
	for _, t := range sec.toks {
		switch t.kind {
		case ftText:
			b.WriteString(s)
		case ftLit:
			b.WriteString(t.text)
		}
	}
	return b.String()
}

func (nf *NumberFormat) numSections() []*fmtSection {
	var out []*fmtSection
	for i, s := range nf.sections {
		if i == 3 || s.kind == secText {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (nf *NumberFormat) formatNumber(v float64) (string, bool) {
	secs := nf.numSections()
	if len(secs) == 0 {
		return NumberToText(v), true
	}
	var sec *fmtSection
	val, minus := math.Abs(v), v < 0
	if secs[0].cond != nil || (len(secs) > 1 && secs[1].cond != nil) {
		for _, s := range secs {
			if s.cond == nil || s.cond.match(v) {
				sec = s
				break
			}
		}
		if sec == nil {
			return strings.Repeat("#", 8), false
		}
	} else {
		switch {
		case v > 0 || len(secs) == 1:
			sec = secs[0]
		case v < 0:
			sec = secs[1] // explicit negative section supplies its own sign
			minus = false
		case len(secs) >= 3:
			sec = secs[2]
		default:
			sec = secs[0]
		}
	}
	var out string
	var isZero, ok bool
	switch sec.kind {
	case secDate:
		if v < 0 {
			return strings.Repeat("#", 8), false
		}
		out, ok = sec.formatDate(v)
		if !ok {
			return strings.Repeat("#", 8), false
		}
		return out, true
	case secGeneral:
		var b strings.Builder
		for _, t := range sec.toks {
			switch t.kind {
			case ftGeneral:
				b.WriteString(NumberToText(val))
			case ftLit:
				b.WriteString(t.text)
			}
		}
		out, isZero = b.String(), val == 0
	default:
		if sec.frac != nil {
			out, isZero = sec.formatFraction(val)
		} else {
			out, isZero = sec.formatNum(val)
		}
	}
	if minus && !isZero {
		out = "-" + out
	}
	return out, true
}

// formatNum renders a non-negative number with a number section.
func (sec *fmtSection) formatNum(v float64) (string, bool) {
	for i := 0; i < sec.percent; i++ {
		v *= 100
	}
	for i := 0; i < sec.scale; i++ {
		v /= 1000
	}
	// Collect placeholder layout.
	var intPh, fracPh, expPh []int // token indexes
	part := 0                      // 0 int, 1 frac, 2 exp
	for i, t := range sec.toks {
		switch t.kind {
		case ftDecimal:
			if part == 0 {
				part = 1
			}
		case ftExp:
			part = 2
		case ftDigit:
			switch part {
			case 0:
				intPh = append(intPh, i)
			case 1:
				fracPh = append(fracPh, i)
			default:
				expPh = append(expPh, i)
			}
		}
	}
	expVal := 0
	if sec.exp && v != 0 {
		e10 := int(math.Floor(math.Log10(v)))
		nInt := max(len(intPh), 1)
		if len(intPh) > 1 && sec.toks[intPh[0]].ch == '#' {
			expVal = floorDiv(e10, nInt) * nInt
		} else {
			zeros := 0
			for _, k := range intPh {
				if sec.toks[k].ch != '#' {
					zeros++
				}
			}
			expVal = e10 - (max(zeros, 1) - 1)
		}
		v /= math.Pow(10, float64(expVal))
		d := toDecimal(v, 15).round(len(fracPh), roundHalfAway)
		if limit := math.Pow(10, float64(max(len(intPh), 1))); d.float() >= limit && !(len(intPh) > 1 && sec.toks[intPh[0]].ch == '#') {
			v /= 10
			expVal++
		}
	}
	d := toDecimal(v, 15).round(len(fracPh), roundHalfAway)
	isZero := d.zero()
	intStr := d.intDigits()
	frac := d.fracDigits()
	if len(frac) < len(fracPh) {
		frac += strings.Repeat("0", len(fracPh)-len(frac))
	}
	// Integer digits: minimum width from the first '0'/'?' placeholder on.
	minDigits := 0
	for k, idx := range intPh {
		if c := sec.toks[idx].ch; c == '0' || c == '?' {
			minDigits = len(intPh) - k
			break
		}
	}
	if intStr == "0" {
		intStr = ""
	}
	interleaved := false
	if len(intPh) > 1 {
		for k := intPh[0]; k < intPh[len(intPh)-1]; k++ {
			if sec.toks[k].kind == ftLit {
				interleaved = true
				break
			}
		}
	}
	// Pad on the left; '?' placeholders pad with spaces.
	if len(intStr) < minDigits {
		var pad strings.Builder
		for k := len(intPh) - minDigits; k < len(intPh)-len(intStr); k++ {
			if sec.toks[intPh[k]].ch == '?' {
				pad.WriteByte(' ')
			} else {
				pad.WriteByte('0')
			}
		}
		intStr = pad.String() + intStr
	}
	if sec.thousands && !interleaved {
		intStr = groupThousands(intStr)
	}
	// Fraction: trim optional trailing digits.
	fracOut := []byte(frac)
	for k := len(fracPh) - 1; k >= 0; k-- {
		ch := sec.toks[fracPh[k]].ch
		if fracOut[k] != '0' || ch == '0' {
			break
		}
		if ch == '#' {
			fracOut = fracOut[:k]
		} else {
			fracOut[k] = ' '
		}
	}
	// Distribute integer digits over interleaved placeholders (e.g. 000-0000).
	intAt := map[int]string{}
	if interleaved {
		digits := intStr
		for k := len(intPh) - 1; k >= 0; k-- {
			if k == 0 {
				intAt[intPh[k]] = digits
				break
			}
			if digits == "" {
				intAt[intPh[k]] = ""
				continue
			}
			intAt[intPh[k]] = digits[len(digits)-1:]
			digits = digits[:len(digits)-1]
		}
	}
	var b strings.Builder
	fracIdx := 0
	expDone := false
	firstInt := true
	for i, t := range sec.toks {
		switch t.kind {
		case ftLit:
			b.WriteString(t.text)
		case ftPercent:
			b.WriteByte('%')
		case ftDecimal:
			if i == firstDecimal(sec.toks) {
				b.WriteByte('.')
			}
		case ftDigit:
			switch {
			case interleaved && containsIdx(intPh, i):
				b.WriteString(intAt[i])
			case containsIdx(intPh, i):
				if firstInt {
					b.WriteString(intStr)
					firstInt = false
				}
			case containsIdx(fracPh, i):
				if fracIdx < len(fracOut) {
					b.WriteByte(fracOut[fracIdx])
				}
				fracIdx++
			}
		case ftExp:
			if expDone {
				continue
			}
			expDone = true
			b.WriteByte('E')
			ev := expVal
			if ev < 0 {
				b.WriteByte('-')
				ev = -ev
			} else if t.text == "+" {
				b.WriteByte('+')
			}
			es := strconv.Itoa(ev)
			zeros := 0
			for _, k := range expPh {
				if sec.toks[k].ch == '0' {
					zeros++
				}
			}
			if len(es) < zeros {
				es = strings.Repeat("0", zeros-len(es)) + es
			}
			b.WriteString(es)
		}
	}
	return b.String(), isZero
}

func firstDecimal(toks []ftok) int {
	for i, t := range toks {
		if t.kind == ftDecimal {
			return i
		}
	}
	return -1
}

func containsIdx(xs []int, i int) bool {
	for _, x := range xs {
		if x == i {
			return true
		}
	}
	return false
}

func groupThousands(s string) string {
	// keep any leading space padding intact
	lead := 0
	for lead < len(s) && s[lead] == ' ' {
		lead++
	}
	digits := s[lead:]
	if len(digits) <= 3 {
		return s
	}
	var b strings.Builder
	b.WriteString(s[:lead])
	first := len(digits) % 3
	if first > 0 {
		b.WriteString(digits[:first])
	}
	for i := first; i < len(digits); i += 3 {
		if b.Len() > lead {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

func pad2(n int) string {
	if n < 10 && n >= 0 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func padN(n, width int) string {
	s := strconv.Itoa(n)
	if len(s) < width {
		s = strings.Repeat("0", width-len(s)) + s
	}
	return s
}

func (sec *fmtSection) formatDate(v float64) (string, bool) {
	fracDigits := 0
	for _, t := range sec.toks {
		if t.kind == ftFracSec {
			fracDigits = max(fracDigits, t.n)
		}
	}
	if v >= MaxDateSerial+1 {
		return "", false
	}
	days := int(math.Floor(v))
	scale := math.Pow(10, float64(fracDigits))
	// total time in units of 10^-fracDigits seconds
	units := int64(math.Round((v - float64(days)) * 86400 * scale))
	unitsPerDay := int64(86400 * scale)
	if units >= unitsPerDay {
		days++
		units -= unitsPerDay
	}
	secsTotal := units / int64(scale)
	subSec := units % int64(scale)
	y, mo, d := SerialToDate(days)
	h, mi, s := int(secsTotal/3600), int(secsTotal/60%60), int(secsTotal%60)
	var b strings.Builder
	for _, t := range sec.toks {
		switch t.kind {
		case ftLit:
			b.WriteString(t.text)
		case ftYear:
			if t.n <= 2 {
				b.WriteString(pad2(y % 100))
			} else {
				b.WriteString(padN(y, 4))
			}
		case ftMonth:
			switch {
			case t.n == 1:
				b.WriteString(strconv.Itoa(mo))
			case t.n == 2:
				b.WriteString(pad2(mo))
			case t.n == 3:
				b.WriteString(monthNames[mo-1][:3])
			case t.n == 5:
				b.WriteString(monthNames[mo-1][:1])
			default:
				b.WriteString(monthNames[mo-1])
			}
		case ftDay:
			switch {
			case t.n == 1:
				b.WriteString(strconv.Itoa(d))
			case t.n == 2:
				b.WriteString(pad2(d))
			case t.n == 3:
				b.WriteString(dayNames[weekdayIndex(days)][:3])
			default:
				b.WriteString(dayNames[weekdayIndex(days)])
			}
		case ftHour:
			hh := h
			if sec.hasAMPM {
				hh = h % 12
				if hh == 0 {
					hh = 12
				}
			}
			if t.n >= 2 {
				b.WriteString(pad2(hh))
			} else {
				b.WriteString(strconv.Itoa(hh))
			}
		case ftMinute:
			if t.n >= 2 {
				b.WriteString(pad2(mi))
			} else {
				b.WriteString(strconv.Itoa(mi))
			}
		case ftSecond:
			if t.n >= 2 {
				b.WriteString(pad2(s))
			} else {
				b.WriteString(strconv.Itoa(s))
			}
		case ftFracSec:
			b.WriteByte('.')
			digits := padN(int(subSec), fracDigits)
			b.WriteString(digits[:t.n])
		case ftAMPM:
			pm := h >= 12
			lower := t.text[0] == 'a' || t.text[0] == 'p'
			var out string
			if len(t.text) == 5 {
				out = "AM"
				if pm {
					out = "PM"
				}
			} else {
				out = "A"
				if pm {
					out = "P"
				}
			}
			if lower {
				out = strings.ToLower(out)
			}
			b.WriteString(out)
		case ftElapsedH:
			b.WriteString(padN(days*24+h, t.n))
		case ftElapsedM:
			b.WriteString(padN((days*24+h)*60+mi, t.n))
		case ftElapsedS:
			b.WriteString(padN(((days*24+h)*60+mi)*60+s, t.n))
		}
	}
	return b.String(), true
}

// FormatValue renders a value for display using a number-format code.
// Values that cannot be displayed (e.g. negative dates) render as "########".
func FormatValue(v Value, format string) string {
	s, _ := CompileFormat(format).Format(v)
	return s
}

// Built-in number formats (ECMA-376 §18.8.30, en-US).
var builtinNumFmts = map[int]string{
	0: "General", 1: "0", 2: "0.00", 3: "#,##0", 4: "#,##0.00",
	5: `$#,##0_);($#,##0)`, 6: `$#,##0_);[Red]($#,##0)`, 7: `$#,##0.00_);($#,##0.00)`, 8: `$#,##0.00_);[Red]($#,##0.00)`,
	9: "0%", 10: "0.00%", 11: "0.00E+00", 12: "# ?/?", 13: "# ??/??",
	14: "m/d/yyyy", 15: "d-mmm-yy", 16: "d-mmm", 17: "mmm-yy", 18: "h:mm AM/PM", 19: "h:mm:ss AM/PM",
	20: "h:mm", 21: "h:mm:ss", 22: "m/d/yyyy h:mm",
	37: "#,##0 ;(#,##0)", 38: "#,##0 ;[Red](#,##0)", 39: "#,##0.00;(#,##0.00)", 40: "#,##0.00;[Red](#,##0.00)",
	41: `_(* #,##0_);_(* \(#,##0\);_(* "-"_);_(@_)`,
	42: `_("$"* #,##0_);_("$"* \(#,##0\);_("$"* "-"_);_(@_)`,
	43: `_(* #,##0.00_);_(* \(#,##0.00\);_(* "-"??_);_(@_)`,
	44: `_("$"* #,##0.00_);_("$"* \(#,##0.00\);_("$"* "-"??_);_(@_)`,
	45: "mm:ss", 46: "[h]:mm:ss", 47: "mm:ss.0", 48: "##0.0E+0", 49: "@",
}

// BuiltinNumFmt returns the format code of a built-in numFmtId.
func BuiltinNumFmt(id int) (string, bool) {
	s, ok := builtinNumFmts[id]
	return s, ok
}

// BuiltinNumFmtID returns the built-in id for a format code ("" → 0).
func BuiltinNumFmtID(code string) (int, bool) {
	if code == "" || strings.EqualFold(code, "general") {
		return 0, true
	}
	for id, c := range builtinNumFmts {
		if id != 0 && c == code {
			return id, true
		}
	}
	return 0, false
}
