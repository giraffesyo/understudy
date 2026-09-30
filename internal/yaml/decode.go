package yaml

import (
	"encoding/base64"
	"fmt"
	"math"
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
func (n *Node) Decode() (any, error) { return n.decode(false) }

func (n *Node) decode(unsafe bool) (any, error) {
	n = n.resolveAlias()
	if n.recursive {
		return nil, n.markedErr("", "found unconstructable recursive node")
	}
	switch n.Tag {
	case tagUnsafe:
		return n.decodeImplicit(true)
	case tagVault, tagVaultEn:
		v, err := n.decodeImplicit(unsafe)
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
		return n.decodeImplicit(unsafe)
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
		return n.decodeSequence(unsafe)
	}
	return n.decodeMapping(unsafe)
}

// decodeImplicit constructs a node by its implicitly resolved tag. A "!"
// tag and ansible's !unsafe resolve even quoted scalars as if plain.
func (n *Node) decodeImplicit(unsafe bool) (any, error) {
	switch n.Kind {
	case SequenceNode:
		return n.decodeSequence(unsafe)
	case MappingNode:
		return n.decodeMapping(unsafe)
	}
	if n.Style == Plain || n.Tag != "" {
		v := resolveScalar(n.Value)
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
		base = 8
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

func (n *Node) decodeSequence(unsafe bool) (any, error) {
	out := make([]any, 0, len(n.Content))
	for _, item := range n.Content {
		v, err := item.decode(unsafe)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (n *Node) decodeMapping(unsafe bool) (any, error) {
	pairs, err := n.flatten()
	if err != nil {
		return nil, err
	}
	out := NewOMap()
	names := map[any]string{} // Python-equal keys share the first's name
	for i := 0; i+1 < len(pairs); i += 2 {
		k, err := pairs[i].decode(false)
		if err != nil {
			return nil, err
		}
		v, err := pairs[i+1].decode(unsafe)
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
func (n *Node) flatten() ([]*Node, error) {
	var merge, own []*Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i].resolveAlias(), n.Content[i+1]
		if !isMergeKey(key) {
			own = append(own, n.Content[i], value)
			continue
		}
		switch v := value.resolveAlias(); v.Kind {
		case MappingNode:
			sub, err := v.flatten()
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
				sub, err := it.flatten()
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
	if n.recursive {
		return n.markedErr("", "found unconstructable recursive node")
	}
	switch n.Tag {
	case tagUnsafe, tagVault, tagVaultEn:
		// Built deep, right away.
		_, err := n.decode(false)
		return err
	case "", "!":
		if n.Kind != ScalarNode {
			c.queue = append(c.queue, n)
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
