package template

import (
	"errors"
	"fmt"
	"time"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Python's datetime.date, datetime.time and datetime.timezone, as YAML
// timestamps and TOML dates and times load them: attributes, methods,
// arithmetic and comparison; datetime.datetime's methods beyond those
// pydatetime.go gives it.

// pyAttrOf is getattr(v, name) for the values with Python attributes.
func pyAttrOf(v any, name string) (any, bool) {
	switch t := v.(type) {
	case pyDatetime:
		return datetimeAttr(t, name)
	case pyDate:
		return dateAttr(t, name)
	case pyTime:
		return timeAttr(t, name)
	case *pyTZ:
		return tzAttr(t, name)
	case pyObject:
		return t.PyAttr(name)
	}
	return nil, false
}

func pyMethod(f func(args []any, kwargs map[string]any) (any, error)) (any, bool) {
	return boundMethod(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
		return f(args, kwargs)
	}), true
}

// tzLocation is a timezone as strftime's %Z and %z see it (nil: naive).
func tzLocation(tz *pyTZ) *time.Location {
	if tz == nil {
		return nil
	}
	return time.FixedZone(tz.String(), int(tz.Offset/time.Second))
}

func utcoffset(tz *pyTZ) any {
	if tz == nil {
		return nil
	}
	return pyTimedelta{int64(tz.Offset / time.Microsecond)}
}

func tzname(tz *pyTZ) any {
	if tz == nil {
		return nil
	}
	return tz.String()
}

// ordinal is date.toordinal(): day 1 is 0001-01-01.
func ordinal(t time.Time) int64 {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return (d.Unix()-time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Unix())/86400 + 1
}

func ctime(t time.Time) string {
	return strftimeTime("%a %b %e %H:%M:%S %Y", t, nil, true)
}

func strftimeMethod(t time.Time, tz *pyTZ) (any, bool) {
	return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
		if len(args) != 1 || len(kwargs) > 0 {
			return nil, fmt.Errorf("strftime() takes exactly 1 argument (%d given)", len(args)+len(kwargs))
		}
		f, ok := asString(Undeprecate(args[0]))
		if !ok {
			return nil, fmt.Errorf("strftime() argument 1 must be str, not %s", pyClassName(args[0], false))
		}
		return strftimeTime(f, t, tzLocation(tz), tz == nil), nil
	})
}

// replaceArgs binds replace()'s arguments (names in positional order)
// to ints, with tzinfo (a timezone or None) when it is among the names.
func replaceArgs(method string, names []string, cur []int, curTZ *pyTZ, args []any, kwargs map[string]any) ([]int, *pyTZ, error) {
	out := append([]int(nil), cur...)
	tz := curTZ
	if len(args) > len(names) {
		return nil, nil, fmt.Errorf("%s() takes at most %d arguments (%d given)", method, len(names), len(args))
	}
	set := func(i int, v any) error {
		v = Undeprecate(v)
		if names[i] == "tzinfo" {
			switch z := v.(type) {
			case nil:
				tz = nil
			case *pyTZ:
				tz = z
			default:
				return fmt.Errorf("tzinfo argument must be None or of a tzinfo subclass, not type '%s'", pyClassName(v, false))
			}
			return nil
		}
		n, ok := v.(int64)
		if !ok {
			if b, isBool := v.(bool); isBool {
				n = map[bool]int64{true: 1}[b]
			} else {
				return fmt.Errorf("'%s' object cannot be interpreted as an integer", pyClassName(v, false))
			}
		}
		out[i] = int(n)
		return nil
	}
	for i, a := range args {
		if err := set(i, a); err != nil {
			return nil, nil, err
		}
	}
	for _, k := range sortedKeys(kwargs) {
		i := -1
		for j, n := range names {
			if n == k {
				i = j
			}
		}
		if i < 0 {
			return nil, nil, fmt.Errorf("%s() got an unexpected keyword argument '%s'", method, k)
		}
		if i < len(args) {
			return nil, nil, fmt.Errorf("argument for %s() given by name ('%s') and position (%d)", method, k, i+1)
		}
		if err := set(i, kwargs[k]); err != nil {
			return nil, nil, err
		}
	}
	return out, tz, nil
}

// datetimeMoreAttr is the rest of datetime.datetime's methods.
func datetimeMoreAttr(d pyDatetime, name string) (any, bool) {
	t := d.T
	switch name {
	case "min":
		return pyDatetime{T: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)}, true
	case "max":
		return pyDatetime{T: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)}, true
	case "resolution":
		return pyTimedelta{1}, true
	case "date":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs("date", args, kwargs); err != nil {
				return nil, err
			}
			return pyDate{T: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)}, nil
		})
	case "time", "timetz":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			tm := pyTime{T: time.Date(0, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)}
			if name == "timetz" {
				tm.TZ = d.TZ
			}
			return tm, nil
		})
	case "utcoffset", "tzname", "dst":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			switch name {
			case "utcoffset":
				return utcoffset(d.TZ), nil
			case "tzname":
				return tzname(d.TZ), nil
			}
			if d.TZ == nil {
				return nil, nil
			}
			return pyTimedelta{}, nil
		})
	case "toordinal":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			return ordinal(t), nil
		})
	case "ctime":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if err := noArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			return ctime(t), nil
		})
	case "replace":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			names := []string{"year", "month", "day", "hour", "minute", "second", "microsecond", "tzinfo", "fold"}
			cur := []int{t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond() / 1000, 0, 0}
			v, tz, err := replaceArgs("replace", names, cur, d.TZ, args, kwargs)
			if err != nil {
				return nil, err
			}
			return yaml.NewDatetime(v[0], v[1], v[2], v[3], v[4], v[5], v[6], tz)
		})
	}
	return nil, false
}

func dateAttr(d pyDate, name string) (any, bool) {
	t := d.T
	switch name {
	case "year":
		return int64(t.Year()), true
	case "month":
		return int64(t.Month()), true
	case "day":
		return int64(t.Day()), true
	case "min":
		return pyDate{T: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)}, true
	case "max":
		return pyDate{T: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}, true
	case "resolution":
		return pyTimedelta{usPerDay}, true
	case "strftime":
		return strftimeMethod(t, nil)
	case "replace":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			v, _, err := replaceArgs("replace", []string{"year", "month", "day"}, []int{t.Year(), int(t.Month()), t.Day()}, nil, args, kwargs)
			if err != nil {
				return nil, err
			}
			return yaml.NewDate(v[0], v[1], v[2])
		})
	}
	var f func() any
	switch name {
	case "isoformat":
		f = func() any { return d.Isoformat() }
	case "weekday":
		f = func() any { return (int64(t.Weekday()) + 6) % 7 }
	case "isoweekday":
		f = func() any { return (int64(t.Weekday())+6)%7 + 1 }
	case "toordinal":
		f = func() any { return ordinal(t) }
	case "ctime":
		f = func() any { return ctime(t) }
	default:
		return nil, false
	}
	return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
		if err := noArgs(name, args, kwargs); err != nil {
			return nil, err
		}
		return f(), nil
	})
}

func timeAttr(tm pyTime, name string) (any, bool) {
	t := tm.T
	switch name {
	case "hour":
		return int64(t.Hour()), true
	case "minute":
		return int64(t.Minute()), true
	case "second":
		return int64(t.Second()), true
	case "microsecond":
		return int64(t.Nanosecond() / 1000), true
	case "fold":
		return int64(0), true
	case "min":
		return pyTime{T: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)}, true
	case "max":
		return pyTime{T: time.Date(0, 1, 1, 23, 59, 59, 999999000, time.UTC)}, true
	case "resolution":
		return pyTimedelta{1}, true
	case "tzinfo":
		if tm.TZ == nil {
			return nil, true
		}
		return tm.TZ, true
	case "strftime":
		return strftimeMethod(time.Date(1900, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC), tm.TZ)
	case "replace":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			names := []string{"hour", "minute", "second", "microsecond", "tzinfo", "fold"}
			cur := []int{t.Hour(), t.Minute(), t.Second(), t.Nanosecond() / 1000, 0, 0}
			v, tz, err := replaceArgs("replace", names, cur, tm.TZ, args, kwargs)
			if err != nil {
				return nil, err
			}
			return yaml.NewTime(v[0], v[1], v[2], v[3], tz)
		})
	}
	var f func() any
	switch name {
	case "isoformat":
		f = func() any { return tm.Isoformat() }
	case "utcoffset":
		f = func() any { return utcoffset(tm.TZ) }
	case "tzname":
		f = func() any { return tzname(tm.TZ) }
	case "dst":
		f = func() any {
			if tm.TZ == nil {
				return nil
			}
			return pyTimedelta{}
		}
	default:
		return nil, false
	}
	return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
		if err := noArgs(name, args, kwargs); err != nil {
			return nil, err
		}
		return f(), nil
	})
}

func tzAttr(tz *pyTZ, name string) (any, bool) {
	switch name {
	case "utcoffset", "tzname", "dst":
		return pyMethod(func(args []any, kwargs map[string]any) (any, error) {
			if len(args) != 1 || len(kwargs) > 0 {
				return nil, fmt.Errorf("timezone.%s() takes exactly one argument (%d given)", name, len(args)+len(kwargs))
			}
			switch name {
			case "utcoffset":
				return utcoffset(tz), nil
			case "tzname":
				return tz.String(), nil
			}
			return nil, nil
		})
	}
	return nil, false
}

func dateRepr(d pyDate) string { return d.Repr() }

func timeRepr(tm pyTime) string { return tm.Repr() }

// dateArith is a - b, a + b for dates (handled reports whether either
// operand is a date or time).
func dateArith(op tokKind, a, b any) (out any, handled bool, err error) {
	da, aD := a.(pyDate)
	db, bD := b.(pyDate)
	_, aT := a.(pyTime)
	_, bT := b.(pyTime)
	if !aD && !bD && !aT && !bT {
		return nil, false, nil
	}
	ta, aTD := a.(pyTimedelta)
	tb, bTD := b.(pyTimedelta)
	addDays := func(d pyDate, us int64) (any, bool, error) {
		days := floorDiv(us, usPerDay)
		n := ordinal(d.T) + days
		if n < 1 || n > 3652059 {
			return nil, true, errors.New("date value out of range")
		}
		return pyDate{T: d.T.AddDate(0, 0, int(days))}, true, nil
	}
	switch {
	case op == tokSub && aD && bD:
		return pyTimedelta{(ordinal(da.T) - ordinal(db.T)) * usPerDay}, true, nil
	case op == tokSub && aD && bTD:
		return addDays(da, -tb.us)
	case op == tokAdd && aD && bTD:
		return addDays(da, tb.us)
	case op == tokAdd && aTD && bD:
		return addDays(db, ta.us)
	}
	return nil, true, newOperandError("unsupported operand type(s) for "+opName(op)+": '%s' and '%s'", a, b)
}

// dateCompare orders dates, and times (handled reports whether both
// operands are).
func dateCompare(a, b any) (c int, handled bool, err error) {
	switch x := a.(type) {
	case pyDate:
		y, ok := b.(pyDate)
		if !ok {
			return 0, false, nil
		}
		return x.T.Compare(y.T), true, nil
	case pyTime:
		y, ok := b.(pyTime)
		if !ok {
			return 0, false, nil
		}
		if (x.TZ == nil) != (y.TZ == nil) {
			return 0, true, errors.New("can't compare offset-naive and offset-aware times")
		}
		return timeInstant(x).Compare(timeInstant(y)), true, nil
	}
	return 0, false, nil
}

func timeInstant(t pyTime) time.Time {
	if t.TZ == nil {
		return t.T
	}
	return t.T.Add(-t.TZ.Offset)
}

// dateEqual is a == b for dates, times and timezones.
func dateEqual(a, b any) (eq, handled bool) {
	switch x := a.(type) {
	case pyDate:
		y, ok := b.(pyDate)
		return ok && x.T.Equal(y.T), true
	case pyTime:
		y, ok := b.(pyTime)
		if !ok || (x.TZ == nil) != (y.TZ == nil) {
			return false, true
		}
		return timeInstant(x).Equal(timeInstant(y)), true
	case *pyTZ:
		y, ok := b.(*pyTZ)
		return ok && x.Offset == y.Offset, true
	}
	return false, false
}
