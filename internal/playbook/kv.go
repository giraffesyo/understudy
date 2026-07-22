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
	// Only LEADING k=v words with known option names are options (Ansible
	// parses them anywhere, but trailing ones like `echo a=b` must stay in
	// the command; leading is the documented style and unambiguous).
	i := 0
	for ; i < len(words); i++ {
		eq := strings.IndexByte(words[i], '=')
		if eq <= 0 || !knownFreeFormOption(words[i][:eq]) {
			break
		}
		kv[words[i][:eq]] = unquote(words[i][eq+1:])
	}
	rest := strings.Join(words[i:], " ")
	if i == 0 {
		rest = strings.TrimSpace(s) // preserve original spacing when no options
	}
	if len(kv) == 0 {
		return rest, nil
	}
	return rest, kv
}

func knownFreeFormOption(key string) bool {
	switch key {
	case "chdir", "creates", "removes", "executable", "warn", "stdin",
		"stdin_add_newline", "strip_empty_ends":
		return true
	}
	return false
}

// splitWords tokenizes on whitespace, honoring single/double quotes.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
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
