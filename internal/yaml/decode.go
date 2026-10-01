package yaml

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Warning is a non-fatal load diagnostic (a duplicate mapping key), with
// the position ansible-core reports it at.
type Warning struct {
	Msg, Help string
	File      string
	Line, Col int
	// Value stands in for the source when the key has no origin (Python's
	// bools cannot carry one); "" with Line 0 means no context at all (None).
	Value string
	// Key is the key's text, shown in place of a source that cannot be
	// read (YAML text from the command line).
	Key string
}

// OnWarning, if set, receives load warnings. The CLI points this at its
// display layer.
var OnWarning func(Warning)

// DuplicateKeyMode is ansible-core's DUPLICATE_YAML_DICT_KEY: "warn" (the
// default), "error" or "ignore".
var DuplicateKeyMode = "warn"

const (
	tagPrefix  = "tag:yaml.org,2002:"
	tagStr     = tagPrefix + "str"
	tagInt     = tagPrefix + "int"
	tagFloat   = tagPrefix + "float"
	tagBool    = tagPrefix + "bool"
	tagNull    = tagPrefix + "null"
	tagBinary  = tagPrefix + "binary"
	tagTime    = tagPrefix + "timestamp"
	tagSeq     = tagPrefix + "seq"
	tagMap     = tagPrefix + "map"
	tagSet     = tagPrefix + "set"
	tagOmap    = tagPrefix + "omap"
	tagPairs   = tagPrefix + "pairs"
	tagPyDict  = tagPrefix + "python/dict"
	tagPyUni   = tagPrefix + "python/unicode"
	tagUnsafe  = "!unsafe"
	tagVault   = "!vault"
	tagVaultEn = "!vault-encrypted"
)

// ctorKind is the node kind a tag's constructor accepts.
func ctorKind(tag string) (Kind, bool) {
	switch tag {
	case tagStr, tagPyUni, tagInt, tagFloat, tagBool, tagNull, tagBinary, tagTime:
		return ScalarNode, true
	case tagSeq, tagOmap, tagPairs:
		return SequenceNode, true
	case tagMap, tagPyDict, tagSet:
		return MappingNode, true
	}
	return 0, false
}

// Decode converts a node tree to Go values: nil, bool, int64, float64,
// string, []any, *OMap, VaultedString, or UnsafeString, as ansible-core's
// constructor builds them: YAML 1.1 implicit typing for plain scalars,
// merge keys (<<) applied in PyYAML's order, aliases followed.
//
// A node is constructed once: every alias to it is the same value, and an
// alias inside the collection it names makes a recursive structure, as
// PyYAML builds them.
func (n *Node) Decode() (any, error) { return n.decode(false) }

func (n *Node) decode(unsafe bool) (any, error) {
	return n.decodeIn(&decodeState{memo: map[memoKey]any{}}, unsafe)
}

// decodeState is one Decode's constructed collections, by node.
type decodeState struct {
	memo map[memoKey]any
}

type memoKey struct {
	n      *Node
	unsafe bool
}

func (n *Node) decodeIn(st *decodeState, unsafe bool) (any, error) {
	n = n.resolveAlias()
	switch n.Tag {
	case tagUnsafe:
		return n.decodeImplicit(st, true)
	case tagVault, tagVaultEn:
		v, err := n.decodeImplicit(st, unsafe)
		if err != nil {
			return nil, err
		}
		switch s := v.(type) {
		case string:
			return VaultedString{Ciphertext: s}, nil
		case UnsafeString:
			return VaultedString{Ciphertext: string(s)}, nil
		}
		return nil, n.ansibleErr(fmt.Sprintf("the %s tag requires a string value", pyRepr(n.Tag)))
	case "", "!":
		return n.decodeImplicit(st, unsafe)
	}
	if k, ok := ctorKind(n.Tag); !ok {
		return nil, n.markedErr("", fmt.Sprintf("could not determine a constructor for the tag %s", pyRepr(n.Tag)))
	} else if k != n.Kind {
		return nil, n.markedErr("", fmt.Sprintf("expected a %s node, but found %s", kindName(k), kindName(n.Kind)))
	}
	switch n.Kind {
	case ScalarNode:
		return n.decodeScalar(n.Tag, unsafe)
	case SequenceNode:
		return n.decodeSequence(st, unsafe)
	}
	return n.decodeMapping(st, unsafe)
}

// decodeImplicit constructs a node by its implicitly resolved tag. A "!"
// tag and ansible's !unsafe resolve even quoted scalars as if plain.
func (n *Node) decodeImplicit(st *decodeState, unsafe bool) (any, error) {
	switch n.Kind {
	case SequenceNode:
		return n.decodeSequence(st, unsafe)
	case MappingNode:
		return n.decodeMapping(st, unsafe)
	}
	if n.Style == Plain || n.Tag != "" {
		v, err := resolveScalar(n.Value)
		if err != nil {
			return nil, err
		}
		if s, ok := v.(string); ok && unsafe {
			return UnsafeString(s), nil
		}
		return v, nil
	}
	if unsafe {
		return UnsafeString(n.Value), nil
	}
	return n.Value, nil
}

func kindName(k Kind) string {
	switch k {
	case SequenceNode:
		return "sequence"
	case MappingNode:
		return "mapping"
	}
	return "scalar"
}

func (n *Node) decodeScalar(tag string, unsafe bool) (any, error) {
	v := n.Value
	switch tag {
	case tagInt:
		return constructInt(v)
	case tagFloat:
		return constructFloat(v)
	case tagBool:
		switch strings.ToLower(v) {
		case "yes", "true", "on":
			return true, nil
		case "no", "false", "off":
			return false, nil
		}
		return nil, &Error{Msg: pyRepr(strings.ToLower(v))} // KeyError
	case tagNull:
		return nil, nil
	case tagTime:
		return constructTimestamp(v)
	case tagBinary:
		b, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			if r == '\n' || r == ' ' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, v))
		if err != nil {
			return nil, n.markedErr("", fmt.Sprintf("failed to decode base64 data: %v", err))
		}
		return string(b), nil
	}
	if unsafe {
		return UnsafeString(v), nil
	}
	return v, nil
}

// constructInt is PyYAML's construct_yaml_int; failures carry Python's
// int() error text.
func constructInt(s string) (any, error) {
	value := strings.ReplaceAll(s, "_", "")
	sign := int64(1)
	if value != "" && value[0] == '-' {
		sign = -1
	}
	if value != "" && (value[0] == '-' || value[0] == '+') {
		value = value[1:]
	}
	base, digits := 10, value
	switch {
	case value == "0":
		return int64(0), nil
	case strings.HasPrefix(value, "0b"):
		base, digits = 2, value[2:]
	case strings.HasPrefix(value, "0x"):
		base, digits = 16, value[2:]
	case value == "":
		return nil, &Error{Msg: "string index out of range"}
	case value[0] == '0':
		base = 8 // int(value, 8) takes a 0o prefix too
		if len(value) > 1 && (value[1] == 'o' || value[1] == 'O') {
			digits = value[2:]
		}
	case strings.Contains(value, ":"):
		var total int64
		for _, part := range strings.Split(value, ":") {
			d, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, &Error{Msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyRepr(part))}
			}
			total = total*60 + d
		}
		return sign * total, nil
	}
	d, err := strconv.ParseInt(strings.TrimSpace(digits), base, 64)
	if err != nil {
		// Python ints have no size limit.
		if b, ok := new(big.Int).SetString(strings.TrimSpace(digits), base); ok && !strings.ContainsAny(digits, "+-_") {
			if sign < 0 {
				b.Neg(b)
			}
			if b.IsInt64() {
				return b.Int64(), nil
			}
			return b, nil
		}
		return nil, &Error{Msg: fmt.Sprintf("invalid literal for int() with base %d: %s", base, pyRepr(digits))}
	}
	return sign * d, nil
}

// constructFloat is PyYAML's construct_yaml_float.
func constructFloat(s string) (any, error) {
	value := strings.ToLower(strings.ReplaceAll(s, "_", ""))
	sign := 1.0
	if value != "" && value[0] == '-' {
		sign = -1
	}
	if value != "" && (value[0] == '-' || value[0] == '+') {
		value = value[1:]
	}
	switch {
	case value == ".inf":
		return sign * math.Inf(1), nil
	case value == ".nan":
		return math.NaN(), nil
	case strings.Contains(value, ":"):
		total := 0.0
		for _, part := range strings.Split(value, ":") {
			d, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				return nil, &Error{Msg: fmt.Sprintf("could not convert string to float: %s", pyRepr(part))}
			}
			total = total*60 + d
		}
		return sign * total, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || strings.HasPrefix(strings.TrimSpace(value), "0x") {
		return nil, &Error{Msg: fmt.Sprintf("could not convert string to float: %s", pyRepr(value))}
	}
	return sign * f, nil
}

func (n *Node) decodeSequence(st *decodeState, unsafe bool) (any, error) {
	key := memoKey{n, unsafe}
	if v, ok := st.memo[key]; ok {
		return v, nil
	}
	// Every list gets its own backing array, even an empty one, so it has
	// an identity (to_yaml aliases a list referenced twice, as PyYAML
	// does by id(); zero-capacity slices would all share one address).
	// It is registered at its full length before its items are built, so
	// an item that aliases it shares it.
	out := make([]any, len(n.Content), max(len(n.Content), 1))
	st.memo[key] = out
	items := make(map[string]*Node, len(n.Content))
	for i, item := range n.Content {
		v, err := item.decodeIn(st, unsafe)
		if err != nil {
			return nil, err
		}
		out[i] = v
		items[indexKey(i)] = item
	}
	recordContainer(out, n, items)
	return out, nil
}

func (n *Node) decodeMapping(st *decodeState, unsafe bool) (any, error) {
	key := memoKey{n, unsafe}
	if v, ok := st.memo[key]; ok {
		return v, nil
	}
	pairs, err := n.flatten()
	if err != nil {
		return nil, err
	}
	out := NewOMap()
	st.memo[key] = out
	items := make(map[string]*Node, len(pairs)/2)
	defer recordContainer(out, n, items)
	names := map[any]string{} // Python-equal keys share the first's name
	for i := 0; i+1 < len(pairs); i += 2 {
		k, err := pairs[i].decodeIn(st, false)
		if err != nil {
			return nil, err
		}
		v, err := pairs[i+1].decodeIn(st, unsafe)
		if err != nil {
			return nil, err
		}
		id := pyKeyIdentity(k)
		name, ok := names[id]
		if !ok {
			name = keyString(k)
			names[id] = name
		}
		out.Set(name, v)
		items[name] = pairs[i+1]
	}
	return out, nil
}

// keyString is a decoded mapping key as the string OMap stores: Python's
// str() of the key.
func keyString(k any) string {
	switch t := k.(type) {
	case string:
		return t
	case UnsafeString:
		return string(t)
	case Datetime:
		return t.Isoformat("T")
	case Date:
		return t.Isoformat()
	}
	return pyValueRepr(k)
}

func isMergeKey(k *Node) bool {
	return k.Kind == ScalarNode && k.Tag == "" && k.Style == Plain && k.Value == "<<"
}

// flatten returns a mapping's key/value nodes with merge keys expanded as
// PyYAML's flatten_mapping does: the merged pairs come first (for a list of
// sources, later sources first, so earlier ones win), then the mapping's own
// pairs, so its own keys override merged ones.
func (n *Node) flatten() ([]*Node, error) { return n.flattenIn(map[*Node]bool{}) }

// flattenIn is flatten with the mappings being flattened: one that merges
// itself cannot be built.
func (n *Node) flattenIn(active map[*Node]bool) ([]*Node, error) {
	if active[n] {
		return nil, n.markedErr("", "found unconstructable recursive node")
	}
	active[n] = true
	defer delete(active, n)
	var merge, own []*Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i].resolveAlias(), n.Content[i+1]
		if !isMergeKey(key) {
			own = append(own, n.Content[i], value)
			continue
		}
		switch v := value.resolveAlias(); v.Kind {
		case MappingNode:
			sub, err := v.flattenIn(active)
			if err != nil {
				return nil, err
			}
			merge = append(merge, sub...)
		case SequenceNode:
			var submerge [][]*Node
			for _, item := range v.Content {
				it := item.resolveAlias()
				if it.Kind != MappingNode {
					return nil, n.mappingErr(fmt.Sprintf("expected a mapping for merging, but found %s", kindName(it.Kind)), item)
				}
				sub, err := it.flattenIn(active)
				if err != nil {
					return nil, err
				}
				submerge = append(submerge, sub)
			}
			for i := len(submerge) - 1; i >= 0; i-- {
				merge = append(merge, submerge[i]...)
			}
		default:
			return nil, n.mappingErr(fmt.Sprintf("expected a mapping or list of mappings for merging, but found %s", kindName(v.Kind)), value)
		}
	}
	return append(merge, own...), nil
}

// markedErr is a constructor error at the node.
func (n *Node) markedErr(context, problem string) *Error {
	return &Error{Line: n.Line, Col: n.Column, Context: context, Problem: problem}
}

// mappingErr is a "while constructing a mapping" error at the node at.
func (n *Node) mappingErr(problem string, at *Node) *Error {
	return &Error{Line: at.Line, Col: at.Column, Context: "while constructing a mapping", Problem: problem}
}

// ansibleErr is an error raised by ansible-core's own constructor.
func (n *Node) ansibleErr(problem string) *Error {
	return &Error{Line: n.Line, Col: n.Column, Problem: problem, Ansible: true}
}

// construct checks a composed document the way ansible-core's constructor
// builds it, raising the constructor errors it raises and emitting its
// duplicate-key warnings, in its order: PyYAML constructs collections
// breadth-first (each mapping's or sequence's items once its parent is
// done), scalars as their collection is built.
func construct(root *Node, file string, src []byte) error {
	c := &constructor{seen: map[*Node]bool{}}
	err := c.run(root)
	if e, ok := err.(*Error); ok {
		e.File, e.src = file, src
	}
	for i := range c.warnings {
		c.warnings[i].File = file
		if OnWarning != nil {
			OnWarning(c.warnings[i])
		}
	}
	return err
}

type constructor struct {
	seen     map[*Node]bool
	queue    []*Node
	warnings []Warning
}

func (c *constructor) run(root *Node) error {
	if err := c.object(root); err != nil {
		return err
	}
	for len(c.queue) > 0 {
		n := c.queue[0]
		c.queue = c.queue[1:]
		if err := c.collection(n); err != nil {
			return err
		}
	}
	return nil
}

// object is construct_object: a scalar is built (and checked) now, a
// collection is queued.
func (c *constructor) object(n *Node) error {
	n = n.resolveAlias()
	if c.seen[n] {
		return nil
	}
	c.seen[n] = true
	switch n.Tag {
	case tagUnsafe, tagVault, tagVaultEn:
		// Built deep, right away.
		_, err := n.decode(false)
		return err
	case "", "!":
		if n.Kind != ScalarNode {
			c.queue = append(c.queue, n)
		} else if (n.Style == Plain || n.Tag != "") && resolveTag(n.Value, true) == tagTime {
			// A timestamp is constructed now: a date that does not exist fails.
			_, err := n.decode(false)
			return err
		}
		return nil
	}
	if k, ok := ctorKind(n.Tag); !ok || k != n.Kind || n.Kind == ScalarNode {
		_, err := n.decode(false)
		return err
	}
	c.queue = append(c.queue, n)
	return nil
}

func (c *constructor) collection(n *Node) error {
	if n.Kind == SequenceNode {
		for _, item := range n.Content {
			if err := c.object(item); err != nil {
				return err
			}
		}
		return nil
	}
	pairs, err := n.flatten()
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		key := pairs[i]
		if err := c.object(key); err != nil {
			return err
		}
		if k := key.resolveAlias(); k.Kind != ScalarNode {
			return n.mappingErr("found unhashable key", key)
		}
		if err := c.object(pairs[i+1]); err != nil {
			return err
		}
	}
	// ansible-core's duplicate key check, over the flattened pairs.
	seen := map[any]bool{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key := pairs[i]
		v, err := key.decode(false)
		if err != nil {
			return err
		}
		id := pyKeyIdentity(v)
		if !seen[id] {
			seen[id] = true
			continue
		}
		msg := fmt.Sprintf("Found duplicate mapping key %s.", pyValueRepr(v))
		switch DuplicateKeyMode {
		case "error":
			return key.ansibleErr(msg)
		case "warn":
			w := Warning{Msg: msg, Help: "Using last defined value only."}
			switch v.(type) {
			case bool:
				w.Value = pyValueRepr(v)
			case nil:
			default:
				w.Line, w.Col = key.Line, key.Column
				w.Key = fmt.Sprint(v)
			}
			c.warnings = append(c.warnings, w)
		}
	}
	return nil
}

// pyKeyIdentity maps a decoded key to a value equal where Python's dict
// keys are equal (True == 1 == 1.0).
func pyKeyIdentity(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1<<62 {
			return int64(t)
		}
		return t
	case UnsafeString:
		return string(t)
	}
	return v
}

// pyValueRepr is Python's repr() of a decoded scalar.
func pyValueRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyRepr(t)
	case UnsafeString:
		return pyRepr(string(t))
	case float64:
		switch {
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		case math.IsNaN(t):
			return "nan"
		}
		s := strconv.FormatFloat(t, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") {
			s += ".0"
		}
		return s
	case interface{ Repr() string }:
		return t.Repr()
	}
	return fmt.Sprint(v)
}

// pyRepr is Python's repr() of a str.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
