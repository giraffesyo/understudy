// The understudy agent runs on managed hosts. It reads one task frame from
// stdin, executes the module in-process, and writes the result behind a
// sentinel line on stdout. It must import only modules + agentproto (the
// Makefile's depcheck enforces this).
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules"
)

var version = "0.1.0-dev" // set via -ldflags at release

func main() {
	modules.AsyncReexec = true // async jobs re-exec this agent binary
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent <run|version>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case modules.AsyncRunArg:
		timeout, _ := strconv.Atoi(os.Args[3])
		os.Exit(modules.RunAsyncJob(os.Args[2], timeout))
	case "version":
		fmt.Printf("understudy-agent proto=%d version=%s\n", agentproto.ProtoVersion, version)
	case "run":
		os.Exit(modules.ServeFrame(os.Stdin, os.Stdout))
	default:
		fmt.Fprintf(os.Stderr, "agent: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
