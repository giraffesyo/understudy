package connection

import (
	"context"
	"io"
	"testing"
)

type immediateReplyPTY struct {
	prompt, reply string
	written       chan struct{}
	drained       chan struct{}
	reads         int
}

func (p *immediateReplyPTY) Read(buf []byte) (int, error) {
	p.reads++
	switch p.reads {
	case 1:
		return copy(buf, p.prompt), nil
	case 2:
		<-p.written
		return copy(buf, p.reply), nil
	default:
		close(p.drained)
		return 0, io.EOF
	}
}

func (p *immediateReplyPTY) Write(buf []byte) (int, error) {
	close(p.written)
	// The reader appends the reply before asking for the next read.
	<-p.drained
	return len(buf), nil
}

func (p *immediateReplyPTY) Kill()       {}
func (p *immediateReplyPTY) Wait() error { return nil }

func TestPTYDialogKeepsImmediateReply(t *testing.T) {
	const marker = "BECOME-SUCCESS-test"
	for _, tc := range []struct {
		method, prompt, reply, want string
	}{
		{"su", "Password: ", "su: Authentication failure\n", "Incorrect su password"},
		{"doas", "doas (user@host) password: ", "doas: Permission denied\n", "Incorrect doas password"},
		{"su", "Password: ", marker + "\n", ""},
		{"doas", "doas (user@host) password: ", marker + "\n", ""},
	} {
		t.Run(tc.method+"/"+tc.reply, func(t *testing.T) {
			proc := &immediateReplyPTY{prompt: tc.prompt, reply: tc.reply, written: make(chan struct{}), drained: make(chan struct{})}
			start := func(context.Context, string) (ptyProcess, error) { return proc, nil }
			err := runPTYDialog(t.Context(), start, &BecomeSpec{Method: tc.method, Password: "secret"}, "", marker)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
