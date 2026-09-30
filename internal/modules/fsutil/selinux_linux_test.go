package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// The raw security.selinux attribute round-trips (needs root; skipped
// where the filesystem or kernel refuses it).
func TestSELinuxXattrRoundTrip(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	const con = "system_u:object_r:ssh_home_t:s0"
	if err := lsetfilecon(f, con); err != nil {
		t.Skipf("cannot set security.selinux here: %v", err)
	}
	if got, err := lgetfilecon(f); err != nil || got != con {
		t.Errorf("lgetfilecon = %q, %v", got, err)
	}
	if _, err := lgetfilecon(filepath.Join(filepath.Dir(f), "missing")); err == nil {
		t.Error("a missing file has a label")
	}
}
