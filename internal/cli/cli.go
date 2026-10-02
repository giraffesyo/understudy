// Package cli implements argument parsing and the playbook/adhoc
// subcommands. Flag parsing is hand-rolled: stdlib flag cannot count -vvv,
// collect repeated -e, or intersperse positionals.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/factcache"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vault"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// version is stamped at build time by the Makefile and GoReleaser
// (.goreleaser.yaml): -ldflags "-X github.com/giraffesyo/understudy/internal/cli.version=v1.2.3".
// Unstamped builds fall back to the module version `go install` records.
var version = "0.1.0-dev"

// connWarned holds the connection warnings already shown.
var connWarned sync.Map

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
		return withConfig(playbookCmd, args)
	case "ansible":
		return withConfig(adhocCmd, args)
	}
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "playbook":
		return withConfig(playbookCmd, args[1:])
	case "adhoc":
		return withConfig(adhocCmd, args[1:])
	case "vault":
		return withConfig(vaultCmd, args[1:])
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

// withConfig runs a command once the configuration loads, as ansible's
// commands load their constants first: a configuration error ends the
// command (exit 5) before its command line is even parsed, and the
// configuration's warnings come first.
func withConfig(cmd func([]string) int, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		// ansible-core prints the exception's traceback after the message.
		fmt.Fprintf(os.Stderr, "ERROR: %s\n\n", err)
		var ce interface{ ExitCode() int }
		if errors.As(err, &ce) {
			return ce.ExitCode()
		}
		return 5
	}
	for _, w := range cfg.Warnings {
		warnOnce(w + "\n")
	}
	return cmd(args)
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
	inventory []string
	limit     string
	extraVars map[string]any
	// extraVarArgs are the -e options, loaded with the inventory.
	extraVarArgs    []string
	extraVarsLoaded bool
	// extraVarsErr is the first -e option that could not be loaded: as
	// ansible-core loads extra vars, it fails the inventory sources'
	// parsing and then the run.
	extraVarsErr error
	// extraVarOrigins are where -e named reserved variables.
	extraVarOrigins []template.KeyOrigin
	// extraVarValues are where each -e variable's value came from.
	extraVarValues map[string]template.Position
	forks          int
	verbosity      int
	check          bool
	diff           bool
	become         bool
	becomeUser     string
	askBecome      bool
	askPass        bool
	askVault       bool
	vaultFiles     []string
	remoteUser     string
	privateKey     string
	connection     string
	tags           string
	skipTags       string
	syntax         bool
	listHosts      bool
	listTasks      bool
	module         string // adhoc -m
	moduleArgs     string // adhoc -a
	positional     []string

	becomeMethod   string
	becomePassFile string
	connPassFile   string
	timeout        int
	forceHandlers  bool
	flushCache     bool
	startAtTask    string
	step           bool
	listTags       bool
	sshArgs        []string // --ssh-common-args & co: not applicable to the native client
	prog           string   // the command ansible-core's parser names (-vv version banner)
	forksSet       bool     // -f given (0 is then an error)

	// ad-hoc only: -P, -B, --playbook-dir, --task-timeout.
	poll           int
	pollSet        bool
	background     int
	playbookDir    string
	taskTimeout    int
	taskTimeoutSet bool
	oneLine        bool   // -o: the oneline callback
	tree           string // -t: the tree callback's directory
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
	{[]string{"-e", "--extra-vars"}, true, func(p *parsedArgs, v string) error {
		p.extraVarArgs = append(p.extraVarArgs, v)
		return nil
	}},
	{[]string{"-f", "--forks"}, true, func(p *parsedArgs, v string) error { p.forks, _ = pyInt(v); p.forksSet = true; return nil }},
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
	{[]string{"-T", "--timeout"}, true, func(p *parsedArgs, v string) error { p.timeout, _ = pyInt(v); return nil }},
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
	{[]string{"--flush-cache"}, false, boolFlag(func(p *parsedArgs) { p.flushCache = true })},
	// No Python module path: accepted, nothing to do.
	{[]string{"-M", "--module-path"}, true, func(*parsedArgs, string) error { return nil }},
	{[]string{"-m", "--module-name"}, true, func(p *parsedArgs, v string) error { p.module = v; return nil }},
	{[]string{"-a", "--args"}, true, func(p *parsedArgs, v string) error { p.moduleArgs = v; return nil }},
}

func joinCSV(prev, v string) string {
	if prev == "" {
		return v
	}
	return prev + "," + v
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

// announceConfig is CLI.run's opening: at -vv the version banner
// (display.vv of --version, naming understudy in place of ansible-core's
// Python details), at -v the configuration source.
func announceConfig(p *parsedArgs) {
	if p.verbosity == 0 {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	configFile := ""
	if cfg.Source != "" {
		configFile, _ = filepath.Abs(cfg.Source)
	}
	if p.verbosity > 1 {
		shown := configFile
		if shown == "" {
			shown = "None"
		}
		exe, _ := os.Executable()
		displayVerbose(fmt.Sprintf("%s [understudy %s]\n  config file = %s\n  executable location = %s", p.prog, version, shown, exe))
	}
	if configFile == "" {
		displayVerbose("No config file found; using defaults")
	} else {
		displayVerbose(fmt.Sprintf("Using %s as config file", configFile))
	}
}

// buildOptions assembles executor options from CLI flags layered over
// ansible.cfg (CLI > env > cfg > defaults), running -k/-K prompts once.
func buildOptions(p *parsedArgs, baseDir string, secrets *vault.Secrets) (executor.Options, error) {
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
		ForceHandlers:   p.forceHandlers,
		FlushCache:      p.flushCache,
		StartAtTask:     p.startAtTask,
		Step:            p.step,
		Forks:           forks,
		CheckMode:       p.check,
		Diff:            p.diff,
		Verbosity:       p.verbosity,
		ExtraVars:       p.extraVars,
		ExtraVarOrigins: p.extraVarOrigins,
		ExtraVarValues:  p.extraVarValues,
		Become:          p.become,
		BecomeUser:      p.becomeUser,
		BecomeMethod:    becomeMethod,
		Connection:      p.connection,
		BaseDir:         baseDir,
		RolesPath:       cfg.RolesPath,
		Inventory:       p.inventory,
		RefreshInventory: func() (*inventory.Inventory, error) {
			return loadInventory(p, baseDir)
		},
		Tags:     splitCSV(p.tags),
		SkipTags: splitCSV(p.skipTags),

		NoColor:                 callback.NoColor(),
		NoDeprecationWarnings:   !cfg.DeprecationWarnings,
		InjectFactsSet:          cfg.InjectFactsSet,
		AllowBrokenConditionals: cfg.AllowBrokenConditionals,
		TaskTimeout:             cfg.TaskTimeout,
		FactCache:               factCacheSettings(cfg),
		Gathering:               cfg.Gathering,
		PluginOption:            cfg.PluginOption,
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
		Pipelining:      configPipelining(cfg),
		Shell: connection.ShellOptions{
			AdminUsers: cfg.AdminUsers, SystemTmpdirs: cfg.SystemTmpdirs,
			CommonRemoteGroup: cfg.CommonRemoteGroup, WorldReadableTemp: cfg.WorldReadableTemp,
		},
		SSHArgs: p.sshArgs,
		Warn: func(msg string) {
			// Display.warning shows each distinct warning once.
			if _, seen := connWarned.LoadOrStore(msg, true); !seen {
				fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg)
			}
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
		if errors.Is(err, fs.ErrNotExist) {
			// get_file_vault_secret's error, warned about as the default
			// vault id's secret and then raised.
			abs, _ := filepath.Abs(f)
			msg := fmt.Sprintf("The vault password file %s was not found", abs)
			warnOnce("Error getting vault password file (default): " + msg + "\n")
			return nil, errors.New(msg)
		}
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

// loadExtraVars is load_extra_vars, once: the -e options in order, up to
// the first that fails.
func (p *parsedArgs) loadExtraVars() {
	if p.extraVarsLoaded {
		return
	}
	p.extraVarsLoaded = true
	p.extraVarValues = map[string]template.Position{}
	for _, v := range p.extraVarArgs {
		if p.extraVarsErr = parseExtraVars(v, p.extraVars, &p.extraVarOrigins, p.extraVarValues); p.extraVarsErr != nil {
			return
		}
	}
}

// cliExtraVarsOrigin is the origin ansible-core gives -e text.
const cliExtraVarsOrigin = "<CLI option '-e'>"

// extraVarsError is an error loading the extra vars: the event every
// inventory source fails with, and ansible-core's exit status for its
// exception (4 for an AnsibleParserError, 5 for an AnsibleOptionsError,
// 1 for a missing file).
type extraVarsError struct {
	event error
	code  int
}

func (e *extraVarsError) Error() string { return e.event.Error() }

// Formatted is the error as Display.error shows it.
func (e *extraVarsError) Formatted() string { return "[ERROR]: " + inventory.FormatEvent(e.event) }

// ExitCode is ansible-core's exit status for the error.
func (e *extraVarsError) ExitCode() int { return e.code }

// extraVarsEvent is the event an extra vars error fails the inventory
// sources with (nil for none).
func extraVarsEvent(err error) error {
	if e, ok := err.(*extraVarsError); ok {
		return e.event
	}
	return err
}

// parseExtraVars is one -e option as load_extra_vars reads it: @file (a
// YAML or JSON file), YAML or JSON text starting with [ or {, or
// key=value words (parse_kv); a path not given as @file is refused.
func parseExtraVars(s string, into map[string]any, origins *[]template.KeyOrigin, values map[string]template.Position) error {
	if s == "" {
		return nil
	}
	optionsErr := func(msg string) error {
		return &extraVarsError{inventory.EventError(msg, "", ""), 5}
	}
	if strings.HasPrefix(s, "@") {
		// The option's type, maybe_unfrack_path('@').
		s = "@" + unfrackPath(s[1:])
	}
	notMapping := func() error {
		return optionsErr(fmt.Sprintf("Invalid extra vars data supplied. '%s' could not be made into a dictionary", s))
	}
	// load is DataLoader.load: Python's json first (ints stay ints, keys
	// keep their order; the values carry just the source as their origin
	// and the keys none), then YAML.
	load := func(data []byte, name string) error {
		if jv, err := omap.UnmarshalJSON(data); err == nil {
			om, ok := jv.(*omap.OMap)
			if !ok {
				return notMapping()
			}
			for _, k := range om.Keys() {
				into[k] = om.Get(k)
				values[k] = template.Position{File: name}
				if template.IsReservedName(k) {
					*origins = append(*origins, template.KeyOrigin{Name: k})
				}
			}
			return nil
		}
		v, err := yaml.Unmarshal(data, name)
		if err != nil {
			var ye *yaml.Error
			if name == cliExtraVarsOrigin && errors.As(err, &ye) {
				// The text has no file to show or analyze it from.
				ctx := "Origin: " + name
				if ye.Line > 0 {
					ctx = fmt.Sprintf("Origin: %s:%d:%d\n\n(source not shown: TypeError)", name, ye.Line, ye.Col)
				}
				return &extraVarsError{inventory.EventError(ye.UnanalyzedMessage(), ctx, ""), 4}
			}
			return &extraVarsError{inventory.AsEvent(err), 4}
		}
		m, ok := yaml.PlainMap(v)
		if !ok {
			return notMapping()
		}
		if node, err := yaml.ParseSingle(data, name); err == nil {
			*origins = append(*origins, playbook.ReservedKeyOrigins(node, name)...)
		}
		for k, val := range m {
			into[k] = val
			if file, line, col, ok := yaml.ChildOrigin(m, k); ok {
				values[k] = template.Position{File: file, Line: line, Col: col}
			} else {
				values[k] = template.Position{File: name}
			}
		}
		return nil
	}
	switch {
	case strings.HasPrefix(s, "@"):
		path := s[1:]
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return &extraVarsError{inventory.FileNotFoundError(path), 1}
			}
			return &extraVarsError{inventory.EventError(fmt.Sprintf("An error occurred while trying to read the file '%s': %s", path, pyOSError(err, path)), "", ""), 4}
		}
		return load(data, path)
	case s[0] == '/' || s[0] == '.':
		return optionsErr(fmt.Sprintf("Please prepend extra_vars filename '%s' with '@'", s))
	case s[0] == '[' || s[0] == '{':
		return load([]byte(s), cliExtraVarsOrigin)
	default:
		keys, kv, err := playbook.ParseKV(s)
		if err != nil {
			return &extraVarsError{inventory.EventError(err.Error(), "", ""), 4}
		}
		for _, k := range keys {
			into[k] = kv[k]
			values[k] = template.Position{File: cliExtraVarsOrigin}
			if template.IsReservedName(k) {
				*origins = append(*origins, template.KeyOrigin{Name: k, Label: cliExtraVarsOrigin})
			}
		}
		return nil
	}
}

// pyOSError is how Python words an OSError reading path ("[Errno 21] Is
// a directory: '<path>'").
func pyOSError(err error, path string) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := map[syscall.Errno]string{
			syscall.EISDIR: "Is a directory", syscall.EACCES: "Permission denied",
			syscall.ENOTDIR: "Not a directory", syscall.ELOOP: "Too many levels of symbolic links",
		}[errno]
		if text == "" {
			text = errno.Error()
		}
		return fmt.Sprintf("[Errno %d] %s: '%s'", int(errno), text, path)
	}
	return err.Error()
}

// loadInventory builds the inventory from -i sources (falling back to
// ansible.cfg's inventory setting, then /etc/ansible/hosts), applying
// group_vars/host_vars adjacent to sources and to the playbook directory.
// Its warnings print as ansible-core's Display prints them.
func loadInventory(p *parsedArgs, playbookDir string) (*inventory.Inventory, error) {
	p.loadExtraVars()
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	sources := p.inventory
	if len(sources) == 0 {
		sources = cfg.Inventory
		if len(sources) == 0 {
			sources = []string{inventory.DefaultSource}
		}
	}
	var varsDirs []string
	if playbookDir != "" {
		varsDirs = append(varsDirs, playbookDir)
	}
	inv, err := inventory.LoadWith(sources, inventory.Options{
		VarsDirs:            varsDirs,
		Enabled:             cfg.InventoryEnabled,
		IgnoreExts:          cfg.InventoryIgnoreExts,
		IgnorePatterns:      cfg.InventoryIgnorePatterns,
		UnparsedWarning:     cfg.InventoryUnparsedWarning,
		UnparsedIsFailed:    cfg.InventoryUnparsedIsFailed,
		AnyUnparsedIsFailed: cfg.InventoryAnyUnparsedIsFailed,
		ExtraVarsErr:        extraVarsEvent(p.extraVarsErr),
		TransformGroupChars: cfg.TransformInvalidGroupChars,
		Warn:                warnOnce,
		Verbose: func(level int, msg string) {
			if p.verbosity >= level {
				displayVerbose(msg)
			}
		},
		ExtraVars:    p.extraVars,
		FactCache:    factCacheSettings(cfg),
		PluginOption: cfg.PluginOption,
	})
	if err != nil {
		return nil, err
	}
	if p.extraVarsErr != nil {
		// The variable manager loads them again, for good.
		return nil, p.extraVarsErr
	}
	inv.PatternMismatch = cfg.HostPatternMismatch
	inv.TransformGroupChars = cfg.TransformInvalidGroupChars
	return inv, nil
}

// configPipelining is the pipelining the configuration and environment
// set (ANSIBLE_PIPELINING, ANSIBLE_SSH_PIPELINING, the defaults,
// connection and ssh_connection sections).
func configPipelining(cfg *config.Config) bool {
	v, _, ok := cfg.PluginOption([]string{"ANSIBLE_PIPELINING", "ANSIBLE_SSH_PIPELINING"},
		[]string{"defaults.pipelining", "connection.pipelining", "ssh_connection.pipelining"})
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes", "on", "1", "true", "t", "1.0":
		return true
	}
	return false
}

// factCacheSettings is the fact cache's configuration.
func factCacheSettings(cfg *config.Config) factcache.Settings {
	return factcache.Settings{Plugin: cfg.FactCaching, URI: cfg.FactCachingConnection,
		Prefix: cfg.FactCachingPrefix, Timeout: cfg.FactCachingTimeout}
}

// checkHostList is CLI.get_host_list: an inventory with no hosts warns
// that only the implicit localhost is left, and a --limit leaving no
// hosts of a non-empty inventory is an error.
func checkHostList(inv *inventory.Inventory, limit, pattern string) error {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	noHosts := false
	if len(inv.ListHosts()) == 0 {
		if cfg.LocalhostWarning && pattern != "localhost" && pattern != "127.0.0.1" && pattern != "::1" {
			warnOnce("provided hosts list is empty, only localhost is available. Note that the implicit localhost does not match 'all'\n")
		}
		noHosts = true
	}
	hosts, err := inv.Match(pattern)
	if err != nil {
		return err
	}
	if limit != "" {
		limited, err := inv.Match(limit)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, h := range limited {
			keep[h.Name] = true
		}
		var out []*inventory.Host
		for _, h := range hosts {
			if keep[h.Name] {
				out = append(out, h)
			}
		}
		hosts = out
	}
	if len(hosts) == 0 && !noHosts {
		return errors.New("Specified inventory, host pattern and/or --limit leaves us with no hosts to target.")
	}
	return nil
}

var (
	warnMu    sync.Mutex
	warnShown = map[string]bool{}
)

// warnOnce prints a warning as Display.warning does: "[WARNING]: " and the
// formatted message, each distinct message once.
func warnOnce(msg string) {
	warnMu.Lock()
	defer warnMu.Unlock()
	if warnShown[msg] {
		return
	}
	warnShown[msg] = true
	fmt.Fprint(os.Stderr, "[WARNING]: "+msg)
}

func playbookCmd(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	p, code, done := parseCommand(playbookParser(cfg), cfg, args)
	if done {
		return code
	}
	announceConfig(p)

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
		configureYAML(cfg)
	}
	for _, path := range p.positional {
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR]: the playbook: %s could not be found\n", path)
			return 1
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeNamedPipe == 0 {
			fmt.Fprintf(os.Stderr, "[ERROR]: the playbook: %s does not appear to be a file\n", path)
			return 1
		}
	}
	// As ansible-playbook, vault secrets and the inventory load before
	// the playbooks, whatever the mode.
	secrets, err := setupVault(p)
	if err != nil {
		printError(err)
		return 1
	}
	inv, err := loadInventory(p, filepath.Dir(p.positional[0]))
	if err != nil {
		printError(err)
		var ce interface{ ExitCode() int }
		if errors.As(err, &ce) {
			return ce.ExitCode()
		}
		return 1
	}
	if err := checkHostList(inv, p.limit, "all"); err != nil {
		printError(err)
		return 1
	}
	for _, path := range p.positional {
		// Load by absolute path: error origins show it, as in Ansible.
		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = path
		}
		plays, loaded, err := playbook.LoadFileTasks(absPath)
		if err != nil {
			// The tasks loaded before the error report their
			// deprecations as they load.
			if cfg, cerr := config.Load(); cerr != nil || cfg.DeprecationWarnings {
				for _, w := range executor.TaskDeprecationWarnings(loaded) {
					fmt.Fprint(os.Stderr, w)
				}
			}
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

	if p.verbosity > 1 && (p.syntax || p.listTasks || p.listTags || p.listHosts) {
		// PlaybookExecutor loads each playbook as a run would, before
		// the listing.
		for _, b := range books {
			for _, pl := range b.plays {
				for _, n := range pl.LoadNotes {
					displayVerbose(n)
				}
			}
			displayVerbose(fmt.Sprintf("%d plays in %s", len(b.plays), b.path))
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
	var paths []string
	for _, b := range books {
		all = append(all, b.plays)
		paths = append(paths, b.path)
	}
	cb, cbNotes, err := buildCallback(p.verbosity, false, filepath.Dir(p.positional[0]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
		return 1
	}
	runner := executor.NewRunner(inv, cb, opts)
	runner.Limit = p.limit
	runner.BookPaths = paths
	runner.CallbackNotes = cbNotes
	code, err = runner.RunPlaybooks(context.Background(), all)
	if err != nil {
		printError(err)
		var ye *yaml.Error
		if errors.As(err, &ye) {
			return 4 // a parser error, as at load
		}
		var ce interface{ ExitCode() int }
		if errors.As(err, &ce) {
			return ce.ExitCode()
		}
		return 1
	}
	return code
}

// playHeader is the list modes' "play #N (pattern): name\tTAGS: [...]".
func playHeader(i int, play *playbook.Play) string {
	name := play.Name
	if strings.TrimSpace(name) == "" {
		name = play.HostPattern // Play.get_name: the hosts, unnamed
	}
	return fmt.Sprintf("play #%d (%s): %s\tTAGS: [%s]", i+1, play.HostPattern, name, strings.Join(play.Tags, ", "))
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
				if t.Implicit {
					continue // a role's role_complete marker
				}
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
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	p, code, done := parseCommand(adhocParser(cfg), cfg, args)
	if done {
		return code
	}
	announceConfig(p)
	configureYAML(cfg)
	module := p.module
	if module == "" {
		module = cfg.ModuleName
	}
	task := &playbook.Task{
		Name:    module,
		Module:  module,
		LoopVar: "item",
		Poll:    -1,
		Src:     playbook.Pos{File: "<adhoc>", Line: 1},
	}
	poll := cfg.PollInterval
	if p.pollSet {
		poll = p.poll
	}
	if p.background > 0 {
		task.Async, task.Poll = p.background, poll
	}
	taskTimeout := cfg.TaskTimeout
	if p.taskTimeoutSet {
		taskTimeout = p.taskTimeout
	}
	if taskTimeout != 0 {
		task.Timeout = taskTimeout
	}
	baseDir := "."
	if p.playbookDir != "" {
		baseDir = p.playbookDir
	}
	pattern := p.positional[0]
	gather := false
	play := &playbook.Play{
		Name:        "understudy Ad-Hoc",
		HostPattern: pattern,
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
	inv, err := loadInventory(p, p.playbookDir)
	if err != nil {
		printError(err)
		var ce interface{ ExitCode() int }
		if errors.As(err, &ce) {
			return ce.ExitCode()
		}
		return 1
	}
	// AdHocCLI.run: the hosts (none matching is a warning, unless --limit
	// left none), --list-hosts, then the checks of the module.
	var hosts []*inventory.Host
	if err := checkHostList(inv, p.limit, pattern); err != nil {
		if p.limit != "" {
			printError(err)
			return 1
		}
		warnOnce("No hosts matched, nothing to do\n")
	} else if hosts, err = limitedHosts(inv, pattern, p.limit); err != nil {
		printError(err)
		return 1
	}
	if p.listHosts {
		fmt.Printf("  hosts (%d):\n", len(hosts))
		for _, h := range hosts {
			fmt.Printf("    %s\n", h.Name)
		}
		return 0
	}
	if requireArgsModules[module] && p.moduleArgs == "" {
		msg := fmt.Sprintf("No argument passed to %s module", module)
		if strings.HasSuffix(pattern, ".yml") {
			msg += " (did you mean to run ansible-playbook?)"
		}
		printError(errors.New(msg))
		return 5
	}
	switch module {
	case "import_playbook", "ansible.builtin.import_playbook", "ansible.legacy.import_playbook":
		printError(fmt.Errorf("'%s' is not a valid action for ad-hoc commands", module))
		return 5
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
	opts, err := buildOptions(p, baseDir, secrets)
	if err != nil {
		printError(err)
		return 1
	}
	cb, cbNotes, err := buildCallback(p.verbosity, true, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
		return 1
	}
	if m, ok := cb.(*callback.Minimal); ok {
		m.ArgOrder = argKeyOrder(p.moduleArgs)
		m.TaskTimeout, m.Async, m.Poll = taskTimeout, p.background, poll
		if p.oneLine {
			m.OneLine = true
			if cfg.DeprecationWarnings {
				showDeprecation(callbackDeprecation("oneline"))
			}
		}
	}
	if p.tree != "" {
		if cfg.DeprecationWarnings {
			showDeprecation(callbackDeprecation("tree"))
		}
		cb = callback.WithExtras(cb, callback.NewTree(unfrackPath(p.tree), p.verbosity, func(msg string) {
			fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg)
		}))
	}
	runner := executor.NewRunner(inv, cb, opts)
	runner.Limit = p.limit
	runner.CallbackNotes = cbNotes
	code, err = runner.Run(context.Background(), []*playbook.Play{play})
	if err != nil {
		printError(err)
		return 1
	}
	return code
}

// requireArgsModules are C.MODULE_REQUIRE_ARGS: the modules ad-hoc runs
// only with -a.
var requireArgsModules = map[string]bool{
	"command": true, "raw": true, "script": true, "shell": true, "win_command": true, "win_shell": true,
	"ansible.builtin.command": true, "ansible.builtin.raw": true, "ansible.builtin.script": true,
	"ansible.builtin.shell": true, "ansible.builtin.win_command": true, "ansible.builtin.win_shell": true,
	"ansible.legacy.command": true, "ansible.legacy.raw": true, "ansible.legacy.script": true,
	"ansible.legacy.shell": true, "ansible.legacy.win_command": true, "ansible.legacy.win_shell": true,
	"ansible.windows.win_command": true, "ansible.windows.win_shell": true,
}

// limitedHosts is inventory.list_hosts(pattern) under --limit.
func limitedHosts(inv *inventory.Inventory, pattern, limit string) ([]*inventory.Host, error) {
	hosts, err := inv.Match(pattern)
	if err != nil || limit == "" {
		return hosts, err
	}
	limited, err := inv.Match(limit)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, h := range limited {
		keep[h.Name] = true
	}
	var out []*inventory.Host
	for _, h := range hosts {
		if keep[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
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
		ri, err := playbook.LoadRoleForInclude(name, play.Dir, rolesPath, playbook.RoleIncludeOptions{TasksFrom: from})
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
func buildCallback(verbosity int, adhoc bool, playbookDir string) (executor.Callback, []string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	s := callback.Settings{
		StdoutCallback:      cfg.StdoutCallback,
		CallbacksEnabled:    cfg.CallbacksEnabled,
		DisplayOkHosts:      cfg.DisplayOkHosts,
		DisplaySkippedHosts: cfg.DisplaySkippedHosts,
		ShowCustomStats:     cfg.ShowCustomStats,
		Verbosity:           verbosity,
		Adhoc:               adhoc,
		PluginDirs:          callback.PluginDirs(cfg.CallbackPlugins, playbookDir),
	}
	if adhoc {
		// The ad-hoc command uses minimal and ignores stdout_callback unless
		// bin_ansible_callbacks is set; aggregate callbacks do not load.
		s.StdoutCallback, s.CallbacksEnabled = "", nil
	}
	cb, err := callback.Build(s, func(msg string) { fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg) })
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	if verbosity > 1 {
		// TaskQueueManager.load_callbacks passes over ansible-core's other
		// stdout callbacks.
		stdout := strings.TrimPrefix(s.StdoutCallback, "ansible.builtin.")
		switch {
		case stdout == "" && adhoc:
			stdout = "minimal"
		case stdout == "":
			stdout = "default"
		}
		for _, name := range []string{"default", "minimal", "oneline"} {
			if name != stdout {
				notes = append(notes, fmt.Sprintf("Skipping callback '%s', as we already have a stdout callback.", name))
			}
		}
	}
	return cb, notes, nil
}

// displayVerbose is Display.verbose: msg on stdout in COLOR_VERBOSE
// (stringc colors each line).
func displayVerbose(msg string) {
	if !callback.NoColor() {
		lines := strings.Split(msg, "\n")
		for i, l := range lines {
			lines[i] = "\x1b[0;34m" + l + "\x1b[0m"
		}
		msg = strings.Join(lines, "\n")
	}
	fmt.Println(msg)
}

// configureYAML applies the YAML loader settings and shows its warnings
// (duplicate mapping keys) as ansible-core's Display.warning does: the
// message, the origin with the source excerpt, and the help text; an
// identical warning shows once.
func configureYAML(cfg *config.Config) {
	yaml.DuplicateKeyMode = cfg.DuplicateDictKey
	var mu sync.Mutex
	shown := map[string]bool{}
	yaml.OnWarning = func(w yaml.Warning) {
		var msg string
		switch {
		case w.Line > 0:
			excerpt := template.SourceExcerpt(w.File, w.Line, w.Col)
			if excerpt == "" {
				// No source to show (-e YAML text): the key stands in.
				excerpt = w.Key + "\n"
			}
			msg = fmt.Sprintf("[WARNING]: %s\nOrigin: %s:%d:%d\n\n%s\n%s\n\n", w.Msg, w.File, w.Line, w.Col, excerpt, w.Help)
		case w.Value != "":
			// A key with no origin (a bool): its value stands in for the source.
			msg = fmt.Sprintf("[WARNING]: %s\nOrigin: <unknown>\n\n%s\n\n%s\n\n", w.Msg, w.Value, w.Help)
		default:
			msg = fmt.Sprintf("[WARNING]: %s %s\n", w.Msg, w.Help)
		}
		mu.Lock()
		defer mu.Unlock()
		if !shown[msg] {
			shown[msg] = true
			fmt.Fprint(os.Stderr, msg)
		}
	}
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
	var fe interface{ Formatted() string }
	if errors.As(err, &fe) {
		if f := fe.Formatted(); f != "" {
			fmt.Fprint(os.Stderr, f)
			return
		}
	}
	var oe playbook.OriginError
	if errors.As(err, &oe) {
		help := ""
		if h, ok := oe.(interface{ HelpText() string }); ok && h.HelpText() != "" {
			help = strings.TrimRight(h.HelpText(), "\n") + "\n\n"
		}
		if file, line, col := oe.Origin(); line > 0 {
			fmt.Fprintf(os.Stderr, "[ERROR]: %s\nOrigin: %s:%d:%d\n\n%s\n%s", oe.Message(), file, line, col,
				template.SourceExcerpt(file, line, col), help)
			return
		} else if h, ok := oe.(interface{ HasOrigin() bool }); ok && h.HasOrigin() {
			// An error that names its file but no position in it.
			fmt.Fprintf(os.Stderr, "[ERROR]: %s\nOrigin: %s\n\n", oe.Message(), file)
			return
		}
		fmt.Fprintf(os.Stderr, "[ERROR]: %s\n", oe.Message())
		return
	}
	fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
}
