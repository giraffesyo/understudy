//go:build golden

package e2e

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestGoldenCLI runs ansible-playbook and ansible command lines through
// both tools (understudy through its ansible-playbook and ansible
// symlinks) and compares stdout, stderr and the exit code byte for byte:
// argparse's usage, errors and help, the configuration file's discovery
// and errors, and settings only a command line or ansible.cfg reach.
//
// Each golden/cli/<name>.args holds the command (ansible-playbook or
// ansible) and then its arguments, one per line. The case runs in a fresh
// working directory holding a copy of golden/cli/<name>.files (if any);
// @WORK@ in arguments and environment stands for that directory, and an
// argument @EMPTY@ for an empty one.
// <name>.env adds environment variables (KEY=VALUE lines; "!KEY" unsets
// one), and <name>.chmod sets modes in the working directory ("<path>
// <octal mode>" lines; "." is the directory itself).
//
// A configuration error ends ansible with its Python traceback after the
// message; that part cannot be reproduced and is not compared.
func TestGoldenCLI(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	cases, _ := filepath.Glob("golden/cli/*.args")
	if len(cases) == 0 {
		t.Fatal("no CLI corpus found")
	}
	base := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_SYSTEM_WARNINGS=False", "ANSIBLE_RETRY_FILES_ENABLED=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		base = append(base, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	for _, argsFile := range cases {
		name := strings.TrimSuffix(filepath.Base(argsFile), ".args")
		t.Run(name, func(t *testing.T) {
			stem := strings.TrimSuffix(argsFile, ".args")
			lines := readLines(t, argsFile)
			if len(lines) == 0 {
				t.Fatal("empty args file")
			}
			tool, args := lines[0], lines[1:]
			envLines := readLines(t, stem+".env")
			chmods := readLines(t, stem+".chmod")
			run := func(bin string) (string, string, int) {
				work := t.TempDir()
				if err := copyTree(stem+".files", work); err != nil {
					t.Fatal(err)
				}
				real, _ := filepath.EvalSymlinks(work)
				sub := func(s string) string { return strings.ReplaceAll(s, "@WORK@", work) }
				env := cleanEnv(base)
				for _, l := range envLines {
					if k, ok := strings.CutPrefix(l, "!"); ok {
						env = withoutEnv(env, k)
						continue
					}
					k, _, _ := strings.Cut(l, "=")
					env = append(withoutEnv(env, k), sub(l))
				}
				var argv []string
				for _, a := range args {
					argv = append(argv, strings.ReplaceAll(sub(a), "@EMPTY@", ""))
				}
				for _, l := range chmods {
					path, mode, _ := strings.Cut(l, " ")
					m, err := strconv.ParseUint(mode, 8, 32)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Join(work, path), fs.FileMode(m)); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command(bin, argv...)
				cmd.Dir = work
				cmd.Env = env
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				cmd.Run()
				// A world writable directory must not outlive the test.
				os.Chmod(work, 0o700)
				norm := func(s string) string {
					if i := strings.Index(s, "Traceback (most recent call last):"); i >= 0 {
						s = s[:i]
					}
					s = strings.ReplaceAll(s, real, "WORK")
					s = strings.ReplaceAll(s, work, "WORK")
					return versionBannerAnyRe.ReplaceAllString(s, "VERSION\n")
				}
				return norm(stdout.String()), norm(stderr.String()), cmd.ProcessState.ExitCode()
			}
			aOut, aErr, aRC := run(filepath.Join(filepath.Dir(ansible), tool))
			uOut, uErr, uRC := run(filepath.Join(filepath.Dir(understudy), tool))
			if uRC != aRC {
				t.Errorf("exit code %d, %s %d", uRC, tool, aRC)
			}
			if uOut != aOut {
				t.Errorf("stdout differs from %s:\n%s", tool, lineDiff(aOut, uOut))
			}
			if uErr != aErr {
				t.Errorf("stderr differs from %s:\n%s", tool, lineDiff(aErr, uErr))
			}
		})
	}
}

// readLines reads a case file's lines ("" lines and a missing file: none).
func readLines(t *testing.T, path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// cleanEnv is the environment less what would make the comparison
// depend on the caller's (a configuration, the terminal width, colors).
func cleanEnv(extra []string) []string {
	env := os.Environ()
	for _, k := range []string{"ANSIBLE_CONFIG", "COLUMNS", "FORCE_COLOR", "PYTHON_COLORS", "ANSIBLE_FORCE_COLOR"} {
		env = withoutEnv(env, k)
	}
	return append(env, extra...)
}

func withoutEnv(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// copyTree copies a directory's contents into dst (a missing src: none).
func copyTree(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// versionBannerAnyRe is --version's and -vv's banner of either command.
var versionBannerAnyRe = regexp.MustCompile(`(?m)^(?:ansible-playbook|ansible) \[.*\]\n(?:  .*\n)*`)
