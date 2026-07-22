// Package callback renders execution events in the style of
// ansible-playbook's default callback: PLAY/TASK banners padded to 79
// columns, colored per-host result lines, and the PLAY RECAP table.
package callback

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
)

const bannerWidth = 79

type color string

const (
	cGreen  color = "\x1b[0;32m"
	cYellow color = "\x1b[0;33m"
	cRed    color = "\x1b[0;31m"
	cCyan   color = "\x1b[0;36m"
	cBlue   color = "\x1b[0;34m"
	cReset  color = "\x1b[0m"
)

// Default is the standard output callback.
type Default struct {
	Out       io.Writer
	Verbosity int
	NoColor   bool
	mu        sync.Mutex
	started   bool
}

// New builds the default callback, auto-detecting color support.
func New(verbosity int) *Default {
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("ANSIBLE_NOCOLOR") != "" ||
		!term.IsTerminal(int(os.Stdout.Fd()))
	return &Default{Out: os.Stdout, Verbosity: verbosity, NoColor: noColor}
}

func (d *Default) paint(c color, s string) string {
	if d.NoColor {
		return s
	}
	return string(c) + s + string(cReset)
}

func (d *Default) banner(text string) string {
	pad := bannerWidth - len(text) - 1
	if pad < 0 {
		pad = 0
	}
	return text + " " + strings.Repeat("*", pad)
}

func (d *Default) PlayStart(play *playbook.Play) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := play.Name
	if name == "" {
		name = play.HostPattern
	}
	if d.started {
		fmt.Fprintln(d.Out)
	}
	d.started = true
	fmt.Fprintln(d.Out, d.banner(fmt.Sprintf("PLAY [%s]", name)))
}

func (d *Default) TaskStart(task *playbook.Task, handler bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := task.Name
	if name == "" {
		name = task.Module
	}
	kind := "TASK"
	if handler {
		kind = "RUNNING HANDLER"
	}
	fmt.Fprintf(d.Out, "\n%s\n", d.banner(fmt.Sprintf("%s [%s]", kind, name)))
}

func (d *Default) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	d.mu.Lock()
	defer d.mu.Unlock()

	itemSuffix := ""
	if item != nil {
		itemSuffix = fmt.Sprintf(" => (item=%s)", compactValue(item))
	}

	switch {
	case res.Failed:
		detail := resultJSON(res)
		line := fmt.Sprintf("fatal: [%s]: FAILED!%s => %s", host, itemSuffix, detail)
		fmt.Fprintln(d.Out, d.paint(cRed, line))
		if ignored {
			fmt.Fprintln(d.Out, d.paint(cCyan, "...ignoring"))
		}
	case res.Skipped:
		line := fmt.Sprintf("skipping: [%s]%s", host, itemSuffix)
		if d.Verbosity > 0 && res.Msg != "" {
			line += fmt.Sprintf(" => {\"msg\": %q}", res.Msg)
		}
		fmt.Fprintln(d.Out, d.paint(cCyan, line))
	case res.Changed:
		line := fmt.Sprintf("changed: [%s]%s", host, itemSuffix)
		if d.Verbosity > 0 {
			line += " => " + resultJSON(res)
		}
		fmt.Fprintln(d.Out, d.paint(cYellow, line))
	default:
		line := fmt.Sprintf("ok: [%s]%s", host, itemSuffix)
		if d.Verbosity > 0 {
			line += " => " + resultJSON(res)
		} else if task.Module == "debug" {
			// debug prints its message even at verbosity 0.
			line += " => " + resultJSON(res)
		}
		fmt.Fprintln(d.Out, d.paint(cGreen, line))
	}
}

func (d *Default) HostUnreachable(host string, task *playbook.Task, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	line := fmt.Sprintf("fatal: [%s]: UNREACHABLE! => {\"changed\": false, \"msg\": %q, \"unreachable\": true}", host, msg)
	fmt.Fprintln(d.Out, d.paint(cRed, line))
}

func (d *Default) Recap(stats map[string]*executor.HostStats, order []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.Out, "\n%s\n", d.banner("PLAY RECAP"))
	for _, host := range order {
		st := stats[host]
		if st == nil {
			continue
		}
		hostLabel := host
		switch {
		case st.Failed > 0 || st.Unreachable > 0:
			hostLabel = d.paint(cRed, host)
		case st.Changed > 0:
			hostLabel = d.paint(cYellow, host)
		default:
			hostLabel = d.paint(cGreen, host)
		}
		fmt.Fprintf(d.Out,
			"%-26s : %s    %s    %s    %s    %s    %s    %s\n",
			hostLabel,
			d.stat("ok", st.OK, cGreen, st.OK > 0),
			d.stat("changed", st.Changed, cYellow, st.Changed > 0),
			d.stat("unreachable", st.Unreachable, cRed, st.Unreachable > 0),
			d.stat("failed", st.Failed, cRed, st.Failed > 0),
			d.stat("skipped", st.Skipped, cCyan, st.Skipped > 0),
			d.stat("rescued", st.Rescued, cGreen, st.Rescued > 0),
			d.stat("ignored", st.Ignored, cRed, st.Ignored > 0),
		)
	}
}

func (d *Default) stat(label string, n int, c color, hot bool) string {
	s := fmt.Sprintf("%s=%d", label, n)
	if hot {
		return d.paint(c, s)
	}
	return s
}

// resultJSON renders a result the way ansible-playbook shows it after =>.
func resultJSON(res *agentproto.Result) string {
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Sprintf("{\"msg\": %q}", res.Msg)
	}
	return string(data)
}

func compactValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}
