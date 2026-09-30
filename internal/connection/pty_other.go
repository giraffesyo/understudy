//go:build !linux && !darwin

package connection

import (
	"context"
	"fmt"
	"runtime"
)

func startLocalPTY(context.Context, string) (ptyProcess, error) {
	return nil, fmt.Errorf("su/doas become on a local connection needs a pseudo-terminal, which is not implemented on %s", runtime.GOOS)
}
