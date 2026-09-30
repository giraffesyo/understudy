package modules

import (
	"os"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports community.general.timezone for Linux: SystemdTimezone
// (timedatectl) and, when timedatectl is unusable (containers), the
// NosystemdTimezone name handling for Debian (/etc/timezone +
// dpkg-reconfigure) and RHEL (/etc/sysconfig/clock). hwclock without
// systemd is rejected.

func init() {
	names := []string{"timezone", "community.general.timezone"}
	Register(timezoneModule, names...)
	for _, n := range names {
		specs[n] = timezoneSpec
	}
}

var timezoneSpec = args.Spec{
	"hwclock": {Choices: []string{"local", "UTC"}, Aliases: []string{"rtc"}},
	"name":    {},
}

var timezoneRe = map[string]*regexp.Regexp{
	"hwclock": regexp.MustCompile(`(?m)^\s*RTC in local TZ\s*:\s*(\S+)`),
	"name":    regexp.MustCompile(`(?m)^\s*Time ?zone\s*:\s*(\S+)`),
}

func timezoneModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := timezoneSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if !p.Has("hwclock") && !p.Has("name") {
		return agentproto.Fail("one of the following is required: hwclock, name")
	}
	loc := map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}
	var msgs []string
	abort := func(msg string) *agentproto.Result {
		out := []string{"Error message:", msg}
		if len(msgs) > 0 {
			out = append(out, "Other message(s):")
			out = append(out, msgs...)
		}
		return agentproto.Fail("%s", strings.Join(out, "\n"))
	}
	tdc, err := lookPath("timedatectl")
	usable := err == nil
	if usable {
		if rc, _, _ := runCommand(env, []string{tdc}, cmdOpts{Env: loc}); rc != 0 {
			usable = false
		}
	}
	keys := []string{}
	planned := map[string]string{}
	for _, k := range []string{"hwclock", "name"} {
		if p.Has(k) {
			keys = append(keys, k)
			planned[k] = p.Str(k)
		}
	}
	if tz, ok := planned["name"]; ok {
		if st, err := os.Stat("/usr/share/zoneinfo/" + tz); err != nil || st.IsDir() {
			return abort(`given timezone "` + tz + `" is not available`)
		}
	}
	execute := func(argv []string, log bool) (string, *agentproto.Result) {
		rc, out, stderr := runCommand(env, argv, cmdOpts{Env: loc})
		if rc != 0 {
			msg := stderr
			if msg == "" {
				msg = out
			}
			res := agentproto.Fail("%s", strings.TrimSpace(msg))
			res.RC = &rc
			res.Stdout, res.Stderr = out, stderr
			return "", res
		}
		if log {
			msgs = append(msgs, "executed `"+strings.Join(argv, " ")+"`")
		}
		return out, nil
	}
	if !usable {
		return nosystemdTimezone(env, p, keys, planned, abort, execute, &msgs)
	}
	read := func() (map[string]string, *agentproto.Result) {
		status, fail := execute([]string{tdc, "status"}, false)
		if fail != nil {
			return nil, fail
		}
		vals := map[string]string{}
		for _, k := range keys {
			m := timezoneRe[k].FindStringSubmatch(status)
			if m == nil {
				return nil, abort("could not read " + k + " from timedatectl status")
			}
			v := m[1]
			if k == "hwclock" {
				if b, ok := parseBoolish(v); ok && b {
					v = "local"
				} else {
					v = "UTC"
				}
			}
			vals[k] = v
		}
		return vals, nil
	}
	before, fail := read()
	if fail != nil {
		return fail
	}
	after := planned
	if !env.CheckMode {
		for _, k := range keys {
			if before[k] == planned[k] {
				continue
			}
			v := planned[k]
			sub := "set-timezone"
			if k == "hwclock" {
				sub = "set-local-rtc"
				if v == "local" {
					v = "yes"
				} else {
					v = "no"
				}
			}
			if _, fail := execute([]string{tdc, sub, v}, true); fail != nil {
				return fail
			}
		}
		if after, fail = read(); fail != nil {
			return fail
		}
		for _, k := range keys {
			if after[k] != planned[k] {
				return abort("still not desired state, though changes have made - planned: " +
					pyDictStr(keys, planned) + ", after: " + pyDictStr(keys, after))
			}
		}
	}
	b, a := map[string]any{}, map[string]any{}
	changed := false
	for _, k := range keys {
		b[k], a[k] = before[k], after[k]
		if before[k] != after[k] {
			changed = true
		}
	}
	res := &agentproto.Result{Changed: changed, Diff: map[string]any{"before": b, "after": a}}
	if len(msgs) > 0 {
		res.Msg = strings.Join(msgs, "\n")
	}
	return res
}

func parseBoolish(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "yes", "true", "on", "1", "y", "t":
		return true, true
	case "no", "false", "off", "0", "n", "f":
		return false, true
	}
	return false, false
}

func pyDictStr(keys []string, m map[string]string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, "'"+k+"': '"+m[k]+"'")
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// nosystemdTimezone is NosystemdTimezone for the name key.
func nosystemdTimezone(env *RunEnv, p *args.Parsed, keys []string, planned map[string]string,
	abort func(string) *agentproto.Result,
	execute func([]string, bool) (string, *agentproto.Result), msgs *[]string) *agentproto.Result {
	if _, ok := planned["hwclock"]; ok {
		return abort("hwclock without systemd (timedatectl) is not supported by understudy")
	}
	tz := planned["name"]
	tzfile := "/usr/share/zoneinfo/" + tz
	confFile := "/etc/timezone"
	nameRe := regexp.MustCompile(`(?m)^(\S+)`)
	lineFormat := "%s\n"
	var update [][]string
	if _, err := lookPath("dpkg-reconfigure"); err == nil {
		update = [][]string{{"ln", "-sf", tzfile, "/etc/localtime"},
			{"dpkg-reconfigure", "--frontend", "noninteractive", "tzdata"}}
	} else {
		update = [][]string{{"cp", "--remove-destination", tzfile, "/etc/localtime"}}
		if _, err := lookPath("tzdata-update"); err == nil {
			if fi, err := os.Lstat("/etc/localtime"); err == nil && fi.Mode()&os.ModeSymlink == 0 {
				update = [][]string{{"tzdata-update"}}
			}
		}
		confFile = "/etc/sysconfig/clock"
		nameRe = regexp.MustCompile(`(?m)^ZONE\s*=\s*"?([^"\s]+)"?`)
		lineFormat = "ZONE=\"%s\"\n"
		if b, err := os.ReadFile(confFile); err == nil && regexp.MustCompile(`(?m)^TIMEZONE\s*=`).Match(b) {
			nameRe = regexp.MustCompile(`(?m)^TIMEZONE\s*=\s*"?([^"\s]+)"?`)
			lineFormat = "TIMEZONE=\"%s\"\n"
		}
	}
	get := func(phase string) (string, *agentproto.Result) {
		b, err := os.ReadFile(confFile)
		value := "n/a"
		if err != nil {
			if !os.IsNotExist(err) {
				return "", abort(`tried to configure name using a file "` + confFile + `", but could not read it`)
			}
		} else if m := nameRe.FindSubmatch(b); m != nil {
			value = string(m[1])
		} else if phase != "before" {
			return "", abort(`tried to configure name using a file "` + confFile + `", but could not find a valid value in it`)
		}
		if value == tz {
			if fi, err := os.Lstat("/etc/localtime"); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if _, err := os.Stat("/etc/localtime"); err == nil {
					target, _ := os.Readlink("/etc/localtime")
					if m := regexp.MustCompile(`(?:/(?:usr/share|etc)/zoneinfo/)(.*)`).FindStringSubmatch(target); m != nil {
						if m[1] != tz {
							value = m[1]
						}
					} else {
						value = "n/a"
					}
				} else {
					value = "n/a"
				}
			} else {
				a, err1 := os.ReadFile("/etc/localtime")
				b, err2 := os.ReadFile(tzfile)
				if err1 != nil || err2 != nil || string(a) != string(b) {
					return "n/a", nil
				}
			}
		}
		return value, nil
	}
	before, fail := get("before")
	if fail != nil {
		return fail
	}
	after := tz
	if !env.CheckMode {
		if before != tz {
			data, err := os.ReadFile(confFile)
			var lines []string
			if err == nil {
				lines = strings.SplitAfter(string(data), "\n")
				if len(lines) > 0 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
			} else if !os.IsNotExist(err) {
				return abort(`tried to configure name using a file "` + confFile + `", but could not read it`)
			}
			var matched []int
			for i, l := range lines {
				if nameRe.MatchString(l) {
					matched = append(matched, i)
				}
			}
			insert := 0
			if len(matched) > 0 {
				insert = matched[0]
			}
			for i := len(matched) - 1; i >= 0; i-- {
				lines = append(lines[:matched[i]], lines[matched[i]+1:]...)
			}
			line := strings.Replace(lineFormat, "%s", tz, 1)
			lines = append(lines[:insert], append([]string{line}, lines[insert:]...)...)
			if err := os.WriteFile(confFile, []byte(strings.Join(lines, "")), 0o644); err != nil {
				return abort(`tried to configure name using a file "` + confFile + `", but could not write to it`)
			}
			*msgs = append(*msgs, "Added 1 line and deleted "+itoa(len(matched))+" line(s) on "+confFile)
			for _, cmd := range update {
				if _, fail := execute(cmd, false); fail != nil {
					return fail
				}
			}
		}
		if after, fail = get("after"); fail != nil {
			return fail
		}
		if after != tz {
			return abort("still not desired state, though changes have made - planned: {'name': '" + tz + "'}, after: {'name': '" + after + "'}")
		}
	}
	res := &agentproto.Result{Changed: before != after,
		Diff: map[string]any{"before": map[string]any{"name": before}, "after": map[string]any{"name": after}}}
	if len(*msgs) > 0 {
		res.Msg = strings.Join(*msgs, "\n")
	}
	return res
}
