package modules

import (
	"bufio"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(getURLModule, "get_url", "ansible.builtin.get_url")
}

// getURLSpec is url_argument_spec() plus get_url's options and the file
// common arguments (add_file_common_args).
var getURLSpec = args.Spec{
	"url":                  {Required: true},
	"dest":                 {Required: true},
	"force":                {Type: "bool", Default: false},
	"http_agent":           {Default: "ansible-httpget"},
	"use_proxy":            {Type: "bool", Default: true},
	"validate_certs":       {Type: "bool", Default: true},
	"url_username":         {Aliases: []string{"username"}},
	"url_password":         {Aliases: []string{"password"}},
	"force_basic_auth":     {Type: "bool", Default: false},
	"client_cert":          {},
	"client_key":           {},
	"use_gssapi":           {Type: "bool", Default: false},
	"backup":               {Type: "bool", Default: false},
	"checksum":             {Default: ""},
	"timeout":              {Type: "int", Default: 10},
	"headers":              {Type: "dict"},
	"tmp_dest":             {},
	"unredirected_headers": {Type: "list", Default: []any{}},
	"decompress":           {Type: "bool", Default: true},
	"ciphers":              {Type: "list"},
	"use_netrc":            {Type: "bool", Default: true},
	"mode":                 {Type: "any"},
	"owner":                {},
	"group":                {},
	"seuser":               {},
	"serole":               {},
	"setype":               {},
	"selevel":              {},
	"attributes":           {Aliases: []string{"attr"}},
	"unsafe_writes":        {Type: "bool", Default: false},
}

// getURLRun is one get_url invocation.
type getURLRun struct {
	env    *RunEnv
	p      *args.Parsed
	tmpdir string // module.tmpdir
	start  time.Time
}

// fetchInfo is fetch_url's info dict (the parts get_url reads).
type fetchInfo struct {
	status  int
	msg     string
	url     string
	headers http.Header
}

func (r *getURLRun) elapsed() int64 { return int64(time.Since(r.start).Seconds()) }

func (r *getURLRun) fail(msg string, extra map[string]any) *agentproto.Result {
	res := &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{}}
	for k, v := range extra {
		res.Extra[k] = v
	}
	return res
}

// getURLModule ports ansible.builtin.get_url.
func getURLModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := getURLSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	r := &getURLRun{env: env, p: p}
	defer func() {
		if r.tmpdir != "" {
			os.RemoveAll(r.tmpdir)
		}
	}()

	rawURL := p.Str("url")
	dest := pyExpandPath(p.Str("dest"))
	force := p.Bool("force")
	checksum := p.Str("checksum")
	result := map[string]any{
		"changed": false, "checksum_dest": nil, "checksum_src": nil,
		"dest": dest, "elapsed": int64(0), "url": rawURL,
	}
	out := func(extra map[string]any) *agentproto.Result {
		res := &agentproto.Result{Extra: map[string]any{}}
		for k, v := range result {
			res.Extra[k] = v
		}
		for k, v := range extra {
			res.Extra[k] = v
		}
		if c, ok := res.Extra["changed"].(bool); ok {
			res.Changed = c
		}
		delete(res.Extra, "changed")
		if m, ok := res.Extra["msg"].(string); ok {
			res.Msg = m
			if m != "" {
				delete(res.Extra, "msg")
			}
		}
		return res
	}
	failR := func(msg string) *agentproto.Result {
		res := out(nil)
		res.Failed, res.Msg = true, msg
		delete(res.Extra, "msg")
		return res
	}

	destIsDir := isDir(dest)
	var lastMod time.Time
	algorithm := ""
	if checksum != "" {
		var ok bool
		algorithm, checksum, ok = strings.Cut(checksum, ":")
		if !ok {
			return failR("The checksum parameter has to be in format <algorithm>:<checksum>")
		}
		if isChecksumURL(checksum) {
			r.start = time.Now()
			tmp, _, fail := r.urlGet(checksum, dest, lastMod, force, "GET", false)
			if fail != nil {
				return fail
			}
			data, _ := os.ReadFile(tmp)
			os.Remove(tmp)
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(data) == 0 {
				lines = nil
			}
			filename := urlFilename(rawURL)
			found := ""
			for _, pair := range parseDigestLines(filename, lines) {
				if pair[1] == filename {
					found = pair[0]
					break
				}
			}
			if found == "" {
				return agentproto.Fail("Unable to find a checksum for file '%s' in '%s'", filename, checksum)
			}
			checksum = found
		}
		checksum = strings.ToLower(regexp.MustCompile(`\W+`).ReplaceAllString(checksum, ""))
		if !isHexString(checksum) {
			return failR("The checksum format is invalid")
		}
	}

	if !destIsDir && pathExists(dest) {
		mismatch := false
		if !force && checksum != "" {
			cur, err := digestFileAlgo(dest, algorithm)
			if err != nil {
				return agentproto.Fail("%s", err)
			}
			mismatch = checksum != cur
		}
		if !force && checksum != "" && !mismatch {
			changed, fail := setFSAttrs(env, loadFileAttrs(p, dest, false), false, nil)
			if fail != nil {
				return fail
			}
			result["changed"] = changed
			if changed {
				return out(map[string]any{"msg": "file already exists but file attributes changed"})
			}
			return out(map[string]any{"msg": "file already exists"})
		}
		if info, err := os.Stat(dest); err == nil {
			lastMod = info.ModTime()
		}
		if mismatch {
			force = true
		}
	}

	r.start = time.Now()
	method := "GET"
	if env.CheckMode {
		method = "HEAD"
	}
	tmpsrc, info, fail := r.urlGet(rawURL, dest, lastMod, force, method, true)
	if fail != nil {
		return fail
	}
	result["elapsed"] = r.elapsed()
	result["src"] = tmpsrc

	if destIsDir {
		filename := filenameFromDisposition(info.headers.Get("Content-Disposition"))
		if filename == "" {
			filename = urlFilename(info.url)
		}
		dest = filepath.Join(dest, filename)
		result["dest"] = dest
	}
	if !pathExists(tmpsrc) {
		res := failR("Request failed")
		res.Extra["status_code"], res.Extra["response"] = int64(info.status), info.msg
		return res
	}
	result["checksum_src"], _ = digestFileAlgo(tmpsrc, "sha1")

	if pathExists(dest) {
		if !accessOK(dest, 2) {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("Destination %s is not writable", dest))
		}
		if !accessOK(dest, 4) {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("Destination %s is not readable", dest))
		}
		result["checksum_dest"], _ = digestFileAlgo(dest, "sha1")
	} else {
		dir := filepath.Dir(dest)
		if !pathExists(dir) {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("Destination %s does not exist", dir))
		}
		if !accessOK(dir, 2) {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("Destination %s is not writable", dir))
		}
	}

	if env.CheckMode {
		os.Remove(tmpsrc)
		result["changed"] = result["checksum_src"] != result["checksum_dest"]
		return out(map[string]any{"msg": info.msg})
	}

	if checksum != "" {
		got, err := digestFileAlgo(tmpsrc, algorithm)
		if err != nil {
			return agentproto.Fail("%s", err)
		}
		if checksum != got {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("The checksum for %s did not match %s; it was %s.", tmpsrc, checksum, got))
		}
	}

	backupFile := ""
	changed := false
	if result["checksum_src"] != result["checksum_dest"] {
		if p.Bool("backup") && pathExists(dest) {
			b, err := fsutil.Backup(dest)
			if err != nil {
				os.Remove(tmpsrc)
				return failR(fmt.Sprintf("failed to copy %s to %s: %s", tmpsrc, dest, err))
			}
			backupFile = b
		}
		if err := fsutil.AtomicMove(tmpsrc, dest, true); err != nil {
			os.Remove(tmpsrc)
			return failR(fmt.Sprintf("failed to copy %s to %s: %s", tmpsrc, dest, pyOSError(err)))
		}
		changed = true
	} else {
		os.Remove(tmpsrc)
	}
	changed, fail = setFSAttrs(env, loadFileAttrs(p, dest, false), changed, nil)
	if fail != nil {
		return fail
	}
	result["changed"] = changed
	result["md5sum"], _ = digestFileAlgo(dest, "md5")
	extra := map[string]any{"msg": info.msg, "status_code": int64(info.status)}
	if strings.HasPrefix(rawURL, "file://") {
		extra["status_code"] = nil // urllib's file responses have no code
	}
	if backupFile != "" {
		extra["backup_file"] = backupFile
	}
	return out(extra)
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	_, err := hex.DecodeString(strings.Repeat("0", len(s)%2) + s)
	return err == nil
}

// isChecksumURL is get_url's is_url(): a supported URL scheme.
func isChecksumURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "file":
		return true
	}
	return false
}

// urlFilename is url_filename(): the URL path's basename, or index.html.
func urlFilename(raw string) string {
	u, err := url.Parse(raw)
	p := raw
	if err == nil {
		p = u.Path
	}
	fn := path.Base(p)
	if p == "" || strings.HasSuffix(p, "/") || fn == "/" || fn == "." {
		return "index.html"
	}
	return fn
}

// filenameFromDisposition is extract_filename_from_headers().
func filenameFromDisposition(h string) string {
	if h == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(h)
	if err != nil {
		return ""
	}
	if fn := params["filename"]; fn != "" {
		return filepath.Base(fn)
	}
	return ""
}

var (
	bsdDigestLine = regexp.MustCompile(`^(\w+) ?\((?P<path>.+)\) ?= (?P<digest>[\w.]+)$`)
	gnuDigestLine = regexp.MustCompile(`^(?P<digest>[\w.]+)\s+(\*|\./|\.)?(?P<path>.+)$`)
)

// parseDigestLines is parse_digest_lines(): (digest, path) pairs.
func parseDigestLines(filename string, lines []string) [][2]string {
	var out [][2]string
	if len(lines) == 1 && len(strings.Fields(lines[0])) == 1 {
		return append(out, [2]string{lines[0], filename})
	}
	for _, line := range lines {
		if m := bsdDigestLine.FindStringSubmatch(line); m != nil {
			out = append(out, [2]string{m[3], m[2]})
		} else if m := gnuDigestLine.FindStringSubmatch(line); m != nil {
			out = append(out, [2]string{m[1], strings.TrimLeft(m[3], "./")})
		}
	}
	return out
}

// digestFileAlgo is AnsibleModule.digest_from_file.
func digestFileAlgo(p, algorithm string) (string, error) {
	var h hash.Hash
	switch algorithm {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha224":
		h = sha256.New224()
	case "sha256":
		h = sha256.New()
	case "sha384":
		h = sha512.New384()
	case "sha512":
		h = sha512.New()
	default:
		return "", fmt.Errorf("Could not hash file '%s' with algorithm '%s'. Available algorithms: %s",
			p, algorithm, "md5, sha1, sha224, sha256, sha384, sha512")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// moduleTmpdir is AnsibleModule.tmpdir: a fresh
// ~/.ansible/tmp/ansible-tmp-<time>-<pid>-<rand> directory.
func moduleTmpdir() (string, error) {
	base := pyExpandUser("~/.ansible/tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		base = os.TempDir()
	}
	name := fmt.Sprintf("ansible-tmp-%s-%d-%d", pyFloat(float64(time.Now().UnixNano())/1e9), os.Getpid(), rand.Int63n(1<<48))
	dir := filepath.Join(base, name)
	return dir, os.Mkdir(dir, 0o700)
}

// mkstemp is tempfile.mkstemp(dir=dir): "tmp" plus 8 random characters.
func mkstemp(dir string) (*os.File, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789_"
	for i := 0; i < 100; i++ {
		b := make([]byte, 8)
		for j := range b {
			b[j] = chars[rand.Intn(len(chars))]
		}
		f, err := os.OpenFile(filepath.Join(dir, "tmp"+string(b)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil || !os.IsExist(err) {
			return f, err
		}
	}
	return nil, os.ErrExist
}

// urlGet is get_url's url_get(): fetch into a temp file. A failure is
// returned as the module result to hand back.
func (r *getURLRun) urlGet(rawURL, dest string, lastMod time.Time, force bool, method string, main bool) (string, *fetchInfo, *agentproto.Result) {
	p := r.p
	resp, info, fail := r.fetch(rawURL, lastMod, force, method)
	if fail != nil {
		return "", nil, fail
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	base := map[string]any{"url": rawURL, "dest": dest}
	if info.status == 304 {
		res := &agentproto.Result{Msg: info.msg, Extra: map[string]any{
			"url": rawURL, "dest": dest, "status_code": int64(304), "elapsed": r.elapsed()}}
		return "", nil, res // exit_json: not a failure
	}
	if info.status == -1 {
		base["elapsed"] = r.elapsed()
		return "", nil, r.fail(info.msg, base)
	}
	if info.status != 200 && !strings.HasPrefix(rawURL, "file:/") {
		base["elapsed"] = r.elapsed()
		base["status_code"] = int64(info.status)
		base["response"] = info.msg
		return "", nil, r.fail("Request failed", base)
	}
	tmpDest := pyExpandPath(p.Str("tmp_dest"))
	if tmpDest != "" {
		if !isDir(tmpDest) {
			msg := tmpDest + " directory does not exist."
			if pathExists(tmpDest) {
				msg = tmpDest + " is a file but should be a directory."
			}
			return "", nil, r.fail(msg, map[string]any{"elapsed": r.elapsed()})
		}
	} else {
		if r.tmpdir == "" {
			dir, err := moduleTmpdir()
			if err != nil {
				return "", nil, moduleCrash(err)
			}
			r.tmpdir = dir
		}
		tmpDest = r.tmpdir
	}
	f, err := mkstemp(tmpDest)
	if err != nil {
		return "", nil, moduleCrash(err)
	}
	tempname := f.Name()
	var n int64
	if resp != nil {
		var body io.Reader = resp.Body
		if !main || p.Bool("decompress") {
			if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
				gz, err := gzip.NewReader(bufio.NewReader(resp.Body))
				if err == nil {
					body = gz
				}
			}
		}
		n, err = io.Copy(f, body)
		if err != nil {
			f.Close()
			os.Remove(tempname)
			return "", nil, r.fail("failed to create temporary content file: "+err.Error(), map[string]any{"elapsed": r.elapsed()})
		}
	}
	f.Close()
	isGzip := resp != nil && resp.Header.Get("Content-Encoding") == "gzip"
	decompress := !main || p.Bool("decompress")
	if !r.env.CheckMode && resp != nil && resp.Header.Get("Content-Length") != "" && (!isGzip || !decompress) {
		if cl, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n != cl {
			return "", nil, r.fail(fmt.Sprintf("Incomplete read, (rsp.length=0, cl=%d, st.st_size=%d) failed to read remaining %d bytes", cl, n, cl-n), nil)
		}
	}
	return tempname, info, nil
}

// fetch is fetch_url/open_url for get_url: auth (URL credentials,
// challenge-driven or forced basic auth, ~/.netrc), proxies, TLS options,
// unredirected headers and urllib's request headers.
func (r *getURLRun) fetch(rawURL string, lastMod time.Time, force bool, method string) (*http.Response, *fetchInfo, *agentproto.Result) {
	p := r.p
	info := &fetchInfo{status: -1, url: rawURL}
	if path, ok := strings.CutPrefix(rawURL, "file://"); ok {
		f, err := os.Open(path)
		if err != nil {
			info.msg = "Request failed: <urlopen error " + pyOSErrorURL(err) + ">"
			return nil, info, nil
		}
		st, _ := f.Stat()
		h := http.Header{}
		if st != nil {
			h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		}
		info.status, info.headers = 200, h
		info.msg = "OK (" + h.Get("Content-Length") + " bytes)"
		return &http.Response{StatusCode: 200, Header: h, Body: f}, info, nil
	}

	username, password := p.Str("url_username"), p.Str("url_password")
	u, err := url.Parse(rawURL)
	if err != nil {
		info.msg = err.Error()
		return nil, nil, r.fail(info.msg, map[string]any{"url": rawURL, "status": int64(-1)})
	}
	if username == "" && u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
		rawURL = u.String()
	}
	headers := http.Header{}
	challengeAuth := false
	switch {
	case p.Bool("use_gssapi"):
		return nil, nil, &agentproto.Result{Failed: true, Msg: missingRequiredLib("gssapi", "for use_gssapi=True", "https://pypi.org/project/gssapi/")}
	case username != "" && !p.Bool("force_basic_auth"):
		challengeAuth = true
	case username != "":
		headers.Set("Authorization", basicAuthHeader(username, password))
	case p.Bool("use_netrc"):
		if login, pass, ok := netrcLogin(r.env, u.Hostname()); ok && login != "" && pass != "" {
			headers.Set("Authorization", basicAuthHeader(login, pass))
		}
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: !p.Bool("validate_certs")}
	if cert := pyExpandPath(p.Str("client_cert")); cert != "" {
		key := pyExpandPath(p.Str("client_key"))
		if key == "" {
			key = cert
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			info.msg = "Request failed: <urlopen error " + err.Error() + ">"
			return nil, info, nil
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	if list := p.List("ciphers"); len(list) > 0 {
		suites, err := tlsCipherSuites(list)
		if err != nil {
			return nil, nil, r.fail(err.Error(), map[string]any{"url": rawURL, "status": int64(-1)})
		}
		tlsCfg.CipherSuites = suites
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg, DisableCompression: true}
	if p.Bool("use_proxy") {
		transport.Proxy = http.ProxyFromEnvironment
	}
	unredirected := map[string]bool{}
	for _, h := range p.List("unredirected_headers") {
		unredirected[strings.ToLower(pyStrValue(h))] = true
	}
	client := &http.Client{
		Timeout:   time.Duration(p.Int("timeout")) * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			// urllib re-sends the original headers minus the
			// unredirected ones, and adds no Referer.
			req.Header.Del("Referer")
			for k := range req.Header {
				if unredirected[strings.ToLower(k)] {
					req.Header.Del(k)
				}
			}
			for k, v := range via[0].Header {
				if !unredirected[strings.ToLower(k)] && req.Header.Get(k) == "" {
					req.Header[k] = v
				}
			}
			return nil
		},
	}
	build := func(extraAuth string) (*http.Request, error) {
		req, err := http.NewRequest(method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Connection", "close")
		if agent := p.Str("http_agent"); agent != "" {
			req.Header.Set("User-Agent", agent)
		}
		if force {
			req.Header.Set("Cache-Control", "no-cache")
		} else if !lastMod.IsZero() {
			req.Header.Set("If-Modified-Since", lastMod.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
		}
		for k, v := range headers {
			req.Header[k] = v
		}
		for k, v := range p.Dict("headers") {
			req.Header.Set(k, pyStrValue(v))
		}
		if extraAuth != "" {
			req.Header.Set("Authorization", extraAuth)
		}
		req.Close = true
		return req, nil
	}
	req, err := build("")
	if err != nil {
		return nil, nil, r.fail(err.Error(), map[string]any{"url": rawURL, "status": int64(-1)})
	}
	resp, err := client.Do(req)
	if err == nil && resp.StatusCode == 401 && challengeAuth &&
		strings.HasPrefix(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "basic") {
		resp.Body.Close()
		req, _ = build(basicAuthHeader(username, password))
		resp, err = client.Do(req)
	}
	if err != nil {
		info.msg = urlErrorMsg(err)
		return nil, info, nil
	}
	info.headers = resp.Header
	info.url = resp.Request.URL.String()
	info.status = resp.StatusCode
	if resp.StatusCode >= 300 {
		info.msg = fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, httpReason(resp))
		resp.Body.Close()
		return nil, info, nil
	}
	cl := resp.Header.Get("Content-Length")
	if cl == "" {
		cl = "unknown"
	}
	info.msg = "OK (" + cl + " bytes)"
	return resp, info, nil
}

func basicAuthHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// netrcLogin is netrc.netrc(os.environ.get('NETRC')).authenticators(host):
// the machine entry for host, else the default entry.
func netrcLogin(env *RunEnv, host string) (login, password string, ok bool) {
	file, set := env.Env["NETRC"]
	if !set {
		file = os.Getenv("NETRC")
	}
	if file == "" {
		file = pyExpandUser("~/.netrc")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", "", false
	}
	var toks []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		toks = append(toks, strings.Fields(line)...)
	}
	type entry struct{ login, account, password string }
	entries := map[string]*entry{}
	var def *entry
	var cur *entry
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "machine":
			if i+1 < len(toks) {
				cur = &entry{}
				if _, seen := entries[toks[i+1]]; !seen {
					entries[toks[i+1]] = cur
				}
				i++
			}
		case "default":
			cur = &entry{}
			def = cur
		case "login", "user":
			if cur != nil && i+1 < len(toks) {
				cur.login = toks[i+1]
				i++
			}
		case "account":
			if cur != nil && i+1 < len(toks) {
				cur.account = toks[i+1]
				i++
			}
		case "password", "passwd":
			if cur != nil && i+1 < len(toks) {
				cur.password = toks[i+1]
				i++
			}
		case "macdef":
			cur = nil
		}
	}
	if e, found := entries[host]; found {
		return e.login, e.password, true
	}
	if def != nil {
		return def.login, def.password, true
	}
	return "", "", false
}

// tlsCipherSuites maps OpenSSL or IANA cipher names onto Go's suites
// (TLS 1.3 suites are not configurable and are skipped).
func tlsCipherSuites(names []any) ([]uint16, error) {
	byName := map[string]uint16{}
	for _, cs := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
		byName[cs.Name] = cs.ID
		byName[opensslCipherName(cs.Name)] = cs.ID
	}
	var out []uint16
	for _, n := range names {
		for _, name := range strings.Split(pyStrValue(n), ":") {
			if id, ok := byName[name]; ok {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("('No cipher can be selected.',)")
	}
	return out, nil
}

// opensslCipherName converts an IANA TLS 1.2 suite name to OpenSSL's
// (TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 -> ECDHE-RSA-AES128-GCM-SHA256).
func opensslCipherName(iana string) string {
	s := strings.TrimPrefix(iana, "TLS_")
	kx, enc, ok := strings.Cut(s, "_WITH_")
	if !ok {
		return iana
	}
	enc = strings.NewReplacer("AES_128_", "AES128_", "AES_256_", "AES256_", "CHACHA20_POLY1305_SHA256", "CHACHA20_POLY1305",
		"3DES_EDE_CBC", "DES-CBC3", "_CBC_SHA", "_SHA").Replace(enc)
	name := kx + "_" + enc
	if kx == "RSA" {
		name = enc
	}
	return strings.ReplaceAll(name, "_", "-")
}

// missingRequiredLib is basic.missing_required_lib().
func missingRequiredLib(library, reason, url string) string {
	host, _ := os.Hostname()
	py, err := lookPath("python3")
	if err != nil {
		py = "/usr/bin/python3"
	}
	msg := fmt.Sprintf("Failed to import the required Python library (%s) on %s's Python %s.", library, host, py)
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

// pyOSErrorURL renders an OSError for a urlopen error message.
func pyOSErrorURL(err error) string {
	var errno syscall.Errno
	var pe *os.PathError
	if errors.As(err, &pe) && errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), pyStrerror(errno), pyStrRepr(pe.Path))
	}
	return err.Error()
}
