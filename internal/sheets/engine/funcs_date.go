package engine

import (
	"math"
	"strings"
)

func init() {
	register(
		&funcDef{name: "NOW", syntax: "NOW()", desc: "Current date and time as a serial; refreshed when the cell is recalculated.", min: 0, max: 0, volatile: true,
			scalar: func(c *evalCtx, _ []Value) Value { return NumberValue(TimeToSerial(c.e.now())) }},
		&funcDef{name: "TODAY", syntax: "TODAY()", desc: "Current date as a serial; refreshed when the cell is recalculated.", min: 0, max: 0, volatile: true,
			scalar: func(c *evalCtx, _ []Value) Value { return NumberValue(math.Floor(TimeToSerial(c.e.now()))) }},
		&funcDef{name: "DATE", syntax: "DATE(year, month, day)", desc: "Serial number of a date (1900 date system).", min: 3, max: 3, scalar: fnDate},
		&funcDef{name: "TIME", syntax: "TIME(hour, minute, second)", desc: "Fraction of a day for a time.", min: 3, max: 3, scalar: fnTime},
		&funcDef{name: "YEAR", syntax: "YEAR(serial_number)", desc: "Year of a date.", min: 1, max: 1, scalar: datePart(func(y, m, d int) int { return y })},
		&funcDef{name: "MONTH", syntax: "MONTH(serial_number)", desc: "Month of a date (1-12).", min: 1, max: 1, scalar: datePart(func(y, m, d int) int { return m })},
		&funcDef{name: "DAY", syntax: "DAY(serial_number)", desc: "Day of the month (1-31).", min: 1, max: 1, scalar: datePart(func(y, m, d int) int { return d })},
		&funcDef{name: "HOUR", syntax: "HOUR(serial_number)", desc: "Hour (0-23).", min: 1, max: 1, scalar: timePart(0)},
		&funcDef{name: "MINUTE", syntax: "MINUTE(serial_number)", desc: "Minute (0-59).", min: 1, max: 1, scalar: timePart(1)},
		&funcDef{name: "SECOND", syntax: "SECOND(serial_number)", desc: "Second (0-59).", min: 1, max: 1, scalar: timePart(2)},
		&funcDef{name: "WEEKDAY", syntax: "WEEKDAY(serial_number, [return_type])", desc: "Day of the week (type 1: Sunday=1 … Saturday=7).", min: 1, max: 2, scalar: fnWeekday},
		&funcDef{name: "WEEKNUM", syntax: "WEEKNUM(serial_number, [return_type])", desc: "Week of the year (type 1: weeks start Sunday, 2: Monday).", min: 1, max: 2, scalar: fnWeeknum},
		&funcDef{name: "EDATE", syntax: "EDATE(start_date, months)", desc: "Date a number of months before/after a date.", min: 2, max: 2, scalar: edate(false)},
		&funcDef{name: "EOMONTH", syntax: "EOMONTH(start_date, months)", desc: "Last day of the month a number of months away.", min: 2, max: 2, scalar: edate(true)},
		&funcDef{name: "DAYS", syntax: "DAYS(end_date, start_date)", desc: "Number of days between two dates.", min: 2, max: 2, scalar: func(_ *evalCtx, a []Value) Value {
			e, err := serialArg(a[0])
			if err != "" {
				return ErrorValue(err)
			}
			s, err := serialArg(a[1])
			if err != "" {
				return ErrorValue(err)
			}
			return NumberValue(math.Floor(e) - math.Floor(s))
		}},
		&funcDef{name: "DATEDIF", syntax: `DATEDIF(start_date, end_date, unit)`, desc: `Difference between dates in "Y", "M", "D", "MD", "YM" or "YD".`, min: 3, max: 3, scalar: fnDatedif},
		&funcDef{name: "DATEVALUE", syntax: "DATEVALUE(date_text)", desc: "Converts date text to a serial number.", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			if a[0].Kind != KindString {
				if a[0].Kind == KindError {
					return a[0]
				}
				return ErrorValue(CodeValue)
			}
			f, ok := parseDateTimeText(a[0].Str)
			if !ok || f < 1 {
				return ErrorValue(CodeValue)
			}
			return NumberValue(math.Floor(f))
		}},
		&funcDef{name: "TIMEVALUE", syntax: "TIMEVALUE(time_text)", desc: "Converts time text to a fraction of a day.", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			if a[0].Kind != KindString {
				if a[0].Kind == KindError {
					return a[0]
				}
				return ErrorValue(CodeValue)
			}
			f, ok := parseDateTimeText(a[0].Str)
			if !ok {
				return ErrorValue(CodeValue)
			}
			return NumberValue(f - math.Floor(f))
		}},
	)
}

// serialArg coerces a date argument and checks the valid serial range.
func serialArg(v Value) (float64, string) {
	f, e := toNum(v)
	if e != "" {
		return 0, e
	}
	if f < 0 || f >= MaxDateSerial+1 {
		return 0, CodeNum
	}
	return f, ""
}

func fnDate(_ *evalCtx, a []Value) Value {
	var p [3]int
	for i := 0; i < 3; i++ {
		f, e := toNum(a[i])
		if e != "" {
			return ErrorValue(e)
		}
		p[i] = clampInt(f, -1<<30, 1<<30)
	}
	if p[1] > 1e6 || p[1] < -1e6 || p[2] > 1e8 || p[2] < -1e8 {
		return ErrorValue(CodeNum)
	}
	n, ok := DateSerial(p[0], p[1], p[2])
	if !ok {
		return ErrorValue(CodeNum)
	}
	return NumberValue(float64(n))
}

func fnTime(_ *evalCtx, a []Value) Value {
	var p [3]float64
	for i := 0; i < 3; i++ {
		f, e := toNum(a[i])
		if e != "" {
			return ErrorValue(e)
		}
		f = math.Trunc(f)
		if f > 32767 {
			return ErrorValue(CodeNum)
		}
		p[i] = f
	}
	secs := p[0]*3600 + p[1]*60 + p[2]
	if secs < 0 {
		return ErrorValue(CodeNum)
	}
	return NumberValue(math.Mod(secs, 86400) / 86400)
}

func datePart(get func(y, m, d int) int) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		f, e := serialArg(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		y, m, d, _, _, _, _ := SplitSerial(f)
		return NumberValue(float64(get(y, m, d)))
	}
}

func timePart(which int) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		f, e := serialArg(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		_, _, _, h, mi, s, _ := SplitSerial(f)
		return NumberValue(float64([3]int{h, mi, s}[which]))
	}
}

func fnWeekday(_ *evalCtx, a []Value) Value {
	f, e := serialArg(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	t, e := optNum(a, 1, 1)
	if e != "" {
		return ErrorValue(e)
	}
	wd := weekdayIndex(int(math.Floor(f))) // 0 = Sunday
	switch int(t) {
	case 1, 17:
		return NumberValue(float64(wd + 1))
	case 2, 11:
		return NumberValue(float64((wd+6)%7 + 1))
	case 3:
		return NumberValue(float64((wd + 6) % 7))
	case 12, 13, 14, 15, 16: // week starts Tuesday..Saturday
		start := int(t) - 10 // 2=Tue ... 6=Sat
		return NumberValue(float64((wd-start+7)%7 + 1))
	}
	return ErrorValue(CodeNum)
}

func fnWeeknum(_ *evalCtx, a []Value) Value {
	f, e := serialArg(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	t, e := optNum(a, 1, 1)
	if e != "" {
		return ErrorValue(e)
	}
	day := int(math.Floor(f))
	y, _, _ := SerialToDate(day)
	jan1, _ := DateSerial(y, 1, 1)
	wd := weekdayIndex(jan1)
	switch int(t) {
	case 1, 17:
	case 2, 11:
		wd = (wd + 6) % 7
	default:
		return ErrorValue(CodeNum)
	}
	return NumberValue(float64((day-jan1+wd)/7 + 1))
}

func edate(eom bool) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		f, e := serialArg(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		mf, e := toNum(a[1])
		if e != "" {
			return ErrorValue(e)
		}
		months := clampInt(mf, -200000, 200000)
		y, m, d := SerialToDate(int(math.Floor(f)))
		tm := y*12 + (m - 1) + months
		ny := floorDiv(tm, 12)
		nm := tm - ny*12 + 1
		if ny < 1900 || ny > 9999 {
			return ErrorValue(CodeNum)
		}
		last := daysInMonth(ny, nm)
		if ny == 1900 && nm == 2 {
			last = 28 // EDATE/EOMONTH never land on the phantom 1900-02-29
		}
		if eom || d > last {
			d = last
		}
		n, ok := DateSerial(ny, nm, d)
		if !ok {
			return ErrorValue(CodeNum)
		}
		return NumberValue(float64(n))
	}
}

func fnDatedif(_ *evalCtx, a []Value) Value {
	s, e := serialArg(a[0])
	if e != "" {
		return ErrorValue(e)
	}
	en, e := serialArg(a[1])
	if e != "" {
		return ErrorValue(e)
	}
	unit, e := toText(a[2])
	if e != "" {
		return ErrorValue(e)
	}
	si, ei := int(math.Floor(s)), int(math.Floor(en))
	if si > ei {
		return ErrorValue(CodeNum)
	}
	y1, m1, d1 := SerialToDate(si)
	y2, m2, d2 := SerialToDate(ei)
	months := (y2-y1)*12 + (m2 - m1)
	if d2 < d1 {
		months--
	}
	switch strings.ToUpper(unit) {
	case "D":
		return NumberValue(float64(ei - si))
	case "M":
		return NumberValue(float64(months))
	case "Y":
		return NumberValue(float64(months / 12))
	case "YM":
		return NumberValue(float64(months % 12))
	case "MD":
		if d2 >= d1 {
			return NumberValue(float64(d2 - d1))
		}
		pm, py := m2-1, y2
		if pm == 0 {
			pm, py = 12, y2-1
		}
		return NumberValue(float64(daysInMonth(py, pm) - d1 + d2))
	case "YD":
		ny := y2
		if m2 < m1 || (m2 == m1 && d2 < d1) {
			ny--
		}
		dd := min(d1, daysInMonth(ny, m1))
		st, ok := DateSerial(ny, m1, dd)
		if !ok {
			return ErrorValue(CodeNum)
		}
		return NumberValue(float64(ei - st))
	}
	return ErrorValue(CodeNum)
}
