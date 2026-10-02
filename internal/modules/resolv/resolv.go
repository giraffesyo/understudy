// Package resolv makes Go's DNS resolution follow the target's C library
// where their rules differ: modules look names up as the Python they
// stand in for would, through getaddrinfo.
package resolv

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	muslOnce sync.Once
	musl     bool
)

// Musl reports whether the system C library is musl (Alpine).
func Musl() bool {
	muslOnce.Do(func() {
		m, _ := filepath.Glob("/lib/ld-musl-*.so.1")
		musl = len(m) > 0
	})
	return musl
}

// GaiError is str(socket.gaierror) for a lookup that failed with err, as
// getaddrinfo reports it through the C library (glibc's or musl's codes
// and gai_strerror text). NXDOMAIN is EAI_NONAME. A NOERROR answer with
// no records and neither the AA nor the RA bit (what Go calls a lame
// referral; some DNS proxies answer every unknown name so) is a server
// failure to glibc, which tries the next server and ends in EAI_AGAIN,
// and an empty answer, EAI_NODATA, to musl. Everything else (SERVFAIL,
// timeouts, unreadable answers) is EAI_AGAIN.
func GaiError(err *net.DNSError) string {
	switch {
	case err.IsNotFound && Musl():
		return "[Errno -2] Name does not resolve"
	case err.IsNotFound:
		return "[Errno -2] Name or service not known"
	case err.Err == "lame referral" && Musl():
		return "[Errno -5] Name has no usable address"
	case Musl():
		return "[Errno -3] Try again"
	}
	return "[Errno -3] Temporary failure in name resolution"
}

// Addr is a "host:port" address to dial as the C library would look its
// host up. musl applies no search domains to a name with at least ndots
// dots (name_from_dns_search), where Go's resolver, as glibc's, tries
// them after the name itself: on musl such a name is made absolute.
func Addr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !Musl() {
		return addr
	}
	return net.JoinHostPort(Host(host), port)
}

// Host is a host name to look up as the C library would (see Addr).
func Host(host string) string {
	if !Musl() || host == "" || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return host
	}
	if strings.Count(host, ".") < ndots("/etc/resolv.conf") {
		return host
	}
	return host + "."
}

// ndots is resolv.conf's ndots option as musl reads it (the last one
// set; default 1, at most 15).
func ndots(path string) int {
	n := 1
	data, err := os.ReadFile(path)
	if err != nil {
		return n
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "options" {
			continue
		}
		for _, opt := range f[1:] {
			if v, ok := strings.CutPrefix(opt, "ndots:"); ok {
				if x, err := strconv.Atoi(v); err == nil && x >= 0 {
					n = min(x, 15)
				}
			}
		}
	}
	return n
}
