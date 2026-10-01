package modules

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports ansible.builtin.apt_repository for one-line (.list)
// sources. ppa: repositories (which need the Launchpad API and key
// import) are rejected with a clear message.

func init() {
	names := []string{"apt_repository", "ansible.builtin.apt_repository"}
	Register(aptRepositoryModule, names...)
	for _, n := range names {
		specs[n] = aptRepositorySpec
	}
}

var aptRepositorySpec = args.Spec{
	"repo":                         {Required: true},
	"state":                        {Default: "present", Choices: []string{"absent", "present"}},
	"mode":                         {Type: "any"},
	"update_cache":                 {Type: "bool", Default: true, Aliases: []string{"update-cache"}},
	"update_cache_retries":         {Type: "int", Default: 5},
	"update_cache_retry_max_delay": {Type: "int", Default: 12},
	"filename":                     {},
	"install_python_apt":           {Type: "bool", Default: true},
	"validate_certs":               {Type: "bool", Default: true},
	"codename":                     {},
}

const (
	aptSourcesList = "/etc/apt/sources.list"
	aptSourcesDir  = "/etc/apt/sources.list.d"
)

type aptSourceLine struct {
	valid, enabled  bool
	source, comment string
}

type aptSourcesFiles struct {
	order    []string
	files    map[string][]aptSourceLine
	newRepos map[string]bool
}

func aptParseSourceLine(line string) aptSourceLine {
	l := aptSourceLine{enabled: true}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#") {
		l.enabled = false
		line = line[1:]
	}
	if i := strings.Index(line, "#"); i > 0 {
		l.comment = strings.TrimSpace(line[i+1:])
		line = line[:i]
	}
	l.source = strings.TrimSpace(line)
	if l.source != "" {
		chunks := strings.Fields(l.source)
		if chunks[0] == "deb" || chunks[0] == "deb-src" {
			l.valid = true
			l.source = strings.Join(chunks, " ")
		}
	}
	return l
}

func loadAptSources() *aptSourcesFiles {
	s := &aptSourcesFiles{files: map[string][]aptSourceLine{}, newRepos: map[string]bool{}}
	load := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var group []aptSourceLine
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if line == "" {
				continue
			}
			group = append(group, aptParseSourceLine(line))
		}
		if _, seen := s.files[path]; !seen {
			s.order = append(s.order, path)
		}
		s.files[path] = group
	}
	if st, err := os.Stat(aptSourcesList); err == nil && st.Mode().IsRegular() {
		load(aptSourcesList)
	}
	matches, _ := filepath.Glob(aptSourcesDir + "/*.list")
	for _, m := range matches {
		load(m)
	}
	return s
}

func aptLineText(l aptSourceLine) string {
	var b strings.Builder
	if !l.enabled {
		b.WriteString("# ")
	}
	b.WriteString(l.source)
	if l.comment != "" {
		b.WriteString(" # " + l.comment)
	}
	b.WriteString("\n")
	return b.String()
}

func (s *aptSourcesFiles) dump() map[string]string {
	out := map[string]string{}
	for path, lines := range s.files {
		if len(lines) == 0 {
			continue
		}
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(aptLineText(l))
		}
		out[path] = b.String()
	}
	return out
}

var (
	aptBracketRe = regexp.MustCompile(`\[[^\]]+\]`)
	aptSchemeRe  = regexp.MustCompile(`\w+://`)
	aptNonAlnum  = regexp.MustCompile(`[^a-zA-Z0-9]`)
)

func aptSuggestFilename(line, filename string) string {
	if filename != "" {
		return filename + ".list"
	}
	line = aptBracketRe.ReplaceAllString(line, "")
	line = aptSchemeRe.ReplaceAllString(line, "")
	var parts []string
	for _, p := range strings.Fields(line) {
		if p != "deb" && p != "deb-src" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ".list"
	}
	first := parts[0]
	if i := strings.Index(first, "@"); i >= 0 {
		first = first[i+1:]
	}
	return strings.Join(strings.Fields(aptNonAlnum.ReplaceAllString(first, " ")), "_") + ".list"
}

func aptRepositoryModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := aptRepositorySpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if aptBindingsMissing(env) {
		switch {
		case env.CheckMode:
			return agentproto.Fail("python3-apt must be installed to use check mode. If run normally this module can auto-install it.")
		case !p.Bool("install_python_apt"):
			return agentproto.Fail("python3-apt is not installed, and install_python_apt is False")
		}
		// install_python_apt: apt-get update and install, then carry on
		// under the Python that can import the bindings.
		if aptGet, err := getBinPath("apt-get"); err == nil {
			for _, argv := range [][]string{{aptGet, "update"}, {aptGet, "install", "python3-apt", "-y", "-q"}} {
				if rc, _, errOut := runCommand(env, argv, cmdOpts{}); rc != 0 {
					return agentproto.Fail("Failed to auto-install python3-apt. Error was: '%s'", strings.TrimSpace(errOut))
				}
			}
		}
		if aptBindingsMissing(env) {
			return agentproto.Fail("python3-apt must be installed and visible from %s.", targetPythonExecutable(env))
		}
	}
	repo := p.Str("repo")
	state := p.Str("state")
	if repo == "" {
		return agentproto.Fail("Please set argument 'repo' to a non-empty value")
	}
	if strings.HasPrefix(repo, "ppa:") {
		return agentproto.Fail("apt_repository: ppa: repositories are not supported by understudy yet; add the repository line and its signing key explicitly")
	}
	if _, err := os.Stat("/etc/apt"); err != nil {
		return agentproto.Fail("Module apt_repository is not supported on target.")
	}
	s := loadAptSources()
	before := s.dump()

	parsed := aptParseSourceLine(repo)
	if !parsed.valid || !parsed.enabled {
		// InvalidSource carries the line with a leading '#' stripped and any
		// comment removed, as _parse leaves it.
		l := strings.TrimSpace(repo)
		l = strings.TrimPrefix(l, "#")
		if i := strings.Index(l, "#"); i > 0 {
			l = l[:i]
		}
		return agentproto.Fail("Invalid repository string: %s", l)
	}
	source := parsed.source
	if state == "present" {
		found := false
		for _, path := range s.order {
			for i, l := range s.files[path] {
				if l.valid && l.source == source {
					s.files[path][i].enabled = true
					found = true
				}
			}
		}
		if !found {
			file := aptSuggestFilename(source, p.Str("filename"))
			if !strings.Contains(file, "/") {
				file = filepath.Join(aptSourcesDir, file)
			}
			if _, ok := s.files[file]; !ok {
				s.order = append(s.order, file)
			}
			s.files[file] = append(s.files[file], aptSourceLine{valid: true, enabled: true, source: source})
			s.newRepos[file] = true
		}
	} else {
		for _, path := range s.order {
			kept := s.files[path][:0]
			for _, l := range s.files[path] {
				if l.valid && l.enabled && l.source == source {
					continue
				}
				kept = append(kept, l)
			}
			s.files[path] = kept
		}
	}
	after := s.dump()
	changed := len(before) != len(after)
	if !changed {
		for k, v := range before {
			if after[k] != v {
				changed = true
				break
			}
		}
	}
	var added, removed []string
	var diff []any
	if changed {
		for k := range after {
			if _, ok := before[k]; !ok {
				added = append(added, k)
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok {
				removed = append(removed, k)
			}
		}
		sort.Strings(added)
		sort.Strings(removed)
		if env.DiffMode {
			for _, f := range append(append([]string{}, added...), removed...) {
				bh, ah := f, f
				if _, ok := before[f]; !ok {
					bh = "/dev/null"
				}
				if _, ok := after[f]; !ok {
					ah = "/dev/null"
				}
				diff = append(diff, map[string]any{"before": before[f], "after": after[f],
					"before_header": bh, "after_header": ah})
			}
		}
	}
	if changed && !env.CheckMode {
		mode := os.FileMode(0o644)
		if m, ok := raw["mode"]; ok && m != nil {
			if parsedMode, err := parseAptMode(m); err == nil {
				mode = parsedMode
			} else {
				return agentproto.Fail("%v", err)
			}
		}
		for _, path := range s.order {
			lines := s.files[path]
			if len(lines) == 0 {
				os.Remove(path)
				continue
			}
			var b bytes.Buffer
			for _, l := range lines {
				b.WriteString(aptLineText(l))
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return agentproto.Fail("Failed to create directory %s: %v", filepath.Dir(path), err)
			}
			if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b.Bytes()) && !s.newRepos[path] {
				continue
			}
			perm := os.FileMode(0o644)
			if st, err := os.Stat(path); err == nil {
				perm = st.Mode().Perm()
			}
			if s.newRepos[path] {
				perm = mode
			}
			if err := fsutil.AtomicWrite(path, bytes.NewReader(b.Bytes()), perm); err != nil {
				return agentproto.Fail("%v", err)
			}
			if s.newRepos[path] {
				os.Chmod(path, mode)
			}
		}
		if p.Bool("update_cache") {
			if out, err := runAptGet(env, "update"); err != nil {
				return agentproto.Fail("Failed to update apt cache after %d retries: %s", p.Int("update_cache_retries"), strings.TrimSpace(out))
			}
		}
	}
	if added == nil {
		added = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
	if diff == nil {
		diff = []any{}
	}
	return &agentproto.Result{Changed: changed, Diff: diff, Extra: map[string]any{
		"repo": repo, "state": state,
		"sources_added": anyList(added), "sources_removed": anyList(removed),
	}}
}

func parseAptMode(m any) (os.FileMode, error) {
	switch t := m.(type) {
	case int64:
		return os.FileMode(t), nil
	case string:
		n, err := strconv.ParseUint(t, 8, 32)
		if err != nil {
			return 0, err
		}
		return os.FileMode(n), nil
	}
	return 0, strconv.ErrSyntax
}
