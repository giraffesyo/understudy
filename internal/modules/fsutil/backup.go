package fsutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Backup copies path to Ansible's backup name — "<path>.<pid>.<YYYY-mm-dd@
// HH:MM:SS>~" — preserving mode, ownership and timestamps (backup_local's
// preserved_copy), and returns the backup path.
func Backup(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dest := fmt.Sprintf("%s.%d.%s", path, os.Getpid(), time.Now().Format("2006-01-02@15:04:05~"))
	if err := os.WriteFile(dest, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	os.Chmod(dest, info.Mode().Perm()|specialBits(info.Mode()))
	if uid, gid, ok := statIDs(info); ok {
		os.Chown(dest, uid, gid)
	}
	os.Chtimes(dest, info.ModTime(), info.ModTime())
	return dest, nil
}

// ValidateError is a failed validate command, carrying its output.
type ValidateError struct {
	Cmd            string
	RC             int
	Stdout, Stderr string
}

func (e *ValidateError) Error() string { return "failed to validate" }

// Validate runs a module's validate command against candidate content
// before it replaces dest: the content goes to a temp file beside dest and
// "%s" in cmd is replaced with its path (Ansible requires the "%s").
func Validate(cmd, dest string, content []byte) error {
	if !strings.Contains(cmd, "%s") {
		return fmt.Errorf("validate must contain %%s: %s", cmd)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".understudy-validate-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	full := strings.ReplaceAll(cmd, "%s", tmp.Name())
	c := exec.Command("/bin/sh", "-c", full)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		rc := -1
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
		return &ValidateError{Cmd: full, RC: rc, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	return nil
}
