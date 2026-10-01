package template

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Python's datetime.datetime and datetime.timedelta, as the to_datetime
// filter returns them and templates use them: attributes, a few
// methods, subtraction, addition and comparison. Variable storage keeps a
// datetime (it shows as its isoformat()) and rejects a timedelta.

// pyObject is a value with Python attributes (datetime's year, a
// timedelta's days, their methods).
type pyObject interface {
	PyAttr(name string) (any, bool)
}

// pyTZ is a datetime.timezone: a fixed UTC offset.
type pyTZ struct {
	offset time.Duration
	name   string // the %Z name it was parsed with, if any
}

// pyDatetime is a datetime.datetime: its wall clock (in UTC) and, when
// aware, its timezone.
type pyDatetime struct {
	t  time.Time
	tz *pyTZ
}

// pyTimedelta is a datetime.timedelta in microseconds.
type pyTimedelta struct{ us int64 }

const usPerDay = 86400 * 1000000

func (d pyTimedelta) parts() (days, secs, us int64) {
	days = floorDiv(d.us, usPerDay)
	rem := d.us - days*usPerDay
	return days, rem / 1000000, rem % 1000000
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func (d pyTimedelta) String() string {
	days, secs, us := d.parts()
	s := fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	if days != 0 {
		plural := "s"
		if days == 1 || days == -1 {
			plural = ""
		}
		s = fmt.Sprintf("%d day%s, ", days, plural) + s
	}
	if us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func (d pyTimedelta) PyRepr() string {
	days, secs, us := d.parts()
	var args []string
	if days != 0 {
		args = append(args, fmt.Sprintf("days=%d", days))
	}
	if secs != 0 {
		args = append(args, fmt.Sprintf("seconds=%d", secs))
	}
	if us != 0 {
		args = append(args, fmt.Sprintf("microseconds=%d", us))
	}
	if len(args) == 0 {
		args = []string{"0"}
	}
	return "datetime.timedelta(" + strings.Join(args, ", ") + ")"
}

func (d pyTimedelta) PyAttr(name string) (any, bool) {
	days, secs, us := d.parts()
	switch name {
	case "days":
		return days, true
	case "seconds":
		return secs, true
	case "microseconds":
		return us, true
	case "total_seconds":
		return boundMethod(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
			if err := noArgs("total_seconds", args, kwargs); err != nil {
				return nil, err
			}
			return float64((days*86400+secs)*1000000+us) / 1e6, nil
		}), true
	}
	return nil, false
}

// tzSuffix is a UTC offset as str() and isoformat() show it: +HH:MM,
// with seconds and microseconds when present.
func tzSuffix(off time.Duration) string {
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	h, m := int64(off/time.Hour), int64(off/time.Minute)%60
	s, us := int64(off/time.Second)%60, int64(off/time.Microsecond)%1000000
	out := fmt.Sprintf("%s%02d:%02d", sign, h, m)
	if s != 0 || us != 0 {
		out += fmt.Sprintf(":%02d", s)
		if us != 0 {
			out += fmt.Sprintf(".%06d", us)
		}
	}
	return out
}

func (d pyDatetime) isoformat(sep string) string {
	t := d.t
	s := fmt.Sprintf("%04d-%02d-%02d%s%02d:%02d:%02d", t.Year(), int(t.Month()), t.Day(), sep, t.Hour(), t.Minute(), t.Second())
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if d.tz != nil {
		s += tzSuffix(d.tz.offset)
	}
	return s
}

// Isoformat is datetime.isoformat(), which ansible-core's JSON encoders
// emit for a datetime.
func (d pyDatetime) Isoformat() string { return d.isoformat("T") }

func (d pyDatetime) String() string { return d.isoformat(" ") }

func (d pyDatetime) PyRepr() string {
	t := d.t
	args := []string{strconv.Itoa(t.Year()), strconv.Itoa(int(t.Month())), strconv.Itoa(t.Day()),
		strconv.Itoa(t.Hour()), strconv.Itoa(t.Minute())}
	us := t.Nanosecond() / 1000
	if t.Second() != 0 || us != 0 {
		args = append(args, strconv.Itoa(t.Second()))
	}
	if us != 0 {
		args = append(args, strconv.Itoa(us))
	}
	if d.tz != nil {
		args = append(args, "tzinfo="+d.tz.repr())
	}
	return "datetime.datetime(" + strings.Join(args, ", ") + ")"
}

func (tz *pyTZ) repr() string {
	if tz.offset == 0 && tz.name == "" {
		return "datetime.timezone.utc"
	}
	td := pyTimedelta{int64(tz.offset / time.Microsecond)}.PyRepr()
	if tz.name != "" {
		return "datetime.timezone(" + td + ", " + pyStrRepr(tz.name) + ")"
	}
	return "datetime.timezone(" + td + ")"
}

// instant is the datetime as a point in time (aware), or its wall clock.
func (d pyDatetime) instant() time.Time {
	if d.tz == nil {
		return d.t
	}
	return d.t.Add(-d.tz.offset)
}

func (d pyDatetime) PyAttr(name string) (any, bool) {
	t := d.t
	switch name {
	case "year":
		return int64(t.Year()), true
	case "month":
		return int64(t.Month()), true
	case "day":
		return int64(t.Day()), true
	case "hour":
		return int64(t.Hour()), true
	case "minute":
		return int64(t.Minute()), true
	case "second":
		return int64(t.Second()), true
	case "microsecond":
		return int64(t.Nanosecond() / 1000), true
	case "tzinfo":
		if d.tz == nil {
			return nil, true
		}
	}
	method := func(f func(args []any, kwargs map[string]any) (any, error)) (any, bool) {
		return boundMethod(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
			return f(args, kwargs)
		}), true
	}
	switch name {
	case "strftime":
		return method(func(args []any, kwargs map[string]any) (any, error) {
			if len(args) != 1 || len(kwargs) > 0 {
				return nil, fmt.Errorf("strftime() takes exactly 1 argument (%d given)", len(args)+len(kwargs))
			}
			f, ok := asString(Undeprecate(args[0]))
			if !ok {
				return nil, fmt.Errorf("strftime() argument 1 must be str, not %s", pyClassName(args[0], false))
			}
			var tz *time.Location
			if d.tz != nil {
				name := d.tz.name
				if name == "" {
					name = "UTC"
					if d.tz.offset != 0 {
						name += tzSuffix(d.tz.offset)
					}
				}
				tz = time.FixedZone(name, int(d.tz.offset/time.Second))
			}
			return strftimeTime(f, d.t, tz, d.tz == nil), nil
		})
	case "isoformat":
		return method(func(args []any, kwargs map[string]any) (any, error) {
			sep := "T"
			if v, ok := filterArg(args, 0, kwargs, "sep"); ok {
				s, isStr := asString(Undeprecate(v))
				if !isStr || len([]rune(s)) != 1 {
					return nil, errors.New("isoformat() argument 1 must be a unicode character, not " + pyClassName(v, false))
				}
				sep = s
			}
			return d.isoformat(sep), nil
		})
	case "timestamp":
		return method(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs("timestamp", args, kwargs); err != nil {
				return nil, err
			}
			var at time.Time
			if d.tz != nil {
				at = d.instant()
			} else {
				t := d.t
				at = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
			}
			return float64(at.Unix()) + float64(at.Nanosecond()/1000)/1e6, nil
		})
	case "weekday", "isoweekday":
		return method(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			wd := (int64(t.Weekday()) + 6) % 7
			if name == "isoweekday" {
				wd++
			}
			return wd, nil
		})
	}
	return nil, false
}

func noArgs(name string, args []any, kwargs map[string]any) error {
	if len(args) > 0 || len(kwargs) > 0 {
		return fmt.Errorf("%s() takes no arguments (%d given)", name, len(args)+len(kwargs))
	}
	return nil
}

var errDateOverflow = errors.New("date value out of range")

func (d pyDatetime) add(us int64) (pyDatetime, error) {
	t := d.t.Add(time.Duration(us) * time.Microsecond)
	if t.Year() < 1 || t.Year() > 9999 {
		return pyDatetime{}, errDateOverflow
	}
	return pyDatetime{t: t, tz: d.tz}, nil
}

// pyObjArith is a - b, a + b and the like for datetimes and timedeltas
// (handled reports whether either operand is one).
func pyObjArith(op tokKind, a, b any) (out any, handled bool, err error) {
	da, aDT := a.(pyDatetime)
	db, bDT := b.(pyDatetime)
	ta, aTD := a.(pyTimedelta)
	tb, bTD := b.(pyTimedelta)
	if !aDT && !bDT && !aTD && !bTD {
		return nil, false, nil
	}
	switch {
	case op == tokSub && aDT && bDT:
		if (da.tz == nil) != (db.tz == nil) {
			return nil, true, errors.New("can't subtract offset-naive and offset-aware datetimes")
		}
		return pyTimedelta{int64(da.instant().Sub(db.instant()) / time.Microsecond)}, true, nil
	case op == tokSub && aDT && bTD:
		r, err := da.add(-tb.us)
		return r, true, err
	case op == tokAdd && aDT && bTD:
		r, err := da.add(tb.us)
		return r, true, err
	case op == tokAdd && aTD && bDT:
		r, err := db.add(ta.us)
		return r, true, err
	case op == tokAdd && aTD && bTD:
		return pyTimedelta{ta.us + tb.us}, true, nil
	case op == tokSub && aTD && bTD:
		return pyTimedelta{ta.us - tb.us}, true, nil
	case op == tokMul && aTD:
		if n, ok := asInt(b); ok {
			return pyTimedelta{ta.us * n}, true, nil
		}
	case op == tokMul && bTD:
		if n, ok := asInt(a); ok {
			return pyTimedelta{tb.us * n}, true, nil
		}
	case op == tokDiv && aTD && bTD:
		if tb.us == 0 {
			return nil, true, errors.New("division by zero")
		}
		return float64(ta.us) / float64(tb.us), true, nil
	case op == tokFloorDiv && aTD && bTD:
		if tb.us == 0 {
			return nil, true, errors.New("integer division or modulo by zero")
		}
		return floorDiv(ta.us, tb.us), true, nil
	}
	return nil, true, fmt.Errorf("unsupported operand type(s) for %s: '%s' and '%s'", opName(op), pyClassName(a, false), pyClassName(b, false))
}

// pyObjCompare orders datetimes and timedeltas (handled reports whether
// either operand is one).
func pyObjCompare(a, b any, op string) (c int, handled bool, err error) {
	switch x := a.(type) {
	case pyDatetime:
		y, ok := b.(pyDatetime)
		if !ok {
			return 0, false, nil
		}
		if (x.tz == nil) != (y.tz == nil) {
			return 0, true, errors.New("can't compare offset-naive and offset-aware datetimes")
		}
		return x.instant().Compare(y.instant()), true, nil
	case pyTimedelta:
		y, ok := b.(pyTimedelta)
		if !ok {
			return 0, false, nil
		}
		switch {
		case x.us < y.us:
			return -1, true, nil
		case x.us > y.us:
			return 1, true, nil
		}
		return 0, true, nil
	}
	return 0, false, nil
}

// pyObjEqual is a == b for datetimes and timedeltas.
func pyObjEqual(a, b any) (eq, handled bool) {
	switch x := a.(type) {
	case pyDatetime:
		y, ok := b.(pyDatetime)
		if !ok {
			return false, true
		}
		if (x.tz == nil) != (y.tz == nil) {
			return false, true
		}
		return x.instant().Equal(y.instant()), true
	case pyTimedelta:
		y, ok := b.(pyTimedelta)
		return ok && x.us == y.us, true
	}
	return false, false
}

// strptime is datetime.datetime.strptime(data, format) in the C locale.
func strptime(data, format string) (pyDatetime, error) {
	re, err := strptimeRegex(format)
	if err != nil {
		return pyDatetime{}, err
	}
	m := re.FindStringSubmatchIndex(data)
	if m == nil {
		return pyDatetime{}, fmt.Errorf("time data %s does not match format %s", pyStrRepr(data), pyStrRepr(format))
	}
	if m[1] != len(data) {
		return pyDatetime{}, fmt.Errorf("unconverted data remains: %s", data[m[1]:])
	}
	found := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" && m[2*i] >= 0 {
			found[name] = data[m[2*i]:m[2*i+1]]
		}
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
	year, haveYear := 0, false
	month, day := 1, 1
	hour, minute, second, fraction := 0, 0, 0, 0
	var tz *pyTZ
	weekday, julian := -1, -1
	weekOfYear, weekStartMon := -1, false
	if v, ok := found["y"]; ok {
		year, haveYear = atoi(v), true
		if year <= 68 {
			year += 2000
		} else {
			year += 1900
		}
	}
	if v, ok := found["Y"]; ok {
		year, haveYear = atoi(v), true
	}
	if v, ok := found["m"]; ok {
		month = atoi(v)
	}
	if v, ok := found["B"]; ok {
		month = indexFold(longMonths, v) + 1
	}
	if v, ok := found["b"]; ok {
		month = indexFold(shortMonths, v) + 1
	}
	if v, ok := found["d"]; ok {
		day = atoi(v)
	}
	if v, ok := found["H"]; ok {
		hour = atoi(v)
	}
	if v, ok := found["I"]; ok {
		hour = atoi(v)
		switch strings.ToLower(found["p"]) {
		case "", "am":
			if hour == 12 {
				hour = 0
			}
		case "pm":
			if hour != 12 {
				hour += 12
			}
		}
	}
	if v, ok := found["M"]; ok {
		minute = atoi(v)
	}
	if v, ok := found["S"]; ok {
		second = atoi(v)
	}
	if v, ok := found["f"]; ok {
		fraction = atoi(v + strings.Repeat("0", 6-len(v)))
	}
	if v, ok := found["A"]; ok {
		weekday = indexFold(longDays, v)
	}
	if v, ok := found["a"]; ok {
		weekday = indexFold(shortDays, v)
	}
	if v, ok := found["w"]; ok {
		weekday = (atoi(v) + 6) % 7
	}
	if v, ok := found["u"]; ok {
		weekday = atoi(v) - 1
	}
	if v, ok := found["j"]; ok {
		julian = atoi(v)
	}
	if v, ok := found["U"]; ok {
		weekOfYear = atoi(v)
	}
	if v, ok := found["W"]; ok {
		weekOfYear, weekStartMon = atoi(v), true
	}
	if v, ok := found["z"]; ok {
		if v == "Z" {
			tz = &pyTZ{}
		} else {
			z := v
			if z[3] == ':' {
				z = z[:3] + z[4:]
				if len(z) > 5 {
					if z[5] != ':' {
						return pyDatetime{}, fmt.Errorf("Inconsistent use of : in %s", v)
					}
					z = z[:5] + z[6:]
				}
			}
			secs := atoi(z[1:3])*3600 + atoi(z[3:5])*60
			if len(z) >= 7 {
				secs += atoi(z[5:7])
			}
			frac := 0
			if len(z) > 8 {
				r := z[8:]
				frac = atoi(r + strings.Repeat("0", 6-len(r)))
			}
			off := time.Duration(secs)*time.Second + time.Duration(frac)*time.Microsecond
			if z[0] == '-' {
				off = -off
			}
			tz = &pyTZ{offset: off}
		}
		if tz != nil {
			if name, ok := found["Z"]; ok {
				tz.name = name
			}
		}
	}
	leapFix := false
	if !haveYear {
		if month == 2 && day == 29 {
			year, leapFix = 1904, true
		} else {
			year = 1900
		}
	}
	if julian < 0 && weekday >= 0 && weekOfYear >= 0 {
		julian = julianFromWeek(year, weekOfYear, weekday, weekStartMon)
		if julian <= 0 {
			year--
			julian += 365
			if isLeap(year) {
				julian++
			}
		}
	}
	if julian < 0 {
		if err := checkDate(year, month, day); err != nil {
			return pyDatetime{}, err
		}
	} else {
		t := time.Date(year, 1, julian, 0, 0, 0, 0, time.UTC)
		year, month, day = t.Year(), int(t.Month()), t.Day()
	}
	if leapFix {
		year = 1900
	}
	if err := checkDate(year, month, day); err != nil {
		return pyDatetime{}, err
	}
	if second > 59 {
		return pyDatetime{}, fmt.Errorf("second must be in 0..59, not %d", second)
	}
	return pyDatetime{t: time.Date(year, time.Month(month), day, hour, minute, second, fraction*1000, time.UTC), tz: tz}, nil
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// checkDate is datetime.date's range check.
func checkDate(y, m, d int) error {
	if y < 1 || y > 9999 {
		return fmt.Errorf("year %d is out of range", y)
	}
	dim := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m-1]
	if m == 2 && isLeap(y) {
		dim = 29
	}
	if d < 1 || d > dim {
		return fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", d, dim, m, y)
	}
	return nil
}

// julianFromWeek is _strptime's _calc_julian_from_U_or_W.
func julianFromWeek(year, week, weekday int, startMon bool) int {
	first := int((time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday() + 6) % 7)
	if !startMon {
		first = (first + 1) % 7
		weekday = (weekday + 1) % 7
	}
	weekZeroLen := (7 - first) % 7
	if week == 0 {
		return 1 + weekday - first
	}
	daysToWeek := weekZeroLen + 7*(week-1)
	return 1 + daysToWeek + weekday
}

var (
	longMonths  = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	shortMonths = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	longDays    = []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	shortDays   = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

func indexFold(list []string, s string) int {
	for i, x := range list {
		if strings.EqualFold(x, s) {
			return i
		}
	}
	return -1
}

// seqRE is _strptime's __seqToRE: the names longest first.
func seqRE(names []string, group string) string {
	sorted := append([]string(nil), names...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && len(sorted[j]) > len(sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return "(?P<" + group + ">" + strings.Join(sorted, "|") + ")"
}

var strptimeDirectives = map[string]string{
	"d": `(?P<d>3[0-1]|[1-2]\d|0[1-9]|[1-9]| [1-9])`,
	"f": `(?P<f>[0-9]{1,6})`,
	"H": `(?P<H>2[0-3]|[0-1]\d|\d| \d)`,
	"k": `(?P<H>2[0-3]|[0-1]\d|\d| \d)`,
	"I": `(?P<I>1[0-2]|0[1-9]|[1-9]| [1-9])`,
	"l": `(?P<I>1[0-2]|0[1-9]|[1-9]| [1-9])`,
	"j": `(?P<j>36[0-6]|3[0-5]\d|[1-2]\d\d|0[1-9]\d|00[1-9]|[1-9]\d|0[1-9]|[1-9])`,
	"m": `(?P<m>1[0-2]|0[1-9]|[1-9])`,
	"M": `(?P<M>[0-5]\d|\d)`,
	"S": `(?P<S>6[0-1]|[0-5]\d|\d)`,
	"U": `(?P<U>5[0-3]|[0-4]\d|\d)`,
	"W": `(?P<W>5[0-3]|[0-4]\d|\d)`,
	"w": `(?P<w>[0-6])`,
	"u": `(?P<u>[1-7])`,
	"y": `(?P<y>\d\d)`,
	"Y": `(?P<Y>\d\d\d\d)`,
	"z": `(?P<z>[+-]\d\d:?[0-5]\d(:?[0-5]\d(\.\d{1,6})?)?|(?-i:Z))`,
	"A": seqRE([]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, "A"),
	"a": seqRE(shortDays, "a"),
	"B": seqRE(longMonths, "B"),
	"b": seqRE(shortMonths, "b"),
	"p": seqRE([]string{"am", "pm"}, "p"),
	"Z": seqRE([]string{"utc", "gmt"}, "Z"),
	"%": "%",
}

func init() {
	strptimeDirectives["e"] = strptimeDirectives["d"]
	strptimeDirectives["P"] = strptimeDirectives["p"]
	strptimeDirectives["h"] = strptimeDirectives["b"]
}

var (
	strptimeCache   sync.Map
	strptimeEscRe   = regexp.MustCompile(`([\\.^$*+?(){}\[\]|])`)
	strptimeSpaceRe = regexp.MustCompile(`[` + pySpaceClass + `]+`)
	strptimeDirRe   = regexp.MustCompile(`%[-_0^#]*[0-9]*([OE]?\\?.?)`)
)

// strptimeRegex is TimeRE.compile(format), case-insensitive.
func strptimeRegex(format string) (*regexp.Regexp, error) {
	if re, ok := strptimeCache.Load(format); ok {
		return re.(*regexp.Regexp), nil
	}
	pat := strptimeEscRe.ReplaceAllString(format, `\$1`)
	pat = strptimeSpaceRe.ReplaceAllString(pat, `\s+`)
	var bad string
	pat = strptimeDirRe.ReplaceAllStringFunc(pat, func(m string) string {
		d := strptimeDirRe.FindStringSubmatch(m)[1]
		expansions := map[string]string{"T": "%H:%M:%S", "R": "%H:%M", "r": "%I:%M:%S %p",
			"X": "%H:%M:%S", "x": "%m/%d/%y", "c": "%a %b %d %H:%M:%S %Y"}
		if e, ok := expansions[d]; ok {
			return strptimeDirRe.ReplaceAllStringFunc(e, func(m2 string) string {
				return strptimeDirectives[strptimeDirRe.FindStringSubmatch(m2)[1]]
			})
		}
		if r, ok := strptimeDirectives[d]; ok {
			return r
		}
		if bad == "" {
			bad = d
			if bad == "" {
				bad = "\x00"
			}
		}
		return m
	})
	if bad != "" {
		bad = strings.ReplaceAll(bad, `\s`, "")
		if bad == "" || bad == "\x00" {
			return nil, fmt.Errorf("stray %% in format '%s'", format)
		}
		bad = strings.Replace(bad, `\`, "", 1)
		return nil, fmt.Errorf("'%s' is a bad directive in format '%s'", bad, format)
	}
	// Python's \s and \d are Unicode classes.
	pat = strings.ReplaceAll(pat, `\s+`, `[`+pySpaceClass+`]+`)
	re, err := regexp.Compile("(?i)^(?:" + pat + ")")
	if err != nil {
		return nil, fmt.Errorf("redefinition of group name: %v", err)
	}
	strptimeCache.Store(format, re)
	return re, nil
}

// strftimeTime is strftime of t's wall clock; loc names its zone (%Z,
// %z), empty for a naive datetime when naive is set.
func strftimeTime(format string, t time.Time, loc *time.Location, naive bool) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch c := format[i]; c {
		case 'Y':
			fmt.Fprintf(&b, "%d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'C':
			fmt.Fprintf(&b, "%02d", t.Year()/100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'k':
			fmt.Fprintf(&b, "%2d", t.Hour())
		case 'I', 'l':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			if c == 'I' {
				fmt.Fprintf(&b, "%02d", h)
			} else {
				fmt.Fprintf(&b, "%2d", h)
			}
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'f':
			fmt.Fprintf(&b, "%06d", t.Nanosecond()/1000)
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'w':
			b.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'u':
			b.WriteString(strconv.Itoa(int(t.Weekday()+6)%7 + 1))
		case 'F':
			fmt.Fprintf(&b, "%d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
		case 'T':
			fmt.Fprintf(&b, "%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second())
		case 'R':
			fmt.Fprintf(&b, "%02d:%02d", t.Hour(), t.Minute())
		case 'D':
			fmt.Fprintf(&b, "%02d/%02d/%02d", int(t.Month()), t.Day(), t.Year()%100)
		case 's':
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'Z':
			if !naive && loc != nil {
				b.WriteString(t.In(loc).Format("MST"))
			}
		case 'z':
			if !naive && loc != nil {
				_, off := t.In(loc).Zone()
				sign := '+'
				if off < 0 {
					sign, off = '-', -off
				}
				fmt.Fprintf(&b, "%c%02d%02d", sign, off/3600, off/60%60)
			}
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(c)
		}
	}
	return b.String()
}
