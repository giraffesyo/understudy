//go:build !linux && !darwin

package modules

// childGroupLeaders is unknown here: signals are not forwarded.
func childGroupLeaders(int) []int { return nil }

// outputGone is not detected here: an orphan is told by its parent.
func outputGone() bool { return false }
