package playbook

import (
	"fmt"
	"strings"
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

// splitFreeForm separates trailing k=v pairs (chdir=, creates=, removes=,
// executable=, warn=) from a free-form command line, matching how Ansible
// treats command/shell arguments.
func splitFreeForm(s, module string) (string, map[string]any) {
	if module == "raw" || module == "meta" {
		return s, nil
	}
	words := splitWords(s)
	kv := map[string]any{}
	// Like parse_kv(check_raw=True): k=v words whose key is a known option
	// (creates=, chdir=, ...) are options wherever they appear
	// (`iptables -F creates=/etc/x`); anything else (`echo a=b`) stays in
	// the command.
	var rest []string
	for _, w := range words {
		eq := strings.IndexByte(w, '=')
		if eq > 0 && knownFreeFormOption(w[:eq]) {
			kv[w[:eq]] = unquote(w[eq+1:])
			continue
		}
		rest = append(rest, w)
	}
	if len(kv) == 0 {
		return strings.TrimSpace(s), nil // preserve original spacing
	}
	return strings.Join(rest, " "), kv
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

// unquote strips one level of surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
