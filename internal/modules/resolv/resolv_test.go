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
	if notFoundAfterDNS("/etc/nsswitch.conf") {
		t.Skip("a hosts source after dns has the last word")
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

// TestNotFoundAfterDNS: glibc's getaddrinfo reports the h_errno of the
// last hosts source it asked; one after a failed dns that does not know
// the name makes EAI_AGAIN EAI_NONAME.
func TestNotFoundAfterDNS(t *testing.T) {
	dir := t.TempDir()
	for conf, want := range map[string]bool{
		"":                                   false,
		"hosts:      files dns myhostname\n": true,  // Rocky 9
		"hosts:          files dns\n":        false, // Ubuntu, Debian
		"hosts: files myhostname resolve [!UNAVAIL=return] dns\n": false, // Fedora
		"hosts: files dns [UNAVAIL=return] myhostname\n":          false,
		"hosts: dns [!UNAVAIL=return] files\n":                    true,
		"hosts: dns [!TRYAGAIN=return] files\n":                   false,
		"hosts: dns [ NOTFOUND = return ] files\n":                true,
		"hosts: files dns mdns4_minimal\n":                        false,
		"hosts: files dns mdns4 [NOTFOUND=return] files\n":        true,
		"hosts: files dns myhostname [NOTFOUND=return] mdns4\n":   true,
		"# hosts: files dns myhostname\nhosts: files dns\n":       false,
		"hosts: files dns # myhostname\n":                         false,
	} {
		p := filepath.Join(dir, "nsswitch.conf")
		os.WriteFile(p, []byte(conf), 0o644)
		if got := notFoundAfterDNS(p); got != want {
			t.Errorf("%q: %v, want %v", conf, got, want)
		}
	}
	if notFoundAfterDNS(filepath.Join(dir, "missing")) {
		t.Error("no nsswitch.conf: glibc's default is files dns")
	}
}
