package modules

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// This file ports ansible.builtin.user's DarwinUser class: macOS accounts
// live in Directory Services and are edited with dscl(1) and
// dseditgroup(8). A local connection runs modules in-process on the
// controller, which may be a Mac, so this is reached there.

// darwinGetentLines serves getentLines on macOS, whose accounts are not in
// /etc/passwd and /etc/group: passwd entries come from `id -P` (getpwnam,
// its master.passwd line cut to the passwd fields) and groups from
// `dscacheutil -q group` (getgrent order and members).
func darwinGetentLines(db, key string) []string {
	switch db {
	case "passwd":
		if key == "" {
			return nil
		}
		rc, out, _ := runCommand(&RunEnv{}, []string{"id", "-P", key}, cmdOpts{})
		if rc != 0 {
			return nil
		}
		// name:passwd:uid:gid:class:change:expire:gecos:dir:shell
		f := strings.Split(strings.TrimRight(out, "\n"), ":")
		if len(f) != 10 {
			return nil
		}
		f[2], f[3] = pyUnsignedID(f[2]), pyUnsignedID(f[3])
		return []string{strings.Join(append(f[:4:4], f[7:]...), ":")}
	case "group":
		argv := []string{"dscacheutil", "-q", "group"}
		if key != "" {
			argv = append(argv, "-a", "name", key)
		}
		rc, out, _ := runCommand(&RunEnv{}, argv, cmdOpts{})
		if rc != 0 {
			return nil
		}
		return parseDscacheGroups(out)
	}
	return nil
}

// parseDscacheGroups turns `dscacheutil -q group` records ("name: x",
// "password: *", "gid: 20", "users: a b ", blank-line separated) into
// group(5) lines.
func parseDscacheGroups(out string) []string {
	var lines []string
	rec := map[string]string{}
	flush := func() {
		if rec["name"] != "" {
			lines = append(lines, rec["name"]+":"+rec["password"]+":"+pyUnsignedID(rec["gid"])+":"+
				strings.Join(strings.Fields(rec["users"]), ","))
		}
		rec = map[string]string{}
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if ok {
			rec[k] = strings.TrimSpace(v)
		}
	}
	flush()
	return lines
}

// pyUnsignedID is a uid or gid as Python's pwd and grp report it: the
// unsigned 32-bit value, except that (uid_t)-1 stays -1.
func pyUnsignedID(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n >= -1 {
		return s
	}
	return strconv.FormatInt(n+1<<32, 10)
}

// darwinFields are DarwinUser.fields: parameter -> dscl property.
var darwinFields = [][2]string{
	{"comment", "RealName"},
	{"home", "NFSHomeDirectory"},
	{"shell", "UserShell"},
	{"uid", "UniqueID"},
	{"group", "PrimaryGroupID"},
	{"hidden", "IsHidden"},
}

// darwinState is the DarwinUser instance state that differs from the
// parsed parameters: create_user fills in defaults, and group becomes a gid.
type darwinState struct {
	fields  [][2]string
	comment *string
	group   *string
	uid     *string
	hidden  *string
}

func (u *userRun) darwinInit() {
	d := &darwinState{fields: darwinFields, comment: u.comment, group: u.group}
	if u.uid != nil {
		s := strconv.FormatInt(*u.uid, 10)
		d.uid = &s
	}
	// The user is hidden if asked to be, or by default for a system user.
	one, zero := "1", "0"
	switch {
	case !u.p.Has("hidden"):
		if u.p.Bool("system") {
			d.hidden = &one
		}
	case u.p.Bool("hidden"):
		d.hidden = &one
	default:
		d.hidden = &zero
	}
	if d.hidden != nil {
		// ansible-core appends the field again (fields is a class list).
		d.fields = append(append([][2]string(nil), darwinFields...), [2]string{"hidden", "IsHidden"})
	}
	u.darwin = d
}

// darwinValue is self.__dict__[field]: the value when set and truthy.
func (u *userRun) darwinValue(field string) string {
	var v *string
	switch field {
	case "comment":
		v = u.darwin.comment
	case "home":
		v = u.home
	case "shell":
		v = u.shell
	case "uid":
		v = u.darwin.uid
	case "group":
		v = u.darwin.group
	case "hidden":
		v = u.darwin.hidden
	}
	// Python truthiness: an empty string, or a 0 given as an int.
	if v == nil || *v == "" || ((field == "hidden" || field == "uid") && *v == "0") {
		return ""
	}
	return *v
}

func (u *userRun) dscl(args ...string) []string {
	return append([]string{u.binPath("dscl"), "."}, args...)
}

func (u *userRun) darwinFail(msg, errOut, out string, rc int) {
	u.fail(msg, map[string]any{"err": errOut, "out": out, "rc": rc})
}

// darwinUserExists is DarwinUser.user_exists.
func (u *userRun) darwinUserExists() bool {
	rc, _, _ := u.execute(u.dscl("-read", "/Users/"+u.name, "UniqueID"), nil, false)
	return rc == 0
}

// darwinListUserGroups is _list_user_groups: the groups whose
// GroupMembership names the user.
func (u *userRun) darwinListUserGroups() []string {
	_, out, _ := u.execute(u.dscl("-search", "/Groups", "GroupMembership", u.name), nil, false)
	var groups []string
	for _, line := range pySplitlines(out) {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, ")") {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			groups = append(groups, f[0])
		}
	}
	return groups
}

// darwinUserProperty is _get_user_property: the property's value as
// dscl -read shows it, or nil.
func (u *userRun) darwinUserProperty(prop string) *string {
	rc, out, _ := u.execute(u.dscl("-read", "/Users/"+u.name, prop), nil, false)
	if rc != 0 {
		return nil
	}
	return dsclPropertyValue(out)
}

func dsclPropertyValue(out string) *string {
	lines := pySplitlines(out)
	var v string
	switch {
	case len(lines) == 1:
		parts := strings.Split(lines[0], ": ")
		if len(parts) < 2 {
			return nil // IndexError upstream
		}
		v = parts[1]
	case len(lines) > 2:
		v = strings.Join(append([]string{strings.TrimSpace(lines[1])}, lines[2:]...), "\n")
	case len(lines) == 2:
		v = strings.TrimSpace(lines[1])
	default:
		return nil
	}
	return &v
}

// darwinNextUID is _get_next_uid: one past the highest uid, or past the
// highest system uid (under 500) for a system user when there is room.
func (u *userRun) darwinNextUID(system bool) string {
	rc, out, errOut := u.execute(u.dscl("-list", "/Users", "UniqueID"), nil, false)
	if rc != 0 {
		u.fail("Unable to get the next available uid", map[string]any{"rc": rc, "out": out, "err": errOut})
	}
	var maxUID, maxSystem int64
	for _, line := range pySplitlines(out) {
		f := strings.Split(line, " ")
		cur, err := strconv.ParseInt(strings.TrimSpace(f[len(f)-1]), 10, 64)
		if err != nil {
			u.fail("invalid literal for int() with base 10: "+pyStrRepr(f[len(f)-1]), nil)
		}
		if maxUID < cur {
			maxUID = cur
		}
		if maxSystem < cur && cur < 500 {
			maxSystem = cur
		}
	}
	if system && 0 < maxSystem && maxSystem < 499 {
		return strconv.FormatInt(maxSystem+1, 10)
	}
	return strconv.FormatInt(maxUID+1, 10)
}

// darwinChangePassword is _change_user_password: dscl takes the cleartext
// password; without one the account gets the '*' placeholder.
func (u *userRun) darwinChangePassword() (int, string, string) {
	var cmd []string
	if u.password != nil && *u.password != "" {
		cmd = u.dscl("-passwd", "/Users/"+u.name, *u.password)
	} else {
		cmd = u.dscl("-create", "/Users/"+u.name, "Password", "*")
	}
	rc, out, errOut := u.execute(cmd, nil, true)
	if rc != 0 {
		u.darwinFail("Error when changing password", errOut, out, rc)
	}
	return rc, out, errOut
}

// darwinGroupNumerical is _make_group_numerical: the group (by name only,
// default nogroup) as its gid.
func (u *userRun) darwinGroupNumerical() {
	name := "nogroup"
	if u.darwin.group != nil {
		name = *u.darwin.group
	}
	var gid *int64
	for _, g := range getentLines("group", name) {
		f := strings.Split(g, ":")
		if len(f) >= 3 && f[0] == name {
			n := parseInt64(f[2])
			gid = &n
			break
		}
	}
	if gid == nil {
		u.fail(`Group "`+name+`" not found. Try to create it first using "group" module.`, nil)
	}
	s := strconv.FormatInt(*gid, 10)
	u.darwin.group = &s
}

// darwinEditGroup is __modify_group: dseditgroup adds or removes the user.
func (u *userRun) darwinEditGroup(group, action string) (int, string, string) {
	option := "-d"
	if action == "add" {
		option = "-a"
	}
	rc, out, errOut := u.execute([]string{"dseditgroup", "-o", "edit", option, u.name, "-t", "user", group}, nil, true)
	if rc != 0 {
		u.darwinFail(`Cannot `+action+` user "`+u.name+`" to group "`+group+`".`, errOut, out, rc)
	}
	return rc, out, errOut
}

// darwinModifyGroups is _modify_group: reconcile supplementary groups
// (the primary group excluded) with dseditgroup.
func (u *userRun) darwinModifyGroups() (int, string, string, bool) {
	rc, out, errOut, changed := 0, "", "", false
	current := u.darwinListUserGroups()
	var target []string
	if u.groups != nil {
		target = u.groupsSet(true, true)
	}
	if !u.p.Bool("append") {
		remove := setMinus(current, target)
		sort.Strings(remove)
		for _, g := range remove {
			_, o, e := u.darwinEditGroup(g, "delete")
			out, errOut = out+o, errOut+e
			changed = true
		}
	}
	add := setMinus(target, current)
	sort.Strings(add)
	for _, g := range add {
		r, o, e := u.darwinEditGroup(g, "add")
		rc, out, errOut = rc+r, out+o, errOut+e
		changed = true
	}
	return rc, out, errOut, changed
}

// darwinUpdateSystemUser is _update_system_user: keep the login window's
// HiddenUsersList in step with system. It reports whether it changed it.
func (u *userRun) darwinUpdateSystemUser() bool {
	const plist = "/Library/Preferences/com.apple.loginwindow.plist"
	_, out, _ := u.execute([]string{"defaults", "read", plist, "HiddenUsersList"}, nil, false)
	var hidden []string
	lines := pySplitlines(out)
	if len(lines) > 2 {
		for _, x := range lines[1 : len(lines)-1] {
			if parts := strings.Split(x, `"`); len(parts) > 1 {
				x = parts[1]
			} else {
				x = strings.TrimSpace(x)
			}
			hidden = append(hidden, x)
		}
	}
	if u.p.Bool("system") {
		if !containsString(hidden, u.name) {
			rc, o, e := u.execute([]string{"defaults", "write", plist, "HiddenUsersList", "-array-add", u.name}, nil, true)
			if rc != 0 {
				u.darwinFail(`Cannot user "`+u.name+`" to hidden user list.`, e, o, rc)
			}
			return true
		}
		return false
	}
	if !containsString(hidden, u.name) {
		return false
	}
	kept := setMinusOnce(hidden, u.name)
	rc, o, e := u.execute(append([]string{"defaults", "write", plist, "HiddenUsersList", "-array"}, kept...), nil, true)
	if rc != 0 {
		u.darwinFail(`Cannot remove user "`+u.name+`" from hidden user list.`, e, o, rc)
	}
	return true
}

// setMinusOnce removes the first occurrence of x.
func setMinusOnce(list []string, x string) []string {
	out := append([]string(nil), list...)
	for i, v := range out {
		if v == x {
			return append(out[:i], out[i+1:]...)
		}
	}
	return out
}

// darwinRemove is DarwinUser.remove_user: dscl -delete, and with force the
// home directory too.
func (u *userRun) darwinRemove() (int, string, string) {
	info, _ := u.userInfo()
	rc, out, errOut := u.execute(u.dscl("-delete", "/Users/"+u.name), nil, true)
	if rc != 0 {
		u.darwinFail(`Cannot delete user "`+u.name+`".`, errOut, out, rc)
	}
	if u.p.Bool("force") && info != nil && pathExists(info.dir) {
		if err := os.RemoveAll(info.dir); err != nil {
			u.fail(pyStrOSError(err, info.dir), nil)
		}
		out += "Removed " + info.dir
	}
	return rc, out, errOut
}

// darwinCreate is DarwinUser.create_user.
func (u *userRun) darwinCreate() (int, string, string) {
	rc, out, errOut := u.execute(u.dscl("-create", "/Users/"+u.name), nil, true)
	if rc != 0 {
		u.darwinFail(`Cannot create user "`+u.name+`".`, errOut, out, rc)
	}
	if u.darwin.comment == nil {
		u.darwin.comment = &u.name
	}
	if u.darwin.group == nil {
		staff := "staff"
		u.darwin.group = &staff
	}
	u.darwinGroupNumerical()
	if u.darwin.uid == nil {
		uid := u.darwinNextUID(u.p.Bool("system"))
		u.darwin.uid = &uid
	}
	if u.p.Bool("create_home") {
		if u.home == nil {
			h := "/Users/" + u.name
			u.home = &h
		}
		if !u.env.CheckMode {
			if !pathExists(*u.home) {
				if err := os.MkdirAll(*u.home, 0o777); err != nil {
					u.fail(pyStrOSError(err, *u.home), nil)
				}
			}
			u.chownHomedir(parseInt64(*u.darwin.uid), parseInt64(*u.darwin.group), *u.home)
		}
	}
	if !u.p.Bool("system") && u.shell == nil {
		sh := "/bin/bash"
		u.shell = &sh
	}
	for _, f := range u.darwin.fields {
		v := u.darwinValue(f[0])
		if v == "" {
			continue
		}
		r, o, e := u.execute(u.dscl("-create", "/Users/"+u.name, f[1], v), nil, true)
		if r != 0 {
			u.darwinFail(`Cannot add property "`+f[0]+`" to user "`+u.name+`".`, errOut, out, r)
		}
		out, errOut = out+o, errOut+e
	}
	r, o, e := u.darwinChangePassword()
	rc, out, errOut = r, out+o, errOut+e
	u.darwinUpdateSystemUser()
	if u.groups != nil && *u.groups != "" {
		r, o, e, _ := u.darwinModifyGroups()
		rc, out, errOut = r, out+o, errOut+e
	}
	return rc, out, errOut
}

// darwinModify is DarwinUser.modify_user: set each differing property,
// the password (always, when update_password is always), groups and the
// hidden-users list.
func (u *userRun) darwinModify() (*int, string, string) {
	var changed *int
	out, errOut := "", ""
	if u.darwin.group != nil && *u.darwin.group != "" {
		u.darwinGroupNumerical()
	}
	for _, f := range u.darwin.fields {
		v := u.darwinValue(f[0])
		if v == "" {
			continue
		}
		if cur := u.darwinUserProperty(f[1]); cur == nil || *cur != v {
			rc, o, e := u.execute(u.dscl("-create", "/Users/"+u.name, f[1], v), nil, true)
			if rc != 0 {
				u.darwinFail(`Cannot update property "`+f[0]+`" for user "`+u.name+`".`, errOut, out, rc)
			}
			changed = &rc
			out, errOut = out+o, errOut+e
		}
	}
	if u.p.Str("update_password") == "always" && u.password != nil {
		rc, o, e := u.darwinChangePassword()
		out, errOut = out+o, errOut+e
		changed = &rc
	}
	if u.groups != nil && *u.groups != "" {
		rc, o, e, groupsChanged := u.darwinModifyGroups()
		out, errOut = out+o, errOut+e
		if groupsChanged {
			changed = &rc
		}
	}
	if u.darwinUpdateSystemUser() {
		zero := 0
		changed = &zero
	}
	return changed, out, errOut
}
