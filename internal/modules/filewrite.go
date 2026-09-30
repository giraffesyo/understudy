package modules

import (
	"bytes"
	"os"

	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// writeIfDifferent is the sha256-compare + atomic_move +
// set_mode_if_different sequence the repository modules share. content is
// written only when it differs (an existing file keeps its owner and
// mode); mode (nil = leave alone) is then enforced. In check mode nothing
// is touched and the would-be change is reported.
func writeIfDifferent(check bool, path string, content []byte, mode any) (bool, error) {
	changed := false
	cur, err := os.ReadFile(path)
	exists := err == nil
	if !exists || !bytes.Equal(cur, content) {
		changed = true
		if !check {
			if err := fsutil.AtomicRewrite(path, bytes.NewReader(content), os.FileMode(0o666&^fsutil.Umask())); err != nil {
				return changed, err
			}
		}
	}
	if mode == nil {
		return changed, nil
	}
	if check {
		if !exists {
			return true, nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return changed, err
		}
		want, err := fsutil.ResolveMode(mode, info.Mode())
		if err != nil {
			return changed, err
		}
		return changed || fsutil.UnixBits(want) != fsutil.UnixBits(info.Mode()), nil
	}
	c, err := fsutil.ApplyFileAttrs(path, mode, "", "", false)
	return changed || c, err
}
