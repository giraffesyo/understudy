//go:build golden

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestGoldenOutput holds understudy to byte-for-byte parity with
// ansible-playbook's default stdout callback over the whole golden corpus
// (forks=1 so multi-host result order is deterministic). Only values that
// can never match are normalized: per-run work directories and command
// timings.
func TestGoldenOutput(t *testing.T) { testGoldenOutput(t) }

// TestGoldenOutputVerbose is TestGoldenOutput at -v, where every task
// result is printed, including values the corpus keeps out of its debug
// output because they differ on every run (see normalizeVerbose).
func TestGoldenOutputVerbose(t *testing.T) { testGoldenOutput(t, "-v") }

// TestGoldenOutputVV is TestGoldenOutput at -vv: the version banner, the
// PLAYBOOK banner and play count, task paths, handler notification and
// META lines, and skipped stdout callbacks (see normalizeVerbose).
func TestGoldenOutputVV(t *testing.T) { testGoldenOutput(t, "-vv") }

// TestGoldenOutputVVV is TestGoldenOutput at -vvv, where results dump
// indented and the local connection announces itself, less the lines that
// trace ansible-core's own machinery rather than the run (see
// vvvUnmodeled).
func TestGoldenOutputVVV(t *testing.T) { testGoldenOutput(t, "-vvv") }

func testGoldenOutput(t *testing.T, flags ...string) {
	verbose := len(flags) > 0
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)

	corpus, err := filepath.Glob("golden/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no golden corpus found: %v", err)
	}
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_LOCALHOST_WARNING=False", "ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_SYSTEM_WARNINGS=False",
	}
	env = append(env, goldenPackagingEnv(ansible)...)
	// An explicit interpreter skips discovery, whose discovered_interpreter
	// facts are a Python-only artifact.
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}

	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			if reason, ok := vvvSkip[filepath.Base(pb)]; ok && slices.Contains(flags, "-vvv") {
				t.Skip(reason)
			}
			abs, _ := filepath.Abs(pb)
			base := filepath.Base(pb)
			var extra []string
			if strings.HasPrefix(base, "check_") {
				extra = []string{"--check"}
			}
			if strings.HasPrefix(base, "tags_") {
				extra = []string{"--tags", "run_me,also"}
			}
			invSrc := strings.TrimSuffix(pb, ".yml") + ".inventory"

			run := func(bin string, pre ...string) string {
				work := t.TempDir()
				inv := writeGoldenInventory(t, work, invSrc)
				args := append(append(append(pre, flags...), "-f", "1", "-i", inv, "-c", "local", "-e", "workdir="+work), extra...)
				cmd := exec.Command(bin, append(args, abs)...)
				cmd.Env = append(os.Environ(), env...)
				cmd.Stdin = nil
				out, _ := cmd.Output() // stdout only; stderr carries warnings
				s := normalizeOutput(string(out), work)
				if verbose {
					s = normalizeVerbose(s)
				}
				if slices.Contains(flags, "-vvv") {
					s = vvvUnmodeled.ReplaceAllString(s, "")
				}
				return s
			}
			want := run(ansible)
			got := run(understudy, "playbook")
			if got != want {
				t.Errorf("stdout differs from ansible-playbook:\n%s", lineDiff(want, got))
			}
		})
	}
}

var (
	timingRe = regexp.MustCompile(`"(delta|start|end)": "[^"]*"`)
	// The HTTP fixture's (golden/files/http_fixture.py) ephemeral ports.
	loopbackPortRe = regexp.MustCompile(`//([^/@\s]*@)?(127\.0\.0\.1|localhost):[0-9]{4,5}\b`)
	// Async job ids: j<random>.<pid>.
	asyncJIDRe = regexp.MustCompile(`\bj[0-9]+\.[0-9]+\b`)
	// A module's temp files: <remote_tmp>/ansible-tmp-<time>-<pid>-<random>/
	// and tempfile names in it (get_url's download), which failure
	// messages show at any verbosity.
	stagingDirRe  = regexp.MustCompile(`/ansible-tmp-[0-9.]+-[0-9]+-[0-9]+/`)
	stagingFileRe = regexp.MustCompile(`/ansible-tmp-X/tmp[a-z0-9_]{8}`)
)

func normalizeOutput(s, work string) string {
	s = strings.ReplaceAll(s, work, "WORK")
	s = loopbackPortRe.ReplaceAllString(s, "//$1$2:PORT")
	s = asyncJIDRe.ReplaceAllString(s, "JID")
	s = stagingDirRe.ReplaceAllString(s, "/ansible-tmp-X/")
	s = stagingFileRe.ReplaceAllString(s, "/ansible-tmp-X/tmpX")
	return timingRe.ReplaceAllString(s, `"$1": "T"`)
}

// versionBannerRe is -vv's version banner, which describes the
// installation (ansible-core's Python, module paths; understudy's build)
// rather than the run.
var versionBannerRe = regexp.MustCompile(`(?m)^ansible-playbook \[.*\]\n(?:  .*\n)*`)

// Values only -v shows that differ on every run. Each pattern masks just
// the per-run part, so the surrounding shape (and number formatting) is
// still compared.
var verboseMasks = []struct {
	re   *regexp.Regexp
	repl string
}{
	{versionBannerRe, "VERSION\n"},
	// A transfer's staging dir: <remote_tmp>/ansible-tmp-<time>-<pid>-<random>/.
	{regexp.MustCompile(`/ansible-tmp-[0-9.]+-[0-9]+-[0-9]+/`), "/ansible-tmp-X/"},
	// The controller's per-run temp dir, ~/.ansible/tmp/ansible-local-<pid><random>,
	// and the tempfile-named content/template renders inside it.
	{regexp.MustCompile(`/ansible-local-[0-9]+[a-z0-9_]{8}/(\.|tmp)[a-z0-9_]{8}`), "/ansible-local-X/${1}X"},
	// Backup files: <dest>.<pid>.<timestamp>~.
	{regexp.MustCompile(`\.[0-9]+\.[0-9]{4}-[0-9]{2}-[0-9]{2}@[0-9]{2}:[0-9]{2}:[0-9]{2}~"`), `.PID.TIME~"`},
	// tempfile's random names (the corpus uses the default and "work_" prefixes).
	{regexp.MustCompile(`/(ansible\.|work_)[a-z0-9_]{8}`), "/${1}X"},
	// cron's backup_file: tempfile.mkstemp(prefix='crontab').
	{regexp.MustCompile(`/crontab[a-z0-9_]{8}"`), `/crontabX"`},
	// stat/find timestamps (positional Python floats) and inode numbers.
	{regexp.MustCompile(`"(atime|mtime|ctime|birthtime)": [0-9]+\.[0-9]+([,}])`), `"$1": T$2`},
	{regexp.MustCompile(`"inode": [0-9]+`), `"inode": N`},
}

// vvvUnmodeled are -vvv lines that trace how ansible-core executes rather
// than what the run does, which understudy does not reproduce: the shell
// commands and file transfers that build and run AnsiballZ Python payloads
// (EXEC/PUT, "Using module file") and the variable manager re-reading
// vars_files on each variable lookup.
var vvvUnmodeled = regexp.MustCompile("(?m)^(?:<[^>\n]*> (?:EXEC|PUT) .*|Using module file .*|Read `vars_file` .*)\n")

// vvvSkip are corpus cases whose ansible-playbook run itself changes at
// -vvv (the default and -v/-vv harnesses still cover them).
var vvvSkip = map[string]string{
	// A one-second task timeout races the extra connection work -vvv adds.
	"task_timeout.yml": "timing-sensitive under -vvv",
	// At -vvv the module's unknown-state failure surfaces as a result
	// deserialization error instead of its message.
	"results_wait_for.yml": "ansible-core's result changes at -vvv",
}

func normalizeVerbose(s string) string {
	for _, m := range verboseMasks {
		s = m.re.ReplaceAllString(s, m.repl)
	}
	return s
}

// lineDiff shows the first differing lines with a little context.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		b.WriteString("line " + itoa(i+1) + ":\n  ansible:    " + wl + "\n  understudy: " + gl + "\n")
		if shown++; shown == 8 {
			b.WriteString("  ...\n")
			break
		}
	}
	return b.String()
}
