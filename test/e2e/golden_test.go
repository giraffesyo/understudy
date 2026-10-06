//go:build golden

// Golden differential tests: run the SAME playbook through real
// ansible-playbook and through understudy, then assert they make the same
// decisions — identical per-task status (ok/changed/skipping/failed) and
// identical PLAY RECAP counts per host.
//
// This is the strongest evidence that understudy is a faithful drop-in: it
// compares behavior, not byte-for-byte formatting. Requires ansible-playbook
// on PATH (override with UNDERSTUDY_ANSIBLE_PLAYBOOK) and a built
// bin/understudy. Run with: go test -tags golden ./test/e2e/
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func ansiblePlaybookBin(t *testing.T) string {
	if p := os.Getenv("UNDERSTUDY_ANSIBLE_PLAYBOOK"); p != "" {
		return p
	}
	p, err := exec.LookPath("ansible-playbook")
	if err != nil {
		missingPrereq(t, "ansible-playbook not on PATH (set UNDERSTUDY_ANSIBLE_PLAYBOOK)")
	}
	return p
}

// goldenPackagingEnv names, as GOLDEN_PACKAGING_PYTHON, a Python with
// the `packaging` library the pip module needs: the one running
// ansible-playbook (its script's shebang).
func goldenPackagingEnv(ansible string) []string {
	data, err := os.ReadFile(ansible)
	if err != nil || !strings.HasPrefix(string(data), "#!") {
		return nil
	}
	line, _, _ := strings.Cut(string(data[2:]), "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	py := fields[0]
	if filepath.Base(py) == "env" && len(fields) > 1 {
		if p, err := exec.LookPath(fields[1]); err == nil {
			py = p
		}
	}
	return []string{"GOLDEN_PACKAGING_PYTHON=" + py}
}

func understudyBin(t *testing.T) string {
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		missingPrereq(t, "bin/understudy not built (run: make build)")
	}
	return bin
}

// taskStatus is one (task, host) -> status observation.
type taskStatus struct {
	task, host, status string
}

// recap is one host's PLAY RECAP counts.
type recap struct {
	ok, changed, unreachable, failed, skipped, rescued, ignored int
}

var (
	taskBanner = regexp.MustCompile(`^(?:TASK|RUNNING HANDLER) \[(.+?)\]`)
	// "ok: [host]", "changed: [host] => (item=x)", "fatal: [host]: FAILED! => ..."
	statusLine = regexp.MustCompile(`^(ok|changed|skipping|fatal|ignoring): \[([^\]]+)\]`)
	recapLine  = regexp.MustCompile(`^(\S+)\s+:\s+ok=(\d+)\s+changed=(\d+)\s+unreachable=(\d+)\s+failed=(\d+)\s+skipped=(\d+)\s+rescued=(\d+)\s+ignored=(\d+)`)
)

// parseRun extracts the behavioral signal from a tool's output: the set of
// (task, host, status) observations and the per-host recap.
func parseRun(output string) (map[taskStatus]int, map[string]recap) {
	statuses := map[taskStatus]int{}
	recaps := map[string]recap{}
	currentTask := ""
	inRecap := false

	for _, raw := range strings.Split(output, "\n") {
		line := stripANSI(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(line, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if m := taskBanner.FindStringSubmatch(line); m != nil {
			currentTask = m[1]
			continue
		}
		if inRecap {
			if m := recapLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				recaps[m[1]] = recap{
					ok: atoi(m[2]), changed: atoi(m[3]), unreachable: atoi(m[4]),
					failed: atoi(m[5]), skipped: atoi(m[6]), rescued: atoi(m[7]), ignored: atoi(m[8]),
				}
			}
			continue
		}
		if m := statusLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			status := m[1]
			if status == "fatal" {
				status = "failed"
			}
			// Count occurrences so loops (multiple items) compare too.
			statuses[taskStatus{task: currentTask, host: m[2], status: status}]++
		}
	}
	return statuses, recaps
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// toolRun is one tool's run of a playbook: its combined output and exit
// code.
type toolRun struct {
	out string
	rc  int
}

// runTool runs a playbook (twice, returning the second run) so the
// comparison is at steady state — idempotence differences surface as recap
// mismatches.
func runTool(t *testing.T, bin string, args, env []string, runs int) toolRun {
	t.Helper()
	var run toolRun
	for i := 0; i < runs; i++ {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		run = toolRun{out: string(out), rc: cmd.ProcessState.ExitCode()}
		if err != nil && cmd.ProcessState == nil {
			t.Fatalf("running %s: %v", bin, err)
		}
	}
	return run
}

// taskOrder retains every banner and each host's order, independent of host scheduling.
func taskOrder(output string) ([]string, map[string][]string) {
	var names []string
	byHost := map[string][]string{}
	current := ""
	for _, raw := range strings.Split(output, "\n") {
		line := stripANSI(strings.TrimRight(raw, "\r"))
		if m := taskBanner.FindStringSubmatch(line); m != nil {
			current = m[1]
			names = append(names, current)
		}
		if m := statusLine.FindStringSubmatch(line); m != nil {
			byHost[m[2]] = append(byHost[m[2]], current)
		}
	}
	sort.Strings(names)
	return names, byHost
}

// errorLines are the "[ERROR]: ..." headlines of a run (the first line of
// each error block), with the given work directories normalized.
func errorLines(output string, work ...string) []string {
	var lines []string
	for _, raw := range strings.Split(output, "\n") {
		line := stripANSI(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(line, "[ERROR]: ") || strings.HasPrefix(line, "ERROR! ") {
			for _, w := range work {
				line = strings.ReplaceAll(line, w, "WORK")
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// compareRuns asserts that understudy made the same decisions as
// ansible-playbook: exit code, task counts and order per host, per-task status
// and the recap. A playbook either tool failed to run (no task results: a
// parse or load error) fails the comparison, after checking that the two
// errors read the same, so an error in both tools never passes as a match.
func compareRuns(t *testing.T, a, u toolRun, work ...string) {
	t.Helper()
	dump := "\n\n--- ansible ---\n" + a.out + "\n--- understudy ---\n" + u.out
	aStatus, aRecap := parseRun(a.out)
	uStatus, uRecap := parseRun(u.out)
	if len(aStatus) == 0 || len(uStatus) == 0 {
		if ae, ue := errorLines(a.out, work...), errorLines(u.out, work...); !reflect.DeepEqual(ae, ue) {
			t.Errorf("error messages differ\n ansible:    %q\n understudy: %q", ae, ue)
		}
		t.Fatalf("no task results (ansible: %d, exit %d; understudy: %d, exit %d): the playbook did not run%s",
			len(aStatus), a.rc, len(uStatus), u.rc, dump)
	}
	if a.rc != u.rc {
		t.Errorf("exit code differs: ansible %d, understudy %d%s", a.rc, u.rc, dump)
	}
	ab, ah := taskOrder(a.out)
	ub, uh := taskOrder(u.out)
	if !reflect.DeepEqual(ab, ub) {
		t.Errorf("task banners differ\n ansible:    %q\n understudy: %q%s", ab, ub, dump)
	}
	if !reflect.DeepEqual(ah, uh) {
		t.Errorf("per-host task sequence differs\n ansible:    %q\n understudy: %q%s", ah, uh, dump)
	}
	if !reflect.DeepEqual(aRecap, uRecap) {
		t.Errorf("PLAY RECAP differs\n ansible:    %v\n understudy: %v%s",
			sortRecap(aRecap), sortRecap(uRecap), dump)
	}
	if !reflect.DeepEqual(aStatus, uStatus) {
		t.Errorf("per-task status differs\n%s%s", diffStatus(aStatus, uStatus), dump)
	}
}

func parallelGoldenCase(t *testing.T, playbook string) {
	t.Helper()
	// One-second task deadlines must run without competing playbooks.
	if filepath.Base(playbook) != "task_timeout.yml" {
		t.Parallel()
	}
}

func TestGoldenDifferential(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)

	corpus, err := filepath.Glob("golden/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no golden corpus found: %v", err)
	}

	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			parallelGoldenCase(t, pb)
			workA := t.TempDir()
			workB := t.TempDir()
			// A sibling "<name>.inventory" file (multi-host / groups) is used
			// when present; otherwise a single local host.
			invSrc := strings.TrimSuffix(pb, ".yml") + ".inventory"
			invA := writeGoldenInventory(t, workA, invSrc)
			invB := writeGoldenInventory(t, workB, invSrc)

			// Two runs each so idempotence is part of the comparison.
			env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
				"ANSIBLE_LOCALHOST_WARNING=False", "ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
				"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_SYSTEM_WARNINGS=False"}
			env = append(env, goldenPackagingEnv(ansible)...)

			// Filename-prefix directives: "check_" runs --check; "tags_"
			// runs --tags run_me,also to exercise tag filtering.
			var extra []string
			base := filepath.Base(pb)
			if strings.HasPrefix(base, "check_") {
				extra = []string{"--check"}
			}
			if strings.HasPrefix(base, "tags_") {
				extra = []string{"--tags", "run_me,also"}
			}
			aArgs := append([]string{"-i", invA, "-c", "local", "-e", "workdir=" + workA}, extra...)
			uArgs := append([]string{"playbook", "-i", invB, "-c", "local", "-e", "workdir=" + workB}, extra...)

			a := runTool(t, ansible, append(aArgs, pb), env, 2)
			u := runTool(t, understudy, append(uArgs, pb), env, 2)
			compareRuns(t, a, u, workA, workB)
		})
	}
}

func writeGoldenInventory(t *testing.T, dir, src string) string {
	t.Helper()
	content := []byte("localhost ansible_connection=local\n")
	if data, err := os.ReadFile(src); err == nil {
		content = data
	}
	inv := filepath.Join(dir, "hosts")
	if err := os.WriteFile(inv, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return inv
}

func sortRecap(m map[string]recap) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		r := m[k]
		b.WriteString(k)
		b.WriteString(":")
		b.WriteString(strings.TrimSpace(strings.NewReplacer().Replace(
			" ok=" + itoa(r.ok) + " changed=" + itoa(r.changed) + " unreachable=" + itoa(r.unreachable) +
				" failed=" + itoa(r.failed) + " skipped=" + itoa(r.skipped) + " rescued=" + itoa(r.rescued) +
				" ignored=" + itoa(r.ignored))))
		b.WriteString("  ")
	}
	return b.String()
}

func diffStatus(a, b map[taskStatus]int) string {
	var b2 strings.Builder
	seen := map[taskStatus]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	keys := make([]taskStatus, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].task != keys[j].task {
			return keys[i].task < keys[j].task
		}
		return keys[i].status < keys[j].status
	})
	for _, k := range keys {
		if a[k] != b[k] {
			b2.WriteString("  [" + k.task + " / " + k.host + " / " + k.status + "] ansible=" +
				itoa(a[k]) + " understudy=" + itoa(b[k]) + "\n")
		}
	}
	return b2.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
