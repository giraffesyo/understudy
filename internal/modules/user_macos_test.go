package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseDscacheGroups(t *testing.T) {
	out := "name: staff\npassword: *\ngid: 20\nusers: root alice \n\n" +
		"name: nobody\npassword: *\ngid: -2\n\nname: nogroup\npassword: *\ngid: -1\n"
	want := []string{"staff:*:20:root,alice", "nobody:*:4294967294:", "nogroup:*:-1:"}
	if got := parseDscacheGroups(out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDsclPropertyValue(t *testing.T) {
	for _, c := range []struct {
		out  string
		want any
	}{
		{"UserShell: /bin/zsh\n", "/bin/zsh"},
		{"RealName:\n Michael McQuade\n", "Michael McQuade"},
		{"RealName:\n first\nsecond\n", "first\nsecond"},
		{"Weird: a: b\n", "a"}, // split(': ')[1]
		{"", nil},
	} {
		got := dsclPropertyValue(c.out)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%q: got %q, want None", c.out, *got)
		case c.want != nil && (got == nil || *got != c.want):
			t.Errorf("%q: got %v, want %q", c.out, got, c.want)
		}
	}
}

// fakeDarwinTools puts dscl, dseditgroup, defaults, id and dscacheutil
// stand-ins first on PATH. They log every call; dscl answers for a user
// that exists once `dscl . -create /Users/<name>` has run.
func fakeDarwinTools(t *testing.T) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	state := filepath.Join(dir, "created")
	scripts := map[string]string{
		"dscl": `echo "dscl $*" >> ` + logFile + `
case "$2 $3" in
"-read /Users/newuser") [ -f ` + state + ` ] || exit 56; [ "$4" = UniqueID ] && echo "UniqueID: 502"; exit 0;;
"-create /Users/newuser") touch ` + state + `;;
"-list /Users") printf 'root 0\n_www 70\nalice 501\n';;
esac
exit 0`,
		"id": `[ -f ` + state + ` ] || exit 1
echo "newuser:********:502:20::0:0:newuser:/Users/newuser:/bin/bash"`,
		"dscacheutil": `if [ "$5" = staff ] || [ -z "$5" ]; then printf 'name: staff\npassword: *\ngid: 20\nusers: root\n\n'; fi
if [ "$5" = admin ] || [ -z "$5" ]; then printf 'name: admin\npassword: *\ngid: 80\nusers: root\n\n'; fi`,
		"dseditgroup": `echo "dseditgroup $*" >> ` + logFile,
		"defaults": `echo "defaults $*" >> ` + logFile + `
[ "$1" = read ] && printf '(\n    "_hidden"\n)\n'
exit 0`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logFile
}

// darwinWrites is the logged calls that change something.
func darwinWrites(t *testing.T, logFile string) []string {
	data, _ := os.ReadFile(logFile)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(l, " -create ") || strings.Contains(l, " -passwd ") ||
			strings.Contains(l, " -delete ") || strings.HasPrefix(l, "dseditgroup ") ||
			strings.HasPrefix(l, "defaults write") {
			out = append(out, l)
		}
	}
	return out
}

// DarwinUser.create_user: the account, each property in fields order
// (uid from the highest in use, group as a gid, bash for a non-system
// user), the password, then supplementary groups.
func TestDarwinUserCreate(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DarwinUser is chosen on macOS only")
	}
	logFile := fakeDarwinTools(t)
	res := userModule(&RunEnv{}, map[string]any{
		"name": "newuser", "groups": []any{"admin"}, "home": "/Users/newuser",
		"create_home": false, "password": "pw",
	})
	if res.Failed || !res.Changed {
		t.Fatalf("result: %+v", res)
	}
	want := []string{
		"dscl . -create /Users/newuser",
		"dscl . -create /Users/newuser RealName newuser",
		"dscl . -create /Users/newuser NFSHomeDirectory /Users/newuser",
		"dscl . -create /Users/newuser UserShell /bin/bash",
		"dscl . -create /Users/newuser UniqueID 502",
		"dscl . -create /Users/newuser PrimaryGroupID 20",
		"dscl . -passwd /Users/newuser pw",
		"dseditgroup -o edit -a newuser -t user admin",
	}
	if got := darwinWrites(t, logFile); !reflect.DeepEqual(got, want) {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res.Extra["uid"] != int64(502) || res.Extra["group"] != int64(20) || res.Extra["system"] != false {
		t.Errorf("result: %v", res.Extra)
	}
}

// A system user is hidden (IsHidden, twice as upstream's class-level
// fields list has it) and added to the login window's hidden users; its
// uid comes from the system range.
func TestDarwinSystemUserCreate(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DarwinUser is chosen on macOS only")
	}
	logFile := fakeDarwinTools(t)
	res := userModule(&RunEnv{}, map[string]any{"name": "newuser", "system": true, "create_home": false})
	if res.Failed || !res.Changed {
		t.Fatalf("result: %+v", res)
	}
	want := []string{
		"dscl . -create /Users/newuser",
		"dscl . -create /Users/newuser RealName newuser",
		"dscl . -create /Users/newuser UniqueID 71",
		"dscl . -create /Users/newuser PrimaryGroupID 20",
		"dscl . -create /Users/newuser IsHidden 1",
		"dscl . -create /Users/newuser IsHidden 1",
		"dscl . -create /Users/newuser Password *",
		"defaults write /Library/Preferences/com.apple.loginwindow.plist HiddenUsersList -array-add newuser",
	}
	if got := darwinWrites(t, logFile); !reflect.DeepEqual(got, want) {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
