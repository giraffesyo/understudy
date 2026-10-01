package template

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// ansible-core's union, intersect, difference and symmetric_difference
// are Python set operations (list(set(a) | set(b))), falling back, when
// building a set raises TypeError (an operand that is not iterable, an
// unhashable item), to list code over the operands: unique(a + b), [x
// for x in a if x in b]. The fallbacks raise Python's own errors for
// operands that are not lists. A set's items come out in its hash table
// order, which pySet reproduces for items whose hash is not randomized
// per process (numbers and booleans); strings hash randomly in Python,
// and keep their first-seen order here.

const (
	setUnion = iota
	setIntersect
	setDifference
	setSymmetricDifference
)

func setOp(op int) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		a, b := Undeprecate(in), Undeprecate(args1(args, 0))
		if len(args) == 0 {
			b = Undeprecate(kwargs["b"])
		}
		names := [2]string{pyOperandClass(a, ec.fromVar(-1)), pyOperandClass(b, ec.fromVar(0))}
		out, err := pySetOp(op, a, b, names)
		var plain *noContextError
		if err != nil && !errors.As(err, &plain) {
			// Only the fallbacks raise, handling set()'s TypeError.
			return nil, whileHandling("%s", err)
		}
		if err != nil {
			return nil, errors.New(plain.msg)
		}
		return out, nil
	}
}

// pyOperandClass is the class of a set operation's operand: containers
// are lazy (ansible-core lazifies a plugin's container arguments).
func pyOperandClass(v any, fromVar bool) string {
	switch v.(type) {
	case []any:
		return "_AnsibleLazyTemplateList"
	}
	if isMap(v) {
		return "_AnsibleLazyTemplateDict"
	}
	return pyClassName(v, fromVar)
}

func pySetOp(op int, a, b any, names [2]string) (any, error) {
	ia, okA := pyIterItems(a)
	ib, okB := pyIterItems(b)
	if okA && okB && allHashable(ia) && allHashable(ib) {
		return pySetResult(op, ia, ib), nil
	}
	switch op {
	case setUnion:
		sum, err := pyAdd(a, b, names)
		if err != nil {
			return nil, err
		}
		items, ok := pyIterItems(sum)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not iterable", pyClassName(sum, false))
		}
		return uniqueItems(items), nil
	case setIntersect, setDifference:
		if !okA {
			return nil, fmt.Errorf("'%s' object is not iterable", names[0])
		}
		var kept []any
		for _, x := range ia {
			in, err := pyContains(b, x, names[1])
			if err != nil {
				return nil, err
			}
			if in == (op == setIntersect) {
				kept = append(kept, x)
			}
		}
		return uniqueItems(kept), nil
	}
	isect, err := pySetOp(setIntersect, a, b, names)
	if err != nil {
		return nil, err
	}
	union, err := pySetOp(setUnion, a, b, names)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, x := range union.([]any) {
		found := false
		for _, y := range isect.([]any) {
			if equal(x, y) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, x)
		}
	}
	return out, nil
}

// pyIterItems is what iterating v yields in Python: a list's items, a
// string's characters, a dict's keys. ok is false when v is not iterable.
func pyIterItems(v any) ([]any, bool) {
	switch v.(type) {
	case nil, bool, int64, int, *big.Int, float64, Omit:
		return nil, false
	}
	items, err := iterate(v)
	return items, err == nil
}

func pyHashable(v any) bool {
	switch Undeprecate(v).(type) {
	case []any:
		return false
	}
	return !isMap(v)
}

func allHashable(items []any) bool {
	for _, it := range items {
		if !pyHashable(it) {
			return false
		}
	}
	return true
}

// uniqueItems is ansible-core's unique fallback: first occurrences, in
// order.
func uniqueItems(items []any) []any {
	out := []any{}
	for _, it := range items {
		found := false
		for _, seen := range out {
			if equal(seen, it) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, it)
		}
	}
	return out
}

// pyAdd is Python's a + b for the operands of a union's fallback.
func pyAdd(a, b any, names [2]string) (any, error) {
	switch ta := a.(type) {
	case []any:
		if tb, ok := b.([]any); ok {
			return append(append([]any{}, ta...), tb...), nil
		}
		return nil, fmt.Errorf("can only concatenate list (not %q) to list", names[1])
	case string, yaml.UnsafeString:
		if sb, ok := asString(b); ok {
			sa, _ := asString(a)
			return sa + sb, nil
		}
		return nil, fmt.Errorf("can only concatenate str (not %q) to str", names[1])
	}
	if a == nil || isMap(a) {
		msg := fmt.Sprintf("unsupported operand type(s) for +: '%s' and '%s'", names[0], names[1])
		if _, isList := b.([]any); isList {
			// The lazy list's __radd__ raises it "from None".
			return nil, &noContextError{msg}
		}
		return nil, errors.New(msg)
	}
	// A number.
	switch b.(type) {
	case []any:
		// int.__add__ returns NotImplemented, which the lazy list's
		// __radd__ wraps as the "list".
		return nil, fmt.Errorf("'NotImplementedType' object is not iterable")
	case bool, int64, int, *big.Int:
		if _, isFloat := a.(float64); isFloat {
			return 0.0, nil
		}
		return int64(0), nil
	case float64:
		return 0.0, nil
	}
	return nil, fmt.Errorf("unsupported operand type(s) for +: '%s' and '%s'", names[0], names[1])
}

// noContextError is an exception raised "from None": shown without the
// one it was raised handling.
type noContextError struct{ msg string }

func (e *noContextError) Error() string { return e.msg }

// pyContains is Python's x in container.
func pyContains(container, x any, containerName string) (bool, error) {
	switch t := container.(type) {
	case []any:
		for _, it := range t {
			if equal(it, x) {
				return true, nil
			}
		}
		return false, nil
	case string, yaml.UnsafeString:
		xs, ok := asString(x)
		if !ok {
			return false, fmt.Errorf("'in <string>' requires string as left operand, not %s", pyOperandClass(Undeprecate(x), false))
		}
		s, _ := asString(t)
		return strings.Contains(s, xs), nil
	}
	if keys, ok := pyIterItems(container); ok && isMap(container) {
		if !pyHashable(x) {
			cls := pyOperandClass(Undeprecate(x), false)
			return false, fmt.Errorf("cannot use 'ansible._internal._templating._lazy_containers.%s' as a dict key (unhashable type: '%s')", cls, cls)
		}
		for _, k := range keys {
			if equal(k, x) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("argument of type '%s' is not a container or iterable", containerName)
}

// pySetResult is list(set(a) <op> set(b)) for hashable items.
func pySetResult(op int, a, b []any) []any {
	sa, okA := newPySet(a)
	sb, okB := newPySet(b)
	if !okA || !okB {
		// A randomized hash: first-seen order.
		var out []any
		appendUnique := func(v any) {
			for _, seen := range out {
				if equal(seen, v) {
					return
				}
			}
			out = append(out, v)
		}
		contains := func(list []any, v any) bool {
			for _, it := range list {
				if equal(it, v) {
					return true
				}
			}
			return false
		}
		for _, v := range a {
			inB := contains(b, v)
			if op == setUnion || (op == setIntersect && inB) || ((op == setDifference || op == setSymmetricDifference) && !inB) {
				appendUnique(v)
			}
		}
		if op == setUnion || op == setSymmetricDifference {
			for _, v := range b {
				if op == setUnion || !contains(a, v) {
					appendUnique(v)
				}
			}
		}
		if out == nil {
			out = []any{}
		}
		return out
	}
	var r *pySet
	switch op {
	case setUnion:
		r = sa.copySet()
		r.merge(sb)
	case setIntersect:
		r = sa.intersection(sb)
	case setDifference:
		r = sa.difference(sb)
	default:
		r = sb.copySet()
		for _, e := range sa.entries() {
			if !r.discard(e.key, e.hash) {
				r.add(e.key, e.hash)
			}
		}
	}
	out := []any{}
	for _, e := range r.entries() {
		out = append(out, e.key)
	}
	return out
}

// pySet is CPython's set hash table (Objects/setobject.c), for the order
// a set iterates in.
type pySet struct {
	table      []pySetEntry
	fill, used int
}

type pySetEntry struct {
	key   any
	hash  int64
	state int // 0 unused, 1 active, 2 dummy
}

const (
	pySetMinSize     = 8
	pySetLinearProbe = 9
	pySetPerturb     = 5
)

func newPySet(items []any) (*pySet, bool) {
	s := &pySet{table: make([]pySetEntry, pySetMinSize)}
	for _, it := range items {
		h, ok := pyHash(it)
		if !ok {
			return nil, false
		}
		s.add(it, h)
	}
	return s, true
}

func (s *pySet) mask() uint64 { return uint64(len(s.table) - 1) }

func (s *pySet) add(key any, hash int64) {
	mask := s.mask()
	i := uint64(hash) & mask
	perturb := uint64(hash)
	freeslot := -1
	for {
		probes := 0
		if i+pySetLinearProbe <= mask {
			probes = pySetLinearProbe
		}
		j := i
		for {
			e := &s.table[j]
			switch {
			case e.state == 0:
				if freeslot >= 0 {
					s.used++
					s.table[freeslot] = pySetEntry{key: key, hash: hash, state: 1}
					return
				}
				s.fill++
				s.used++
				*e = pySetEntry{key: key, hash: hash, state: 1}
				if uint64(s.fill)*5 >= mask*3 {
					s.resize(s.used * 4)
				}
				return
			case e.state == 1 && e.hash == hash && equal(e.key, key):
				return
			case e.state == 2 && freeslot < 0:
				freeslot = int(j)
			}
			if probes == 0 {
				break
			}
			probes--
			j++
		}
		perturb >>= pySetPerturb
		i = (i*5 + 1 + perturb) & mask
	}
}

func (s *pySet) resize(minused int) {
	size := pySetMinSize
	for size <= minused {
		size <<= 1
	}
	old := s.table
	s.table = make([]pySetEntry, size)
	s.fill = s.used
	for _, e := range old {
		if e.state == 1 {
			s.insertClean(e.key, e.hash)
		}
	}
}

func (s *pySet) insertClean(key any, hash int64) {
	mask := s.mask()
	i := uint64(hash) & mask
	perturb := uint64(hash)
	for {
		if s.table[i].state == 0 {
			s.table[i] = pySetEntry{key: key, hash: hash, state: 1}
			return
		}
		if i+pySetLinearProbe <= mask {
			for j := uint64(1); j <= pySetLinearProbe; j++ {
				if s.table[i+j].state == 0 {
					s.table[i+j] = pySetEntry{key: key, hash: hash, state: 1}
					return
				}
			}
		}
		perturb >>= pySetPerturb
		i = (i*5 + 1 + perturb) & mask
	}
}

func (s *pySet) find(key any, hash int64) int {
	mask := s.mask()
	i := uint64(hash) & mask
	perturb := uint64(hash)
	for {
		probes := 0
		if i+pySetLinearProbe <= mask {
			probes = pySetLinearProbe
		}
		j := i
		for {
			e := &s.table[j]
			if e.state == 0 {
				return -1
			}
			if e.state == 1 && e.hash == hash && equal(e.key, key) {
				return int(j)
			}
			if probes == 0 {
				break
			}
			probes--
			j++
		}
		perturb >>= pySetPerturb
		i = (i*5 + 1 + perturb) & mask
	}
}

func (s *pySet) discard(key any, hash int64) bool {
	j := s.find(key, hash)
	if j < 0 {
		return false
	}
	s.table[j] = pySetEntry{hash: -1, state: 2}
	s.used--
	return true
}

func (s *pySet) entries() []pySetEntry {
	var out []pySetEntry
	for _, e := range s.table {
		if e.state == 1 {
			out = append(out, e)
		}
	}
	return out
}

// merge is set_merge: other's entries into s.
func (s *pySet) merge(other *pySet) {
	if other.used == 0 {
		return
	}
	if uint64(s.fill+other.used)*5 >= s.mask()*3 {
		s.resize((s.used + other.used) * 2)
	}
	if s.fill == 0 && len(s.table) == len(other.table) && other.fill == other.used {
		copy(s.table, other.table)
		s.fill, s.used = other.fill, other.used
		return
	}
	if s.fill == 0 {
		s.fill, s.used = other.used, other.used
		for _, e := range other.table {
			if e.state == 1 {
				s.insertClean(e.key, e.hash)
			}
		}
		return
	}
	for _, e := range other.table {
		if e.state == 1 {
			s.add(e.key, e.hash)
		}
	}
}

func (s *pySet) copySet() *pySet {
	c := &pySet{table: make([]pySetEntry, pySetMinSize)}
	c.merge(s)
	return c
}

// intersection is set_intersection: the smaller set's entries the larger
// has.
func (s *pySet) intersection(other *pySet) *pySet {
	r := &pySet{table: make([]pySetEntry, pySetMinSize)}
	big, small := s, other
	if other.used > s.used {
		big, small = other, s
	}
	for _, e := range small.entries() {
		if big.find(e.key, e.hash) >= 0 {
			r.add(e.key, e.hash)
		}
	}
	return r
}

// difference is set_difference.
func (s *pySet) difference(other *pySet) *pySet {
	if s.used>>2 > other.used {
		r := s.copySet()
		for _, e := range other.entries() {
			r.discard(e.key, e.hash)
		}
		return r
	}
	r := &pySet{table: make([]pySetEntry, pySetMinSize)}
	for _, e := range s.entries() {
		if other.find(e.key, e.hash) < 0 {
			r.add(e.key, e.hash)
		}
	}
	return r
}

const (
	pyHashBits    = 61
	pyHashModulus = (1 << pyHashBits) - 1
	pyHashInf     = 314159
)

// pyHash is Python's hash() of v, ok false where it is randomized per
// process (str) or not reproduced.
func pyHash(v any) (int64, bool) {
	switch t := Undeprecate(v).(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int64:
		return pyHashBig(big.NewInt(t)), true
	case int:
		return pyHashBig(big.NewInt(int64(t))), true
	case *big.Int:
		return pyHashBig(t), true
	case float64:
		return pyHashFloat(t)
	}
	return 0, false
}

func pyHashBig(n *big.Int) int64 {
	m := new(big.Int).Abs(n)
	m.Mod(m, big.NewInt(pyHashModulus))
	x := m.Int64()
	if n.Sign() < 0 {
		x = -x
	}
	if x == -1 {
		x = -2
	}
	return x
}

func pyHashFloat(v float64) (int64, bool) {
	if math.IsInf(v, 0) {
		if v > 0 {
			return pyHashInf, true
		}
		return -pyHashInf, true
	}
	if math.IsNaN(v) {
		return 0, false
	}
	m, e := math.Frexp(v)
	sign := int64(1)
	if m < 0 {
		sign, m = -1, -m
	}
	var x uint64
	for m != 0 {
		x = ((x << 28) & pyHashModulus) | x>>(pyHashBits-28)
		m *= 268435456.0
		e -= 28
		y := uint64(m)
		m -= float64(y)
		x += y
		if x >= pyHashModulus {
			x -= pyHashModulus
		}
	}
	if e >= 0 {
		e %= pyHashBits
	} else {
		e = pyHashBits - 1 - ((-1 - e) % pyHashBits)
	}
	x = ((x << uint(e)) & pyHashModulus) | x>>(pyHashBits-uint(e))
	r := int64(x) * sign
	if r == -1 {
		r = -2
	}
	return r, true
}
