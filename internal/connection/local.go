package connection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Local runs commands on the control node itself (ansible_connection=local).
type Local struct{}

func NewLocal() *Local { return &Local{} }

func (l *Local) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	if opts.Become.needsPTY() {
		plain := func(ctx context.Context, c string, in io.Reader) (ExecResult, error) {
			return l.Exec(ctx, c, ExecOptions{Stdin: in, Timeout: opts.Timeout})
		}
		return execPTYBecome(ctx, plain, startLocalPTY, cmd, opts)
	}
	shellCmd, stdin := applyBecome(cmd, opts)
	c := exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
	if stdin != nil {
		c.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	res := ExecResult{RC: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.RC = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("local exec: %w", err)
	}
	return res, nil
}

func (l *Local) WriteFile(ctx context.Context, path string, content io.Reader, mode uint32) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".understudy-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(os.FileMode(mode)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (l *Local) Close() error { return nil }
