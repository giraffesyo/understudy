package modules

import (
	"regexp"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// Python's re syntax errors and replacement templates (shared with the
// control side's regex filters).
var (
	pyRegexSyntaxError = pyre.SyntaxError
	parsePyTemplate    = pyre.ParseTemplate
	expandPyTemplate   = pyre.ExpandTemplate
)

type pyTplError = pyre.TemplateError

// pyCompile is re.compile as a module uses it: a Python syntax error
// escapes as an unhandled exception (module crash); a pattern RE2 cannot
// run fails with a clear message.
func pyCompile(pattern string) (*regexp.Regexp, *agentproto.Result) {
	if msg := pyRegexSyntaxError(pattern); msg != "" {
		return nil, &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + msg}
	}
	re, err := compilePyPattern(pattern)
	if err != nil {
		return nil, agentproto.Fail("%v", err)
	}
	return re, nil
}
