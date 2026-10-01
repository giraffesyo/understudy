package pyre

import (
	"strings"
)

// FnmatchTranslate is Python 3.14's fnmatch.translate: a shell pattern
// as a regular expression (*, ?, [seq], [!seq]), anchored at the end;
// match it from the start.
func FnmatchTranslate(pat string) string {
	parts, stars := fnmatchParts([]rune(pat))
	if len(stars) == 0 {
		return `(?s:` + strings.Join(parts, "") + `)\z`
	}
	j := stars[0]
	buf := append([]string(nil), parts[:j]...)
	i := j + 1
	for _, j := range stars[1:] {
		// STAR fixed: a minimal .*? and the fixed part, without
		// backtracking (an atomic group).
		buf = append(buf, `(?>.*?`)
		buf = append(buf, parts[i:j]...)
		buf = append(buf, `)`)
		i = j + 1
	}
	buf = append(buf, `.*`)
	buf = append(buf, parts[i:]...)
	return `(?s:` + strings.Join(buf, "") + `)\z`
}

// fnmatchParts is fnmatch._translate(pat, '*', '.'): the pattern's
// pieces and the indices of its stars.
func fnmatchParts(pat []rune) ([]string, []int) {
	var res []string
	var stars []int
	n := len(pat)
	i := 0
	for i < n {
		c := pat[i]
		i++
		switch c {
		case '*':
			stars = append(stars, len(res))
			res = append(res, "*")
			for i < n && pat[i] == '*' {
				i++
			}
		case '?':
			res = append(res, ".")
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				res = append(res, `\[`)
				continue
			}
			var stuff string
			if !strings.ContainsRune(string(pat[i:j]), '-') {
				stuff = strings.ReplaceAll(string(pat[i:j]), `\`, `\\`)
			} else {
				var chunks []string
				k := i + 1
				if pat[i] == '!' {
					k = i + 2
				}
				for {
					k = indexRune(pat, '-', k, j)
					if k < 0 {
						break
					}
					chunks = append(chunks, string(pat[i:k]))
					i = k + 1
					k = k + 3
				}
				if chunk := string(pat[i:j]); chunk != "" {
					chunks = append(chunks, chunk)
				} else {
					chunks[len(chunks)-1] += "-"
				}
				// Remove empty ranges -- invalid in RE.
				for k := len(chunks) - 1; k > 0; k-- {
					prev, cur := []rune(chunks[k-1]), []rune(chunks[k])
					if prev[len(prev)-1] > cur[0] {
						chunks[k-1] = string(prev[:len(prev)-1]) + string(cur[1:])
						chunks = append(chunks[:k], chunks[k+1:]...)
					}
				}
				for k, s := range chunks {
					chunks[k] = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "-", `\-`)
				}
				stuff = strings.Join(chunks, "-")
			}
			i = j + 1
			switch {
			case stuff == "":
				res = append(res, "(?!)") // empty range: never matches
			case stuff == "!":
				res = append(res, ".") // negated empty range: any character
			default:
				// Escape set operations (&&, ~~ and ||).
				var b strings.Builder
				for _, r := range stuff {
					if r == '&' || r == '~' || r == '|' {
						b.WriteByte('\\')
					}
					b.WriteRune(r)
				}
				stuff = b.String()
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' || stuff[0] == '[' {
					stuff = `\` + stuff
				}
				res = append(res, "["+stuff+"]")
			}
		default:
			res = append(res, Escape(string(c)))
		}
	}
	return res, stars
}

// indexRune is str.find(r, start, end) over runes.
func indexRune(s []rune, r rune, start, end int) int {
	for k := start; k < end && k < len(s); k++ {
		if s[k] == r {
			return k
		}
	}
	return -1
}

// CompileFnmatch compiles a shell pattern as fnmatch does (re.compile of
// its translation, cached).
func CompileFnmatch(pat string) (*Pattern, error) {
	return Compile(FnmatchTranslate(pat), 0)
}

// Fnmatch is fnmatch.fnmatchcase(name, pat) (fnmatch.fnmatch on POSIX).
func Fnmatch(name, pat string) bool {
	p, err := CompileFnmatch(pat)
	if err != nil {
		return false
	}
	return p.Match(name, 0, -1) != nil
}
