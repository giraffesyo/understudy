package template

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SourceExcerpt is the source context ansible-core shows under an error's
// or warning's "Origin:" line: up to three lines ending at line, then a
// caret at col.
func SourceExcerpt(file string, line, col int) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return ExcerptLines(strings.Split(string(data), "\n"), line, col)
}

// ExcerptLines is SourceExcerpt over a file's lines.
func ExcerptLines(lines []string, line, col int) string {
	if line > len(lines) || line < 1 {
		return ""
	}
	width := len(strconv.Itoa(line))
	var b strings.Builder
	for n := max(1, line-2); n <= line; n++ {
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("%*d %s", width, n, lines[n-1]), " \t\r"))
	}
	fmt.Fprintf(&b, "%s^ column %d\n", strings.Repeat(" ", width+1+max(col-1, 0)), col)
	return b.String()
}
