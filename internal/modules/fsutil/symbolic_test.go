package fsutil

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/symbolic_ansible.json holds [mode, current, isDir, want] rows
// produced by Ansible's own AnsibleModule._symbolic_mode_to_octal with
// umask 022, so this is a differential test against the reference.
func TestSymbolicModeMatchesAnsible(t *testing.T) {
	data, err := os.ReadFile("testdata/symbolic_ansible.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows [][4]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		sym, cur, isDir := r[0].(string), uint32(r[1].(float64)), r[2].(bool)
		got, err := applySymbolic(sym, cur, isDir, 0o022)
		if err != nil {
			t.Errorf("%q on %o (dir=%v): %v", sym, cur, isDir, err)
			continue
		}
		if want := uint32(r[3].(float64)); got != want {
			t.Errorf("%q on %o (dir=%v) = %o, want %o", sym, cur, isDir, got, want)
		}
	}
}

func TestSymbolicModeRejectsGarbage(t *testing.T) {
	for _, s := range []string{"z=rw", "u=q", "rw"} {
		if _, err := applySymbolic(s, 0o644, false, 0o022); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestResolveModeNumericIgnoresCurrent(t *testing.T) {
	m, err := ResolveMode("0600", 0o755)
	if err != nil || m.Perm() != 0o600 {
		t.Fatalf("got %v, %v", m, err)
	}
	if IsSymbolicMode("0644") || IsSymbolicMode(int64(420)) || !IsSymbolicMode("u=rw,g=r,o=r") {
		t.Fatal("IsSymbolicMode misclassifies")
	}
}
