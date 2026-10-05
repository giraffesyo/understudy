package userlookup

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func fakeGetent(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "getent"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestNSSLookups(t *testing.T) {
	fakeGetent(t, `case "$1:$2" in
passwd:directory-user|passwd:12345|passwd:012345)
  echo 'directory-user:x:12345:23456:Directory User,Room 1:/home/directory-user:/bin/sh';;
group:directory-group|group:23456|group:023456)
  echo 'directory-group:x:23456:';;
*) exit 2;;
esac`)
	wantUser := &user.User{Username: "directory-user", Uid: "12345", Gid: "23456", Name: "Directory User", HomeDir: "/home/directory-user"}
	for _, lookup := range []func() (*user.User, error){
		func() (*user.User, error) { return Lookup("directory-user") },
		func() (*user.User, error) { return LookupId("12345") },
		func() (*user.User, error) { return LookupId("012345") },
	} {
		got, err := lookup()
		if err != nil || !reflect.DeepEqual(got, wantUser) {
			t.Fatalf("user = %+v, %v; want %+v", got, err, wantUser)
		}
	}
	wantGroup := &user.Group{Name: "directory-group", Gid: "23456"}
	for _, lookup := range []func() (*user.Group, error){
		func() (*user.Group, error) { return LookupGroup("directory-group") },
		func() (*user.Group, error) { return LookupGroupId("23456") },
		func() (*user.Group, error) { return LookupGroupId("023456") },
	} {
		got, err := lookup()
		if err != nil || !reflect.DeepEqual(got, wantGroup) {
			t.Fatalf("group = %+v, %v; want %+v", got, err, wantGroup)
		}
	}
}

func TestCurrentUsesNSS(t *testing.T) {
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	fakeGetent(t, fmt.Sprintf("[ \"$1:$2\" = passwd:%s ] || exit 2\necho 'directory-user:x:%s:%s::/srv/home:/bin/sh'", uid, uid, gid))
	got, err := Current()
	if err != nil || got.Username != "directory-user" || got.HomeDir != "/srv/home" {
		t.Fatalf("Current() = %+v, %v", got, err)
	}
}

func TestNoGetentFallsBackToLocalAccounts(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Lookup(u.Username)
	if err != nil || !reflect.DeepEqual(got, u) {
		t.Fatalf("Lookup = %+v, %v; want %+v", got, err, u)
	}
	got, err = LookupId(u.Uid)
	if err != nil || !reflect.DeepEqual(got, u) {
		t.Fatalf("LookupId = %+v, %v; want %+v", got, err, u)
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	gotGroup, err := LookupGroup(g.Name)
	if err != nil || !reflect.DeepEqual(gotGroup, g) {
		t.Fatalf("LookupGroup = %+v, %v; want %+v", gotGroup, err, g)
	}
	gotGroup, err = LookupGroupId(g.Gid)
	if err != nil || !reflect.DeepEqual(gotGroup, g) {
		t.Fatalf("LookupGroupId = %+v, %v; want %+v", gotGroup, err, g)
	}
}

func TestGetentFailureDoesNotFallBack(t *testing.T) {
	for _, body := range []string{"exit 2", "exit 1", "echo malformed", "exit 0"} {
		t.Run(body, func(t *testing.T) {
			fakeGetent(t, body)
			if _, err := Lookup("root"); err == nil {
				t.Fatal("resolved a user despite NSS failure")
			}
			if _, err := LookupGroup("root"); err == nil {
				t.Fatal("resolved a group despite NSS failure")
			}
		})
	}
}

func TestRejectInvalidRecords(t *testing.T) {
	for _, record := range []string{
		"directory-user:x:bad:23456::/home/user:/bin/sh",
		"directory-user:x:12345:-1::/home/user:/bin/sh",
		"directory-user:x:4294967296:23456::/home/user:/bin/sh",
		"directory-user:x:12345:23456::/home/user:/bin/sh\ndirectory-user:x:0:0::/:/bin/sh",
	} {
		t.Run(record, func(t *testing.T) {
			fakeGetent(t, "echo '"+record+"'")
			if _, err := Lookup("directory-user"); err == nil {
				t.Fatal("accepted invalid passwd record")
			}
		})
	}
	for _, record := range []string{"directory-group:x:bad:", "directory-group:x:-1:"} {
		t.Run(record, func(t *testing.T) {
			fakeGetent(t, "echo '"+record+"'")
			if _, err := LookupGroup("directory-group"); err == nil {
				t.Fatal("accepted invalid group record")
			}
		})
	}
}

func TestCanonicalNames(t *testing.T) {
	fakeGetent(t, `case "$1" in
passwd) echo 'directory-user:x:12345:23456::/home/user:/bin/sh';;
group) echo 'directory-group:x:23456:';;
esac`)
	u, err := Lookup("DIRECTORY-USER")
	if err != nil || u.Username != "directory-user" {
		t.Fatalf("Lookup() = %+v, %v", u, err)
	}
	g, err := LookupGroup("DIRECTORY-GROUP")
	if err != nil || g.Name != "directory-group" {
		t.Fatalf("LookupGroup() = %+v, %v", g, err)
	}
}

func TestNumericNameDoesNotResolveDifferentAccount(t *testing.T) {
	fakeGetent(t, `case "$1" in
passwd) echo 'root:x:0:0::/root:/bin/sh';;
group) echo 'root:x:0:';;
esac`)
	if _, err := Lookup("0"); err == nil {
		t.Fatal("resolved numeric name as a different user")
	}
	if _, err := LookupGroup("0"); err == nil {
		t.Fatal("resolved numeric name as a different group")
	}
	if _, err := LookupId("12345"); err == nil {
		t.Fatal("accepted wrong UID")
	}
	if _, err := LookupGroupId("23456"); err == nil {
		t.Fatal("accepted wrong GID")
	}
}

func TestGroupWithoutMembersField(t *testing.T) {
	fakeGetent(t, "echo 'directory-group:x:23456'")
	g, err := LookupGroup("directory-group")
	if err != nil || g.Gid != "23456" {
		t.Fatalf("LookupGroup() = %+v, %v", g, err)
	}
}
