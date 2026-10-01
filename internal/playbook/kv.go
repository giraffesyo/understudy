package playbook

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// parseKV splits Ansible's `key=value key2="v 2"` inline argument form.
// Values keep Ansible's INI-style coercion off — everything stays a string;
// modules coerce their own args.
func parseKV(s string) (map[string]any, error) {
	out := map[string]any{}
	for _, word := range splitWords(s) {
		eq := strings.IndexByte(word, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("expected key=value, got %q", word)
		}
		out[word[:eq]] = unquote(word[eq+1:])
	}
	return out, nil
}

// parseKVRaw is parse_kv: key=value words are arguments, the others are
// joined as the raw params.
func parseKVRaw(s string) (map[string]any, string) {
	out := map[string]any{}
	var raw []string
	for _, orig := range pySplitArgs(s) {
		word := decodeEscapes(orig)
		if pos := kvSplitPos(word); pos > 0 {
			out[strings.TrimSpace(word[:pos])] = unquote(strings.TrimSpace(word[pos+1:]))
			continue
		}
		raw = append(raw, rawParam(orig, word))
	}
	return out, pyJoinArgs(raw)
}

// SplitArgsError is split_args' error for args that end inside quotes or
// a Jinja2 block (an AnsibleParserError).
type SplitArgsError struct{ Args string }

func (e *SplitArgsError) Error() string {
	return "failed at splitting arguments, either an unbalanced jinja2 block or quotes: " + e.Args
}

// ParseKV is parse_kv(args) as ansible-core parses `-e key=value` extra
// vars: the key=value words (keys and values stripped, values unquoted)
// in order, a repeated key keeping its first place, and any other words
// joined as `_raw_params`. Every value is a string.
func ParseKV(s string) (keys []string, vals map[string]string, err error) {
	words, balanced := pySplitArgsChecked(s)
	if !balanced {
		return nil, nil, &SplitArgsError{s}
	}
	vals = map[string]string{}
	set := func(k, v string) {
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	var raw []string
	for _, orig := range words {
		x := decodeEscapes(orig)
		if !strings.Contains(x, "=") {
			raw = append(raw, orig)
			continue
		}
		pos := kvSplitPos(x)
		if pos < 0 {
			raw = append(raw, strings.ReplaceAll(x, `\=`, "="))
			continue
		}
		set(strings.TrimFunc(x[:pos], isPyStrSpace), unquote(strings.TrimFunc(x[pos+1:], isPyStrSpace)))
	}
	if len(raw) > 0 {
		set("_raw_params", pyJoinArgs(raw))
	}
	return keys, vals, nil
}

// isPyStrSpace is str.isspace for one character (what str.strip strips).
func isPyStrSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// isAllTemplate reports whether s starts and ends with template
// delimiters (is_possibly_all_template).
func isAllTemplate(s string) bool {
	return (strings.HasPrefix(s, "{{") || strings.HasPrefix(s, "{%") || strings.HasPrefix(s, "{#")) &&
		(strings.HasSuffix(s, "}}") || strings.HasSuffix(s, "%}") || strings.HasSuffix(s, "#}"))
}

// IsAllTemplate is isAllTemplate for the executor.
func IsAllTemplate(s string) bool { return isAllTemplate(s) }

// splitFreeForm separates trailing k=v pairs (chdir=, creates=, removes=,
// executable=, warn=) from a free-form command line, matching how Ansible
// treats command/shell arguments.
func splitFreeForm(s, module string) (string, map[string]any) {
	if module == "raw" || module == "meta" {
		return s, nil
	}
	// parse_kv(check_raw=True): k=v words whose key is a known option
	// (creates=, chdir=, ...) are options wherever they appear
	// (`iptables -F creates=/etc/x`); everything else (`echo a=b`) is
	// rejoined with its original spacing and newlines (split_args +
	// join_args).
	kv := map[string]any{}
	var raw []string
	for _, orig := range pySplitArgs(s) {
		w := decodeEscapes(orig)
		pos := kvSplitPos(w)
		if pos > 0 && knownFreeFormOption(w[:pos]) {
			kv[strings.TrimSpace(w[:pos])] = unquote(strings.TrimSpace(w[pos+1:]))
			continue
		}
		if pos > 0 {
			raw = append(raw, orig)
		} else {
			raw = append(raw, rawParam(orig, w))
		}
	}
	if len(kv) == 0 {
		kv = nil
	}
	return pyJoinArgs(raw), kv
}

// kvSplitPos is parse_kv's search for the first unescaped '=' after the
// first character (-1 when there is none).
func kvSplitPos(x string) int {
	for pos := 1; pos < len(x); pos++ {
		if x[pos] == '=' && x[pos-1] != '\\' {
			return pos
		}
	}
	return -1
}

// pyJoinArgs is ansible.parsing.splitter.join_args.
func pyJoinArgs(params []string) string {
	var b strings.Builder
	for _, p := range params {
		if b.Len() == 0 || strings.HasSuffix(b.String(), "\n") {
			b.WriteString(p)
		} else {
			b.WriteString(" " + p)
		}
	}
	return b.String()
}

// pySplitArgs is ansible.parsing.splitter.split_args: split on spaces and
// newlines, keeping quoted strings and Jinja2 blocks together, with runs
// of spaces and the newlines carried on the tokens so join_args rebuilds
// the original text.
func pySplitArgs(args string) []string {
	params, _ := pySplitArgsChecked(args)
	return params
}

// pySplitArgsChecked is pySplitArgs that also reports whether the args
// end outside quotes and Jinja2 blocks (split_args raises
// SplitArgsError when they do not).
func pySplitArgsChecked(args string) (params []string, balanced bool) {
	if args == "" {
		return nil, true
	}
	var quoteChar byte
	insideQuotes := false
	depths := []struct {
		n           int
		open, close string
	}{{0, "{{", "}}"}, {0, "{%", "%}"}, {0, "{#", "#}"}}
	inJinja := func() bool { return depths[0].n > 0 || depths[1].n > 0 || depths[2].n > 0 }
	items := strings.Split(args, "\n")
	for itemIdx, item := range items {
		lineContinuation := false
		for idx, token := range strings.Split(item, " ") {
			if token == "" && idx != 0 {
				if len(params) == 0 {
					params = append(params, "")
				}
				params[len(params)-1] += " "
				continue
			}
			if token == "\\" && !insideQuotes {
				lineContinuation = true
				continue
			}
			wasInsideQuotes := insideQuotes
			quoteChar = pyQuoteState(token, quoteChar)
			insideQuotes = quoteChar != 0
			appended := false
			if insideQuotes && !wasInsideQuotes && !inJinja() {
				params = append(params, token)
				appended = true
			} else if inJinja() || insideQuotes || wasInsideQuotes {
				spacer := ""
				if idx > 0 {
					spacer = " "
				}
				params[len(params)-1] += spacer + token
				appended = true
			}
			for i := range depths {
				d := &depths[i]
				prev := d.n
				if o, c := strings.Count(token, d.open), strings.Count(token, d.close); o != c {
					d.n = max(d.n+o-c, 0)
				}
				if d.n != prev && !appended {
					params = append(params, token)
					appended = true
				}
			}
			if !inJinja() && !insideQuotes && !appended && token != "" {
				params = append(params, token)
			}
		}
		if len(items) > 1 && itemIdx != len(items)-1 && !lineContinuation {
			if len(params) == 0 {
				params = append(params, "")
			}
			params[len(params)-1] += "\n"
		}
	}
	return params, !inJinja() && !insideQuotes
}

// pyQuoteState is splitter._get_quote_state.
func pyQuoteState(token string, quoteChar byte) byte {
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c == '"' || c == '\'') && (i == 0 || token[i-1] != '\\') {
			if quoteChar == 0 {
				quoteChar = c
			} else if c == quoteChar {
				quoteChar = 0
			}
		}
	}
	return quoteChar
}

func knownFreeFormOption(key string) bool {
	switch key {
	case "chdir", "creates", "removes", "executable", "warn", "stdin",
		"stdin_add_newline", "strip_empty_ends":
		return true
	}
	return false
}

// splitWords tokenizes on whitespace, honoring single/double quotes and,
// like Ansible's split_args, Jinja2 blocks: whitespace inside {{ }}, {% %}
// or {# #} does not split (`name={{ item }} state=present`).
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	depth := 0 // open Jinja2 delimiters outside quotes
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == 0 && i+1 < len(s) {
			switch pair := s[i : i+2]; pair {
			case "{{", "{%", "{#":
				depth++
				inWord = true
				cur.WriteString(pair)
				i++
				continue
			case "}}", "%}", "#}":
				if depth > 0 {
					depth--
					cur.WriteString(pair)
					i++
					continue
				}
			}
		}
		if depth > 0 && quote == 0 && (c == ' ' || c == '\t' || c == '\n') {
			cur.WriteByte(c)
			continue
		}
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
			cur.WriteByte(c)
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// unquote is parsing.quoting.unquote: one level of surrounding quotes
// stripped, unless the closing one is escaped.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] && s[len(s)-2] != '\\' {
		return s[1 : len(s)-1]
	}
	return s
}

// rawParam is a word parse_kv keeps as a raw parameter: as written, or
// (decoded) with its \= unescaped when every '=' in it is escaped.
func rawParam(orig, decoded string) string {
	if strings.Contains(decoded, "=") {
		return strings.ReplaceAll(decoded, `\=`, "=")
	}
	return orig
}

// decodeEscapes is splitter._decode_escapes: the \U........, \u...., \x..,
// \N{name} and single-character escapes (\\ \' \" \a \b \f \n \r \t \v)
// decoded as unicode-escape decodes them; any other backslash stays.
func decodeEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		rest := s[i+2:]
		switch c := s[i+1]; c {
		case 'U', 'u', 'x':
			n := map[byte]int{'U': 8, 'u': 4, 'x': 2}[c]
			if r, ok := hexRune(rest, n); ok && r <= 0x10FFFF {
				b.WriteRune(r)
				i += 1 + n
				continue
			}
		case 'N':
			if strings.HasPrefix(rest, "{") {
				if end := strings.IndexByte(rest, '}'); end > 1 {
					if r, ok := pyre.LookupName(rest[1:end]); ok {
						b.WriteRune(r)
						i += 2 + end
						continue
					}
				}
			}
		default:
			if out, ok := singleEscapes[c]; ok {
				b.WriteString(out)
				i++
				continue
			}
		}
		b.WriteByte('\\')
	}
	return b.String()
}

var singleEscapes = map[byte]string{'\\': `\`, '\'': "'", '"': `"`, 'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v"}

// hexRune reads n hex digits at the start of t.
func hexRune(t string, n int) (rune, bool) {
	if len(t) < n {
		return 0, false
	}
	var r rune
	for i := 0; i < n; i++ {
		c := t[i]
		switch {
		case c >= '0' && c <= '9':
			r = r*16 + rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r*16 + rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r*16 + rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}
