package cli

import "github.com/giraffesyo/understudy/internal/playbook"

// adhocArgs parses -a values using the same rules as playbook module args.
func adhocArgs(task *playbook.Task, module, raw string) error {
	return playbook.ParseAdhocArgs(task, module, raw)
}
