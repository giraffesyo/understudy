package cli

import (
	"strings"
	"unicode"
)

// pyWrap is Python's textwrap.wrap(text, width) with its defaults
// (break_long_words, break_on_hyphens, drop_whitespace), for text whose
// whitespace argparse already collapsed to single spaces.
func pyWrap(text string, width int) []string {
	return pyWrapIndent(text, width, "", "")
}

// pyFill is textwrap.fill(text, width, initial_indent=indent,
// subsequent_indent=indent).
func pyFill(text string, width int, indent string) string {
	return strings.Join(pyWrapIndent(text, width, indent, indent), "\n")
}

func pyWrapIndent(text string, width int, initial, subsequent string) []string {
	chunks := wrapChunks([]rune(text))
	// TextWrapper._wrap_chunks, chunks consumed from the end.
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	var lines []string
	for len(chunks) > 0 {
		var cur []string
		curLen := 0
		indent := initial
		if len(lines) > 0 {
			indent = subsequent
		}
		w := width - len([]rune(indent))
		if strings.TrimSpace(chunks[len(chunks)-1]) == "" && len(lines) > 0 {
			chunks = chunks[:len(chunks)-1]
		}
		for len(chunks) > 0 {
			l := len([]rune(chunks[len(chunks)-1]))
			if curLen+l > w {
				break
			}
			cur = append(cur, chunks[len(chunks)-1])
			chunks = chunks[:len(chunks)-1]
			curLen += l
		}
		if len(chunks) > 0 && len([]rune(chunks[len(chunks)-1])) > w {
			// _handle_long_word: break it, after its last hyphen that fits
			// (one with something other than hyphens before it).
			spaceLeft := w - curLen
			if w < 1 {
				spaceLeft = 1
			}
			chunk := []rune(chunks[len(chunks)-1])
			end := spaceLeft
			if spaceLeft > 0 && len(chunk) > spaceLeft {
				if h := lastIndexRune(chunk[:spaceLeft], '-'); h > 0 && strings.Trim(string(chunk[:h]), "-") != "" {
					end = h + 1
				}
			}
			if spaceLeft > 0 {
				cur = append(cur, string(chunk[:end]))
				chunks[len(chunks)-1] = string(chunk[end:])
			}
		}
		if len(cur) > 0 && strings.TrimSpace(cur[len(cur)-1]) == "" {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, indent+strings.Join(cur, ""))
		}
	}
	return lines
}

func lastIndexRune(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// wrapChunks is TextWrapper._split with break_on_hyphens: runs of
// whitespace, and words split after the hyphens of hyphenated words
// (wordsep_re).
func wrapChunks(t []rune) []string {
	isWS := func(i int) bool { return i >= 0 && i < len(t) && unicode.IsSpace(t[i]) }
	isWord := func(i int) bool {
		return i >= 0 && i < len(t) && (t[i] == '_' || unicode.IsLetter(t[i]) || unicode.IsDigit(t[i]) || unicode.Is(unicode.Mn, t[i]))
	}
	isLetter := func(i int) bool { return isWord(i) && !unicode.IsDigit(t[i]) }
	isWP := func(i int) bool { return isWord(i) || i >= 0 && i < len(t) && strings.ContainsRune(`!"'&.,?`, t[i]) }
	// dashesThenWord reports whether t[i:] is two or more hyphens and a
	// word character, returning where the hyphens end.
	dashesThenWord := func(i int) (int, bool) {
		j := i
		for j < len(t) && t[j] == '-' {
			j++
		}
		return j, j-i >= 2 && isWord(j)
	}
	var out []string
	for i := 0; i < len(t); {
		if isWS(i) {
			j := i
			for j < len(t) && isWS(j) {
				j++
			}
			out = append(out, string(t[i:j]))
			i = j
			continue
		}
		if isWP(i - 1) {
			if j, ok := dashesThenWord(i); ok {
				out = append(out, string(t[i:j])) // an em-dash between words
				i = j
				continue
			}
		}
		end := -1
		for e := i + 1; e <= len(t) && end < 0; e++ {
			// A hyphenated word: the hyphen at e, after two letters or a
			// letter-hyphen-letter, before a letter, an optional hyphen
			// and a letter.
			if e < len(t) && t[e] == '-' {
				behind := isLetter(e-2) && isLetter(e-1) || isLetter(e-3) && e-2 >= 0 && t[e-2] == '-' && isLetter(e-1)
				ahead := isLetter(e+1) && (isLetter(e+2) || e+2 < len(t) && t[e+2] == '-' && isLetter(e+3))
				if behind && ahead {
					end = e + 1
					break
				}
			}
			if e == len(t) || isWS(e) {
				end = e
				break
			}
			if isWP(e - 1) {
				if _, ok := dashesThenWord(e); ok {
					end = e
					break
				}
			}
		}
		if end < 0 {
			end = len(t)
		}
		out = append(out, string(t[i:end]))
		i = end
	}
	return out
}
