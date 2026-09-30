package callback

import (
	"fmt"
	"sync"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
)

// EventVersion identifies the event schema sent to external callbacks.
const EventVersion = 1

// Event is one callback event, named after ansible-core's CallbackBase
// hooks (v2_playbook_on_task_start, v2_runner_on_ok, ...). External
// callback plugins receive these as JSON lines; Go programs embedding
// understudy receive them through Options.OnEvent.
type Event struct {
	Event string `json:"event"`

	Play *EventPlay `json:"play,omitempty"`
	Task *EventTask `json:"task,omitempty"`

	Host   string         `json:"host,omitempty"`
	Result map[string]any `json:"result,omitempty"`

	IgnoreErrors bool   `json:"ignore_errors,omitempty"`
	IsHandler    bool   `json:"is_handler,omitempty"`
	Item         any    `json:"item,omitempty"`
	RetriesLeft  *int   `json:"retries_left,omitempty"`
	JobID        string `json:"jid,omitempty"`

	// v2_playbook_on_include
	IncludedFile  string   `json:"included_file,omitempty"`
	IncludedHosts []string `json:"hosts,omitempty"`

	// v2_playbook_on_stats: per-host recap counters.
	Stats map[string]map[string]int `json:"stats,omitempty"`

	// v2_playbook_on_start
	Version int `json:"version,omitempty"`
}

type EventPlay struct {
	Name  string `json:"name"`
	Hosts string `json:"hosts"`
}

type EventTask struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Role   string `json:"role,omitempty"`
	Path   string `json:"path,omitempty"` // "file:line" where the task is defined
}

// events adapts the executor's callback hooks into Events for a sink.
type events struct {
	mu      sync.Mutex
	sink    func(Event)
	started bool
	names   map[*playbook.Task]string
}

func newEvents(sink func(Event)) *events {
	return &events{sink: sink, names: map[*playbook.Task]string{}}
}

var _ executor.Callback = (*events)(nil)

func (e *events) emit(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		e.started = true
		e.sink(Event{Event: "v2_playbook_on_start", Version: EventVersion})
	}
	e.sink(ev)
}

func (e *events) task(t *playbook.Task) *EventTask {
	e.mu.Lock()
	name := e.names[t]
	e.mu.Unlock()
	if name == "" {
		name = t.Name
	}
	if name == "" {
		name = t.Module
	}
	et := &EventTask{Name: name, Action: t.Module, Role: t.RoleName}
	if t.Src.File != "" {
		et.Path = fmt.Sprintf("%s:%d", t.Src.File, t.Src.Line)
	}
	return et
}

func resultMap(res *agentproto.Result) map[string]any {
	m, _ := template.Plain(res.ToVars()).(map[string]any)
	return m
}

func (e *events) PlayStart(p *playbook.Play) {
	e.emit(Event{Event: "v2_playbook_on_play_start", Play: &EventPlay{Name: p.Name, Hosts: p.HostPattern}})
}

func (e *events) NoHostsRemaining() {
	e.emit(Event{Event: "v2_playbook_on_no_hosts_remaining"})
}

func (e *events) TaskStart(t *playbook.Task, name string, handler bool) {
	e.mu.Lock()
	e.names[t] = name
	e.mu.Unlock()
	ev := "v2_playbook_on_task_start"
	if handler {
		ev = "v2_playbook_on_handler_task_start"
	}
	e.emit(Event{Event: ev, Task: e.task(t), IsHandler: handler})
}

func (e *events) HostResult(host string, t *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	kind := "ok"
	switch {
	case res.Failed:
		kind = "failed"
	case res.Skipped:
		kind = "skipped"
	}
	name := "v2_runner_on_" + kind
	if item != nil || res.Extra["ansible_loop_var"] != nil {
		name = "v2_runner_item_on_" + kind
	}
	e.emit(Event{Event: name, Host: host, Task: e.task(t), Result: resultMap(res), IgnoreErrors: ignored, Item: template.Plain(item)})
}

func (e *events) LoopResult(host string, t *playbook.Task, res *agentproto.Result, ignored bool) {
	kind := "ok"
	switch {
	case res.Failed:
		kind = "failed"
	case res.Skipped:
		kind = "skipped"
	}
	e.emit(Event{Event: "v2_runner_on_" + kind, Host: host, Task: e.task(t), Result: resultMap(res), IgnoreErrors: ignored})
}

func (e *events) HostUnreachable(host string, t *playbook.Task, msg string) {
	e.emit(Event{Event: "v2_runner_on_unreachable", Host: host, Task: e.task(t),
		Result: map[string]any{"changed": false, "msg": msg, "unreachable": true}})
}

func (e *events) Included(t *playbook.Task, target string, hosts []string, item any, hasItem bool) {
	ev := Event{Event: "v2_playbook_on_include", Task: e.task(t), IncludedFile: target, IncludedHosts: hosts}
	if hasItem {
		ev.Item = template.Plain(item)
	}
	e.emit(ev)
}

func (e *events) Retrying(host string, t *playbook.Task, _ string, left int, res *agentproto.Result) {
	e.emit(Event{Event: "v2_runner_retry", Host: host, Task: e.task(t), Result: resultMap(res), RetriesLeft: &left})
}

func (e *events) AsyncPoll(host, jid string) {
	e.emit(Event{Event: "v2_runner_on_async_poll", Host: host, JobID: jid})
}

func (e *events) AsyncDone(host, jid string, failed bool) {
	name := "v2_runner_on_async_ok"
	if failed {
		name = "v2_runner_on_async_failed"
	}
	e.emit(Event{Event: name, Host: host, JobID: jid})
}

func (e *events) Recap(stats map[string]*executor.HostStats, order []string) {
	out := make(map[string]map[string]int, len(stats))
	for h, st := range stats {
		if !st.Processed() {
			continue
		}
		out[h] = map[string]int{"ok": st.OK, "changed": st.Changed, "unreachable": st.Unreachable,
			"failures": st.Failed, "skipped": st.Skipped, "rescued": st.Rescued, "ignored": st.Ignored}
	}
	e.emit(Event{Event: "v2_playbook_on_stats", Stats: out})
}

// NewEventCallback returns a callback that delivers every event to sink
// (serially). Used by external plugins and the Go API's OnEvent.
func NewEventCallback(sink func(Event)) executor.Callback { return newEvents(sink) }
