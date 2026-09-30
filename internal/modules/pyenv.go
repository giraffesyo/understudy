package modules

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Some of ansible's output depends on the target's Python: CPython's
// source line in SSL error text, the version of an installed library a
// module reports. understudy never runs Python, so it reads the answer
// off the filesystem: the interpreter the task's
// ansible_python_interpreter names (or that ansible would discover), and
// the installed packages' metadata.

// pyDiscoveryFallback is ansible-core's INTERPRETER_PYTHON_FALLBACK: with
// interpreter_python=auto the first of these found on the target runs
// the modules.
var pyDiscoveryFallback = []string{"python3.14", "python3.13", "python3.12", "python3.11",
	"python3.10", "python3.9", "/usr/bin/python3", "python3"}

var pyVersionRe = regexp.MustCompile(`python(3\.\d+)$`)

// pyTarget is what understudy knows about the Python that would run
// ansible's modules on this host.
type pyTarget struct {
	version string   // "3.12", or "" when unknown
	paths   []string // the interpreter as named and as resolved

	buildOnce sync.Once
	build     string // "3.12.3" (see targetPythonBuild)
}

var pyTargets sync.Map // interpreter setting -> *pyTarget

// targetPython resolves the interpreter the task's
// ansible_python_interpreter names, or the one discovery would find.
func targetPython(env *RunEnv) *pyTarget {
	interp := ""
	if env != nil {
		interp = env.PythonInterpreter
	}
	if t, ok := pyTargets.Load(interp); ok {
		return t.(*pyTarget)
	}
	t := &pyTarget{}
	candidates := pyDiscoveryFallback
	if interp != "" {
		candidates = []string{interp}
	}
	for _, name := range candidates {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		t.paths = append(t.paths, p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			t.paths = append(t.paths, real)
		}
		for _, q := range t.paths {
			if m := pyVersionRe.FindStringSubmatch(q); m != nil {
				t.version = m[1]
			}
		}
		break
	}
	v, _ := pyTargets.LoadOrStore(interp, t)
	return v.(*pyTarget)
}

// targetPythonVersion is the major.minor ("3.12") of the task's Python,
// or "" when there is none or its version cannot be told from its path.
func targetPythonVersion(env *RunEnv) string { return targetPython(env).version }

// pySitePackages lists the directories the target's Python imports
// third-party packages from, most specific first: the interpreter's own
// prefix, the version's site-packages (lib and lib64, /usr/local and
// /usr), then Debian's version-less dist-packages.
func pySitePackages(env *RunEnv) []string {
	t := targetPython(env)
	v := t.version
	var dirs []string
	if v != "" {
		// <prefix>/bin/python -> <prefix>/lib/pythonX.Y/site-packages
		for _, p := range t.paths {
			dirs = append(dirs, filepath.Join(filepath.Dir(filepath.Dir(p)), "lib", "python"+v, "site-packages"))
		}
		for _, prefix := range []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/usr/lib64"} {
			dirs = append(dirs, prefix+"/python"+v+"/site-packages", prefix+"/python"+v+"/dist-packages")
		}
	}
	dirs = append(dirs, "/usr/local/lib/python3/dist-packages", "/usr/lib/python3/dist-packages")
	return dirs
}

// pyDistVersion returns the installed version of a Python distribution
// (PyMySQL, mysqlclient, ...) from its .dist-info or .egg-info metadata,
// as importlib.metadata would find it, or "".
func pyDistVersion(env *RunEnv, dist string) string {
	return pyDistVersionIn(pySitePackages(env), dist)
}

func pyDistVersionIn(dirs []string, dist string) string {
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(s))
	}
	want := norm(dist)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			base, ok := strings.CutSuffix(name, ".dist-info")
			if !ok {
				if base, ok = strings.CutSuffix(name, ".egg-info"); !ok {
					continue
				}
			}
			// name-version[-pyX.Y]
			project, _, _ := strings.Cut(base, "-")
			if norm(project) != want {
				continue
			}
			if v := pyMetadataVersion(filepath.Join(dir, name)); v != "" {
				return v
			}
		}
	}
	return ""
}

// pyMetadataVersion reads the Version header of a dist-info METADATA or
// egg-info PKG-INFO (a file, or a directory holding one).
func pyMetadataVersion(path string) string {
	for _, f := range []string{filepath.Join(path, "METADATA"), filepath.Join(path, "PKG-INFO"), path} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Version:"); ok {
				fh.Close()
				return strings.TrimSpace(v)
			}
		}
		fh.Close()
	}
	return ""
}
