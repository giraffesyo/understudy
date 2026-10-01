package modules

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// AnsibleModule.tmpdir: the action normally hands a module the directory
// its files were staged in (_ansible_tmpdir), but not when it runs as an
// unprivileged become user, who could not write there. The module then
// makes its own under remote_tmp (expanded for the user it runs as),
// creating remote_tmp itself with a warning when it is missing. It does
// so the first time it needs the directory: when its working directory
// is not readable to it (_set_cwd moves there), or when a module writes a
// temporary file (lineinfile, blockinfile, replace, uri, get_url, user).

// moduleTmpdir is AnsibleModule.tmpdir: the module's own temporary
// directory, made on first use ("" with a temp dir of the system's
// choosing). It is only made when the module got no tmpdir
// (env.ModuleRemoteTmp set); otherwise the caller's default applies.
func (env *RunEnv) moduleTmpdir() string {
	if env == nil || env.ModuleRemoteTmp == "" {
		return ""
	}
	if env.modTmpMade {
		return env.modTmp
	}
	env.modTmpMade = true
	base := pyExpandPath(env.ModuleRemoteTmp)
	if base != "" && !pathExists(base) {
		if err := os.MkdirAll(base, 0o700); err != nil {
			env.warn(fmt.Sprintf("Unable to use %s as temporary directory, falling back to system default.", pyStrRepr(base)))
			base = ""
		} else {
			env.warn(fmt.Sprintf("Module remote_tmp %s did not exist and was created with a mode of 0700, "+
				"this may cause issues when running as another user. To avoid this, create the remote_tmp dir "+
				"with the correct permissions manually", base))
		}
	}
	prefix := "ansible-moduletmp-" + strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', -1, 64) + "-"
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return ""
	}
	env.modTmp = dir
	return dir
}

// warn is AnsibleModule.warn for warnings given outside a module's own
// result (they lead the result's warnings).
func (env *RunEnv) warn(msg string) {
	env.modWarnings = append(env.modWarnings, msg)
}

// moduleSetCwd is AnsibleModule._set_cwd for a module without a tmpdir:
// a working directory it cannot read is swapped for its tmpdir (made
// then), else $HOME, else the system temp dir.
func (env *RunEnv) moduleSetCwd() {
	if env.ModuleRemoteTmp == "" {
		return
	}
	if cwd, err := os.Getwd(); err == nil && syscall.Access(cwd, 4) == nil {
		return
	}
	for _, d := range []string{env.moduleTmpdir(), pyExpandVars("$HOME"), pyGettempdir()} {
		if d != "" && syscall.Access(d, 4) == nil && os.Chdir(d) == nil {
			return
		}
	}
}

// moduleCleanup removes the module's tmpdir and puts the warnings given
// along the way ahead of the result's own.
func (env *RunEnv) moduleCleanup(res *agentproto.Result) {
	if env.modTmp != "" {
		os.RemoveAll(env.modTmp)
	}
	if len(env.modWarnings) == 0 || res == nil {
		return
	}
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	prior, _ := res.Extra["warnings"].([]any)
	res.Extra["warnings"] = append(anyList(env.modWarnings), prior...)
}

// tempFileIn is tempfile.mkstemp(dir=module.tmpdir): in the module's own
// tmpdir when it has made one, else the system's.
func (env *RunEnv) tempFileIn() (*os.File, error) {
	if d := env.moduleTmpdir(); d != "" {
		return mkstemp(d)
	}
	return os.CreateTemp("", "tmp")
}
