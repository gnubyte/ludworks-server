package engine

import (
	"math"
	"sort"
)

func init() {
	register(
		&funcDef{name: "AVERAGE", syntax: "AVERAGE(number1, [number2], ...)", desc: "Arithmetic mean of the numbers.", min: 1, max: -1, eager: fnAverage},
		&funcDef{name: "MIN", syntax: "MIN(number1, [number2], ...)", desc: "Smallest number (0 when there are none).", min: 1, max: -1, eager: minMax(false)},
		&funcDef{name: "MAX", syntax: "MAX(number1, [number2], ...)", desc: "Largest number (0 when there are none).", min: 1, max: -1, eager: minMax(true)},
		&funcDef{name: "COUNT", syntax: "COUNT(value1, [value2], ...)", desc: "Counts numbers.", min: 1, max: -1, eager: fnCount},
		&funcDef{name: "COUNTA", syntax: "COUNTA(value1, [value2], ...)", desc: "Counts non-empty values.", min: 1, max: -1, eager: fnCountA},
		&funcDef{name: "COUNTBLANK", syntax: "COUNTBLANK(range)", desc: "Counts empty cells (including empty text).", min: 1, max: 1, eager: fnCountBlank},
		&funcDef{name: "MEDIAN", syntax: "MEDIAN(number1, [number2], ...)", desc: "Median of the numbers.", min: 1, max: -1, eager: fnMedian},
		&funcDef{name: "MODE", syntax: "MODE(number1, [number2], ...)", desc: "Most frequently occurring number.", min: 1, max: -1, eager: fnMode},
		&funcDef{name: "MODE.SNGL", syntax: "MODE.SNGL(number1, [number2], ...)", desc: "Most frequently occurring number.", min: 1, max: -1, eager: fnMode},
		&funcDef{name: "STDEV", syntax: "STDEV(number1, [number2], ...)", desc: "Sample standard deviation.", min: 1, max: -1, eager: variance(true, true)},
		&funcDef{name: "STDEV.S", syntax: "STDEV.S(number1, [number2], ...)", desc: "Sample standard deviation.", min: 1, max: -1, eager: variance(true, true)},
		&funcDef{name: "STDEV.P", syntax: "STDEV.P(number1, [number2], ...)", desc: "Population standard deviation.", min: 1, max: -1, eager: variance(false, true)},
		&funcDef{name: "STDEVP", syntax: "STDEVP(number1, [number2], ...)", desc: "Population standard deviation.", min: 1, max: -1, eager: variance(false, true)},
		&funcDef{name: "VAR", syntax: "VAR(number1, [number2], ...)", desc: "Sample variance.", min: 1, max: -1, eager: variance(true, false)},
		&funcDef{name: "VAR.S", syntax: "VAR.S(number1, [number2], ...)", desc: "Sample variance.", min: 1, max: -1, eager: variance(true, false)},
		&funcDef{name: "VAR.P", syntax: "VAR.P(number1, [number2], ...)", desc: "Population variance.", min: 1, max: -1, eager: variance(false, false)},
		&funcDef{name: "VARP", syntax: "VARP(number1, [number2], ...)", desc: "Population variance.", min: 1, max: -1, eager: variance(false, false)},
		&funcDef{name: "LARGE", syntax: "LARGE(array, k)", desc: "k-th largest value.", min: 2, max: 2, eager: kth(true)},
		&funcDef{name: "SMALL", syntax: "SMALL(array, k)", desc: "k-th smallest value.", min: 2, max: 2, eager: kth(false)},
		&funcDef{name: "RANK", syntax: "RANK(number, ref, [order])", desc: "Rank of a number in a list (order 0 = descending).", min: 2, max: 3, eager: fnRank},
		&funcDef{name: "RANK.EQ", syntax: "RANK.EQ(number, ref, [order])", desc: "Rank of a number in a list (order 0 = descending).", min: 2, max: 3, eager: fnRank},
	)
}

func fnAverage(c *evalCtx, args []operand) operand {
	s, n := 0.0, 0
	if e := c.numbers(args, func(x float64, k int) { s += x * float64(k); n += k }); e != "" {
		return errOp(e)
	}
	if n == 0 {
		return errOp(CodeDiv0)
	}
	return numOp(s / float64(n))
}

func minMax(isMax bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		best, seen := 0.0, false
		if e := c.numbers(args, func(x float64, _ int) {
			if !seen || (isMax && x > best) || (!isMax && x < best) {
				best, seen = x, true
			}
		}); e != "" {
			return errOp(e)
		}
		return numOp(best)
	}
}

func fnCount(c *evalCtx, args []operand) operand {
	n := 0
	c.visit(args, func(v Value, k int, direct bool) bool {
		switch v.Kind {
		case KindNumber:
			n += k
		case KindBool:
			if direct {
				n += k
			}
		case KindString:
			if direct {
				if _, ok := ParseNumberText(v.Str); ok {
					n += k
				}
			}
		}
		return true
	})
	return numOp(float64(n))
}

func fnCountA(c *evalCtx, args []operand) operand {
	n := 0
	c.visit(args, func(v Value, k int, _ bool) bool {
		if !v.IsEmpty() {
			n += k
		}
		return true
	})
	return numOp(float64(n))
}

func isBlankish(v Value) bool {
	return v.IsEmpty() || (v.Kind == KindString && v.Str == "")
}

func fnCountBlank(c *evalCtx, args []operand) operand {
	a := args[0]
	switch a.k {
	case opRef:
		nonBlank := 0
		c.e.eachCell(a.s, a.r, false, func(_ Ref, cell *Cell) bool {
			if !isBlankish(cell.Value) {
				nonBlank++
			}
			return true
		})
		return numOp(float64(a.r.Area() - nonBlank))
	case opArr:
		n := 0
		for _, v := range a.arr.vals {
			if isBlankish(v) {
				n++
			}
		}
		if isBlankish(a.arr.fill) {
			n += a.arr.fillCount()
		}
		return numOp(float64(n))
	}
	if isBlankish(a.v) {
		return numOp(1)
	}
	return numOp(0)
}

func fnMedian(c *evalCtx, args []operand) operand {
	xs, e := c.numberList(args)
	if e != "" {
		return errOp(e)
	}
	if len(xs) == 0 {
		return errOp(CodeNum)
	}
	sort.Float64s(xs)
	m := len(xs) / 2
	if len(xs)%2 == 1 {
		return numOp(xs[m])
	}
	return numOp((xs[m-1] + xs[m]) / 2)
}

func fnMode(c *evalCtx, args []operand) operand {
	xs, e := c.numberList(args)
	if e != "" {
		return errOp(e)
	}
	counts := map[float64]int{}
	best, bestN := 0.0, 1
	for _, x := range xs { // first value reaching the highest count wins, like Excel
		counts[x]++
		if counts[x] > bestN {
			best, bestN = x, counts[x]
		}
	}
	if bestN < 2 {
		return errOp(CodeNA)
	}
	return numOp(best)
}

func variance(sample, sqrt bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		// Welford's algorithm for numerical stability.
		n, mean, m2 := 0.0, 0.0, 0.0
		if e := c.numbers(args, func(x float64, k int) {
			for i := 0; i < k; i++ {
				n++
				d := x - mean
				mean += d / n
				m2 += d * (x - mean)
			}
		}); e != "" {
			return errOp(e)
		}
		den := n
		if sample {
			den = n - 1
		}
		if den <= 0 {
			return errOp(CodeDiv0)
		}
		v := m2 / den
		if sqrt {
			v = math.Sqrt(v)
		}
		return numOp(v)
	}
}

func kth(largest bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		xs, e := c.numberList(args[:1])
		if e != "" {
			return errOp(e)
		}
		kf, e := toNum(c.scalar(args[1]))
		if e != "" {
			return errOp(e)
		}
		k := int(math.Ceil(kf))
		if k < 1 || k > len(xs) {
			return errOp(CodeNum)
		}
		sort.Float64s(xs)
		if largest {
			return numOp(xs[len(xs)-k])
		}
		return numOp(xs[k-1])
	}
}

func fnRank(c *evalCtx, args []operand) operand {
	x, e := toNum(c.scalar(args[0]))
	if e != "" {
		return errOp(e)
	}
	asc := false
	if len(args) > 2 {
		o, e := toNum(c.scalar(args[2]))
		if e != "" {
			return errOp(e)
		}
		asc = o != 0
	}
	if args[1].k == opVal {
		return errOp(CodeValue)
	}
	rank, found := 1, false
	ecode := c.numbers(args[1:2], func(y float64, k int) {
		if y == x {
			found = true
		}
		if (!asc && y > x) || (asc && y < x) {
			rank += k
		}
	})
	if ecode != "" {
		return errOp(ecode)
	}
	if !found {
		return errOp(CodeNA)
	}
	return numOp(float64(rank))
}
