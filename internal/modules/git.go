package modules

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(gitModule, "git", "ansible.builtin.git")
}

var gitSpec = args.Spec{
	"dest":              {},
	"repo":              {Required: true, Aliases: []string{"name"}},
	"version":           {Default: "HEAD"},
	"remote":            {Default: "origin"},
	"refspec":           {},
	"reference":         {},
	"force":             {Type: "bool", Default: false},
	"depth":             {Type: "int"},
	"clone":             {Type: "bool", Default: true},
	"update":            {Type: "bool", Default: true},
	"verify_commit":     {Type: "bool", Default: false},
	"gpg_allowlist":     {Type: "list", Default: []any{}},
	"accept_hostkey":    {Type: "bool", Default: false},
	"accept_newhostkey": {Type: "bool", Default: false},
	"key_file":          {},
	"ssh_opts":          {},
	"executable":        {},
	"bare":              {Type: "bool", Default: false},
	"recursive":         {Type: "bool", Default: true},
	"single_branch":     {Type: "bool", Default: false},
	"track_submodules":  {Type: "bool", Default: false},
	"umask":             {Type: "any"},
	"archive":           {},
	"archive_prefix":    {},
	"separate_git_dir":  {},
}

// gitAbort unwinds a fail_json/exit_json raised deep inside the port.
type gitAbort struct{ res *agentproto.Result }

// gitRun is one module invocation: the parameters plus the process
// environment the module's run_command calls see.
type gitRun struct {
	env      *RunEnv
	p        *args.Parsed
	gitPath  string
	extraEnv map[string]string // run_command_environ_update + GIT_SSH_COMMAND
	params   map[string]any    // module.params['repo'] etc. as given
	warnings []any
	result   map[string]any

	restoreUmask func()
}

func (g *gitRun) fail(msg string, extra map[string]any) {
	res := &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{}}
	for k, v := range g.result {
		switch k {
		case "changed":
			res.Changed, _ = v.(bool)
		default:
			res.Extra[k] = v
		}
	}
	for k, v := range extra {
		if k == "msg" {
			continue
		}
		res.Extra[k] = v
	}
	// The controller adds *_lines next to a result's stdout/stderr.
	for _, k := range []string{"stdout", "stderr"} {
		if s, ok := res.Extra[k].(string); ok {
			res.Extra[k+"_lines"] = anyList(pySplitLines(s))
		}
	}
	if len(g.warnings) > 0 {
		res.Extra["warnings"] = g.warnings
	}
	panic(gitAbort{res})
}

// failOnly is fail_json without the accumulated result.
func (g *gitRun) failOnly(msg string, extra map[string]any) {
	g.result = nil
	g.fail(msg, extra)
}

func (g *gitRun) exit() {
	res := &agentproto.Result{Extra: map[string]any{}}
	for k, v := range g.result {
		switch k {
		case "changed":
			res.Changed, _ = v.(bool)
		case "msg":
			res.Msg, _ = v.(string)
		case "diff":
			res.Diff = v
		default:
			res.Extra[k] = v
		}
	}
	if len(g.warnings) > 0 {
		res.Extra["warnings"] = g.warnings
	}
	panic(gitAbort{res})
}

func (g *gitRun) warn(msg string) { g.warnings = append(g.warnings, msg) }

// run is AnsibleModule.run_command over an argv (user and variable
// expansion applied to every argument, as run_command does).
func (g *gitRun) run(argv []string, cwd string) (int, string, string) {
	expanded := make([]string, len(argv))
	for i, a := range argv {
		expanded[i] = pyExpandPath(a)
	}
	opts := cmdOpts{Env: g.extraEnv}
	if cwd != "" {
		if abs, err := filepath.Abs(pyExpandPath(cwd)); err == nil && isDir(abs) {
			opts.Cwd = abs
		}
	}
	rc, out, errOut := runCommand(g.env, expanded, opts)
	if rc == 257 && out == "" {
		g.failOnly("Error executing command.", map[string]any{"rc": int64(syscall.ENOENT), "stdout": "", "stderr": "",
			"cmd": gitCleanArgs(expanded)})
	}
	return rc, out, errOut
}

// runCheck is run_command(..., check_rc=True).
func (g *gitRun) runCheck(argv []string, cwd string) (int, string, string) {
	rc, out, errOut := g.run(argv, cwd)
	if rc != 0 {
		g.failOnly(heuristicLogSanitize(strings.TrimRight(errOut, " \t\r\n\v\f")), map[string]any{
			"cmd": gitCleanArgs(argv), "rc": int64(rc), "stdout": out, "stderr": errOut})
	}
	return rc, out, errOut
}

// runStr is run_command on a command string (shlex-split).
func (g *gitRun) runStr(cmd, cwd string) (int, string, string) {
	return g.run(shlexWords(cmd), cwd)
}

func (g *gitRun) runStrCheck(cmd, cwd string) (int, string, string) {
	return g.runCheck(shlexWords(cmd), cwd)
}

var passwdArgRe = regexp.MustCompile(`(?i)^[-]{0,2}pass[-]?(word|wd)?`)

// gitCleanArgs is AnsibleModule._clean_args: the printable command.
func gitCleanArgs(argv []string) string {
	var out []string
	isPasswd := false
	for _, a := range argv {
		if isPasswd {
			isPasswd = false
			out = append(out, "********")
			continue
		}
		if passwdArgRe.MatchString(a) {
			if i := strings.Index(a, "="); i > -1 {
				out = append(out, a[:i]+"=********")
				continue
			}
			isPasswd = true
		}
		out = append(out, shQuote(heuristicLogSanitize(a)))
	}
	return strings.Join(out, " ")
}

// heuristicLogSanitize is ansible's heuristic_log_sanitize: whatever
// looks like the user:password part of a URL (or of ssh's user@host)
// before an '@' is masked, false positives included
// ("ssh://user@host" becomes "ssh:********@host").
func heuristicLogSanitize(data string) string {
	var output []string
	begin := len(data)
	prevBegin := begin
	for {
		end := strings.LastIndex(data[:begin], "@")
		if end < 0 {
			output = append([]string{data[:begin]}, output...)
			break
		}
		sep := -1
		sepSearchEnd := end
		for sep < 0 {
			begin = strings.LastIndex(data[:sepSearchEnd], "://")
			if begin < 0 {
				begin = 0
			}
			if begin+3 < end {
				if i := strings.Index(data[begin+3:end], ":"); i >= 0 {
					sep = begin + 3 + i
					break
				}
			}
			if begin == 0 {
				output = append([]string{data[:prevBegin]}, output...)
				break
			}
			sepSearchEnd = begin
		}
		if sep < 0 {
			break
		}
		output = append([]string{data[begin : sep+1], "********", data[end:prevBegin]}, output...)
		prevBegin = begin
	}
	return strings.Join(output, "")
}

// shlexWords is shlex.split (the module's command strings never carry
// unbalanced quotes).
func shlexWords(s string) []string {
	words, _ := shlexSplit(s)
	return words
}

// gitModule ports ansible.builtin.git on top of the git CLI.
func gitModule(env *RunEnv, rawArgs map[string]any) (res *agentproto.Result) {
	defer func() {
		if r := recover(); r != nil {
			if a, ok := r.(gitAbort); ok {
				res = a.res
				return
			}
			panic(r)
		}
	}()
	if err := gitSpec.MutuallyExclusive(rawArgs, []string{"separate_git_dir", "bare"},
		[]string{"accept_hostkey", "accept_newhostkey"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := gitSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if p.Has("archive_prefix") && !p.Has("archive") {
		return agentproto.Fail("missing parameter(s) required by 'archive_prefix': archive")
	}
	g := &gitRun{env: env, p: p, result: map[string]any{"changed": false}}
	defer func() {
		if g.restoreUmask != nil {
			g.restoreUmask()
		}
	}()
	g.main()
	return nil
}

func (g *gitRun) main() {
	p := g.p
	dest := ""
	if p.Has("dest") {
		dest = pyExpandPath(p.Str("dest"))
	}
	origRepo := p.Str("repo")
	repo := origRepo
	version := p.Str("version")
	remote := p.Str("remote")
	refspec := p.Str("refspec")
	force := p.Bool("force")
	depth := int(p.Int("depth"))
	hasDepth := p.Has("depth")
	update := p.Bool("update")
	allowClone := p.Bool("clone")
	bare := p.Bool("bare")
	verifyCommit := p.Bool("verify_commit")
	gpgAllowlist := strList(p.List("gpg_allowlist"))
	reference := p.Str("reference")
	singleBranch := p.Bool("single_branch")
	archive := ""
	if p.Has("archive") {
		archive = pyExpandPath(p.Str("archive"))
	}
	var archivePrefix *string
	if p.Has("archive_prefix") {
		s := p.Str("archive_prefix")
		archivePrefix = &s
	}
	separateGitDir := ""
	if p.Has("separate_git_dir") {
		separateGitDir = pyExpandPath(p.Str("separate_git_dir"))
	}
	keyFile := ""
	if p.Has("key_file") {
		keyFile = pyExpandPath(p.Str("key_file"))
	}
	var sshOpts *string
	if p.Has("ssh_opts") {
		s := p.Str("ssh_opts")
		sshOpts = &s
	}

	if p.Has("executable") {
		g.gitPath = pyExpandPath(p.Str("executable"))
	} else {
		path, err := getBinPathIn(envPATH(g.env), "git")
		if err != nil {
			g.failOnly(err.Error(), nil)
		}
		g.gitPath = path
	}
	gitPath := g.gitPath

	addHostKeyOpt := func(opt string) {
		if sshOpts != nil {
			if !strings.Contains(*sshOpts, "-o StrictHostKeyChecking=no") && !strings.Contains(*sshOpts, "-o StrictHostKeyChecking=accept-new") {
				s := *sshOpts + " -o " + opt
				sshOpts = &s
			}
		} else {
			s := "-o " + opt
			sshOpts = &s
		}
	}
	if p.Bool("accept_hostkey") {
		addHostKeyOpt("StrictHostKeyChecking=no")
	}
	if p.Bool("accept_newhostkey") {
		if !g.sshSupportsAcceptNewHostKey() {
			g.warn("Your ssh client does not support accept_newhostkey option, therefore it cannot be used.")
		} else {
			addHostKeyOpt("StrictHostKeyChecking=accept-new")
		}
	}

	if p.Has("umask") {
		s, ok := p.Any("umask").(string)
		if !ok {
			g.failOnly("umask must be defined as a quoted octal integer", nil)
		}
		n, ok := pyIntBase(s, 8)
		if !ok {
			g.failOnly("umask must be an octal integer", map[string]any{
				"details": fmt.Sprintf("invalid literal for int() with base 8: %s", pyStrRepr(s))})
		}
		// The module process's umask; restored when the module returns,
		// since in-process runs share the control process.
		old := syscall.Umask(int(n))
		g.restoreUmask = func() { syscall.Umask(old) }
	}

	if strings.HasPrefix(pyExpandUser(repo), "/") {
		repo = "file://" + pyExpandUser(repo)
	}

	loc := bestParsableLocale(g.env)
	g.extraEnv = map[string]string{"LANG": loc, "LC_ALL": loc, "LC_MESSAGES": loc, "LC_CTYPE": loc, "LANGUAGE": loc}

	if separateGitDir != "" {
		separateGitDir = pyRealpath(separateGitDir)
	}

	gitconfig := ""
	if dest == "" && allowClone {
		g.failOnly("the destination directory must be specified unless clone=no", nil)
	} else if dest != "" {
		dest, _ = filepath.Abs(dest)
		repoPath, err := getRepoPath(dest, bare)
		if err == nil && separateGitDir != "" && pathExists(repoPath) && separateGitDir != repoPath {
			g.result["changed"] = true
			if !g.env.CheckMode {
				g.relocateRepo(separateGitDir, repoPath, dest)
				repoPath = separateGitDir
			}
		}
		if err != nil {
			g.failOnly("Current repo does not have a valid reference to a separate Git dir or it refers to the invalid path",
				map[string]any{"details": err.Error()})
		}
		gitconfig = pyJoin(repoPath, "config")
	}

	gitVersion := g.gitVersion()
	g.setSSHEnv(keyFile, sshOpts, gitVersion)

	if hasDepth && gitVersion != "" && looseVersionLess(gitVersion, "1.9.1") {
		g.warn("git version is too old to fully support the depth argument. Falling back to full checkouts.")
		hasDepth, depth = false, 0
	}
	recursive := p.Bool("recursive")
	trackSubmodules := p.Bool("track_submodules")

	g.result["before"] = nil
	localMods := false
	remoteURLChanged := false
	var remoteHead string
	if (dest != "" && !pathExists(gitconfig)) || (dest == "" && !allowClone) {
		if g.env.CheckMode || !allowClone {
			remoteHead = g.getRemoteHead(dest, version, repo, bare, origRepo)
			g.result["changed"] = true
			g.result["after"] = remoteHead
			if g.env.DiffMode {
				if d := g.getDiff(dest, repo, remote, depth, bare, nil, remoteHead, refspec, force); d != nil {
					g.result["diff"] = d
				}
			}
			g.exit()
		}
		g.clone(repo, dest, remote, depth, version, bare, reference, refspec, gitVersion, verifyCommit,
			separateGitDir, gpgAllowlist, singleBranch, origRepo)
	} else if !update {
		before := g.getVersion(dest, "HEAD")
		g.result["before"] = before
		g.result["after"] = before
		if archive != "" {
			if g.env.CheckMode {
				g.result["changed"] = true
				g.exit()
			}
			g.createArchive(dest, archive, archivePrefix, version, repo)
		}
		g.exit()
	} else {
		localMods = g.hasLocalMods(dest, bare)
		g.result["before"] = g.getVersion(dest, "HEAD")
		if localMods {
			if !force {
				g.fail("Local modifications exist in the destination: "+dest+" (force=no).", nil)
			}
			if !g.env.CheckMode {
				g.runStrCheck(gitPath+" reset --hard HEAD", dest)
				g.result["changed"] = true
				g.result["msg"] = "Local modifications exist in the destination: " + dest
			}
		}
		if g.env.CheckMode {
			url, ok := g.getRemoteURL(dest, remote)
			remoteURLChanged = ok && url != "" && url != repo && unfrackGitPath(url) != unfrackGitPath(repo)
			if ok && url == "" {
				g.result["remote_url_changed"] = ""
			} else if !ok {
				g.result["remote_url_changed"] = nil
			} else {
				g.result["remote_url_changed"] = remoteURLChanged
			}
		} else {
			remoteURLChanged = g.setRemoteURL(repo, dest, remote)
			g.result["remote_url_changed"] = remoteURLChanged
		}
		if g.env.CheckMode {
			remoteHead = g.getRemoteHead(dest, version, remote, bare, origRepo)
			before, _ := g.result["before"].(string)
			g.result["changed"] = before != remoteHead || remoteURLChanged
			g.result["after"] = remoteHead
			if g.env.DiffMode {
				if d := g.getDiff(dest, repo, remote, depth, bare, &before, remoteHead, refspec, force); d != nil {
					g.result["diff"] = d
				}
			}
			g.exit()
		}
		g.fetch(repo, dest, version, remote, depth, bare, refspec, gitVersion, force)
		g.result["after"] = g.getVersion(dest, "HEAD")
	}

	if !bare {
		g.switchVersion(dest, remote, version, verifyCommit, depth, gpgAllowlist)
	}

	submodulesUpdated := false
	if recursive && !bare {
		submodulesUpdated = g.submodulesFetch(remote, trackSubmodules, dest)
		if submodulesUpdated {
			g.result["submodules_changed"] = true
			if g.env.CheckMode {
				g.result["changed"] = true
				g.result["after"] = remoteHead
				g.exit()
			}
			g.submoduleUpdate(dest, trackSubmodules, force)
		}
	}

	after := g.getVersion(dest, "HEAD")
	g.result["after"] = after
	if g.result["before"] != any(after) || localMods || submodulesUpdated || remoteURLChanged {
		g.result["changed"] = true
		if g.env.DiffMode {
			var before *string
			if b, ok := g.result["before"].(string); ok {
				before = &b
			}
			if d := g.getDiff(dest, repo, remote, depth, bare, before, after, refspec, force); d != nil {
				g.result["diff"] = d
			}
		}
	}

	if archive != "" {
		if g.env.CheckMode {
			g.result["changed"] = true
			g.exit()
		}
		g.createArchive(dest, archive, archivePrefix, version, repo)
	}
	g.exit()
}

// unfrackGitPath is os.path.normpath(os.path.realpath(expanduser(expandvars(p)))).
func unfrackGitPath(p string) string {
	return filepath.Clean(pyRealpath(pyExpandPath(p)))
}

func (g *gitRun) relocateRepo(repoDir, oldRepoDir, worktreeDir string) {
	if pathExists(repoDir) {
		g.failOnly(fmt.Sprintf("Separate-git-dir path %s already exists.", repoDir), nil)
	}
	if worktreeDir == "" {
		return
	}
	if err := shutilMove(oldRepoDir, repoDir); err != nil {
		g.failOnly("Unable to move git dir.", nil)
	}
	if err := os.WriteFile(pyJoin(worktreeDir, ".git"), []byte("gitdir: "+repoDir), 0o644); err != nil {
		if pathExists(repoDir) {
			shutilMove(repoDir, oldRepoDir)
		}
		g.failOnly("Unable to move git dir.", nil)
	}
	g.result["git_dir_before"] = oldRepoDir
	g.result["git_dir_now"] = repoDir
}

// headSplitter extracts the branch from a HEAD file.
func headSplitter(headfile, remote string) (string, bool) {
	data, err := os.ReadFile(headfile)
	if err != nil {
		return "", false
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i+1]
	}
	if line == "" {
		return "", false
	}
	line = strings.Replace(line, "refs/remotes/"+remote, "", 1)
	parts := strings.Split(line, " ")
	newref := parts[len(parts)-1]
	nparts := strings.SplitN(newref, "/", 3)
	return strings.TrimRight(nparts[len(nparts)-1], "\n"), true
}

func (g *gitRun) sshSupportsAcceptNewHostKey() bool {
	ssh, err := getBinPathIn(envPATH(g.env), "ssh")
	if err != nil {
		g.failOnly("Remote host is missing ssh command, so you cannot use acceptnewhostkey option.",
			map[string]any{"details": err.Error()})
	}
	rc, _, _ := g.run([]string{ssh, "-o", "StrictHostKeyChecking=accept-new", "-V"}, "")
	return rc == 0
}

// setSSHEnv is set_git_ssh_env: GIT_SSH_COMMAND for git >= 2.3, else a
// GIT_SSH wrapper script.
func (g *gitRun) setSSHEnv(keyFile string, sshOpts *string, gitVersion string) {
	envGet := func(k string) (string, bool) {
		if v, ok := g.env.Env[k]; ok {
			return v, true
		}
		return os.LookupEnv(k)
	}
	base, _ := envGet("GIT_SSH_OPTS")
	opts := base
	if sshOpts != nil {
		opts = base + " " + *sshOpts
	}
	if g.p.Bool("accept_hostkey") && !strings.Contains(opts, "StrictHostKeyChecking=no") {
		opts += " -o StrictHostKeyChecking=no"
	}
	if !strings.Contains(opts, "BatchMode=yes") {
		opts += " -o BatchMode=yes"
	}
	if keyFile != "" {
		keyOpt := "-i " + keyFile
		if !strings.Contains(opts, keyOpt) {
			opts += "  " + keyOpt
		}
		if !strings.Contains(opts, "IdentitiesOnly=yes") {
			opts += " -o IdentitiesOnly=yes"
		}
	}
	sshCmd, ok := envGet("GIT_SSH")
	if !ok {
		if sshCmd, ok = envGet("GIT_SSH_COMMAND"); !ok {
			sshCmd = "ssh"
		}
	}
	if gitVersion != "" && looseVersionLess(gitVersion, "2.3.0") {
		g.extraEnv["GIT_SSH_OPTS"] = opts
		f, err := pyMkstemp("", "tmp")
		if err == nil {
			fmt.Fprintf(f, "#!/bin/sh\n%s $GIT_SSH_OPTS \"$@\"\n", sshCmd)
			f.Chmod(0o700)
			f.Close()
			g.extraEnv["GIT_SSH"] = f.Name()
		}
		return
	}
	full := sshCmd
	if opts != "" {
		full += " " + opts
	}
	g.extraEnv["GIT_SSH_COMMAND"] = full
}

func (g *gitRun) getVersion(dest, ref string) string {
	_, out, _ := g.runStr(g.gitPath+" rev-parse "+ref, dest)
	return strings.TrimRight(out, "\n")
}

func (g *gitRun) gitVersion() string {
	rc, out, _ := g.runStr(g.gitPath+" --version", "")
	if rc != 0 {
		return ""
	}
	m := regexp.MustCompile(`git version (.*)$`).FindStringSubmatch(strings.TrimRight(out, "\n"))
	if m == nil {
		return ""
	}
	return m[1]
}

func (g *gitRun) getSubmoduleVersions(dest string) map[string]string {
	rc, out, errOut := g.run([]string{g.gitPath, "submodule", "foreach", g.gitPath, "rev-parse", "HEAD"}, dest)
	if rc != 0 {
		g.failOnly("Unable to determine hashes of submodules", map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
	}
	subs := map[string]string{}
	var name *string
	for _, line := range pySplitLines(out) {
		switch {
		case strings.HasPrefix(line, "Entering '"):
			n := line[10 : len(line)-1]
			name = &n
		case len(strings.TrimSpace(line)) == 40:
			if name == nil {
				g.failOnly("No message provided", nil)
			}
			subs[*name] = strings.TrimSpace(line)
			name = nil
		default:
			g.failOnly("Unable to parse submodule hash line: "+strings.TrimSpace(line), nil)
		}
	}
	if name != nil {
		g.failOnly("Unable to find hash for submodule: "+*name, nil)
	}
	return subs
}

func (g *gitRun) getSubmoduleBranch(dest, submodule string) string {
	rc, out, _ := g.run([]string{g.gitPath, "config", "-f", ".gitmodules", "submodule." + submodule + ".branch"}, dest)
	if rc == 0 && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	rc, out, _ = g.run([]string{g.gitPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"}, pyJoin(dest, submodule))
	if rc == 0 && strings.TrimSpace(out) != "" {
		parts := strings.SplitN(strings.TrimSpace(out), "/", 2)
		return parts[len(parts)-1]
	}
	return "HEAD"
}

func (g *gitRun) clone(repo, dest, remote string, depth int, version string, bare bool, reference, refspec, gitVersion string,
	verifyCommit bool, separateGitDir string, gpgAllowlist []string, singleBranch bool, origRepo string) {
	destDir := filepath.Dir(dest)
	os.MkdirAll(destDir, 0o777)
	cmd := []string{g.gitPath, "clone"}
	if bare {
		cmd = append(cmd, "--bare")
	} else {
		cmd = append(cmd, "--origin", remote)
	}
	isBranchOrTag := g.isRemoteBranch(dest, repo, version) || g.isRemoteTag(dest, repo, version)
	if depth != 0 {
		switch {
		case version == "HEAD" || refspec != "":
			cmd = append(cmd, "--depth", strconv.Itoa(depth))
		case isBranchOrTag:
			cmd = append(cmd, "--depth", strconv.Itoa(depth), "--branch", version)
		default:
			g.warn("Ignoring depth argument. Shallow clones are only available for HEAD, branches, tags or in combination with refspec.")
		}
	}
	if reference != "" {
		cmd = append(cmd, "--reference", reference)
	}
	if singleBranch {
		if gitVersion == "" {
			g.failOnly("Cannot find git executable at "+g.gitPath, nil)
		}
		if looseVersionLess(gitVersion, "1.7.10") {
			g.warn(fmt.Sprintf("git version '%s' is too old to use 'single-branch'. Ignoring.", gitVersion))
		} else {
			cmd = append(cmd, "--single-branch")
			if isBranchOrTag {
				cmd = append(cmd, "--branch", version)
			}
		}
	}
	fallback := false
	if separateGitDir != "" {
		if gitVersion == "" {
			g.failOnly("Cannot find git executable at "+g.gitPath, nil)
		}
		if looseVersionLess(gitVersion, "1.7.5") {
			fallback = true
		} else {
			cmd = append(cmd, "--separate-git-dir="+separateGitDir)
		}
	}
	cmd = append(cmd, repo, dest)
	g.runCheck(cmd, destDir)
	if fallback {
		g.relocateRepo(separateGitDir, pyJoin(dest, ".git"), dest)
	}
	if bare && remote != "origin" {
		g.runCheck([]string{g.gitPath, "remote", "add", remote, repo}, dest)
	}
	if refspec != "" {
		c := []string{g.gitPath, "fetch"}
		if depth != 0 {
			c = append(c, "--depth", strconv.Itoa(depth))
		}
		g.runCheck(append(c, remote, refspec), dest)
	}
	if verifyCommit {
		g.verifyCommitSign(dest, version, gpgAllowlist)
	}
}

func (g *gitRun) hasLocalMods(dest string, bare bool) bool {
	if bare {
		return false
	}
	_, out, _ := g.runStr(g.gitPath+" status --porcelain", dest)
	n := 0
	for _, l := range pySplitLines(out) {
		if !strings.HasPrefix(l, "??") {
			n++
		}
	}
	return n > 0
}

// getDiff returns the module's diff ({"prepared": ...}), nil for none.
func (g *gitRun) getDiff(dest, repo, remote string, depth int, bare bool, before *string, after, refspec string, force bool) map[string]any {
	if before == nil {
		return map[string]any{"prepared": ">> Newly checked out " + after}
	}
	if *before == after {
		return nil
	}
	g.fetch(repo, dest, after, remote, depth, bare, refspec, g.gitVersion(), force)
	rc, out, errOut := g.runStr(fmt.Sprintf("%s diff %s %s", g.gitPath, *before, after), dest)
	switch {
	case rc == 0 && out != "":
		return map[string]any{"prepared": out}
	case rc == 0:
		return map[string]any{"prepared": fmt.Sprintf(">> No visual differences between %s and %s", *before, after)}
	case errOut != "":
		return map[string]any{"prepared": fmt.Sprintf(">> Failed to get proper diff between %s and %s:\n>> %s", *before, after, errOut)}
	}
	return map[string]any{"prepared": fmt.Sprintf(">> Failed to get proper diff between %s and %s", *before, after)}
}

func (g *gitRun) getSHAHash(remote, version, cwd string) string {
	if cwd == "" || !pathExists(cwd) {
		dir, err := os.MkdirTemp("", "ansible-moduletmp-")
		if err != nil {
			g.failOnly(pyOSErrorStr(err), nil)
		}
		defer os.RemoveAll(dir)
		cwd = pyJoin(dir, "tmp_repo")
		os.Mkdir(cwd, 0o700)
		g.run([]string{g.gitPath, "init"}, cwd)
		g.run([]string{g.gitPath, "remote", "add", "origin", remote}, cwd)
	}
	g.runCheck([]string{g.gitPath, "fetch", "--dry-run", remote, version}, cwd)
	return version
}

func (g *gitRun) getRemoteHead(dest, version, remote string, bare bool, origRepo string) string {
	cloning := false
	cwd := ""
	if remote == origRepo || remote == "file://"+pyExpandUser(origRepo) {
		cloning = true
	} else {
		cwd = dest
	}
	tag := false
	var cmd string
	switch {
	case version == "HEAD":
		if cloning {
			cmd = fmt.Sprintf("%s ls-remote %s -h HEAD", g.gitPath, remote)
		} else {
			head := g.getHeadBranch(dest, remote, bare)
			cmd = fmt.Sprintf("%s ls-remote %s -h refs/heads/%s", g.gitPath, remote, head)
		}
	case g.isRemoteBranch(dest, remote, version):
		cmd = fmt.Sprintf("%s ls-remote %s -h refs/heads/%s", g.gitPath, remote, version)
	case g.isRemoteTag(dest, remote, version):
		tag = true
		cmd = fmt.Sprintf("%s ls-remote %s -t refs/tags/%s*", g.gitPath, remote, version)
	default:
		return g.getSHAHash(remote, version, cwd)
	}
	rc, out, errOut := g.runStrCheck(cmd, cwd)
	if len(out) < 1 {
		g.failOnly("Could not determine remote revision for "+version, map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
	}
	if tag {
		for _, t := range strings.Split(out, "\n") {
			if strings.HasSuffix(t, version+"^{}") {
				out = t
				break
			} else if strings.HasSuffix(t, version) {
				out = t
			}
		}
	}
	return strings.Fields(out)[0]
}

func (g *gitRun) isRemoteTag(dest, remote, version string) bool {
	_, out, _ := g.runStrCheck(fmt.Sprintf("%s ls-remote %s -t refs/tags/%s", g.gitPath, remote, version), dest)
	return strings.Contains(out, version)
}

func (g *gitRun) isRemoteBranch(dest, remote, version string) bool {
	_, out, _ := g.runStrCheck(fmt.Sprintf("%s ls-remote %s -h refs/heads/%s", g.gitPath, remote, version), dest)
	return strings.Contains(out, version)
}

func (g *gitRun) getBranches(dest string) []string {
	rc, out, errOut := g.runStr(g.gitPath+" branch --no-color -a", dest)
	if rc != 0 {
		g.failOnly("Could not determine branch data - received "+out, map[string]any{"stdout": out, "stderr": errOut})
	}
	var branches []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			branches = append(branches, strings.TrimSpace(l))
		}
	}
	return branches
}

func (g *gitRun) getAnnotatedTags(dest string) []string {
	rc, out, errOut := g.run([]string{g.gitPath, "for-each-ref", "refs/tags/", "--format", "%(objecttype):%(refname:short)"}, dest)
	if rc != 0 {
		g.failOnly("Could not determine tag data - received "+out, map[string]any{"stdout": out, "stderr": errOut})
	}
	var tags []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		typ, name, _ := strings.Cut(strings.TrimSpace(l), ":")
		if typ == "tag" {
			tags = append(tags, name)
		}
	}
	return tags
}

func (g *gitRun) isLocalBranch(dest, branch string) bool {
	for _, b := range g.getBranches(dest) {
		if b == branch || b == "* "+branch {
			return true
		}
	}
	return false
}

func (g *gitRun) isNotABranch(dest string) bool {
	for _, b := range g.getBranches(dest) {
		if strings.HasPrefix(b, "* ") && (strings.Contains(b, "no branch") || strings.Contains(b, "detached from") || strings.Contains(b, "detached at")) {
			return true
		}
	}
	return false
}

// getRepoPath is get_repo_path: dest/.git, following a "gitdir:" file.
func getRepoPath(dest string, bare bool) (string, error) {
	repoPath := dest
	if !bare {
		repoPath = pyJoin(dest, ".git")
	}
	if isFile(repoPath) {
		data, err := os.ReadFile(repoPath)
		if err != nil {
			return "", fmt.Errorf("%s", pyOSErrorStr(err))
		}
		prefix, gitdir, ok := strings.Cut(strings.TrimRight(string(data), " \t\r\n"), "gitdir: ")
		if !ok {
			return "", fmt.Errorf("not enough values to unpack (expected 2, got 1)")
		}
		if prefix != "" {
			return "", fmt.Errorf(".git file has invalid git dir reference format")
		}
		if filepath.IsAbs(gitdir) {
			repoPath = gitdir
		} else {
			repoPath = pyJoin(dest, gitdir)
		}
		if !isDir(repoPath) {
			return "", fmt.Errorf("%s is not a directory", repoPath)
		}
	}
	return repoPath, nil
}

func (g *gitRun) getHeadBranch(dest, remote string, bare bool) string {
	repoPath, err := getRepoPath(dest, bare)
	if err != nil {
		g.failOnly("Current repo does not have a valid reference to a separate Git dir or it refers to the invalid path",
			map[string]any{"details": err.Error()})
	}
	headfile := pyJoin(repoPath, "HEAD")
	if g.isNotABranch(dest) {
		headfile = pyJoin(pyJoin(pyJoin(pyJoin(repoPath, "refs"), "remotes"), remote), "HEAD")
	}
	branch, _ := headSplitter(headfile, remote)
	return branch
}

// getRemoteURL returns the remote's URL; ok is false when git failed.
func (g *gitRun) getRemoteURL(dest, remote string) (string, bool) {
	rc, out, _ := g.run([]string{g.gitPath, "ls-remote", "--get-url", remote}, dest)
	if rc != 0 {
		return "", false
	}
	return strings.TrimRight(out, "\n"), true
}

func (g *gitRun) setRemoteURL(repo, dest, remote string) bool {
	url, ok := g.getRemoteURL(dest, remote)
	if ok && (url == repo || unfrackGitPath(url) == unfrackGitPath(repo)) {
		return false
	}
	rc, out, errOut := g.run([]string{g.gitPath, "remote", "set-url", remote, repo}, dest)
	if rc != 0 {
		g.failOnly(fmt.Sprintf("Failed to set a new url %s for %s: %s %s", repo, remote, out, errOut), nil)
	}
	return ok
}

func (g *gitRun) fetch(repo, dest, version, remote string, depth int, bare bool, refspec, gitVersion string, force bool) {
	g.setRemoteURL(repo, dest, remote)
	type labeled struct {
		label string
		cmd   []string
	}
	var commands []labeled
	const fetchStr = "download remote objects and refs"
	fetchCmd := []string{g.gitPath, "fetch"}
	var refspecs []string
	if depth != 0 {
		currentHead := g.getHeadBranch(dest, remote, false)
		switch {
		case refspec != "":
			refspecs = append(refspecs, refspec)
		case version == "HEAD":
			refspecs = append(refspecs, currentHead)
		case g.isRemoteBranch(dest, repo, version):
			if currentHead != version {
				refspecs = append(refspecs, fmt.Sprintf("+refs/heads/%s:refs/heads/%s", version, version))
			}
			refspecs = append(refspecs, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", version, remote, version))
		case g.isRemoteTag(dest, repo, version):
			refspecs = append(refspecs, "+refs/tags/"+version+":refs/tags/"+version)
		}
		if len(refspecs) > 0 {
			fetchCmd = append(fetchCmd, "--depth", strconv.Itoa(depth))
		}
	}
	if depth == 0 || len(refspecs) == 0 {
		if bare {
			refspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
		} else if gitVersion != "" && !looseVersionLess(gitVersion, "1.9") {
			fetchCmd = append(fetchCmd, "--tags")
		} else {
			commands = append(commands, labeled{fetchStr, append(append([]string{}, fetchCmd...), remote)})
			refspecs = []string{"+refs/tags/*:refs/tags/*"}
		}
		if refspec != "" {
			refspecs = append(refspecs, refspec)
		}
	}
	if force {
		fetchCmd = append(fetchCmd, "--force")
	}
	fetchCmd = append(fetchCmd, remote)
	commands = append(commands, labeled{fetchStr, append(append([]string{}, fetchCmd...), refspecs...)})
	for _, c := range commands {
		rc, out, errOut := g.run(c.cmd, dest)
		if rc != 0 {
			g.failOnly(fmt.Sprintf("Failed to %s: %s %s", c.label, out, errOut), map[string]any{"cmd": anyList(c.cmd)})
		}
	}
}

func (g *gitRun) submodulesFetch(remote string, track bool, dest string) bool {
	gm := pyJoin(dest, ".gitmodules")
	if !pathExists(gm) {
		return false
	}
	changed := false
	data, _ := os.ReadFile(gm)
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if !changed && strings.HasPrefix(strings.TrimSpace(line), "path") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				path := strings.TrimSpace(parts[1])
				if !pathExists(pyJoin(pyJoin(dest, path), ".git")) {
					changed = true
				}
			}
		}
	}
	if changed {
		return true
	}
	begin := g.getSubmoduleVersions(dest)
	g.runCheck([]string{g.gitPath, "submodule", "foreach", g.gitPath, "fetch"}, dest)
	if track {
		after := map[string]string{}
		for sub := range begin {
			branch := g.getSubmoduleBranch(dest, sub)
			ref := "HEAD"
			if branch != "HEAD" {
				ref = remote + "/" + branch
			}
			rc, out, errOut := g.run([]string{g.gitPath, "rev-parse", ref}, pyJoin(dest, sub))
			if rc != 0 {
				g.failOnly(fmt.Sprintf("Unable to determine hash of submodule %s at %s", sub, ref),
					map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
			}
			after[sub] = strings.TrimSpace(out)
		}
		if len(after) != len(begin) {
			return true
		}
		for k, v := range begin {
			if after[k] != v {
				return true
			}
		}
		return false
	}
	_, out, _ := g.runCheck([]string{g.gitPath, "submodule", "status"}, dest)
	for _, line := range pySplitLines(out) {
		if line != "" && line[0] != ' ' {
			return true
		}
	}
	return false
}

// submoduleUpdateParams scrapes `git submodule update --help`'s usage for
// the options this git supports.
func (g *gitRun) submoduleUpdateParams(dest string) []string {
	_, _, errOut := g.runStr(g.gitPath+" submodule update --help", dest)
	var line string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.Contains(l, "git submodule [--quiet] update ") {
			line = l
		}
	}
	var params []string
	if line != "" {
		line = strings.NewReplacer("[", "", "]", "", "|", " ").Replace(line)
		for _, part := range shlexWords(line) {
			if strings.HasPrefix(part, "--") {
				params = append(params, strings.ReplaceAll(part, "--", ""))
			}
		}
	}
	return params
}

func (g *gitRun) submoduleUpdate(dest string, track, force bool) {
	params := g.submoduleUpdateParams(dest)
	if !pathExists(pyJoin(dest, ".gitmodules")) {
		return
	}
	g.runCheck([]string{g.gitPath, "submodule", "sync"}, dest)
	cmd := []string{g.gitPath, "submodule", "update", "--init", "--recursive"}
	hasRemote := false
	for _, p := range params {
		if p == "remote" {
			hasRemote = true
		}
	}
	if hasRemote && track {
		cmd = append(cmd, "--remote")
	}
	if force {
		cmd = append(cmd, "--force")
	}
	rc, out, errOut := g.run(cmd, dest)
	if rc != 0 {
		g.failOnly("Failed to init/update submodules: "+out+errOut, nil)
	}
}

func (g *gitRun) setRemoteBranch(dest, remote, version string, depth int) {
	branchref := fmt.Sprintf("+refs/heads/%s:refs/heads/%s +refs/heads/%s:refs/remotes/%s/%s", version, version, version, remote, version)
	rc, out, errOut := g.runStr(fmt.Sprintf("%s fetch --depth=%d %s %s", g.gitPath, depth, remote, branchref), dest)
	if rc != 0 {
		g.failOnly("Failed to fetch branch from remote: "+version, map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
	}
}

func (g *gitRun) switchVersion(dest, remote, version string, verifyCommit bool, depth int, gpgAllowlist []string) {
	var cmd, branch string
	if version == "HEAD" {
		branch = g.getHeadBranch(dest, remote, false)
		rc, out, errOut := g.runStr(fmt.Sprintf("%s checkout --force %s", g.gitPath, branch), dest)
		if rc != 0 {
			g.failOnly("Failed to checkout branch "+branch, map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
		}
		cmd = fmt.Sprintf("%s reset --hard %s/%s --", g.gitPath, remote, branch)
	} else if g.isRemoteBranch(dest, remote, version) {
		if depth != 0 && !g.isLocalBranch(dest, version) {
			g.setRemoteBranch(dest, remote, version, depth)
		}
		if !g.isLocalBranch(dest, version) {
			cmd = fmt.Sprintf("%s checkout --track -b %s %s/%s", g.gitPath, version, remote, version)
		} else {
			rc, out, errOut := g.runStr(fmt.Sprintf("%s checkout --force %s", g.gitPath, version), dest)
			if rc != 0 {
				g.failOnly("Failed to checkout branch "+version, map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
			}
			cmd = fmt.Sprintf("%s reset --hard %s/%s", g.gitPath, remote, version)
		}
	} else {
		cmd = fmt.Sprintf("%s checkout --force %s", g.gitPath, version)
	}
	rc, out, errOut := g.runStr(cmd, dest)
	if rc != 0 {
		what := "Failed to checkout " + version
		if version == "HEAD" {
			what = "Failed to checkout branch " + branch
		}
		g.failOnly(what, map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc), "cmd": cmd})
	}
	if verifyCommit {
		g.verifyCommitSign(dest, version, gpgAllowlist)
	}
}

func (g *gitRun) verifyCommitSign(dest, version string, allowlist []string) {
	sub := "verify-commit"
	for _, t := range g.getAnnotatedTags(dest) {
		if t == version {
			sub = "verify-tag"
		}
	}
	cmd := fmt.Sprintf("%s %s %s", g.gitPath, sub, version)
	if len(allowlist) > 0 {
		cmd += " --raw"
	}
	rc, out, errOut := g.runStr(cmd, dest)
	if rc != 0 {
		g.failOnly(fmt.Sprintf("Failed to verify GPG signature of commit/tag \"%s\"", version),
			map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
	}
	if len(allowlist) > 0 {
		fp := gpgFingerprint(errOut)
		for _, a := range allowlist {
			if a == fp {
				return
			}
		}
		g.failOnly(fmt.Sprintf("The gpg_allowlist does not include the public key \"%s\" for this commit", fp),
			map[string]any{"stdout": out, "stderr": errOut, "rc": int64(rc)})
	}
}

func gpgFingerprint(output string) string {
	for _, line := range pySplitLines(output) {
		data := strings.Fields(line)
		if len(data) < 2 || data[1] != "VALIDSIG" {
			continue
		}
		id := 2
		if len(data) == 11 {
			id = 10
		}
		if id < len(data) {
			return data[id]
		}
	}
	return "None"
}

func (g *gitRun) gitArchive(dest, archive, format string, prefix *string, version string) {
	cmd := []string{g.gitPath, "archive", "--format", format, "--output", archive}
	if prefix != nil {
		cmd = append(cmd, "--prefix", *prefix)
	}
	cmd = append(cmd, version)
	rc, _, errOut := g.run(cmd, dest)
	if rc != 0 {
		g.failOnly("Failed to perform archive operation", map[string]any{
			"details": fmt.Sprintf("Git archive command failed to create archive %s using %s directory.Error: %s", archive, dest, errOut)})
	}
}

func (g *gitRun) createArchive(dest, archive string, prefix *string, version, repo string) {
	formats := map[string]string{".zip": "zip", ".gz": "tar.gz", ".tar": "tar", ".tgz": "tgz"}
	format, ok := formats[pySplitExtOnly(archive)]
	if !ok {
		g.failOnly("Unable to get file extension from archive file name : "+archive, map[string]any{
			"details": "Please specify archive as filename with extension. File extension can be one of ['tar', 'tar.gz', 'zip', 'tgz']"})
	}
	parts := strings.Split(repo, "/")
	repoName := strings.ReplaceAll(parts[len(parts)-1], ".git", "")
	if pathExists(archive) {
		tmp, err := os.MkdirTemp("", "")
		if err != nil {
			g.failOnly(pyOSErrorStr(err), nil)
		}
		defer os.RemoveAll(tmp)
		newArchive := pyJoin(tmp, repoName) + "." + format
		g.gitArchive(dest, newArchive, format, prefix, version)
		a, _ := os.ReadFile(newArchive)
		b, _ := os.ReadFile(archive)
		if bytes.Equal(a, b) {
			g.result["changed"] = false
			return
		}
		if err := shutilMove(newArchive, archive); err != nil {
			g.failOnly(fmt.Sprintf("Failed to move %s to %s", newArchive, archive),
				map[string]any{"details": "Error occurred while moving : " + pyOSErrorStr(err)})
		}
		g.result["changed"] = true
		return
	}
	g.gitArchive(dest, archive, format, prefix, version)
	g.result["changed"] = true
}

// shutilMove is shutil.move: a rename, else a copy and delete.
func shutilMove(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errorsIsEXDEV(err) {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := fsutil.CopyFile(src, dst, true); err != nil {
			return err
		}
		return os.Remove(src)
	}
	err = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		}
		return fsutil.CopyFile(p, target, true)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func errorsIsEXDEV(err error) bool {
	var errno syscall.Errno
	if le, ok := err.(*os.LinkError); ok {
		errno, _ = le.Err.(syscall.Errno)
	}
	return errno == syscall.EXDEV
}

// pySplitExtOnly is os.path.splitext(p)[1].
func pySplitExtOnly(p string) string {
	base := p[strings.LastIndexByte(p, '/')+1:]
	i := strings.LastIndexByte(base, '.')
	if i <= 0 || strings.Trim(base[:i], ".") == "" {
		return ""
	}
	return base[i:]
}
