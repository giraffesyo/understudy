package main

import (
	"os"
	"strconv"

	"github.com/giraffesyo/understudy/internal/cli"
	"github.com/giraffesyo/understudy/internal/modules"
)

func main() {
	modules.AsyncReexec = true
	// In-process (local) async jobs re-exec this binary.
	if len(os.Args) == 4 && os.Args[1] == modules.AsyncRunArg {
		timeout, _ := strconv.Atoi(os.Args[3])
		os.Exit(modules.RunAsyncJob(os.Args[2], timeout))
	}
	os.Exit(cli.Main())
}
