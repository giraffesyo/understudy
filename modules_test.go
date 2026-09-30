package understudy_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	understudy "github.com/giraffesyo/understudy"
)

func TestAlternativesArgs(t *testing.T) {
	a := understudy.Alternatives{
		Name:     "java",
		Path:     "/usr/lib/jvm/java-17/bin/java",
		Link:     "/usr/bin/java",
		Priority: understudy.Int(0),
		State:    "present",
		Subcommands: []understudy.AlternativeSubcommand{
			{Name: "keytool", Path: "/usr/lib/jvm/java-17/bin/keytool", Link: "/usr/bin/keytool"},
			{Name: "jar", Path: "/usr/lib/jvm/java-17/bin/jar"},
		},
	}
	if a.ModuleName() != "community.general.alternatives" {
		t.Errorf("module name %q", a.ModuleName())
	}
	want := map[string]any{
		"name": "java", "path": "/usr/lib/jvm/java-17/bin/java", "link": "/usr/bin/java",
		"priority": int64(0), "state": "present",
		"subcommands": []any{
			map[string]any{"name": "keytool", "path": "/usr/lib/jvm/java-17/bin/keytool", "link": "/usr/bin/keytool"},
			map[string]any{"name": "jar", "path": "/usr/lib/jvm/java-17/bin/jar"},
		},
	}
	if got := a.ModuleArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %#v\nwant %#v", got, want)
	}
	// Unset options are left to the module's defaults.
	if got := (understudy.Alternatives{Name: "editor", Path: "/usr/bin/vim"}).ModuleArgs(); !reflect.DeepEqual(got,
		map[string]any{"name": "editor", "path": "/usr/bin/vim"}) {
		t.Errorf("minimal args = %#v", got)
	}
}

func TestAlternativesRenders(t *testing.T) {
	pb := understudy.Playbook{{Hosts: "all", Tasks: []understudy.Task{{
		Name: "java",
		Action: understudy.Alternatives{Name: "java", Path: "/opt/java/bin/java", Priority: understudy.Int(100),
			Subcommands: []understudy.AlternativeSubcommand{{Name: "jar", Path: "/opt/java/bin/jar", Link: "/usr/bin/jar"}}},
	}}}}
	data, err := pb.YAML()
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		"community.general.alternatives:\n",
		"name: java\n",
		"path: /opt/java/bin/java\n",
		"priority: 100\n",
		"subcommands:\n",
		"- link: /usr/bin/jar\n",
		"name: jar\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML missing %q:\n%s", want, out)
		}
	}
}

// The task reaches the alternatives module: its argument validation runs
// (an unknown state is rejected with the module's choices message).
func TestAlternativesRuns(t *testing.T) {
	pb := understudy.Playbook{{Hosts: "localhost", Tasks: []understudy.Task{{
		Name:   "bad state",
		Action: understudy.Alternatives{Name: "java", Path: "/nonexistent/java", State: "sideways"},
	}}}}
	var buf bytes.Buffer
	res, err := understudy.Run(context.Background(), pb, understudy.Options{
		Inventory: []string{"localhost,"}, Connection: "local", Output: &buf, NoColor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() || !strings.Contains(buf.String(), "value of state must be one of: present, selected, absent, auto, got: sideways") {
		t.Errorf("want the module's choices failure:\n%s", buf.String())
	}
}
