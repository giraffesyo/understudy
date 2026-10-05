// Package userlookup resolves system accounts without requiring cgo.
package userlookup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// getent consults NSS; os/user in a static Linux binary only reads local files.
func getent(database, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getent", database, key).Output()
	if err != nil {
		return nil, err
	}
	line := strings.TrimSuffix(string(out), "\n")
	if strings.ContainsAny(line, "\r\n") {
		return nil, fmt.Errorf("getent %s returned multiple records", database)
	}
	return strings.Split(line, ":"), nil
}

func validID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 32)
	return err == nil
}

func lookupUser(key string, byID bool) (*user.User, error) {
	if key == "" || (byID && !validID(key)) {
		return nil, fmt.Errorf("invalid user lookup key %q", key)
	}
	f, err := getent("passwd", key)
	if errors.Is(err, exec.ErrNotFound) {
		if byID {
			return user.LookupId(key)
		}
		return user.Lookup(key)
	}
	if err != nil {
		return nil, fmt.Errorf("getent passwd %q: %w", key, err)
	}
	if len(f) != 7 || f[0] == "" || !validID(f[2]) || !validID(f[3]) {
		return nil, fmt.Errorf("invalid passwd record for %q", key)
	}
	if (!byID && validID(key) && f[0] != key) || (byID && !sameID(f[2], key)) {
		return nil, fmt.Errorf("passwd record does not match %q", key)
	}
	name, _, _ := strings.Cut(f[4], ",")
	return &user.User{Username: f[0], Uid: f[2], Gid: f[3], Name: name, HomeDir: f[5]}, nil
}

func lookupGroup(key string, byID bool) (*user.Group, error) {
	if key == "" || (byID && !validID(key)) {
		return nil, fmt.Errorf("invalid group lookup key %q", key)
	}
	f, err := getent("group", key)
	if errors.Is(err, exec.ErrNotFound) {
		if byID {
			return user.LookupGroupId(key)
		}
		return user.LookupGroup(key)
	}
	if err != nil {
		return nil, fmt.Errorf("getent group %q: %w", key, err)
	}
	if (len(f) != 3 && len(f) != 4) || f[0] == "" || !validID(f[2]) {
		return nil, fmt.Errorf("invalid group record for %q", key)
	}
	if (!byID && validID(key) && f[0] != key) || (byID && !sameID(f[2], key)) {
		return nil, fmt.Errorf("group record does not match %q", key)
	}
	return &user.Group{Name: f[0], Gid: f[2]}, nil
}

func sameID(a, b string) bool {
	x, errA := strconv.ParseUint(a, 10, 32)
	y, errB := strconv.ParseUint(b, 10, 32)
	return errA == nil && errB == nil && x == y
}

func Lookup(name string) (*user.User, error) { return lookupUser(name, false) }

func LookupId(id string) (*user.User, error) { return lookupUser(id, true) }

func LookupGroup(name string) (*user.Group, error) { return lookupGroup(name, false) }

func LookupGroupId(id string) (*user.Group, error) { return lookupGroup(id, true) }

func Current() (*user.User, error) { return LookupId(strconv.Itoa(os.Getuid())) }
