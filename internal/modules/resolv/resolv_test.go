package resolv

import (
	"net"
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

func TestGaiError(t *testing.T) {
	if Musl() {
		t.Skip("glibc's wording")
	}
	for _, c := range []struct {
		err  net.DNSError
		want string
	}{
		{net.DNSError{Err: "no such host", IsNotFound: true}, "[Errno -2] Name or service not known"},
		{net.DNSError{Err: "server misbehaving", IsTemporary: true}, "[Errno -3] Temporary failure in name resolution"},
		{net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true}, "[Errno -3] Temporary failure in name resolution"},
		// NOERROR, no records, neither AA nor RA: glibc's res_send takes
		// it for a server failure.
		{net.DNSError{Err: "lame referral"}, "[Errno -3] Temporary failure in name resolution"},
	} {
		if got := GaiError(&c.err); got != c.want {
			t.Errorf("%q: %s, want %s", c.err.Err, got, c.want)
		}
	}
}
