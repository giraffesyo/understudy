package modules

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

var pyTargets sync.Map // interpreter setting + discovery inputs -> *pyTarget

// targetPython resolves the interpreter the task's
// ansible_python_interpreter names, or the one discovery would find:
// the first of the fallback list `command -v` finds in the login user's
// PATH (discovery runs without become).
func targetPython(env *RunEnv) *pyTarget {
	var interp, pathEnv string
	candidates := pyDiscoveryFallback
	if env != nil {
		interp, pathEnv = env.PythonInterpreter, env.DiscoveryPath
		if len(env.PythonFallback) > 0 {
			candidates = env.PythonFallback
		}
	}
	key := interp + "\x00" + pathEnv + "\x00" + strings.Join(candidates, "\x00")
	if t, ok := pyTargets.Load(key); ok {
		return t.(*pyTarget)
	}
	t := &pyTarget{}
	if interp != "" {
		candidates = []string{interp}
	}
	for _, name := range candidates {
		p, err := lookPathIn(name, pathEnv)
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
	v, _ := pyTargets.LoadOrStore(key, t)
	return v.(*pyTarget)
}

// lookPathIn is exec.LookPath against a PATH value ("" for this
// process's).
func lookPathIn(name, pathEnv string) (string, error) {
	if pathEnv == "" || strings.Contains(name, "/") {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// targetHasPython reports whether the task has a Python ansible could
// run its module with.
func targetHasPython(env *RunEnv) bool { return len(targetPython(env).paths) > 0 }

// targetPythonExecutable is the module's sys.executable, for messages:
// the interpreter as invoked (discovery falls back to /usr/bin/python3),
// as that Python reports itself.
func targetPythonExecutable(env *RunEnv) string {
	if t := targetPython(env); len(t.paths) > 0 {
		return pySysExecutable(t.paths[0])
	}
	return pySysExecutable("/usr/bin/python3")
}

// brewPythonRe matches a Homebrew Python's real location.
var brewPythonRe = regexp.MustCompile(`^(.+)/Cellar/python@(3\.\d+)/[^/]+/`)

// pySysExecutable is sys.executable of the Python at path: the path
// itself, except where the interpreter reports another one. macOS's
// /usr/bin/python3 is a shim running the active developer directory's
// python3, and Homebrew's framework Pythons name their opt link (a
// virtual environment's interpreter keeps its own path).
func pySysExecutable(path string) string {
	if path == "/usr/bin/python3" && runtime.GOOS == "darwin" {
		dev := os.Getenv("DEVELOPER_DIR")
		if dev == "" {
			dev, _ = os.Readlink("/var/db/xcode_select_link")
		}
		if dev == "" {
			dev = "/Library/Developer/CommandLineTools"
		}
		if p := filepath.Join(dev, "usr/bin/python3"); isFile(p) {
			return p
		}
		return path
	}
	if isFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "pyvenv.cfg")) {
		return path
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		if m := brewPythonRe.FindStringSubmatch(real); m != nil {
			return m[1] + "/opt/python@" + m[2] + "/bin/python" + m[2]
		}
	}
	return path
}

// missingRequiredLib is basic.missing_required_lib() naming the task's
// Python; reason and url are optional.
func missingRequiredLib(env *RunEnv, library, reason, url string) string {
	host, _ := os.Hostname()
	msg := fmt.Sprintf("Failed to import the required Python library (%s) on %s's Python %s.", library, host, targetPythonExecutable(env))
	if reason != "" {
		msg += " This is required " + reason + "."
	}
	if url != "" {
		msg += " See " + url + " for more info."
	}
	return msg + " Please read the module documentation and install it in the appropriate location." +
		" If the required library is installed, but Ansible is using the wrong Python interpreter," +
		" please consult the documentation on ansible_python_interpreter"
}

// pyLibAvailable reports whether ansible's module could import a Python
// library on this host: with a Python, whether the distribution is
// installed (at least minVersion, when given); without one, true, as
// understudy's native code stands in for the library where ansible
// could not run at all.
func pyLibAvailable(env *RunEnv, dist, minVersion string) bool {
	if !targetHasPython(env) {
		return true
	}
	v := pyDistVersion(env, dist)
	return v != "" && (minVersion == "" || !looseVersionLess(v, minVersion))
}

// targetPythonVersion is the major.minor ("3.12") of the task's Python,
// or "" when there is none or its version cannot be told from its path.
func targetPythonVersion(env *RunEnv) string { return targetPython(env).version }

// pySitePackages lists the directories the target's Python imports
// third-party packages from, as site.py builds sys.path: the user site,
// the interpreter's prefix (a venv's own, plus its base's with
// include-system-site-packages), the distribution's site- and
// dist-packages directories, each followed by the directories its .pth
// files add.
func pySitePackages(env *RunEnv) []string {
	t := targetPython(env)
	v := t.version
	if v == "" {
		return []string{"/usr/local/lib/python3/dist-packages", "/usr/lib/python3/dist-packages"}
	}
	lib := "python" + v
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "lib", lib, "site-packages"))
	}
	for _, p := range t.paths {
		prefix := filepath.Dir(filepath.Dir(p))
		dirs = append(dirs, filepath.Join(prefix, "lib", lib, "site-packages"))
		if cfg, err := os.ReadFile(filepath.Join(prefix, "pyvenv.cfg")); err == nil {
			home, system := "", false
			for _, line := range strings.Split(string(cfg), "\n") {
				k, val, _ := strings.Cut(line, "=")
				switch strings.TrimSpace(k) {
				case "home":
					home = strings.TrimSpace(val)
				case "include-system-site-packages":
					system = strings.EqualFold(strings.TrimSpace(val), "true")
				}
			}
			if system && home != "" {
				dirs = append(dirs, filepath.Join(filepath.Dir(home), "lib", lib, "site-packages"))
			}
		}
	}
	for _, prefix := range []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/usr/lib64"} {
		dirs = append(dirs, prefix+"/"+lib+"/site-packages", prefix+"/"+lib+"/dist-packages")
	}
	dirs = append(dirs, "/usr/local/lib/python3/dist-packages", "/usr/lib/python3/dist-packages")

	var out []string
	seen := map[string]bool{}
	var add func(dir string)
	add = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
		pths, _ := filepath.Glob(filepath.Join(dir, "*.pth"))
		for _, pth := range pths {
			data, err := os.ReadFile(pth)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimRight(line, "\r ")
				switch {
				case line == "" || strings.HasPrefix(line, "#"):
				case strings.HasPrefix(line, "import"):
					// Homebrew's "import site; site.addsitedir('...')".
					if m := pthAddSiteDirRe.FindStringSubmatch(line); m != nil {
						add(m[1])
					}
				default:
					if !filepath.IsAbs(line) {
						line = filepath.Join(dir, line)
					}
					add(line)
				}
			}
		}
	}
	for _, d := range dirs {
		add(d)
	}
	return out
}

var pthAddSiteDirRe = regexp.MustCompile(`site\.addsitedir\(['"]([^'"]+)['"]\)`)

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
