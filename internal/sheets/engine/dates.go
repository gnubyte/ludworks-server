package engine

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Excel 1900 date system. Serial 1 = 1900-01-01. Excel (copying Lotus 1-2-3)
// treats 1900 as a leap year, so serial 60 is the fictitious 1900-02-29 and
// every serial ≥ 61 is one more than the real day count. Serial 0 displays
// as 1900-01-00.

// MaxDateSerial is 9999-12-31.
const MaxDateSerial = 2958465

// Date1904Offset is added to 1904-system serials to get 1900-system serials.
const Date1904Offset = 1462

// daysFromCivil returns days since 1970-01-01 for a proleptic Gregorian date
// (Howard Hinnant's algorithm; valid for any int year).
func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
	}
	era := y
	if era < 0 {
		era -= 399
	}
	era /= 400
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// civilFromDays is the inverse of daysFromCivil.
func civilFromDays(z int) (y, m, d int) {
	z += 719468
	era := z
	if era < 0 {
		era -= 146096
	}
	era /= 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	if mp < 10 {
		m = mp + 3
	} else {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return
}

var epoch1899 = daysFromCivil(1899, 12, 31)

// realDateSerial converts a real calendar date to an Excel serial (may be
// negative or out of range; callers check).
func realDateSerial(y, m, d int) int {
	n := daysFromCivil(y, m, d) - epoch1899
	if n >= 60 { // on/after 1900-03-01: skip the phantom 1900-02-29
		n++
	}
	return n
}

// DateSerial implements DATE(y, m, d) normalisation (month/day overflow,
// years 0..1899 offset by 1900). ok=false for results outside 0..MaxDateSerial.
func DateSerial(y, m, d int) (int, bool) {
	if y >= 0 && y <= 1899 {
		y += 1900
	}
	if y < 0 || y > 9999 {
		return 0, false
	}
	tm := y*12 + (m - 1)
	yy := floorDiv(tm, 12)
	mm := tm - yy*12 + 1
	n := realDateSerial(yy, mm, 1) + d - 1
	// DATE(1900,2,29) and day arithmetic around it follow Excel's calendar:
	// days counted from 1900-02-01 pass through serial 60 naturally.
	if n < 0 || n > MaxDateSerial {
		return 0, false
	}
	return n, true
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// SerialToDate converts the integer part of an Excel serial to y/m/d,
// reproducing 1900-01-00 (serial 0) and 1900-02-29 (serial 60).
func SerialToDate(serial int) (y, m, d int) {
	switch {
	case serial == 0:
		return 1900, 1, 0
	case serial == 60:
		return 1900, 2, 29
	case serial < 60:
		return civilFromDays(epoch1899 + serial)
	default:
		return civilFromDays(epoch1899 + serial - 1)
	}
}

// SplitSerial splits a serial into date parts and time of day (rounded to
// the nearest second, carrying into the next day).
func SplitSerial(f float64) (y, mo, d, h, mi, s int, ok bool) {
	if f < 0 || f >= MaxDateSerial+1 || math.IsNaN(f) {
		return 0, 0, 0, 0, 0, 0, false
	}
	days := int(math.Floor(f))
	secs := int(math.Round((f - float64(days)) * 86400))
	if secs >= 86400 {
		days++
		secs -= 86400
	}
	y, mo, d = SerialToDate(days)
	return y, mo, d, secs / 3600, secs / 60 % 60, secs % 60, true
}

// TimeToSerial converts a wall-clock time (in its own location) to a serial.
func TimeToSerial(t time.Time) float64 {
	y, m, d := t.Date()
	day := realDateSerial(y, int(m), d)
	sec := float64(t.Hour()*3600+t.Minute()*60+t.Second()) + float64(t.Nanosecond())/1e9
	return float64(day) + sec/86400
}

// weekdayIndex returns 0=Sunday..6=Saturday for a serial (Excel considers
// serial 1 a Sunday because of the 1900 leap-year bug).
func weekdayIndex(serial int) int {
	return ((serial+6)%7 + 7) % 7
}

var monthNames = [...]string{"January", "February", "March", "April", "May", "June", "July",
	"August", "September", "October", "November", "December"}
var dayNames = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	}
	if y == 1900 {
		return 29 // Excel's fictitious leap day
	}
	if (y%4 == 0 && y%100 != 0) || y%400 == 0 {
		return 29
	}
	return 28
}

// parseDateTimeText recognises common date/time spellings and returns a
// serial. Supported: yyyy-mm-dd, yyyy/mm/dd, m/d/yyyy, m/d/yy, d-mmm-yyyy,
// d mmm yyyy, mmm d yyyy, mmm d, yyyy; each optionally followed by a time
// (hh:mm[:ss][ am|pm]); or a time alone. ISO "T" separators are accepted.
func parseDateTimeText(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return 0, false
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		day, ok := parseDateOnly(s)
		return float64(day), ok
	}
	sep := strings.LastIndexAny(s[:colon], " Tt")
	if sep < 0 {
		return parseTimeText(s)
	}
	day, ok := parseDateOnly(strings.TrimSpace(s[:sep]))
	if !ok {
		return 0, false
	}
	t, ok := parseTimeText(s[sep+1:])
	if !ok {
		return 0, false
	}
	return float64(day) + t, true
}

func parseTimeText(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	pm, am := false, false
	if strings.HasSuffix(s, "pm") || strings.HasSuffix(s, "p") {
		pm = true
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "m"), "p"))
	} else if strings.HasSuffix(s, "am") || strings.HasSuffix(s, "a") {
		am = true
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "m"), "a"))
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || mi < 0 || mi > 59 || h < 0 || len(parts[1]) != 2 {
		return 0, false
	}
	sec := 0.0
	if len(parts) == 3 {
		f, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || f < 0 || f >= 60 {
			return 0, false
		}
		sec = f
	}
	if am || pm {
		if h < 1 || h > 12 {
			return 0, false
		}
		if h == 12 {
			h = 0
		}
		if pm {
			h += 12
		}
	} else if h > 9999 {
		return 0, false
	}
	return (float64(h)*3600 + float64(mi)*60 + sec) / 86400, true
}

func monthFromName(s string) int {
	s = strings.ToLower(s)
	if len(s) < 3 {
		return 0
	}
	for i, n := range monthNames {
		ln := strings.ToLower(n)
		if s == ln || s == ln[:3] || (len(s) >= 3 && strings.HasPrefix(ln, s)) {
			return i + 1
		}
	}
	return 0
}

func parseDateOnly(s string) (int, bool) {
	atoi := func(x string) (int, bool) {
		if x == "" || len(x) > 4 {
			return 0, false
		}
		n, err := strconv.Atoi(x)
		return n, err == nil && n >= 0
	}
	valid := func(y, m, d int) (int, bool) {
		if m < 1 || m > 12 || d < 1 || d > daysInMonth(y, m) || y < 1900 || y > 9999 {
			return 0, false
		}
		return DateSerial(y, m, d)
	}
	twoDigitYear := func(y int, raw string) int {
		if len(raw) <= 2 {
			if y < 30 {
				return 2000 + y
			}
			return 1900 + y
		}
		return y
	}
	for _, sep := range []string{"-", "/", "."} {
		p := strings.Split(s, sep)
		if len(p) != 3 {
			continue
		}
		if len(p[0]) == 4 { // y-m-d
			y, ok1 := atoi(p[0])
			m, ok2 := atoi(p[1])
			d, ok3 := atoi(p[2])
			if ok1 && ok2 && ok3 {
				return valid(y, m, d)
			}
			continue
		}
		if mon := monthFromName(p[1]); mon > 0 { // d-mmm-yyyy
			d, ok1 := atoi(p[0])
			y, ok2 := atoi(p[2])
			if ok1 && ok2 {
				return valid(twoDigitYear(y, p[2]), mon, d)
			}
			continue
		}
		if sep == "/" { // US m/d/yyyy
			m, ok1 := atoi(p[0])
			d, ok2 := atoi(p[1])
			y, ok3 := atoi(p[2])
			if ok1 && ok2 && ok3 {
				return valid(twoDigitYear(y, p[2]), m, d)
			}
		}
	}
	// "d mmm yyyy", "mmm d yyyy", "mmm d, yyyy"
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(f) == 3 {
		if mon := monthFromName(f[1]); mon > 0 {
			d, ok1 := atoi(f[0])
			y, ok2 := atoi(f[2])
			if ok1 && ok2 {
				return valid(twoDigitYear(y, f[2]), mon, d)
			}
		}
		if mon := monthFromName(f[0]); mon > 0 {
			d, ok1 := atoi(f[1])
			y, ok2 := atoi(f[2])
			if ok1 && ok2 {
				return valid(twoDigitYear(y, f[2]), mon, d)
			}
		}
	}
	return 0, false
}
