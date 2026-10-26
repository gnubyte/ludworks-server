package engine

import (
	"math"
)

func init() {
	register(
		&funcDef{name: "SUM", syntax: "SUM(number1, [number2], ...)", desc: "Adds all numbers in ranges and arguments.", min: 1, max: -1, eager: fnSum},
		&funcDef{name: "PRODUCT", syntax: "PRODUCT(number1, [number2], ...)", desc: "Multiplies all numbers.", min: 1, max: -1, eager: fnProduct},
		&funcDef{name: "SUMSQ", syntax: "SUMSQ(number1, [number2], ...)", desc: "Sum of the squares of the numbers.", min: 1, max: -1, eager: fnSumSq},
		&funcDef{name: "SUMPRODUCT", syntax: "SUMPRODUCT(array1, [array2], ...)", desc: "Sum of the products of corresponding array elements.", min: 1, max: -1, eager: fnSumProduct},
		&funcDef{name: "ROUND", syntax: "ROUND(number, num_digits)", desc: "Rounds half away from zero to a number of digits.", min: 2, max: 2, scalar: roundFn(roundHalfAway)},
		&funcDef{name: "ROUNDUP", syntax: "ROUNDUP(number, num_digits)", desc: "Rounds away from zero.", min: 2, max: 2, scalar: roundFn(roundUp)},
		&funcDef{name: "ROUNDDOWN", syntax: "ROUNDDOWN(number, num_digits)", desc: "Rounds toward zero.", min: 2, max: 2, scalar: roundFn(roundDown)},
		&funcDef{name: "TRUNC", syntax: "TRUNC(number, [num_digits])", desc: "Truncates a number toward zero.", min: 1, max: 2, scalar: fnTrunc},
		&funcDef{name: "INT", syntax: "INT(number)", desc: "Rounds down to the nearest integer.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Floor(x)) })},
		&funcDef{name: "ABS", syntax: "ABS(number)", desc: "Absolute value.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Abs(x)) })},
		&funcDef{name: "SQRT", syntax: "SQRT(number)", desc: "Positive square root.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			if x < 0 {
				return ErrorValue(CodeNum)
			}
			return NumberValue(math.Sqrt(x))
		})},
		&funcDef{name: "POWER", syntax: "POWER(number, power)", desc: "Raises a number to a power.", min: 2, max: 2, scalar: num2(power)},
		&funcDef{name: "MOD", syntax: "MOD(number, divisor)", desc: "Remainder with the sign of the divisor.", min: 2, max: 2, scalar: num2(func(n, d float64) Value {
			if d == 0 {
				return ErrorValue(CodeDiv0)
			}
			r := n - d*math.Floor(n/d)
			return NumberValue(toDecimal(r, 15).float())
		})},
		&funcDef{name: "QUOTIENT", syntax: "QUOTIENT(numerator, denominator)", desc: "Integer portion of a division.", min: 2, max: 2, scalar: num2(func(n, d float64) Value {
			if d == 0 {
				return ErrorValue(CodeDiv0)
			}
			return NumberValue(math.Trunc(n / d))
		})},
		&funcDef{name: "PI", syntax: "PI()", desc: "The value of pi.", min: 0, max: 0, scalar: func(*evalCtx, []Value) Value { return NumberValue(math.Pi) }},
		&funcDef{name: "RAND", syntax: "RAND()", desc: "Random number in [0,1); refreshed when the cell is recalculated.", min: 0, max: 0, volatile: true,
			scalar: func(c *evalCtx, _ []Value) Value { return NumberValue(c.e.random()) }},
		&funcDef{name: "RANDBETWEEN", syntax: "RANDBETWEEN(bottom, top)", desc: "Random integer between bottom and top.", min: 2, max: 2, volatile: true,
			scalar: func(c *evalCtx, a []Value) Value {
				lo, e := toNum(a[0])
				if e != "" {
					return ErrorValue(e)
				}
				hi, e := toNum(a[1])
				if e != "" {
					return ErrorValue(e)
				}
				lo, hi = math.Ceil(lo), math.Floor(hi)
				if lo > hi {
					return ErrorValue(CodeNum)
				}
				return NumberValue(lo + math.Floor(c.e.random()*(hi-lo+1)))
			}},
		&funcDef{name: "SIGN", syntax: "SIGN(number)", desc: "Sign of a number: 1, 0 or -1.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			switch {
			case x > 0:
				return NumberValue(1)
			case x < 0:
				return NumberValue(-1)
			}
			return NumberValue(0)
		})},
		&funcDef{name: "EXP", syntax: "EXP(number)", desc: "e raised to a power.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Exp(x)) })},
		&funcDef{name: "LN", syntax: "LN(number)", desc: "Natural logarithm.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			if x <= 0 {
				return ErrorValue(CodeNum)
			}
			return NumberValue(math.Log(x))
		})},
		&funcDef{name: "LOG10", syntax: "LOG10(number)", desc: "Base-10 logarithm.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			if x <= 0 {
				return ErrorValue(CodeNum)
			}
			return NumberValue(math.Log10(x))
		})},
		&funcDef{name: "LOG", syntax: "LOG(number, [base])", desc: "Logarithm to a base (default 10).", min: 1, max: 2, scalar: func(_ *evalCtx, a []Value) Value {
			x, e := toNum(a[0])
			if e != "" {
				return ErrorValue(e)
			}
			b, e := optNum(a, 1, 10)
			if e != "" {
				return ErrorValue(e)
			}
			if x <= 0 || b <= 0 {
				return ErrorValue(CodeNum)
			}
			if b == 1 {
				return ErrorValue(CodeDiv0)
			}
			return NumberValue(math.Log(x) / math.Log(b))
		}},
		&funcDef{name: "CEILING", syntax: "CEILING(number, significance)", desc: "Rounds up to the nearest multiple of significance.", min: 2, max: 2, scalar: num2(func(x, s float64) Value {
			if s == 0 || x == 0 {
				return NumberValue(0)
			}
			if x > 0 && s < 0 {
				return ErrorValue(CodeNum)
			}
			return NumberValue(math.Ceil(toDecimal(x/s, 15).float()) * s)
		})},
		&funcDef{name: "FLOOR", syntax: "FLOOR(number, significance)", desc: "Rounds down to the nearest multiple of significance.", min: 2, max: 2, scalar: num2(func(x, s float64) Value {
			if x == 0 {
				return NumberValue(0)
			}
			if s == 0 {
				return ErrorValue(CodeDiv0)
			}
			if x > 0 && s < 0 {
				return ErrorValue(CodeNum)
			}
			return NumberValue(math.Floor(toDecimal(x/s, 15).float()) * s)
		})},
		&funcDef{name: "MROUND", syntax: "MROUND(number, multiple)", desc: "Rounds to the nearest multiple.", min: 2, max: 2, scalar: num2(func(x, m float64) Value {
			if m == 0 || x == 0 {
				return NumberValue(0)
			}
			if (x > 0) != (m > 0) {
				return ErrorValue(CodeNum)
			}
			q := roundExcel(x/m, 0, roundHalfAway)
			return NumberValue(toDecimal(q*m, 15).float())
		})},
		&funcDef{name: "EVEN", syntax: "EVEN(number)", desc: "Rounds away from zero to the nearest even integer.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			r := math.Ceil(math.Abs(x))
			if math.Mod(r, 2) != 0 {
				r++
			}
			return NumberValue(math.Copysign(r, x))
		})},
		&funcDef{name: "ODD", syntax: "ODD(number)", desc: "Rounds away from zero to the nearest odd integer.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			r := math.Ceil(math.Abs(x))
			if math.Mod(r, 2) == 0 {
				r++
			}
			if x < 0 {
				r = -r
			}
			return NumberValue(r)
		})},
		&funcDef{name: "FACT", syntax: "FACT(number)", desc: "Factorial.", min: 1, max: 1, scalar: num1(func(x float64) Value {
			if x < 0 || x > 170 {
				return ErrorValue(CodeNum)
			}
			r := 1.0
			for i := 2.0; i <= math.Floor(x); i++ {
				r *= i
			}
			return NumberValue(r)
		})},
		&funcDef{name: "SIN", syntax: "SIN(number)", desc: "Sine of an angle in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Sin(x)) })},
		&funcDef{name: "COS", syntax: "COS(number)", desc: "Cosine of an angle in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Cos(x)) })},
		&funcDef{name: "TAN", syntax: "TAN(number)", desc: "Tangent of an angle in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Tan(x)) })},
		&funcDef{name: "ASIN", syntax: "ASIN(number)", desc: "Arcsine in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Asin(x)) })},
		&funcDef{name: "ACOS", syntax: "ACOS(number)", desc: "Arccosine in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Acos(x)) })},
		&funcDef{name: "ATAN", syntax: "ATAN(number)", desc: "Arctangent in radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(math.Atan(x)) })},
		&funcDef{name: "ATAN2", syntax: "ATAN2(x_num, y_num)", desc: "Arctangent of the point (x, y).", min: 2, max: 2, scalar: num2(func(x, y float64) Value {
			if x == 0 && y == 0 {
				return ErrorValue(CodeDiv0)
			}
			return NumberValue(math.Atan2(y, x))
		})},
		&funcDef{name: "DEGREES", syntax: "DEGREES(angle)", desc: "Converts radians to degrees.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(x * 180 / math.Pi) })},
		&funcDef{name: "RADIANS", syntax: "RADIANS(angle)", desc: "Converts degrees to radians.", min: 1, max: 1, scalar: num1(func(x float64) Value { return NumberValue(x * math.Pi / 180) })},
	)
}

func fnSum(c *evalCtx, args []operand) operand {
	s := 0.0
	if e := c.numbers(args, func(x float64, n int) { s += x * float64(n) }); e != "" {
		return errOp(e)
	}
	return numOp(s)
}

func fnProduct(c *evalCtx, args []operand) operand {
	p, seen := 1.0, false
	if e := c.numbers(args, func(x float64, n int) {
		seen = true
		p *= math.Pow(x, float64(n))
	}); e != "" {
		return errOp(e)
	}
	if !seen {
		return numOp(0)
	}
	return numOp(p)
}

func fnSumSq(c *evalCtx, args []operand) operand {
	s := 0.0
	if e := c.numbers(args, func(x float64, n int) { s += x * x * float64(n) }); e != "" {
		return errOp(e)
	}
	return numOp(s)
}

func fnSumProduct(c *evalCtx, args []operand) operand {
	arrs := make([]*array, len(args))
	for i, a := range args {
		arr, ev := c.toArray(a)
		if ev != nil {
			return valOp(*ev)
		}
		if i > 0 && (arr.rows != arrs[0].rows || arr.cols != arrs[0].cols) {
			return errOp(CodeValue)
		}
		arrs[i] = arr
	}
	sr, sc := 0, 0
	for _, a := range arrs {
		sr, sc = max(sr, a.srows), max(sc, a.scols)
	}
	asNum := func(v Value) (float64, string) {
		switch v.Kind {
		case KindNumber:
			return v.Num, ""
		case KindError:
			return 0, v.Err
		}
		return 0, "" // text, logicals and blanks count as zero
	}
	total := 0.0
	for r := 0; r < sr; r++ {
		for col := 0; col < sc; col++ {
			p := 1.0
			for _, a := range arrs {
				x, e := asNum(a.at(r, col))
				if e != "" {
					return errOp(e)
				}
				p *= x
			}
			total += p
		}
	}
	if rest := arrs[0].rows*arrs[0].cols - sr*sc; rest > 0 {
		p := 1.0
		for _, a := range arrs {
			x, e := asNum(a.fill)
			if e != "" {
				return errOp(e)
			}
			p *= x
		}
		total += p * float64(rest)
	}
	return numOp(total)
}

func roundFn(mode roundMode) func(*evalCtx, []Value) Value {
	return num2(func(x, d float64) Value {
		return NumberValue(roundExcel(x, clampInt(d, -400, 400), mode))
	})
}

// clampInt truncates f toward zero and clamps it (avoids undefined
// float→int conversion for huge arguments).
func clampInt(f float64, lo, hi int) int {
	f = math.Trunc(f)
	if f < float64(lo) {
		return lo
	}
	if f > float64(hi) {
		return hi
	}
	return int(f)
}

func fnTrunc(_ *evalCtx, a []Value) Value {
	x, e := toNum(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	d, e := optNum(a, 1, 0)
	if e != "" {
		return ErrorValue(e)
	}
	return NumberValue(roundExcel(x, clampInt(d, -400, 400), roundDown))
}
