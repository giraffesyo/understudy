package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The script runs from remote_tmp, not the system temp dir, which
// hardened hosts mount noexec.
func TestScriptRunsFromRemoteTmp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	res := scriptModule(&RunEnv{LoginHome: home, Payload: strings.NewReader("#!/bin/sh\necho \"$0\"\n")}, map[string]any{})
	if res.Failed {
		t.Fatalf("script failed: %s %s", res.Msg, res.Stderr)
	}
	rt := filepath.Join(home, ".ansible", "tmp")
	if dir := filepath.Dir(res.Stdout); filepath.Dir(dir) != rt || !strings.HasPrefix(filepath.Base(dir), "ansible-tmp-") {
		t.Errorf("script ran from %q, want under %s/ansible-tmp-*", res.Stdout, rt)
	}
	if entries, err := os.ReadDir(rt); err != nil || len(entries) != 0 {
		t.Errorf("remote_tmp after the run = %v, %v; want empty", entries, err)
	}
}
