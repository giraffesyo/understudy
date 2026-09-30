package modules

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

// httpReason is the server's reason phrase (http.client's resp.reason).
func httpReason(resp *http.Response) string {
	code := strconv.Itoa(resp.StatusCode)
	if r := strings.TrimSpace(strings.TrimPrefix(resp.Status, code)); r != "" {
		return r
	}
	return http.StatusText(resp.StatusCode)
}

// urlErrorMsg approximates fetch_url's info['msg'] for a transport error.
func urlErrorMsg(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "Request failed: <urlopen error [Errno -2] Name or service not known>"
	}
	var opErr *net.OpError
	var errno syscall.Errno
	if errors.As(err, &opErr) && errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
		return fmt.Sprintf("Request failed: <urlopen error [Errno %d] %s>", int(errno), pyStrerror(errno))
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "Connection failure: timed out"
	}
	return "Request failed: <urlopen error " + err.Error() + ">"
}
