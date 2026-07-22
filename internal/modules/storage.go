package modules

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(partedModule, "parted", "community.general.parted")
	Register(filesystemModule, "filesystem", "community.general.filesystem")
}

var partedSpec = args.Spec{
	"device":     {Required: true},
	"number":     {Type: "int"},
	"state":      {Default: "present", Choices: []string{"present", "absent", "info"}},
	"label":      {Default: "msdos"},
	"part_type":  {Default: "primary"},
	"part_start": {Default: "0%"},
	"part_end":   {Default: "100%"},
	"fs_type":    {},
	"unit":       {Default: "KiB"},
}

// partedModule manages partitions via parted, querying with -m print.
func partedModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := partedSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	device := p.Str("device")
	number := p.Int("number")
	state := p.Str("state")

	out, perr := runOut(env, "parted", "-m", "-s", device, "print")
	hasLabel := perr == nil && !strings.Contains(out, "unrecognised disk label")
	partExists := false
	if hasLabel && number > 0 {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, fmt.Sprintf("%d:", number)) {
				partExists = true
				break
			}
		}
	}

	res := &agentproto.Result{Extra: map[string]any{"device": device}}
	switch state {
	case "info":
		res.Extra["script_output"] = out
		return res
	case "present":
		if number <= 0 {
			return agentproto.Fail("state=present requires 'number'")
		}
		if partExists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if !hasLabel {
			if lblOut, err := runOut(env, "parted", "-s", device, "mklabel", p.Str("label")); err != nil {
				return agentproto.Fail("parted mklabel failed: %v: %s", err, tail(lblOut))
			}
		}
		argv := []string{"-s", device, "-a", "optimal", "mkpart", p.Str("part_type")}
		if fs := p.Str("fs_type"); fs != "" {
			argv = append(argv, fs)
		}
		argv = append(argv, p.Str("part_start"), p.Str("part_end"))
		if mkOut, err := runOut(env, "parted", argv...); err != nil {
			return agentproto.Fail("parted mkpart failed: %v: %s", err, tail(mkOut))
		}
		return res
	case "absent":
		if number <= 0 {
			return agentproto.Fail("state=absent requires 'number'")
		}
		if !partExists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if rmOut, err := runOut(env, "parted", "-s", device, "rm", fmt.Sprintf("%d", number)); err != nil {
			return agentproto.Fail("parted rm failed: %v: %s", err, tail(rmOut))
		}
	}
	return res
}

var filesystemSpec = args.Spec{
	"dev":      {Required: true, Aliases: []string{"device"}},
	"fstype":   {Required: true, Aliases: []string{"type"}},
	"force":    {Type: "bool", Default: false},
	"opts":     {},
	"resizefs": {Type: "bool", Default: false},
}

// filesystemModule creates a filesystem, using blkid for idempotence.
func filesystemModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := filesystemSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	dev := p.Str("dev")
	fstype := p.Str("fstype")

	out, _ := runOut(env, "blkid", "-o", "value", "-s", "TYPE", dev)
	current := strings.TrimSpace(out)

	res := &agentproto.Result{Extra: map[string]any{"dev": dev, "fstype": fstype}}
	if current == fstype && !p.Bool("force") {
		return res
	}
	if current != "" && current != fstype && !p.Bool("force") {
		return agentproto.Fail("%s already has a %s filesystem (use force=true to overwrite)", dev, current)
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	mkfs := "mkfs." + fstype
	var argv []string
	if p.Bool("force") {
		switch fstype {
		case "xfs":
			argv = append(argv, "-f")
		case "ext2", "ext3", "ext4":
			argv = append(argv, "-F")
		}
	}
	argv = append(argv, strings.Fields(p.Str("opts"))...)
	argv = append(argv, dev)
	if mkOut, err := runOut(env, mkfs, argv...); err != nil {
		return agentproto.Fail("%s failed: %v: %s", mkfs, err, tail(mkOut))
	}
	return res
}
