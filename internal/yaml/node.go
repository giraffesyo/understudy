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

// MapGet returns the value node for a plain string key, or nil. The receiver
// may be an alias to a mapping. Merge keys are NOT consulted here (Decode
// handles them); structural walkers deal in literal keys.
func (n *Node) MapGet(key string) *Node {
	n = n.resolveAlias()
	if n == nil || n.Kind != MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].resolveAlias()
		if k.Kind == ScalarNode && k.Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// MapKeyNode returns the key node for a plain string key, or nil.
func (n *Node) MapKeyNode(key string) *Node {
	n = n.resolveAlias()
	if n == nil || n.Kind != MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].resolveAlias()
		if k.Kind == ScalarNode && k.Value == key {
			return n.Content[i]
		}
	}
	return nil
}

// MapKeys returns the scalar keys of a mapping in source order.
func (n *Node) MapKeys() []string {
	n = n.resolveAlias()
	if n == nil || n.Kind != MappingNode {
		return nil
	}
	keys := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i].resolveAlias(); k.Kind == ScalarNode {
			keys = append(keys, k.Value)
		}
	}
	return keys
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
