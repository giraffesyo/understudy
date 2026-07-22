// Package cli implements argument parsing and the playbook/adhoc
// subcommands. Flag parsing is hand-rolled: stdlib flag cannot count -vvv,
// collect repeated -e, or intersperse positionals.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/yaml"
)

const version = "0.1.0-dev"

// Main dispatches on argv[0] (multicall) then subcommands.
func Main() int {
	args := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "ansible-playbook":
		return playbookCmd(args)
	case "ansible":
		return adhocCmd(args)
	}
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "playbook":
		return playbookCmd(args[1:])
	case "adhoc":
		return adhocCmd(args[1:])
	case "version", "--version":
		fmt.Printf("understudy %s\n", version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "understudy: unknown command %q\n\n", args[0])
	usage()
	return 1
}

func usage() {
	fmt.Fprint(os.Stderr, `understudy - an Ansible-compatible automation tool

Usage:
  understudy playbook [options] <playbook.yml>...   run playbooks
  understudy adhoc [options] <pattern> -m <module>  run an ad-hoc task
  understudy version                                print version

Common options:
  -i INVENTORY      inventory file/dir, or literal list: 'host1,host2,'
  -l PATTERN        limit hosts
  -e KEY=VAL|@file  extra variables (repeatable)
  -f N              parallel forks (default 5)
  -c CONNECTION     connection type (ssh, local)
  -b, -K, -u USER   become / ask become pass / remote user
  --check, --diff   check mode, show diffs
  -v/-vv/-vvv       verbosity
`)
}

// parsedArgs holds common flag values.
type parsedArgs struct {
	inventory  []string
	limit      string
	extraVars  map[string]any
	forks      int
	verbosity  int
	check      bool
	diff       bool
	become     bool
	becomeUser string
	askBecome  bool
	askPass    bool
	remoteUser string
	privateKey string
	connection string
	tags       string
	skipTags   string
	syntax     bool
	listHosts  bool
	listTasks  bool
	module     string // adhoc -m
	moduleArgs string // adhoc -a
	positional []string
}

func parseArgs(args []string) (*parsedArgs, error) {
	p := &parsedArgs{extraVars: map[string]any{}} // forks 0 = unset (cfg default applies)
	i := 0
	next := func(flag string) (string, error) {
		i++
		if i >= len(args) {
			return "", fmt.Errorf("flag %s requires a value", flag)
		}
		return args[i], nil
	}
	for ; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case a == "-i" || a == "--inventory":
			var v string
			if v, err = next(a); err == nil {
				p.inventory = append(p.inventory, v)
			}
		case strings.HasPrefix(a, "--inventory="):
			p.inventory = append(p.inventory, a[len("--inventory="):])
		case a == "-l" || a == "--limit":
			p.limit, err = next(a)
		case a == "-e" || a == "--extra-vars":
			var v string
			if v, err = next(a); err == nil {
				err = parseExtraVars(v, p.extraVars)
			}
		case strings.HasPrefix(a, "--extra-vars="):
			err = parseExtraVars(a[len("--extra-vars="):], p.extraVars)
		case a == "-f" || a == "--forks":
			var v string
			if v, err = next(a); err == nil {
				p.forks, err = strconv.Atoi(v)
			}
		case a == "-t" || a == "--tags":
			p.tags, err = next(a)
		case a == "--skip-tags":
			p.skipTags, err = next(a)
		case a == "--check":
			p.check = true
		case a == "--diff":
			p.diff = true
		case a == "-b" || a == "--become":
			p.become = true
		case a == "--become-user":
			p.becomeUser, err = next(a)
		case a == "-K" || a == "--ask-become-pass":
			p.askBecome = true
		case a == "-k" || a == "--ask-pass":
			p.askPass = true
		case a == "-u" || a == "--user":
			p.remoteUser, err = next(a)
		case a == "--private-key" || a == "--key-file":
			p.privateKey, err = next(a)
		case a == "-c" || a == "--connection":
			p.connection, err = next(a)
		case a == "--syntax-check":
			p.syntax = true
		case a == "--list-hosts":
			p.listHosts = true
		case a == "--list-tasks":
			p.listTasks = true
		case a == "-m" || a == "--module-name":
			p.module, err = next(a)
		case a == "-a" || a == "--args":
			p.moduleArgs, err = next(a)
		case a == "--version":
			fmt.Printf("understudy %s\n", version)
			os.Exit(0)
		case a == "-h" || a == "--help":
			usage()
			os.Exit(0)
		case strings.HasPrefix(a, "-v") && strings.TrimLeft(a, "-v") == "":
			p.verbosity += strings.Count(a, "v")
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %q", a)
		default:
			p.positional = append(p.positional, a)
		}
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// buildOptions assembles executor options from CLI flags layered over
// ansible.cfg (CLI > env > cfg > defaults), running -k/-K prompts once.
func buildOptions(p *parsedArgs, baseDir string) (executor.Options, error) {
	cfg, err := config.Load()
	if err != nil {
		return executor.Options{}, err
	}
	forks := p.forks
	if forks <= 0 {
		forks = cfg.Forks
	}
	remoteUser := p.remoteUser
	if remoteUser == "" {
		remoteUser = cfg.RemoteUser
	}
	privateKey := p.privateKey
	if privateKey == "" {
		privateKey = cfg.PrivateKeyFile
	}
	opts := executor.Options{
		Forks:      forks,
		CheckMode:  p.check,
		Diff:       p.diff,
		Verbosity:  p.verbosity,
		ExtraVars:  p.extraVars,
		Become:     p.become,
		BecomeUser: p.becomeUser,
		Connection: p.connection,
		BaseDir:    baseDir,
		Tags:       splitCSV(p.tags),
		SkipTags:   splitCSV(p.skipTags),
	}
	opts.ConnOpts = connection.ManagerOptions{
		RemoteUser:      remoteUser,
		PrivateKey:      privateKey,
		HostKeyChecking: cfg.HostKeyChecking,
		Timeout:         cfg.Timeout,
		RemoteTmp:       cfg.RemoteTmp,
		KeyPassphrase: func() (string, error) {
			return promptSecret("SSH key passphrase")
		},
	}
	if p.askPass {
		pw, err := promptSecret("SSH password")
		if err != nil {
			return opts, err
		}
		opts.ConnOpts.Password = pw
	}
	if p.askBecome {
		pw, err := promptSecret("BECOME password")
		if err != nil {
			return opts, err
		}
		opts.BecomePass = pw
	}
	return opts, nil
}

// promptSecret reads a password without echo.
func promptSecret(label string) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", label, err)
	}
	return string(data), nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseExtraVars handles -e k=v, -e '{"json": true}', and -e @file.yml.
func parseExtraVars(s string, into map[string]any) error {
	switch {
	case strings.HasPrefix(s, "@"):
		data, err := os.ReadFile(s[1:])
		if err != nil {
			return fmt.Errorf("extra-vars file: %w", err)
		}
		v, err := yaml.Unmarshal(data, s[1:])
		if err != nil {
			return err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("extra-vars file %s must contain a mapping", s[1:])
		}
		for k, val := range m {
			into[k] = val
		}
		return nil
	case strings.HasPrefix(strings.TrimSpace(s), "{"):
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return fmt.Errorf("extra-vars JSON: %w", err)
		}
		for k, val := range m {
			into[k] = val
		}
		return nil
	default:
		for _, pair := range strings.Fields(s) {
			eq := strings.IndexByte(pair, '=')
			if eq <= 0 {
				return fmt.Errorf("extra-vars: expected key=value, got %q", pair)
			}
			into[pair[:eq]] = pair[eq+1:]
		}
		return nil
	}
}

// loadInventory builds the inventory from -i sources (falling back to
// ansible.cfg's inventory setting), applying group_vars/host_vars adjacent
// to sources and to the playbook directory.
func loadInventory(p *parsedArgs, playbookDir string) (*inventory.Inventory, error) {
	sources := p.inventory
	if len(sources) == 0 {
		if cfg, err := config.Load(); err == nil && len(cfg.Inventory) > 0 {
			// Only use cfg inventory entries that exist (Ansible warns and
			// falls back to implicit localhost otherwise).
			for _, src := range cfg.Inventory {
				if _, err := os.Stat(src); err == nil {
					sources = append(sources, src)
				}
			}
		}
	}
	var varsDirs []string
	if playbookDir != "" {
		varsDirs = append(varsDirs, playbookDir)
	}
	return inventory.Load(sources, varsDirs)
}

func playbookCmd(args []string) int {
	p, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "understudy: %v\n", err)
		return 1
	}
	if len(p.positional) == 0 {
		fmt.Fprintln(os.Stderr, "understudy playbook: at least one playbook file is required")
		return 1
	}

	exit := 0
	for _, path := range p.positional {
		plays, err := playbook.LoadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 4
		}
		if err := playbook.ResolveRoles(plays, filepath.Dir(path), nil); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 4
		}
		if p.syntax {
			fmt.Printf("playbook: %s\n", path)
			continue
		}
		if p.listTasks {
			listTasks(path, plays)
			continue
		}
		inv, err := loadInventory(p, filepath.Dir(path))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		if p.listHosts {
			for i, play := range plays {
				name := play.Name
				if name == "" {
					name = play.HostPattern
				}
				matched, err := inv.Match(play.HostPattern)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
					return 1
				}
				fmt.Printf("\n  play #%d (%s): host count=%d\n", i+1, name, len(matched))
				for _, h := range matched {
					fmt.Printf("    %s\n", h.Name)
				}
			}
			continue
		}

		opts, err := buildOptions(p, filepath.Dir(path))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		cb := callback.New(p.verbosity)
		runner := executor.NewRunner(inv, cb, opts)
		runner.Limit = p.limit
		code, err := runner.Run(context.Background(), plays)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		if code != 0 {
			exit = code
		}
	}
	return exit
}

func listTasks(path string, plays []*playbook.Play) {
	fmt.Printf("\nplaybook: %s\n", path)
	for i, play := range plays {
		name := play.Name
		if name == "" {
			name = play.HostPattern
		}
		fmt.Printf("\n  play #%d (%s):\n", i+1, name)
		for _, t := range play.PreTasks {
			printTaskLine(t)
		}
		for _, t := range play.Tasks {
			printTaskLine(t)
		}
		for _, t := range play.PostTasks {
			printTaskLine(t)
		}
	}
}

func printTaskLine(t *playbook.Task) {
	name := t.Name
	if name == "" {
		name = t.Module
	}
	if len(t.Tags) > 0 {
		fmt.Printf("      %s\tTAGS: [%s]\n", name, strings.Join(t.Tags, ", "))
	} else {
		fmt.Printf("      %s\n", name)
	}
}

// adhocCmd synthesizes a one-task play: `understudy adhoc all -m ping`.
func adhocCmd(args []string) int {
	p, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "understudy: %v\n", err)
		return 1
	}
	if len(p.positional) != 1 {
		fmt.Fprintln(os.Stderr, "understudy adhoc: a host pattern is required")
		return 1
	}
	module := p.module
	if module == "" {
		module = "command"
	}
	task := &playbook.Task{
		Name:    module,
		Module:  module,
		LoopVar: "item",
		Src:     playbook.Pos{File: "<adhoc>", Line: 1},
	}
	if p.moduleArgs != "" {
		tmp := &playbook.Task{Module: module, LoopVar: "item"}
		if err := adhocArgs(tmp, module, p.moduleArgs); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		task.Args = tmp.Args
		task.FreeForm = tmp.FreeForm
	}
	gather := false
	play := &playbook.Play{
		Name:        "understudy Ad-Hoc",
		HostPattern: p.positional[0],
		GatherFacts: &gather,
		Tasks:       []*playbook.Task{task},
		Src:         playbook.Pos{File: "<adhoc>", Line: 1},
	}

	inv, err := loadInventory(p, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	opts, err := buildOptions(p, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	cb := callback.New(p.verbosity)
	runner := executor.NewRunner(inv, cb, opts)
	runner.Limit = p.limit
	code, err := runner.Run(context.Background(), []*playbook.Play{play})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	return code
}
