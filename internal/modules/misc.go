package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(tempfileModule, "tempfile", "ansible.builtin.tempfile")
	Register(findModule, "find", "ansible.builtin.find")
	Register(modprobeModule, "modprobe", "community.general.modprobe")
}

var tempfileSpec = args.Spec{
	"state":  {Default: "file", Choices: []string{"file", "directory"}},
	"path":   {},
	"prefix": {Default: "ansible."},
	"suffix": {},
}

func tempfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := tempfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "tempfile skipped in check mode"}
	}
	dir := p.Str("path")
	if dir == "" {
		dir = os.TempDir()
	}
	pattern := p.Str("prefix") + "*" + p.Str("suffix")
	var path string
	if p.Str("state") == "directory" {
		path, err = os.MkdirTemp(dir, pattern)
	} else {
		var f *os.File
		f, err = os.CreateTemp(dir, pattern)
		if err == nil {
			path = f.Name()
			f.Close()
		}
	}
	if err != nil {
		return agentproto.Fail("tempfile: %v", err)
	}
	return &agentproto.Result{Changed: true, Extra: map[string]any{"path": path}}
}

var findSpec = args.Spec{
	"paths":     {Type: "list", Required: true, Aliases: []string{"path", "name"}},
	"patterns":  {Type: "list", Aliases: []string{"pattern"}},
	"excludes":  {Type: "list", Aliases: []string{"exclude"}},
	"file_type": {Default: "file", Choices: []string{"file", "directory", "link", "any"}},
	"recurse":   {Type: "bool", Default: false},
	"hidden":    {Type: "bool", Default: false},
	"age":       {}, // e.g. "2d", "-1h" (negative = younger than)
	"use_regex": {Type: "bool", Default: false},
	"contains":  {},
}

// findModule locates files, mirroring ansible.builtin.find's core options.
func findModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := findSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	patterns := stringList(p.List("patterns"))
	excludes := stringList(p.List("excludes"))
	fileType := p.Str("file_type")

	var ageCutoff time.Time
	ageYounger := false
	if ageStr := p.Str("age"); ageStr != "" {
		d, younger, err := parseAge(ageStr)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		ageCutoff = time.Now().Add(-d)
		ageYounger = younger
	}

	match := func(name string) (bool, error) {
		if len(patterns) == 0 {
			return true, nil
		}
		for _, pat := range patterns {
			if p.Bool("use_regex") {
				ok, err := regexp.MatchString(pat, name)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			} else if ok, _ := filepath.Match(pat, name); ok {
				return true, nil
			}
		}
		return false, nil
	}

	var files []any
	var examined int
	for _, root := range stringList(p.List("paths")) {
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // unreadable entries are skipped, like Ansible
			}
			if path == root {
				return nil
			}
			if !p.Bool("recurse") && info.IsDir() {
				// Still examine the dir itself below, but don't descend.
				defer func() {}()
			}
			examined++
			name := info.Name()
			if !p.Bool("hidden") && strings.HasPrefix(name, ".") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			switch fileType {
			case "file":
				if !info.Mode().IsRegular() {
					if info.IsDir() && !p.Bool("recurse") {
						return filepath.SkipDir
					}
					return nil
				}
			case "directory":
				if !info.IsDir() {
					return nil
				}
			case "link":
				if info.Mode()&os.ModeSymlink == 0 {
					return nil
				}
			}
			ok, err := match(name)
			if err != nil {
				return err
			}
			if !ok {
				return skipIfShallowDir(info, p.Bool("recurse"))
			}
			for _, ex := range excludes {
				if exOK, _ := filepath.Match(ex, name); exOK {
					return skipIfShallowDir(info, p.Bool("recurse"))
				}
			}
			if !ageCutoff.IsZero() {
				older := info.ModTime().Before(ageCutoff)
				if ageYounger == older {
					return skipIfShallowDir(info, p.Bool("recurse"))
				}
			}
			files = append(files, map[string]any{
				"path":  path,
				"size":  info.Size(),
				"mode":  fmt.Sprintf("0%o", info.Mode().Perm()),
				"isdir": info.IsDir(),
				"mtime": float64(info.ModTime().UnixNano()) / 1e9,
			})
			return skipIfShallowDir(info, p.Bool("recurse"))
		})
		if walkErr != nil {
			return agentproto.Fail("find: %v", walkErr)
		}
	}
	if files == nil {
		files = []any{}
	}
	return &agentproto.Result{Extra: map[string]any{
		"files":    files,
		"matched":  len(files),
		"examined": examined,
	}}
}

func skipIfShallowDir(info os.FileInfo, recurse bool) error {
	if info.IsDir() && !recurse {
		return filepath.SkipDir
	}
	return nil
}

// parseAge parses find's age values: "2d", "3h", "-30m" (negative =
// younger-than).
func parseAge(s string) (time.Duration, bool, error) {
	younger := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0, false, fmt.Errorf("invalid age %q", s)
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	mult := time.Second
	switch unit {
	case 's':
		mult = time.Second
	case 'm':
		mult = time.Minute
	case 'h':
		mult = time.Hour
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	default:
		numStr = s // bare number = seconds
	}
	var n int64
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
		return 0, false, fmt.Errorf("invalid age %q", s)
	}
	return time.Duration(n) * mult, younger, nil
}

var modprobeSpec = args.Spec{
	"name":   {Required: true},
	"state":  {Default: "present", Choices: []string{"present", "absent"}},
	"params": {},
}

// modprobeModule loads/unloads kernel modules, checking /proc/modules.
func modprobeModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := modprobeSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	loaded := moduleLoaded(name)
	res := &agentproto.Result{Extra: map[string]any{"name": name}}

	if p.Str("state") == "present" {
		if loaded {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		argv := []string{name}
		if params := p.Str("params"); params != "" {
			argv = append(argv, strings.Fields(params)...)
		}
		if out, err := runOut(env, "modprobe", argv...); err != nil {
			return agentproto.Fail("modprobe %s failed: %v: %s", name, err, tail(out))
		}
		return res
	}
	// absent
	if !loaded {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if out, err := runOut(env, "modprobe", "-r", name); err != nil {
		return agentproto.Fail("modprobe -r %s failed: %v: %s", name, err, tail(out))
	}
	return res
}

func moduleLoaded(name string) bool {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	normalized := strings.ReplaceAll(name, "-", "_")
	for _, line := range strings.Split(string(data), "\n") {
		if mod, _, ok := strings.Cut(line, " "); ok &&
			strings.ReplaceAll(mod, "-", "_") == normalized {
			return true
		}
	}
	return false
}
