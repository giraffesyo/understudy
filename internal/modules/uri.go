package modules

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/omap"
)

func init() {
	Register(uriModule, "uri", "ansible.builtin.uri")
}

var uriSpec = args.Spec{
	// url_argument_spec()
	"url":              {Required: true},
	"force":            {Type: "bool", Default: false},
	"http_agent":       {Default: "ansible-httpget"},
	"use_proxy":        {Type: "bool", Default: true},
	"validate_certs":   {Type: "bool", Default: true},
	"url_username":     {Aliases: []string{"user"}},
	"url_password":     {Aliases: []string{"password"}},
	"force_basic_auth": {Type: "bool", Default: false},
	"client_cert":      {},
	"client_key":       {},
	"use_gssapi":       {Type: "bool", Default: false},
	// url_redirect_argument_spec()
	"follow_redirects": {Default: "safe", Choices: []string{"all", "no", "none", "safe", "urllib2", "yes"}},
	// uri
	"dest":                 {},
	"body":                 {Type: "any"},
	"body_format":          {Default: "raw", Choices: []string{"form-urlencoded", "json", "raw", "form-multipart"}},
	"src":                  {},
	"method":               {Default: "GET"},
	"return_content":       {Type: "bool", Default: false},
	"creates":              {},
	"removes":              {},
	"status_code":          {Type: "list", Default: []any{int64(200)}},
	"timeout":              {Type: "int", Default: 30},
	"headers":              {Type: "dict", Default: map[string]any{}},
	"unix_socket":          {},
	"remote_src":           {Type: "bool", Default: false},
	"ca_path":              {},
	"unredirected_headers": {Type: "list", Default: []any{}},
	"decompress":           {Type: "bool", Default: true},
	"ciphers":              {Type: "list"},
	"use_netrc":            {Type: "bool", Default: true},
	// add_file_common_args
	"mode":          {Type: "any"},
	"owner":         {},
	"group":         {},
	"seuser":        {},
	"serole":        {},
	"setype":        {},
	"selevel":       {},
	"unsafe_writes": {Type: "bool", Default: false},
	"attributes":    {Aliases: []string{"attr"}},
}

// uriSrcPayloadKey marks a src whose content the uri action transferred
// from the controller as the request payload.
const uriSrcPayloadKey = "_understudy_src_payload"

var uriMethodRe = regexp.MustCompile(`^[A-Z]+$`)

// uriModule ports ansible.builtin.uri. The request itself (urllib's
// opener: redirects, auth handlers, cookies, TLS contexts) lives in
// urifetch.go.
func uriModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (uri) does not support check mode"}
	}
	var srcPayload []byte
	hasSrcPayload := false
	if _, ok := rawArgs[uriSrcPayloadKey]; ok {
		rawArgs = copyArgs(rawArgs)
		delete(rawArgs, uriSrcPayloadKey)
		hasSrcPayload = true
		if env.Payload != nil {
			srcPayload, _ = io.ReadAll(env.Payload)
		}
	}
	if err := uriSpec.MutuallyExclusive(rawArgs, []string{"body", "src"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := uriSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	statusCodes, fail := uriStatusCodes(p.List("status_code"))
	if fail != nil {
		return fail
	}

	url := p.Str("url")
	body := p.Any("body")
	bodyFormat := strings.ToLower(p.Str("body_format"))
	method := strings.ToUpper(p.Str("method"))
	dest := pyExpandPath(p.Str("dest"))
	hasDest := p.Has("dest")
	returnContent := p.Bool("return_content")

	// dict_headers keeps the user's order (a plain map here: sorted).
	headers := newURIHeaders()
	for _, k := range sortedMapKeys(p.Dict("headers")) {
		headers.set(k, uriPyStr(p.Dict("headers")[k]))
	}

	if !uriMethodRe.MatchString(method) {
		return agentproto.Fail("Parameter 'method' needs to be a single word in uppercase, like GET or POST.")
	}

	var data []byte
	hasData := false
	setBody := func(v any) {
		switch t := v.(type) {
		case nil:
		case string:
			data, hasData = []byte(t), true
		case []byte:
			data, hasData = t, true
		default:
			data, hasData = []byte(uriPyStr(t)), true
		}
	}
	switch bodyFormat {
	case "json":
		if s, ok := body.(string); ok {
			setBody(s)
		} else {
			setBody(uriJSONDumps(body))
		}
		if !headers.has("content-type") {
			headers.set("Content-Type", "application/json")
		}
	case "form-urlencoded":
		if _, ok := body.(string); !ok {
			enc, err := uriFormURLEncoded(body)
			if err != nil {
				if te, ok := err.(*uriTypeError); ok {
					return moduleCrash(te)
				}
				return &agentproto.Result{Failed: true, Msg: "failed to parse body as form_urlencoded: " + err.Error(),
					Extra: map[string]any{"elapsed": int64(0)}}
			}
			body = enc
		}
		setBody(body)
		if !headers.has("content-type") {
			headers.set("Content-Type", "application/x-www-form-urlencoded")
		}
	case "form-multipart":
		ctype, b, err := prepareMultipart(body)
		if err != nil {
			if oe, ok := err.(*uriOSError); ok {
				return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + oe.Error()}
			}
			return agentproto.Fail("failed to parse body as form-multipart: %v", err)
		}
		setBody(b)
		headers.set("Content-Type", ctype)
	default:
		setBody(body)
	}

	if p.Has("creates") {
		if creates := pyExpandPath(p.Str("creates")); pathExists(creates) {
			return uriSkipped("skipped, since '" + creates + "' exists")
		}
	}
	if p.Has("removes") {
		if removes := pyExpandPath(p.Str("removes")); !pathExists(removes) {
			return uriSkipped("skipped, since '" + removes + "' does not exist")
		}
	}

	// uri(): src replaces the body.
	if p.Has("src") {
		src := pyExpandPath(p.Str("src"))
		if hasSrcPayload {
			data, hasData = srcPayload, true
		} else {
			b, err := os.ReadFile(src)
			if err != nil {
				return &agentproto.Result{Failed: true, Msg: "Unable to open source file " + src,
					Extra: map[string]any{"elapsed": int64(0)}}
			}
			data, hasData = b, true
		}
		headers.set("Content-Length", strconv.Itoa(len(data)))
	}

	c := newURIClient(env, p)
	var lastMod *time.Time
	if hasDest && isFile(dest) {
		if info, err := os.Stat(dest); err == nil {
			t := info.ModTime().UTC()
			lastMod = &t
		}
	}

	start := time.Now()
	r, info := c.fetch(url, method, data, hasData, headers, lastMod)
	elapsed := int64(time.Since(start) / time.Second)
	if info.fatal != "" {
		// fetch_url's fail_json(msg=..., **info) for ValueErrors.
		extra := map[string]any{}
		for _, k := range []string{"url", "status"} {
			if v, ok := info.fields[k]; ok {
				extra[k] = v
			}
		}
		return &agentproto.Result{Failed: true, Msg: info.fatal, Extra: extra}
	}

	if info.crash != "" {
		return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + info.crash}
	}

	if r != nil && hasDest && isDir(dest) {
		name := uriResponseFilename(r)
		if name == "" {
			name = "index.html"
		}
		dest = pyJoin(dest, name)
	}

	contentType, subType, charset := "application/octet-stream", "octet-stream", "utf-8"
	if r != nil {
		contentType, subType, charset = uriParseContentType(r.header)
	}
	maybeJSON := false
	if _, suffix, ok := strings.Cut(subType, "+"); ok {
		maybeJSON = contentType != "" && uriJSONCandidate(strings.ToLower(suffix))
	} else if subType != "" {
		maybeJSON = contentType != "" && uriJSONCandidate(strings.ToLower(subType))
	}

	status := info.status()
	statusOK := false
	for _, s := range statusCodes {
		if s == status {
			statusOK = true
		}
	}
	maybeOutput := maybeJSON || returnContent || !statusOK

	var content []byte
	hasContent := false
	if maybeOutput {
		if r != nil && !r.httpError {
			content, hasContent = r.body, true
		} else {
			content, hasContent = info.popBody(), true
		}
	} else if r != nil {
		content, hasContent = r.body, true
	}

	resp := map[string]any{}
	resp["redirected"] = info.fields["url"] != url
	for k, v := range info.fields {
		resp[k] = v
	}
	resp["elapsed"] = elapsed
	resp["status"] = int64(status)
	resp["changed"] = false

	if r != nil && hasDest {
		if statusOK && status != 304 {
			if fail := uriWriteFile(env, dest, content, resp); fail != nil {
				return fail
			}
			fa := loadFileAttrs(p, dest, false)
			changed, fail := setFSAttrs(env, fa, true, nil)
			if fail != nil {
				return fail
			}
			resp["changed"] = changed
		}
		resp["path"] = dest
	}

	uresp := map[string]any{}
	for k, v := range resp {
		uresp[strings.ToLower(strings.ReplaceAll(k, "-", "_"))] = v
	}
	if loc, ok := uresp["location"].(string); ok {
		uresp["location"] = pyURLJoin(url, loc)
	}

	var text *string
	if hasContent && content != nil {
		s := uriDecode(content, charset)
		text = &s
		if maybeJSON {
			// json.loads: objects keep the server's key order.
			if js, err := omap.UnmarshalJSON([]byte(s)); err == nil {
				uresp["json"] = js
			}
		}
	}

	if !statusOK {
		msg, _ := uresp["msg"].(string)
		uresp["msg"] = "Status code was " + strconv.Itoa(status) + " and not " + pyIntList(statusCodes) + ": " + msg
	}
	if returnContent {
		if text != nil {
			uresp["content"] = *text
		} else {
			uresp["content"] = ""
		}
	}
	return uriNoLog(uriResult(uresp, !statusOK), p.Str("url_password"))
}

// uriResult turns the module's result dict into a Result.
func uriResult(m map[string]any, failed bool) *agentproto.Result {
	res := &agentproto.Result{Failed: failed, Extra: map[string]any{}}
	for k, v := range m {
		switch k {
		case "changed":
			res.Changed, _ = v.(bool)
		case "msg":
			res.Msg, _ = v.(string)
		default:
			if b, ok := v.([]byte); ok {
				v = strings.ToValidUTF8(string(b), "�")
			}
			res.Extra[k] = v
		}
	}
	if _, ok := m["msg"]; ok && res.Msg == "" {
		setMsgEmpty(res)
	}
	return res
}

// uriNoKeyMask are the result keys sanitize_keys leaves alone.
var uriNoKeyMask = map[string]bool{"msg": true, "exception": true, "warnings": true, "deprecations": true,
	"failed": true, "skipped": true, "changed": true, "rc": true, "stdout": true, "stderr": true,
	"elapsed": true, "path": true, "location": true, "content_type": true}

// uriNoLog applies the url_password no_log value: sanitize_keys on the
// result's keys, then remove_values on every string.
func uriNoLog(res *agentproto.Result, secret string) *agentproto.Result {
	if secret == "" {
		return res
	}
	const mask = "********"
	var scrub func(v any) any
	scrub = func(v any) any {
		switch t := v.(type) {
		case string:
			if t == secret {
				return "VALUE_SPECIFIED_IN_NO_LOG_PARAMETER"
			}
			return strings.ReplaceAll(t, secret, mask)
		case []any:
			out := make([]any, len(t))
			for i, e := range t {
				out[i] = scrub(e)
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, e := range t {
				out[strings.ReplaceAll(k, secret, mask)] = scrub(e)
			}
			return out
		case *omap.OMap:
			out := omap.NewOMap()
			for _, k := range t.Keys() {
				out.Set(strings.ReplaceAll(k, secret, mask), scrub(t.Get(k)))
			}
			return out
		}
		return v
	}
	res.Msg = strings.ReplaceAll(res.Msg, secret, mask)
	extra := make(map[string]any, len(res.Extra))
	for k, v := range res.Extra {
		if !uriNoKeyMask[k] {
			k = strings.ReplaceAll(k, secret, mask)
		}
		extra[k] = scrub(v)
	}
	res.Extra = extra
	return res
}

// uriSkipped is exit_json(stdout=msg, changed=False) for creates/removes;
// the controller adds stdout_lines.
func uriSkipped(msg string) *agentproto.Result {
	return &agentproto.Result{Extra: map[string]any{"stdout": msg, "stdout_lines": anyList(pySplitLines(msg))}}
}

func uriJSONCandidate(s string) bool { return s == "json" || s == "javascript" }

// uriStatusCodes converts status_code (list, elements=int).
func uriStatusCodes(list []any) ([]int, *agentproto.Result) {
	out := make([]int, 0, len(list))
	bad := func(v any) *agentproto.Result {
		return agentproto.Fail("Elements value for option 'status_code' is of type %s and we were unable to convert to int: \"%s\" cannot be converted to an int",
			pyTypeName(v), pyReprValue(v))
	}
	for _, v := range list {
		switch t := v.(type) {
		case int64:
			out = append(out, int(t))
		case int:
			out = append(out, t)
		case bool:
			if t {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case float64:
			if t != float64(int64(t)) {
				return nil, bad(v)
			}
			out = append(out, int(t))
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(t))
			if err != nil {
				return nil, bad(v)
			}
			out = append(out, n)
		default:
			return nil, bad(v)
		}
	}
	return out, nil
}

func pyIntList(l []int) string {
	parts := make([]string, len(l))
	for i, n := range l {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// uriWriteFile is the module's write_file: the content lands in a temp
// file first and replaces dest only when its checksum differs.
func uriWriteFile(env *RunEnv, dest string, content []byte, resp map[string]any) *agentproto.Result {
	fail := func(msg string) *agentproto.Result {
		if m, _ := resp["msg"].(string); m != "" {
			msg += " " + m
		}
		delete(resp, "msg")
		res := uriResult(resp, true)
		res.Msg = msg
		return res
	}
	tmp, err := env.tempFileIn()
	if err != nil {
		return fail("Failed to create temporary content file: " + pyOSErrorStr(err))
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fail("Failed to create temporary content file: " + pyOSErrorStr(err))
	}
	tmp.Close()
	if cur, err := sha1File(dest); err == nil && cur == sha1Hex(content) {
		return nil
	}
	if err := fsutil.AtomicMove(tmpName, dest, true); err != nil {
		return fail("failed to copy " + tmpName + " to " + dest + ": " + pyOSErrorStr(err))
	}
	return nil
}

// uriResponseFilename is get_response_filename: Content-Disposition's
// filename, else the last path segment of the final URL.
func uriResponseFilename(r *uriResponse) string {
	if cd := r.header.Get("Content-Disposition"); cd != "" {
		if name, ok := mimeHeaderParam(cd, "filename"); ok && name != "" {
			return path.Base(name)
		}
	}
	u := r.url
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j:]
		} else {
			u = ""
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	name := u[strings.LastIndexByte(u, '/')+1:]
	if name == "" {
		return ""
	}
	return pyUnquote(name)
}

// uriDecode is to_text(content, encoding=charset).
func uriDecode(b []byte, charset string) string {
	switch strings.ReplaceAll(strings.ToLower(charset), "_", "-") {
	case "iso-8859-1", "iso8859-1", "latin-1", "latin1", "l1", "iso-ir-100", "cp819", "8859":
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// uriGunzip decodes a gzip content-encoded body.
func uriGunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// pyUnquote is urllib.parse.unquote.
func pyUnquote(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b = append(b, byte(n))
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return strings.ToValidUTF8(string(b), "�")
}

// pyURLJoin is urllib.parse.urljoin for the Location header.
func pyURLJoin(base, ref string) string {
	return urlJoin(base, ref)
}
