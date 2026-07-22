// Package agentproto defines the wire contract between the control binary
// and the remote agent. Both import it; it must depend only on the stdlib.
package agentproto

import (
	"encoding/json"
	"fmt"
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
	Proto      int               `json:"proto"`
	Op         string            `json:"op"` // "task"; future: session ops
	Module     string            `json:"module"`
	Args       map[string]any    `json:"args,omitempty"`
	FreeForm   string            `json:"free_form,omitempty"` // command/shell raw params
	CheckMode  bool              `json:"check_mode,omitempty"`
	Diff       bool              `json:"diff,omitempty"`
	Env        map[string]string `json:"env,omitempty"` // task environment: for shell-outs
	PayloadLen int64             `json:"payload_len,omitempty"`
	BecomeUser string            `json:"become_user,omitempty"` // informational
	Background bool              `json:"background,omitempty"`  // async poll:0 fire-and-forget
}

// Diff is a before/after pair rendered by --diff.
type Diff struct {
	Before       string `json:"before,omitempty"`
	After        string `json:"after,omitempty"`
	BeforeHeader string `json:"before_header,omitempty"`
	AfterHeader  string `json:"after_header,omitempty"`
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
	Diff         []Diff         `json:"-"`
	AnsibleFacts map[string]any `json:"-"`
	Extra        map[string]any `json:"-"`
}

// Fail builds a failed result with a formatted message.
func Fail(format string, args ...any) *Result {
	return &Result{Failed: true, Msg: fmt.Sprintf(format, args...)}
}

// IntPtr is a helper for the RC field.
func IntPtr(n int) *int { return &n }

// MarshalJSON flattens Extra alongside the typed fields.
func (r *Result) MarshalJSON() ([]byte, error) {
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
	if r.Msg != "" {
		m["msg"] = r.Msg
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
	if len(r.Diff) > 0 {
		m["diff"] = r.Diff
	}
	if len(r.AnsibleFacts) > 0 {
		m["ansible_facts"] = r.AnsibleFacts
	}
	return json.Marshal(m)
}

// UnmarshalJSON collects typed fields and stashes the rest in Extra.
func (r *Result) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
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
		r.Msg = fmt.Sprintf("%v", v)
	}
	if v, ok := take("rc"); ok {
		if f, isNum := v.(float64); isNum {
			r.RC = IntPtr(int(f))
		}
	}
	if v, ok := take("stdout"); ok {
		r.Stdout, _ = v.(string)
	}
	if v, ok := take("stderr"); ok {
		r.Stderr, _ = v.(string)
	}
	if v, ok := take("diff"); ok {
		if raw, err := json.Marshal(v); err == nil {
			json.Unmarshal(raw, &r.Diff)
		}
	}
	if v, ok := take("ansible_facts"); ok {
		r.AnsibleFacts, _ = v.(map[string]any)
	}
	if len(m) > 0 {
		r.Extra = m
	}
	return nil
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
	m["skipped"] = r.Skipped
	if r.Msg != "" {
		m["msg"] = r.Msg
	}
	if r.RC != nil {
		m["rc"] = int64(*r.RC)
		m["stdout"] = r.Stdout
		m["stderr"] = r.Stderr
		m["stdout_lines"] = splitLines(r.Stdout)
		m["stderr_lines"] = splitLines(r.Stderr)
	}
	if len(r.AnsibleFacts) > 0 {
		m["ansible_facts"] = r.AnsibleFacts
	}
	return m
}

func splitLines(s string) []any {
	if s == "" {
		return []any{}
	}
	var out []any
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
