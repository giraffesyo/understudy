//go:build linux || darwin

package modules

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The passphrase is answered at ssh-keygen's prompts, and the key it makes
// opens with that passphrase and no other.
func TestSSHKeygenPTYPassphrase(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	argv := []string{bin, "-t", "ed25519", "-C", "pty test", "-f", key}
	rc, out, errOut := sshKeygenPTY(&RunEnv{}, argv, "s3cret phrase")
	if rc == nil || *rc != 0 {
		t.Fatalf("rc=%v out=%q err=%q", rc, out, errOut)
	}
	if !strings.Contains(out+errOut, "Enter same passphrase again") {
		t.Errorf("prompts not seen: out=%q err=%q", out, errOut)
	}
	if b, err := exec.Command(bin, "-y", "-P", "s3cret phrase", "-f", key).CombinedOutput(); err != nil {
		t.Errorf("key does not open with the passphrase: %v\n%s", err, b)
	}
	if err := exec.Command(bin, "-y", "-P", "", "-f", key).Run(); err == nil {
		t.Error("key opens without a passphrase")
	}
}

// An existing private key makes ssh-keygen ask to overwrite; that is
// reported like ansible-core (no rc, "Key already exists").
func TestSSHKeygenPTYOverwritePrompt(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rc, out, _ := sshKeygenPTY(&RunEnv{}, []string{bin, "-t", "ed25519", "-f", key}, "x")
	if rc != nil || out != "Key already exists" {
		t.Errorf("rc=%v out=%q", rc, out)
	}
}
