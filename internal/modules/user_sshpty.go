//go:build linux || darwin

package modules

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/ptyutil"
)

// sshKeygenPTY runs ssh-keygen the way ansible-core's User.ssh_key_gen
// does when a passphrase is given: stdin, stdout and stderr each on their
// own pseudo-terminal, in a new session without a controlling terminal (so
// ssh-keygen prompts on stderr and reads stdin), answering "Enter
// passphrase" and then "Enter same passphrase again" with the passphrase
// and a carriage return. The passphrase never appears in the process list.
func sshKeygenPTY(env *RunEnv, argv []string, passphrase string) (*int, string, string) {
	one := 1
	type pair struct{ master, slave *os.File }
	var ptys [3]pair
	closeAll := func() {
		for _, p := range ptys {
			if p.master != nil {
				p.master.Close()
			}
			if p.slave != nil {
				p.slave.Close()
			}
		}
	}
	for i := range ptys {
		master, name, err := ptyutil.Open()
		if err != nil {
			closeAll()
			return &one, "", pyStrOSError(err, "")
		}
		ptys[i].master = master
		slave, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			closeAll()
			return &one, "", pyStrOSError(err, name)
		}
		ptys[i].slave = slave
	}
	defer closeAll()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range env.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "LC_ALL="+bestParsableLocale(env))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = ptys[0].slave, ptys[1].slave, ptys[2].slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return &one, "", pyStrOSError(err, argv[0])
	}
	// Only the child keeps the slave ends, so the masters see EOF (or EIO)
	// once it exits.
	for i := range ptys {
		ptys[i].slave.Close()
		ptys[i].slave = nil
	}

	type chunk struct {
		stderr bool
		data   []byte
	}
	chunks := make(chan chunk)
	readers := make(chan struct{}, 2)
	stop := make(chan struct{})
	defer close(stop)
	for i, stderr := range []bool{false, true} {
		go func(f *os.File, stderr bool) {
			defer func() { readers <- struct{}{} }()
			buf := make([]byte, 10240)
			for {
				n, err := f.Read(buf)
				if n > 0 {
					select {
					case chunks <- chunk{stderr, append([]byte(nil), buf[:n]...)}:
					case <-stop:
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(ptys[i+1].master, stderr)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	const timeout = 900 * time.Second
	deadline := time.After(timeout)
	var outBuf, errBuf []byte
	first, second := []byte("Enter passphrase"), []byte("Enter same passphrase again")
	prompt := first
	answer := append([]byte(passphrase), '\r')
	done, waitErr := 0, error(nil)
	processDone := false
	var drained <-chan time.Time
loop:
	for done < 2 || !processDone {
		select {
		case c := <-chunks:
			buf := &outBuf
			if c.stderr {
				buf = &errBuf
			}
			*buf = append(*buf, c.data...)
			if prompt != nil && bytes.Contains(*buf, prompt) {
				ptys[0].master.Write(answer)
				if bytes.Equal(prompt, first) {
					prompt = second
				} else {
					prompt = nil
				}
			}
			if bytes.Contains(outBuf, []byte("Overwrite (y/n)?")) || bytes.Contains(errBuf, []byte("Overwrite (y/n)?")) {
				// The key was created between the existence check and now.
				cmd.Process.Kill()
				return nil, "Key already exists", ""
			}
		case <-readers:
			done++
		case waitErr = <-exited:
			processDone = true
			// Take what the child wrote before exiting; the masters report
			// EOF or EIO once it is gone, but don't wait on that forever.
			drained = time.After(time.Second)
		case <-drained:
			break loop
		case <-deadline:
			cmd.Process.Kill()
			return &one, "", "Timeout after 900 while reading passphrase for SSH key"
		}
	}
	rc := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			return &one, "", waitErr.Error()
		}
	}
	return &rc, string(outBuf), string(errBuf)
}
