// Package cli implements argument parsing and the playbook/adhoc
// subcommands. Flag parsing is hand-rolled: stdlib flag cannot count -vvv,
// collect repeated -e, or intersperse positionals.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vault"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// version is stamped at build time by the Makefile and the release
// workflow: -ldflags "-X github.com/giraffesyo/understudy/internal/cli.version=v1.2.3".
// Unstamped builds fall back to the module version `go install` records.
var version = "0.1.0-dev"

func init() {
	if version != "0.1.0-dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

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
	case "vault":
		return vaultCmd(args[1:])
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
	askVault   bool
	vaultFiles []string
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

	becomeMethod   string
	becomePassFile string
	connPassFile   string
	timeout        int
	forceHandlers  bool
	startAtTask    string
	step           bool
	listTags       bool
	sshArgs        []string // --ssh-common-args & co: not applicable to the native client
}

// cliFlag is one ansible-playbook/ansible option: its spellings, whether it
// takes a value, and how it applies.
type cliFlag struct {
	names []string
	value bool
	apply func(p *parsedArgs, v string) error
}

func boolFlag(set func(*parsedArgs)) func(*parsedArgs, string) error {
	return func(p *parsedArgs, _ string) error { set(p); return nil }
}

var cliFlags = []cliFlag{
	{[]string{"-i", "--inventory", "--inventory-file"}, true, func(p *parsedArgs, v string) error { p.inventory = append(p.inventory, v); return nil }},
	{[]string{"-l", "--limit"}, true, func(p *parsedArgs, v string) error { p.limit = v; return nil }},
	{[]string{"-e", "--extra-vars"}, true, func(p *parsedArgs, v string) error { return parseExtraVars(v, p.extraVars) }},
	{[]string{"-f", "--forks"}, true, func(p *parsedArgs, v string) (err error) { p.forks, err = strconv.Atoi(v); return }},
	{[]string{"-t", "--tags"}, true, func(p *parsedArgs, v string) error { p.tags = joinCSV(p.tags, v); return nil }},
	{[]string{"--skip-tags"}, true, func(p *parsedArgs, v string) error { p.skipTags = joinCSV(p.skipTags, v); return nil }},
	{[]string{"-C", "--check"}, false, boolFlag(func(p *parsedArgs) { p.check = true })},
	{[]string{"-D", "--diff"}, false, boolFlag(func(p *parsedArgs) { p.diff = true })},
	{[]string{"-b", "--become"}, false, boolFlag(func(p *parsedArgs) { p.become = true })},
	{[]string{"--become-user"}, true, func(p *parsedArgs, v string) error { p.becomeUser = v; return nil }},
	{[]string{"--become-method"}, true, func(p *parsedArgs, v string) error { p.becomeMethod = v; return nil }},
	{[]string{"-K", "--ask-become-pass"}, false, boolFlag(func(p *parsedArgs) { p.askBecome = true })},
	{[]string{"--become-password-file", "--become-pass-file"}, true, func(p *parsedArgs, v string) error { p.becomePassFile = v; return nil }},
	{[]string{"-k", "--ask-pass"}, false, boolFlag(func(p *parsedArgs) { p.askPass = true })},
	{[]string{"--connection-password-file", "--conn-pass-file"}, true, func(p *parsedArgs, v string) error { p.connPassFile = v; return nil }},
	{[]string{"-J", "--ask-vault-pass", "--ask-vault-password"}, false, boolFlag(func(p *parsedArgs) { p.askVault = true })},
	{[]string{"--vault-password-file", "--vault-pass-file"}, true, func(p *parsedArgs, v string) error { p.vaultFiles = append(p.vaultFiles, v); return nil }},
	{[]string{"--vault-id"}, true, func(p *parsedArgs, v string) error {
		// vault-id form is "label@source"; we use the source path.
		if at := strings.LastIndexByte(v, '@'); at >= 0 {
			v = v[at+1:]
		}
		if v == "prompt" {
			p.askVault = true
		} else {
			p.vaultFiles = append(p.vaultFiles, v)
		}
		return nil
	}},
	{[]string{"-u", "--user"}, true, func(p *parsedArgs, v string) error { p.remoteUser = v; return nil }},
	{[]string{"--private-key", "--key-file"}, true, func(p *parsedArgs, v string) error { p.privateKey = v; return nil }},
	{[]string{"-c", "--connection"}, true, func(p *parsedArgs, v string) error { p.connection = v; return nil }},
	{[]string{"-T", "--timeout"}, true, func(p *parsedArgs, v string) (err error) { p.timeout, err = strconv.Atoi(v); return }},
	{[]string{"--ssh-common-args", "--ssh-extra-args", "--sftp-extra-args", "--scp-extra-args"}, true, func(p *parsedArgs, v string) error {
		p.sshArgs = append(p.sshArgs, v)
		return nil
	}},
	{[]string{"--syntax-check"}, false, boolFlag(func(p *parsedArgs) { p.syntax = true })},
	{[]string{"--list-hosts"}, false, boolFlag(func(p *parsedArgs) { p.listHosts = true })},
	{[]string{"--list-tasks"}, false, boolFlag(func(p *parsedArgs) { p.listTasks = true })},
	{[]string{"--list-tags"}, false, boolFlag(func(p *parsedArgs) { p.listTags = true })},
	{[]string{"--force-handlers"}, false, boolFlag(func(p *parsedArgs) { p.forceHandlers = true })},
	{[]string{"--start-at-task"}, true, func(p *parsedArgs, v string) error { p.startAtTask = v; return nil }},
	{[]string{"--step"}, false, boolFlag(func(p *parsedArgs) { p.step = true })},
	// No fact cache and no Python module path: accepted, nothing to do.
	{[]string{"--flush-cache"}, false, boolFlag(func(*parsedArgs) {})},
	{[]string{"-M", "--module-path"}, true, func(*parsedArgs, string) error { return nil }},
	{[]string{"-m", "--module-name"}, true, func(p *parsedArgs, v string) error { p.module = v; return nil }},
	{[]string{"-a", "--args"}, true, func(p *parsedArgs, v string) error { p.moduleArgs = v; return nil }},
	{[]string{"--version"}, false, boolFlag(func(*parsedArgs) { fmt.Printf("understudy %s\n", version); os.Exit(0) })},
	{[]string{"-h", "--help"}, false, boolFlag(func(*parsedArgs) { usage(); os.Exit(0) })},
}

func joinCSV(prev, v string) string {
	if prev == "" {
		return v
	}
	return prev + "," + v
}

// parseArgs parses argparse-style options: "--long value", "--long=value",
// "-x value", "-xvalue", combined short switches ("-bK", "-vvv"), and "--"
// ending option parsing.
func parseArgs(args []string) (*parsedArgs, error) {
	p := &parsedArgs{extraVars: map[string]any{}} // forks 0 = unset (cfg default applies)
	byName := map[string]*cliFlag{}
	for i := range cliFlags {
		for _, n := range cliFlags[i].names {
			byName[n] = &cliFlags[i]
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(flag string, attached string, hasAttached bool) (string, error) {
			if hasAttached {
				return attached, nil
			}
			i++
			if i >= len(args) {
				return "", fmt.Errorf("argument %s: expected one argument", flag)
			}
			return args[i], nil
		}
		switch {
		case a == "--":
			p.positional = append(p.positional, args[i+1:]...)
			return p, nil
		case strings.HasPrefix(a, "--"):
			name, attached, hasAttached := strings.Cut(a, "=")
			f := byName[name]
			if f == nil {
				return nil, fmt.Errorf("unrecognized arguments: %s", a)
			}
			if !f.value {
				if hasAttached {
					return nil, fmt.Errorf("argument %s: ignored explicit argument %q", name, attached)
				}
				if err := f.apply(p, ""); err != nil {
					return nil, err
				}
				continue
			}
			v, err := value(name, attached, hasAttached)
			if err != nil {
				return nil, err
			}
			if err := f.apply(p, v); err != nil {
				return nil, err
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Short options, possibly combined: -bK, -vvv, -e@file, -ihosts.
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'v' {
					p.verbosity++
					continue
				}
				f := byName["-"+string(c)]
				if f == nil {
					return nil, fmt.Errorf("unrecognized arguments: %s", a)
				}
				if !f.value {
					if err := f.apply(p, ""); err != nil {
						return nil, err
					}
					continue
				}
				rest := a[j+1:]
				v, err := value("-"+string(c), rest, rest != "")
				if err != nil {
					return nil, err
				}
				if err := f.apply(p, v); err != nil {
					return nil, err
				}
				break
			}
		default:
			p.positional = append(p.positional, a)
		}
	}
	return p, nil
}

// setupVault builds the vault secrets and installs them as the inventory
// decrypt hook (so group_vars/host_vars decrypt during Load). Call once
// before loadInventory.
func setupVault(p *parsedArgs) (*vault.Secrets, error) {
	secrets, err := buildVaultSecrets(p)
	if err != nil {
		return nil, err
	}
	if !secrets.Empty() {
		inventory.Decrypt = secrets.MaybeDecryptFile
	}
	return secrets, nil
}

// buildOptions assembles executor options from CLI flags layered over
// ansible.cfg (CLI > env > cfg > defaults), running -k/-K prompts once.
func buildOptions(p *parsedArgs, baseDir string, secrets *vault.Secrets) (executor.Options, error) {
	cfg, err := config.Load()
	if err != nil {
		return executor.Options{}, err
	}
	if p.verbosity > 0 {
		// ansible-playbook -v announces its configuration source first.
		if cfg.Source == "" {
			fmt.Println("No config file found; using defaults")
		} else if abs, err := filepath.Abs(cfg.Source); err == nil {
			fmt.Printf("Using %s as config file\n", abs)
		}
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
	becomeMethod := ""
	if p.becomeMethod != "" {
		if becomeMethod = playbook.NormalizeBecomeMethod(p.becomeMethod); becomeMethod == "" {
			return executor.Options{}, fmt.Errorf("become method %q is not supported (supported: sudo, su, doas)", p.becomeMethod)
		}
	}
	timeout := cfg.Timeout
	if p.timeout > 0 {
		timeout = time.Duration(p.timeout) * time.Second
	}
	opts := executor.Options{
		ForceHandlers: p.forceHandlers,
		StartAtTask:   p.startAtTask,
		Step:          p.step,
		Forks:         forks,
		CheckMode:     p.check,
		Diff:          p.diff,
		Verbosity:     p.verbosity,
		ExtraVars:     p.extraVars,
		Become:        p.become,
		BecomeUser:    p.becomeUser,
		BecomeMethod:  becomeMethod,
		Connection:    p.connection,
		BaseDir:       baseDir,
		RolesPath:     cfg.RolesPath,
		Inventory:     p.inventory,
		Tags:          splitCSV(p.tags),
		SkipTags:      splitCSV(p.skipTags),

		NoDeprecationWarnings: !cfg.DeprecationWarnings,
		InjectFactsSet:        cfg.InjectFactsSet,
		TaskTimeout:           cfg.TaskTimeout,
	}
	if cfg.Source != "" {
		if abs, err := filepath.Abs(cfg.Source); err == nil {
			opts.ConfigFile = abs
		}
	}
	opts.ConnOpts = connection.ManagerOptions{
		RemoteUser:      remoteUser,
		PrivateKey:      privateKey,
		HostKeyChecking: cfg.HostKeyChecking,
		Timeout:         timeout,
		RemoteTmp:       cfg.RemoteTmp,
		SSHArgs:         p.sshArgs,
		Warn: func(msg string) {
			fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg)
		},
		KeyPassphrase: func() (string, error) {
			return promptSecret("SSH key passphrase")
		},
	}
	if p.connPassFile != "" {
		pw, err := readPasswordFile(p.connPassFile)
		if err != nil {
			return opts, err
		}
		opts.ConnOpts.Password = pw
	}
	if p.becomePassFile != "" {
		pw, err := readPasswordFile(p.becomePassFile)
		if err != nil {
			return opts, err
		}
		opts.BecomePass = pw
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

	opts.Vault = secrets
	return opts, nil
}

// buildVaultSecrets assembles vault passwords from --vault-password-file,
// --ask-vault-pass, and the ANSIBLE_VAULT_PASSWORD_FILE env var.
func buildVaultSecrets(p *parsedArgs) (*vault.Secrets, error) {
	secrets := vault.NewSecrets()
	files := p.vaultFiles
	if len(files) == 0 {
		if env := os.Getenv("ANSIBLE_VAULT_PASSWORD_FILE"); env != "" {
			files = append(files, env)
		}
	}
	for _, f := range files {
		pw, err := vault.LoadPasswordFile(f)
		if err != nil {
			return nil, fmt.Errorf("vault password file: %w", err)
		}
		secrets.Add(pw)
	}
	if p.askVault {
		pw, err := promptSecret("Vault password")
		if err != nil {
			return nil, err
		}
		secrets.Add(pw)
	}
	return secrets, nil
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
		m, ok := yaml.PlainMap(v)
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

	// All playbooks load first and then run as one run with a single
	// recap, like ansible-playbook a.yml b.yml.
	type book struct {
		path  string
		plays []*playbook.Play
	}
	var books []book
	var rolesPath []string
	if cfg, err := config.Load(); err == nil {
		rolesPath = cfg.RolesPath
	}
	for _, path := range p.positional {
		// Load by absolute path: error origins show it, as in Ansible.
		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = path
		}
		plays, err := playbook.LoadFile(absPath)
		if err != nil {
			printError(err)
			return loadErrorCode(err)
		}
		if err := playbook.ResolveRoles(plays, filepath.Dir(absPath), rolesPath); err != nil {
			printError(err)
			return loadErrorCode(err)
		}
		dir, _ := filepath.Abs(filepath.Dir(path))
		for _, pl := range plays {
			pl.Dir = dir
		}
		books = append(books, book{path, plays})
	}
	// Module routing deprecations print as the tasks are resolved.
	if cfg, err := config.Load(); err != nil || cfg.DeprecationWarnings {
		var all []*playbook.Play
		for _, b := range books {
			all = append(all, b.plays...)
		}
		for _, w := range executor.RoutingDeprecationWarnings(all) {
			fmt.Fprint(os.Stderr, w)
		}
	}

	if p.syntax {
		for _, b := range books {
			fmt.Printf("\nplaybook: %s\n", b.path)
		}
		return 0
	}
	if p.listTasks || p.listTags {
		tags, skip := splitCSV(p.tags), splitCSV(p.skipTags)
		for _, b := range books {
			listTasks(b.path, b.plays, p.listTasks, p.listTags, tags, skip)
		}
		return 0
	}

	secrets, err := setupVault(p)
	if err != nil {
		printError(err)
		return 1
	}
	inv, err := loadInventory(p, filepath.Dir(p.positional[0]))
	if err != nil {
		printError(err)
		return 1
	}
	if p.listHosts {
		for _, b := range books {
			fmt.Printf("\nplaybook: %s\n", b.path)
			for i, play := range b.plays {
				matched, err := inv.Match(play.HostPattern)
				if err != nil {
					printError(err)
					return 1
				}
				if p.limit != "" {
					if limited, err := inv.Match(p.limit); err == nil {
						keep := map[string]bool{}
						for _, h := range limited {
							keep[h.Name] = true
						}
						var out []*inventory.Host
						for _, h := range matched {
							if keep[h.Name] {
								out = append(out, h)
							}
						}
						matched = out
					}
				}
				fmt.Printf("\n  %s\n", playHeader(i, play))
				fmt.Printf("    pattern: ['%s']\n    hosts (%d):\n", play.HostPattern, len(matched))
				for _, h := range matched {
					fmt.Printf("      %s\n", h.Name)
				}
			}
		}
		return 0
	}

	opts, err := buildOptions(p, filepath.Dir(p.positional[0]), secrets)
	if err != nil {
		printError(err)
		return 1
	}
	var all [][]*playbook.Play
	for _, b := range books {
		all = append(all, b.plays)
	}
	cb, err := buildCallback(p.verbosity, false, filepath.Dir(p.positional[0]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
		return 1
	}
	runner := executor.NewRunner(inv, cb, opts)
	runner.Limit = p.limit
	code, err := runner.RunPlaybooks(context.Background(), all)
	if err != nil {
		printError(err)
		return 1
	}
	return code
}

// playHeader is the list modes' "play #N (pattern): name\tTAGS: [...]".
func playHeader(i int, play *playbook.Play) string {
	return fmt.Sprintf("play #%d (%s): %s\tTAGS: [%s]", i+1, play.HostPattern, play.Name, strings.Join(play.Tags, ", "))
}

// listTasks prints --list-tasks / --list-tags output. Tasks are filtered by
// --tags/--skip-tags like a run ("never" tasks hidden unless requested);
// tags shown are each task's effective (inherited) tags.
func listTasks(path string, plays []*playbook.Play, showTasks, showTags bool, want, skip []string) {
	fmt.Printf("\nplaybook: %s\n", path)
	for i, play := range plays {
		fmt.Printf("\n  %s\n", playHeader(i, play))
		if showTasks {
			fmt.Println("    tasks:")
		}
		union := map[string]bool{}
		for _, section := range [][]*playbook.Task{play.PreTasks, play.Tasks, play.PostTasks} {
			for _, t := range expandImports(play, section) {
				tags := effectiveTags(play, t)
				for _, tg := range tags {
					union[tg] = true
				}
				if !showTasks || !tagSelected(tags, want, skip) {
					continue
				}
				name := t.Name
				if name == "" {
					name = t.Module
				}
				if t.RoleName != "" {
					name = t.RoleName + " : " + name
				}
				fmt.Printf("      %s\tTAGS: [%s]\n", name, strings.Join(tags, ", "))
			}
		}
		if showTags {
			all := make([]string, 0, len(union))
			for tg := range union {
				all = append(all, tg)
			}
			sort.Strings(all)
			fmt.Printf("      TASK TAGS: [%s]\n", strings.Join(all, ", "))
		}
	}
}

func effectiveTags(play *playbook.Play, t *playbook.Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, tg := range append(append([]string{}, play.Tags...), t.Tags...) {
		if !seen[tg] {
			seen[tg] = true
			out = append(out, tg)
		}
	}
	sort.Strings(out)
	return out
}

// tagSelected applies --tags/--skip-tags with always/never semantics.
func tagSelected(tags, want, skip []string) bool {
	has := func(x string) bool {
		for _, t := range tags {
			if t == x {
				return true
			}
		}
		return false
	}
	for _, s := range skip {
		if has(s) || (s == "all" && len(tags) > 0) {
			return false
		}
	}
	wanted := func() bool {
		for _, w := range want {
			switch {
			case w == "all" && !has("never"):
				return true
			case w == "tagged" && len(tags) > 0:
				return true
			case w == "untagged" && len(tags) == 0:
				return true
			case has(w):
				return true
			}
		}
		return false
	}
	if has("never") {
		return len(want) > 0 && wanted()
	}
	if len(want) == 0 || has("always") {
		return true
	}
	return wanted()
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
		Poll:    -1,
		Src:     playbook.Pos{File: "<adhoc>", Line: 1},
	}
	if p.moduleArgs != "" {
		tmp := &playbook.Task{Module: module, LoopVar: "item"}
		if err := adhocArgs(tmp, module, p.moduleArgs); err != nil {
			printError(err)
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
		// Unset, as the playbook parser leaves it (0 would mean "abort on
		// any failure").
		MaxFailPercentage: -1,
		Tasks:             []*playbook.Task{task},
		Src:               playbook.Pos{File: "<adhoc>", Line: 1},
	}

	secrets, err := setupVault(p)
	if err != nil {
		printError(err)
		return 1
	}
	inv, err := loadInventory(p, "")
	if err != nil {
		printError(err)
		return 1
	}
	opts, err := buildOptions(p, ".", secrets)
	if err != nil {
		printError(err)
		return 1
	}
	cb, err := buildCallback(p.verbosity, true, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
		return 1
	}
	if m, ok := cb.(*callback.Minimal); ok {
		m.ArgOrder = argKeyOrder(p.moduleArgs)
	}
	runner := executor.NewRunner(inv, cb, opts)
	runner.Limit = p.limit
	code, err := runner.Run(context.Background(), []*playbook.Play{play})
	if err != nil {
		printError(err)
		return 1
	}
	return code
}

// argKeyOrder lists the keys of a k=v argument string in the order given.
func argKeyOrder(raw string) []string {
	var keys []string
	for _, tok := range strings.Fields(raw) {
		if k, _, ok := strings.Cut(tok, "="); ok && k != "" && !strings.ContainsAny(k, "'\"") {
			keys = append(keys, k)
		}
	}
	return keys
}

// readPasswordFile reads a password file's first line; an executable file
// is run and its output used, as Ansible does for password files.
func readPasswordFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	var data []byte
	if info.Mode()&0o111 != 0 {
		data, err = exec.Command(path).Output()
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("reading password file %s: %v", path, err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimRight(line, "\r"), nil
}

// expandImports inlines import_role tasks (static, so listed like the
// role's own tasks), inheriting the import's tags.
func expandImports(play *playbook.Play, tasks []*playbook.Task) []*playbook.Task {
	var out []*playbook.Task
	for _, t := range tasks {
		name, _ := t.Args["name"].(string)
		if t.Module != "import_role" || name == "" {
			out = append(out, t)
			continue
		}
		from, _ := t.Args["tasks_from"].(string)
		var rolesPath []string
		if cfg, err := config.Load(); err == nil {
			rolesPath = cfg.RolesPath
		}
		ri, err := playbook.LoadRoleForInclude(name, play.Dir, rolesPath, from)
		if err != nil {
			out = append(out, t)
			continue
		}
		for _, rt := range ri.Tasks {
			rt.Tags = append(append([]string{}, t.Tags...), rt.Tags...)
			if rt.RoleName == "" {
				rt.RoleName = name
			}
		}
		out = append(out, expandImports(play, ri.Tasks)...)
	}
	return out
}

// buildCallback loads the configured stdout and aggregate callbacks.
func buildCallback(verbosity int, adhoc bool, playbookDir string) (executor.Callback, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	s := callback.Settings{
		StdoutCallback:      cfg.StdoutCallback,
		CallbacksEnabled:    cfg.CallbacksEnabled,
		DisplayOkHosts:      cfg.DisplayOkHosts,
		DisplaySkippedHosts: cfg.DisplaySkippedHosts,
		Verbosity:           verbosity,
		Adhoc:               adhoc,
		PluginDirs:          callback.PluginDirs(cfg.CallbackPlugins, playbookDir),
	}
	if adhoc {
		// The ad-hoc command uses minimal and ignores stdout_callback unless
		// bin_ansible_callbacks is set; aggregate callbacks do not load.
		s.StdoutCallback, s.CallbacksEnabled = "", nil
	}
	return callback.Build(s, func(msg string) { fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg) })
}

// loadErrorCode is ansible-playbook's exit status for a playbook that
// fails to load: 4 for a parser error, 1 for other errors (a missing file
// or role).
func loadErrorCode(err error) int {
	var ce interface{ ExitCode() int }
	if errors.As(err, &ce) {
		return ce.ExitCode()
	}
	return 4
}

// printError prints a fatal error as ansible-core's Display.error does:
// "[ERROR]: <message>", then the Origin and the source excerpt when the
// error points into a file.
func printError(err error) {
	var oe playbook.OriginError
	if errors.As(err, &oe) {
		if file, line, col := oe.Origin(); line > 0 {
			fmt.Fprintf(os.Stderr, "[ERROR]: %s\nOrigin: %s:%d:%d\n\n%s\n", oe.Message(), file, line, col,
				template.SourceExcerpt(file, line, col))
			return
		}
		fmt.Fprintf(os.Stderr, "[ERROR]: %s\n", oe.Message())
		return
	}
	fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
}
