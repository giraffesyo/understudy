package modules

import (
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// Modules match with Python's re (the pyre port, shared with the control
// side's regex filters): its syntax, matching and replacement templates.

// pyCompile is re.compile(pattern, flags) as a module uses it: a
// re.error escapes as an unhandled exception (module crash).
func pyCompile(pattern string, flags ...pyre.Flag) (*pyre.Pattern, *agentproto.Result) {
	var f pyre.Flag
	for _, x := range flags {
		f |= x
	}
	re, err := pyre.Compile(pattern, f)
	if err != nil {
		return nil, &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + err.Error()}
	}
	return re, nil
}

// pySearch is re.search(line): the match's spans, or nil.
func pySearch(re *pyre.Pattern, line string) []int { return re.Search(line, 0, -1) }

// pySubn is Python's re.subn(pattern, repl, s): every non-overlapping match
// replaced, repl interpreted with Python's escape and group syntax (parsed
// up front, so a bad template fails even without matches).
func pySubn(re *pyre.Pattern, repl, s string) (string, int, error) {
	return re.Sub(repl, s, 0)
}

// pyExpand is match.expand(template) for one match.
func pyExpand(re *pyre.Pattern, repl, s string, m []int) (string, error) {
	parts, err := pyre.ParseTemplate(re, repl)
	if err != nil {
		return "", err
	}
	return pyre.ExpandTemplate(parts, s, m), nil
}

// isIndexError reports a replacement template's IndexError (an unknown
// group name), which modules do not catch.
func isIndexError(err error) bool {
	e, ok := err.(*pyre.Error)
	return ok && e.ExcName() == "IndexError"
}
