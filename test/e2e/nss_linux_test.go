package e2e

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/giraffesyo/understudy/internal/executor"
)

func TestCopyNSSOwnership(t *testing.T) {
	const owner, group = "understudy-nss-user", "understudy-nss-group"
	if _, err := user.Lookup(owner); err == nil {
		t.Fatal("fixture user must not exist in local account files")
	}
	if _, err := user.LookupGroup(group); err == nil {
		t.Fatal("fixture group must not exist in local account files")
	}
	dir := t.TempDir()
	getent := fmt.Sprintf(`#!/bin/sh
case "$1:$2" in
passwd:%s|passwd:%d) echo '%s:x:%d:%d::%s:/bin/sh';;
group:%s|group:%d) echo '%s:x:%d:';;
*) exit 2;;
esac
`, owner, os.Getuid(), owner, os.Getuid(), os.Getgid(), dir, group, os.Getgid(), group, os.Getgid())
	if err := os.WriteFile(filepath.Join(dir, "getent"), []byte(getent), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	dest := filepath.Join(dir, "script.sh")
	code, out, _ := run(t, fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - copy:
        content: '#!/bin/sh'
        dest: %q
        owner: %s
        group: %s
        mode: '0755'
      register: copied
    - assert:
        that:
          - copied.owner == '%s'
          - copied.group == '%s'
          - copied.uid == %d
          - copied.gid == %d
    - file:
        path: %q
        owner: %s
        group: %s
        mode: '0755'
      register: repeated
    - assert:
        that:
          - not repeated.changed
`, dest, owner, group, owner, group, os.Getuid(), os.Getgid(), dest, owner, group), executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if content, err := os.ReadFile(dest); err != nil || string(content) != "#!/bin/sh" {
		t.Fatalf("copy content = %q, %v", content, err)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("copy mode = %v, %v", info, err)
	}
}
