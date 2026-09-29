package executor

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPasswordTermParsing(t *testing.T) {
	p, err := parsePasswordTerm("/tmp/pw chars=ascii_letters,digits,hexdigits length=12", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.path != "/tmp/pw" || p.length != 12 || strings.Join(p.chars, "|") != "ascii_letters|digits|hexdigits" {
		t.Fatalf("got %+v", p)
	}
	if got := strings.Join(splitPasswordChars("digits,,,x"), "|"); got != ",|digits|x" {
		t.Fatalf("',,' handling: %q", got)
	}
	if _, err := parsePasswordTerm("/tmp/pw bogus=1", nil); err == nil || !strings.Contains(err.Error(), "Unrecognized parameter") {
		t.Fatalf("expected unrecognized-parameter error, got %v", err)
	}
	if p, _ := parsePasswordTerm("/tmp/pw", map[string]any{"length": 7}); p.length != 7 {
		t.Fatalf("kwargs length not applied: %d", p.length)
	}
}

func TestPasswordGeneratedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Opts: Options{BaseDir: dir}}
	term := "creds/db chars=digits length=16"

	// Concurrent first use (forks) must converge on one stored password.
	var wg sync.WaitGroup
	got := make([]any, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := lookupPassword(r, nil, []any{term}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = v[0]
		}(i)
	}
	wg.Wait()
	for _, v := range got[1:] {
		if v != got[0] {
			t.Fatalf("hosts saw different passwords: %v vs %v", got[0], v)
		}
	}
	pw := got[0].(string)
	if len(pw) != 16 || strings.Trim(pw, "0123456789") != "" {
		t.Fatalf("bad password %q", pw)
	}
	path := filepath.Join(dir, "creds/db")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored file: %v %v", info, err)
	}

	// An existing file (including Ansible's " salt=" suffix) is read back.
	os.WriteFile(path, []byte("hunter2 salt=abcd\n"), 0o600)
	if v, _ := lookupPassword(r, nil, []any{term}, nil); len(v) != 1 || v[0] != "hunter2" {
		t.Fatalf("read back %v", v)
	}
}
