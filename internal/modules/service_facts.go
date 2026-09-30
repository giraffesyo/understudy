package modules

import (
	"os"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports ansible.builtin.service_facts for Linux: the SysV
// `service --status-all` scan and the systemd list-units/list-unit-files
// scan (upstart, chkconfig and OpenRC hosts are not covered yet).

func init() {
	names := []string{"service_facts", "ansible.builtin.service_facts"}
	Register(serviceFactsModule, names...)
	for _, n := range names {
		specs[n] = args.Spec{}
	}
}

var sysvStatusRe = regexp.MustCompile(`(?m)^\s*\[ (\+|\-) \]\s+(.+)$`)

func serviceFactsModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	if _, err := (args.Spec{}).Parse(raw); err != nil {
		return agentproto.Fail("%v", err)
	}
	loc := localeEnv(bestParsableLocale(env))
	run := func(argv ...string) (int, string, string) {
		return runCommand(env, argv, cmdOpts{Env: loc})
	}
	var warnings []any
	services := map[string]any{}

	// ServiceScanService: SysV via `service --status-all` when neither
	// chkconfig nor rc-status exists.
	servicePath, _ := lookPath("service")
	_, chkErr := lookPath("chkconfig")
	_, rcErr := lookPath("rc-status")
	if servicePath != "" && chkErr != nil && rcErr != nil {
		rc, out, stderr := run(servicePath, "--status-all")
		skip := false
		if rc == 4 {
			if _, err := os.Stat("/etc/init.d"); err != nil {
				skip = true
			}
		}
		if !skip {
			if rc != 0 {
				warnings = append(warnings, "Unable to query 'service' tool ("+itoa(rc)+"): "+stderr)
			}
			for _, m := range sysvStatusRe.FindAllStringSubmatch(out, -1) {
				state := "stopped"
				if m[1] == "+" {
					state = "running"
				}
				name := strings.TrimRight(m[2], "\r")
				services[name] = map[string]any{"name": name, "state": state, "source": "sysv"}
			}
		}
	}

	// SystemctlScanService.
	if systemdManaged() {
		if systemctl, err := lookPath("systemctl"); err == nil {
			bad := []string{"not-found", "masked", "failed"}
			rc, out, stderr := run(systemctl, "list-units", "--no-pager", "--type", "service", "--all", "--plain")
			if rc != 0 {
				warnings = append(warnings, "Could not list units from systemd: "+stderr)
			} else {
				for _, line := range strings.Split(out, "\n") {
					if !strings.Contains(line, ".service") {
						continue
					}
					f := strings.Fields(line)
					if len(f) < 4 {
						continue
					}
					status := f[2]
					for _, b := range bad {
						if containsString(f[:len(f)-1], b) {
							status = b
							break
						}
					}
					state := "stopped"
					if f[3] == "running" {
						state = "running"
					}
					services[f[0]] = map[string]any{"name": f[0], "state": state, "status": status, "source": "systemd"}
				}
			}
			rc, out, stderr = run(systemctl, "list-unit-files", "--no-pager", "--type", "service", "--all")
			if rc != 0 {
				warnings = append(warnings, "Could not get unit files data from systemd: "+stderr)
			} else {
				for _, line := range strings.Split(out, "\n") {
					if !strings.Contains(line, ".service") {
						continue
					}
					f := strings.Fields(line)
					if len(f) < 2 {
						return agentproto.Fail("Malformed output discovered from systemd list-unit-files: %s", line)
					}
					name, status := f[0], f[1]
					if cur, ok := services[name].(map[string]any); ok {
						if s, _ := cur["status"].(string); !containsString(bad, s) {
							cur["status"] = status
						}
						continue
					}
					state := "unknown"
					if rc, so, _ := run(systemctl, "show", name, "--property=ActiveState"); rc == 0 && so != "" {
						state = strings.TrimRight(strings.Replace(so, "ActiveState=", "", 1), " \t\r\n")
					}
					services[name] = map[string]any{"name": name, "state": state, "status": status, "source": "systemd"}
				}
			}
		}
	}

	res := &agentproto.Result{Extra: map[string]any{}}
	if len(warnings) > 0 {
		res.Extra["warnings"] = warnings
	}
	if len(services) == 0 {
		res.Skipped = true
		res.Msg = "Failed to find any services. This can be due to privileges or some other configuration issue."
		return res
	}
	res.AnsibleFacts = map[string]any{"services": services}
	return res
}

// systemdManaged is module_utils.service.is_systemd_managed.
func systemdManaged() bool {
	if _, err := lookPath("systemctl"); err != nil {
		return false
	}
	for _, c := range []string{"/run/systemd/system/", "/dev/.run/systemd/", "/dev/.systemd/"} {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	b, err := os.ReadFile("/proc/1/comm")
	return err == nil && strings.TrimSpace(string(b)) == "systemd"
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
