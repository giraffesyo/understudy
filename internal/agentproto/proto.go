// Package agentproto defines the wire contract between the control binary
// and the remote agent. Both import it; it must depend only on the stdlib.
package agentproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// ProtoVersion is bumped on incompatible frame changes. In practice the
	// checksum-named agent path makes version skew impossible; this is a
	// belt-and-suspenders check.
	ProtoVersion = 1

	// ResultSentinel precedes the result JSON on the agent's stdout. Output
	// pollution (sudo lectures, PAM noise, motd) lands before it and is
	// ignored by the control side.
	ResultSentinel = "#UND1#"
)

// TaskRequest is the JSON header line the control node writes to the agent's
// stdin, optionally followed by PayloadLen raw bytes (file content).
type TaskRequest struct {
	Proto        int               `json:"proto"`
	Op           string            `json:"op"` // "task"; future: session ops
	Module       string            `json:"module"`
	Args         map[string]any    `json:"args,omitempty"`
	FreeForm     string            `json:"free_form,omitempty"` // command/shell raw params
	CheckMode    bool              `json:"check_mode,omitempty"`
	Diff         bool              `json:"diff,omitempty"`
	Env          map[string]string `json:"env,omitempty"`       // task environment: for shell-outs
	EnvOrder     []string          `json:"env_order,omitempty"` // Env's variables in the task's order
	PayloadLen   int64             `json:"payload_len,omitempty"`
	BecomeUser   string            `json:"become_user,omitempty"`   // informational
	Background   bool              `json:"background,omitempty"`    // run as an async job (ansible's async_wrapper)
	AsyncTimeout int               `json:"async_timeout,omitempty"` // async: seconds before the job is killed

	// PythonInterpreter is ansible_python_interpreter ("" for discovery).
	PythonInterpreter string `json:"python_interpreter,omitempty"`
	// PythonFallback is ansible_interpreter_python_fallback, the list
	// interpreter discovery tries (nil for ansible's default).
	PythonFallback []string `json:"python_fallback,omitempty"`
	// DiscoveryPath is the login user's PATH, which interpreter discovery
	// searches ("" when the module runs as that user).
	DiscoveryPath string `json:"discovery_path,omitempty"`

	// LoginHome, LoginUID and LoginGID describe the login user when the
	// module runs as another one: remote_tmp's ~ is the login user's
	// home, and directories made there belong to that user (a negative
	// id: unknown).
	LoginHome string `json:"login_home,omitempty"`
	LoginUID  int    `json:"login_uid,omitempty"`
	LoginGID  int    `json:"login_gid,omitempty"`
	// StageDir is the temporary directory a transferred file is reported
	// in when an unprivileged become user runs the module (made by the
	// login user in a system temp dir), "" otherwise.
	StageDir string `json:"stage_dir,omitempty"`
}

// Result is the outcome of one module invocation. Its shape mirrors
// Ansible's task result: well-known fields are typed, module-specific keys
// ride in Extra and are flattened into the same JSON object.
type Result struct {
	Changed      bool           `json:"-"`
	Failed       bool           `json:"-"`
	Skipped      bool           `json:"-"`
	Msg          string         `json:"-"`
	RC           *int           `json:"-"`
	Stdout       string         `json:"-"`
	Stderr       string         `json:"-"`
	Diff         any            `json:"-"` // the module's "diff" value: a dict or a list of dicts
	AnsibleFacts map[string]any `json:"-"`
	Extra        map[string]any `json:"-"`

	// Cause is the message of the exception a module passed to fail_json
	// (Ansible's exception chain): not part of the result dict, but the
	// error display appends it to Msg (see ErrorMessage).
	Cause string `json:"-"`

	// ErrorChain, when set, displays the failure as ansible-core shows an
	// exception with a cause: the outer error (with the task's source
	// context) "<<< caused by >>>" the inner one.
	ErrorChain *ErrorChain `json:"-"`

	// ErrorText, when set, is the error block's message in place of the
	// result's, located at ErrorFile:ErrorLine:ErrorCol (an expression
	// that failed to evaluate) rather than at the task.
	ErrorText           string `json:"-"`
	ErrorFile           string `json:"-"`
	ErrorLine, ErrorCol int    `json:"-"`

	// Control-plane display hints; never cross the agent wire.
	Origin        string `json:"-"` // "action" (control-side) or "module"
	VerboseAlways bool   `json:"-"` // shown with its JSON even at -v0 (debug, assert)
	DelegatedTo   string `json:"-"` // delegate_to target, when not the host itself
	ShowDiff      bool   `json:"-"` // diff mode is on for the task: display Diff
	Censored      bool   `json:"-"` // no_log: display only the censored placeholder
}

// ErrorChain is a two-level exception chain for error display.
type ErrorChain struct {
	Outer string // outer event message, shown with the source context
	// OuterFile/OuterLine/OuterCol locate the outer event, when it is not
	// the task (a keyword's value).
	OuterFile           string
	OuterLine, OuterCol int
	Inner               string // cause message
	Help                string // the cause's help text

	// InnerFile/InnerLine/InnerCol locate the cause's origin, when known.
	InnerFile           string
	InnerLine, InnerCol int

	// Mid, when set, is a cause between the two (Outer caused by Mid,
	// caused by Inner), located at MidFile:MidLine:MidCol.
	Mid             string
	MidFile         string
	MidLine, MidCol int
}

// causeKey carries Result.Cause across the agent wire.
const causeKey = "_understudy_cause"

// originKey carries Origin "action" across the wire: a module that
// performs action-plugin work (copy) reports action-level failures.
const originKey = "_understudy_origin"

// msgTextKey carries Msg across the wire when the result's msg is not a
// string (Extra["msg"] holds it): Msg is then its Python str(), which
// the error display shows.
const msgTextKey = "_understudy_msg_text"

// structuredMsg reports whether Extra carries a non-string msg, which
// then is the result's msg (Msg only being its display text).
func (r *Result) structuredMsg() bool {
	v, ok := r.Extra["msg"]
	if !ok || v == nil {
		return false
	}
	_, isStr := v.(string)
	return !isStr
}

// Fail builds a failed result with a formatted message.
func Fail(format string, args ...any) *Result {
	return &Result{Failed: true, Msg: fmt.Sprintf(format, args...)}
}

// ErrorMessage is the brief error message ansible-core displays for a
// failure: Msg, with a chained Cause appended the way
// _event_utils.deduplicate_message_parts does.
func (r *Result) ErrorMessage() string {
	if r.Cause == "" {
		return r.Msg
	}
	if strings.HasSuffix(r.Msg, r.Cause) {
		return r.Msg
	}
	return strings.TrimRight(r.Msg, ". ") + ": " + r.Cause
}

// IntPtr is a helper for the RC field.
func IntPtr(n int) *int { return &n }

// MarshalJSON flattens Extra alongside the typed fields.
func (r *Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireFloats(r.fields()))
}

// fields is the result as one dict: Extra with the typed fields.
func (r *Result) fields() map[string]any {
	m := make(map[string]any, len(r.Extra)+8)
	for k, v := range r.Extra {
		m[k] = v
	}
	m["changed"] = r.Changed
	if r.Failed {
		m["failed"] = true
	}
	if r.Skipped {
		m["skipped"] = true
	}
	if r.structuredMsg() {
		if r.Msg != "" {
			m[msgTextKey] = r.Msg
		}
	} else if r.Msg != "" {
		m["msg"] = r.Msg
	}
	if r.Cause != "" {
		m[causeKey] = r.Cause
	}
	if r.Origin == "action" {
		m[originKey] = r.Origin
	}
	if r.RC != nil {
		m["rc"] = *r.RC
	}
	if r.Stdout != "" || r.RC != nil {
		m["stdout"] = r.Stdout
	}
	if r.Stderr != "" || r.RC != nil {
		m["stderr"] = r.Stderr
	}
	if r.Diff != nil {
		m["diff"] = r.Diff
	}
	if len(r.AnsibleFacts) > 0 {
		m["ansible_facts"] = r.AnsibleFacts
	}
	return m
}

// UnmarshalJSON collects typed fields and stashes the rest in Extra.
func (r *Result) UnmarshalJSON(data []byte) error {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return err
	}
	// Keep integers integers (as the in-process path does): JSON has one
	// number type, but templates distinguish 8 from 8.0.
	r.fromFields(numbers(m).(map[string]any))
	return nil
}

// fromFields collects typed fields from a result dict and stashes the rest
// in Extra.
func (r *Result) fromFields(m map[string]any) {
	take := func(key string) (any, bool) {
		v, ok := m[key]
		if ok {
			delete(m, key)
		}
		return v, ok
	}
	if v, ok := take("changed"); ok {
		r.Changed, _ = v.(bool)
	}
	if v, ok := take("failed"); ok {
		r.Failed, _ = v.(bool)
	}
	if v, ok := take("skipped"); ok {
		r.Skipped, _ = v.(bool)
	}
	if v, ok := take("msg"); ok {
		switch v.(type) {
		case string, nil:
			r.Msg = fmt.Sprintf("%v", v)
			if v == "" {
				// An explicitly empty msg (command's success result) is part
				// of the result shape; keep it visible through Extra.
				m["msg"] = ""
			}
		default:
			// A structured msg stays as it is; its display text travels
			// separately.
			m["msg"] = v
			r.Msg = fmt.Sprintf("%v", v)
		}
	}
	if v, ok := take(msgTextKey); ok {
		r.Msg, _ = v.(string)
	}
	if v, ok := take(causeKey); ok {
		r.Cause, _ = v.(string)
	}
	if v, ok := take(originKey); ok {
		r.Origin, _ = v.(string)
	}
	_, hasOut := m["stdout"]
	_, hasErr := m["stderr"]
	// rc without stdout/stderr (fail_json(rc=...)) is a plain result key,
	// not a command result that grows stdout_lines.
	if v, ok := m["rc"]; ok && (hasOut || hasErr) {
		delete(m, "rc")
		switch n := v.(type) {
		case int64:
			r.RC = IntPtr(int(n))
		case float64:
			r.RC = IntPtr(int(n))
		}
	}
	// An explicitly empty stdout/stderr (a failed validate) is part of the
	// result shape even without rc; keep it visible through Extra.
	if v, ok := take("stdout"); ok {
		r.Stdout, _ = v.(string)
		if v == "" {
			m["stdout"] = ""
		}
	}
	if v, ok := take("stderr"); ok {
		r.Stderr, _ = v.(string)
		if v == "" {
			m["stderr"] = ""
		}
	}
	if v, ok := take("diff"); ok {
		r.Diff = v
	}
	if v, ok := take("ansible_facts"); ok {
		r.AnsibleFacts, _ = v.(map[string]any)
	}
	if len(m) > 0 {
		r.Extra = m
	}
}

// ToVars converts a result to the map shape a registered variable exposes,
// including the always-present booleans and stdout_lines/stderr_lines.
func (r *Result) ToVars() map[string]any {
	m := make(map[string]any, len(r.Extra)+10)
	for k, v := range r.Extra {
		m[k] = v
	}
	m["changed"] = r.Changed
	m["failed"] = r.Failed
	if r.Skipped {
		m["skipped"] = true // absent unless true, as in Ansible
	}
	if r.Msg != "" && !r.structuredMsg() {
		m["msg"] = r.Msg
	}
	if r.RC != nil {
		m["rc"] = int64(*r.RC)
		m["stdout"] = r.Stdout
		m["stderr"] = r.Stderr
		m["stdout_lines"] = splitLines(r.Stdout)
		m["stderr_lines"] = splitLines(r.Stderr)
	} else {
		// A module's stdout/stderr without rc: the action layer still
		// pre-splits them into lines.
		for _, s := range [][2]string{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
			if s[1] != "" {
				m[s[0]] = s[1]
			}
			// An explicitly empty one (in Extra) is split too.
			if v, ok := m[s[0]].(string); ok {
				if _, ok := m[s[0]+"_lines"]; !ok {
					m[s[0]+"_lines"] = splitLines(v)
				}
			}
		}
	}
	if len(r.AnsibleFacts) > 0 {
		m["ansible_facts"] = r.AnsibleFacts
	}
	if r.Diff != nil {
		m["diff"] = r.Diff
	}
	return m
}

func splitLines(s string) []any {
	if s == "" {
		return []any{}
	}
	// str.splitlines: \r\n, \r, \v, \f, \x1c-\x1e, \x85 and the Unicode
	// line and paragraph separators end lines too.
	var out []any
	start := 0
	for i, r := range s {
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			continue
		}
		if r == '\n' && i > 0 && s[i-1] == '\r' {
			start = i + 1
			continue
		}
		out = append(out, s[start:i])
		start = i + len(string(r))
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// numbers converts decoded json.Numbers the way Python's json module
// would: integer literals to int64, the rest to float64.
func numbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, e := range t {
			t[k] = numbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = numbers(e)
		}
		return t
	}
	return v
}
