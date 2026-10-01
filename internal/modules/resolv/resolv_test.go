package resolv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNdots(t *testing.T) {
	dir := t.TempDir()
	for conf, want := range map[string]int{
		"":                     1,
		"nameserver 1.1.1.1\n": 1,
		"options ndots:3\n":    3,
		"options ndots:2 timeout:1\noptions ndots:5\n": 5,
		"options ndots:40\n":                           15,
	} {
		p := filepath.Join(dir, "resolv.conf")
		os.WriteFile(p, []byte(conf), 0o644)
		if got := ndots(p); got != want {
			t.Errorf("%q: ndots %d, want %d", conf, got, want)
		}
	}
}
