package yaml

// tokKind enumerates libyaml's token types. The scanner owns all
// indentation logic: block structure arrives as BLOCK-*-START/BLOCK-END
// tokens so the parser is a plain grammar walk.
type tokKind uint8

const (
	tokStreamStart tokKind = iota
	tokStreamEnd
	tokVersionDirective // %YAML
	tokTagDirective     // %TAG
	tokDocStart         // ---
	tokDocEnd           // ...
	tokBlockSeqStart
	tokBlockMapStart
	tokBlockEnd
	tokFlowSeqStart
	tokFlowSeqEnd
	tokFlowMapStart
	tokFlowMapEnd
	tokBlockEntry // "-" in block context
	tokFlowEntry  // "," in flow context
	tokKey
	tokValue // ":"
	tokAlias
	tokAnchor
	tokTag
	tokScalar
)

type token struct {
	kind       tokKind
	start, end mark
	val        string // scalar text, anchor/alias name, tag or %TAG handle
	suffix     string // tag suffix, %TAG prefix
	style      Style  // scalars
	major      int    // %YAML version
	minor      int
}
