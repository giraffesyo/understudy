package template

import "fmt"

// Position locates template source for error messages. File/Line/Col refer
// to the enclosing document (e.g. the playbook YAML node the template string
// came from); Offset is the rune offset within the template string itself.
type Position struct {
	File string
	Line int
	Col  int

	// InContainer marks a template that is one item of a templated
	// container (a list or dict argument): deprecated values its result
	// still holds are reported for the container, whose origin is
	// unknown, rather than at File:Line:Col.
	InContainer bool
}

// ContainerOrigin is the origin a deprecated value found while finishing
// a templated container is reported at: ansible-core's "<<container>>".
var ContainerOrigin = Position{File: "<<container>>"}

func (p Position) String() string {
	if p.File == "" {
		return "template"
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

type tokKind uint8

const (
	tokEOF        tokKind = iota
	tokText               // literal template text
	tokVarStart           // {{
	tokVarEnd             // }}
	tokBlockStart         // {%
	tokBlockEnd           // %}
	tokName
	tokInt
	tokFloat
	tokString
	// Operators.
	tokAdd      // +
	tokSub      // -
	tokMul      // *
	tokPow      // **
	tokDiv      // /
	tokFloorDiv // //
	tokMod      // %
	tokTilde    // ~
	tokEq       // ==
	tokNe       // !=
	tokLt       // <
	tokLe       // <=
	tokGt       // >
	tokGe       // >=
	tokAssign   // =
	tokPipe     // |
	tokDot      // .
	tokComma    // ,
	tokColon    // :
	tokLParen   // (
	tokRParen   // )
	tokLBracket // [
	tokRBracket // ]
	tokLBrace   // {
	tokRBrace   // }
)

var tokLabels = map[tokKind]string{
	tokEOF: "end of template", tokText: "text", tokVarStart: "'{{'",
	tokVarEnd: "'}}'", tokBlockStart: "'{%'", tokBlockEnd: "'%}'",
	tokName: "name", tokInt: "integer", tokFloat: "float", tokString: "string",
	tokAdd: "'+'", tokSub: "'-'", tokMul: "'*'", tokPow: "'**'", tokDiv: "'/'",
	tokFloorDiv: "'//'", tokMod: "'%'", tokTilde: "'~'", tokEq: "'=='",
	tokNe: "'!='", tokLt: "'<'", tokLe: "'<='", tokGt: "'>'", tokGe: "'>='",
	tokAssign: "'='", tokPipe: "'|'", tokDot: "'.'", tokComma: "','",
	tokColon: "':'", tokLParen: "'('", tokRParen: "')'",
	tokLBracket: "'['", tokRBracket: "']'", tokLBrace: "'{'", tokRBrace: "'}'",
}

func (k tokKind) String() string { return tokLabels[k] }

type token struct {
	kind tokKind
	val  string
	off  int // rune offset in the template source, for error context
}
