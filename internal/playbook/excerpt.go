package playbook

import (
	"os"
	"strings"
	"sync"

	"github.com/giraffesyo/understudy/internal/template"
)

var (
	srcMu    sync.Mutex
	srcCache = map[string][]string{}
)

// SourceContext is ansible-core's annotated source excerpt for an origin:
// up to two lines of context plus the target line, right-aligned line
// numbers, and a caret under the column. Annotated lines are at most 120
// columns; longer source lines are cut with "...", and a caret beyond the
// usable width is omitted. "" when the file or line is unavailable.
func SourceContext(file string, line, col int) string {
	srcMu.Lock()
	lines, ok := srcCache[file]
	if !ok {
		if data, err := os.ReadFile(file); err == nil {
			lines = strings.Split(string(data), "\n")
		}
		srcCache[file] = lines
	}
	srcMu.Unlock()
	return template.ExcerptLines(lines, line, col)
}
