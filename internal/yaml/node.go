package yaml

// Kind is the structural kind of a Node.
type Kind uint8

const (
	ScalarNode Kind = iota
	MappingNode
	SequenceNode
	AliasNode
)

// Style records how a scalar was written in the source. Resolution (1.1
// typing) applies only to Plain scalars; every other style is a string.
type Style uint8

const (
	Plain Style = iota
	SingleQuoted
	DoubleQuoted
	Literal   // |
	Folded    // >
	FlowStyle // came from {} / [] (collections only)
)

// Node is a parsed YAML node with source position. Playbook loaders walk the
// tree for structure (keeping line info for errors) and call Decode for values.
type Node struct {
	Kind    Kind
	Tag     string // "" if untagged; "!vault", "!unsafe" are the only app tags
	Value   string // scalar text after quote/block-scalar processing
	Style   Style
	Anchor  string
	Target  *Node   // AliasNode: the anchored node
	Content []*Node // MappingNode: k,v,k,v...; SequenceNode: items
	Line    int     // 1-based
	Column  int     // 1-based

	recursive bool // a collection that contains an alias to itself
}

// File is one parsed YAML file: zero or more documents.
type File struct {
	Name string
	Docs []*Node
}

// resolveAlias follows an alias to its target. Alias cycles are rejected at
// parse time, so this terminates.
func (n *Node) resolveAlias() *Node {
	for n != nil && n.Kind == AliasNode {
		n = n.Target
	}
	return n
}

// entries returns a mapping's key and value nodes as the constructed dict
// holds them: merge keys expanded, and a repeated key kept at its first
// position (and key node) with its last value. nil if n is not a mapping.
func (n *Node) entries() (keys, values []*Node) {
	n = n.resolveAlias()
	if n == nil || n.Kind != MappingNode {
		return nil, nil
	}
	pairs, err := n.flatten()
	if err != nil {
		pairs = n.Content
	}
	index := map[any]int{}
	for i := 0; i+1 < len(pairs); i += 2 {
		k := pairs[i].resolveAlias()
		if k.Kind != ScalarNode {
			continue
		}
		var id any = k.Value
		if v, err := k.decode(false); err == nil {
			id = pyKeyIdentity(v)
		}
		if j, ok := index[id]; ok {
			values[j] = pairs[i+1]
			continue
		}
		index[id] = len(keys)
		keys = append(keys, pairs[i])
		values = append(values, pairs[i+1])
	}
	return keys, values
}

// keyName is a key node's name as a constructed dict key.
func keyName(k *Node) string {
	k = k.resolveAlias()
	if v, err := k.decode(false); err == nil {
		return keyString(v)
	}
	return k.Value
}

// KeyName is a key node's name as a constructed dict key.
func KeyName(k *Node) string { return keyName(k) }

// MapGet returns the value node for a key, or nil. The receiver may be an
// alias to a mapping; merge keys are expanded.
func (n *Node) MapGet(key string) *Node {
	keys, values := n.entries()
	for i, k := range keys {
		if keyName(k) == key {
			return values[i]
		}
	}
	return nil
}

// MapKeyNode returns the key node for a key, or nil.
func (n *Node) MapKeyNode(key string) *Node {
	keys, _ := n.entries()
	for _, k := range keys {
		if keyName(k) == key {
			return k
		}
	}
	return nil
}

// MapKeys returns a mapping's keys in the constructed dict's order.
func (n *Node) MapKeys() []string {
	keys, _ := n.entries()
	if keys == nil {
		if m := n.resolveAlias(); m == nil || m.Kind != MappingNode {
			return nil
		}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyName(k))
	}
	return out
}

// IsNull reports whether the node is a scalar that constructs to None.
func (n *Node) IsNull() bool {
	if n = n.resolveAlias(); n == nil || n.Kind != ScalarNode {
		return false
	}
	v, err := n.Decode()
	return err == nil && v == nil
}

// Str returns the node's scalar text if it is a scalar.
func (n *Node) Str() (string, bool) {
	if n = n.resolveAlias(); n != nil && n.Kind == ScalarNode {
		return n.Value, true
	}
	return "", false
}

// Seq returns the item nodes if the node is a sequence.
func (n *Node) Seq() ([]*Node, bool) {
	if n = n.resolveAlias(); n != nil && n.Kind == SequenceNode {
		return n.Content, true
	}
	return nil, false
}

// VaultedString is the decoded form of a !vault-tagged scalar. Decryption is
// performed lazily by the vars layer via a configured hook.
type VaultedString struct {
	Ciphertext string
}

// UnsafeString is the decoded form of a !unsafe-tagged scalar. The template
// engine renders it but never re-templates its contents.
type UnsafeString string
