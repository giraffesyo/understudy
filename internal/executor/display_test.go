package executor

import (
	"testing"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// The -vv redirect table covers only modules understudy implements.
func TestModuleRedirectsImplemented(t *testing.T) {
	for _, name := range playbook.ModuleRedirectNames() {
		if !actions.Known(name) {
			t.Errorf("%s has a builtin redirect but is not implemented", name)
		}
	}
}

func TestModuleRedirects(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   []string
	}{
		{"ini_file", []string{"redirecting (type: modules) ansible.builtin.ini_file to community.general.ini_file"}},
		{"ansible.builtin.ini_file", []string{"redirecting (type: modules) ansible.builtin.ini_file to community.general.ini_file"}},
		{"ansible.legacy.ini_file", []string{"redirecting (type: modules) ansible.builtin.ini_file to community.general.ini_file"}},
		{"community.general.ini_file", nil},
		{"copy", nil},
		{"mysql_info", []string{
			"redirecting (type: modules) ansible.builtin.mysql_info to community.mysql.mysql_info",
			"redirecting (type: modules) community.mysql.mysql_info to ansible.mysql.mysql_info",
		}},
		{"community.mysql.mysql_info", []string{"redirecting (type: modules) community.mysql.mysql_info to ansible.mysql.mysql_info"}},
	} {
		got := playbook.ModuleRedirects(tc.action)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %q, want %q", tc.action, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %q, want %q", tc.action, got, tc.want)
			}
		}
	}
	if got := playbook.ActionRedirect("yum"); got != "redirecting (type: action) ansible.builtin.yum to ansible.builtin.dnf" {
		t.Errorf("yum: %q", got)
	}
	if got := playbook.LookupRedirect("community.general.flattened"); got != "" {
		t.Errorf("FQCN lookup: %q", got)
	}
}
