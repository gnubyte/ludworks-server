package engine

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is the type of a cell value.
type Kind uint8

// Value kinds.
const (
	KindEmpty Kind = iota
	KindNumber
	KindString
	KindBool
	KindError
)

// TypeCode returns the API "t" code: n, s, b, e or z.
func (k Kind) TypeCode() string {
	switch k {
	case KindNumber:
		return "n"
	case KindString:
		return "s"
	case KindBool:
		return "b"
	case KindError:
		return "e"
	}
	return "z"
}

// String returns a readable kind name.
func (k Kind) String() string {
	switch k {
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindBool:
		return "bool"
	case KindError:
		return "error"
	}
	return "empty"
}

// Excel error codes. #CIRC! is Ludworks-specific (circular reference).
const (
	CodeNull  = "#NULL!"
	CodeDiv0  = "#DIV/0!"
	CodeValue = "#VALUE!"
	CodeRef   = "#REF!"
	CodeName  = "#NAME?"
	CodeNum   = "#NUM!"
	CodeNA    = "#N/A"
	CodeCirc  = "#CIRC!"
)

var errorCodes = []string{CodeNull, CodeDiv0, CodeValue, CodeRef, CodeName, CodeNum, CodeNA, CodeCirc}

// CanonicalErrorCode returns the canonical spelling of an error literal
// (case-insensitive) and whether it is known.
func CanonicalErrorCode(s string) (string, bool) {
	for _, c := range errorCodes {
		if strings.EqualFold(s, c) {
			return c, true
		}
	}
	return "", false
}

// Value is a computed or literal cell value. The zero Value is empty.
// Invariant: Num is never NaN or ±Inf (constructors map those to #NUM!).
type Value struct {
	Kind Kind
	Num  float64
	Str  string
	Bool bool
	Err  string
}

// NumberValue returns a number; NaN/Inf become #NUM!. -0 is normalised to 0.
func NumberValue(f float64) Value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Value{Kind: KindError, Err: CodeNum}
	}
	if f == 0 {
		f = 0
	}
	return Value{Kind: KindNumber, Num: f}
}

// StringValue returns a text value.
func StringValue(s string) Value { return Value{Kind: KindString, Str: s} }

// BoolValue returns a logical value.
func BoolValue(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// ErrorValue returns an error value with the given code (e.g. CodeDiv0).
func ErrorValue(code string) Value { return Value{Kind: KindError, Err: code} }

// IsEmpty reports whether the value is blank.
func (v Value) IsEmpty() bool { return v.Kind == KindEmpty }

// IsError reports whether the value is an error.
func (v Value) IsError() bool { return v.Kind == KindError }

// Equal reports exact equality (kind and payload).
func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindNumber:
		return v.Num == o.Num
	case KindString:
		return v.Str == o.Str
	case KindBool:
		return v.Bool == o.Bool
	case KindError:
		return v.Err == o.Err
	}
	return true
}

// JSON returns the API "v" payload: float64, string, bool, nil (empty) or
// the error code string for errors.
func (v Value) JSON() any {
	switch v.Kind {
	case KindNumber:
		return v.Num
	case KindString:
		return v.Str
	case KindBool:
		return v.Bool
	case KindError:
		return v.Err
	}
	return nil
}

// Text returns the value as Excel converts it to text (general format,
// 15 significant digits, TRUE/FALSE, error code, "" for empty).
func (v Value) Text() string {
	switch v.Kind {
	case KindNumber:
		return NumberToText(v.Num)
	case KindString:
		return v.Str
	case KindBool:
		if v.Bool {
			return "TRUE"
		}
		return "FALSE"
	case KindError:
		return v.Err
	}
	return ""
}

// String implements fmt.Stringer (debug form).
func (v Value) String() string { return v.Text() }

// NumberToText formats a number like Excel's general text conversion:
// at most 15 significant digits, scientific notation for very large or very
// small magnitudes ("1E+15", "1.5E-10").
func NumberToText(f float64) string {
	if f == 0 {
		return "0"
	}
	d := toDecimal(f, 15)
	if d.zero() {
		return "0"
	}
	// d represents 0.D × 10^exp
	sciExp := d.exp - 1 // exponent in d1.d2... form
	if sciExp >= 15 || sciExp < -9 {
		var b strings.Builder
		if d.neg {
			b.WriteByte('-')
		}
		b.WriteByte(d.digits[0])
		if len(d.digits) > 1 {
			b.WriteByte('.')
			b.Write(d.digits[1:])
		}
		b.WriteByte('E')
		if sciExp < 0 {
			b.WriteByte('-')
			sciExp = -sciExp
		} else {
			b.WriteByte('+')
		}
		if sciExp < 10 {
			b.WriteByte('0')
		}
		b.WriteString(strconv.Itoa(sciExp))
		return b.String()
	}
	return d.plain()
}

// textLen counts characters (runes) as Excel's LEN does for BMP text.
func textLen(s string) int { return utf8.RuneCountInString(s) }

// ParseNumberText converts text to a number the way Excel coerces text in
// arithmetic: optional sign, thousands separators, currency "$" prefix,
// trailing "%", parentheses for negatives, exponents, and date/time text
// (ISO "2024-01-31", "1/31/2024", "13:45", "2024-01-31 08:00").
func ParseNumberText(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if f, ok := parsePlainNumber(s); ok {
		return f, true
	}
	if f, ok := parseDateTimeText(s); ok {
		return f, true
	}
	return 0, false
}

// parsePlainNumber handles the non-date numeric spellings.
func parsePlainNumber(s string) (float64, bool) {
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg = true
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return 0, false
	}
	if s[0] == '-' || s[0] == '+' {
		if neg {
			return 0, false
		}
		neg = s[0] == '-'
		s = strings.TrimSpace(s[1:])
	}
	pct := 0
	for strings.HasSuffix(s, "%") {
		pct++
		s = strings.TrimSpace(s[:len(s)-1])
	}
	if strings.HasPrefix(s, "$") {
		s = strings.TrimSpace(s[1:])
	}
	if s == "" {
		return 0, false
	}
	// Thousands separators must be in valid groups of three.
	if strings.Contains(s, ",") {
		intPart, rest := s, ""
		if i := strings.IndexAny(s, ".eE"); i >= 0 {
			intPart, rest = s[:i], s[i:]
		}
		groups := strings.Split(intPart, ",")
		if len(groups[0]) == 0 || len(groups[0]) > 3 {
			return 0, false
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return 0, false
			}
		}
		s = strings.Join(groups, "") + rest
		if strings.Contains(s, ",") {
			return 0, false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && c != '.' && c != 'e' && c != 'E' && c != '+' && c != '-' {
			return 0, false
		}
	}
	if s[0] == 'e' || s[0] == 'E' {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	for ; pct > 0; pct-- {
		f /= 100
	}
	if neg {
		f = -f
	}
	return f, true
}
