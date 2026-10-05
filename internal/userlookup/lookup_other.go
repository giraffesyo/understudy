//go:build !linux

package userlookup

import "os/user"

func Lookup(name string) (*user.User, error) { return user.Lookup(name) }

func LookupId(id string) (*user.User, error) { return user.LookupId(id) }

func LookupGroup(name string) (*user.Group, error) { return user.LookupGroup(name) }

func LookupGroupId(id string) (*user.Group, error) { return user.LookupGroupId(id) }

func Current() (*user.User, error) { return user.Current() }
