package modules

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

func init() {
	Register(tempfileModule, "tempfile", "ansible.builtin.tempfile")
	Register(findModule, "find", "ansible.builtin.find")
}

var tempfileSpec = args.Spec{
	"state":  {Default: "file", Choices: []string{"file", "directory"}},
	"path":   {},
	"prefix": {Default: "ansible."},
	"suffix": {Default: ""},
}

// pyTempName is tempfile's random name: 8 characters of [a-z0-9_].
func pyTempName() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789_"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// pyGettempdir is tempfile.gettempdir().
func pyGettempdir() string {
	for _, env := range []string{"TMPDIR", "TEMP", "TMP"} {
		if d := os.Getenv(env); d != "" && isDir(d) {
			abs, _ := filepath.Abs(d)
			return abs
		}
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/usr/tmp"} {
		if isDir(d) {
			return d
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// pyStrOSError renders an OSError raised for a str path:
// "[Errno 2] No such file or directory: '/x'".
func pyStrOSError(err error, path string) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	return fmt.Sprintf("[Errno %d] %s: %s", int(errno), pyStrerror(errno), pyStrRepr(path))
}

// tempfileModule ports ansible.builtin.tempfile (mkstemp / mkdtemp). The
// module does not support check mode, so --check skips it.
func tempfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := tempfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (tempfile) does not support check mode"}
	}
	dir := pyGettempdir()
	if p.Has("path") {
		dir = pyExpandPath(p.Str("path"))
	}
	for attempt := 0; attempt < 100; attempt++ {
		path := pyJoin(dir, p.Str("prefix")+pyTempName()+p.Str("suffix"))
		var err error
		if p.Str("state") == "directory" {
			err = os.Mkdir(path, 0o700)
		} else {
			var f *os.File
			if f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
				f.Close()
			}
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return agentproto.Fail("%s", pyStrOSError(err, path))
		}
		return &agentproto.Result{Changed: true, Extra: map[string]any{"path": path}}
	}
	return agentproto.Fail("[Errno 17] No usable temporary file name found")
}

var findSpec = args.Spec{
	"paths":              {Type: "list", Required: true, Aliases: []string{"name", "path"}},
	"patterns":           {Type: "list", Aliases: []string{"pattern"}},
	"excludes":           {Type: "list", Aliases: []string{"exclude"}},
	"contains":           {},
	"read_whole_file":    {Type: "bool", Default: false},
	"file_type":          {Default: "file", Choices: []string{"any", "directory", "file", "link"}},
	"age":                {},
	"age_stamp":          {Default: "mtime", Choices: []string{"atime", "ctime", "mtime"}},
	"size":               {},
	"recurse":            {Type: "bool", Default: false},
	"hidden":             {Type: "bool", Default: false},
	"follow":             {Type: "bool", Default: false},
	"get_checksum":       {Type: "bool", Default: false},
	"checksum_algorithm": {Default: "sha1", Aliases: []string{"checksum", "checksum_algo"}, Choices: []string{"md5", "sha1", "sha224", "sha256", "sha384", "sha512"}},
	"use_regex":          {Type: "bool", Default: false},
	"depth":              {Type: "int"},
	"mode":               {Type: "any"},
	"exact_mode":         {Type: "bool", Default: true},
	"encoding":           {},
	"limit":              {Type: "int"},
}

// findFilter is pfilter: the name matches a pattern and no exclude.
type findFilter struct {
	patterns, excludes []*pyre.Pattern
}

func (f *findFilter) match(name string) bool {
	for _, p := range f.patterns {
		if p.Match(name, 0, -1) != nil {
			for _, e := range f.excludes {
				if e.Match(name, 0, -1) != nil {
					return false
				}
			}
			return true
		}
	}
	return false
}

// findModule ports ansible.builtin.find (os.walk order, filters, result
// shape and skipped-path reporting).
func findModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := findSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	var mode string
	if p.Has("mode") {
		s, ok := p.Any("mode").(string)
		if !ok {
			return agentproto.Fail("argument 'mode' is not a string and conversion is not allowed, value is of type %s", pyTypeName(p.Any("mode")))
		}
		mode = s
	}
	useRegex := p.Bool("use_regex")
	// re.compile(p) and .match, or fnmatch.fnmatch.
	compile := func(list []any) ([]*pyre.Pattern, *agentproto.Result) {
		var out []*pyre.Pattern
		for _, v := range list {
			s := fmt.Sprint(v)
			if !useRegex {
				s = pyre.FnmatchTranslate(s)
			}
			re, fail := pyCompile(s)
			if fail != nil {
				return nil, fail
			}
			out = append(out, re)
		}
		return out, nil
	}
	patterns := p.List("patterns")
	if len(patterns) == 0 {
		if useRegex {
			patterns = []any{".*"}
		} else {
			patterns = []any{"*"}
		}
	}
	filter := &findFilter{}
	var fail *agentproto.Result
	if filter.patterns, fail = compile(patterns); fail != nil {
		return fail
	}
	if filter.excludes, fail = compile(p.List("excludes")); fail != nil {
		return fail
	}

	var age *float64
	if p.Has("age") {
		m := regexp.MustCompile(`^(-?\d+)(s|m|h|d|w)?$`).FindStringSubmatch(strings.ToLower(p.Str("age")))
		if m == nil {
			return &agentproto.Result{Failed: true, Msg: "failed to process age", Extra: map[string]any{"age": p.Str("age")}}
		}
		n, _ := strconv.ParseFloat(m[1], 64)
		mult := map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800, "": 1}[m[2]]
		v := n * mult
		age = &v
	}
	var size *int64
	if p.Has("size") {
		m := regexp.MustCompile(`^(-?\d+)(b|k|m|g|t)?$`).FindStringSubmatch(strings.ToLower(p.Str("size")))
		if m == nil {
			return &agentproto.Result{Failed: true, Msg: "failed to process size", Extra: map[string]any{"size": p.Str("size")}}
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		mult := map[string]int64{"b": 1, "k": 1024, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40, "": 1}[m[2]]
		v := n * mult
		size = &v
	}
	limit := int64(-1)
	if p.Has("limit") {
		limit = p.Int("limit")
		if limit <= 0 {
			return agentproto.Fail("limit cannot be %d (use None for unlimited)", limit)
		}
	}
	var contains *pyre.Pattern
	if p.Has("contains") {
		var fail *agentproto.Result
		if contains, fail = pyCompile(p.Str("contains")); fail != nil {
			return fail
		}
	}

	now := float64(time.Now().UnixNano()) / 1e9
	msg := "All paths examined"
	looked := 0
	hasWarnings := false
	var warnings []any
	files := []any{}
	skipped := map[string]any{}
	fileType := p.Str("file_type")
	depthLimit := p.Int("depth")

	ageOK := func(info os.FileInfo) bool {
		if age == nil {
			return true
		}
		mtime, atime := statTimes(info)
		ts := map[string]float64{"mtime": mtime, "atime": atime, "ctime": statCtime(info)}[p.Str("age_stamp")]
		if *age >= 0 {
			return now-ts >= *age
		}
		return now-ts <= -*age
	}
	sizeOK := func(info os.FileInfo) bool {
		if size == nil {
			return true
		}
		if *size >= 0 {
			return info.Size() >= *size
		}
		return info.Size() <= -*size
	}
	modeOK := func(info os.FileInfo) bool {
		if mode == "" {
			return true
		}
		want, ok := pyIntBase(mode, 8)
		if !ok {
			sym, err := fsutil.SymbolicToOctal(mode, 0, false)
			if err != nil {
				return false
			}
			want = int64(sym)
		}
		want &= 0o7777
		cur := int64(sIMode(info.Mode()))
		if p.Bool("exact_mode") {
			return cur == want
		}
		return cur&want != 0
	}
	containsOK := func(path string) bool {
		if contains == nil {
			return true
		}
		lines, err := readLines(path)
		if err != nil {
			return false
		}
		if p.Bool("read_whole_file") {
			return contains.Search(strings.Join(lines, ""), 0, -1) != nil
		}
		for _, l := range lines {
			if contains.Match(l, 0, -1) != nil {
				return true
			}
		}
		return false
	}

	for _, raw := range p.List("paths") {
		npath := pyExpandPath(fmt.Sprint(raw))
		if !isDir(npath) {
			skipped[npath] = fmt.Sprintf("'%s' is not a directory", npath)
			warnings = append(warnings, fmt.Sprintf("Skipped '%s' path due to this access issue: %s\n", npath, skipped[npath]))
			hasWarnings = true
			continue
		}
		stop := false
		var walk func(root string)
		walk = func(root string) {
			fh, err := os.Open(root)
			if err != nil {
				if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
					skipped[root] = pyStrOSError(err, root)
				}
				return
			}
			entries, err := fh.ReadDir(-1) // directory order, like os.scandir
			fh.Close()
			if err != nil {
				skipped[root] = pyStrOSError(err, root)
				return
			}
			var fileNames, dirNames []string
			for _, e := range entries {
				full := filepath.Join(root, e.Name())
				if info, err := os.Stat(full); err == nil && info.IsDir() {
					dirNames = append(dirNames, e.Name())
				} else {
					fileNames = append(fileNames, e.Name())
				}
			}
			looked += len(fileNames) + len(dirNames)
			var descend []string
			for idx, name := range append(append([]string{}, fileNames...), dirNames...) {
				isDirEntry := idx >= len(fileNames)
				fsname := filepath.Clean(filepath.Join(root, name))
				if depthLimit != 0 {
					wpath := strings.TrimRight(npath, "/") + "/"
					depth := strings.Count(fsname, "/") - strings.Count(wpath, "/") + 1
					if int64(depth) > depthLimit {
						continue
					}
				}
				if isDirEntry {
					descend = append(descend, fsname)
				}
				if strings.HasPrefix(name, ".") && !p.Bool("hidden") {
					continue
				}
				info, err := os.Lstat(fsname)
				if err != nil {
					skipped[fsname] = pyStrOSError(err, fsname)
					hasWarnings = true
					continue
				}
				r := map[string]any{"path": fsname}
				m := info.Mode()
				switch {
				case fileType == "any":
					if filter.match(name) && ageOK(info) && modeOK(info) {
						findStatinfo(r, info)
						if m.IsRegular() && p.Bool("get_checksum") {
							r["checksum"], _ = digestFile(fsname, newHash(p.Str("checksum_algorithm")))
						}
						if !m.IsRegular() || sizeOK(info) {
							files = append(files, r)
						}
					}
				case m.IsDir() && fileType == "directory":
					if filter.match(name) && ageOK(info) && modeOK(info) {
						findStatinfo(r, info)
						files = append(files, r)
					}
				case m.IsRegular() && fileType == "file":
					if filter.match(name) && ageOK(info) && sizeOK(info) && containsOK(fsname) && modeOK(info) {
						findStatinfo(r, info)
						if p.Bool("get_checksum") {
							r["checksum"], _ = digestFile(fsname, newHash(p.Str("checksum_algorithm")))
						}
						files = append(files, r)
					}
				case m&os.ModeSymlink != 0 && fileType == "link":
					if filter.match(name) && ageOK(info) && modeOK(info) {
						findStatinfo(r, info)
						files = append(files, r)
					}
				}
				if int64(len(files)) == limit {
					msg = "Limit of matches reached"
					break
				}
			}
			if !p.Bool("recurse") || int64(len(files)) == limit {
				stop = true
				return
			}
			for _, d := range descend {
				if stop {
					return
				}
				if !p.Bool("follow") {
					if info, err := os.Lstat(d); err == nil && info.Mode()&os.ModeSymlink != 0 {
						continue
					}
				}
				walk(d)
			}
		}
		walk(npath)
	}
	if hasWarnings {
		msg = "Not all paths examined, check warnings for details"
	}
	res := &agentproto.Result{Msg: msg, Extra: map[string]any{
		"files": files, "matched": int64(len(files)), "examined": int64(looked), "skipped_paths": skipped,
	}}
	if len(warnings) > 0 {
		res.Extra["warnings"] = warnings
	}
	return res
}

// findStatinfo is the find module's statinfo().
func findStatinfo(r map[string]any, info os.FileInfo) {
	mode := info.Mode()
	bits := sIMode(mode)
	uid, gid, _ := statIDsOf(info)
	mtime, atime := statTimes(info)
	pw, gr := userName(uid), groupName(gid)
	if pw == fmt.Sprint(uid) {
		pw = ""
	}
	if gr == fmt.Sprint(gid) {
		gr = ""
	}
	for k, v := range map[string]any{
		"mode":  fmt.Sprintf("%04o", bits),
		"isdir": mode.IsDir(), "ischr": mode&os.ModeCharDevice != 0,
		"isblk":  mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0,
		"isreg":  mode.IsRegular(),
		"isfifo": mode&os.ModeNamedPipe != 0, "islnk": mode&os.ModeSymlink != 0,
		"issock": mode&os.ModeSocket != 0,
		"uid":    int64(uid), "gid": int64(gid), "size": info.Size(),
		"inode": statInode(info), "dev": statDev(info), "nlink": int64(nlinkOf(info)),
		"atime": atime, "mtime": mtime, "ctime": statCtime(info),
		"gr_name": gr, "pw_name": pw,
		"wusr": bits&0o200 != 0, "rusr": bits&0o400 != 0, "xusr": bits&0o100 != 0,
		"wgrp": bits&0o020 != 0, "rgrp": bits&0o040 != 0, "xgrp": bits&0o010 != 0,
		"woth": bits&0o002 != 0, "roth": bits&0o004 != 0, "xoth": bits&0o001 != 0,
		"isuid": bits&0o4000 != 0, "isgid": bits&0o2000 != 0,
	} {
		r[k] = v
	}
	if pl := statPlatform(info); pl != nil {
		r["blocks"] = pl["blocks"]
		r["disk_usage_bytes"] = pl["disk_usage_bytes"]
	}
}

// pyTypeName is type(v).__name__ for decoded YAML/JSON values.
func pyTypeName(v any) string {
	switch v.(type) {
	case int, int64:
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	case nil:
		return "NoneType"
	}
	return "str"
}
