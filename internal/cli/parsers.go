package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/executor"
)

// The ansible-playbook and ansible command lines, as ansible-core's
// cli/arguments/option_helpers.py builds them: the same options, in the
// same order and groups, with the same help (whose defaults come from the
// configuration).

// value wraps a single-value apply function as an action's.
func value(f func(p *parsedArgs, v string) error) func(*parsedArgs, []string, string) error {
	return func(p *parsedArgs, vs []string, _ string) error { return f(p, vs[0]) }
}

func flag(f func(p *parsedArgs)) func(*parsedArgs, []string, string) error {
	return func(p *parsedArgs, _ []string, _ string) error { f(p); return nil }
}

// applyOf is the existing flag's apply function for an option string.
func applyOf(name string) func(*parsedArgs, []string, string) error {
	for i := range cliFlags {
		for _, n := range cliFlags[i].names {
			if n == name {
				f := cliFlags[i].apply
				return func(p *parsedArgs, vs []string, _ string) error {
					v := ""
					if len(vs) > 0 {
						v = vs[0]
					}
					return f(p, v)
				}
			}
		}
	}
	panic("no flag " + name)
}

const versionHelp = "show program's version number, config file location, configured module search path, module location, executable location and exit"

// baseParser is create_base_parser: --version, -h and -v.
func baseParser(prog, desc, epilog string) *argParser {
	ap := newArgParser(prog, desc, epilog)
	ap.add(nil, &argAction{opts: []string{"-h", "--help"}, dest: "help", help: "show this help message and exit",
		exits: func(ap *argParser) int { fmt.Print(ap.formatHelp()); return 0 }})
	ap.add(nil, &argAction{opts: []string{"--version"}, dest: "version", help: versionHelp,
		exits: func(ap *argParser) int { fmt.Println(versionText(ap.prog)); return 0 }})
	ap.add(nil, &argAction{opts: []string{"-v", "--verbose"}, dest: "verbosity", help: "Causes Ansible to print more debug messages. Adding multiple -v will increase the verbosity, the builtin plugins currently evaluate up to -vvvvvv. A reasonable level to start is -vvv, connection debugging might require -vvvv. This argument may be specified multiple times.",
		apply: flag(func(p *parsedArgs) { p.verbosity++ })})
	return ap
}

// versionText is --version's report (ansible-core's version(prog)),
// naming understudy in place of ansible-core's Python details.
func versionText(prog string) string {
	configFile := "None"
	if cfg, err := config.Load(); err == nil && cfg.Source != "" {
		configFile = cfg.Source
	}
	exe, _ := os.Executable()
	return fmt.Sprintf("%s [understudy %s]\n  config file = %s\n  executable location = %s", prog, version, configFile, exe)
}

func addConnectOptions(ap *argParser, cfg *config.Config) {
	g := ap.group("Connection Options", "control as whom and how to connect to hosts")
	user := cfg.RemoteUser
	if user == "" {
		user = "None"
	}
	ap.add(g, &argAction{opts: []string{"--private-key", "--key-file"}, dest: "private_key_file", nargs: 1,
		help: "use this file to authenticate the connection", apply: applyOf("--private-key")})
	ap.add(g, &argAction{opts: []string{"-u", "--user"}, dest: "remote_user", nargs: 1,
		help: "connect as this user (default=" + user + ")", apply: applyOf("-u")})
	ap.add(g, &argAction{opts: []string{"-c", "--connection"}, dest: "connection", nargs: 1,
		help: "connection type to use (default=" + cfg.Transport + ")", apply: applyOf("-c")})
	ap.add(g, &argAction{opts: []string{"-T", "--timeout"}, dest: "timeout", nargs: 1, isInt: true,
		help: "override the connection timeout in seconds (default depends on connection)", apply: applyOf("-T")})
	for _, o := range []struct{ name, help string }{
		{"--ssh-common-args", "specify common arguments to pass to sftp/scp/ssh (e.g. ProxyCommand)"},
		{"--sftp-extra-args", "specify extra arguments to pass to sftp only (e.g. -f, -l)"},
		{"--scp-extra-args", "specify extra arguments to pass to scp only (e.g. -l)"},
		{"--ssh-extra-args", "specify extra arguments to pass to ssh only (e.g. -R)"},
	} {
		ap.add(g, &argAction{opts: []string{o.name}, dest: strings.ReplaceAll(strings.TrimPrefix(o.name, "--"), "-", "_"),
			nargs: 1, help: o.help, apply: applyOf(o.name)})
	}
	ap.addMutex(
		&argAction{opts: []string{"-k", "--ask-pass"}, dest: "ask_pass", help: "ask for connection password", apply: applyOf("-k")},
		&argAction{opts: []string{"--connection-password-file", "--conn-pass-file"}, dest: "connection_password_file", nargs: 1,
			help: "Connection password file", apply: applyOf("--connection-password-file")},
	)
}

func addRunasOptions(ap *argParser, cfg *config.Config) {
	g := ap.group("Privilege Escalation Options", "control how and which user you become as on target hosts")
	ap.add(g, &argAction{opts: []string{"-b", "--become"}, dest: "become",
		help: "run operations with become (does not imply password prompting)", apply: applyOf("-b")})
	ap.add(g, &argAction{opts: []string{"--become-method"}, dest: "become_method", nargs: 1,
		help:  "privilege escalation method to use (default=" + cfg.BecomeMethod + "), use `ansible-doc -t become -l` to list valid choices.",
		apply: applyOf("--become-method")})
	ap.add(g, &argAction{opts: []string{"--become-user"}, dest: "become_user", nargs: 1,
		help: "run operations as this user (default=" + cfg.BecomeUser + ")", apply: applyOf("--become-user")})
	ap.addMutex(
		&argAction{opts: []string{"-K", "--ask-become-pass"}, dest: "become_ask_pass", help: "ask for privilege escalation password", apply: applyOf("-K")},
		&argAction{opts: []string{"--become-password-file", "--become-pass-file"}, dest: "become_password_file", nargs: 1,
			help: "Become password file", apply: applyOf("--become-password-file")},
	)
}

func addInventoryOptions(ap *argParser) {
	ap.add(nil, &argAction{opts: []string{"-i", "--inventory", "--inventory-file"}, dest: "inventory", nargs: 1,
		help:  "specify inventory host path or comma separated host list. This argument may be specified multiple times.",
		apply: applyOf("-i"), deprecated: &argDeprecation{"--inventory-file", "2.23", "-i or --inventory"}})
	ap.add(nil, &argAction{opts: []string{"--list-hosts"}, dest: "listhosts",
		help: "outputs a list of matching hosts; does not execute anything else", apply: applyOf("--list-hosts")})
	ap.add(nil, &argAction{opts: []string{"-l", "--limit"}, dest: "subset", nargs: 1,
		help: "further limit selected hosts to an additional pattern", apply: applyOf("-l")})
	ap.add(nil, &argAction{opts: []string{"--flush-cache"}, dest: "flush_cache",
		help: "clear the fact cache for every host in inventory", apply: applyOf("--flush-cache")})
}

func addCheckOptions(ap *argParser) {
	ap.add(nil, &argAction{opts: []string{"-C", "--check"}, dest: "check",
		help: "don't make any changes; instead, try to predict some of the changes that may occur", apply: applyOf("-C")})
	ap.add(nil, &argAction{opts: []string{"-D", "--diff"}, dest: "diff",
		help: "when changing (small) files and templates, show the differences in those files; works great with --check", apply: applyOf("-D")})
}

func addRuntaskOptions(ap *argParser) {
	ap.add(nil, &argAction{opts: []string{"-e", "--extra-vars"}, dest: "extra_vars", nargs: 1,
		help:  "set additional variables as key=value or YAML/JSON, if filename prepend with @. This argument may be specified multiple times.",
		apply: applyOf("-e")})
}

func addVaultOptions(ap *argParser) {
	ap.add(nil, &argAction{opts: []string{"--vault-id"}, dest: "vault_ids", nargs: 1,
		help: "the vault identity to use. This argument may be specified multiple times.", apply: applyOf("--vault-id")})
	ap.addMutex(
		&argAction{opts: []string{"-J", "--ask-vault-password", "--ask-vault-pass"}, dest: "ask_vault_pass",
			help: "ask for vault password", apply: applyOf("-J")},
		&argAction{opts: []string{"--vault-password-file", "--vault-pass-file"}, dest: "vault_password_files", nargs: 1,
			help: "vault password file", apply: applyOf("--vault-password-file")},
	)
}

func addForkOptions(ap *argParser, cfg *config.Config) {
	ap.add(nil, &argAction{opts: []string{"-f", "--forks"}, dest: "forks", nargs: 1, isInt: true,
		help: fmt.Sprintf("specify number of parallel processes to use (default=%d)", cfg.Forks), apply: applyOf("-f")})
}

func addModuleOptions(ap *argParser) {
	ap.add(nil, &argAction{opts: []string{"-M", "--module-path"}, dest: "module_path", nargs: 1,
		help:  `prepend colon-separated path(s) to module library (default={{ ANSIBLE_HOME ~ "/plugins/modules:/usr/share/ansible/plugins/modules" }}). This argument may be specified multiple times.`,
		apply: applyOf("-M")})
}

// playbookParser is PlaybookCLI.init_parser.
func playbookParser(cfg *config.Config) *argParser {
	ap := baseParser("ansible-playbook", "Runs Ansible playbooks, executing the defined tasks on the targeted hosts.", "")
	addConnectOptions(ap, cfg)
	ap.add(nil, &argAction{opts: []string{"--force-handlers"}, dest: "force_handlers",
		help: "run handlers even if a task fails", apply: applyOf("--force-handlers")})
	addRunasOptions(ap, cfg)
	ap.add(nil, &argAction{opts: []string{"-t", "--tags"}, dest: "tags", nargs: 1,
		help: "only run plays and tasks tagged with these values. This argument may be specified multiple times.", apply: applyOf("-t")})
	ap.add(nil, &argAction{opts: []string{"--skip-tags"}, dest: "skip_tags", nargs: 1,
		help: "only run plays and tasks whose tags do not match these values. This argument may be specified multiple times.", apply: applyOf("--skip-tags")})
	addCheckOptions(ap)
	addInventoryOptions(ap)
	addRuntaskOptions(ap)
	addVaultOptions(ap)
	addForkOptions(ap, cfg)
	addModuleOptions(ap)
	ap.add(nil, &argAction{opts: []string{"--syntax-check"}, dest: "syntax",
		help: "perform a syntax check on the playbook, but do not execute it", apply: applyOf("--syntax-check")})
	ap.add(nil, &argAction{opts: []string{"--list-tasks"}, dest: "listtasks",
		help: "list all tasks that would be executed", apply: applyOf("--list-tasks")})
	ap.add(nil, &argAction{opts: []string{"--list-tags"}, dest: "listtags",
		help: "list all available tags", apply: applyOf("--list-tags")})
	ap.add(nil, &argAction{opts: []string{"--step"}, dest: "step",
		help: "one-step-at-a-time: confirm each task before running", apply: applyOf("--step")})
	ap.add(nil, &argAction{opts: []string{"--start-at-task"}, dest: "start_at_task", nargs: 1,
		help: "start the playbook at the task matching this name", apply: applyOf("--start-at-task")})
	ap.add(nil, &argAction{dest: "args", metavar: "playbook", help: "Playbook(s)", nargs: -1,
		apply: func(p *parsedArgs, vs []string, _ string) error {
			p.positional = append(p.positional, vs...)
			return nil
		}})
	return ap
}

// adhocParser is AdHocCLI.init_parser.
func adhocParser(cfg *config.Config) *argParser {
	ap := baseParser("ansible", "Define and run a single task 'playbook' against a set of hosts",
		"Some actions do not make sense in Ad-Hoc (include, meta, etc)")
	addRunasOptions(ap, cfg)
	addInventoryOptions(ap)
	ap.add(nil, &argAction{opts: []string{"-P", "--poll"}, dest: "poll_interval", nargs: 1, isInt: true,
		help:  fmt.Sprintf("set the poll interval if using -B (default=%d)", cfg.PollInterval),
		apply: value(func(p *parsedArgs, v string) error { p.poll, _ = pyInt(v); p.pollSet = true; return nil })})
	ap.add(nil, &argAction{opts: []string{"-B", "--background"}, dest: "seconds", nargs: 1, isInt: true,
		help:  "run asynchronously, failing after X seconds (default=N/A)",
		apply: value(func(p *parsedArgs, v string) error { p.background, _ = pyInt(v); return nil })})
	ap.add(nil, &argAction{opts: []string{"-o", "--one-line"}, dest: "one_line", help: "condense output",
		deprecated: &argDeprecation{"", "2.23", "callback configuration to enable the oneline callback"},
		apply:      flag(func(p *parsedArgs) { p.oneLine = true })})
	ap.add(nil, &argAction{opts: []string{"-t", "--tree"}, dest: "tree", nargs: 1, help: "log output to this directory",
		deprecated: &argDeprecation{"", "2.23", "callback configuration to enable the tree callback"},
		apply:      value(func(p *parsedArgs, v string) error { p.tree = v; return nil })})
	addConnectOptions(ap, cfg)
	addCheckOptions(ap)
	addRuntaskOptions(ap)
	addVaultOptions(ap)
	addForkOptions(ap, cfg)
	addModuleOptions(ap)
	ap.add(nil, &argAction{opts: []string{"--playbook-dir"}, dest: "basedir", nargs: 1,
		help:  "Since this tool does not use playbooks, use this as a substitute playbook directory. This sets the relative path for many features including roles/ group_vars/ etc.",
		apply: value(func(p *parsedArgs, v string) error { p.playbookDir = unfrackPath(v); return nil })})
	ap.add(nil, &argAction{opts: []string{"--task-timeout"}, dest: "task_timeout", nargs: 1, isInt: true,
		help:  "set task timeout limit in seconds, must be positive integer.",
		apply: value(func(p *parsedArgs, v string) error { p.taskTimeout, _ = pyInt(v); p.taskTimeoutSet = true; return nil })})
	ap.add(nil, &argAction{opts: []string{"-a", "--args"}, dest: "module_args", nargs: 1,
		help:  `The action's options in space separated k=v format: -a 'opt1=val1 opt2=val2' or a json string: -a '{"opt1": "val1", "opt2": "val2"}'`,
		apply: applyOf("-a")})
	ap.add(nil, &argAction{opts: []string{"-m", "--module-name"}, dest: "module_name", nargs: 1,
		help: "Name of the action to execute (default=" + cfg.ModuleName + ")", apply: applyOf("-m")})
	ap.add(nil, &argAction{dest: "args", metavar: "pattern", help: "host pattern", nargs: 1,
		apply: func(p *parsedArgs, vs []string, _ string) error {
			p.positional = append(p.positional, vs...)
			return nil
		}})
	return ap
}

// parseCommand parses a command line with the command's parser, as
// CLI.parse does, then post_process_args' checks (--forks). done reports
// the command is over (help, version, an error), with its exit code.
func parseCommand(ap *argParser, cfg *config.Config, args []string) (p *parsedArgs, code int, done bool) {
	p = &parsedArgs{extraVars: map[string]any{}, prog: ap.prog, verbosity: cfg.Verbosity}
	ap.deprecationsOn = cfg.DeprecationWarnings
	res := ap.parse(args, p)
	if res.exit {
		return nil, res.code, true
	}
	forks := cfg.Forks
	if p.forks != 0 || p.forksSet {
		forks = p.forks
	}
	if forks < 1 {
		ap.fail("The number of processes (--forks) must be >= 1", false)
		return nil, 2, true
	}
	if res.apply != nil {
		printError(res.apply)
		return nil, 1, true
	}
	return p, 0, false
}

// showDeprecation prints a deprecation warning, after the one-time hint
// on silencing them.
func showDeprecation(msg string) {
	fmt.Fprint(os.Stderr, executor.DeprecationHint()+msg)
}

// unfrackPath is unfrackpath(path): ~ and $VARS expanded, absolute, with
// symlinks resolved.
func unfrackPath(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	p = os.ExpandEnv(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// callbackDeprecation is the deprecation warning loading a deprecated
// callback plugin shows.
func callbackDeprecation(name string) string {
	return fmt.Sprintf("[DEPRECATION WARNING]: %s has been deprecated. Use another callback plugin, or vendor and/or move the %s callback to a collection. "+
		"This feature will be removed from ansible-core version 2.23.\nOrigin: <unknown>\n\n%s\n\n", name, name, name)
}
