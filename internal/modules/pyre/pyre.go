// Package pyre gives Go regular expressions Python's re match-iteration
// semantics (re.sub, re.subn, re.findall, re.finditer), shared by the
// target-side modules and the control-side template filters. Standard
// library only: it compiles into the agent.
package pyre

import (
	"regexp"
	"unicode/utf8"
)

// FindAllSubmatchIndex returns the successive non-overlapping matches of re
// in s as Python 3.7+ iterates them. It differs from Go's
// FindAllStringSubmatchIndex in one rule: Python allows an empty match
// immediately after a non-empty one (re.sub('x*', '-', 'abxd') gives
// '-a-b--d-', where Go's ReplaceAllString gives '-a-b-d-'). After an empty
// match both engines move on; Python may still take a non-empty match at
// the same position by backtracking into a lower-priority alternative,
// which RE2 cannot express, so there the next position is searched (the
// same as Go). n < 0 means all matches.
func FindAllSubmatchIndex(re *regexp.Regexp, s string, n int) [][]int {
	var out [][]int
	var shifted *regexp.Regexp
	pos := 0
	mustAdvance := false
	for pos <= len(s) && (n < 0 || len(out) < n) {
		var m []int
		if pos == 0 {
			m = re.FindStringSubmatchIndex(s)
		} else {
			if shifted == nil {
				var err error
				shifted, err = regexp.Compile(`(?s:.)(?:` + re.String() + `)`)
				if err != nil {
					// Cannot happen for a pattern that compiled; fall back.
					return re.FindAllStringSubmatchIndex(s, n)
				}
			}
			m = searchFrom(shifted, s, pos)
		}
		if m == nil {
			break
		}
		if mustAdvance && m[0] == pos && m[1] == pos {
			if pos >= len(s) {
				break
			}
			_, w := utf8.DecodeRuneInString(s[pos:])
			pos += w
			mustAdvance = false
			continue
		}
		out = append(out, m)
		mustAdvance = m[0] == m[1]
		pos = m[1]
	}
	return out
}

// searchFrom is a leftmost search for re's pattern starting at pos > 0 with
// the text before pos still visible to ^, \b and friends: shifted is
// "(?s:.)(?:pattern)" run from the rune before pos, so the pattern part
// starts at or after pos and sees its real left context. Leftmost-first
// semantics make the pattern's match at each start the one re would find.
func searchFrom(shifted *regexp.Regexp, s string, pos int) []int {
	_, w := utf8.DecodeLastRuneInString(s[:pos])
	base := pos - w
	m := shifted.FindStringSubmatchIndex(s[base:])
	if m == nil {
		return nil
	}
	for i := range m {
		if m[i] >= 0 {
			m[i] += base
		}
	}
	_, w0 := utf8.DecodeRuneInString(s[m[0]:])
	m[0] += w0
	return m
}
