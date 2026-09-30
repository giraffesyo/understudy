package modules

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// DarwinGroup: dseditgroup creates (a system group gets the lowest free
// gid under 500), deletes (force is refused) and changes the gid.
func TestDarwinGroup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DarwinGroup is chosen on macOS only")
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls.log")
	scripts := map[string]string{
		"dscl":        `printf 'staff                 20\n_www                  70\n_x                   451\nbig                 1000\n'`,
		"dseditgroup": `echo "dseditgroup $*" >> ` + logFile,
		"dscacheutil": `[ "$5" = staff ] && printf 'name: staff\npassword: *\ngid: 20\nusers: root\n'; exit 0`,
	}
	for name, body := range scripts {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	run := func(args map[string]any) (string, map[string]any, bool) {
		os.Remove(logFile)
		res := groupModule(&RunEnv{}, args)
		data, _ := os.ReadFile(logFile)
		return strings.TrimSpace(string(data)), res.Extra, res.Failed
	}
	if got, _, _ := run(map[string]any{"name": "newgrp", "system": true}); got != "dseditgroup -o create -i 452 -L newgrp" {
		t.Errorf("system create: %q", got)
	}
	if got, _, _ := run(map[string]any{"name": "newgrp", "gid": 3000}); got != "dseditgroup -o create -i 3000 -L newgrp" {
		t.Errorf("create with gid: %q", got)
	}
	if got, extra, _ := run(map[string]any{"name": "staff", "gid": 21}); got != "dseditgroup -o edit -i 21 -L staff" || extra["gid"] != int64(20) {
		t.Errorf("gid change: %q %v", got, extra)
	}
	if got, _, _ := run(map[string]any{"name": "staff", "gid": 20}); got != "" {
		t.Errorf("same gid ran %q", got)
	}
	if got, _, _ := run(map[string]any{"name": "staff", "state": "absent"}); got != "dseditgroup -o delete -L staff" {
		t.Errorf("delete: %q", got)
	}
	res := groupModule(&RunEnv{}, map[string]any{"name": "staff", "state": "absent", "force": true})
	if !res.Failed || res.Msg != "The force option is not supported for group deletion on this platform." {
		t.Errorf("force: %+v", res)
	}
}
