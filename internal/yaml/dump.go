package yaml

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DumpOptions are the yaml.dump() keyword arguments ansible-core's
// to_yaml/to_nice_yaml filters pass through to PyYAML.
type DumpOptions struct {
	Indent int // best indent (2..9; anything else means 2)
	Width  int // best width (<= 2*indent means 80; negative means unlimited)
	// DefaultFlowStyle is PyYAML's default_flow_style: nil (None) lays
	// out collections of scalars in flow style and the rest in block
	// style; true forces flow, false forces block.
	DefaultFlowStyle *bool
	SortKeys         bool
	AllowUnicode     bool
	Canonical        bool
	ExplicitStart    bool
	ExplicitEnd      bool
	// DefaultStyle is the scalar style every scalar asks for: 0 (plain
	// where possible), '\'', '"', '|' or '>'.
	DefaultStyle byte
	LineBreak    string // "\n" (default), "\r" or "\r\n"
	// Normalize, when set, converts caller-specific values (wrappers,
	// lazy mappings) into the plain types Dump represents. It must return
	// shared containers unchanged so repeated references still alias.
	Normalize func(any) any
}

// Dump serializes v as PyYAML's yaml.dump(v, Dumper=CSafeDumper, ...)
// does: SafeRepresenter builds the node graph (repeated containers become
// &idNNN anchors and *idNNN aliases, as PyYAML tracks objects by
// identity), the serializer resolves implicit tags, and libyaml's emitter
// lays out the text.
func Dump(v any, opts DumpOptions) (string, error) {
	r := representer{opts: opts, objects: map[identity]*rnode{}}
	root, err := r.represent(v)
	if err != nil {
		return "", err
	}
	s := serializer{anchors: map[*rnode]string{}, serialized: map[*rnode]bool{}}
	s.anchorNode(root)
	s.events = append(s.events,
		emitEvent{kind: eeStreamStart},
		emitEvent{kind: eeDocStart, implicit: !opts.ExplicitStart})
	s.serializeNode(root)
	s.events = append(s.events,
		emitEvent{kind: eeDocEnd, implicit: !opts.ExplicitEnd},
		emitEvent{kind: eeStreamEnd})

	em := emitter{
		canonical:  opts.Canonical,
		bestIndent: opts.Indent,
		bestWidth:  opts.Width,
		unicode:    opts.AllowUnicode,
		lineBreak:  opts.LineBreak,
	}
	return em.emit(s.events)
}

// ---- representer (PyYAML's SafeRepresenter) ----------------------------

type rkind int

const (
	rScalar rkind = iota
	rSequence
	rMapping
)

type rnode struct {
	kind  rkind
	tag   string
	value string
	style byte // 0: None (plain)
	flow  bool
	items []*rnode // sequence items; mapping keys and values alternating
}

// identity is a container's object identity, as Python's id(): a slice's
// backing array (and length), a map's header, an *OMap pointer.
type identity struct {
	kind byte
	ptr  uintptr
	n    int
}

type representer struct {
	opts    DumpOptions
	objects map[identity]*rnode
}

func (r *representer) scalar(tag, value string) *rnode {
	return &rnode{kind: rScalar, tag: tag, value: value, style: r.opts.DefaultStyle}
}

func identityOf(v any) (identity, bool) {
	switch t := v.(type) {
	case []any:
		if cap(t) == 0 {
			return identity{}, false // no backing array to tell lists apart
		}
		return identity{kind: 's', ptr: reflect.ValueOf(t).Pointer(), n: len(t)}, true
	case map[string]any:
		if t == nil {
			return identity{}, false
		}
		return identity{kind: 'm', ptr: reflect.ValueOf(t).Pointer()}, true
	case *OMap:
		if t == nil {
			return identity{}, false
		}
		return identity{kind: 'o', ptr: reflect.ValueOf(t).Pointer()}, true
	}
	return identity{}, false
}

func (r *representer) represent(v any) (*rnode, error) {
	if r.opts.Normalize != nil {
		v = r.opts.Normalize(v)
	}
	id, track := identityOf(v)
	if track {
		if n, ok := r.objects[id]; ok {
			return n, nil
		}
	}
	switch t := v.(type) {
	case nil:
		return r.scalar(tagNull, "null"), nil
	case bool:
		return r.scalar(tagBool, strconv.FormatBool(t)), nil
	case int:
		return r.scalar(tagInt, strconv.Itoa(t)), nil
	case *big.Int:
		return r.scalar(tagInt, t.String()), nil
	case int64:
		return r.scalar(tagInt, strconv.FormatInt(t, 10)), nil
	case float64:
		return r.scalar(tagFloat, reprFloat(t)), nil
	case string:
		return r.scalar(tagStr, t), nil
	case UnsafeString:
		return r.scalar(tagStr, string(t)), nil
	case VaultedString:
		return &rnode{kind: rScalar, tag: "!vault", value: t.Ciphertext, style: '|'}, nil
	case Datetime:
		return r.scalar(tagTime, t.Isoformat(" ")), nil
	case Date:
		return r.scalar(tagTime, t.Isoformat()), nil
	case []any:
		n := &rnode{kind: rSequence, tag: tagSeq}
		if track {
			r.objects[id] = n
		}
		best := true
		for _, item := range t {
			in, err := r.represent(item)
			if err != nil {
				return nil, err
			}
			if in.kind != rScalar || in.style != 0 {
				best = false
			}
			n.items = append(n.items, in)
		}
		n.flow = r.flowStyle(best)
		return n, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // a Go map has no insertion order to keep
		return r.mapping(id, track, keys, func(k string) any { return t[k] })
	case *OMap:
		keys := append([]string(nil), t.Keys()...)
		if r.opts.SortKeys {
			sort.Strings(keys)
		}
		n, err := r.mapping(id, track, keys, t.Get)
		if err != nil {
			return nil, err
		}
		// A key that is not a string is represented as itself.
		for i, k := range keys {
			if typed, ok := t.TypedKey(k); ok {
				kn, err := r.represent(typed)
				if err != nil {
					return nil, err
				}
				n.items[2*i] = kn
			}
		}
		return n, nil
	case OrderedMap:
		keys := make([]string, len(t))
		vals := make(map[string]any, len(t))
		for i, kv := range t {
			keys[i] = kv.K
			vals[kv.K] = kv.V
		}
		if r.opts.SortKeys {
			sort.Strings(keys)
		}
		return r.mapping(id, false, keys, func(k string) any { return vals[k] })
	}
	// Other slices, maps and numbers from Go callers, via reflection.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return r.represent(items)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			m := make(map[string]any, rv.Len())
			for _, k := range rv.MapKeys() {
				m[k.String()] = rv.MapIndex(k).Interface()
			}
			return r.represent(m)
		}
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint, reflect.Uint8,
		reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return r.represent(rv.Convert(reflect.TypeOf(int64(0))).Interface())
	case reflect.Float32:
		return r.represent(rv.Float())
	case reflect.String:
		return r.represent(rv.String())
	}
	repr := fmt.Sprint(v)
	if p, ok := v.(interface{ PyRepr() string }); ok {
		repr = p.PyRepr()
	}
	return nil, fmt.Errorf("('cannot represent an object', %s)", repr)
}

func (r *representer) mapping(id identity, track bool, keys []string, get func(string) any) (*rnode, error) {
	n := &rnode{kind: rMapping, tag: tagMap}
	if track {
		r.objects[id] = n
	}
	best := true
	for _, k := range keys {
		kn, err := r.represent(k)
		if err != nil {
			return nil, err
		}
		vn, err := r.represent(get(k))
		if err != nil {
			return nil, err
		}
		if kn.kind != rScalar || kn.style != 0 || vn.kind != rScalar || vn.style != 0 {
			best = false
		}
		n.items = append(n.items, kn, vn)
	}
	n.flow = r.flowStyle(best)
	return n, nil
}

func (r *representer) flowStyle(best bool) bool {
	if r.opts.DefaultFlowStyle != nil {
		return *r.opts.DefaultFlowStyle
	}
	return best
}

// reprFloat is SafeRepresenter.represent_float: repr(f).lower(), with
// ".0" added to an exponent form that has no fraction.
func reprFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	s := pyFloatRepr(f)
	if !strings.Contains(s, ".") && strings.Contains(s, "e") {
		s = strings.Replace(s, "e", ".0e", 1)
	}
	return s
}

// pyFloatRepr is Python's repr() of a finite float: the shortest
// round-tripping digits, positional unless the decimal exponent is below
// -4 or at least 16.
func pyFloatRepr(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// ---- serializer ----------------------------------------------------------

type serializer struct {
	anchors    map[*rnode]string
	serialized map[*rnode]bool
	lastID     int
	events     []emitEvent
}

func (s *serializer) anchorNode(n *rnode) {
	if a, seen := s.anchors[n]; seen {
		if a == "" {
			s.lastID++
			s.anchors[n] = fmt.Sprintf("id%03d", s.lastID)
		}
		return
	}
	s.anchors[n] = ""
	for _, item := range n.items {
		s.anchorNode(item)
	}
}

func (s *serializer) serializeNode(n *rnode) {
	anchor := s.anchors[n]
	if s.serialized[n] {
		s.events = append(s.events, emitEvent{kind: eeAlias, anchor: anchor})
		return
	}
	s.serialized[n] = true
	switch n.kind {
	case rScalar:
		style := stylePlain
		switch n.style {
		case '\'':
			style = styleSingle
		case '"':
			style = styleDouble
		case '|':
			style = styleLiteral
		case '>':
			style = styleFolded
		}
		s.events = append(s.events, emitEvent{
			kind: eeScalar, anchor: anchor, tag: n.tag, value: n.value, style: style,
			implicit:       resolveTag(n.value, true) == n.tag,
			quotedImplicit: resolveTag(n.value, false) == n.tag,
		})
	case rSequence, rMapping:
		start, end, tag := eeSeqStart, eeSeqEnd, tagSeq
		if n.kind == rMapping {
			start, end, tag = eeMapStart, eeMapEnd, tagMap
		}
		s.events = append(s.events, emitEvent{kind: start, anchor: anchor, tag: n.tag, implicit: n.tag == tag, flow: n.flow})
		for _, item := range n.items {
			s.serializeNode(item)
		}
		s.events = append(s.events, emitEvent{kind: end})
	}
}

// ---- resolver (PyYAML's implicit resolvers) ----------------------------

// Python's re `$` also matches before a final newline.
var implicitResolvers = []struct {
	tag   string
	first string
	re    *regexp.Regexp
}{
	{tagBool, "yYnNtTfFoO", regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)\n?$`)},
	{tagFloat, "-+0123456789.", regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))\n?$`)},
	{tagInt, "-+0123456789", regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)\n?$`)},
	{"tag:yaml.org,2002:merge", "<", regexp.MustCompile(`^(?:<<)\n?$`)},
	{tagNull, "~nN", regexp.MustCompile(`^(?:~|null|Null|NULL|)\n?$`)},
	{tagTime, "0123456789", regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)\n?$`)},
	{"tag:yaml.org,2002:value", "=", regexp.MustCompile(`^(?:=)\n?$`)},
	{"tag:yaml.org,2002:yaml", "!&*", regexp.MustCompile(`^(?:!|&|\*)\n?$`)},
}

// resolveTag is Resolver.resolve for a scalar: the implicit tag a plain
// scalar with this value would get, or str for a quoted one.
func resolveTag(value string, plain bool) string {
	if plain {
		if value == "" {
			return tagNull
		}
		for _, res := range implicitResolvers {
			if strings.IndexByte(res.first, value[0]) >= 0 && res.re.MatchString(value) {
				return res.tag
			}
		}
	}
	return tagStr
}
