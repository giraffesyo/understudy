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
	// Lines wider than 120 columns (with the label) are cut with "...",
	// and the column marker is shown only when it falls on visible text.
	const maxWidth, marker = 120, "..."
	maxLen := maxWidth - width - 1
	usable := maxLen
	var b strings.Builder
	for n := max(1, line-2); n <= line; n++ {
		src := []rune(strings.ReplaceAll(strings.TrimRight(lines[n-1], "\r"), "\t", " "))
		if len(src) > maxLen {
			src = append(src[:maxLen-len(marker)], []rune(marker)...)
			usable = maxLen - len(marker)
		}
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("%*d %s", width, n, string(src)), " \t\r"))
	}
	if col >= 1 && col <= usable {
		label := fmt.Sprintf("column %d", col)
		if col-1+2+len(label) > maxLen {
			fmt.Fprintf(&b, "%s%s ^\n", strings.Repeat(" ", width+1+max(col-1-len(label)-1, 0)), label)
		} else {
			fmt.Fprintf(&b, "%s^ %s\n", strings.Repeat(" ", width+1+col-1), label)
		}
	} else if col < 1 {
		fmt.Fprintf(&b, "%s^ column %d\n", strings.Repeat(" ", width+1), col)
	}
	return b.String()
}
