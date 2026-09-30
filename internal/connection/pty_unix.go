//go:build linux || darwin

package connection

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/giraffesyo/understudy/internal/ptyutil"
)

// localPTY is a local command on a pseudo-terminal.
type localPTY struct {
	master *os.File
	cmd    *exec.Cmd
}

func (p *localPTY) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	if errors.Is(err, syscall.EIO) {
		err = os.ErrClosed // Linux: the slave side closed (process exited)
	}
	return n, err
}
func (p *localPTY) Write(b []byte) (int, error) { return p.master.Write(b) }
func (p *localPTY) Wait() error {
	err := p.cmd.Wait()
	p.master.Close()
	return err
}
func (p *localPTY) Kill() {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

// startLocalPTY runs cmd under /bin/sh on a new pseudo-terminal as its
// controlling terminal (a new session), for su and doas on a local
// connection.
func startLocalPTY(ctx context.Context, cmd string) (ptyProcess, error) {
	master, slaveName, err := ptyutil.Open()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close()
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = slave, slave, slave
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := c.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return &localPTY{master: master, cmd: c}, nil
}
