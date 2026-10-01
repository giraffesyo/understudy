package callback

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
)

// Tree is ansible-core's tree callback (ad-hoc --tree): each host's last
// result, as JSON, in a file named after the host in Dir.
type Tree struct {
	quiet
	Dir  string
	Warn func(string) // Display.error_as_warning
	d    Default
}

// NewTree builds the tree callback writing into dir.
func NewTree(dir string, verbosity int, warn func(string)) *Tree {
	return &Tree{Dir: dir, Warn: warn, d: Default{Verbosity: verbosity}}
}

var _ executor.Callback = (*Tree)(nil)

func (t *Tree) write(host, buf string) {
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		t.Warn(fmt.Sprintf("Unable to access or create the configured directory %s.", template.PyRepr(t.Dir)))
	}
	if err := os.WriteFile(filepath.Join(t.Dir, host), []byte(buf), 0o644); err != nil {
		t.Warn(fmt.Sprintf("Unable to write to %s's file.", template.PyRepr(host)))
	}
}

func (t *Tree) HostResult(host string, task *playbook.Task, res *agentproto.Result, _ bool, item any) {
	if item != nil || res.Skipped {
		return // a loop's items are reported with its result; skips are not written
	}
	t.write(host, t.d.dumpRaw(task, res, false))
}

func (t *Tree) LoopResult(host string, task *playbook.Task, res *agentproto.Result, _ bool) {
	if !res.Skipped {
		t.write(host, t.d.dumpRaw(task, res, false))
	}
}

func (t *Tree) HostUnreachable(host string, _ *playbook.Task, msg string) {
	t.write(host, template.PyJSON(map[string]any{"changed": false, "msg": msg, "unreachable": true}, 0, true, false))
}

// WithExtras adds callbacks after a stdout callback.
func WithExtras(primary executor.Callback, extras ...executor.Callback) executor.Callback {
	if len(extras) == 0 {
		return primary
	}
	return &fanout{primary: primary, extras: extras}
}
