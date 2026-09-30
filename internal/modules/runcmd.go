package modules

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
)

// cmdOpts are the run_command keyword arguments the ported modules use.
type cmdOpts struct {
	Cwd      string
	Env      map[string]string // environ_update
	Stdin    io.Reader
	Data     string // run_command(data=...): stdin text, newline added
	UseShell bool
}

// runCommand is AnsibleModule.run_command for an argv list: (rc, stdout,
// stderr). A spawn failure reports rc 257 like ansible's OSError branch,
// with the error text on stderr.
func runCommand(env *RunEnv, argv []string, o cmdOpts) (int, string, string) {
	if len(argv) == 0 {
		return 257, "", "no command given"
	}
	path := argv[0]
	if !strings.Contains(path, "/") {
		if p, err := lookPath(path); err == nil {
			path = p
		}
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = o.Cwd
	if len(env.Env) > 0 || len(o.Env) > 0 {
		cmd.Env = env.Environ(o.Env)
	}
	switch {
	case o.Stdin != nil:
		cmd.Stdin = o.Stdin
	case o.Data != "":
		d := o.Data
		if !strings.HasSuffix(d, "\n") {
			d += "\n"
		}
		cmd.Stdin = strings.NewReader(d)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), stdout.String(), stderr.String()
		}
		return 257, stdout.String(), err.Error()
	}
	return 0, stdout.String(), stderr.String()
}

// bestParsableLocale is get_best_parsable_locale: the first preferred
// locale `locale -a` lists, else C.
func bestParsableLocale(env *RunEnv) string {
	if _, err := lookPath("locale"); err != nil {
		return "C"
	}
	rc, out, _ := runCommand(env, []string{"locale", "-a"}, cmdOpts{})
	if rc != 0 || strings.TrimSpace(out) == "" {
		return "C"
	}
	avail := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		avail[strings.TrimSpace(l)] = true
	}
	for _, pref := range []string{"C.utf8", "C.UTF-8", "en_US.utf8", "en_US.UTF-8", "C", "POSIX"} {
		if avail[pref] {
			return pref
		}
	}
	return "C"
}

func localeEnv(loc string) map[string]string {
	return map[string]string{"LANG": loc, "LC_ALL": loc, "LC_MESSAGES": loc, "LANGUAGE": loc}
}

// strList converts a parsed list argument to strings.
func strList(items []any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch t := it.(type) {
		case string:
			out = append(out, t)
		case nil:
		default:
			s, _ := argString(map[string]any{"v": t}, "v")
			out = append(out, s)
		}
	}
	return out
}

func anyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
