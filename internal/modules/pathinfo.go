package modules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// pathInfoModules are the modules whose Ansible counterparts return a
// path/dest key that AnsibleModule.add_path_info decorates with the file's
// stat details (on success and on failure alike).
var pathInfoModules = map[string]bool{
	"file": true, "ansible.builtin.file": true,
	"copy": true, "ansible.builtin.copy": true,
	"tempfile": true, "ansible.builtin.tempfile": true,
	"get_url": true, "ansible.builtin.get_url": true,
	"wait_for": true, "ansible.builtin.wait_for": true,
}

// addPathInfo is AnsibleModule.add_path_info: when the result names an
// existing path (path, else dest), add uid, gid, owner, group, mode, state
// and size from lstat (existence itself follows symlinks, as
// os.path.exists does).
func addPathInfo(res *agentproto.Result) {
	if res == nil || res.Extra == nil {
		return
	}
	v, ok := res.Extra["path"]
	if !ok {
		v, ok = res.Extra["dest"]
	}
	path, isStr := v.(string)
	if !ok || !isStr {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	lst, err := os.Lstat(path)
	if err != nil {
		return
	}
	uid, gid, _ := statIDsOf(lst)
	res.Extra["uid"] = int64(uid)
	res.Extra["gid"] = int64(gid)
	res.Extra["owner"] = userName(uid)
	res.Extra["group"] = groupName(gid)
	res.Extra["mode"] = fmt.Sprintf("0%03o", sIMode(lst.Mode()))
	switch {
	case lst.Mode()&os.ModeSymlink != 0:
		res.Extra["state"] = "link"
	case st.IsDir():
		res.Extra["state"] = "directory"
	case nlinkOf(st) > 1:
		res.Extra["state"] = "hard"
	default:
		res.Extra["state"] = "file"
	}
	res.Extra["size"] = lst.Size()
}

// userName is pwd.getpwuid(uid)[0], falling back to the number.
func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// groupName is grp.getgrgid(gid)[0], falling back to the number.
func groupName(gid int) string {
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
		return g.Name
	}
	return strconv.Itoa(gid)
}

// sIMode is stat.S_IMODE: permission bits plus setuid/setgid/sticky.
func sIMode(m os.FileMode) uint32 {
	n := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		n |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		n |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		n |= 0o1000
	}
	return n
}

// pyOSError renders a Go filesystem error the way str(OSError) does in
// Python when the path was passed as bytes: "[Errno 2] No such file or
// directory: b'/x'" (two-path errors: "b'/a' -> b'/b'").
func pyOSError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	head := fmt.Sprintf("[Errno %d] %s", int(errno), pyStrerror(errno))
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Sprintf("%s: %s -> %s", head, pyBytesRepr(le.Old), pyBytesRepr(le.New))
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return head + ": " + pyBytesRepr(pe.Path)
	}
	return head
}

// pyStrerror is libc's strerror text (Go's table is the same text with a
// lowercased first letter).
func pyStrerror(errno syscall.Errno) string {
	s := errno.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// pyBytesRepr is repr() of a Python bytes value.
func pyBytesRepr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteString("b")
	b.WriteByte(quote)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == quote || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
