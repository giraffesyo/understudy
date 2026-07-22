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
		t.Skip("ansible-playbook not on PATH (set UNDERSTUDY_ANSIBLE_PLAYBOOK)")
	}
	return p
}

func understudyBin(t *testing.T) string {
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built (run: make build)")
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

// runTool runs a playbook (twice, returning the second run's output) so the
// comparison is at steady state — idempotence differences surface as recap
// mismatches.
func runTool(t *testing.T, bin string, args, env []string, runs int) string {
	t.Helper()
	var out []byte
	for i := 0; i < runs; i++ {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		out, _ = cmd.CombinedOutput()
	}
	return string(out)
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

			// A "check_" prefixed corpus file runs both tools in --check mode.
			var extra []string
			if strings.HasPrefix(filepath.Base(pb), "check_") {
				extra = []string{"--check"}
			}
			aArgs := append([]string{"-i", invA, "-c", "local", "-e", "workdir=" + workA}, extra...)
			uArgs := append([]string{"playbook", "-i", invB, "-c", "local", "-e", "workdir=" + workB}, extra...)

			aOut := runTool(t, ansible, append(aArgs, pb), env, 2)
			uOut := runTool(t, understudy, append(uArgs, pb), env, 2)

			aStatus, aRecap := parseRun(aOut)
			uStatus, uRecap := parseRun(uOut)

			if !reflect.DeepEqual(aRecap, uRecap) {
				t.Errorf("PLAY RECAP differs\n ansible:   %v\n understudy: %v\n\n--- ansible ---\n%s\n--- understudy ---\n%s",
					sortRecap(aRecap), sortRecap(uRecap), aOut, uOut)
			}
			if !reflect.DeepEqual(aStatus, uStatus) {
				t.Errorf("per-task status differs\n%s\n\n--- ansible ---\n%s\n--- understudy ---\n%s",
					diffStatus(aStatus, uStatus), aOut, uOut)
			}
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
