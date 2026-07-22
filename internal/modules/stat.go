package modules

import (
	"os"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(statModule, "stat", "ansible.builtin.stat")
}

var statSpec = args.Spec{
	"path":         {Required: true, Aliases: []string{"dest", "name"}},
	"follow":       {Type: "bool", Default: false},
	"get_checksum": {Type: "bool", Default: true},
}

func statModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := statSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")

	statFn := os.Lstat
	if p.Bool("follow") {
		statFn = os.Stat
	}
	info, err := statFn(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentproto.Result{Extra: map[string]any{
				"stat": map[string]any{"exists": false},
			}}
		}
		return agentproto.Fail("stat %s: %v", path, err)
	}

	st := map[string]any{
		"exists": true,
		"path":   path,
		"mode":   fsutil.ModeString(info.Mode()),
		"isdir":  info.IsDir(),
		"isreg":  info.Mode().IsRegular(),
		"islnk":  info.Mode()&os.ModeSymlink != 0,
		"size":   info.Size(),
		"mtime":  float64(info.ModTime().UnixNano()) / 1e9,
	}
	if st["islnk"] == true {
		if target, err := os.Readlink(path); err == nil {
			st["lnk_target"] = target
		}
	}
	if uid, gid, ok := statIDsOf(info); ok {
		st["uid"] = uid
		st["gid"] = gid
	}
	if p.Bool("get_checksum") && info.Mode().IsRegular() {
		if sum, err := fsutil.Sha256File(path); err == nil {
			st["checksum"] = sum
		}
	}
	return &agentproto.Result{Extra: map[string]any{"stat": st}}
}
