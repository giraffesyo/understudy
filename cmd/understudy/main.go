package main

import (
	"os"
	"strconv"

	"github.com/giraffesyo/understudy/internal/cli"
	"github.com/giraffesyo/understudy/internal/modules"
)

func main() {
	modules.AsyncReexec = true
	modules.LocalAgent = true
	// In-process (local) async jobs re-exec this binary.
	if len(os.Args) == 4 && os.Args[1] == modules.AsyncRunArg {
		timeout, _ := strconv.Atoi(os.Args[3])
		os.Exit(modules.RunAsyncJob(os.Args[2], timeout))
	}
	// Commands modules start lead their own process groups; a Ctrl-C
	// still reaches them.
	modules.ForwardSignals()
	// Local become runs modules in a child of this binary.
	if len(os.Args) == 3 && os.Args[1] == modules.LocalAgentArg && os.Args[2] == "run" {
		modules.ExitWithParent()
		os.Exit(modules.ServeFrame(os.Stdin, os.Stdout))
	}
	os.Exit(cli.Main())
}
