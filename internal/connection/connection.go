// Package connection provides transports for executing commands and placing
// files on managed hosts: ssh (with agent bootstrap) and local.
package connection

import (
	"context"
	"io"
	"time"
)

// ExecResult is the outcome of one remote command. A non-zero RC is NOT a Go
// error; a returned error means the transport itself failed (host unreachable).
type ExecResult struct {
	RC     int
	Stdout []byte
	Stderr []byte
}

// BecomeSpec describes privilege escalation for one execution.
type BecomeSpec struct {
	User     string  // default "root"
	Method   string  // sudo (default), su or doas
	Password string  // empty: rely on NOPASSWD (sudo -n, doas -n) / root su
	Exe      string  // become_exe: the escalation binary (default: the method)
	Flags    *string // become_flags (nil: the method's default, "-H -S -n" for sudo)
}

// ExecOptions modify one Exec call.
type ExecOptions struct {
	Stdin   io.Reader
	Become  *BecomeSpec // nil = no escalation
	Timeout time.Duration
}

// Connection executes commands and writes files on one host. WriteFile
// exists for bootstrap (before the agent is present); regular file placement
// goes through the agent's copy module.
type Connection interface {
	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
	WriteFile(ctx context.Context, path string, content io.Reader, mode uint32) error
	Close() error
}
