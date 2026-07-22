// depcheck fails the build if the agent binary imports packages that would
// bloat it or leak control-plane code onto targets.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var forbidden = []string{
	"github.com/giraffesyo/understudy/internal/yaml",
	"github.com/giraffesyo/understudy/internal/template",
	"github.com/giraffesyo/understudy/internal/vars",
	"github.com/giraffesyo/understudy/internal/inventory",
	"github.com/giraffesyo/understudy/internal/playbook",
	"github.com/giraffesyo/understudy/internal/executor",
	"github.com/giraffesyo/understudy/internal/actions",
	"github.com/giraffesyo/understudy/internal/connection",
	"github.com/giraffesyo/understudy/internal/cli",
	"github.com/giraffesyo/understudy/internal/callback",
	"golang.org/x/crypto",
}

func main() {
	target := "./cmd/agent"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	out, err := exec.Command("go", "list", "-deps", target).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "depcheck: go list failed: %v\n", err)
		os.Exit(1)
	}
	var bad []string
	for _, dep := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if dep == f || strings.HasPrefix(dep, f+"/") {
				bad = append(bad, dep)
			}
		}
	}
	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "depcheck: the agent must not import:\n  %s\n", strings.Join(bad, "\n  "))
		os.Exit(1)
	}
	fmt.Println("depcheck: agent dependencies OK")
}
