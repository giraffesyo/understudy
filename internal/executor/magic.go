package executor

import (
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/vars"
)

// AnsibleVersion is the ansible-core release understudy is compatible with,
// exposed as the ansible_version magic variable.
var AnsibleVersion = map[string]any{
	"full": "2.21.0", "major": int64(2), "minor": int64(21), "revision": int64(0), "string": "2.21.0",
}

func strList(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// setRunMagic installs the run-level magic variables Ansible defines for
// every host: ansible_version, ansible_run_tags, ansible_skip_tags,
// ansible_diff_mode, ansible_forks, ansible_verbosity, ansible_config_file,
// ansible_inventory_sources, ansible_play_batch, the role-name lists, and
// an empty ansible_facts before facts are gathered (withPlay: the play's
// too: the batch and role names).
func (r *Runner) setRunMagic(c *vars.Context, host string, playHosts []string, withPlay bool) {
	c.SetMagic("ansible_version", AnsibleVersion)
	if py := playbookPython(); py != "" {
		c.SetMagic("ansible_playbook_python", py)
	}
	runTags := strList(r.Opts.Tags)
	if len(runTags) == 0 {
		runTags = []any{"all"}
	}
	c.SetMagic("ansible_run_tags", runTags)
	c.SetMagic("ansible_skip_tags", strList(r.Opts.SkipTags))
	c.SetMagic("ansible_diff_mode", r.Opts.Diff)
	c.SetMagic("ansible_forks", int64(r.Opts.Forks))
	c.SetMagic("ansible_verbosity", int64(r.Opts.Verbosity))
	if r.Opts.ConfigFile != "" {
		c.SetMagic("ansible_config_file", r.Opts.ConfigFile)
	} else {
		c.SetMagic("ansible_config_file", nil)
	}
	var sources []any
	for _, s := range r.Opts.Inventory {
		if abs, err := filepath.Abs(s); err == nil && !isHostList(s) {
			s = abs
		}
		sources = append(sources, s)
	}
	if sources == nil {
		sources = []any{}
	}
	c.SetMagic("ansible_inventory_sources", sources)
	if playHosts != nil && withPlay {
		c.SetMagic("ansible_play_batch", strList(playHosts))
	}
	if p := r.curPlay; p != nil && withPlay {
		play := strList(p.PlayRoleNames)
		deps := strList(p.DependentRoleNames)
		c.SetMagic("ansible_play_role_names", play)
		c.SetMagic("role_names", play)
		c.SetMagic("ansible_dependent_role_names", deps)
		c.SetMagic("ansible_role_names", append(append([]any{}, play...), deps...))
	}
	if _, ok := c.Get("ansible_facts"); !ok {
		c.SetMagic("ansible_facts", map[string]any{})
	}
}

// playbookPython is ansible_playbook_python: sys.executable of the Python
// ansible-playbook would run under, the first python3 on PATH ("" when
// there is none, and the variable is not set).
var playbookPython = sync.OnceValue(func() string {
	p, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return modules.PySysExecutable(p)
})

// isHostList reports an inline host list inventory ("a,b,").
func isHostList(s string) bool {
	for _, c := range s {
		if c == ',' {
			return true
		}
	}
	return false
}
