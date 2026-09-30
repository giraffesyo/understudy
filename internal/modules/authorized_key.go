package modules

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports ansible.posix.authorized_key: the key file is parsed
// into an insertion-ordered map of key blob -> entry (comment and invalid
// lines are kept verbatim), the requested keys are merged in or removed,
// and the file is rewritten in rank order only when something changed.

func init() {
	names := []string{"authorized_key", "ansible.posix.authorized_key"}
	Register(authorizedKeyModule, names...)
	for _, n := range names {
		specs[n] = authorizedKeySpec
		pathInfoModules[n] = true
	}
}

var authorizedKeySpec = args.Spec{
	"user":           {Required: true},
	"key":            {Required: true},
	"path":           {},
	"manage_dir":     {Type: "bool", Default: true},
	"state":          {Default: "present", Choices: []string{"absent", "present"}},
	"key_options":    {},
	"exclusive":      {Type: "bool", Default: false},
	"comment":        {},
	"validate_certs": {Type: "bool", Default: true},
	"follow":         {Type: "bool", Default: false},
}

var sshKeyTypes = map[string]bool{
	"sk-ecdsa-sha2-nistp256@openssh.com":          true,
	"sk-ecdsa-sha2-nistp256-cert-v01@openssh.com": true,
	"webauthn-sk-ecdsa-sha2-nistp256@openssh.com": true,
	"ecdsa-sha2-nistp256":                         true,
	"ecdsa-sha2-nistp256-cert-v01@openssh.com":    true,
	"ecdsa-sha2-nistp384":                         true,
	"ecdsa-sha2-nistp384-cert-v01@openssh.com":    true,
	"ecdsa-sha2-nistp521":                         true,
	"ecdsa-sha2-nistp521-cert-v01@openssh.com":    true,
	"sk-ssh-ed25519@openssh.com":                  true,
	"sk-ssh-ed25519-cert-v01@openssh.com":         true,
	"ssh-ed25519":                                 true,
	"ssh-ed25519-cert-v01@openssh.com":            true,
	"ssh-dss":                                     true,
	"ssh-rsa":                                     true,
	"ssh-xmss@openssh.com":                        true,
	"ssh-xmss-cert-v01@openssh.com":               true,
	"rsa-sha2-256":                                true,
	"rsa-sha2-512":                                true,
	"ssh-rsa-cert-v01@openssh.com":                true,
	"rsa-sha2-256-cert-v01@openssh.com":           true,
	"rsa-sha2-512-cert-v01@openssh.com":           true,
	"ssh-dss-cert-v01@openssh.com":                true,
}

// akOption is one key option; value nil is a bare flag (no-pty).
type akOption struct {
	key   string
	value *string
}

// akOptions is the module's keydict: ordered, repeatable keys.
type akOptions []akOption

// equal is dict equality of two keydicts: per key, the list of values in
// order, regardless of how the keys interleave.
func (o akOptions) equal(other akOptions) bool {
	group := func(opts akOptions) map[string][]*string {
		m := map[string][]*string{}
		for _, op := range opts {
			m[op.key] = append(m[op.key], op.value)
		}
		return m
	}
	a, b := group(o), group(other)
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if (av[i] == nil) != (bv[i] == nil) || av[i] != nil && *av[i] != *bv[i] {
				return false
			}
		}
	}
	return true
}

// akEntry is parsekey's (key, key_type, options, comment, rank) tuple.
// keyType "skipped" marks a line re-emitted verbatim (key holds it).
type akEntry struct {
	key, keyType string
	options      akOptions
	comment      string
	rank         int
}

func (e *akEntry) sameAs(o *akEntry) bool {
	return e.key == o.key && e.keyType == o.keyType && e.comment == o.comment && e.options.equal(o.options)
}

// akKeys is the insertion-ordered dict of entries keyed by key blob.
type akKeys struct {
	order []string
	m     map[string]*akEntry
}

func newAKKeys() *akKeys { return &akKeys{m: map[string]*akEntry{}} }

func (k *akKeys) set(id string, e *akEntry) {
	if _, ok := k.m[id]; !ok {
		k.order = append(k.order, id)
	}
	k.m[id] = e
}

func (k *akKeys) del(id string) {
	if _, ok := k.m[id]; !ok {
		return
	}
	delete(k.m, id)
	for i, o := range k.order {
		if o == id {
			k.order = append(k.order[:i], k.order[i+1:]...)
			break
		}
	}
}

var akOptionsRe = regexp.MustCompile(`((?:[^,"']|"[^"]*"|'[^']*')+)`)

// akParseOptions is parseoptions: split on commas outside quotes. It is
// re.split(...)[1:-1], so the separators between matches are parts too
// (a lone "," is dropped, anything else becomes a bare option).
func akParseOptions(options string) akOptions {
	var out akOptions
	if options == "" {
		return out
	}
	var parts []string
	last := 0
	for _, m := range akOptionsRe.FindAllStringIndex(options, -1) {
		parts = append(parts, options[last:m[0]], options[m[0]:m[1]])
		last = m[1]
	}
	parts = append(parts, options[last:])
	if len(parts) < 2 {
		return out
	}
	for _, part := range parts[1 : len(parts)-1] {
		if k, v, ok := strings.Cut(part, "="); ok {
			out = append(out, akOption{key: k, value: &v})
		} else if part != "," {
			out = append(out, akOption{key: part})
		}
	}
	return out
}

// akSplit is the module's shlex split (no quotes, no comments): runs of
// space, tab, CR and LF separate tokens.
func akSplit(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n'
	})
}

// akParseKey is parsekey. ok is false for a line without a known key type;
// bad is set when a key type has no key after it (Python's IndexError).
func akParseKey(raw string, rank int) (e *akEntry, ok, bad bool) {
	raw = strings.ReplaceAll(raw, `\#`, "#")
	parts := akSplit(raw)
	if len(parts) > 0 && parts[0] == "#" {
		return &akEntry{key: raw, keyType: "skipped", rank: rank}, true, false
	}
	typeIndex := -1
	for i, p := range parts {
		if sshKeyTypes[p] {
			typeIndex = i
			break
		}
	}
	if typeIndex < 0 {
		return nil, false, false
	}
	if typeIndex+1 >= len(parts) {
		return nil, false, true
	}
	e = &akEntry{key: parts[typeIndex+1], keyType: parts[typeIndex], rank: rank}
	if typeIndex > 0 {
		e.options = akParseOptions(strings.Join(parts[:typeIndex], " "))
	}
	e.comment = strings.Join(parts[typeIndex+2:], " ")
	return e, true, false
}

func akParseKeys(content string) (*akKeys, bool) {
	keys := newAKKeys()
	for i, line := range pySplitlinesKeep(content) {
		e, ok, bad := akParseKey(line, i)
		if bad {
			return nil, false
		}
		if ok {
			keys.set(e.key, e)
		} else {
			keys.set(line, &akEntry{key: line, keyType: "skipped", rank: i})
		}
	}
	return keys, true
}

func akSerialize(keys *akKeys) string {
	entries := make([]*akEntry, 0, len(keys.order))
	for _, id := range keys.order {
		entries = append(entries, keys.m[id])
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].rank < entries[j].rank })
	var b strings.Builder
	for _, e := range entries {
		if e.keyType == "skipped" {
			b.WriteString(e.key)
			continue
		}
		if len(e.options) > 0 {
			strs := make([]string, 0, len(e.options))
			for _, o := range e.options {
				if o.value == nil {
					strs = append(strs, o.key)
				} else {
					strs = append(strs, o.key+"="+*o.value)
				}
			}
			b.WriteString(strings.Join(strs, ",") + " ")
		}
		b.WriteString(e.keyType + " " + e.key + " " + e.comment + "\n")
	}
	return b.String()
}

// pySplitlinesKeep is str.splitlines(True).
func pySplitlinesKeep(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	cur := 0
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		size := len(string(r))
		switch r {
		case '\r':
			end := cur + size
			if i+1 < len(rs) && rs[i+1] == '\n' {
				end++
				i++
			}
			out = append(out, s[start:end])
			start, cur = end, end
			continue
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			out = append(out, s[start:cur+size])
			start = cur + size
		}
		cur += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pySplitlines is str.splitlines().
func pySplitlines(s string) []string {
	lines := pySplitlinesKeep(s)
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, func(r rune) bool {
			switch r {
			case '\r', '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
				return true
			}
			return false
		})
	}
	return lines
}

type akRun struct {
	env                     *RunEnv
	user                    string
	path                    *string
	manageDir, follow       bool
	uid, gid                int
	userLooked, userMissing bool
	lookupErr               string
	homeKeys                *string
}

func (a *akRun) lookup() {
	if a.userLooked {
		return
	}
	a.userLooked = true
	u, err := user.Lookup(a.user)
	if err != nil {
		a.userMissing = true
		a.lookupErr = `"getpwnam(): name not found: ` + pyStrRepr(a.user) + `"`
		return
	}
	a.uid, _ = strconv.Atoi(u.Uid)
	a.gid, _ = strconv.Atoi(u.Gid)
	home := u.HomeDir
	if a.path == nil {
		p := pyJoin(pyJoin(home, ".ssh"), "authorized_keys")
		a.homeKeys = &p
	}
}

// keyfile is the module's keyfile(): the authorized_keys path, and with
// write the directory and file created with the right owner and modes.
func (a *akRun) keyfile(write bool) (string, *agentproto.Result) {
	if a.env.CheckMode && a.path != nil {
		if a.follow {
			return pyRealpath(*a.path), nil
		}
		return *a.path, nil
	}
	a.lookup()
	if a.userMissing {
		if a.env.CheckMode && a.path == nil {
			return "", agentproto.Fail("Either user must exist or you must provide full path to key file in check mode")
		}
		return "", agentproto.Fail("Failed to lookup user %s: %s", a.user, a.lookupErr)
	}
	var sshdir, keysfile string
	if a.path == nil {
		keysfile = *a.homeKeys
		sshdir = filepath.Dir(keysfile)
	} else {
		keysfile = *a.path
		sshdir = pyDirname(keysfile)
	}
	if a.follow {
		keysfile = pyRealpath(keysfile)
	}
	if !write || a.env.CheckMode {
		return keysfile, nil
	}
	if a.manageDir {
		if !pathExists(sshdir) {
			if err := os.Mkdir(sshdir, 0o700); err != nil {
				return "", agentproto.Fail("Failed to create directory %s : %s", sshdir, pyStrOSError(err, sshdir))
			}
			if err := fsutil.SetDefaultSELinuxContext(sshdir, false); err != nil {
				return "", seFailure(err)
			}
		}
		if a.follow {
			os.Chown(sshdir, a.uid, a.gid)
		} else {
			os.Lchown(sshdir, a.uid, a.gid)
		}
		os.Chmod(sshdir, 0o700)
	}
	if !pathExists(keysfile) {
		if base := pyDirname(keysfile); base != "" && !pathExists(base) {
			if err := os.MkdirAll(base, 0o777); err != nil {
				return "", agentproto.Fail("%s", pyStrOSError(err, base))
			}
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if !a.follow {
			flags |= syscall.O_NOFOLLOW
		}
		f, err := os.OpenFile(keysfile, flags, 0o600)
		if err != nil {
			return "", agentproto.Fail("File open failed %s : %s", keysfile, pyStrOSError(err, keysfile))
		}
		f.Close()
		if err := fsutil.SetDefaultSELinuxContext(keysfile, false); err != nil {
			return "", seFailure(err)
		}
	}
	if a.follow {
		os.Chown(keysfile, a.uid, a.gid)
	} else {
		os.Lchown(keysfile, a.uid, a.gid)
	}
	os.Chmod(keysfile, 0o600)
	return keysfile, nil
}

// pyDirname is os.path.dirname.
func pyDirname(p string) string {
	i := strings.LastIndexByte(p, '/') + 1
	head := p[:i]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// akFetch is fetch_url for a key URL: only a final 200 counts.
func akFetch(u string, validateCerts bool) (string, bool) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !validateCerts {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", "ansible-httpget")
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(body), true
}

func authorizedKeyModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := authorizedKeySpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	a := &akRun{env: env, user: p.Str("user"), manageDir: p.Bool("manage_dir"), follow: p.Bool("follow")}
	optStr := func(name string) any {
		if p.Has(name) {
			return p.Str(name)
		}
		return nil
	}
	var pathArg any
	if p.Has("path") {
		exp := pyExpandPath(p.Str("path"))
		a.path = &exp
		pathArg = exp
	}
	state := p.Str("state")
	res := &agentproto.Result{Extra: map[string]any{
		"user": a.user, "key": p.Str("key"), "path": pathArg, "manage_dir": a.manageDir,
		"state": state, "key_options": optStr("key_options"), "exclusive": p.Bool("exclusive"),
		"comment": optStr("comment"), "validate_certs": p.Bool("validate_certs"), "follow": a.follow,
	}}

	key := p.Str("key")
	if strings.HasPrefix(key, "http") {
		body, ok := akFetch(key, p.Bool("validate_certs"))
		if !ok {
			return agentproto.Fail("Error getting key from: %s", key)
		}
		key = body
	}
	if strings.HasPrefix(key, "file") {
		keyPath := key
		if u, err := url.Parse(key); err == nil {
			keyPath = u.Path
		}
		st, err := os.Stat(keyPath)
		if err != nil {
			return agentproto.Fail("Path to a key file not found: %s", keyPath)
		}
		if st.Mode().IsDir() || !st.Mode().IsRegular() {
			return agentproto.Fail("Path to a key is a directory and must be a file: %s", keyPath)
		}
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return agentproto.Fail("Failed to read key file %s : %s", keyPath, pyStrOSError(err, keyPath))
		}
		key = string(data)
	}
	var newKeys []string
	for _, s := range pySplitlines(key) {
		if s != "" && !strings.HasPrefix(s, "#") {
			newKeys = append(newKeys, s)
		}
	}

	doWrite := false
	keyfile, fail := a.keyfile(false)
	if fail != nil {
		return fail
	}
	res.Extra["keyfile"] = keyfile
	var existingContent string
	if data, err := os.ReadFile(keyfile); err == nil {
		existingContent = string(data)
	} else if os.IsPermission(err) {
		return agentproto.Fail("Permission denied on file or path for authorized keys file: %s", keyfile)
	} else if !os.IsNotExist(err) {
		return agentproto.Fail("%s", pyStrOSError(err, keyfile))
	}
	existing, ok := akParseKeys(existingContent)
	if !ok {
		return agentproto.Fail("list index out of range")
	}

	var keysToExist []string
	maxRank := len(existing.order)
	for rank, nk := range newKeys {
		parsed, ok, _ := akParseKey(nk, rank)
		if !ok {
			return agentproto.Fail("invalid key specified: %s", nk)
		}
		if p.Has("key_options") {
			parsed.options = akParseOptions(p.Str("key_options"))
		}
		if p.Has("comment") {
			parsed.comment = p.Str("comment")
		}
		matched := false
		var nonMatching []*akEntry
		if cur, ok := existing.m[parsed.key]; ok {
			if !parsed.sameAs(cur) && state == "present" {
				nonMatching = append(nonMatching, cur)
			} else {
				matched = true
			}
		}
		switch state {
		case "present":
			keysToExist = append(keysToExist, parsed.key)
			for _, nm := range nonMatching {
				if _, ok := existing.m[nm.key]; ok {
					existing.del(nm.key)
					doWrite = true
				}
			}
			if !matched {
				e := *parsed
				e.rank = maxRank + parsed.rank
				existing.set(parsed.key, &e)
				doWrite = true
			}
		case "absent":
			if !matched {
				continue
			}
			existing.del(parsed.key)
			doWrite = true
		}
	}
	if state == "present" && p.Bool("exclusive") {
		keep := map[string]bool{}
		for _, k := range keysToExist {
			keep[k] = true
		}
		for _, id := range append([]string(nil), existing.order...) {
			if !keep[id] {
				existing.del(id)
				doWrite = true
			}
		}
	}

	if doWrite {
		filename, fail := a.keyfile(true)
		if fail != nil {
			return fail
		}
		newContent := akSerialize(existing)
		if env.DiffMode {
			res.Diff = map[string]any{
				"before_header": keyfile, "after_header": filename,
				"before": existingContent, "after": newContent,
			}
		}
		if !env.CheckMode {
			tmp, err := os.CreateTemp("", "tmp")
			if err != nil {
				return agentproto.Fail("Failed to write to file %s: %v", filename, err)
			}
			_, werr := tmp.WriteString(newContent)
			tmp.Close()
			if werr != nil {
				os.Remove(tmp.Name())
				return agentproto.Fail("Failed to write to file %s: %s", tmp.Name(), pyStrOSError(werr, tmp.Name()))
			}
			if err := fsutil.AtomicMove(tmp.Name(), filename, true); err != nil {
				os.Remove(tmp.Name())
				if res := seFailure(err); res != nil {
					return res
				}
				return agentproto.Fail("Unable to make %s into to %s, failed final rename from %s: %s",
					tmp.Name(), filename, tmp.Name(), pyStrOSError(err, filename))
			}
			a.lookup()
			if a.follow {
				os.Chown(filename, a.uid, a.gid)
			} else {
				os.Lchown(filename, a.uid, a.gid)
			}
			os.Chmod(filename, 0o600)
		}
		res.Changed = true
	}
	return res
}
