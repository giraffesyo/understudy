package modules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/sshkey"
)

func init() {
	Register(yumRepositoryModule, "yum_repository", "ansible.builtin.yum_repository")
	Register(sudoersModule, "sudoers", "community.general.sudoers")
	Register(opensshKeypairModule, "openssh_keypair", "community.crypto.openssh_keypair")
}

// fileCommonSpec is FILE_COMMON_ARGUMENTS (add_file_common_args=True).
// SELinux contexts and chattr attributes are accepted and not applied, as
// in the other file-family ports.
func fileCommonSpec(s args.Spec) args.Spec {
	for k, v := range map[string]args.Def{
		"mode": {Type: "any"}, "owner": {}, "group": {},
		"seuser": {}, "serole": {}, "setype": {}, "selevel": {},
		"attributes": {Aliases: []string{"attr"}}, "unsafe_writes": {Type: "bool", Default: false},
	} {
		s[k] = v
	}
	return s
}

// yumRepoParams are the repo options written as keys of the repo section
// (the argument spec minus file, reposdir, state and file-common args).
var yumRepoParams = map[string]args.Def{
	"async":                        {Type: "bool"},
	"bandwidth":                    {},
	"baseurl":                      {Type: "list"},
	"cost":                         {},
	"countme":                      {Type: "bool"},
	"deltarpm_metadata_percentage": {},
	"deltarpm_percentage":          {},
	"description":                  {},
	"enabled":                      {Type: "bool"},
	"enablegroups":                 {Type: "bool"},
	"exclude":                      {Type: "list", Aliases: []string{"excludepkgs"}},
	"failovermethod":               {Choices: []string{"roundrobin", "priority"}},
	"gpgcakey":                     {},
	"gpgcheck":                     {Type: "bool"},
	"gpgkey":                       {Type: "list"},
	"module_hotfixes":              {Type: "bool"},
	"http_caching":                 {Choices: []string{"all", "packages", "none"}},
	"include":                      {},
	"includepkgs":                  {Type: "list"},
	"ip_resolve":                   {Choices: []string{"4", "6", "IPv4", "IPv6", "whatever"}},
	"keepalive":                    {Type: "bool"},
	"metadata_expire":              {},
	"metadata_expire_filter":       {Choices: []string{"never", "read-only:past", "read-only:present", "read-only:future"}},
	"metalink":                     {},
	"mirrorlist":                   {},
	"mirrorlist_expire":            {},
	"name":                         {Required: true},
	"password":                     {},
	"priority":                     {},
	"protect":                      {Type: "bool"},
	"proxy":                        {},
	"proxy_password":               {},
	"proxy_username":               {},
	"repo_gpgcheck":                {Type: "bool"},
	"retries":                      {},
	"s3_enabled":                   {Type: "bool"},
	"skip_if_unavailable":          {Type: "bool"},
	"sslcacert":                    {Aliases: []string{"ca_cert"}},
	"ssl_check_cert_permissions":   {Type: "bool"},
	"sslclientcert":                {Aliases: []string{"client_cert"}},
	"sslclientkey":                 {Aliases: []string{"client_key"}},
	"sslverify":                    {Type: "bool", Aliases: []string{"validate_certs"}},
	"throttle":                     {},
	"timeout":                      {},
	"ui_repoid_vars":               {},
	"username":                     {},
}

var yumRepoSpec = func() args.Spec {
	s := args.Spec{
		"file":     {},
		"reposdir": {Default: "/etc/yum.repos.d"},
		"state":    {Default: "present", Choices: []string{"present", "absent"}},
	}
	for k, v := range yumRepoParams {
		s[k] = v
	}
	return fileCommonSpec(s)
}()

// yumDeprecatedNoEffect are the options yum_repository deprecates because
// dnf ignores them.
var yumDeprecatedNoEffect = map[string]bool{
	"deltarpm_metadata_percentage": true, "gpgcakey": true, "http_caching": true,
	"keepalive": true, "metadata_expire_filter": true, "mirrorlist_expire": true,
	"protect": true, "ssl_check_cert_permissions": true, "ui_repoid_vars": true,
}

// repoSection is one section of a configparser file, keys in file order.
type repoSection struct {
	name string
	keys []string
	vals map[string]string
}

// rawConfig is the part of configparser.RawConfigParser yum_repository
// relies on: ordered sections, lower-cased keys, continuation lines.
type rawConfig struct {
	defaults *repoSection
	sections []*repoSection
}

var (
	repoSectRe = regexp.MustCompile(`^\[(.+)\]`)
	repoOptRe  = regexp.MustCompile(`^(.*?)\s*[=:]\s*(.*)$`)
)

func readRawConfig(path string) (*rawConfig, error) {
	rc := &rawConfig{}
	data, err := os.ReadFile(path)
	if err != nil {
		return rc, nil
	}
	var cur *repoSection
	opt := ""
	indent := 0
	for n, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			if cur != nil && opt != "" {
				cur.vals[opt] += "\n"
			}
			continue
		}
		if stripped[0] == '#' || stripped[0] == ';' {
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " \t\f\v\r"))
		if cur != nil && opt != "" && lead > indent {
			cur.vals[opt] += "\n" + stripped
			continue
		}
		indent = lead
		if m := repoSectRe.FindStringSubmatch(stripped); m != nil {
			if rc.section(m[1]) != nil {
				return nil, fmt.Errorf("While reading from %s [line %2d]: section %s already exists", pyStrRepr(path), n+1, pyStrRepr(m[1]))
			}
			cur = &repoSection{name: m[1], vals: map[string]string{}}
			if m[1] == "DEFAULT" {
				rc.defaults = cur
			} else {
				rc.sections = append(rc.sections, cur)
			}
			opt = ""
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("File contains no section headers.\nfile: %s, line: %d\n%s", pyStrRepr(path), n+1, pyStrRepr(line+"\n"))
		}
		m := repoOptRe.FindStringSubmatch(stripped)
		if m == nil {
			return nil, fmt.Errorf("Source contains parsing errors: %s\n\t[line %2d]: %s", pyStrRepr(path), n+1, pyStrRepr(line+"\n"))
		}
		opt = strings.ToLower(strings.TrimRight(m[1], " \t"))
		if _, dup := cur.vals[opt]; !dup {
			cur.keys = append(cur.keys, opt)
		}
		cur.vals[opt] = strings.TrimSpace(m[2])
	}
	for _, s := range append(rc.sections, rc.defaults) {
		if s == nil {
			continue
		}
		for k, v := range s.vals {
			s.vals[k] = strings.TrimRight(v, "\n")
		}
	}
	return rc, nil
}

func (rc *rawConfig) section(name string) *repoSection {
	for _, s := range rc.sections {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (rc *rawConfig) remove(name string) {
	for i, s := range rc.sections {
		if s.name == name {
			rc.sections = append(rc.sections[:i], rc.sections[i+1:]...)
			return
		}
	}
}

// dump is YumRepo.dump(): sections and keys sorted.
func (rc *rawConfig) dump() string {
	secs := append([]*repoSection{}, rc.sections...)
	sort.Slice(secs, func(i, j int) bool { return secs[i].name < secs[j].name })
	var b strings.Builder
	for _, s := range secs {
		fmt.Fprintf(&b, "[%s]\n", s.name)
		items := map[string]string{}
		if rc.defaults != nil {
			for k, v := range rc.defaults.vals {
				items[k] = v
			}
		}
		for k, v := range s.vals {
			items[k] = v
		}
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, items[k])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// write is RawConfigParser.write: file order, multi-line values continued
// with a tab, a blank line after every section.
func (rc *rawConfig) write() string {
	var b strings.Builder
	for _, s := range append([]*repoSection{rc.defaults}, rc.sections...) {
		if s == nil {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n", s.name)
		for _, k := range s.keys {
			fmt.Fprintf(&b, "%s = %s\n", k, strings.ReplaceAll(s.vals[k], "\n", "\n\t"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// moduleDeprecation is a module-side deprecate() entry of a builtin
// module's result.
func moduleDeprecation(module, msg, version string) map[string]any {
	return map[string]any{
		"collection_name": "ansible.builtin",
		"deprecator":      map[string]any{"resolved_name": "ansible.builtin." + module, "type": "module"},
		"msg":             msg,
		"version":         version,
	}
}

// yumRepositoryModule ports ansible.builtin.yum_repository.
func yumRepositoryModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := yumRepoSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	state := p.Str("state")
	if state == "present" {
		if !p.Has("baseurl") && !p.Has("mirrorlist") && !p.Has("metalink") {
			return agentproto.Fail("state is present but any of the following are missing: baseurl, mirrorlist, metalink")
		}
		if !p.Has("description") {
			return agentproto.Fail("state is present but all of the following are missing: description")
		}
	}
	noLog := &mysqlModule{noLog: append(noLogStrings(p.Any("password")), noLogStrings(p.Any("proxy_password"))...)}

	name := p.Str("name")
	values := map[string]string{}
	for key, def := range yumRepoParams {
		if !p.Has(key) {
			continue
		}
		var v string
		switch def.Type {
		case "bool":
			v = boolTo01(p.Bool(key))
		case "list":
			sep := " "
			if key == "baseurl" || key == "gpgkey" {
				sep = "\n"
			}
			v = strings.Join(strElems(p.List(key)), sep)
		default:
			v = p.Str(key)
		}
		switch key {
		case "name":
			continue
		case "description":
			key = "name"
		}
		values[key] = v
	}

	reposDir := pyExpandPath(p.Str("reposdir"))
	if info, err := os.Stat(reposDir); err != nil || !info.IsDir() {
		return agentproto.Fail("Repo directory '%s' does not exist.", reposDir)
	}
	file := name
	if p.Has("file") {
		file = p.Str("file")
	}
	dest := filepath.Join(reposDir, file+".repo")

	repo, err := readRawConfig(dest)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	before := repo.dump()
	var deprecations []any
	if state == "present" {
		repo.remove(name)
		sec := &repoSection{name: name, vals: map[string]string{}}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch {
			case k == "async":
				deprecations = append(deprecations, moduleDeprecation("yum_repository",
					"'async' parameter is deprecated as it has been removed on systems supported by ansible-core", "2.22"))
			case yumDeprecatedNoEffect[k]:
				deprecations = append(deprecations, moduleDeprecation("yum_repository",
					fmt.Sprintf("'%s' parameter is deprecated as it has no effect with dnf as an underlying package manager.", k), "2.22"))
			}
			sec.keys = append(sec.keys, k)
			sec.vals[k] = values[k]
		}
		repo.sections = append(repo.sections, sec)
	} else {
		repo.remove(name)
	}
	after := repo.dump()
	changed := before != after

	if !env.CheckMode && changed {
		if len(repo.sections) > 0 {
			if err := os.WriteFile(dest, []byte(repo.write()), 0o666); err != nil {
				return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Problems handling file %s.", pyStrRepr(dest)),
					Extra: map[string]any{"details": pyOSError(err)}}
			}
		} else if err := os.Remove(dest); err != nil {
			return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Cannot remove empty repo file %s.", dest),
				Extra: map[string]any{"details": pyOSError(err)}}
		}
	}
	if pathExists(dest) {
		var fail *agentproto.Result
		if changed, fail = setFSAttrs(env, loadFileAttrs(p, dest, true), changed, nil); fail != nil {
			return fail
		}
	}
	res := &agentproto.Result{
		Changed: changed,
		Extra:   map[string]any{"repo": name, "state": state},
		Diff: noLog.censor(map[string]any{
			"before_header": dest, "before": before,
			"after_header": dest, "after": after,
		}),
	}
	if len(deprecations) > 0 {
		res.Extra["deprecations"] = deprecations
	}
	return res
}

func boolTo01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

var sudoersSpec = args.Spec{
	"commands":     {Type: "list"},
	"defaults":     {Type: "list"},
	"group":        {},
	"name":         {Required: true},
	"noexec":       {Type: "bool", Default: false},
	"nopassword":   {Type: "bool", Default: true},
	"setenv":       {Type: "bool", Default: false},
	"host":         {Default: "ALL"},
	"runas":        {},
	"sudoers_path": {Default: "/etc/sudoers.d"},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"user":         {},
	"validation":   {Default: "detect", Choices: []string{"absent", "detect", "required"}},
}

// sudoersContent is Sudoers.content().
func sudoersContent(p *args.Parsed) (string, error) {
	var owner string
	switch {
	case p.Str("user") != "":
		owner = p.Str("user")
	case p.Str("group") != "":
		owner = "%" + p.Str("group")
	default:
		return "", fmt.Errorf("cannot access local variable 'owner' where it is not associated with a value")
	}
	defaults := ""
	if d := strElems(p.List("defaults")); len(d) > 0 {
		lines := make([]string, len(d))
		for i, x := range d {
			lines[i] = "Defaults:" + owner + " " + x
		}
		defaults = strings.Join(lines, "\n") + "\n"
	}
	tags := ""
	if p.Bool("noexec") {
		tags += "NOEXEC:"
	}
	if p.Bool("nopassword") {
		tags += "NOPASSWD:"
	}
	if p.Bool("setenv") {
		tags += "SETENV:"
	}
	runas := ""
	if p.Has("runas") {
		runas = "(" + p.Str("runas") + ")"
	}
	return fmt.Sprintf("%s%s %s=%s%s %s\n", defaults, owner, p.Str("host"), runas, tags,
		strings.Join(strElems(p.List("commands")), ", ")), nil
}

// sudoersModule ports community.general.sudoers.
func sudoersModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if err := sudoersSpec.MutuallyExclusive(rawArgs, []string{"user", "group"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := sudoersSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if p.Str("state") == "present" && !p.Has("commands") {
		return agentproto.Fail("state is present but all of the following are missing: commands")
	}
	path := filepath.Join(p.Str("sudoers_path"), p.Str("name"))
	done := func(changed bool) *agentproto.Result { return &agentproto.Result{Changed: changed} }
	crash := func(err error) *agentproto.Result { return agentproto.Fail("%s", pyOSError(err)) }

	if p.Str("state") == "absent" {
		if !pathExists(path) {
			return done(false)
		}
		if !env.CheckMode {
			if err := os.Remove(path); err != nil {
				return crash(err)
			}
		}
		return done(true)
	}

	content, err := sudoersContent(p)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if v := p.Str("validation"); v != "absent" {
		visudo, lerr := getBinPath("visudo")
		if lerr != nil && v == "required" {
			return agentproto.Fail("%v", lerr)
		}
		if lerr == nil {
			rc, stdout, stderr := runCommand(env, []string{visudo, "-c", "-f", "-"}, cmdOpts{Data: content})
			if rc != 0 {
				out := stdout
				if out == "" {
					out = stderr
				}
				lines := func(s string) []any {
					out := []any{}
					if s == "" {
						return out
					}
					for _, l := range pySplitLines(s) {
						out = append(out, l)
					}
					return out
				}
				// The action adds the *_lines of any stdout/stderr.
				return &agentproto.Result{Failed: true, Msg: "Failed to validate sudoers rule:\n" + out,
					Stdout: stdout, Stderr: stderr, Extra: map[string]any{"stdout": stdout, "stderr": stderr,
						"stdout_lines": lines(stdout), "stderr_lines": lines(stderr)}}
			}
		}
	}
	if info, err := os.Stat(path); err == nil {
		cur, rerr := os.ReadFile(path)
		if rerr != nil {
			return crash(rerr)
		}
		if string(cur) == content && info.Mode().Perm() == 0o440 {
			return done(false)
		}
	}
	if !env.CheckMode {
		if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
			return crash(err)
		}
		if err := os.Chmod(path, 0o440); err != nil {
			return crash(err)
		}
	}
	return done(true)
}

var keypairSpec = fileCommonSpec(args.Spec{
	"state":              {Default: "present", Choices: []string{"present", "absent"}},
	"size":               {Type: "int"},
	"type":               {Default: "rsa", Choices: []string{"rsa", "dsa", "rsa1", "ecdsa", "ed25519"}},
	"force":              {Type: "bool", Default: false},
	"path":               {Required: true},
	"comment":            {},
	"regenerate":         {Default: "partial_idempotence", Choices: []string{"never", "fail", "partial_idempotence", "full_idempotence", "always"}},
	"passphrase":         {},
	"private_key_format": {Default: "auto", Choices: []string{"auto", "pkcs1", "pkcs8", "ssh"}},
	"backend":            {Default: "auto", Choices: []string{"auto", "cryptography", "opensshbin"}},
})

// sshPrivateKey is PrivateKey (ssh-keygen -l output).
type sshPrivateKey struct {
	size        int64
	typ, fp     string
	format      string
	initialized bool
}

func (k *sshPrivateKey) dict() map[string]any {
	return map[string]any{"size": k.size, "type": k.typ, "fingerprint": k.fp, "format": k.format}
}

// sshPublicKey is PublicKey; comment nil means "unset" (matches any).
type sshPublicKey struct {
	typ, data string
	comment   *string
}

func (k *sshPublicKey) String() string { return k.typ + " " + k.data }

func (k *sshPublicKey) equal(o *sshPublicKey) bool {
	if k == nil || o == nil {
		return false
	}
	return k.typ == o.typ && k.data == o.data && (k.comment == nil || o.comment == nil || *k.comment == *o.comment)
}

func (k *sshPublicKey) dict() map[string]any {
	var c any
	if k.comment != nil {
		c = *k.comment
	}
	return map[string]any{"comment": c, "public_key": k.data}
}

func parsePublicKey(s string, trim string) *sshPublicKey {
	parts := strings.SplitN(strings.Trim(s, trim), " ", 3)
	if len(parts) < 2 {
		return nil
	}
	c := ""
	if len(parts) > 2 {
		c = parts[2]
	}
	return &sshPublicKey{typ: parts[0], data: parts[1], comment: &c}
}

// keypairBackend is KeypairBackend with both of its implementations:
// KeypairBackendOpensshBin (ssh-keygen) and, when crypto is set,
// KeypairBackendCryptography, whose python-cryptography keys the sshkey
// package generates, writes and loads natively.
type keypairBackend struct {
	env        *RunEnv
	p          *args.Parsed
	keygen     string
	path, pub  string
	comment    *string
	regenerate string
	typ        string
	size       int64
	changed    bool

	crypto     bool
	passphrase []byte // nil: none
	noBcrypt   bool   // the target's cryptography cannot use the passphrase
	format     string // private key format written: SSH, PKCS1 or PKCS8

	origPriv, priv *sshPrivateKey
	origPub, pubK  *sshPublicKey
}

func keypairFail(msg string) *agentproto.Result { return agentproto.Fail("%s", msg) }

// opensshKeypairModule ports community.crypto.openssh_keypair.
func opensshKeypairModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := keypairSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	// select_backend: ssh-keygen unless a passphrase is set, else the
	// cryptography backend. That one is native here; where the target has
	// a Python, it is available when ansible's would be (python
	// cryptography >= 3.3 installed), so both pick and fail alike.
	keygen, kerr := getBinPath("ssh-keygen")
	canCrypto := pyLibAvailable(env, "cryptography", "3.3")
	backend := p.Str("backend")
	if backend == "auto" {
		switch {
		case kerr == nil && p.Str("passphrase") == "":
			backend = "opensshbin"
		case canCrypto:
			backend = "cryptography"
		default:
			return keypairFail("Cannot find either the OpenSSH binary in the PATH or cryptography >= 3.3 installed on this system")
		}
	}
	if backend == "opensshbin" && kerr != nil {
		return keypairFail("Cannot find the OpenSSH binary in the PATH")
	}
	if backend == "cryptography" && !canCrypto {
		return keypairFail(missingRequiredLib(env, "cryptography >= 3.3", "", ""))
	}

	k := &keypairBackend{env: env, p: p, keygen: keygen, typ: p.Str("type"), regenerate: p.Str("regenerate"),
		crypto: backend == "cryptography"}
	if p.Bool("force") {
		k.regenerate = "always"
	}
	if p.Has("comment") {
		c := p.Str("comment")
		k.comment = &c
	}
	k.path = pyExpandPath(p.Str("path"))
	k.pub = k.path + ".pub"
	if fail := k.setSize(); fail != nil {
		return fail
	}
	base := filepath.Dir(k.path)
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("The directory %s does not exist or the file is not a directory", base),
			Extra: map[string]any{"name": base}}
	}
	if info, err := os.Stat(k.path); err == nil && info.IsDir() {
		return keypairFail(k.path + " is a directory. Please specify a path to a file.")
	}
	if k.crypto {
		if k.typ == "rsa1" {
			return keypairFail("RSA1 keys are not supported by the cryptography backend")
		}
		if pw := p.Str("passphrase"); pw != "" {
			k.passphrase = []byte(pw)
			// cryptography encrypts (and re-encodes) OpenSSH keys with the
			// bcrypt module; without it every passphrase operation fails.
			k.noBcrypt = !pyLibAvailable(env, "bcrypt", "")
		}
		k.format = k.keyFormat()
	} else if p.Str("private_key_format") != "auto" {
		return keypairFail("'auto' is the only valid option for 'private_key_format' when 'backend' is not 'cryptography'")
	}
	if fail := k.execute(); fail != nil {
		return fail
	}
	return k.result()
}

func (k *keypairBackend) setSize() *agentproto.Result {
	size, given := k.p.Int("size"), k.p.Has("size")
	switch k.typ {
	case "rsa", "rsa1":
		if !given {
			size = 4096
		}
		if size < 1024 {
			return keypairFail("For RSA keys, the minimum size is 1024 bits and the default is 4096 bits. " +
				"Attempting to use bit lengths under 1024 will cause the module to fail.")
		}
	case "dsa":
		if !given {
			size = 1024
		}
		if size != 1024 {
			return keypairFail("DSA keys must be exactly 1024 bits as specified by FIPS 186-2.")
		}
	case "ecdsa":
		if !given {
			size = 256
		}
		if size != 256 && size != 384 && size != 521 {
			return keypairFail("For ECDSA keys, size determines the key length by selecting from one of " +
				"three elliptic curve sizes: 256, 384 or 521 bits. " +
				"Attempting to use bit lengths other than these three values for ECDSA keys will " +
				"cause this module to fail.")
		}
	case "ed25519":
		size = 256
	}
	k.size = size
	return nil
}

func (k *keypairBackend) run(argv []string, data string) (int, string, string) {
	return runCommand(k.env, append([]string{k.keygen}, argv...), cmdOpts{Data: data})
}

// keyFormat is _get_key_format: auto writes OpenSSH's own format, or
// PKCS1 for non-ed25519 keys when the target's OpenSSH predates 7.8.
func (k *keypairBackend) keyFormat() string {
	switch f := k.p.Str("private_key_format"); f {
	case "auto":
		v := sshVersion(k.env)
		if v == "" {
			v = "7.8"
		}
		if looseVersionLess(v, "7.8") && k.typ != "ed25519" {
			return sshkey.FormatPKCS1
		}
		return sshkey.FormatSSH
	default:
		return strings.ToUpper(f)
	}
}

// errNeedBcrypt is cryptography's UnsupportedAlgorithm without bcrypt.
var errNeedBcrypt = errors.New("Need bcrypt module")

// loadCrypto is OpensshKeypair.load(no_public_key=True).
func (k *keypairBackend) loadCrypto(passphrase []byte) (any, error) {
	if passphrase != nil && k.noBcrypt {
		return nil, errNeedBcrypt
	}
	data, err := os.ReadFile(k.path)
	if err != nil {
		return nil, &sshkey.ErrInvalid{Msg: "No file was found at " + k.path}
	}
	return sshkey.ParsePrivate(data, passphrase)
}

// privateKeyFormat is parse_private_key_format: the PEM header's format.
func privateKeyFormat(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	switch strings.TrimSpace(line) {
	case "-----BEGIN OPENSSH PRIVATE KEY-----":
		return "SSH"
	case "-----BEGIN PRIVATE KEY-----":
		return "PKCS8"
	case "-----BEGIN RSA PRIVATE KEY-----":
		return "PKCS1"
	}
	return ""
}

func (k *keypairBackend) loadPrivate() *sshPrivateKey {
	if !pathExists(k.path) {
		return nil
	}
	if k.crypto {
		key, err := k.loadCrypto(k.passphrase)
		if err != nil {
			return nil
		}
		typ, size := sshkey.TypeAndSize(key)
		return &sshPrivateKey{size: int64(size), typ: typ, fp: sshkey.Fingerprint(sshkey.AuthorizedKey(key)),
			format: privateKeyFormat(k.path), initialized: true}
	}
	rc, out, _ := k.run([]string{"-l", "-f", k.path}, "")
	if rc != 0 {
		return nil
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return nil
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return nil
	}
	last := f[len(f)-1]
	return &sshPrivateKey{size: n, fp: f[1], typ: strings.ToLower(last[1 : len(last)-1]), initialized: true}
}

func (k *keypairBackend) loadPublic() *sshPublicKey {
	data, err := os.ReadFile(k.pub)
	if err != nil {
		return nil
	}
	return parsePublicKey(string(data), " \n")
}

func (k *keypairBackend) privateReadable() bool {
	if k.crypto {
		if _, err := k.loadCrypto(k.passphrase); err != nil {
			return false
		}
		// A passphrase given for a key that needs none: not readable.
		if k.passphrase != nil {
			_, err := k.loadCrypto(nil)
			return err != nil
		}
		return true
	}
	rc, _, stderr := k.run([]string{"-P", "", "-y", "-f", k.path}, "")
	return !(rc == 255 || strings.Contains(stderr, "is not a public key file") ||
		strings.Contains(stderr, "incorrect passphrase") || strings.Contains(stderr, "load failed"))
}

func (k *keypairBackend) privateValid() bool {
	if k.origPriv == nil || k.origPriv.size != k.size || k.origPriv.typ != k.typ {
		return false
	}
	// _private_key_valid_backend: an explicit format must match the
	// file's (auto never converts an existing key).
	return !k.crypto || k.p.Str("private_key_format") == "auto" || k.format == k.origPriv.format
}

func (k *keypairBackend) shouldGenerate() (bool, *agentproto.Result) {
	switch {
	case k.origPriv == nil:
		return true, nil
	case k.regenerate == "never":
		return false, nil
	case k.regenerate == "fail":
		if !k.privateValid() {
			return false, keypairFail("Key has wrong type and/or size. Will not proceed. " +
				"To force regeneration, call the module with `generate` set to " +
				"`partial_idempotence`, `full_idempotence` or `always`, or with `force=true`.")
		}
		return false, nil
	case k.regenerate == "partial_idempotence" || k.regenerate == "full_idempotence":
		return !k.privateValid(), nil
	}
	return true, nil
}

func (k *keypairBackend) execute() *agentproto.Result {
	k.origPriv = k.loadPrivate()
	k.origPub = k.loadPublic()
	if k.p.Str("state") == "absent" {
		if pathExists(k.path) || pathExists(k.pub) {
			if !k.env.CheckMode {
				for _, f := range []string{k.path, k.pub} {
					if pathExists(f) {
						if err := os.Remove(f); err != nil {
							return keypairFail(pyOSError(err))
						}
					}
				}
			}
			k.changed = true
		}
		return nil
	}
	if pathExists(k.path) && (k.regenerate == "never" || k.regenerate == "fail" || k.regenerate == "partial_idempotence") &&
		(k.origPriv == nil || !k.privateReadable()) {
		return keypairFail("Unable to read the key. The key is protected with a passphrase or broken. " +
			"Will not proceed. To force regeneration, call the module with `generate` " +
			"set to `full_idempotence` or `always`, or with `force=true`.")
	}
	gen, fail := k.shouldGenerate()
	if fail != nil {
		return fail
	}
	if gen {
		if fail := k.generate(); fail != nil {
			return fail
		}
	} else if !k.publicValid() {
		if fail := k.restorePublic(); fail != nil {
			return fail
		}
	}
	k.priv = k.loadPrivate()
	k.pubK = k.loadPublic()
	for _, path := range []string{k.path, k.pub} {
		if k.env.CheckMode && !pathExists(path) {
			k.changed = true
			continue
		}
		fa := loadFileAttrs(k.p, path, true)
		var fail *agentproto.Result
		if k.changed, fail = setFSAttrs(k.env, fa, k.changed, nil); fail != nil {
			return fail
		}
	}
	return nil
}

func (k *keypairBackend) generate() *agentproto.Result {
	k.changed = true
	if k.env.CheckMode {
		return nil
	}
	dir, err := os.MkdirTemp("", "ansible-moduletmp-")
	if err != nil {
		return keypairFail(pyOSError(err))
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, filepath.Base(k.path))
	comment := ""
	if k.comment != nil {
		comment = *k.comment
	}
	if k.crypto {
		if fail := k.generateCrypto(tmp, comment); fail != nil {
			return fail
		}
		return k.secureMove([][2]string{{tmp, k.path}, {tmp + ".pub", k.pub}})
	}
	rc, out, stderr := k.run([]string{"-q", "-N", "", "-b", strconv.FormatInt(k.size, 10), "-t", k.typ, "-f", tmp, "-C", comment}, "")
	if rc != 0 {
		return &agentproto.Result{Failed: true, Msg: strings.TrimSpace(stderr), RC: agentproto.IntPtr(rc), Stdout: out, Stderr: stderr}
	}
	return k.secureMove([][2]string{{tmp, k.path}, {tmp + ".pub", k.pub}})
}

// generateCrypto is KeypairBackendCryptography._generate_keypair: the
// private key (0600) in the chosen format, and the public key (0644)
// with the comment and no trailing newline.
func (k *keypairBackend) generateCrypto(tmp, comment string) *agentproto.Result {
	if k.passphrase != nil && k.noBcrypt {
		return keypairFail("unexpected error occurred: " + errNeedBcrypt.Error())
	}
	if k.typ == "rsa" && k.size >= 16384 {
		return keypairFail(fmt.Sprintf("unexpected error occurred: %d is not a valid key size for rsa keys", k.size))
	}
	key, err := sshkey.Generate(k.typ, int(k.size))
	var pem []byte
	if err == nil {
		pem, err = sshkey.MarshalPrivate(key, k.format, k.passphrase)
	}
	if err != nil {
		return keypairFail("unexpected error occurred: " + err.Error())
	}
	pub := sshkey.AuthorizedKey(key)
	if comment != "" {
		pub += " " + comment
	}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{tmp, pem, 0o600}, {tmp + ".pub", []byte(pub), 0o644}} {
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			return keypairFail(pyOSError(err))
		}
		if err := os.Chmod(f.path, f.mode); err != nil {
			return keypairFail(pyOSError(err))
		}
	}
	return nil
}

// secureMove is _safe_secure_move: an existing destination is replaced
// keeping its attributes, a new one keeps the source's permissions.
func (k *keypairBackend) secureMove(pairs [][2]string) *agentproto.Result {
	for _, sd := range pairs {
		data, err := os.ReadFile(sd[0])
		if err != nil {
			return keypairFail(pyOSError(err))
		}
		mode := os.FileMode(0o600)
		// preserved_copy keeps the source's owner and group for a new file;
		// atomic_move keeps the replaced file's.
		owner := sd[0]
		if info, err := os.Stat(sd[1]); err == nil {
			mode, owner = info.Mode().Perm(), sd[1]
		} else if info, err := os.Stat(sd[0]); err == nil {
			mode = info.Mode().Perm()
		}
		tmp := sd[1] + ".understudy-tmp"
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return keypairFail(pyOSError(err))
		}
		os.Chmod(tmp, mode)
		if info, err := os.Stat(owner); err == nil {
			if uid, gid, ok := statIDsOf(info); ok {
				os.Chown(tmp, uid, gid)
			}
		}
		if err := os.Rename(tmp, sd[1]); err != nil {
			os.Remove(tmp)
			return keypairFail(pyOSError(err))
		}
	}
	return nil
}

func (k *keypairBackend) matchingPublic() *sshPublicKey {
	if k.crypto {
		key, err := k.loadCrypto(k.passphrase)
		if err != nil {
			return nil // simulates ssh-keygen's empty output
		}
		return parsePublicKey(sshkey.AuthorizedKey(key), "\n")
	}
	_, out, _ := k.run([]string{"-P", "", "-y", "-f", k.path}, "")
	return parsePublicKey(out, "\n")
}

func (k *keypairBackend) publicValid() bool {
	if k.origPub == nil {
		return false
	}
	valid := k.matchingPublic()
	if valid == nil {
		return false
	}
	valid.comment = k.comment
	return k.origPub.equal(valid)
}

func (k *keypairBackend) restorePublic() *agentproto.Result {
	k.changed = true
	if k.env.CheckMode {
		return nil
	}
	pub := k.matchingPublic()
	dir, err := os.MkdirTemp("", "ansible-moduletmp-")
	if err != nil {
		return keypairFail(pyOSError(err))
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, filepath.Base(k.pub))
	mode := os.FileMode(0o644)
	if info, err := os.Stat(k.pub); err == nil && info.Mode().Perm() != 0 {
		mode = info.Mode().Perm()
	}
	content := ""
	if pub != nil {
		content = pub.String()
	}
	if err := os.WriteFile(tmp, []byte(content+"\n"), mode); err != nil {
		return keypairFail(pyOSError(err))
	}
	if fail := k.secureMove([][2]string{{tmp, k.pub}}); fail != nil {
		return keypairFail("The public key is missing or does not match the private key. Unable to regenerate the public key.")
	}
	if k.comment != nil && *k.comment != "" && k.crypto {
		// _update_comment: the public key rewritten with the comment.
		key, err := k.loadCrypto(k.passphrase)
		if err != nil {
			return keypairFail("unexpected error occurred: " + err.Error())
		}
		tmp2 := filepath.Join(dir, filepath.Base(k.pub)+".comment")
		mode := os.FileMode(0o644)
		if info, err := os.Stat(k.pub); err == nil && info.Mode().Perm() != 0 {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(tmp2, []byte(sshkey.AuthorizedKey(key)+" "+*k.comment+"\n"), mode); err != nil {
			return keypairFail(pyOSError(err))
		}
		return k.secureMove([][2]string{{tmp2, k.pub}})
	}
	if k.comment != nil && *k.comment != "" {
		if info, err := os.Stat(k.path); err == nil && info.Mode().Perm()&0o200 == 0 {
			os.Chmod(k.path, 0o600)
		}
		argv := []string{"-q"}
		if v := sshVersion(k.env); v != "" && !looseVersionLess(v, "6.5") && looseVersionLess(v, "7.8") {
			argv = append(argv, "-o")
		}
		argv = append(argv, "-c", "-C", *k.comment, "-f", k.path)
		if rc, out, stderr := k.run(argv, ""); rc != 0 {
			return &agentproto.Result{Failed: true, Msg: strings.TrimSpace(stderr), RC: agentproto.IntPtr(rc), Stdout: out, Stderr: stderr}
		}
	}
	return nil
}

var opensshVersionRe = regexp.MustCompile(`^.*openssh_([0-9.]+)(p?[0-9]+)[^0-9]*.*$`)

// sshVersion is _get_ssh_version: the version `ssh -V` reports.
func sshVersion(env *RunEnv) string {
	ssh, err := getBinPath("ssh")
	if err != nil {
		return ""
	}
	_, _, stderr := runCommand(env, []string{ssh, "-V", "-q"}, cmdOpts{})
	if m := opensshVersionRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(stderr))); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func (k *keypairBackend) result() *agentproto.Result {
	priv := k.priv
	if priv == nil {
		priv = k.origPriv
	}
	pub := k.pubK
	if pub == nil {
		pub = k.origPub
	}
	extra := map[string]any{"size": k.size, "type": k.typ, "filename": k.path,
		"fingerprint": "", "public_key": "", "comment": ""}
	if priv != nil {
		extra["fingerprint"] = priv.fp
	}
	if pub != nil {
		extra["public_key"] = pub.String()
		if pub.comment != nil {
			extra["comment"] = *pub.comment
		} else {
			extra["comment"] = nil
		}
	}
	res := &agentproto.Result{Changed: k.changed, Extra: extra}
	if k.env.DiffMode {
		before, after := map[string]any{}, map[string]any{}
		if k.origPriv != nil {
			for kk, v := range k.origPriv.dict() {
				before[kk] = v
			}
		}
		if k.origPub != nil {
			for kk, v := range k.origPub.dict() {
				before[kk] = v
			}
		}
		if k.priv != nil {
			for kk, v := range k.priv.dict() {
				after[kk] = v
			}
		}
		if k.pubK != nil {
			for kk, v := range k.pubK.dict() {
				after[kk] = v
			}
		}
		res.Diff = map[string]any{"before": before, "after": after}
	}
	return res
}
