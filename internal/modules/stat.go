package modules

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(statModule, "stat", "ansible.builtin.stat")
}

var statSpec = args.Spec{
	"path":                {Required: true, Aliases: []string{"dest", "name"}},
	"follow":              {Type: "bool", Default: false},
	"get_checksum":        {Type: "bool", Default: true},
	"get_mime":            {Type: "bool", Default: true, Aliases: []string{"mime", "mime_type", "mime-type"}},
	"get_attributes":      {Type: "bool", Default: true, Aliases: []string{"attr", "attributes"}},
	"get_selinux_context": {Type: "bool", Default: false},
	"checksum_algorithm": {Default: "sha1", Aliases: []string{"checksum", "checksum_algo"},
		Choices: []string{"md5", "sha1", "sha224", "sha256", "sha384", "sha512"}},
	// Accepted from the action plugins' remote stat calls.
	"get_size": {Type: "bool"},
}

// fileAttributeNames is FILE_ATTRIBUTES (lsattr flag letters).
var fileAttributeNames = map[rune]string{
	'A': "noatime", 'a': "append", 'c': "compressed", 'C': "nocow", 'd': "nodump",
	'D': "dirsync", 'e': "extents", 'E': "encrypted", 'h': "blocksize", 'i': "immutable",
	'I': "indexed", 'j': "journalled", 'N': "inline", 's': "zero", 'S': "synchronous",
	't': "notail", 'T': "blockroot", 'u': "undelete", 'X': "compressedraw", 'Z': "compresseddirty",
}

// statModule ports ansible.builtin.stat, including the platform-dependent
// keys Python's os.stat_result exposes.
func statModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := statSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := pyExpandPath(p.Str("path"))

	statFn := os.Lstat
	if p.Bool("follow") {
		statFn = os.Stat
	}
	info, err := statFn(path)
	if err != nil {
		var errno syscall.Errno
		if errors.Is(err, fs.ErrNotExist) && errors.As(err, &errno) && errno == syscall.ENOENT {
			return &agentproto.Result{Extra: map[string]any{"stat": map[string]any{"exists": false}}}
		}
		if errors.As(err, &errno) {
			return &agentproto.Result{Failed: true, Msg: pyStrerror(errno), Cause: pyOSError(err)}
		}
		return agentproto.Fail("%v", err)
	}

	mode := info.Mode()
	bits := sIMode(mode)
	uid, gid, _ := statIDsOf(info)
	mtime, atime := statTimes(info)
	st := map[string]any{
		"exists": true,
		"path":   path,
		"mode":   fmt.Sprintf("%04o", bits),
		"isdir":  mode.IsDir(),
		"ischr":  mode&os.ModeCharDevice != 0,
		"isblk":  mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0,
		"isreg":  mode.IsRegular(),
		"isfifo": mode&os.ModeNamedPipe != 0,
		"islnk":  mode&os.ModeSymlink != 0,
		"issock": mode&os.ModeSocket != 0,
		"uid":    int64(uid),
		"gid":    int64(gid),
		"size":   info.Size(),
		"inode":  statInode(info),
		"dev":    statDev(info),
		"nlink":  int64(nlinkOf(info)),
		"atime":  atime,
		"mtime":  mtime,
		"ctime":  statCtime(info),
		"wusr":   bits&0o200 != 0, "rusr": bits&0o400 != 0, "xusr": bits&0o100 != 0,
		"wgrp": bits&0o020 != 0, "rgrp": bits&0o040 != 0, "xgrp": bits&0o010 != 0,
		"woth": bits&0o002 != 0, "roth": bits&0o004 != 0, "xoth": bits&0o001 != 0,
		"isuid": bits&0o4000 != 0, "isgid": bits&0o2000 != 0,
	}
	for k, v := range statPlatform(info) {
		st[k] = v
	}
	st["readable"] = accessOK(path, 4)
	st["writeable"] = accessOK(path, 2)
	st["executable"] = accessOK(path, 1)
	if st["islnk"] == true {
		st["lnk_source"] = pyRealpath(path)
		if target, err := os.Readlink(path); err == nil {
			st["lnk_target"] = target
		}
	}
	if name := userName(uid); name != fmt.Sprint(uid) {
		st["pw_name"] = name
	}
	if name := groupName(gid); name != fmt.Sprint(gid) {
		st["gr_name"] = name
	}
	if st["isreg"] == true && st["readable"] == true && p.Bool("get_checksum") {
		if sum, err := digestFile(path, newHash(p.Str("checksum_algorithm"))); err == nil {
			st["checksum"] = sum
		}
	}
	if p.Bool("get_mime") {
		st["mimetype"], st["charset"] = "unknown", "unknown"
		if bin, err := lookPath("file"); err == nil {
			if out, err := exec.Command(bin, "--mime-type", "--mime-encoding", path).Output(); err == nil {
				s := string(out)
				if i := strings.LastIndex(s, ":"); i >= 0 {
					if mt, cs, ok := strings.Cut(s[i+1:], ";"); ok {
						st["mimetype"] = strings.TrimSpace(mt)
						if _, v, ok := strings.Cut(cs, "="); ok {
							st["charset"] = strings.TrimSpace(v)
						}
					}
				}
			}
		}
	}
	if p.Bool("get_attributes") {
		st["version"], st["attributes"], st["attr_flags"] = nil, []any{}, ""
		for k, v := range fileAttributes(path) {
			st[k] = v
		}
	}
	if p.Bool("get_selinux_context") {
		st["selinux_context"] = []any{nil, nil, nil, nil}
	}
	return &agentproto.Result{Extra: map[string]any{"stat": st}}
}

func newHash(algo string) hash.Hash {
	switch algo {
	case "md5":
		return md5.New()
	case "sha224":
		return sha256.New224()
	case "sha256":
		return sha256.New()
	case "sha384":
		return sha512.New384()
	case "sha512":
		return sha512.New()
	}
	return sha1.New()
}

// fileAttributes is AnsibleModule.get_file_attributes: lsattr -vd, when
// lsattr exists and succeeds.
func fileAttributes(path string) map[string]any {
	bin, err := lookPath("lsattr")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin, "-vd", path).Output()
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return nil
	}
	flags := strings.ReplaceAll(fields[1], "-", "")
	names := []any{}
	for _, r := range flags {
		if n, ok := fileAttributeNames[r]; ok {
			names = append(names, n)
		}
	}
	return map[string]any{"version": fields[0], "attr_flags": flags, "attributes": names}
}
