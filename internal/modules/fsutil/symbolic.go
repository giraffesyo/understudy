package fsutil

import (
	"fmt"
	"os"
	"strings"
)

// Unix permission bits, spelled out so the symbolic-mode arithmetic below
// reads like Ansible's _symbolic_mode_to_octal, which it mirrors.
const (
	sISUID      = 0o4000
	sISGID      = 0o2000
	sISVTX      = 0o1000
	sIRWXU      = 0o700
	sIRWXG      = 0o070
	sIRWXO      = 0o007
	permBitsAll = 0o7777
)

// IsSymbolicMode reports whether v is a symbolic mode string ("u=rw,g=r").
func IsSymbolicMode(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(s)
	return s != "" && !isOctalString(strings.TrimPrefix(s, "0o")) && strings.ContainsAny(s, "+-=")
}

// ResolveMode is ParseMode for a path that already exists: symbolic modes
// are applied relative to current (which must carry os.ModeDir for
// directories, since 'X' depends on it). Numeric modes ignore current.
func ResolveMode(v any, current os.FileMode) (os.FileMode, error) {
	if !IsSymbolicMode(v) {
		return ParseMode(v)
	}
	n, err := applySymbolic(strings.TrimSpace(v.(string)), unixBits(current), current.IsDir(), processUmask())
	if err != nil {
		return 0, err
	}
	return permBits(n)
}

// SymbolicToOctal is AnsibleModule._symbolic_mode_to_octal: sym applied to
// the current st_mode permission bits (cur) of a file or directory.
func SymbolicToOctal(sym string, cur uint32, isDir bool) (uint32, error) {
	return applySymbolic(sym, cur, isDir, processUmask())
}

// applySymbolic follows Ansible's semantics: comma-separated clauses of
// [ugoa]*[-+=][rwxXstugo]*, possibly chained ("u+r-w"); an omitted user
// list means 'a' filtered through the umask.
func applySymbolic(sym string, mode uint32, isDir bool, umask uint32) (uint32, error) {
	mode &= permBitsAll
	for _, clause := range strings.Split(sym, ",") {
		i := strings.IndexAny(clause, "+-=")
		if i < 0 {
			if strings.Trim(clause, "ugoa") == "" {
				continue // users only, no operation: a no-op, as in Python
			}
			return 0, fmt.Errorf("bad symbolic permission for mode: %s", clause)
		}
		users := clause[:i]
		useUmask := users == ""
		if users == "" || users == "a" {
			users = "ugo"
		}
		if strings.Trim(users, "ugo") != "" {
			return 0, fmt.Errorf("bad symbolic permission for mode: %s", clause)
		}
		rest := clause[i:]
		for rest != "" {
			op := rest[0]
			j := strings.IndexAny(rest[1:], "+-=")
			perms := rest[1:]
			if j >= 0 {
				perms, rest = rest[1:1+j], rest[1+j:]
			} else {
				rest = ""
			}
			if strings.Trim(perms, "rwxXstugo") != "" {
				return 0, fmt.Errorf("bad symbolic permission for mode: %s", clause)
			}
			for _, u := range users {
				apply := symbolicPermBits(byte(u), perms, mode, isDir, useUmask, umask)
				mode = applyOperation(byte(u), op, apply, mode)
			}
		}
	}
	return mode, nil
}

func symbolicPermBits(user byte, perms string, prev uint32, isDir, useUmask bool, umask uint32) uint32 {
	shift := map[byte]uint{'u': 6, 'g': 3, 'o': 0}[user]
	rev := uint32(permBitsAll)
	if useUmask {
		rev = umask ^ permBitsAll
	}
	var out uint32
	for i := 0; i < len(perms); i++ {
		switch perms[i] {
		case 'r':
			out |= rev & (0o4 << shift)
		case 'w':
			out |= rev & (0o2 << shift)
		case 'x':
			out |= rev & (0o1 << shift)
		case 'X':
			if isDir || prev&0o111 != 0 {
				out |= 0o1 << shift
			}
		case 's':
			out |= map[byte]uint32{'u': sISUID, 'g': sISGID}[user]
		case 't':
			if user == 'o' {
				out |= sISVTX
			}
		case 'u', 'g', 'o':
			// Copy another class's rwx bits into this class.
			src := map[byte]uint{'u': 6, 'g': 3, 'o': 0}[perms[i]]
			out |= ((prev >> src) & 0o7) << shift
		}
	}
	return out
}

func applyOperation(user, op byte, apply, mode uint32) uint32 {
	switch op {
	case '=':
		mask := map[byte]uint32{'u': sIRWXU | sISUID, 'g': sIRWXG | sISGID, 'o': sIRWXO | sISVTX}[user]
		return (mode &^ mask) | apply
	case '+':
		return mode | apply
	default: // '-'
		return mode &^ apply
	}
}

// unixBits converts a FileMode to numeric st_mode permission bits.
func unixBits(mode os.FileMode) uint32 {
	n := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		n |= sISUID
	}
	if mode&os.ModeSetgid != 0 {
		n |= sISGID
	}
	if mode&os.ModeSticky != 0 {
		n |= sISVTX
	}
	return n
}
