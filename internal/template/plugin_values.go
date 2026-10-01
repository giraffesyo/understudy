package template

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// IntEnum is a Python IntEnum or IntFlag a plugin returned (dnspython's
// Algorithm, Flag): an int to templates, which variable storage
// converts to a plain int with a warning.
type IntEnum struct {
	Class string // the enum's class name
	Value int64
}

// MarshalJSON encodes the plain int.
func (e IntEnum) MarshalJSON() ([]byte, error) { return json.Marshal(e.Value) }

func (e IntEnum) String() string { return strconv.FormatInt(e.Value, 10) }

// LookupError is a lookup plugin's exception: Msg as ansible-core's
// "The lookup plugin '...' failed: <Msg>" words it, and Help, when set,
// its help text ("Valid values are: ..."): an AnsibleError with help
// text, which ansible-core's error display shows as an event of its own.
//
// Separate marks an exception raised while handling another (Python's
// implicit __context__), which the display shows apart too.
type LookupError struct {
	Msg      string
	Help     string
	Separate bool
}

func (e *LookupError) Error() string { return e.Msg }

// lookupPluginError is AnsibleTemplatePluginRuntimeError for a lookup:
// "The lookup plugin 'name' failed: ...", name as the template gave it.
func (ec *EvalCtx) lookupPluginError(name string, le *LookupError) error {
	head := "The lookup plugin " + pyStrRepr(name) + " failed."
	msg := head
	if !strings.HasSuffix(head, le.Msg) {
		msg = strings.TrimRight(head, ". ") + ": " + le.Msg
	}
	te := &TemplateError{Pos: ec.pos, Msg: msg, Src: ec.src, Plugin: true}
	if le.Help != "" || le.Separate {
		te.pluginHead, te.pluginDetail, te.pluginHelp = head, le.Msg, le.Help
	}
	return te
}

// PluginHelp is the help text of a plugin failure's cause (see
// SplitCause), "" for none.
func PluginHelp(err error) string {
	var te *TemplateError
	if errors.As(err, &te) && te.Plugin {
		return te.pluginHelp
	}
	return ""
}
