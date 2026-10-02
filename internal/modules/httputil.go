package modules

import (
	"errors"
	"fmt"
	"io"
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

// URLOpenError is str() of the urllib URLError open_url raises for a
// transport error: "<urlopen error [Errno 111] Connection refused>".
func URLOpenError(err error) string {
	m := urlErrorMsg(err)
	if s, ok := strings.CutPrefix(m, "Request failed: "); ok {
		return s
	}
	return "<urlopen error " + strings.TrimPrefix(m, "Connection failure: ") + ">"
}

// urlErrorMsg approximates fetch_url's info['msg'] for a transport error.
func urlErrorMsg(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "Request failed: <urlopen error " + pyGaiError(dnsErr) + ">"
	}
	var opErr *net.OpError
	var errno syscall.Errno
	if errors.As(err, &opErr) && errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
		return fmt.Sprintf("Request failed: <urlopen error [Errno %d] %s>", int(errno), pyStrerror(errno))
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "Connection failure: timed out"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		// http.client's RemoteDisconnected, a ConnectionError.
		return "Connection failure: Remote end closed connection without response"
	}
	return "Request failed: <urlopen error " + err.Error() + ">"
}
