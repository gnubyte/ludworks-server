package engine

import (
	"math"
	"strconv"
	"strings"
)

// decimal is |value| = 0.digits × 10^exp with a sign. It is used for
// Excel-compatible rounding and display: Excel works on the 15-significant
// digit decimal form of a double, which is why ROUND(2.675,2) is 2.68 there
// while naive binary arithmetic gives 2.67.
type decimal struct {
	digits []byte // '0'..'9', no leading/trailing zeros; empty = zero
	exp    int
	neg    bool
}

type roundMode uint8

const (
	roundHalfAway roundMode = iota // ROUND
	roundUp                        // ROUNDUP: away from zero
	roundDown                      // ROUNDDOWN/TRUNC: toward zero
)

// toDecimal converts f using sig significant digits (1..17).
func toDecimal(f float64, sig int) decimal {
	var d decimal
	if f < 0 {
		d.neg = true
		f = -f
	}
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return decimal{}
	}
	s := strconv.FormatFloat(f, 'e', sig-1, 64) // d.dddde±xx
	mant, expStr, _ := strings.Cut(s, "e")
	e, _ := strconv.Atoi(expStr)
	digits := make([]byte, 0, len(mant))
	for i := 0; i < len(mant); i++ {
		if mant[i] != '.' {
			digits = append(digits, mant[i])
		}
	}
	d.digits = digits
	d.exp = e + 1
	d.trim()
	return d
}

func (d *decimal) trim() {
	i := 0
	for i < len(d.digits) && d.digits[i] == '0' {
		i++
		d.exp--
	}
	d.digits = d.digits[i:]
	j := len(d.digits)
	for j > 0 && d.digits[j-1] == '0' {
		j--
	}
	d.digits = d.digits[:j]
	if len(d.digits) == 0 {
		d.exp = 0
		d.neg = false
	}
}

func (d decimal) zero() bool { return len(d.digits) == 0 }

// round keeps `places` digits after the decimal point (negative places round
// to tens, hundreds, ...).
func (d decimal) round(places int, mode roundMode) decimal {
	if d.zero() {
		return d
	}
	n := d.exp + places // digits to keep
	if n >= len(d.digits) {
		return d
	}
	out := decimal{neg: d.neg}
	if n < 0 {
		if mode == roundUp {
			out.digits = []byte{'1'}
			out.exp = 1 - places
			return out
		}
		return decimal{}
	}
	inc := false
	switch mode {
	case roundHalfAway:
		inc = d.digits[n] >= '5'
	case roundUp:
		inc = true // remaining digits are non-zero because of trim()
	}
	kept := make([]byte, n, n+1)
	copy(kept, d.digits[:n])
	out.exp = d.exp
	if inc {
		i := n - 1
		for i >= 0 {
			if kept[i] == '9' {
				kept[i] = '0'
				i--
				continue
			}
			kept[i]++
			break
		}
		if i < 0 {
			kept = append([]byte{'1'}, kept...)
			out.exp++
		}
	}
	out.digits = kept
	out.trim()
	return out
}

// float converts back to float64.
func (d decimal) float() float64 {
	if d.zero() {
		return 0
	}
	s := "0." + string(d.digits) + "e" + strconv.Itoa(d.exp)
	f, _ := strconv.ParseFloat(s, 64)
	if d.neg {
		f = -f
	}
	return f
}

// intDigits returns the integer part digits ("0" when < 1).
func (d decimal) intDigits() string {
	if d.exp <= 0 || d.zero() {
		return "0"
	}
	if d.exp >= len(d.digits) {
		return string(d.digits) + strings.Repeat("0", d.exp-len(d.digits))
	}
	return string(d.digits[:d.exp])
}

// fracDigits returns the fractional digits (no trailing zeros, may be "").
func (d decimal) fracDigits() string {
	if d.zero() || d.exp >= len(d.digits) {
		return ""
	}
	if d.exp >= 0 {
		return string(d.digits[d.exp:])
	}
	return strings.Repeat("0", -d.exp) + string(d.digits)
}

// plain renders without exponent ("-12.5", "0.001").
func (d decimal) plain() string {
	if d.zero() {
		return "0"
	}
	s := d.intDigits()
	if f := d.fracDigits(); f != "" {
		s += "." + f
	}
	if d.neg {
		s = "-" + s
	}
	return s
}

// roundExcel rounds f to places decimals with Excel semantics.
func roundExcel(f float64, places int, mode roundMode) float64 {
	return toDecimal(f, 15).round(places, mode).float()
}
