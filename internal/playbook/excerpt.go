package playbook

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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
	if line < 1 || line > len(lines) {
		return ""
	}
	width := len(strconv.Itoa(line))
	const maxAnnotated, marker = 120, "..."
	maxSrc := maxAnnotated - width - 1
	usable := maxSrc
	var b strings.Builder
	for n := max(1, line-2); n <= line; n++ {
		src := strings.ReplaceAll(lines[n-1], "\t", " ")
		if r := []rune(src); len(r) > maxSrc {
			src = string(r[:maxSrc-len(marker)]) + marker
			usable = maxSrc - len(marker)
		}
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("%*d %s", width, n, src), " \t\r"))
	}
	if col >= 1 && col <= usable {
		label := fmt.Sprintf("column %d", col)
		if col-1+2+len(label) > maxSrc {
			fmt.Fprintf(&b, "%s %s%s ^\n", strings.Repeat(" ", width), strings.Repeat(" ", max(col-1-len(label)-1, 0)), label)
		} else {
			fmt.Fprintf(&b, "%s^ %s\n", strings.Repeat(" ", width+1+col-1), label)
		}
	} else if col < 1 {
		fmt.Fprintf(&b, "%s^ column %d\n", strings.Repeat(" ", width+1), col)
	}
	return b.String()
}
