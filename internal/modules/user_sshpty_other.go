//go:build !linux && !darwin

package modules

import "runtime"

func sshKeygenPTY(*RunEnv, []string, string) (*int, string, string) {
	one := 1
	return &one, "", "answering ssh-keygen's passphrase prompts needs a pseudo-terminal, which is not implemented on " + runtime.GOOS
}
