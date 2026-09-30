package modules

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports ansible.builtin.user for Linux: the generic User class
// (useradd/usermod/userdel, chage, ssh-keygen; luser* with local) and the
// BusyBox class Alpine and Buildroot use (adduser/deluser/delgroup,
// chpasswd, /etc/passwd edited in place).

func osEnviron() []string { return os.Environ() }

func init() {
	Register(userModule, "user", "ansible.builtin.user")
}

var userSpec = args.Spec{
	"state":                           {Default: "present", Choices: []string{"absent", "present"}},
	"name":                            {Required: true, Aliases: []string{"user"}},
	"uid":                             {Type: "int"},
	"non_unique":                      {Type: "bool", Default: false},
	"group":                           {},
	"groups":                          {Type: "list"},
	"comment":                         {},
	"home":                            {},
	"shell":                           {},
	"password":                        {},
	"login_class":                     {},
	"password_expire_max":             {Type: "int"},
	"password_expire_min":             {Type: "int"},
	"password_expire_warn":            {Type: "int"},
	"hidden":                          {Type: "bool"},
	"seuser":                          {},
	"force":                           {Type: "bool", Default: false},
	"remove":                          {Type: "bool", Default: false},
	"create_home":                     {Type: "bool", Default: true, Aliases: []string{"createhome"}},
	"skeleton":                        {},
	"system":                          {Type: "bool", Default: false},
	"move_home":                       {Type: "bool", Default: false},
	"append":                          {Type: "bool", Default: false},
	"generate_ssh_key":                {Type: "bool"},
	"ssh_key_bits":                    {Type: "int", Default: 0},
	"ssh_key_type":                    {Default: "rsa"},
	"ssh_key_file":                    {},
	"ssh_key_comment":                 {},
	"ssh_key_passphrase":              {},
	"update_password":                 {Default: "always", Choices: []string{"always", "on_create"}},
	"expires":                         {Type: "float"},
	"password_lock":                   {Type: "bool"},
	"local":                           {Type: "bool"},
	"profile":                         {},
	"authorization":                   {},
	"role":                            {},
	"umask":                           {},
	"password_expire_account_disable": {Type: "int"},
	"uid_min":                         {Type: "int"},
	"uid_max":                         {Type: "int"},
}

// placeholderHomes are conventional system-account homes that are never
// created on disk.
var placeholderHomes = map[string]bool{"/nonexistent": true, "/dev/null": true, "/var/empty": true}

var hashRe = regexp.MustCompile(`[^a-zA-Z0-9./=]`)

// userAbort carries a fail_json (or exit_json) out of deep helpers.
type userAbort struct{ res *agentproto.Result }

// pwEntry is a passwd entry: name, passwd, uid, gid, gecos, dir, shell.
type pwEntry struct {
	name, passwd   string
	uid, gid       int64
	gecos, dir, sh string
}

// grEntry is a group entry.
type grEntry struct {
	name string
	gid  int64
	mem  []string
}

// spEntry is the part of a shadow entry the module reads.
type spEntry struct {
	pwd                    string
	min, max, warn, expire int64
}

type userRun struct {
	env *RunEnv
	p   *args.Parsed

	name, state                     string
	uid                             *int64
	group, comment, shell, home     *string
	password                        *string
	skeleton, seuser, umask         *string
	groups                          *string
	sshFile                         string
	expires                         *time.Time
	passwordLock                    *bool
	local                           bool
	inactive                        *int64
	uidMin, uidMax                  *int64
	expireMin, expireMax, expireWrn *int64
	busybox                         bool

	warnings []string
}

func (u *userRun) fail(msg string, extra map[string]any) {
	res := agentproto.Fail("%s", msg)
	if extra != nil {
		res.Extra = extra
	}
	panic(userAbort{res})
}

func (u *userRun) binPath(name string) string {
	p, err := getBinPath(name)
	if err != nil {
		u.fail(err.Error(), nil)
	}
	return p
}

// execute is execute_command: in check mode (unless told otherwise) a
// command is only reported as run.
func (u *userRun) execute(argv []string, data *string, obeyCheck bool) (int, string, string) {
	if u.env.CheckMode && obeyCheck {
		return 0, "", ""
	}
	o := cmdOpts{}
	if data != nil {
		o.Data = *data
	}
	return runCommand(u.env, argv, o)
}

// --- account database ----------------------------------------------------

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// getentLines returns `getent <db> [key]` lines, or the file's lines when
// getent is unavailable.
func getentLines(db, key string) []string {
	argv := []string{"getent", db}
	if key != "" {
		argv = append(argv, key)
	}
	if _, err := lookPath("getent"); err == nil {
		rc, out, _ := runCommand(&RunEnv{}, argv, cmdOpts{})
		if rc != 0 {
			return nil
		}
		return strings.Split(strings.TrimRight(out, "\n"), "\n")
	}
	data, err := os.ReadFile("/etc/" + db)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if key == "" || strings.HasPrefix(l, key+":") {
			out = append(out, l)
		}
	}
	return out
}

func getpwnam(name string) (*pwEntry, bool) {
	for _, l := range getentLines("passwd", name) {
		f := strings.Split(l, ":")
		if len(f) < 7 || f[0] != name {
			continue
		}
		return &pwEntry{name: f[0], passwd: f[1], uid: parseInt64(f[2]), gid: parseInt64(f[3]),
			gecos: f[4], dir: f[5], sh: f[6]}, true
	}
	return nil, false
}

// getgrall is grp.getgrall(), in database order.
func getgrall() []grEntry {
	var out []grEntry
	for _, l := range getentLines("group", "") {
		f := strings.Split(l, ":")
		if len(f) < 3 { // musl's getent drops the empty member list
			continue
		}
		var mem []string
		if len(f) > 3 && f[3] != "" {
			mem = strings.Split(f[3], ",")
		}
		out = append(out, grEntry{name: f[0], gid: parseInt64(f[2]), mem: mem})
	}
	return out
}

// groupInfo is group_info: the group as a gid first, then as a name.
func groupInfo(group string) (*grEntry, bool) {
	all := getgrall()
	if gid, err := strconv.ParseInt(strings.TrimSpace(group), 10, 64); err == nil {
		for i := range all {
			if all[i].gid == gid {
				return &all[i], true
			}
		}
	}
	for i := range all {
		if all[i].name == group {
			return &all[i], true
		}
	}
	return nil, false
}

func groupExists(group string) bool {
	_, ok := groupInfo(group)
	return ok
}

// getspnam reads the user's shadow entry (nil when there is none or the
// shadow file is unreadable, where libc's getspnam returns NULL).
func getspnam(name string) *spEntry {
	data, err := os.ReadFile("/etc/shadow")
	if err != nil {
		return nil
	}
	num := func(s string) int64 {
		if s == "" {
			return -1
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return -1
		}
		return n
	}
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Split(l, ":")
		if len(f) < 9 || f[0] != name {
			continue
		}
		return &spEntry{pwd: f[1], min: num(f[3]), max: num(f[4]), warn: num(f[5]), expire: num(f[7])}
	}
	return nil
}

func (u *userRun) userExists() bool {
	if u.local {
		data, err := os.ReadFile("/etc/passwd")
		if err != nil {
			u.fail("'local: true' specified but unable to find local account file /etc/passwd to parse.", nil)
		}
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, u.name+":") {
				return true
			}
		}
		return false
	}
	_, ok := getpwnam(u.name)
	return ok
}

// userInfo is user_info: the passwd entry with the shadow hash in place
// of a one-character placeholder.
func (u *userRun) userInfo() (*pwEntry, bool) {
	if !u.userExists() {
		return nil, false
	}
	info, ok := getpwnam(u.name)
	if !ok {
		return nil, false
	}
	if len(info.passwd) <= 1 {
		info.passwd, _ = u.userPassword()
	}
	return info, true
}

// userPassword is user_password: (hash, sp_expire) from shadow.
func (u *userRun) userPassword() (string, *int64) {
	sp := getspnam(u.name)
	if sp == nil {
		return "", nil
	}
	return sp.pwd, &sp.expire
}

func (u *userRun) membership(excludePrimary bool) []string {
	info, ok := getpwnam(u.name)
	var out []string
	for _, g := range getgrall() {
		if !containsString(g.mem, u.name) {
			continue
		}
		if !excludePrimary || !ok || info.gid != g.gid {
			out = append(out, g.name)
		}
	}
	return out
}

// groupsSet is get_groups_set: the requested groups (validated), without
// the primary group when removeExisting, or as names when namesOnly.
func (u *userRun) groupsSet(removeExisting, namesOnly bool) []string {
	if u.groups == nil {
		return nil
	}
	info, hasInfo := u.userInfo()
	seen := map[string]bool{}
	var out []string
	for _, g := range strings.Split(*u.groups, ",") {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		gi, ok := groupInfo(g)
		if !ok {
			u.fail("Group "+g+" does not exist", nil)
		}
		if hasInfo && removeExisting && gi.gid == info.gid {
			continue
		}
		if namesOnly {
			if !containsString(out, gi.name) {
				out = append(out, gi.name)
			}
		} else {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// --- create / modify / remove ------------------------------------------

func (u *userRun) removeUserdel() (int, string, string) {
	if u.busybox {
		cmd := []string{u.binPath("deluser"), u.name}
		if u.p.Bool("remove") {
			cmd = append(cmd, "--remove-home")
		}
		return u.execute(cmd, nil, true)
	}
	name := "userdel"
	if u.local {
		name = "luserdel"
	}
	cmd := []string{u.binPath(name)}
	if u.p.Bool("force") && !u.local {
		cmd = append(cmd, "-f")
	}
	if u.p.Bool("remove") {
		cmd = append(cmd, "-r")
	}
	return u.execute(append(cmd, u.name), nil, true)
}

func epochStart(t *time.Time) bool { return t.Before(time.Unix(0, 0)) }

func (u *userRun) createUseradd() (int, string, string) {
	name := "useradd"
	var lgroupmod, lchage string
	if u.local {
		name = "luseradd"
		lgroupmod = u.binPath("lgroupmod")
		lchage = u.binPath("lchage")
	}
	cmd := []string{u.binPath(name)}
	if u.uid != nil {
		cmd = append(cmd, "-u", strconv.FormatInt(*u.uid, 10))
		if u.p.Bool("non_unique") {
			cmd = append(cmd, "-o")
		}
	}
	if u.seuser != nil && selinuxEnabled() {
		cmd = append(cmd, "-Z", *u.seuser)
	}
	if u.group != nil {
		if !groupExists(*u.group) {
			u.fail("Group "+*u.group+" does not exist", nil)
		}
		cmd = append(cmd, "-g", *u.group)
	} else if groupExists(u.name) {
		if u.local {
			cmd = append(cmd, "-n")
		} else if pathExists("/etc/redhat-release") {
			v := loadDistro(newFactEnv("", 10*time.Second)).version(false)
			major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
			if major <= 5 {
				cmd = append(cmd, "-n")
			} else {
				cmd = append(cmd, "-N")
			}
		} else if pathExists("/etc/SuSE-release") {
			v := loadDistro(newFactEnv("", 10*time.Second)).version(false)
			if major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); major >= 12 {
				cmd = append(cmd, "-N")
			}
		} else {
			cmd = append(cmd, "-N")
		}
	}
	var groups []string
	if u.groups != nil && *u.groups != "" {
		groups = u.groupsSet(true, false)
		if !u.local {
			cmd = append(cmd, "-G", strings.Join(groups, ","))
		}
	}
	if u.comment != nil {
		cmd = append(cmd, "-c", *u.comment)
	}
	if u.home != nil {
		if u.p.Bool("create_home") && !isDir(pyDirname(*u.home)) {
			u.createHomedir(*u.home)
		}
		cmd = append(cmd, "-d", *u.home)
	}
	if u.shell != nil {
		cmd = append(cmd, "-s", *u.shell)
	}
	if u.expires != nil && !u.local {
		if epochStart(u.expires) {
			cmd = append(cmd, "-e", "")
		} else {
			cmd = append(cmd, "-e", u.expires.Format("2006-01-02"))
		}
	}
	if u.inactive != nil {
		cmd = append(cmd, "-f", strconv.FormatInt(*u.inactive, 10))
	}
	if u.password != nil {
		if u.passwordLock != nil && *u.passwordLock {
			cmd = append(cmd, "-p", "!"+*u.password)
		} else {
			cmd = append(cmd, "-p", *u.password)
		}
	}
	if u.p.Bool("create_home") {
		if !u.local {
			cmd = append(cmd, "-m")
		}
		if u.skeleton != nil {
			cmd = append(cmd, "-k", *u.skeleton)
		}
		if u.umask != nil {
			cmd = append(cmd, "-K", "UMASK="+*u.umask)
		}
	} else {
		cmd = append(cmd, "-M")
	}
	if u.p.Bool("system") {
		cmd = append(cmd, "-r")
	}
	if u.uidMin != nil {
		cmd = append(cmd, "-K", "UID_MIN="+strconv.FormatInt(*u.uidMin, 10))
	}
	if u.uidMax != nil {
		cmd = append(cmd, "-K", "UID_MAX="+strconv.FormatInt(*u.uidMax, 10))
	}
	rc, out, errOut := u.execute(append(cmd, u.name), nil, true)
	if !u.local || rc != 0 {
		return rc, out, errOut
	}
	if u.expires != nil {
		lexpires := int64(-1)
		if !epochStart(u.expires) {
			lexpires = int64(math.Floor(u.p.Float("expires"))) / 86400
		}
		var o, e string
		rc, o, e = u.execute([]string{lchage, "-E", strconv.FormatInt(lexpires, 10), u.name}, nil, true)
		out, errOut = out+o, errOut+e
		if rc != 0 {
			return rc, out, errOut
		}
	}
	if u.groups == nil || *u.groups == "" {
		return rc, out, errOut
	}
	for _, g := range groups {
		var o, e string
		rc, o, e = u.execute([]string{lgroupmod, "-M", u.name, g}, nil, true)
		out, errOut = out+o, errOut+e
		if rc != 0 {
			return rc, out, errOut
		}
	}
	return rc, out, errOut
}

// usermodHasAppend is _check_usermod_append.
func (u *userRun) usermodHasAppend(bin string) bool {
	if !accessOK(bin, 1) {
		return false
	}
	_, o, e := u.execute([]string{bin, "--help"}, nil, false)
	for _, l := range strings.Split(o+e, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "-a, --append") {
			return true
		}
	}
	return false
}

func (u *userRun) modifyUsermod() (*int, string, string) {
	name := "usermod"
	var lgroupmod, lchage string
	var lgroupAdd, lgroupDel []string
	var lexpires *int64
	if u.local {
		name = "lusermod"
		lgroupmod = u.binPath("lgroupmod")
		lchage = u.binPath("lchage")
	}
	bin := u.binPath(name)
	cmd := []string{bin}
	info, _ := u.userInfo()
	hasAppend := u.usermodHasAppend(bin)

	if u.uid != nil && info.uid != *u.uid {
		cmd = append(cmd, "-u", strconv.FormatInt(*u.uid, 10))
		if u.p.Bool("non_unique") {
			cmd = append(cmd, "-o")
		}
	}
	if u.group != nil {
		if !groupExists(*u.group) {
			u.fail("Group "+*u.group+" does not exist", nil)
		}
		gi, _ := groupInfo(*u.group)
		if info.gid != gi.gid {
			cmd = append(cmd, "-g", strconv.FormatInt(gi.gid, 10))
		}
	}
	if u.groups != nil {
		current := u.membership(false)
		needMod := false
		var groups []string
		var diff []string
		if *u.groups == "" {
			if len(current) > 0 && !u.p.Bool("append") {
				needMod = true
			}
		} else {
			groups = u.groupsSet(false, true)
			diff = symDiff(current, groups)
			if len(diff) > 0 {
				if u.p.Bool("append") {
					for _, g := range groups {
						if containsString(diff, g) {
							if hasAppend {
								cmd = append(cmd, "-a")
							}
							needMod = true
							break
						}
					}
				} else {
					needMod = true
				}
			}
		}
		if needMod {
			if u.local {
				lgroupAdd = setMinus(groups, current)
				if !u.p.Bool("append") {
					lgroupDel = setMinus(current, groups)
				}
			} else if u.p.Bool("append") && !hasAppend {
				cmd = append(cmd, "-A", strings.Join(diff, ","))
			} else {
				cmd = append(cmd, "-G", strings.Join(groups, ","))
			}
		}
	}
	if u.comment != nil && info.gecos != *u.comment {
		cmd = append(cmd, "-c", *u.comment)
	}
	if u.home != nil && info.dir != *u.home {
		cmd = append(cmd, "-d", *u.home)
		if u.p.Bool("move_home") {
			cmd = append(cmd, "-m")
		}
	}
	if u.shell != nil && info.sh != *u.shell {
		cmd = append(cmd, "-s", *u.shell)
	}
	if u.expires != nil {
		_, cur := u.userPassword()
		current := int64(0)
		if cur != nil && *cur != 0 {
			current = *cur
		}
		if epochStart(u.expires) {
			if current >= 0 {
				if u.local {
					v := int64(-1)
					lexpires = &v
				} else {
					cmd = append(cmd, "-e", "")
				}
			}
		} else {
			curDate := time.Unix(current*86400, 0).UTC().Format("2006-01-02")
			if current < 0 || curDate != u.expires.Format("2006-01-02") {
				if u.local {
					v := int64(math.Floor(u.p.Float("expires"))) / 86400
					lexpires = &v
				} else {
					cmd = append(cmd, "-e", u.expires.Format("2006-01-02"))
				}
			}
		}
	}
	if u.inactive != nil {
		cmd = append(cmd, "-f", strconv.FormatInt(*u.inactive, 10))
	}
	if u.passwordLock != nil && *u.passwordLock && !strings.HasPrefix(info.passwd, "!") {
		cmd = append(cmd, "-L")
	} else if u.passwordLock != nil && !*u.passwordLock && strings.HasPrefix(info.passwd, "!") {
		cmd = append(cmd, "-U")
	}
	if u.p.Str("update_password") == "always" && u.password != nil &&
		strings.TrimLeft(info.passwd, "!") != strings.TrimLeft(*u.password, "!") {
		kept := cmd[:0:0]
		for _, c := range cmd {
			if c != "-U" && c != "-L" {
				kept = append(kept, c)
			}
		}
		cmd = kept
		if u.passwordLock != nil && *u.passwordLock {
			cmd = append(cmd, "-p", "!"+*u.password)
		} else {
			cmd = append(cmd, "-p", *u.password)
		}
	}

	var rc *int
	out, errOut := "", ""
	if len(cmd) > 1 {
		r, o, e := u.execute(append(cmd, u.name), nil, true)
		rc, out, errOut = &r, o, e
	}
	if !u.local || (rc != nil && *rc != 0) {
		return rc, out, errOut
	}
	if lexpires != nil {
		r, o, e := u.execute([]string{lchage, "-E", strconv.FormatInt(*lexpires, 10), u.name}, nil, true)
		rc, out, errOut = &r, out+o, errOut+e
		if r != 0 {
			return rc, out, errOut
		}
	}
	for _, g := range lgroupAdd {
		r, o, e := u.execute([]string{lgroupmod, "-M", u.name, g}, nil, true)
		rc, out, errOut = &r, out+o, errOut+e
		if r != 0 {
			return rc, out, errOut
		}
	}
	for _, g := range lgroupDel {
		r, o, e := u.execute([]string{lgroupmod, "-m", u.name, g}, nil, true)
		rc, out, errOut = &r, out+o, errOut+e
		if r != 0 {
			return rc, out, errOut
		}
	}
	return rc, out, errOut
}

func symDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !containsString(b, x) && !containsString(out, x) {
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !containsString(a, x) && !containsString(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// --- BusyBox --------------------------------------------------------------

// bbPassword is BusyBox._build_password_string.
func (u *userRun) bbPassword(current string) string {
	lock := ""
	if u.passwordLock != nil && *u.passwordLock {
		lock = "!"
	}
	password := "*"
	if u.password != nil {
		password = *u.password
	} else if current != "" {
		password = current
		if current == "!" {
			lock = ""
		} else if strings.HasPrefix(current, "!") {
			password = strings.TrimLeft(current, "!")
		}
	}
	return lock + password
}

func (u *userRun) bbFailRC(rc int, errOut string) {
	u.fail(errOut, map[string]any{"name": u.name, "rc": rc})
}

func (u *userRun) createBusybox() (int, string, string) {
	cmd := []string{u.binPath("adduser"), "-D"}
	if u.uid != nil {
		cmd = append(cmd, "-u", strconv.FormatInt(*u.uid, 10))
	}
	if u.group != nil {
		if !groupExists(*u.group) {
			u.fail("Group "+*u.group+" does not exist", nil)
		}
		cmd = append(cmd, "-G", *u.group)
	}
	if u.comment != nil {
		cmd = append(cmd, "-g", *u.comment)
	}
	if u.home != nil {
		cmd = append(cmd, "-h", *u.home)
	}
	if u.shell != nil {
		cmd = append(cmd, "-s", *u.shell)
	}
	if !u.p.Bool("create_home") {
		cmd = append(cmd, "-H")
	}
	if u.skeleton != nil {
		cmd = append(cmd, "-k", *u.skeleton)
	}
	if u.umask != nil {
		cmd = append(cmd, "-K", "UMASK="+*u.umask)
	}
	if u.p.Bool("system") {
		cmd = append(cmd, "-S")
	}
	if u.uidMin != nil {
		cmd = append(cmd, "-K", "UID_MIN="+strconv.FormatInt(*u.uidMin, 10))
	}
	if u.uidMax != nil {
		cmd = append(cmd, "-K", "UID_MAX="+strconv.FormatInt(*u.uidMax, 10))
	}
	rc, out, errOut := u.execute(append(cmd, u.name), nil, true)
	if rc != 0 {
		u.bbFailRC(rc, errOut)
	}
	data := u.name + ":" + u.bbPassword("")
	rc, out, errOut = u.execute([]string{u.binPath("chpasswd"), "--encrypted"}, &data, true)
	if rc != 0 {
		u.bbFailRC(rc, errOut)
	}
	if u.groups != nil && *u.groups != "" {
		add := u.binPath("adduser")
		for _, g := range u.groupsSet(true, false) {
			rc, out, errOut = u.execute([]string{add, u.name, g}, nil, true)
			if rc != 0 {
				u.bbFailRC(rc, errOut)
			}
		}
	}
	return rc, out, errOut
}

func (u *userRun) modifyBusybox() (*int, string, string) {
	current := u.membership(true)
	var rc *int
	out, errOut := "", ""
	info, ok := u.userInfo()
	if !ok {
		return rc, out, errOut
	}
	gid := info.gid
	if u.group != nil {
		if !groupExists(*u.group) {
			u.fail("Group "+*u.group+" does not exist", nil)
		}
		if gi, ok := groupInfo(*u.group); ok {
			gid = gi.gid
		}
	}
	add := u.binPath("adduser")
	del := u.binPath("delgroup")
	run := func(argv []string, data *string) {
		r, o, e := u.execute(argv, data, true)
		rc, out, errOut = &r, o, e
		if r != 0 {
			u.bbFailRC(r, e)
		}
	}
	if u.groups != nil && *u.groups != "" {
		groups := u.groupsSet(true, false)
		diff := symDiff(current, groups)
		if len(diff) > 0 {
			for _, g := range groups {
				if containsString(diff, g) {
					run([]string{add, u.name, g}, nil)
				}
			}
			for _, g := range diff {
				if !containsString(groups, g) && !u.p.Bool("append") {
					run([]string{del, u.name, g}, nil)
				}
			}
		}
	}
	curPw := info.passwd
	newPw := u.bbPassword(curPw)
	if u.p.Str("update_password") == "always" {
		lockMismatch := u.passwordLock != nil && *u.passwordLock && !strings.HasPrefix(curPw, "!")
		if lockMismatch || newPw != curPw {
			data := u.name + ":" + newPw
			run([]string{u.binPath("chpasswd"), "--encrypted"}, &data)
		}
	}
	uid := info.uid
	if u.uid != nil {
		uid = *u.uid
	}
	orStr := func(p *string, def string) string {
		if p != nil && *p != "" {
			return *p
		}
		return def
	}
	entry := strings.Join([]string{u.name, "x", strconv.FormatInt(uid, 10), strconv.FormatInt(gid, 10),
		orStr(u.comment, info.gecos), orStr(u.home, info.dir), orStr(u.shell, info.sh)}, ":")
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		u.fail(pyStrOSError(err, "/etc/passwd"), nil)
	}
	lines := pySplitlinesKeep(string(data))
	change := false
	for i, l := range lines {
		if strings.HasPrefix(l, u.name+":") && strings.TrimSpace(l) != entry {
			change = true
			lines[i] = entry + "\n"
		}
	}
	if change {
		zero := 0
		rc = &zero
		if !u.env.CheckMode {
			tmp, err := os.CreateTemp("", "tmp")
			if err != nil {
				u.fail(err.Error(), nil)
			}
			tmp.WriteString(strings.Join(lines, ""))
			tmp.Close()
			fsutil.Backup("/etc/passwd")
			if err := fsutil.AtomicMove(tmp.Name(), "/etc/passwd", true); err != nil {
				os.Remove(tmp.Name())
				u.fail(pyStrOSError(err, "/etc/passwd"), nil)
			}
		}
	}
	return rc, out, errOut
}

// --- home directories and ssh keys ---------------------------------------

// createHomedir is create_homedir: copy the skeleton (or mkdir -p), then
// apply HOME_MODE / UMASK from login.defs.
func (u *userRun) createHomedir(path string) {
	if pathExists(path) {
		return
	}
	skel := "/etc/skel"
	if u.skeleton != nil {
		skel = *u.skeleton
	}
	if pathExists(skel) && skel != os.DevNull {
		if err := copyTreeSymlinks(skel, path); err != nil {
			u.fail(err.Error(), nil)
		}
	} else if err := os.MkdirAll(path, 0o777); err != nil {
		u.fail(pyStrOSError(err, path), nil)
	}
	if data, err := os.ReadFile("/etc/login.defs"); err == nil {
		mode := os.FileMode(0o755)
		homeRe := regexp.MustCompile(`^HOME_MODE\s+(\d+)$`)
		umaskRe := regexp.MustCompile(`^UMASK\s+(\d+)$`)
		for _, line := range strings.Split(string(data), "\n") {
			if m := homeRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.ParseUint(m[1], 8, 32); err == nil {
					mode = os.FileMode(n)
				}
				break
			}
			if m := umaskRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.ParseUint(m[1], 8, 32); err == nil {
					mode = os.FileMode(0o777 &^ n)
				}
			}
		}
		if err := os.Chmod(path, mode); err != nil {
			u.fail(pyStrOSError(err, path), nil)
		}
	}
}

// copyTreeSymlinks is shutil.copytree(src, dst, symlinks=True).
func copyTreeSymlinks(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		default:
			return fsutil.CopyFile(p, target, true)
		}
	})
}

func (u *userRun) chownHomedir(uid, gid int64, path string) {
	if err := os.Chown(path, int(uid), int(gid)); err != nil {
		u.fail(pyStrOSError(err, path), nil)
	}
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == path {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		os.Chown(p, int(uid), int(gid))
		return nil
	})
}

func (u *userRun) sshKeyPath() (string, error) {
	info, _ := u.userInfo()
	if filepath.IsAbs(u.sshFile) {
		return u.sshFile, nil
	}
	if !pathExists(info.dir) && !u.env.CheckMode {
		return "", fmt.Errorf("User %s home directory does not exist", u.name)
	}
	return pyJoin(info.dir, u.sshFile), nil
}

func (u *userRun) sshKeyGen() (*int, string, string) {
	one, zero := 1, 0
	info, _ := u.userInfo()
	keyFile, err := u.sshKeyPath()
	if err != nil {
		return &one, "", err.Error()
	}
	pub := keyFile + ".pub"
	sshDir := pyDirname(keyFile)
	if !pathExists(sshDir) {
		if u.env.CheckMode {
			return &zero, "", ""
		}
		if err := os.Mkdir(sshDir, 0o700); err != nil {
			return &one, "", "Failed to create " + sshDir + ": " + pyStrOSError(err, sshDir)
		}
		os.Chown(sshDir, int(info.uid), int(info.gid))
	}
	var overwrite *string
	if pathExists(keyFile) {
		if u.p.Bool("force") {
			u.warnings = append(u.warnings, `Overwriting existing ssh key private file "`+keyFile+`"`)
			y := "y"
			overwrite = &y
		} else {
			u.warnings = append(u.warnings, `Found existing ssh key private file "`+keyFile+`", no force, so skipping ssh-keygen generation`)
			return nil, `Key already exists, use "force: yes" to overwrite`, ""
		}
	}
	if pathExists(pub) {
		if u.p.Bool("force") {
			u.warnings = append(u.warnings, `Overwriting existing ssh key public file "`+pub+`"`)
			os.Remove(pub)
		} else {
			u.warnings = append(u.warnings, `Found existing ssh key public file "`+pub+`", no force, so skipping ssh-keygen generation`)
			return nil, `Public key already exists, use "force: yes" to overwrite`, ""
		}
	}
	cmd := []string{u.binPath("ssh-keygen"), "-t", u.p.Str("ssh_key_type")}
	if bits := u.p.Int("ssh_key_bits"); bits > 0 {
		cmd = append(cmd, "-b", strconv.FormatInt(bits, 10))
	}
	comment := u.p.Str("ssh_key_comment")
	if !u.p.Has("ssh_key_comment") {
		host, _ := os.Hostname()
		comment = "ansible-generated on " + host
	}
	cmd = append(cmd, "-C", comment, "-f", keyFile)
	// ansible-core answers ssh-keygen's passphrase prompts on a pty; the
	// passphrase given with -N makes the same key.
	passphrase := ""
	if u.p.Has("ssh_key_passphrase") {
		passphrase = u.p.Str("ssh_key_passphrase")
	}
	cmd = append(cmd, "-N", passphrase)
	rc, out, errOut := u.execute(cmd, overwrite, true)
	if rc == 0 && !u.env.CheckMode {
		os.Chown(keyFile, int(info.uid), int(info.gid))
		os.Chown(pub, int(info.uid), int(info.gid))
	}
	return &rc, out, errOut
}

// --- main -----------------------------------------------------------------

func (u *userRun) warnPasswordHash() {
	pw := u.p.Str("password")
	if pw == "" {
		return
	}
	maybeInvalid := false
	if pw != "*" && pw != "!" && pw != "*************" {
		if strings.ContainsAny(pw, ":*!") {
			maybeInvalid = true
		}
		if !strings.Contains(pw, "$") {
			maybeInvalid = true
		} else {
			fields := strings.Split(pw, "$")
			if len(fields) >= 3 {
				last := fields[len(fields)-1]
				n := len([]rune(last))
				if hashRe.MatchString(last) {
					maybeInvalid = true
				}
				switch fields[1] {
				case "1":
					maybeInvalid = maybeInvalid || n != 22
				case "5", "y":
					maybeInvalid = maybeInvalid || n != 43
				case "6":
					maybeInvalid = maybeInvalid || n != 86
				}
			} else {
				maybeInvalid = true
			}
		}
	}
	if maybeInvalid {
		u.warnings = append(u.warnings, "The input password appears not to have been hashed. "+
			"The 'password' argument must be encrypted for this module to work properly.")
	}
}

func selinuxEnabled() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	return err == nil && len(b) > 0 && pathExists("/etc/selinux/config")
}

func userModule(env *RunEnv, rawArgs map[string]any) (res *agentproto.Result) {
	p, err := userSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if p.Bool("append") && !p.Has("groups") {
		return agentproto.Fail("append is True but all of the following are missing: groups")
	}
	u := &userRun{env: env, p: p, name: p.Str("name"), state: p.Str("state"), local: p.Bool("local")}
	defer func() {
		if r := recover(); r != nil {
			a, ok := r.(userAbort)
			if !ok {
				panic(r)
			}
			res = a.res
			if len(u.warnings) > 0 {
				if res.Extra == nil {
					res.Extra = map[string]any{}
				}
				res.Extra["warnings"] = anyList(u.warnings)
			}
		}
	}()
	str := func(name string, path bool) *string {
		if !p.Has(name) {
			return nil
		}
		v := p.Str(name)
		if path {
			v = pyExpandPath(v)
		}
		return &v
	}
	i64 := func(name string) *int64 {
		if !p.Has(name) {
			return nil
		}
		v := p.Int(name)
		return &v
	}
	u.group, u.comment, u.password = str("group", false), str("comment", false), str("password", false)
	u.shell, u.home = str("shell", true), str("home", true)
	u.skeleton, u.seuser, u.umask = str("skeleton", false), str("seuser", false), str("umask", false)
	u.uid, u.inactive = i64("uid"), i64("password_expire_account_disable")
	u.uidMin, u.uidMax = i64("uid_min"), i64("uid_max")
	u.expireMin, u.expireMax, u.expireWrn = i64("password_expire_min"), i64("password_expire_max"), i64("password_expire_warn")
	if p.Has("password_lock") {
		b := p.Bool("password_lock")
		u.passwordLock = &b
	}
	if u.local {
		switch {
		case u.umask != nil:
			return agentproto.Fail("'umask' can not be used with 'local'")
		case u.uidMin != nil:
			return agentproto.Fail("'uid_min' can not be used with 'local'")
		case u.uidMax != nil:
			return agentproto.Fail("'uid_max' can not be used with 'local'")
		}
	}
	if p.Has("groups") {
		g := strings.Join(strList(p.List("groups")), ",")
		u.groups = &g
	}
	if p.Has("expires") {
		t := time.Unix(int64(math.Floor(p.Float("expires"))), 0).UTC()
		u.expires = &t
	}
	if p.Has("ssh_key_file") {
		u.sshFile = pyExpandPath(p.Str("ssh_key_file"))
	} else {
		u.sshFile = pyJoin(".ssh", "id_"+p.Str("ssh_key_type"))
	}
	if platformSystem() == "Linux" {
		switch pyDistribution() {
		case "Alpine", "Buildroot":
			u.busybox = true
		}
	}
	u.warnPasswordHash()
	if u.seuser != nil && !selinuxEnabled() {
		u.warnings = append(u.warnings, "'seuser' is set to '"+*u.seuser+"' but SELinux is not enabled on "+
			"this system. The 'seuser' parameter will be ignored.")
	}

	result := map[string]any{"name": u.name, "state": u.state}
	res = &agentproto.Result{Extra: result}
	finish := func() *agentproto.Result {
		if len(u.warnings) > 0 {
			result["warnings"] = anyList(u.warnings)
		}
		return res
	}
	checkChanged := func() *agentproto.Result {
		res = &agentproto.Result{Changed: true, Extra: map[string]any{}}
		result = res.Extra
		return finish()
	}
	failRC := func(rc int, msg string) *agentproto.Result {
		r := agentproto.Fail("%s", msg)
		r.Extra = map[string]any{"name": u.name, "rc": rc}
		if len(u.warnings) > 0 {
			r.Extra["warnings"] = anyList(u.warnings)
		}
		return r
	}

	var rc *int
	out, errOut := "", ""
	if u.state == "absent" {
		if u.userExists() {
			if env.CheckMode {
				return checkChanged()
			}
			var r int
			r, out, errOut = u.removeUserdel()
			rc = &r
			if r != 0 {
				return failRC(r, errOut)
			}
			result["force"] = p.Bool("force")
			result["remove"] = p.Bool("remove")
		}
	} else {
		if !u.userExists() {
			if env.CheckMode {
				return checkChanged()
			}
			needsParents := u.home != nil && p.Bool("create_home") && !isDir(pyDirname(*u.home))
			var r int
			if u.busybox {
				r, out, errOut = u.createBusybox()
			} else {
				r, out, errOut = u.createUseradd()
			}
			rc = &r
			if needsParents {
				if info, ok := u.userInfo(); ok {
					u.chownHomedir(info.uid, info.gid, *u.home)
				}
			}
			result["system"] = p.Bool("system")
			result["create_home"] = p.Bool("create_home")
		} else {
			if u.busybox {
				rc, out, errOut = u.modifyBusybox()
			} else {
				rc, out, errOut = u.modifyUsermod()
			}
			result["append"] = p.Bool("append")
			result["move_home"] = p.Bool("move_home")
		}
		if rc != nil && *rc != 0 {
			return failRC(*rc, errOut)
		}
		if u.password != nil {
			result["password"] = "NOT_LOGGING_PASSWORD"
		}
	}
	res.Changed = rc != nil
	if out != "" {
		result["stdout"] = out
		result["stdout_lines"] = anyList(pySplitlines(out))
	}
	if errOut != "" {
		result["stderr"] = errOut
		result["stderr_lines"] = anyList(pySplitlines(errOut))
	}

	if u.state == "present" && u.userExists() {
		info, ok := u.userInfo()
		if !ok {
			result["msg"] = "failed to look up user name: " + u.name
			res.Failed = true
			return finish()
		}
		result["uid"] = info.uid
		result["group"] = info.gid
		result["comment"] = info.gecos
		result["home"] = info.dir
		result["shell"] = info.sh
		result["groups"] = strings.Join(u.membership(true), ",")

		home := info.dir
		if u.home != nil {
			home = *u.home
		}
		if !pathExists(home) && !placeholderHomes[home] && p.Bool("create_home") {
			if !env.CheckMode {
				u.createHomedir(home)
				u.chownHomedir(info.uid, info.gid, home)
			}
			res.Changed = true
		}

		if p.Bool("generate_ssh_key") {
			krc, _, kerr := u.sshKeyGen()
			if krc != nil && *krc != 0 {
				return failRC(*krc, kerr)
			}
			if krc != nil && *krc == 0 {
				res.Changed = true
			}
			keyFile, _ := u.sshKeyPath()
			if !pathExists(keyFile) {
				result["ssh_fingerprint"] = ""
			} else {
				frc, fo, fe := runCommand(env, []string{u.binPath("ssh-keygen"), "-l", "-f", keyFile}, cmdOpts{})
				if frc == 0 {
					result["ssh_fingerprint"] = strings.TrimSpace(fo)
				} else {
					result["ssh_fingerprint"] = strings.TrimSpace(fe)
				}
			}
			result["ssh_key_file"] = keyFile
			if b, err := os.ReadFile(keyFile + ".pub"); err == nil {
				result["ssh_public_key"] = strings.TrimSpace(string(b))
			} else {
				result["ssh_public_key"] = nil
			}
		}

		if erc, _, eerr := u.setPasswordExpire(); erc != nil {
			if *erc != 0 {
				return failRC(*erc, eerr)
			}
			res.Changed = true
		}
	}
	return finish()
}

// setPasswordExpire is set_password_expire: chage for the fields that
// differ from shadow (nothing when the shadow entry is unreadable).
func (u *userRun) setPasswordExpire() (*int, string, string) {
	minC, maxC, warnC := u.expireMin != nil, u.expireMax != nil, u.expireWrn != nil
	sp := getspnam(u.name)
	if sp == nil {
		return nil, "", ""
	}
	minC = minC && *u.expireMin != sp.min
	maxC = maxC && *u.expireMax != sp.max
	warnC = warnC && *u.expireWrn != sp.warn
	if !minC && !maxC && !warnC {
		return nil, "", ""
	}
	cmd := []string{u.binPath("chage")}
	if minC {
		cmd = append(cmd, "-m", strconv.FormatInt(*u.expireMin, 10))
	}
	if maxC {
		cmd = append(cmd, "-M", strconv.FormatInt(*u.expireMax, 10))
	}
	if warnC {
		cmd = append(cmd, "-W", strconv.FormatInt(*u.expireWrn, 10))
	}
	rc, out, errOut := u.execute(append(cmd, u.name), nil, true)
	return &rc, out, errOut
}

func atoiOr(s string) any {
	var n int64
	if _, err := fmt.Sscan(s, &n); err == nil {
		return n
	}
	return s
}

func groupID(env *RunEnv, group string) string {
	out, err := runOut(env, "getent", "group", group)
	if err != nil {
		return ""
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

func stringList(items []any) []string {
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
