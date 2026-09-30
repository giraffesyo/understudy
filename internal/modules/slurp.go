package modules

import (
	"encoding/base64"
	"errors"
	"os"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports ansible.builtin.slurp.

func init() {
	names := []string{"slurp", "ansible.builtin.slurp"}
	Register(slurpModule, names...)
	for _, n := range names {
		specs[n] = slurpSpec
	}
}

var slurpSpec = args.Spec{
	"src":   {Required: true, Aliases: []string{"path"}},
	"armor": {Type: "bool", Default: true},
}

func slurpModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := slurpSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	source := pyExpandPath(p.Str("src"))
	data, err := os.ReadFile(source)
	if err != nil {
		var msg string
		switch {
		case errors.Is(err, syscall.ENOENT):
			msg = "File not found: " + source
		case errors.Is(err, syscall.EACCES):
			msg = "File is not readable: " + source
		case errors.Is(err, syscall.EISDIR):
			msg = "Source is a directory and must be a file: " + source
		default:
			msg = "Unable to slurp file: {source}"
		}
		return &agentproto.Result{Failed: true, Msg: msg, Cause: pyStrOSError(err, source)}
	}
	res := &agentproto.Result{Extra: map[string]any{"source": source}}
	if p.Bool("armor") {
		res.Extra["encoding"] = "base64"
		res.Extra["content"] = base64.StdEncoding.EncodeToString(data)
	} else {
		res.Extra["encoding"] = "utf-8"
		res.Extra["content"] = pyToText(data)
	}
	return res
}
