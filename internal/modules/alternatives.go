package modules

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports community.general.alternatives over update-alternatives
// (dpkg's on Debian, chkconfig's alternatives symlink on RHEL): parse
// `--display`, then --install / --set / --auto / --remove as needed.

func init() {
	names := []string{"alternatives", "community.general.alternatives"}
	Register(alternativesModule, names...)
	for _, n := range names {
		specs[n] = alternativesSpec
	}
}

var alternativesSpec = args.Spec{
	"name":        {Required: true},
	"path":        {},
	"family":      {},
	"link":        {},
	"priority":    {Type: "int"},
	"state":       {Default: "selected", Choices: []string{"present", "selected", "absent", "auto"}},
	"subcommands": {Type: "list", Aliases: []string{"slaves"}},
}

var (
	altModeRe       = regexp.MustCompile(`(?m)\s-\s(?:status\sis\s)?(\w*)(?:\smode|.)$`)
	altPathRe       = regexp.MustCompile(`(?m)^\s*link currently points to (.*)$`)
	altLinkRe       = regexp.MustCompile(`(?m)^\s*link \w+ is (.*)$`)
	altSubPathRe    = regexp.MustCompile(`(?m)^\s*(?:slave|follower) (\S+) is (.*)$`)
	altAlternRe     = regexp.MustCompile(`(?m)^(\/.*)\s-\s(?:family\s(\S+)\s)?priority\s(\d+)((?:\s+(?:slave|follower).*)*)`)
	altSubcommandRe = regexp.MustCompile(`(?m)^\s+(?:slave|follower) (.*): (.*)$`)
)

// altSub is one subcommand dict: name, path, link (link may be None).
type altSub struct {
	name, path string
	link       *string
}

func (s altSub) dict() map[string]any {
	d := map[string]any{"name": s.name, "path": s.path, "link": nil}
	if s.link != nil {
		d["link"] = *s.link
	}
	return d
}

func (s altSub) equal(o altSub) bool {
	return s.name == o.name && s.path == o.path && (s.link == nil) == (o.link == nil) &&
		(s.link == nil || *s.link == *o.link)
}

type altEntry struct {
	priority int64
	family   string
	subs     []altSub
}

type altRun struct {
	env      *RunEnv
	p        *args.Parsed
	bin      string
	name     string
	path     *string
	family   *string
	subsArg  []altSub // nil when subcommands was not given
	hasSubs  bool
	mode     string
	curPath  *string
	curLink  *string
	current  map[string]*altEntry
	order    []string
	messages []string
	changed  bool
	before   map[string]any
	after    map[string]any
}

func subsDicts(subs []altSub) []any {
	out := make([]any, len(subs))
	for i, s := range subs {
		out[i] = s.dict()
	}
	return out
}

func (a *altRun) link() any {
	if l := a.p.Str("link"); l != "" {
		return pyExpandPath(l)
	}
	if a.curLink != nil {
		return *a.curLink
	}
	return nil
}

func (a *altRun) priority() int64 {
	if a.p.Has("priority") {
		return a.p.Int("priority")
	}
	if a.path != nil {
		if e, ok := a.current[*a.path]; ok {
			return e.priority
		}
	}
	return 50
}

// subcommands is the module's subcommands property.
func (a *altRun) subcommands() []altSub {
	if a.hasSubs {
		return a.subsArg
	}
	if a.path != nil {
		if e, ok := a.current[*a.path]; ok && len(e.subs) > 0 {
			return e.subs
		}
	}
	return nil
}

func (a *altRun) runChecked(argv []string) *agentproto.Result {
	if a.env.CheckMode {
		return nil
	}
	rc, out, errOut := runCommand(a.env, argv, cmdOpts{Env: map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}})
	if rc != 0 {
		return checkRCFail(argv, rc, out, errOut)
	}
	return nil
}

// checkRCFail is run_command(check_rc=True)'s fail_json.
func checkRCFail(argv []string, rc int, out, errOut string) *agentproto.Result {
	quoted := make([]string, len(argv))
	for i, s := range argv {
		quoted[i] = shQuote(s)
	}
	res := agentproto.Fail("%s", strings.TrimRight(errOut, " \t\n\r\v\f"))
	res.RC = &rc
	res.Stdout, res.Stderr = out, errOut
	res.Extra = map[string]any{"cmd": strings.Join(quoted, " ")}
	return res
}

func (a *altRun) parse() {
	a.current = map[string]*altEntry{}
	rc, out, _ := runCommand(a.env, []string{a.bin, "--display", a.name},
		cmdOpts{Env: map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}})
	if rc != 0 {
		return
	}
	m := altModeRe.FindStringSubmatch(out)
	if m == nil {
		return
	}
	a.mode = m[1]
	if m := altPathRe.FindStringSubmatch(out); m != nil {
		a.curPath = &m[1]
	}
	if m := altLinkRe.FindStringSubmatch(out); m != nil {
		a.curLink = &m[1]
	}
	links := map[string]string{}
	for _, m := range altSubPathRe.FindAllStringSubmatch(out, -1) {
		links[m[1]] = m[2]
	}
	if len(links) == 0 {
		for _, s := range a.subcommands() {
			if s.link != nil {
				links[s.name] = *s.link
			}
		}
	}
	for _, m := range altAlternRe.FindAllStringSubmatch(out, -1) {
		prio, _ := strconv.ParseInt(m[3], 10, 64)
		e := &altEntry{priority: prio, family: m[2]}
		for _, sm := range altSubcommandRe.FindAllStringSubmatch(m[4], -1) {
			if sm[2] == "(null)" {
				continue
			}
			s := altSub{name: sm[1], path: sm[2]}
			if l, ok := links[sm[1]]; ok {
				s.link = &l
			}
			e.subs = append(e.subs, s)
		}
		if _, seen := a.current[m[1]]; !seen {
			a.order = append(a.order, m[1])
		}
		a.current[m[1]] = e
	}
	if a.env.DiffMode {
		if e, ok := a.pathEntry(); ok {
			a.before["state"] = "present"
			a.before["path"] = *a.path
			a.before["priority"] = e.priority
			a.before["link"] = strPtrAny(a.curLink)
			if len(e.subs) > 0 {
				a.before["subcommands"] = subsDicts(e.subs)
			}
			if a.mode == "manual" && (a.curPath == nil || *a.curPath != *a.path) {
				a.before["state"] = "selected"
			}
		} else {
			a.before["state"] = "absent"
		}
	}
}

func strPtrAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (a *altRun) pathEntry() (*altEntry, bool) {
	if a.path == nil {
		return nil, false
	}
	e, ok := a.current[*a.path]
	return e, ok
}

func (a *altRun) install() *agentproto.Result {
	if !pathExists(*a.path) {
		return agentproto.Fail("Specified path %s does not exist", *a.path)
	}
	link, _ := a.link().(string)
	if link == "" {
		return agentproto.Fail("Needed to install the alternative, but unable to do so as we are missing the link")
	}
	prio := a.priority()
	cmd := []string{a.bin, "--install", link, a.name, *a.path, strconv.FormatInt(prio, 10)}
	if a.family != nil {
		cmd = append(cmd, "--family", *a.family)
	}
	if a.hasSubs {
		for _, s := range a.subcommands() {
			l := ""
			if s.link != nil {
				l = *s.link
			}
			cmd = append(cmd, "--slave", l, s.name, s.path)
		}
	}
	a.changed = true
	a.messages = append(a.messages, "Install alternative '"+*a.path+"' for '"+a.name+"'.")
	if fail := a.runChecked(cmd); fail != nil {
		return fail
	}
	if a.env.DiffMode {
		a.after = map[string]any{"state": "present", "path": *a.path, "family": strPtrAny(a.family),
			"priority": prio, "link": link}
		if subs := a.subcommands(); len(subs) > 0 {
			a.after["subcommands"] = subsDicts(subs)
		}
	}
	return nil
}

func alternativesModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := alternativesSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	a := &altRun{env: env, p: p, name: p.Str("name"), before: map[string]any{}, after: map[string]any{}}
	if !p.Has("path") && !p.Has("family") {
		return agentproto.Fail("one of the following is required: path, family")
	}
	if p.Has("path") {
		v := pyExpandPath(p.Str("path"))
		a.path = &v
	}
	if p.Has("family") {
		v := p.Str("family")
		a.family = &v
	}
	if p.Has("subcommands") {
		a.hasSubs = true
		for i, item := range p.List("subcommands") {
			d, ok := item.(map[string]any)
			if !ok {
				return agentproto.Fail("Elements value for option 'subcommands' is of type %s and we were unable to convert to dict: dictionary requested, could not parse JSON or key=value", pyTypeName(item))
			}
			var missing, unknown []string
			for k := range d {
				if k != "name" && k != "path" && k != "link" {
					unknown = append(unknown, k)
				}
			}
			for _, k := range []string{"link", "name", "path"} {
				if v, ok := d[k]; !ok || v == nil {
					missing = append(missing, k)
				}
			}
			if len(unknown) > 0 {
				return agentproto.Fail("Unsupported parameters for (alternatives) module: subcommands.%s. Supported parameters include: link, name, path.", strings.Join(unknown, ", subcommands."))
			}
			if len(missing) > 0 {
				return agentproto.Fail("missing required arguments: %s found in subcommands", strings.Join(missing, ", "))
			}
			_ = i
			str := func(v any) string { s, _ := argString(map[string]any{"v": v}, "v"); return s }
			link := pyExpandPath(str(d["link"]))
			a.subsArg = append(a.subsArg, altSub{name: str(d["name"]), path: pyExpandPath(str(d["path"])), link: &link})
		}
	}
	bin, err := getBinPath("update-alternatives")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	a.bin = bin
	a.parse()

	state := p.Str("state")
	if state != "absent" {
		e, installed := a.pathEntry()
		need := a.path != nil && !installed
		if a.path != nil && installed {
			if p.Has("priority") && e.priority != p.Int("priority") {
				need = true
			}
			if a.hasSubs {
				for _, s := range e.subs {
					if !altSubIn(s, a.subsArg) {
						need = true
					}
				}
				for _, s := range a.subsArg {
					if !altSubIn(s, e.subs) {
						need = true
					}
				}
			}
		}
		if need {
			if fail := a.install(); fail != nil {
				return fail
			}
		}
		samePath := a.path != nil && a.curPath != nil && *a.curPath == *a.path
		sameFamily := false
		if a.curPath != nil {
			if ce, ok := a.current[*a.curPath]; ok {
				sameFamily = a.family != nil && ce.family == *a.family
			}
		}
		if state == "selected" && !(samePath || sameFamily) {
			arg := ""
			if a.path == nil {
				arg = *a.family
			} else {
				arg = *a.path
			}
			a.changed = true
			a.messages = append(a.messages, "Set alternative '"+arg+"' for '"+a.name+"'.")
			if fail := a.runChecked([]string{a.bin, "--set", a.name, arg}); fail != nil {
				return fail
			}
			if env.DiffMode {
				a.after["state"] = "selected"
			}
		}
		if state == "auto" && a.mode == "manual" {
			a.messages = append(a.messages, "Set alternative to auto for '"+a.name+"'.")
			a.changed = true
			if fail := a.runChecked([]string{a.bin, "--auto", a.name}); fail != nil {
				return fail
			}
			if env.DiffMode {
				a.after["state"] = "present"
			}
		}
	} else if _, ok := a.pathEntry(); ok {
		a.changed = true
		a.messages = append(a.messages, "Remove alternative '"+*a.path+"' from '"+a.name+"'.")
		if fail := a.runChecked([]string{a.bin, "--remove", a.name, *a.path}); fail != nil {
			return fail
		}
		if env.DiffMode {
			a.after = map[string]any{"state": "absent"}
		}
	}
	return &agentproto.Result{Changed: a.changed,
		Diff:  map[string]any{"before": a.before, "after": a.after},
		Extra: map[string]any{"msg": strings.Join(a.messages, " ")}}
}

func altSubIn(s altSub, list []altSub) bool {
	for _, o := range list {
		if s.equal(o) {
			return true
		}
	}
	return false
}
