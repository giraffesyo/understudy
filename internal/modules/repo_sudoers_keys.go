package modules

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(yumRepositoryModule, "yum_repository", "ansible.builtin.yum_repository")
	Register(sudoersModule, "sudoers", "community.general.sudoers")
	Register(opensshKeypairModule, "openssh_keypair", "community.crypto.openssh_keypair")
}

var yumRepoSpec = args.Spec{
	"name":        {Required: true},
	"description": {},
	"baseurl":     {},
	"metalink":    {},
	"mirrorlist":  {},
	"enabled":     {Type: "bool", Default: true},
	"gpgcheck":    {Type: "bool"},
	"gpgkey":      {},
	"state":       {Default: "present", Choices: []string{"present", "absent"}},
	"file":        {},
	"reposdir":    {Default: "/etc/yum.repos.d"},
	"priority":    {Type: "int"},
	"sslverify":   {Type: "bool"},
	"exclude":     {},
}

// yumRepositoryModule writes /etc/yum.repos.d/<file>.repo entries.
func yumRepositoryModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := yumRepoSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	fileBase := p.Str("file")
	if fileBase == "" {
		fileBase = name
	}
	path := p.Str("reposdir") + "/" + fileBase + ".repo"
	res := &agentproto.Result{Extra: map[string]any{"repo": name, "path": path}}

	if p.Str("state") == "absent" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return res
		}
		res.Changed = true
		if !env.CheckMode {
			if err := os.Remove(path); err != nil {
				return agentproto.Fail("removing %s: %v", path, err)
			}
		}
		return res
	}

	if p.Str("baseurl") == "" && p.Str("metalink") == "" && p.Str("mirrorlist") == "" {
		return agentproto.Fail("yum_repository requires baseurl, metalink, or mirrorlist")
	}

	kv := map[string]string{}
	if v := p.Str("description"); v != "" {
		kv["name"] = v
	}
	for _, key := range []string{"baseurl", "metalink", "mirrorlist", "gpgkey", "exclude"} {
		if v := p.Str(key); v != "" {
			kv[key] = v
		}
	}
	kv["enabled"] = boolTo01(p.Bool("enabled"))
	if p.Has("gpgcheck") {
		kv["gpgcheck"] = boolTo01(p.Bool("gpgcheck"))
	}
	if p.Has("sslverify") {
		kv["sslverify"] = boolTo01(p.Bool("sslverify"))
	}
	if p.Has("priority") {
		kv["priority"] = fmt.Sprintf("%d", p.Int("priority"))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", name)
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, kv[k])
	}
	newContent := []byte(b.String())

	original, rerr := os.ReadFile(path)
	if rerr == nil && bytes.Equal(original, newContent) {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if err := fsutil.AtomicWrite(path, bytes.NewReader(newContent), 0o644); err != nil {
		return agentproto.Fail("writing %s: %v", path, err)
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
	"name":         {Required: true},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"user":         {},
	"group":        {},
	"commands":     {Type: "list"},
	"nopassword":   {Type: "bool", Default: true},
	"runas":        {},
	"sudoers_path": {Default: "/etc/sudoers.d"},
	"validation":   {Default: "detect", Choices: []string{"detect", "required", "absent"}},
}

// sudoersModule writes validated /etc/sudoers.d entries.
func sudoersModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := sudoersSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	path := p.Str("sudoers_path") + "/" + name
	res := &agentproto.Result{Extra: map[string]any{"path": path}}

	if p.Str("state") == "absent" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return res
		}
		res.Changed = true
		if !env.CheckMode {
			if err := os.Remove(path); err != nil {
				return agentproto.Fail("removing %s: %v", path, err)
			}
		}
		return res
	}

	who := p.Str("user")
	if who == "" {
		if g := p.Str("group"); g != "" {
			who = "%" + g
		} else {
			return agentproto.Fail("sudoers requires 'user' or 'group'")
		}
	}
	commands := stringList(p.List("commands"))
	if len(commands) == 0 {
		commands = []string{"ALL"}
	}
	runas := p.Str("runas")
	if runas == "" {
		runas = "ALL"
	}
	tag := ""
	if p.Bool("nopassword") {
		tag = "NOPASSWD: "
	}
	line := fmt.Sprintf("%s ALL=(%s) %s%s\n", who, runas, tag, strings.Join(commands, ", "))
	newContent := []byte(line)

	original, rerr := os.ReadFile(path)
	if rerr == nil && bytes.Equal(original, newContent) {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if err := fsutil.AtomicWrite(path, bytes.NewReader(newContent), 0o440); err != nil {
		return agentproto.Fail("writing %s: %v", path, err)
	}
	// Validate; roll back a broken file rather than locking sudo up.
	if p.Str("validation") != "absent" {
		if out, err := runOut(env, "visudo", "-cf", path); err != nil {
			os.Remove(path)
			if rerr == nil {
				fsutil.AtomicWrite(path, bytes.NewReader(original), 0o440)
			}
			return agentproto.Fail("sudoers validation failed (rolled back): %s", tail(out))
		}
	}
	return res
}

var keypairSpec = args.Spec{
	"path":    {Required: true},
	"type":    {Default: "rsa", Choices: []string{"rsa", "ed25519", "ecdsa", "dsa"}},
	"size":    {Type: "int"},
	"state":   {Default: "present", Choices: []string{"present", "absent"}},
	"comment": {},
	"force":   {Type: "bool", Default: false},
	"mode":    {Type: "any"},
	"owner":   {},
	"group":   {},
}

// opensshKeypairModule generates SSH keypairs via ssh-keygen.
func opensshKeypairModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := keypairSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")
	res := &agentproto.Result{Extra: map[string]any{"filename": path}}

	_, statErr := os.Stat(path)
	exists := statErr == nil

	if p.Str("state") == "absent" {
		if !exists {
			return res
		}
		res.Changed = true
		if !env.CheckMode {
			os.Remove(path)
			os.Remove(path + ".pub")
		}
		return res
	}

	if exists && !p.Bool("force") {
		if pub, err := os.ReadFile(path + ".pub"); err == nil {
			res.Extra["public_key"] = strings.TrimSpace(string(pub))
		}
		return applyKeyAttrs(env, p, path, res)
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if exists {
		os.Remove(path)
		os.Remove(path + ".pub")
	}
	argv := []string{"-t", p.Str("type"), "-f", path, "-N", "", "-q"}
	if p.Has("size") {
		argv = append(argv, "-b", fmt.Sprintf("%d", p.Int("size")))
	}
	if c := p.Str("comment"); c != "" {
		argv = append(argv, "-C", c)
	}
	if out, err := runOut(env, "ssh-keygen", argv...); err != nil {
		return agentproto.Fail("ssh-keygen failed: %v: %s", err, tail(out))
	}
	if pub, err := os.ReadFile(path + ".pub"); err == nil {
		res.Extra["public_key"] = strings.TrimSpace(string(pub))
	}
	return applyKeyAttrs(env, p, path, res)
}

func applyKeyAttrs(env *RunEnv, p *args.Parsed, path string, res *agentproto.Result) *agentproto.Result {
	if env.CheckMode || (!p.Has("mode") && p.Str("owner") == "" && p.Str("group") == "") {
		return res
	}
	var mode any
	if p.Has("mode") {
		mode = p.Any("mode")
	}
	changed, err := fsutil.ApplyFileAttrs(path, mode, p.Str("owner"), p.Str("group"), true)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	res.Changed = res.Changed || changed
	return res
}
