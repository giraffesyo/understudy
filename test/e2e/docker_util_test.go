//go:build e2e || golden

// Docker helpers shared by the container-backed tests. Several test
// processes (other worktrees, other agents) may share one Docker host, so
// every container and network a process creates carries a per-process
// suffix, publishes its ports on a dynamic loopback port, and is removed
// only by the process that made it. Image tags are content-addressed (a
// hash of the build context), so concurrent builds of the same Dockerfile
// agree on the result and differing Dockerfiles never overwrite each other.
package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dockerRunID is unique to this test process: the pid keeps it readable,
// the random part keeps it unique across hosts sharing one daemon.
var dockerRunID = func() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
}()

// dockerName scopes a container or network name to this process.
func dockerName(base string) string { return base + "-" + dockerRunID }

// dockerAvailable skips the test when docker isn't usable.
func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}

// dockerBuild builds the Dockerfile (plus any extra context files) and
// returns a content-addressed tag: repo:<hash of the context>.
func dockerBuild(t *testing.T, repo, dockerfile string, extra map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	h := sha256.New()
	files := map[string][]byte{"Dockerfile": []byte(dockerfile)}
	for k, v := range extra {
		files[k] = v
	}
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", k, len(files[k]))
		h.Write(files[k])
		if err := os.WriteFile(filepath.Join(dir, k), files[k], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tag := repo + ":" + hex.EncodeToString(h.Sum(nil))[:16]
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", repo, err, out)
	}
	return tag
}

// dockerRun starts a detached container named dockerName(base) (removed at
// test cleanup) and returns its name. args go between the name and the
// image; the image may be followed by a command.
func dockerRun(t *testing.T, base string, args ...string) string {
	t.Helper()
	name := dockerName(base)
	exec.Command("docker", "rm", "-f", name).Run()
	runArgs := append([]string{"run", "-d", "--name", name, "--label", "understudy-test=" + dockerRunID}, args...)
	if out, err := exec.Command("docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	return name
}

// dockerPort returns the host port a container's port is published on.
func dockerPort(t *testing.T, name, port string) string {
	t.Helper()
	out, err := exec.Command("docker", "port", name, port).Output()
	if err != nil {
		state, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{.State.Error}}", name).CombinedOutput()
		t.Fatalf("docker port %s %s: %v (container: %s)", name, port, err, strings.TrimSpace(string(state)))
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return line[strings.LastIndexByte(line, ':')+1:]
}
