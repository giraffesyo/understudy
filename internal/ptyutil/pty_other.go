//go:build !linux && !darwin

package ptyutil

import (
	"fmt"
	"os"
	"runtime"
)

// Open is not implemented off Linux and macOS.
func Open() (*os.File, string, error) {
	return nil, "", fmt.Errorf("pseudo-terminals are not implemented on %s", runtime.GOOS)
}
