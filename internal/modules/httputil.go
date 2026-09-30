package modules

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	if errors.As(err, &opErr) && strings.Contains(err.Error(), "connection refused") {
		return "Request failed: <urlopen error [Errno 111] Connection refused>"
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "Connection failure: timed out"
	}
	return "Request failed: <urlopen error " + err.Error() + ">"
}
