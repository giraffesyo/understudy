package connection

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

// ShellOptions are the shell plugin options that decide where a task's
// temporary files go on the target and how a become user that is
// neither privileged nor the login user gets to read them.
type ShellOptions struct {
	RemoteUser        string   // the configured remote_user ("" when unset)
	AdminUsers        []string // admin_users (nil: root, toor)
	SystemTmpdirs     []string // system_tmpdirs (nil: /var/tmp, /tmp)
	RemoteTmp         string   // remote_tmp ("" : ~/.ansible/tmp)
	CommonRemoteGroup string   // common_remote_group ("" when unset)
	WorldReadableTemp bool     // allow_world_readable_tmpfiles
	Warn              func(string)
}

// DefaultAdminUsers and DefaultSystemTmpdirs are the shell plugin's
// defaults.
var (
	DefaultAdminUsers    = []string{"root", "toor"}
	DefaultSystemTmpdirs = []string{"/var/tmp", "/tmp"}
)

// becomeUnprivileged is ActionBase._is_become_unprivileged: become to a
// user that is neither an admin user nor the remote user.
func becomeUnprivileged(b *BecomeSpec) bool {
	if b == nil || b.Shell == nil {
		return false
	}
	user := b.user()
	admins := b.Shell.AdminUsers
	if admins == nil {
		admins = DefaultAdminUsers
	}
	for _, a := range admins {
		if a == user {
			return false
		}
	}
	return user != b.Shell.RemoteUser
}

// privilegeLink is get_versioned_doclink for the privilege escalation
// guide.
const privilegeLink = "https://docs.ansible.com/ansible-core/2.21/playbook_guide/playbooks_privilege_escalation.html"

// systemTmp is ActionBase._make_tmp_path for an unprivileged become user:
// a directory under a system temp dir (remote_tmp when it is one of
// system_tmpdirs, else the first of them), made by the login user with
// the shell plugin's mkdtemp command.
func systemTmp(ctx context.Context, conn Connection, sh *ShellOptions) (string, error) {
	dirs := sh.SystemTmpdirs
	if dirs == nil {
		dirs = DefaultSystemTmpdirs
	}
	base := dirs[0]
	if rt := strings.TrimRight(sh.RemoteTmp, "/"); rt != "" {
		for _, d := range dirs {
			if d == rt {
				base = rt
			}
		}
	}
	name := fmt.Sprintf("ansible-tmp-%s-%d-%d", pyTime(time.Now()), os.Getpid(), rand.Int63n(1<<48))
	full := base + "/" + name
	cmd := fmt.Sprintf("( umask 77 && mkdir -p \"` echo %s `\"&& mkdir \"` echo %s `\" && echo %s=\"` echo %s `\" )",
		base, full, name, full)
	res, err := conn.Exec(ctx, cmd, ExecOptions{})
	if err != nil {
		return "", err
	}
	if res.RC != 0 {
		var msg string
		switch {
		case res.RC == 5:
			msg = "Authentication failure."
		case strings.Contains(string(res.Stderr), "No space left on device"):
			msg = string(res.Stderr)
		default:
			msg = fmt.Sprintf("Failed to create temporary directory. In some cases, you may have been able to authenticate "+
				"and did not have permissions on the target directory. Consider changing the remote tmp path in ansible.cfg "+
				"to a path rooted in \"/tmp\", for more error information use -vvv. Failed command was: %s, exited with result %d", cmd, res.RC)
		}
		if out := string(res.Stdout); out != "" {
			msg += ", stdout output: " + out
		}
		return "", &BecomeError{Msg: msg, Unreachable: true}
	}
	return full, nil
}

// pyTime is repr(time.time()).
func pyTime(t time.Time) string {
	s := strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// fixupPerms is ActionBase._fixup_perms2 (execute=True) for an
// unprivileged become user: POSIX ACLs, else chown, macOS's chmod +a,
// Solaris-style ACLs, common_remote_group, and finally world-readable
// files when allowed; each step runs as the login user and the first
// that works wins. The chain runs as one remote script, after prep (a
// command placing the files, "" for none, reading stdin when given).
func fixupPerms(ctx context.Context, conn Connection, prep string, stdin io.Reader, paths []string, b *BecomeSpec) error {
	sh := b.Shell
	user := b.user()
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = ShellQuote(p)
	}
	ps := strings.Join(quoted, " ")
	admin := false
	admins := sh.AdminUsers
	if admins == nil {
		admins = DefaultAdminUsers
	}
	for _, a := range admins {
		if a == sh.RemoteUser {
			admin = true
		}
	}
	// try runs a command, keeping its rc and exact stderr.
	var s strings.Builder
	s.WriteString(`try() { err=$("$@" 2>&1 >/dev/null; r=$?; echo .; exit $r); rc=$?; err=${err%.}; }; ` +
		`done_() { printf '%s\n' "$1"; printf '%s' "$err"; exit 0; }; `)
	if prep != "" {
		fmt.Fprintf(&s, "try /bin/sh -c %s; [ $rc = 0 ] || done_ \"prep $rc\"; ", ShellQuote(prep))
	}
	fmt.Fprintf(&s, "try setfacl -m %s %s; [ $rc = 0 ] && done_ ok; ", ShellQuote("u:"+user+":r-x"), ps)
	fmt.Fprintf(&s, "try chmod u+rwx %s; [ $rc = 0 ] || done_ \"chmod $rc\"; ", ps)
	fmt.Fprintf(&s, "try chown %s %s; [ $rc = 0 ] && done_ ok; ", ShellQuote(user), ps)
	if admin {
		s.WriteString("done_ admin; ")
	}
	fmt.Fprintf(&s, "try chmod +a %s %s; [ $rc = 0 ] && done_ ok; ", ShellQuote(user+" allow read,execute"), ps)
	fmt.Fprintf(&s, "try chmod %s %s; [ $rc = 0 ] && done_ ok; ", ShellQuote("A+user:"+user+":rx:allow"), ps)
	if sh.CommonRemoteGroup != "" {
		fmt.Fprintf(&s, "try chgrp %s %s; if [ $rc = 0 ]; then printf 'chgrp\\n'; try chmod g+rwx %s; [ $rc = 0 ] && done_ ok; fi; ",
			ShellQuote(sh.CommonRemoteGroup), ps, ps)
	}
	if sh.WorldReadableTemp {
		fmt.Fprintf(&s, "try chmod a+rx %s; done_ \"world $rc\"; ", ps)
	}
	s.WriteString(`done_ "fail $rc"`)
	res, err := conn.Exec(ctx, s.String(), ExecOptions{Stdin: stdin})
	if err != nil {
		return err
	}
	out := string(res.Stdout)
	chgrp := strings.HasPrefix(out, "chgrp\n")
	if chgrp {
		out = strings.TrimPrefix(out, "chgrp\n")
	}
	status, stderr, _ := strings.Cut(out, "\n")
	warn := func(msg string) {
		if sh.Warn != nil {
			sh.Warn(msg)
		}
	}
	if chgrp && sh.WorldReadableTemp {
		warn("Both common_remote_group and allow_world_readable_tmpfiles are set. chgrp was successful, " +
			"but there is no guarantee that Ansible will be able to read the files after this operation, " +
			"particularly if common_remote_group was set to a group of which the unprivileged become user " +
			"is not a member. In this situation, allow_world_readable_tmpfiles is a no-op. See this URL for " +
			"more details: " + privilegeLink + "#risks-of-becoming-an-unprivileged-user")
	}
	kind, rc, _ := strings.Cut(status, " ")
	switch kind {
	case "ok":
		return nil
	case "chmod":
		return &BecomeError{Msg: fmt.Sprintf("Failed to set file mode or acl on remote temporary files (rc: %s, err: %s)", rc, stderr)}
	case "admin":
		return &BecomeError{Msg: "Failed to change ownership of the temporary files Ansible (via chmod nor setfacl) " +
			"needs to create despite connecting as a privileged user. Unprivileged become user would be unable to read the file."}
	case "world":
		warn("Using world-readable permissions for temporary files Ansible needs to create when becoming an " +
			"unprivileged user. This may be insecure. For information on securing this, see " +
			privilegeLink + "#risks-of-becoming-an-unprivileged-user")
		if rc == "0" {
			return nil
		}
		return &BecomeError{Msg: fmt.Sprintf("Failed to set file mode on remote files (rc: %s, err: %s)", rc, stderr)}
	case "prep":
		return fmt.Errorf("placing the module on the target failed (rc: %s): %s", rc, strings.TrimSpace(stderr))
	case "fail":
		return &BecomeError{Msg: fmt.Sprintf("Failed to set permissions on the temporary files Ansible needs to create "+
			"when becoming an unprivileged user (rc: %s, err: %s}). For information on working around this, see %s"+
			"#risks-of-becoming-an-unprivileged-user", rc, stderr, privilegeLink)}
	}
	return fmt.Errorf("setting permissions on the temporary files failed: %s", strings.TrimSpace(string(res.Stderr)))
}
