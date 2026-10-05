//go:build fips

package embedded

import (
	"bytes"
	"crypto/fips140"
	"debug/buildinfo"
	"runtime/debug"
	"strings"
	"testing"
)

func buildSetting(info *debug.BuildInfo, name string) string {
	for _, setting := range info.Settings {
		if setting.Key == name {
			return setting.Value
		}
	}
	return ""
}

func TestFIPSBuilds(t *testing.T) {
	if !fips140.Enabled() {
		t.Fatal("FIPS mode is disabled; run make test-fips")
	}
	self, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("missing test binary build information")
	}
	want := buildSetting(self, "GOFIPS140")
	if !strings.HasPrefix(want, "v") || fips140.Version() == "latest" {
		t.Fatalf("expected a frozen cryptographic module, got %q", want)
	}
	check := func(t *testing.T, info *debug.BuildInfo) {
		t.Helper()
		if got := buildSetting(info, "GOFIPS140"); got != want {
			t.Errorf("GOFIPS140 = %q, want %q", got, want)
		}
		if got := buildSetting(info, "CGO_ENABLED"); got != "0" {
			t.Errorf("CGO_ENABLED = %q, want 0", got)
		}
	}
	check(t, self)
	t.Run("controller", func(t *testing.T) {
		info, err := buildinfo.ReadFile("../../bin/understudy")
		if err != nil {
			t.Fatal(err)
		}
		check(t, info)
	})
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run("agent-"+arch, func(t *testing.T) {
			agent, err := Agent("linux", arch)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := gunzip(agent.GzData)
			if err != nil {
				t.Fatal(err)
			}
			info, err := buildinfo.Read(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			check(t, info)
		})
	}
}
