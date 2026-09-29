package callback

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/template"
)

// This file ports what ansible-core's CallbackBase._get_diff needs from
// Python's difflib: SequenceMatcher (autojunk on, no junk function) and
// unified_diff, so --diff output is byte-identical.

// diffContext is C.DIFF_CONTEXT.
const diffContext = 3

// getDiff is CallbackBase._get_diff (colors applied per line).
func (d *Default) getDiff(diff any) string {
	var list []any
	switch t := diff.(type) {
	case []any:
		list = t
	case map[string]any:
		list = []any{t}
	default:
		return ""
	}
	var ret strings.Builder
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["dst_binary"]; ok {
			ret.WriteString("diff skipped: destination file appears to be binary\n")
		}
		if _, ok := m["src_binary"]; ok {
			ret.WriteString("diff skipped: source file appears to be binary\n")
		}
		if v, ok := m["dst_larger"]; ok {
			ret.WriteString("diff skipped: destination file size is greater than " + template.PyStr(v) + "\n")
		}
		if v, ok := m["src_larger"]; ok {
			ret.WriteString("diff skipped: source file size is greater than " + template.PyStr(v) + "\n")
		}
		before, hasBefore := m["before"]
		after, hasAfter := m["after"]
		if hasBefore && hasAfter {
			bs, as := diffSide(before), diffSide(after)
			beforeHeader, afterHeader := "before", "after"
			if h, ok := m["before_header"]; ok {
				beforeHeader = "before: " + template.PyStr(h)
			}
			if h, ok := m["after_header"]; ok {
				afterHeader = "after: " + template.PyStr(h)
			}
			bl, al := splitLinesKeep(bs), splitLinesKeep(as)
			if len(bl) > 0 && !strings.HasSuffix(bl[len(bl)-1], "\n") {
				bl[len(bl)-1] += "\n\\ No newline at end of file\n"
			}
			if len(al) > 0 && !strings.HasSuffix(al[len(al)-1], "\n") {
				al[len(al)-1] += "\n\\ No newline at end of file\n"
			}
			lines := unifiedDiff(bl, al, beforeHeader, afterHeader, diffContext)
			for _, line := range lines {
				switch {
				case strings.HasPrefix(line, "+"):
					line = d.stringc(line, cGreen)
				case strings.HasPrefix(line, "-"):
					line = d.stringc(line, cRed)
				case strings.HasPrefix(line, "@@"):
					line = d.stringc(line, cCyan)
				}
				ret.WriteString(line)
			}
			if len(lines) > 0 {
				ret.WriteString("\n")
			}
		}
		if v, ok := m["prepared"]; ok {
			ret.WriteString(template.PyStr(v))
		}
	}
	return ret.String()
}

// stringc is ansible's stringc: each '\n'-separated segment colored.
func (d *Default) stringc(s string, c color) string {
	if d.NoColor {
		return s
	}
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = d.paint(c, p)
	}
	return strings.Join(parts, "\n")
}

// diffSide formats one side: dicts as sorted, indented JSON; None as "".
func diffSide(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		return template.PyJSON(t, 4, true, true) + "\n"
	}
	return template.PyStr(v)
}

// splitLinesKeep is Python's str.splitlines(True).
func splitLinesKeep(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		next := i + w
		brk := false
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			brk = true
		case '\r':
			brk = true
			if next < len(s) && s[next] == '\n' {
				next++
			}
		}
		if brk {
			out = append(out, s[start:next])
			start = next
		}
		i = next
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

type match struct{ a, b, size int }

type opcode struct {
	tag            byte // 'e'qual, 'r'eplace, 'd'elete, 'i'nsert
	i1, i2, j1, j2 int
}

// sequenceMatcher is difflib.SequenceMatcher(None, a, b) over lines.
type sequenceMatcher struct {
	a, b []string
	b2j  map[string][]int
}

func newSequenceMatcher(a, b []string) *sequenceMatcher {
	m := &sequenceMatcher{a: a, b: b, b2j: map[string][]int{}}
	for j, line := range b {
		m.b2j[line] = append(m.b2j[line], j)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for k, idx := range m.b2j {
			if len(idx) > ntest {
				delete(m.b2j, k)
			}
		}
	}
	return m
}

func (m *sequenceMatcher) findLongestMatch(alo, ahi, blo, bhi int) match {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	for besti > alo && bestj > blo && m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize++
	}
	return match{besti, bestj, bestsize}
}

func (m *sequenceMatcher) matchingBlocks() []match {
	la, lb := len(m.a), len(m.b)
	queue := [][4]int{{0, la, 0, lb}}
	var blocks []match
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		x := m.findLongestMatch(alo, ahi, blo, bhi)
		if x.size > 0 {
			blocks = append(blocks, x)
			if alo < x.a && blo < x.b {
				queue = append(queue, [4]int{alo, x.a, blo, x.b})
			}
			if x.a+x.size < ahi && x.b+x.size < bhi {
				queue = append(queue, [4]int{x.a + x.size, ahi, x.b + x.size, bhi})
			}
		}
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].a != blocks[j].a {
			return blocks[i].a < blocks[j].a
		}
		if blocks[i].b != blocks[j].b {
			return blocks[i].b < blocks[j].b
		}
		return blocks[i].size < blocks[j].size
	})
	var out []match
	i1, j1, k1 := 0, 0, 0
	for _, x := range blocks {
		if i1+k1 == x.a && j1+k1 == x.b {
			k1 += x.size
		} else {
			if k1 > 0 {
				out = append(out, match{i1, j1, k1})
			}
			i1, j1, k1 = x.a, x.b, x.size
		}
	}
	if k1 > 0 {
		out = append(out, match{i1, j1, k1})
	}
	return append(out, match{la, lb, 0})
}

func (m *sequenceMatcher) opcodes() []opcode {
	i, j := 0, 0
	var out []opcode
	for _, x := range m.matchingBlocks() {
		var tag byte
		switch {
		case i < x.a && j < x.b:
			tag = 'r'
		case i < x.a:
			tag = 'd'
		case j < x.b:
			tag = 'i'
		}
		if tag != 0 {
			out = append(out, opcode{tag, i, x.a, j, x.b})
		}
		i, j = x.a+x.size, x.b+x.size
		if x.size > 0 {
			out = append(out, opcode{'e', x.a, i, x.b, j})
		}
	}
	return out
}

func (m *sequenceMatcher) groupedOpcodes(n int) [][]opcode {
	codes := m.opcodes()
	if len(codes) == 0 {
		codes = []opcode{{'e', 0, 1, 0, 1}}
	}
	if c := &codes[0]; c.tag == 'e' {
		c.i1, c.j1 = max(c.i1, c.i2-n), max(c.j1, c.j2-n)
	}
	if c := &codes[len(codes)-1]; c.tag == 'e' {
		c.i2, c.j2 = min(c.i2, c.i1+n), min(c.j2, c.j1+n)
	}
	nn := n + n
	var groups [][]opcode
	var group []opcode
	for _, c := range codes {
		if c.tag == 'e' && c.i2-c.i1 > nn {
			group = append(group, opcode{'e', c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)})
			groups = append(groups, group)
			group = nil
			c.i1, c.j1 = max(c.i1, c.i2-n), max(c.j1, c.j2-n)
		}
		group = append(group, c)
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == 'e') {
		groups = append(groups, group)
	}
	return groups
}

func formatRangeUnified(start, stop int) string {
	beginning := start + 1
	length := stop - start
	if length == 1 {
		return strconv.Itoa(beginning)
	}
	if length == 0 {
		beginning--
	}
	return strconv.Itoa(beginning) + "," + strconv.Itoa(length)
}

// unifiedDiff is difflib.unified_diff(a, b, fromfile, tofile, '', '', n).
func unifiedDiff(a, b []string, fromfile, tofile string, n int) []string {
	var out []string
	for gi, group := range newSequenceMatcher(a, b).groupedOpcodes(n) {
		if gi == 0 {
			out = append(out, "--- "+fromfile+"\n", "+++ "+tofile+"\n")
		}
		first, last := group[0], group[len(group)-1]
		out = append(out, "@@ -"+formatRangeUnified(first.i1, last.i2)+" +"+formatRangeUnified(first.j1, last.j2)+" @@\n")
		for _, c := range group {
			if c.tag == 'e' {
				for _, line := range a[c.i1:c.i2] {
					out = append(out, " "+line)
				}
				continue
			}
			if c.tag == 'r' || c.tag == 'd' {
				for _, line := range a[c.i1:c.i2] {
					out = append(out, "-"+line)
				}
			}
			if c.tag == 'r' || c.tag == 'i' {
				for _, line := range b[c.j1:c.j2] {
					out = append(out, "+"+line)
				}
			}
		}
	}
	return out
}

// diffTruthy is Python truthiness of a result's diff value.
func diffTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case string:
		return t != ""
	}
	return true
}
