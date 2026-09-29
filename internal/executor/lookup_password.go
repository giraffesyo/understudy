package executor

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// passwordMu serializes the password lookup's read-or-generate step: forks
// evaluate the same group_var concurrently, and every host must end up with
// the one password that lands in the file (Ansible uses a lock file).
var passwordMu sync.Mutex

// Python's string-module constants, which the chars= option names.
var pyStringConsts = map[string]string{
	"ascii_letters":   "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"ascii_lowercase": "abcdefghijklmnopqrstuvwxyz",
	"ascii_uppercase": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"digits":          "0123456789",
	"hexdigits":       "0123456789abcdefABCDEF",
	"octdigits":       "01234567",
	"punctuation":     "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~",
	"letters":         "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"lowercase":       "abcdefghijklmnopqrstuvwxyz",
	"uppercase":       "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
}

type passwordParams struct {
	path   string
	length int
	chars  []string
}

// The password lookup (lookupPassword in lookups.go) implements
// ansible.builtin.password: the first use generates a random password into
// the file on the control node, later uses read it back. /dev/null means
// "generate, never store".

// parsePasswordTerm splits "path key=value ..." (options may also arrive
// as lookup kwargs) and applies Ansible's defaults.
func parsePasswordTerm(term string, kwargs map[string]any) (passwordParams, error) {
	p := passwordParams{length: 20, chars: []string{"ascii_letters", "digits", ".,:-_"}}
	fields := strings.Fields(term)
	if len(fields) == 0 {
		return p, fmt.Errorf("password lookup: a path is required")
	}
	p.path = fields[0]
	opts := map[string]string{}
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return p, fmt.Errorf("password lookup: invalid option %q (expected key=value)", f)
		}
		opts[k] = v
	}
	for k, v := range kwargs {
		opts[k] = fmt.Sprintf("%v", v)
	}
	var unknown []string
	for k, v := range opts {
		switch k {
		case "length":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return p, fmt.Errorf("password lookup: invalid length %q", v)
			}
			p.length = n
		case "chars":
			p.chars = splitPasswordChars(v)
		case "encrypt", "ident", "seed":
			return p, fmt.Errorf("password lookup: the %q option is not supported yet", k)
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return p, fmt.Errorf("Unrecognized parameter(s) given to password lookup: %s", strings.Join(unknown, ", "))
	}
	return p, nil
}

// splitPasswordChars parses chars=: comma-separated, with ",," meaning a
// literal comma, exactly as the Ansible plugin does.
func splitPasswordChars(v string) []string {
	var out []string
	if strings.Contains(v, ",,") {
		out = append(out, ",")
	}
	for _, c := range strings.Split(strings.ReplaceAll(v, ",,", ","), ",") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// candidateChars expands named character sets and drops quotes. Duplicates
// are kept, matching Ansible's (slightly biased) candidate string.
func candidateChars(sets []string) string {
	var b strings.Builder
	for _, c := range sets {
		if s, ok := pyStringConsts[c]; ok {
			b.WriteString(s)
		} else {
			b.WriteString(c)
		}
	}
	return strings.NewReplacer(`"`, "", "'", "").Replace(b.String())
}

func randomPassword(length int, chars string) (string, error) {
	if chars == "" {
		return "", fmt.Errorf("password lookup: chars yields no candidate characters")
	}
	pool := []rune(chars)
	out := make([]rune, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
		if err != nil {
			return "", err
		}
		out[i] = pool[n.Int64()]
	}
	return string(out), nil
}

func (r *Runner) readOrCreatePassword(p passwordParams) (string, error) {
	if p.path == "/dev/null" {
		return randomPassword(p.length, candidateChars(p.chars))
	}
	path := r.resolveLookupPath(p.path)

	passwordMu.Lock()
	defer passwordMu.Unlock()

	if data, err := os.ReadFile(path); err == nil {
		content := strings.TrimRight(string(data), "\n")
		// Stored as "password[ salt=...][ ident=...]"; only the password
		// is returned when encrypt is unset.
		if i := strings.LastIndex(content, " salt="); i >= 0 {
			content = content[:i]
		}
		return content, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("password lookup: %v", err)
	}

	pw, err := randomPassword(p.length, candidateChars(p.chars))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("password lookup: %v", err)
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("password lookup: %v", err)
	}
	return pw, nil
}
