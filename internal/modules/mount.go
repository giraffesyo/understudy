package modules

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(mountModule, "mount", "ansible.posix.mount")
}

var mountSpec = args.Spec{
	"path":   {Required: true, Aliases: []string{"name"}},
	"src":    {},
	"fstype": {},
	"opts":   {Default: "defaults"},
	"dump":   {Type: "int", Default: 0},
	"passno": {Type: "int", Default: 0},
	"state": {Required: true, Choices: []string{
		"mounted", "unmounted", "present", "absent", "remounted"}},
	"fstab": {Default: "/etc/fstab"},
}

// fstabEntry is one /etc/fstab line.
type fstabEntry struct {
	src, path, fstype, opts string
	dump, passno            string
}

// mountModule manages fstab entries and live mounts.
//
//	present:   fstab entry only
//	mounted:   fstab entry + mounted now
//	unmounted: umount now (fstab untouched)
//	absent:    umount + remove fstab entry
//	remounted: remount now
func mountModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mountSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")
	state := p.Str("state")
	fstab := p.Str("fstab")

	if (state == "mounted" || state == "present") && (p.Str("src") == "" || p.Str("fstype") == "") {
		return agentproto.Fail("state=%s requires 'src' and 'fstype'", state)
	}

	res := &agentproto.Result{Extra: map[string]any{"path": path, "state": state}}

	entries, original, err := readFstab(fstab)
	if err != nil && !os.IsNotExist(err) {
		return agentproto.Fail("reading %s: %v", fstab, err)
	}

	want := fstabEntry{
		src: p.Str("src"), path: path, fstype: p.Str("fstype"),
		opts: p.Str("opts"),
		dump: fmt.Sprintf("%d", p.Int("dump")), passno: fmt.Sprintf("%d", p.Int("passno")),
	}

	// fstab reconciliation.
	fstabChanged := false
	switch state {
	case "present", "mounted":
		found := false
		for i, e := range entries {
			if e.path == path {
				found = true
				if e != want {
					entries[i] = want
					fstabChanged = true
				}
				break
			}
		}
		if !found {
			entries = append(entries, want)
			fstabChanged = true
		}
	case "absent":
		var kept []fstabEntry
		for _, e := range entries {
			if e.path == path {
				fstabChanged = true
				continue
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	if fstabChanged {
		res.Changed = true
		if !env.CheckMode {
			if err := writeFstab(fstab, entries, original); err != nil {
				return agentproto.Fail("%v", err)
			}
		}
	}

	// Live mount reconciliation.
	mounted := isMounted(env, path)
	switch state {
	case "mounted":
		if !mounted {
			res.Changed = true
			if !env.CheckMode {
				if _, err := runOut(env, "mkdir", "-p", path); err != nil {
					return agentproto.Fail("creating mountpoint %s failed", path)
				}
				if out, err := runOut(env, "mount", path); err != nil {
					return agentproto.Fail("mount %s failed: %v: %s", path, err, tail(out))
				}
			}
		}
	case "unmounted", "absent":
		if mounted {
			res.Changed = true
			if !env.CheckMode {
				if out, err := runOut(env, "umount", path); err != nil {
					return agentproto.Fail("umount %s failed: %v: %s", path, err, tail(out))
				}
			}
		}
	case "remounted":
		res.Changed = true
		if !env.CheckMode {
			if out, err := runOut(env, "mount", "-o", "remount", path); err != nil {
				return agentproto.Fail("remount %s failed: %v: %s", path, err, tail(out))
			}
		}
	}
	return res
}

func isMounted(env *RunEnv, path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Non-Linux fallback (mount output parse).
		out, err := runOut(env, "mount")
		return err == nil && strings.Contains(out, " on "+path+" ")
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

// readFstab parses managed entries, keeping the original bytes so comments
// and unmanaged lines survive writes.
func readFstab(path string) ([]fstabEntry, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var entries []fstabEntry
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 4 {
			continue
		}
		e := fstabEntry{src: f[0], path: f[1], fstype: f[2], opts: f[3], dump: "0", passno: "0"}
		if len(f) > 4 {
			e.dump = f[4]
		}
		if len(f) > 5 {
			e.passno = f[5]
		}
		entries = append(entries, e)
	}
	return entries, data, nil
}

// writeFstab rewrites fstab: unmanaged lines (comments, blanks) keep their
// positions; entry lines are regenerated from the reconciled set.
func writeFstab(path string, entries []fstabEntry, original []byte) error {
	byPath := map[string]fstabEntry{}
	var order []string
	for _, e := range entries {
		if _, ok := byPath[e.path]; !ok {
			order = append(order, e.path)
		}
		byPath[e.path] = e
	}
	var out []string
	emitted := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(original), "\n"), "\n") {
		t := strings.TrimSpace(line)
		f := strings.Fields(t)
		if t == "" || strings.HasPrefix(t, "#") || len(f) < 4 {
			out = append(out, line)
			continue
		}
		mountPath := f[1]
		if e, ok := byPath[mountPath]; ok {
			out = append(out, fstabLine(e))
			emitted[mountPath] = true
		}
		// Removed entries are dropped.
	}
	for _, p := range order {
		if !emitted[p] {
			out = append(out, fstabLine(byPath[p]))
		}
	}
	content := strings.Join(out, "\n") + "\n"
	return fsutil.AtomicWrite(path, bytes.NewReader([]byte(content)), 0o644)
}

func fstabLine(e fstabEntry) string {
	return fmt.Sprintf("%s %s %s %s %s %s", e.src, e.path, e.fstype, e.opts, e.dump, e.passno)
}
