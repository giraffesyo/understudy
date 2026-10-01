package e2e

import (
	"os"
	"testing"
)

// missingPrereq skips a test whose prerequisite (bin/understudy,
// ansible-playbook, docker, nc) is missing, so a bare `go test` on a
// developer machine passes. With UNDERSTUDY_REQUIRE_PREREQS=1 (CI sets it)
// a missing prerequisite fails the test instead: a suite that silently
// skipped everything would otherwise report success.
func missingPrereq(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("UNDERSTUDY_REQUIRE_PREREQS") == "1" {
		t.Fatalf("prerequisite missing (UNDERSTUDY_REQUIRE_PREREQS=1): %s", reason)
	}
	t.Skip(reason)
}
