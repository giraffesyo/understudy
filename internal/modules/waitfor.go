package modules

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(waitForModule, "wait_for", "ansible.builtin.wait_for")
}

var waitForSpec = args.Spec{
	"host":            {Default: "127.0.0.1"},
	"timeout":         {Type: "int", Default: 300},
	"connect_timeout": {Type: "int", Default: 5},
	"delay":           {Type: "int", Default: 0},
	"port":            {Type: "int"},
	"active_connection_states": {Type: "list", Default: []any{
		"ESTABLISHED", "FIN_WAIT1", "FIN_WAIT2", "SYN_RECV", "SYN_SENT", "TIME_WAIT"}},
	"path":          {},
	"search_regex":  {},
	"state":         {Default: "started", Choices: []string{"absent", "drained", "present", "started", "stopped"}},
	"exclude_hosts": {Type: "list"},
	"sleep":         {Type: "int", Default: 1},
	"msg":           {},
}

// tcpStateIDs is get_connection_state_id(): /proc/net/tcp state codes.
var tcpStateIDs = map[string]string{
	"ESTABLISHED": "01", "SYN_SENT": "02", "SYN_RECV": "03",
	"FIN_WAIT1": "04", "FIN_WAIT2": "05", "TIME_WAIT": "06",
}

// waitForModule ports ansible.builtin.wait_for (no check mode support).
func waitForModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := waitForSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (wait_for) does not support check mode"}
	}
	host := p.Str("host")
	timeout := time.Duration(p.Int("timeout")) * time.Second
	connectTimeout := time.Duration(p.Int("connect_timeout")) * time.Second
	port := p.Int("port")
	state := p.Str("state")
	path := pyExpandPath(p.Str("path"))
	searchRegex := p.Str("search_regex")
	msg := p.Str("msg")
	sleep := time.Duration(p.Int("sleep")) * time.Second

	// invalid is a parameter error: its message stands even with msg set.
	invalid := func(m string) *agentproto.Result {
		return &agentproto.Result{Failed: true, Msg: m, Extra: map[string]any{"elapsed": int64(0)}}
	}
	// fail is a wait that ended badly: msg replaces the message.
	fail := func(m string, elapsed int64) *agentproto.Result {
		if msg != "" {
			m = msg
		}
		return &agentproto.Result{Failed: true, Msg: m, Extra: map[string]any{"elapsed": elapsed}}
	}
	var re *regexp.Regexp
	if p.Has("search_regex") {
		if e := pyRegexSyntaxError(searchRegex); e != "" {
			return agentproto.Fail("Invalid regular expression: %s", e)
		}
		compiled, err := compilePyPattern(searchRegex)
		if err != nil {
			return agentproto.Fail("Invalid regular expression: %v", err)
		}
		if re, err = regexp.Compile("(?m)" + compiled.String()); err != nil {
			return agentproto.Fail("Invalid regular expression: %v", err)
		}
	}
	if port != 0 && path != "" {
		return invalid("port and path parameter can not both be passed to wait_for")
	}
	if path != "" && state == "stopped" {
		return invalid("state=stopped should only be used for checking a port in the wait_for module")
	}
	if path != "" && state == "drained" {
		return invalid("state=drained should only be used for checking a port in the wait_for module")
	}
	if p.Has("exclude_hosts") && state != "drained" {
		return invalid("exclude_hosts should only be with state=drained")
	}
	var stateIDs []string
	for _, s := range p.List("active_connection_states") {
		id, ok := tcpStateIDs[pyStrValue(s)]
		if !ok {
			return invalid(fmt.Sprintf("unknown active_connection_state (%s) defined", pyStrValue(s)))
		}
		stateIDs = append(stateIDs, id)
	}

	start := time.Now()
	elapsed := func() int64 { return int64(time.Since(start).Seconds()) }
	if d := p.Int("delay"); d > 0 {
		time.Sleep(time.Duration(d) * time.Second)
	}
	end := start.Add(timeout)
	addr := net.JoinHostPort(host, strconv.FormatInt(port, 10))
	var groups []any
	groupdict := map[string]any{}
	record := func(s string, m []int) {
		for i, name := range re.SubexpNames()[1:] {
			var v any
			if m[2*(i+1)] >= 0 {
				v = s[m[2*(i+1)]:m[2*(i+1)+1]]
			}
			groups = append(groups, v)
			if name != "" {
				groupdict[name] = v
			}
		}
	}

	switch {
	case port == 0 && path == "" && state != "drained":
		time.Sleep(timeout)
	case state == "absent" || state == "stopped":
		for {
			if !time.Now().Before(end) {
				if port != 0 {
					return fail(fmt.Sprintf("Timeout when waiting for %s:%d to stop.", host, port), elapsed())
				}
				return fail(fmt.Sprintf("Timeout when waiting for %s to be absent.", path), elapsed())
			}
			if path != "" {
				if _, err := os.Stat(path); err != nil {
					break
				}
			} else if port != 0 {
				c, err := net.DialTimeout("tcp", addr, connectTimeout)
				if err != nil {
					break
				}
				c.Close()
			}
			time.Sleep(sleep)
		}
	case state == "started" || state == "present":
	wait:
		for {
			if !time.Now().Before(end) {
				switch {
				case port != 0 && re != nil:
					return fail(fmt.Sprintf("Timeout when waiting for search string %s in %s:%d", searchRegex, host, port), elapsed())
				case port != 0:
					return fail(fmt.Sprintf("Timeout when waiting for %s:%d", host, port), elapsed())
				case re != nil:
					return fail(fmt.Sprintf("Timeout when waiting for search string %s in %s", searchRegex, path), elapsed())
				default:
					return fail(fmt.Sprintf("Timeout when waiting for file %s", path), elapsed())
				}
			}
			if path != "" {
				if _, err := os.Stat(path); err != nil {
					var errno syscall.Errno
					if !errors.Is(err, os.ErrNotExist) && errors.As(err, &errno) {
						return fail(fmt.Sprintf("Failed to stat %s, %s", path, pyStrerror(errno)), elapsed())
					}
				} else {
					if re == nil {
						break wait
					}
					if data, err := os.ReadFile(path); err == nil {
						s := string(data)
						if m := re.FindStringSubmatchIndex(s); m != nil {
							record(s, m)
							break wait
						}
					}
				}
			} else if port != 0 {
				ct := connectTimeout
				if alt := time.Duration(math.Ceil(time.Until(end).Seconds())) * time.Second; alt < ct {
					ct = alt
				}
				if c, err := net.DialTimeout("tcp", addr, ct); err == nil {
					if re == nil {
						c.Close()
						break wait
					}
					var data []byte
					matched := false
					buf := make([]byte, 1024)
					for time.Now().Before(end) {
						c.SetReadDeadline(end)
						n, err := c.Read(buf)
						if n > 0 {
							data = append(data, buf[:n]...)
							// A port match records no groups.
							if re.Match(data) {
								matched = true
								break
							}
						}
						if err != nil {
							break
						}
					}
					c.Close()
					if matched {
						break wait
					}
				}
			}
			time.Sleep(sleep)
		}
	case state == "drained":
		if !p.Has("port") {
			// TCPConnectionInfo's int(module.params['port']) raises.
			return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"}
		}
		for {
			if !time.Now().Before(end) {
				return fail(fmt.Sprintf("Timeout when waiting for %s:%d to drain", host, port), elapsed())
			}
			n, fail := activeTCPConnections(p, host, port, stateIDs)
			if fail != nil {
				return fail
			}
			if n == 0 {
				break
			}
			time.Sleep(sleep)
		}
	}

	var portV, pathV, reV any
	if p.Has("port") {
		portV = port
	}
	if p.Has("path") {
		pathV = path
	}
	if p.Has("search_regex") {
		reV = searchRegex
	}
	if groups == nil {
		groups = []any{}
	}
	return &agentproto.Result{Extra: map[string]any{
		"state": state, "port": portV, "search_regex": reV, "match_groups": groups,
		"match_groupdict": groupdict, "path": pathV, "elapsed": elapsed(),
	}}
}

// activeTCPConnections is LinuxTCPConnectionInfo.get_active_connections_count:
// connections in the active states on host:port per /proc/net/tcp{,6},
// excluding peers in exclude_hosts. Elsewhere Ansible needs psutil.
func activeTCPConnections(p *args.Parsed, host string, port int64, stateIDs []string) (int, *agentproto.Result) {
	if runtime.GOOS != "linux" {
		res := agentproto.Fail("%s", missingRequiredLib("psutil", "", ""))
		res.Cause = "No module named 'psutil'" // the ImportError it carries
		return 0, res
	}
	ips, err := hostToProcHex(host)
	if err != nil {
		return 0, moduleCrash(err)
	}
	var exclude []string
	for _, h := range p.List("exclude_hosts") {
		x, err := hostToProcHex(pyStrValue(h))
		if err != nil {
			return 0, moduleCrash(err)
		}
		exclude = append(exclude, x...)
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	portHex := fmt.Sprintf("%04X", port)
	count := 0
	for fam, file := range map[string]string{"4": "/proc/net/tcp", "6": "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		matchAll := "4:00000000"
		if fam == "6" {
			matchAll = "6:00000000000000000000000000000000"
		}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 4 || f[1] == "local_address" || !has(stateIDs, f[3]) {
				continue
			}
			lip, lport, _ := strings.Cut(f[1], ":")
			if lport != portHex {
				continue
			}
			rip, _, _ := strings.Cut(f[2], ":")
			if has(exclude, fam+":"+rip) {
				continue
			}
			if has(ips, fam+":"+lip) || has(ips, matchAll) ||
				strings.HasPrefix(lip, "0000000000000000FFFF0000") && has(ips, "6:0000000000000000FFFF000000000000") {
				count++
			}
		}
	}
	return count, nil
}

// hostToProcHex is _convert_host_to_hex: the host's addresses (plus the
// IPv4-mapped IPv6 form of IPv4 ones) as /proc/net/tcp renders them,
// prefixed by family.
func hostToProcHex(host string) ([]string, error) {
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	var out []string
	conv := func(fam string, ip []byte) {
		var b strings.Builder
		for i := 0; i < len(ip); i += 4 {
			fmt.Fprintf(&b, "%08X", binary.LittleEndian.Uint32(ip[i:i+4]))
		}
		out = append(out, fam+":"+b.String())
	}
	for _, a := range addrs {
		if v4 := a.To4(); v4 != nil {
			conv("4", v4)
			conv("6", net.ParseIP("::ffff:"+v4.String()).To16())
		} else {
			conv("6", a.To16())
		}
	}
	return out, nil
}
