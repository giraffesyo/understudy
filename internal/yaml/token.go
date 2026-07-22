package yaml

// tokKind enumerates scanner token types. The scanner owns all indentation
// logic: block structure arrives as synthetic *Start/End tokens so the parser
// is a plain grammar walk.
type tokKind uint8

const (
	tokStreamEnd tokKind = iota
	tokDocStart          // ---
	tokDocEnd            // ...
	tokBlockMapStart
	tokBlockSeqStart
	tokBlockEnd
	tokBlockEntry // "-" in block context
	tokFlowMapStart
	tokFlowMapEnd
	tokFlowSeqStart
	tokFlowSeqEnd
	tokFlowEntry // "," in flow context
	tokKey
	tokValue // ":"
	tokScalar
	tokAnchor // &name
	tokAlias  // *name
	tokTag    // !name, !!name
)

var tokNames = map[tokKind]string{
	tokStreamEnd:     "end of stream",
	tokDocStart:      "'---'",
	tokDocEnd:        "'...'",
	tokBlockMapStart: "start of block mapping",
	tokBlockSeqStart: "start of block sequence",
	tokBlockEnd:      "end of block collection",
	tokBlockEntry:    "'-'",
	tokFlowMapStart:  "'{'",
	tokFlowMapEnd:    "'}'",
	tokFlowSeqStart:  "'['",
	tokFlowSeqEnd:    "']'",
	tokFlowEntry:     "','",
	tokKey:           "mapping key",
	tokValue:         "':'",
	tokScalar:        "scalar",
	tokAnchor:        "anchor",
	tokAlias:         "alias",
	tokTag:           "tag",
}

func (k tokKind) String() string { return tokNames[k] }

type token struct {
	kind      tokKind
	val       string
	style     Style // for tokScalar
	line, col int   // 1-based start position
}
