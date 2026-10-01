package yaml

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Python's datetime values, as PyYAML's constructor builds them from
// timestamps (datetime.date, datetime.datetime) and tomllib from TOML
// dates and times (datetime.time too). The template engine gives them
// their attributes and methods; here they are data: their wall clock and
// timezone, isoformat() and str(). They encode to JSON as their
// isoformat(), as ansible-core's encoders write them.

// TZ is a datetime.timezone: a fixed UTC offset, and the %Z name
// strptime parsed it with, if any.
type TZ struct {
	Offset time.Duration
	Name   string
}

// UTC is datetime.timezone.utc.
var UTC = &TZ{}

// Datetime is a datetime.datetime: its wall clock (in time.UTC, to the
// microsecond) and, when aware, its timezone.
type Datetime struct {
	T  time.Time
	TZ *TZ
}

// Date is a datetime.date: T is its midnight in time.UTC.
type Date struct{ T time.Time }

// Time is a datetime.time: T's clock (on 0000-01-01 in time.UTC) and,
// when aware, its timezone.
type Time struct {
	T  time.Time
	TZ *TZ
}

// TZSuffix is a UTC offset as isoformat() shows it: +HH:MM, with seconds
// and microseconds when present.
func TZSuffix(off time.Duration) string {
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

// String is str(timezone): its name, else UTC and the offset ("UTC" for
// a zero offset).
func (tz *TZ) String() string {
	if tz.Name != "" {
		return tz.Name
	}
	if tz.Offset == 0 {
		return "UTC"
	}
	return "UTC" + TZSuffix(tz.Offset)
}

func clock(t time.Time) string {
	s := fmt.Sprintf("%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second())
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

// Isoformat is datetime.isoformat(sep).
func (d Datetime) Isoformat(sep string) string {
	t := d.T
	s := fmt.Sprintf("%04d-%02d-%02d%s%s", t.Year(), int(t.Month()), t.Day(), sep, clock(t))
	if d.TZ != nil {
		s += TZSuffix(d.TZ.Offset)
	}
	return s
}

func (d Datetime) String() string { return d.Isoformat(" ") }

// Instant is the datetime as a point in time (aware), or its wall clock.
func (d Datetime) Instant() time.Time {
	if d.TZ == nil {
		return d.T
	}
	return d.T.Add(-d.TZ.Offset)
}

func (d Datetime) MarshalJSON() ([]byte, error) { return json.Marshal(d.Isoformat("T")) }

// Isoformat is date.isoformat().
func (d Date) Isoformat() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.T.Year(), int(d.T.Month()), d.T.Day())
}

func (d Date) String() string { return d.Isoformat() }

func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.Isoformat()) }

// Isoformat is time.isoformat().
func (t Time) Isoformat() string {
	s := clock(t.T)
	if t.TZ != nil {
		s += TZSuffix(t.TZ.Offset)
	}
	return s
}

func (t Time) String() string { return t.Isoformat() }

func (t Time) MarshalJSON() ([]byte, error) { return json.Marshal(t.Isoformat()) }

// CheckDate is datetime.date's range check, with its ValueError text.
func CheckDate(y, m, d int) error {
	if y < 1 || y > 9999 {
		return fmt.Errorf("year must be in 1..9999, not %d", y)
	}
	if m < 1 || m > 12 {
		return fmt.Errorf("month must be in 1..12, not %d", m)
	}
	dim := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m-1]
	if m == 2 && (y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		dim = 29
	}
	if d < 1 || d > dim {
		return fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", d, dim, m, y)
	}
	return nil
}

// checkTime is datetime.time's range check, with its ValueError text.
func checkTime(h, m, s int) error {
	switch {
	case h < 0 || h > 23:
		return fmt.Errorf("hour must be in 0..23, not %d", h)
	case m < 0 || m > 59:
		return fmt.Errorf("minute must be in 0..59, not %d", m)
	case s < 0 || s > 59:
		return fmt.Errorf("second must be in 0..59, not %d", s)
	}
	return nil
}

// NewTZ is datetime.timezone(offset), with its ValueError for an offset
// of a day or more.
func NewTZ(off time.Duration) (*TZ, error) {
	if off <= -24*time.Hour || off >= 24*time.Hour {
		return nil, fmt.Errorf("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s", timedeltaRepr(off))
	}
	return &TZ{Offset: off}, nil
}

// timedeltaRepr is repr(timedelta) of a duration (to the microsecond).
func timedeltaRepr(d time.Duration) string {
	us := int64(d / time.Microsecond)
	const usPerDay = 86400 * 1000000
	days := us / usPerDay
	if us%usPerDay != 0 && us < 0 {
		days--
	}
	rem := us - days*usPerDay
	secs, frac := rem/1000000, rem%1000000
	var args []string
	if days != 0 {
		args = append(args, fmt.Sprintf("days=%d", days))
	}
	if secs != 0 {
		args = append(args, fmt.Sprintf("seconds=%d", secs))
	}
	if frac != 0 {
		args = append(args, fmt.Sprintf("microseconds=%d", frac))
	}
	if len(args) == 0 {
		args = []string{"0"}
	}
	return "datetime.timedelta(" + strings.Join(args, ", ") + ")"
}

// NewDatetime is datetime.datetime(y, mo, d, h, mi, s, us, tzinfo=tz),
// with its ValueErrors.
func NewDatetime(y, mo, d, h, mi, s, us int, tz *TZ) (Datetime, error) {
	if err := CheckDate(y, mo, d); err != nil {
		return Datetime{}, err
	}
	if err := checkTime(h, mi, s); err != nil {
		return Datetime{}, err
	}
	return Datetime{T: time.Date(y, time.Month(mo), d, h, mi, s, us*1000, time.UTC), TZ: tz}, nil
}

// NewDate is datetime.date(y, m, d), with its ValueErrors.
func NewDate(y, m, d int) (Date, error) {
	if err := CheckDate(y, m, d); err != nil {
		return Date{}, err
	}
	return Date{T: time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)}, nil
}

// NewTime is datetime.time(h, m, s, us, tzinfo=tz), with its ValueErrors.
func NewTime(h, m, s, us int, tz *TZ) (Time, error) {
	if err := checkTime(h, m, s); err != nil {
		return Time{}, err
	}
	return Time{T: time.Date(0, 1, 1, h, m, s, us*1000, time.UTC), TZ: tz}, nil
}

// PyYAML's SafeConstructor.timestamp_regexp.
var timestampRe = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)` +
	`(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?` +
	`(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?\n?$`)

// constructTimestamp is PyYAML's construct_yaml_timestamp: a date, or a
// datetime (naive, UTC for Z, else a fixed offset); a value datetime
// rejects fails with its ValueError.
func constructTimestamp(s string) (any, error) {
	g := timestampRe.FindStringSubmatch(s)
	if g == nil {
		return nil, &Error{Msg: "'NoneType' object has no attribute 'groupdict'"}
	}
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	year, month, day := atoi(g[1]), atoi(g[2]), atoi(g[3])
	if g[4] == "" {
		d, err := NewDate(year, month, day)
		if err != nil {
			return nil, &Error{Msg: err.Error()}
		}
		return d, nil
	}
	fraction := 0
	if f := g[7]; f != "" {
		if len(f) > 6 {
			f = f[:6]
		}
		fraction = atoi(f + strings.Repeat("0", 6-len(f)))
	}
	var tz *TZ
	switch {
	case g[9] != "":
		off := time.Duration(atoi(g[10]))*time.Hour + time.Duration(atoi(g[11]))*time.Minute
		if g[9] == "-" {
			off = -off
		}
		var err error
		if tz, err = NewTZ(off); err != nil {
			return nil, &Error{Msg: err.Error()}
		}
	case g[8] != "":
		tz = UTC
	}
	dt, err := NewDatetime(year, month, day, atoi(g[4]), atoi(g[5]), atoi(g[6]), fraction, tz)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	return dt, nil
}

// WireValue is v as a module receives it over JSON: ordered maps
// flattened (see AsMap) and datetimes as their isoformat().
func WireValue(v any) any {
	switch t := v.(type) {
	case *OMap:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys() {
			out[k] = WireValue(t.Get(k))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = WireValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = WireValue(item)
		}
		return out
	case Datetime:
		return t.Isoformat("T")
	case Date:
		return t.Isoformat()
	case Time:
		return t.Isoformat()
	}
	return v
}

// clockArgs are a clock's constructor arguments as repr() shows them:
// hour and minute, then second and microsecond when not zero.
func clockArgs(t time.Time) []string {
	args := []string{strconv.Itoa(t.Hour()), strconv.Itoa(t.Minute())}
	us := t.Nanosecond() / 1000
	if t.Second() != 0 || us != 0 {
		args = append(args, strconv.Itoa(t.Second()))
	}
	if us != 0 {
		args = append(args, strconv.Itoa(us))
	}
	return args
}

// Repr is repr(timezone).
func (tz *TZ) Repr() string {
	if tz.Offset == 0 && tz.Name == "" {
		return "datetime.timezone.utc"
	}
	td := timedeltaRepr(tz.Offset)
	if tz.Name != "" {
		return "datetime.timezone(" + td + ", " + pyRepr(tz.Name) + ")"
	}
	return "datetime.timezone(" + td + ")"
}

// Repr is repr(datetime).
func (d Datetime) Repr() string {
	t := d.T
	args := append([]string{strconv.Itoa(t.Year()), strconv.Itoa(int(t.Month())), strconv.Itoa(t.Day())}, clockArgs(t)...)
	if d.TZ != nil {
		args = append(args, "tzinfo="+d.TZ.Repr())
	}
	return "datetime.datetime(" + strings.Join(args, ", ") + ")"
}

// Repr is repr(date).
func (d Date) Repr() string {
	return fmt.Sprintf("datetime.date(%d, %d, %d)", d.T.Year(), int(d.T.Month()), d.T.Day())
}

// Repr is repr(time).
func (t Time) Repr() string {
	args := clockArgs(t.T)
	if t.TZ != nil {
		args = append(args, "tzinfo="+t.TZ.Repr())
	}
	return "datetime.time(" + strings.Join(args, ", ") + ")"
}
