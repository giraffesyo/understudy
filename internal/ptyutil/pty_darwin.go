package ptyutil

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Open opens a pseudo-terminal pair: /dev/ptmx granted and unlocked
// (grantpt/unlockpt), and the slave's name (ptsname).
func Open() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctl(master.Fd(), syscall.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("grantpt: %w", err)
	}
	if err := ioctl(master.Fd(), syscall.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	var name [128]byte
	if err := ioctl(master.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("ptsname: %w", err)
	}
	return master, string(name[:bytes.IndexByte(name[:], 0)]), nil
}
