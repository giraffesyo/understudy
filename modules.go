package understudy

import (
	"fmt"
	"reflect"
	"strings"
)

// argsOf builds a module-args map from a struct via `ans:"name"` field
// tags, omitting zero values. Pointer fields express tri-state options
// (nil = unset). Fields tagged `ans:"-"` are skipped.
func argsOf(v any) map[string]any {
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	out := map[string]any{}
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("ans")
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(field.Name)
		}
		fv := rv.Field(i)
		switch fv.Kind() {
		case reflect.Ptr:
			if fv.IsNil() {
				continue
			}
			out[tag] = fv.Elem().Interface()
		case reflect.String:
			if fv.String() == "" {
				continue
			}
			out[tag] = fv.String()
		case reflect.Bool:
			if !fv.Bool() {
				continue
			}
			out[tag] = true
		case reflect.Int, reflect.Int64:
			if fv.Int() == 0 {
				continue
			}
			out[tag] = fv.Int()
		case reflect.Slice, reflect.Map:
			if fv.Len() == 0 {
				continue
			}
			out[tag] = fv.Interface()
		default:
			if fv.IsZero() {
				continue
			}
			out[tag] = fv.Interface()
		}
	}
	return out
}

// Bool returns a *bool for tri-state module options (nil means unset).
func Bool(b bool) *bool { return &b }

// ---- Commands ----

// Command runs an executable directly (no shell interpretation).
type Command struct {
	Cmd     string `ans:"-"`
	Chdir   string
	Creates string
	Removes string
	Stdin   string
}

func (c Command) ModuleName() string         { return "command" }
func (c Command) ModuleArgs() map[string]any { return argsOf(c) }
func (c Command) freeForm() string           { return c.Cmd }

// Shell runs a command through /bin/sh.
type Shell struct {
	Cmd        string `ans:"-"`
	Chdir      string
	Creates    string
	Removes    string
	Executable string
	Stdin      string
}

func (s Shell) ModuleName() string         { return "shell" }
func (s Shell) ModuleArgs() map[string]any { return argsOf(s) }
func (s Shell) freeForm() string           { return s.Cmd }

// Raw runs a command over the bare connection — no agent involved.
type Raw struct {
	Cmd string `ans:"-"`
}

func (r Raw) ModuleName() string         { return "raw" }
func (r Raw) ModuleArgs() map[string]any { return nil }
func (r Raw) freeForm() string           { return r.Cmd }

// ---- Files ----

// Copy places a file on the target from a local source or inline content.
type Copy struct {
	Src     string
	Content string
	Dest    string
	Mode    string
	Owner   string
	Group   string
	Backup  bool
	Force   *bool
}

func (c Copy) ModuleName() string         { return "copy" }
func (c Copy) ModuleArgs() map[string]any { return argsOf(c) }

// Template renders a local Jinja2 template and places the result.
type Template struct {
	Src    string
	Dest   string
	Mode   string
	Owner  string
	Group  string
	Backup bool
}

func (t Template) ModuleName() string         { return "template" }
func (t Template) ModuleArgs() map[string]any { return argsOf(t) }

// File manages filesystem state: directories, touches, links, removal.
type File struct {
	Path    string
	State   string // file, touch, absent, directory, link, hard
	Mode    string
	Owner   string
	Group   string
	Src     string // link target for state=link
	Recurse bool
	Force   bool
	Follow  *bool
}

func (f File) ModuleName() string         { return "file" }
func (f File) ModuleArgs() map[string]any { return argsOf(f) }

// Stat gathers file facts.
type Stat struct {
	Path        string
	Follow      bool
	GetChecksum *bool `ans:"get_checksum"`
}

func (s Stat) ModuleName() string         { return "stat" }
func (s Stat) ModuleArgs() map[string]any { return argsOf(s) }

// LineInFile ensures a line is present in (or absent from) a file.
type LineInFile struct {
	Path         string
	Line         string
	Regexp       string
	State        string // present (default), absent
	InsertAfter  string `ans:"insertafter"`
	InsertBefore string `ans:"insertbefore"`
	Backrefs     bool
	Create       bool
	Backup       bool
	FirstMatch   bool `ans:"firstmatch"`
	Mode         string
	Owner        string
	Group        string
}

func (l LineInFile) ModuleName() string         { return "lineinfile" }
func (l LineInFile) ModuleArgs() map[string]any { return argsOf(l) }

// ---- System ----

// Service manages systemd units.
type Service struct {
	Name         string
	State        string // started, stopped, restarted, reloaded
	Enabled      *bool
	DaemonReload bool `ans:"daemon_reload"`
}

func (s Service) ModuleName() string         { return "service" }
func (s Service) ModuleArgs() map[string]any { return argsOf(s) }

// Package manages packages with the target's native package manager.
type Package struct {
	Name        any    // string or []string
	State       string // present (default), absent, latest
	UpdateCache bool   `ans:"update_cache"`
}

func (p Package) ModuleName() string         { return "package" }
func (p Package) ModuleArgs() map[string]any { return argsOf(p) }

// Apt manages Debian packages explicitly.
type Apt struct {
	Name        any
	State       string
	UpdateCache bool `ans:"update_cache"`
}

func (a Apt) ModuleName() string         { return "apt" }
func (a Apt) ModuleArgs() map[string]any { return argsOf(a) }

// Yum manages RPM packages explicitly.
type Yum struct {
	Name        any
	State       string
	UpdateCache bool `ans:"update_cache"`
}

func (y Yum) ModuleName() string         { return "yum" }
func (y Yum) ModuleArgs() map[string]any { return argsOf(y) }

// User manages accounts.
type User struct {
	Name       string
	State      string
	UID        *int64 `ans:"uid"`
	Group      string
	Groups     []string
	Append     bool
	Shell      string
	Home       string
	CreateHome *bool `ans:"create_home"`
	Comment    string
	System     bool
	Remove     bool
	Password   string
	// UpdatePassword is "always" (the default) or "on_create".
	UpdatePassword string   `ans:"update_password"`
	PasswordLock   *bool    `ans:"password_lock"`
	Expires        *float64 // epoch seconds; negative removes the expiry
	MoveHome       bool     `ans:"move_home"`
	GenerateSSHKey bool     `ans:"generate_ssh_key"`
	SSHKeyType     string   `ans:"ssh_key_type"`
	SSHKeyBits     int64    `ans:"ssh_key_bits"`
	SSHKeyFile     string   `ans:"ssh_key_file"`
	SSHKeyComment  string   `ans:"ssh_key_comment"`
}

func (u User) ModuleName() string         { return "user" }
func (u User) ModuleArgs() map[string]any { return argsOf(u) }

// AuthorizedKey manages keys in a user's authorized_keys file.
type AuthorizedKey struct {
	User       string
	Key        string
	State      string
	Path       string
	KeyOptions string `ans:"key_options"`
	Exclusive  bool
	Comment    string
	ManageDir  *bool `ans:"manage_dir"`
}

func (a AuthorizedKey) ModuleName() string         { return "ansible.posix.authorized_key" }
func (a AuthorizedKey) ModuleArgs() map[string]any { return argsOf(a) }

// Hostname sets the system hostname.
type Hostname struct {
	Name string
	Use  string
}

func (h Hostname) ModuleName() string         { return "hostname" }
func (h Hostname) ModuleArgs() map[string]any { return argsOf(h) }

// ---- Control-side ----

// Debug prints a message or a variable's value.
type Debug struct {
	Msg       string
	Var       string
	Verbosity int64
}

func (d Debug) ModuleName() string         { return "debug" }
func (d Debug) ModuleArgs() map[string]any { return argsOf(d) }

// SetFact sets host facts (persist for the rest of the run).
type SetFact struct {
	Facts map[string]any `ans:"-"`
}

func (s SetFact) ModuleName() string         { return "set_fact" }
func (s SetFact) ModuleArgs() map[string]any { return s.Facts }

// Assert fails unless every expression evaluates true.
type Assert struct {
	That       []string
	FailMsg    string `ans:"fail_msg"`
	SuccessMsg string `ans:"success_msg"`
}

func (a Assert) ModuleName() string         { return "assert" }
func (a Assert) ModuleArgs() map[string]any { return argsOf(a) }

// Fail aborts the host with a message.
type Fail struct {
	Msg string
}

func (f Fail) ModuleName() string         { return "fail" }
func (f Fail) ModuleArgs() map[string]any { return argsOf(f) }

// Ping verifies the target is reachable and the agent works.
type Ping struct{}

func (Ping) ModuleName() string         { return "ping" }
func (Ping) ModuleArgs() map[string]any { return nil }

// Setup gathers facts explicitly.
type Setup struct{}

func (Setup) ModuleName() string         { return "setup" }
func (Setup) ModuleArgs() map[string]any { return nil }

// validateAction reports a helpful error for malformed actions.
func validateAction(t *Task) error {
	if len(t.Block) > 0 {
		if t.Action != nil {
			return fmt.Errorf("task %q: Block and Action are mutually exclusive", t.Name)
		}
		return nil
	}
	if t.Action == nil {
		return fmt.Errorf("task %q: Action is required", t.Name)
	}
	if t.Action.ModuleName() == "" {
		return fmt.Errorf("task %q: action has an empty module name", t.Name)
	}
	return nil
}
