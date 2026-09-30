package callback

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/executor"
)

// External callback plugins: there is no Python, so a callback plugin is
// any executable named after the plugin in one of Ansible's callback
// plugin directories. It is started once per run and receives every Event
// as one JSON object per line on stdin; stdin closes after
// v2_playbook_on_stats and understudy waits (briefly) for it to exit. As a
// stdout_callback its stdout is the run's output; as an aggregate callback
// its stdout goes to stderr so it cannot corrupt the main output.

// PluginDirs lists where callback plugins are searched, in Ansible's
// order: ANSIBLE_CALLBACK_PLUGINS / callback_plugins (cfgDirs), the
// playbook-adjacent callback_plugins/, then the user and system dirs.
func PluginDirs(cfgDirs []string, playbookDir string) []string {
	dirs := append([]string{}, cfgDirs...)
	if playbookDir != "" {
		dirs = append(dirs, filepath.Join(playbookDir, "callback_plugins"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".ansible", "plugins", "callback"))
	}
	return append(dirs, "/usr/share/ansible/plugins/callback")
}

// findPlugin locates an executable callback plugin. It also reports a
// Python-only plugin of that name (which cannot be loaded).
func findPlugin(name string, dirs []string) (path string, pythonOnly bool) {
	short := name[strings.LastIndex(name, ".")+1:]
	for _, d := range dirs {
		for _, cand := range []string{filepath.Join(d, name), filepath.Join(d, short)} {
			if info, err := os.Stat(cand); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return cand, false
			}
		}
		if _, err := os.Stat(filepath.Join(d, short+".py")); err == nil {
			pythonOnly = true
		}
	}
	return "", pythonOnly
}

// execPlugin runs an external callback plugin process.
type execPlugin struct {
	executor.Callback // the event adapter feeding w
	name              string
	cmd               *exec.Cmd
	w                 *bufio.Writer
	stdin             io.WriteCloser
	broken            bool
	warn              func(string)
}

func startExecPlugin(name, path string, stdout bool, warn func(string)) (*execPlugin, error) {
	cmd := exec.Command(path)
	if stdout {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = os.Stderr
	}
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), fmt.Sprintf("UNDERSTUDY_CALLBACK_EVENT_VERSION=%d", EventVersion))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("callback plugin %s: %v", name, err)
	}
	p := &execPlugin{name: name, cmd: cmd, stdin: stdin, w: bufio.NewWriter(stdin), warn: warn}
	p.Callback = newEvents(p.send)
	return p, nil
}

func (p *execPlugin) send(ev Event) {
	if p.broken {
		return
	}
	data, err := json.Marshal(ev)
	if err == nil {
		_, err = p.w.Write(append(data, '\n'))
	}
	if err == nil {
		err = p.w.Flush() // plugins see events as they happen
	}
	if err != nil {
		p.broken = true
		p.warn(fmt.Sprintf("Failure using method (%s) in callback plugin (%s): %v", ev.Event, p.name, err))
	}
	if ev.Event == "v2_playbook_on_stats" {
		p.finish()
	}
}

// finish closes the event stream and waits up to 30s for the plugin.
func (p *execPlugin) finish() {
	p.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			p.warn(fmt.Sprintf("callback plugin %s exited with error: %v", p.name, err))
		}
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
		p.warn(fmt.Sprintf("callback plugin %s did not exit after the run; killed", p.name))
	}
}
