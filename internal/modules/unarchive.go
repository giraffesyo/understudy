package modules

import (
	"archive/zip"
	"crypto/tls"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports ansible.builtin.unarchive (the module half). The
// control-side action (internal/actions/unarchive.go) finds a local src,
// transfers it as the payload and marks the request with
// unarchiveActionKey; with remote_src the archive is already on the target.

func init() {
	names := []string{"unarchive", "ansible.builtin.unarchive"}
	Register(unarchiveModule, names...)
	for _, n := range names {
		specs[n] = unarchiveSpec
	}
}

var unarchiveSpec = args.Spec{
	"src":            {Required: true},
	"dest":           {Required: true},
	"remote_src":     {Type: "bool", Default: false},
	"creates":        {},
	"list_files":     {Type: "bool", Default: false},
	"keep_newer":     {Type: "bool", Default: false},
	"exclude":        {Type: "list", Default: []any{}},
	"include":        {Type: "list", Default: []any{}},
	"extra_opts":     {Type: "list", Default: []any{}},
	"validate_certs": {Type: "bool", Default: true},
	"io_buffer_size": {Type: "int", Default: 64 * 1024},
	"copy":           {Type: "bool", Default: true},
	"decrypt":        {Type: "bool", Default: true},
	// add_file_common_args
	"mode":          {Type: "any"},
	"owner":         {},
	"group":         {},
	"seuser":        {},
	"serole":        {},
	"setype":        {},
	"selevel":       {},
	"attributes":    {Aliases: []string{"attr"}},
	"unsafe_writes": {Type: "bool", Default: false},
}

// unarchiveActionKey marks a request from the control-side action: the
// action's remote checks (dest must be a directory) run here, and a local
// src arrives as the payload.
const unarchiveActionKey = "_unarchive_action"

var (
	tarOwnerDiffRE   = regexp.MustCompile(`: Uid differs$`)
	tarGroupDiffRE   = regexp.MustCompile(`: Gid differs$`)
	tarModeDiffRE    = regexp.MustCompile(`: Mode differs$`)
	tarMtimeDiffRE   = regexp.MustCompile(`: Mod time differs$`)
	tarEmptyFileRE   = regexp.MustCompile(`: : Warning: Cannot stat: No such file or directory$`)
	tarMissingFileRE = regexp.MustCompile(`: Warning: Cannot stat: No such file or directory$`)
	zipFileModeRE    = regexp.MustCompile(`^([r-][w-][SsTtx-]){3}`)
	tarInvalidOwner  = regexp.MustCompile(`: Invalid owner`)
	tarInvalidGroup  = regexp.MustCompile(`: Invalid group`)
	tarSymlinkDiffRE = regexp.MustCompile(`: Symlink differs$`)
	tarContentDiffRE = regexp.MustCompile(`: Contents differ$`)
	tarSizeDiffRE    = regexp.MustCompile(`: Size differs$`)
	zipDateRE        = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})\.(\d{2})(\d{2})(\d{2})$`)
)

type unarchiveFileArgs struct {
	mode         any
	owner, group string
}

type unarchiveCheck struct {
	unarchived bool
	rc         int
	out, err   string
	cmd        []string
	diff       string
}

type archiveHandler interface {
	name() string
	canHandle() (bool, string)
	filesInArchive() ([]string, error)
	isUnarchived() (*unarchiveCheck, *agentproto.Result)
	unarchive() map[string]any
}

type unarchiveRun struct {
	env      *RunEnv
	p        *args.Parsed
	src      string
	bDest    string
	fa       unarchiveFileArgs
	excludes []string
	includes []string
	opts     []string
}

func unarchiveModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	action, fromAction := rawArgs[unarchiveActionKey].(map[string]any)
	user := make(map[string]any, len(rawArgs))
	for k, v := range rawArgs {
		if k != unarchiveActionKey {
			user[k] = v
		}
	}
	var stageDir string
	defer func() {
		if stageDir != "" {
			os.RemoveAll(stageDir)
		}
	}()
	if fromAction {
		// ActionModule.run's remote half: dest must be an existing
		// directory, then the transferred file lands in the remote tmp.
		dest, _ := argString(user, "dest")
		dest = pyExpandUser(dest)
		if st, err := os.Stat(dest); err != nil || !st.IsDir() {
			res := agentproto.Fail("dest '%s' must be an existing dir", dest)
			res.Origin = "raised"
			return res
		}
		user["dest"] = dest
		if transfer, _ := action["transfer"].(bool); transfer {
			dir, src, err := stageStream(env.Payload, "source")
			if err != nil {
				return agentproto.Fail("staging the source file: %v", err)
			}
			stageDir = dir
			user["src"] = src
		}
	}
	res, tmp := unarchiveCore(env, user)
	if tmp != "" {
		os.Remove(tmp)
	}
	addPathInfo(res)
	return res
}

// stageStream writes the payload to <tmp>/ansible-tmp-.../<name>.
func stageStream(r io.Reader, name string) (string, string, error) {
	dname := fmt.Sprintf("ansible-tmp-%s-%d-%d", pyFloat(float64(time.Now().UnixNano())/1e9), os.Getpid(), rand.Int63n(1<<48))
	dir := filepath.Join(os.TempDir(), dname)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", "", err
	}
	src := filepath.Join(dir, name)
	f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	if r != nil {
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			os.RemoveAll(dir)
			return "", "", err
		}
	}
	if err := f.Close(); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return dir, src, nil
}

func unarchiveCore(env *RunEnv, rawArgs map[string]any) (*agentproto.Result, string) {
	p, err := unarchiveSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err), ""
	}
	if err := unarchiveSpec.MutuallyExclusive(rawArgs, []string{"include", "exclude"}); err != nil {
		return agentproto.Fail("%v", err), ""
	}
	src := pyExpandPath(p.Str("src"))
	dest := pyExpandPath(p.Str("dest"))
	absDest, _ := filepath.Abs(dest)
	var warnings []any
	if !filepath.IsAbs(dest) {
		warnings = append(warnings, fmt.Sprintf("Relative destination path '%s' was resolved to absolute path '%s'.", dest, absDest))
	}
	fail := func(msg string, extra map[string]any) *agentproto.Result {
		r := agentproto.Fail("%s", msg)
		r.Extra = extra
		if len(warnings) > 0 {
			if r.Extra == nil {
				r.Extra = map[string]any{}
			}
			r.Extra["warnings"] = warnings
		}
		return r
	}
	remoteSrc := p.Bool("remote_src")
	fa := unarchiveFileArgs{mode: nil}
	if p.Has("mode") {
		fa.mode = p.Any("mode")
	}
	fa.owner, fa.group = p.Str("owner"), p.Str("group")

	tmpFile := ""
	if _, err := os.Stat(src); err != nil {
		switch {
		case !remoteSrc:
			return fail(fmt.Sprintf("Source '%s' failed to transfer", src), nil), ""
		case strings.Contains(src, "://"):
			path, msg := unarchiveFetch(p, src)
			if msg != "" {
				return fail(msg, nil), ""
			}
			src, tmpFile = path, path
		default:
			return fail(fmt.Sprintf("Source '%s' does not exist", src), nil), ""
		}
	}
	if !accessOK(src, 4) {
		return fail(fmt.Sprintf("Source '%s' not readable", src), nil), tmpFile
	}
	src, _ = filepath.Abs(src)
	if st, err := os.Stat(src); err != nil {
		return fail(fmt.Sprintf("Source '%s' not readable, %s", src, pyOSError(err)), nil), tmpFile
	} else if st.Size() == 0 {
		return fail(fmt.Sprintf("Invalid archive '%s', the file is 0 bytes", src), nil), tmpFile
	}
	if st, err := os.Stat(absDest); err != nil || !st.IsDir() {
		return fail(fmt.Sprintf("Destination '%s' is not a directory", dest), nil), tmpFile
	}

	u := &unarchiveRun{env: env, p: p, src: src, bDest: absDest, fa: fa,
		includes: strList(p.List("include")), opts: strList(p.List("extra_opts"))}
	u.excludes = strList(p.List("exclude"))

	handler, res := u.pickHandler()
	if res != nil {
		if len(warnings) > 0 {
			if res.Extra == nil {
				res.Extra = map[string]any{}
			}
			res.Extra["warnings"] = warnings
		}
		return res, tmpFile
	}
	out := map[string]any{"handler": handler.name(), "dest": dest, "src": src}
	result := &agentproto.Result{Extra: out}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	check, cfail := handler.isUnarchived()
	if cfail != nil {
		return cfail, tmpFile
	}
	switch {
	case env.CheckMode:
		result.Changed = !check.unarchived
	case check.unarchived:
	default:
		er := handler.unarchive()
		out["extract_results"] = er
		if rc, _ := er["rc"].(int); rc != 0 {
			r := fail(fmt.Sprintf("failed to unpack %s to %s", src, dest), out)
			return r, tmpFile
		}
		result.Changed = true
	}
	if check.diff != "" {
		result.Diff = map[string]any{"prepared": check.diff}
	}
	if !env.CheckMode {
		files, _ := handler.filesInArchive()
		var top []string
		seen := map[string]bool{}
		fattrs := fileAttrs{Mode: fa.mode}
		if p.Has("owner") {
			s := fa.owner
			fattrs.Owner = &s
		}
		if p.Has("group") {
			s := fa.group
			fattrs.Group = &s
		}
		apply := func(path string) *agentproto.Result {
			if _, err := os.Lstat(path); err != nil {
				r := fail("Unexpected error when accessing exploded file.", out)
				r.Cause = pyOSError(err)
				return r
			}
			fattrs.Path = path
			ch, f := setFSAttrs(env, fattrs, result.Changed, nil)
			if f != nil {
				return f
			}
			result.Changed = ch
			return nil
		}
		for _, f := range files {
			if r := apply(pyJoin(absDest, f)); r != nil {
				return r, tmpFile
			}
			if i := strings.Index(f, "/"); i >= 0 {
				t := f[:i]
				if !seen[t] {
					seen[t] = true
					top = append(top, t)
				}
			}
		}
		for _, t := range top {
			if r := apply(dest + "/" + t); r != nil {
				return r, tmpFile
			}
		}
	}
	if p.Bool("list_files") {
		files, _ := handler.filesInArchive()
		out["files"] = anyList(files)
	}
	return result, tmpFile
}

// unarchiveFetch is fetch_file: download to a temp file named after the
// URL's basename.
func unarchiveFetch(p *args.Parsed, rawURL string) (string, string) {
	parts, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Sprintf("Failure downloading %s, %v", rawURL, err)
	}
	prefix, ext := splitMultiExt(filepath.Base(parts.Path), 2)
	f, err := os.CreateTemp("", prefix+"*"+ext)
	if err != nil {
		return "", fmt.Sprintf("Failure downloading %s, %v", rawURL, err)
	}
	defer f.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	if !p.Bool("validate_certs") {
		client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Sprintf("Failure downloading %s, %s", rawURL, urlErrorMsg(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		os.Remove(f.Name())
		return "", fmt.Sprintf("Failure downloading %s, HTTP Error %d: %s", rawURL, resp.StatusCode, httpReason(resp))
	}
	client.Timeout = 0
	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", fmt.Sprintf("Failure downloading %s, %v", rawURL, err)
	}
	return f.Name(), ""
}

// splitMultiExt is urls._split_multiext.
func splitMultiExt(name string, count int) (string, string) {
	const minLen, maxLen = 3, 5
	for i := 0; i < count; i++ {
		ext := filepath.Ext(name)
		if len(ext) < minLen || len(ext) > maxLen+1 || ext == name {
			break
		}
		root := strings.TrimSuffix(name, ext)
		r2, e2 := splitMultiExt(root, count-1-i)
		return r2, e2 + ext
	}
	return name, ""
}

func (u *unarchiveRun) pickHandler() (archiveHandler, *agentproto.Result) {
	var reasons []string
	seen := map[string]bool{}
	mk := []func() archiveHandler{
		func() archiveHandler { return u.newZip(false) },
		func() archiveHandler { return u.newZip(true) },
		func() archiveHandler { return u.newTar("TgzArchive", "-z") },
		func() archiveHandler { return u.newTar("TarArchive", "") },
		func() archiveHandler { return u.newTar("TarBzipArchive", "-j") },
		func() archiveHandler { return u.newTar("TarXzArchive", "-J") },
		func() archiveHandler { return u.newTar("TarZstdArchive", "--use-compress-program=zstd") },
	}
	for i, f := range mk {
		if i == 2 && u.env.CheckMode {
			// TgzArchive.__init__ exits in check mode.
			return nil, &agentproto.Result{Skipped: true,
				Msg: "remote module (unarchive) does not support check mode when using gtar"}
		}
		h := f()
		ok, reason := h.canHandle()
		if ok {
			return h, nil
		}
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}
	return nil, agentproto.Fail("Failed to find handler for \"%s\". Make sure the required command to extract the file is installed.\n%s",
		u.src, strings.Join(reasons, "\n"))
}

// ---- tar ----

type tarHandler struct {
	*unarchiveRun
	cls      string
	zipflag  string
	cmdPath  string
	excl     []string
	files    []string
	haveList bool
}

func (u *unarchiveRun) newTar(cls, flag string) *tarHandler {
	t := &tarHandler{unarchiveRun: u, cls: cls, zipflag: flag}
	for _, e := range u.excludes {
		t.excl = append(t.excl, strings.TrimRight(e, "/"))
	}
	return t
}

func (t *tarHandler) name() string { return t.cls }

func (t *tarHandler) baseCmd(verb string) []string {
	cmd := []string{t.cmdPath, verb, "-C", t.bDest}
	if t.zipflag != "" {
		cmd = append(cmd, t.zipflag)
	}
	if len(t.opts) > 0 {
		cmd = append(cmd, "--show-transformed-names")
		cmd = append(cmd, t.opts...)
	}
	return cmd
}

func (t *tarHandler) ownerOpts(cmd []string) []string {
	if t.fa.owner != "" {
		cmd = append(cmd, "--owner="+shQuote(t.fa.owner))
	}
	if t.fa.group != "" {
		cmd = append(cmd, "--group="+shQuote(t.fa.group))
	}
	if t.p.Bool("keep_newer") {
		cmd = append(cmd, "--keep-newer-files")
	}
	return cmd
}

func (t *tarHandler) tail(cmd []string) []string {
	for _, e := range t.excl {
		cmd = append(cmd, "--exclude="+e)
	}
	cmd = append(cmd, "-f", t.src)
	return append(cmd, t.includes...)
}

func (t *tarHandler) run(cmd []string) (int, string, string) {
	loc := bestParsableLocale(t.env)
	return runCommand(t.env, cmd, cmdOpts{Cwd: t.bDest, Env: localeEnv(loc)})
}

func (t *tarHandler) filesInArchive() ([]string, error) {
	if t.haveList && len(t.files) > 0 {
		return t.files, nil
	}
	cmd := t.tail(t.baseCmd("--list"))
	rc, out, errOut := t.run(cmd)
	if rc != 0 {
		return nil, fmt.Errorf("Unable to list files in the archive: %s", errOut)
	}
	t.files = nil
	for _, name := range splitLines(out) {
		name = pyEscapeDecode(name)
		name = strings.TrimPrefix(name, "/")
		excluded := false
		for _, e := range t.excl {
			if pyFnmatch(e).MatchString(name) {
				excluded = true
				break
			}
		}
		if !excluded {
			t.files = append(t.files, name)
		}
	}
	t.haveList = true
	return t.files, nil
}

func (t *tarHandler) isUnarchived() (*unarchiveCheck, *agentproto.Result) {
	cmd := t.tail(t.ownerOpts(t.baseCmd("--diff")))
	rc, out, errOut := t.run(cmd)
	runUID := os.Getuid()
	var b strings.Builder
	for _, line := range append(splitLines(out), splitLines(errOut)...) {
		if tarEmptyFileRE.MatchString(line) {
			continue
		}
		if runUID == 0 && t.fa.owner == "" && tarOwnerDiffRE.MatchString(line) {
			b.WriteString(line + "\n")
		}
		if runUID == 0 && t.fa.group == "" && tarGroupDiffRE.MatchString(line) {
			b.WriteString(line + "\n")
		}
		if !modeGiven(t.fa.mode) && tarModeDiffRE.MatchString(line) {
			b.WriteString(line + "\n")
		}
		for _, re := range []*regexp.Regexp{tarMtimeDiffRE, tarMissingFileRE, tarInvalidOwner,
			tarInvalidGroup, tarSymlinkDiffRE, tarContentDiffRE, tarSizeDiffRE} {
			if re.MatchString(line) {
				b.WriteString(line + "\n")
			}
		}
	}
	o := b.String()
	return &unarchiveCheck{unarchived: o == "", rc: rc, out: o, err: errOut, cmd: cmd}, nil
}

func (t *tarHandler) unarchive() map[string]any {
	cmd := t.tail(t.ownerOpts(t.baseCmd("--extract")))
	rc, out, errOut := t.run(cmd)
	return map[string]any{"cmd": anyList(cmd), "rc": rc, "out": out, "err": errOut}
}

func (t *tarHandler) canHandle() (bool, string) {
	if p, err := lookPath("gtar"); err == nil {
		t.cmdPath = p
	} else if p, err := lookPath("tar"); err == nil {
		t.cmdPath = p
	} else {
		return false, "Unable to find required 'gtar' or 'tar' binary in the path"
	}
	_, out, _ := runCommand(t.env, []string{t.cmdPath, "--version"}, cmdOpts{})
	tarType := "None"
	if strings.HasPrefix(out, "bsdtar") {
		tarType = "bsd"
	} else if strings.HasPrefix(out, "tar") && strings.Contains(out, "GNU") {
		tarType = "gnu"
	}
	if tarType != "gnu" {
		return false, fmt.Sprintf("Command \"%s\" detected as tar type %s. GNU tar required.", t.cmdPath, tarType)
	}
	files, err := t.filesInArchive()
	if err != nil {
		return false, fmt.Sprintf("Command \"%s\" could not handle archive: %v", t.cmdPath, err)
	}
	if len(files) > 0 {
		return true, ""
	}
	return false, fmt.Sprintf("Command \"%s\" found no files in archive. Empty archive files are not supported.", t.cmdPath)
}

// modeGiven is the truthiness of file_args['mode'].
func modeGiven(m any) bool {
	switch t := m.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	}
	return true
}

// shQuote is shlex.quote.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./-_", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// pyEscapeDecode is codecs.escape_decode for GNU tar's octal-escaped names.
func pyEscapeDecode(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b = append(b, c)
			continue
		}
		i++
		switch n := s[i]; n {
		case '\\':
			b = append(b, '\\')
		case 'n':
			b = append(b, '\n')
		case 't':
			b = append(b, '\t')
		case 'r':
			b = append(b, '\r')
		case 'a':
			b = append(b, 7)
		case 'b':
			b = append(b, 8)
		case 'f':
			b = append(b, 12)
		case 'v':
			b = append(b, 11)
		case '\'', '"':
			b = append(b, n)
		default:
			if n >= '0' && n <= '7' {
				j := i
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					j++
				}
				v, _ := strconv.ParseUint(s[i:j], 8, 16)
				b = append(b, byte(v))
				i = j - 1
			} else {
				b = append(b, '\\', n)
			}
		}
	}
	return string(b)
}

// ---- zip ----

type zipHandler struct {
	*unarchiveRun
	z           bool // ZipZArchive: unzip -Z instead of zipinfo
	cmdPath     string
	zipinfoPath string
	files       []string
	haveList    bool
	crcs        map[string]uint32
	changed     []string
}

func (u *unarchiveRun) newZip(z bool) *zipHandler { return &zipHandler{unarchiveRun: u, z: z} }

func (z *zipHandler) name() string {
	if z.z {
		return "ZipZArchive"
	}
	return "ZipArchive"
}

func (z *zipHandler) canHandle() (bool, string) {
	bins := [][2]string{{"unzip", "cmd"}, {"zipinfo", "info"}}
	if z.z {
		bins[1][0] = "unzip"
	}
	var missing []string
	for _, b := range bins {
		p, err := lookPath(b[0])
		if err != nil {
			missing = append(missing, b[0])
			continue
		}
		if b[1] == "cmd" {
			z.cmdPath = p
		} else {
			z.zipinfoPath = p
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("Unable to find required '%s' binary in the path.", strings.Join(missing, "' or '"))
	}
	rc, _, errOut := runCommand(z.env, []string{z.cmdPath, "-l", z.src}, cmdOpts{})
	if rc != 0 {
		return false, fmt.Sprintf("Command \"%s\" could not handle archive: %s", z.cmdPath, errOut)
	}
	if !z.z {
		return true, ""
	}
	_, out, errOut := runCommand(z.env, []string{z.zipinfoPath, "-Zl"}, cmdOpts{})
	if strings.Contains(strings.ToLower(out), "zipinfo") {
		return true, ""
	}
	return false, "Command \"unzip -Z\" could not handle archive: " + errOut
}

func (z *zipHandler) readZip() error {
	r, err := zip.OpenReader(z.src)
	if err != nil {
		return err
	}
	defer r.Close()
	z.crcs = map[string]uint32{}
	z.files = nil
	for _, f := range r.File {
		z.crcs[f.Name] = f.CRC32
		member := f.Name
		if len(z.includes) > 0 {
			for _, inc := range z.includes {
				if pyFnmatch(inc).MatchString(member) {
					z.files = append(z.files, member)
				}
			}
			continue
		}
		excluded := false
		for _, e := range z.excludes {
			if pyFnmatch(e).MatchString(member) {
				excluded = true
				break
			}
		}
		if !excluded {
			z.files = append(z.files, member)
		}
	}
	z.haveList = true
	return nil
}

func (z *zipHandler) filesInArchive() ([]string, error) {
	if z.haveList && len(z.files) > 0 {
		return z.files, nil
	}
	if err := z.readZip(); err != nil {
		return nil, fmt.Errorf("Unable to list files in the archive: %v", err)
	}
	return z.files, nil
}

func (z *zipHandler) crc(path string) uint32 {
	if z.crcs == nil {
		_ = z.readZip()
	}
	return z.crcs[path]
}

func permstrToOctal(s string, umask uint32) uint32 {
	var mode uint32
	for j := 0; j < 3; j++ {
		for i := 0; i < 3; i++ {
			c := s[len(s)-1-(i+3*j)]
			if strings.IndexByte("rwxst", c) >= 0 {
				mode += 1 << (i + 3*j)
			}
		}
	}
	return mode &^ umask
}

func zipTimestamp(s string) float64 {
	m := zipDateRE.FindStringSubmatch(s)
	epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.Local)
	if m == nil {
		return float64(epoch.Unix())
	}
	n := make([]int, 6)
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	var t time.Time
	switch {
	case n[0] < 1980:
		t = epoch
	case n[0] > 2107:
		t = time.Date(2107, 12, 31, 23, 59, 59, 0, time.Local)
	default:
		t = time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.Local)
	}
	return float64(t.Unix())
}

func (z *zipHandler) isUnarchived() (*unarchiveCheck, *agentproto.Result) {
	cmd := []string{z.zipinfoPath}
	if z.z {
		cmd = append(cmd, "-Zl")
	}
	cmd = append(cmd, "-T", "-s", z.src)
	if len(z.excludes) > 0 {
		cmd = append(cmd, "-x")
		cmd = append(cmd, z.excludes...)
	}
	cmd = append(cmd, z.includes...)
	rc, oldOut, errOut := runCommand(z.env, cmd, cmdOpts{})
	unarchived := rc == 0
	var diff, out strings.Builder
	errB := &strings.Builder{}
	errB.WriteString(errOut)

	umask := fsutil.Umask()
	runUID, runGID := os.Getuid(), os.Getgid()
	groups, _ := os.Getgroups()
	runOwner := strconv.Itoa(runUID)
	if u, err := user.LookupId(runOwner); err == nil {
		runOwner = u.Username
	}
	runGroup := strconv.Itoa(runGID)
	if g, err := user.LookupGroupId(runGroup); err == nil {
		runGroup = g.Name
	}
	futOwner, futUID := runOwner, runUID
	if z.fa.owner != "" {
		if u, err := user.Lookup(z.fa.owner); err == nil {
			futOwner = u.Username
			futUID, _ = strconv.Atoi(u.Uid)
		} else if u, err := user.LookupId(z.fa.owner); err == nil {
			futOwner = u.Username
			futUID, _ = strconv.Atoi(u.Uid)
		}
	}
	futGroup, futGID := runGroup, runGID
	if z.fa.group != "" {
		if g, err := user.LookupGroup(z.fa.group); err == nil {
			futGroup = g.Name
			futGID, _ = strconv.Atoi(g.Gid)
		} else if g, err := user.LookupGroupId(z.fa.group); err == nil {
			futGroup = g.Name
			futGID, _ = strconv.Atoi(g.Gid)
		}
	}
	inGroups := false
	for _, g := range groups {
		if g == futGID {
			inGroups = true
		}
	}
	isIncluded := func(p string) bool {
		for _, c := range z.changed {
			if c == p {
				return true
			}
		}
		return false
	}
	for _, line := range splitLines(oldOut) {
		change := false
		pcs := splitFieldsN(line, 8)
		if len(pcs) != 8 {
			continue
		}
		if l := len(pcs[0]); l != 7 && l != 8 && l != 10 {
			continue
		}
		if len(pcs[6]) != 15 {
			continue
		}
		if strings.IndexByte("dl-?", pcs[0][0]) < 0 || strings.Trim(pcs[0][1:], "rwxstah-") != "" {
			continue
		}
		ztype := pcs[0][0]
		permstr := pcs[0][1:]
		size, _ := strconv.ParseInt(pcs[3], 10, 64)
		path := pcs[7]
		excludedExact := false
		for _, e := range z.excludes {
			if e == path {
				excludedExact = true
			}
		}
		if excludedExact {
			out.WriteString(fmt.Sprintf("Path %s is excluded on request\n", path))
			continue
		}
		var ftype byte = 'f'
		switch {
		case strings.HasSuffix(path, "/"):
			if ztype != 'd' {
				errB.WriteString(fmt.Sprintf("Path %s incorrectly tagged as \"%c\", but is a directory.\n", path, ztype))
			}
			ftype = 'd'
		case ztype == 'l':
			ftype = 'L'
		}
		var fileUmask uint32
		switch len(permstr) {
		case 6:
			switch {
			case strings.HasSuffix(path, "/"), permstr == "rwx---":
				permstr = "rwxrwxrwx"
			default:
				permstr = "rw-rw-rw-"
			}
			fileUmask = umask
		case 7:
			if permstr == "rwxa---" {
				permstr = "rwxrwxrwx"
			} else {
				permstr = "rw-rw-rw-"
			}
			fileUmask = umask
		}
		if len(permstr) != 9 || !zipFileModeRE.MatchString(permstr) {
			return nil, agentproto.Fail("ZIP info perm format incorrect, %s", permstr)
		}
		dest := pyJoin(z.bDest, path)
		st, err := os.Lstat(dest)
		if err != nil {
			z.changed = append(z.changed, path)
			errB.WriteString(fmt.Sprintf("Path %s is missing\n", path))
			diff.WriteString(fmt.Sprintf(">%c++++++.?? %s\n", ftype, path))
			continue
		}
		if ftype == 'd' && !st.IsDir() {
			z.changed = append(z.changed, path)
			errB.WriteString(fmt.Sprintf("File %s already exists, but not as a directory\n", path))
			diff.WriteString(fmt.Sprintf("c%c++++++.?? %s\n", ftype, path))
			continue
		}
		if ftype == 'f' && !st.Mode().IsRegular() {
			unarchived = false
			z.changed = append(z.changed, path)
			errB.WriteString(fmt.Sprintf("Directory %s already exists, but not as a regular file\n", path))
			diff.WriteString(fmt.Sprintf("c%c++++++.?? %s\n", ftype, path))
			continue
		}
		if ftype == 'L' && st.Mode()&os.ModeSymlink == 0 {
			z.changed = append(z.changed, path)
			errB.WriteString(fmt.Sprintf("Directory %s already exists, but not as a symlink\n", path))
			diff.WriteString(fmt.Sprintf("c%c++++++.?? %s\n", ftype, path))
			continue
		}
		itemized := []byte(fmt.Sprintf(".%c.......??", ftype))
		ts := zipTimestamp(pcs[6])
		mtime := float64(st.ModTime().UnixNano()) / 1e9
		regular := st.Mode().IsRegular()
		if regular {
			if z.p.Bool("keep_newer") {
				if ts > mtime {
					change = true
					z.changed = append(z.changed, path)
					errB.WriteString(fmt.Sprintf("File %s is older, replacing file\n", path))
					itemized[4] = 't'
				} else if ts < mtime {
					out.WriteString(fmt.Sprintf("File %s is newer, excluding file\n", path))
					z.excludes = append(z.excludes, path)
					continue
				}
			} else if ts != mtime {
				change = true
				z.changed = append(z.changed, path)
				errB.WriteString(fmt.Sprintf("File %s differs in mtime (%f vs %f)\n", path, ts, mtime))
				itemized[4] = 't'
			}
			if size != st.Size() {
				change = true
				errB.WriteString(fmt.Sprintf("File %s differs in size (%d vs %d)\n", path, size, st.Size()))
				itemized[3] = 's'
			}
			sum, _ := crc32File(dest, int(z.p.Int("io_buffer_size")))
			if want := z.crc(path); sum != want {
				change = true
				errB.WriteString(fmt.Sprintf("File %s differs in CRC32 checksum (0x%08x vs 0x%08x)\n", path, want, sum))
				itemized[2] = 'c'
			}
		}
		if ftype != 'L' {
			var mode uint32
			switch {
			case modeGiven(z.fa.mode):
				m, f := resolveMode(dest, z.fa.mode, st)
				if f != nil {
					f.Extra = map[string]any{"path": path}
					return nil, f
				}
				mode = uint32(m)
			case ztype == '?':
				mode = permstrToOctal(permstr, 0)
			default:
				mode = permstrToOctal(permstr, fileUmask)
			}
			if cur := sIMode(st.Mode()); mode != cur {
				change = true
				itemized[5] = 'p'
				errB.WriteString(fmt.Sprintf("Path %s differs in permissions (%o vs %o)\n", path, mode, cur))
			}
		}
		stUID, stGID, _ := statIDsOf(st)
		owner := ""
		if u, err := user.LookupId(strconv.Itoa(stUID)); err == nil {
			owner = u.Username
		}
		if runUID != 0 && (futOwner != runOwner || futUID != runUID) {
			return nil, agentproto.Fail("Cannot change ownership of %s to %s, as user %s", path, futOwner, runOwner)
		}
		if owner != "" && owner != futOwner {
			change = true
			errB.WriteString(fmt.Sprintf("Path %s is owned by user %s, not by user %s as expected\n", path, owner, futOwner))
			itemized[6] = 'o'
		} else if owner == "" && stUID != futUID {
			change = true
			errB.WriteString(fmt.Sprintf("Path %s is owned by uid %d, not by uid %d as expected\n", path, stUID, futUID))
			itemized[6] = 'o'
		}
		group := ""
		if g, err := user.LookupGroupId(strconv.Itoa(stGID)); err == nil {
			group = g.Name
		}
		if runUID != 0 && (futGroup != runGroup || futGID != runGID) && !inGroups {
			return nil, agentproto.Fail("Cannot change group ownership of %s to %s, as user %s", path, futGroup, runOwner)
		}
		if group != "" && group != futGroup {
			change = true
			errB.WriteString(fmt.Sprintf("Path %s is owned by group %s, not by group %s as expected\n", path, group, futGroup))
			itemized[6] = 'g'
		} else if group == "" && stGID != futGID {
			change = true
			errB.WriteString(fmt.Sprintf("Path %s is owned by gid %d, not by gid %d as expected\n", path, stGID, futGID))
			itemized[6] = 'g'
		}
		if change {
			if !isIncluded(path) {
				z.changed = append(z.changed, path)
			}
			diff.WriteString(fmt.Sprintf("%s %s\n", itemized, path))
		}
	}
	if len(z.changed) > 0 {
		unarchived = false
	}
	return &unarchiveCheck{unarchived: unarchived, rc: rc, out: out.String(), err: errB.String(),
		cmd: cmd, diff: diff.String()}, nil
}

func (z *zipHandler) unarchive() map[string]any {
	cmd := []string{z.cmdPath, "-o"}
	cmd = append(cmd, z.opts...)
	cmd = append(cmd, z.src)
	if len(z.excludes) > 0 {
		cmd = append(cmd, "-x")
		cmd = append(cmd, z.excludes...)
	}
	cmd = append(cmd, z.includes...)
	cmd = append(cmd, "-d", z.bDest)
	rc, out, errOut := runCommand(z.env, cmd, cmdOpts{})
	return map[string]any{"cmd": anyList(cmd), "rc": rc, "out": out, "err": errOut}
}

func crc32File(path string, bufSize int) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if bufSize <= 0 {
		bufSize = 64 * 1024
	}
	h := crc32.NewIEEE()
	if _, err := io.CopyBuffer(h, f, make([]byte, bufSize)); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// splitFieldsN is str.split(None, n-1): at most n whitespace-separated
// fields, the last holding the remainder.
func splitFieldsN(s string, n int) []string {
	var out []string
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	for len(out) < n-1 && s != "" {
		i := strings.IndexAny(s, " \t\n\r\f\v")
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = strings.TrimLeft(s[i:], " \t\n\r\f\v")
	}
	if s != "" {
		if len(out) < n-1 {
			s = strings.TrimRight(s, " \t\n\r\f\v")
		}
		out = append(out, s)
	}
	return out
}
