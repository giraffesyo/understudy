package modules

import "testing"

// Expected values are ansible-core 2.21's heuristic_log_sanitize.
func TestHeuristicLogSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://tester@localhost/home/tester/repo.git": "ssh:********@localhost/home/tester/repo.git",
		"https://u:p@h/x":                 "https://u:********@h/x",
		"git@github.com:org/repo.git":     "git@github.com:org/repo.git",
		"plain":                           "plain",
		"http://h/x@y":                    "http:********@y",
		"user:pass@host":                  "user:********@host",
		"https://a:b@h1/x https://c@h2/y": "https://a:********@h2/y",
		"ssh://git@host:22/r.git":         "ssh:********@host:22/r.git",
		"x@y@z":                           "x@y@z",
		"ftp://u:p@h:21/a@b":              "ftp://u:********@b",
	} {
		if got := heuristicLogSanitize(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
