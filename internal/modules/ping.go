package modules

import "github.com/giraffesyo/understudy/internal/agentproto"

func init() {
	Register(pingModule, "ping", "ansible.builtin.ping")
}

// pingModule mirrors Ansible's ping: returns pong, or crashes on demand
// (data=crash) for exception-path testing.
func pingModule(env *RunEnv, args map[string]any) *agentproto.Result {
	data := "pong"
	if v, ok := argString(args, "data"); ok {
		if v == "crash" {
			panic("boom") // matches ansible.builtin.ping's testing hook
		}
		data = v
	}
	return &agentproto.Result{Extra: map[string]any{"ping": data}}
}
