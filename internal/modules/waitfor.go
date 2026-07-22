package modules

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(waitForModule, "wait_for", "ansible.builtin.wait_for")
}

var waitForSpec = args.Spec{
	"host":    {Default: "127.0.0.1"},
	"port":    {Type: "int"},
	"path":    {},
	"state":   {Default: "started", Choices: []string{"started", "stopped", "present", "absent", "drained"}},
	"timeout": {Type: "int", Default: 300},
	"delay":   {Type: "int", Default: 0},
	"sleep":   {Type: "int", Default: 1},
	"msg":     {},
}

// waitForModule polls for a port or file condition. Runs on the target.
func waitForModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := waitForSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	port := p.Int("port")
	path := p.Str("path")
	state := p.Str("state")
	if port == 0 && path == "" {
		// Bare wait_for with only timeout acts as a sleep.
		if env.CheckMode {
			return &agentproto.Result{}
		}
		time.Sleep(time.Duration(p.Int("timeout")) * time.Second)
		return &agentproto.Result{}
	}
	if env.CheckMode {
		return &agentproto.Result{Msg: "check mode: not waiting"}
	}
	if d := p.Int("delay"); d > 0 {
		time.Sleep(time.Duration(d) * time.Second)
	}

	check := func() bool {
		if port != 0 {
			addr := net.JoinHostPort(p.Str("host"), fmt.Sprintf("%d", port))
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			up := err == nil
			if conn != nil {
				conn.Close()
			}
			if state == "stopped" || state == "drained" {
				return !up
			}
			return up
		}
		_, err := os.Stat(path)
		if state == "absent" {
			return os.IsNotExist(err)
		}
		return err == nil
	}

	deadline := time.Now().Add(time.Duration(p.Int("timeout")) * time.Second)
	sleep := time.Duration(p.Int("sleep")) * time.Second
	if sleep <= 0 {
		sleep = time.Second
	}
	start := time.Now()
	for {
		if check() {
			return &agentproto.Result{Extra: map[string]any{
				"elapsed": int(time.Since(start).Seconds()),
			}}
		}
		if time.Now().After(deadline) {
			msg := p.Str("msg")
			if msg == "" {
				what := path
				if port != 0 {
					what = fmt.Sprintf("%s:%d", p.Str("host"), port)
				}
				msg = fmt.Sprintf("Timeout when waiting for %s (state=%s)", what, state)
			}
			return &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{
				"elapsed": int(time.Since(start).Seconds()),
			}}
		}
		time.Sleep(sleep)
	}
}
