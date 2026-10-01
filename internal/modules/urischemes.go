package modules

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/modules/resolv"
)

// urllib also opens file:// and ftp:// URLs (FileHandler, FTPHandler).
// Their responses carry no status code (r.code is None), which uri's
// int(resp['status']) cannot take: in ansible-core 2.21 a file:// or
// ftp:// request that succeeds crashes the module, and only the failures
// (reported as URLErrors) come out as ordinary results.

// errNoStatus is a file:// or ftp:// response: fetched, without a status.
var errNoStatus = errors.New("response without a status code")

// pyIntNoneMsg is the TypeError int(None) raises on the target's Python.
func pyIntNoneMsg(env *RunEnv) string {
	if v := targetPythonVersion(env); v == "3.8" || v == "3.9" {
		return "int() argument must be a string, a bytes-like object or a number, not 'NoneType'"
	}
	return "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"
}

// pyAtLeast314 reports whether the target's Python is 3.14 or later,
// whose FileHandler resolves the URL authority up front.
func pyAtLeast314(env *RunEnv) bool {
	v := targetPythonVersion(env)
	if v == "" {
		return true // ansible-core 2.21's controller Python
	}
	minor, _ := strconv.Atoi(strings.TrimPrefix(v, "3."))
	return minor >= 14
}

// localNames is FileHandler.get_names(): the addresses of localhost and
// of this host's name.
func localNames() map[string]bool {
	names := map[string]bool{}
	add := func(host string) {
		addrs, err := net.LookupIP(host)
		if err != nil {
			return
		}
		for _, a := range addrs {
			if a4 := a.To4(); a4 != nil {
				names[a4.String()] = true
			}
		}
	}
	add("localhost")
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	return names
}

// gethostbyname is socket.gethostbyname (IPv4 only).
func gethostbyname(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return ip.To4().String(), nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if a4 := a.To4(); a4 != nil {
			return a4.String(), nil
		}
	}
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// openFileURL is FileHandler.file_open.
func (c *uriClient) openFileURL(u *url.URL) error {
	host := u.Host
	path := u.Path
	isLocal := func(resolve bool) bool {
		if host == "" || host == "localhost" {
			return true
		}
		if h, err := os.Hostname(); err == nil && host == h {
			return true
		}
		if !resolve {
			return false
		}
		addr, err := gethostbyname(host)
		return err == nil && localNames()[addr]
	}
	if pyAtLeast314(c.env) {
		// url2pathname(resolve_host=True) refuses a remote authority
		// before anything is opened.
		if !isLocal(true) {
			return &uriURLError{"file:// scheme is supported only on localhost"}
		}
		return openLocalPath(path)
	}
	// Up to 3.13: stat and open first, then check the host.
	if err := openLocalPath(path); err != errNoStatus {
		return err
	}
	if host != "" {
		h, port, err := net.SplitHostPort(host)
		if err != nil {
			h, port = host, ""
		}
		addr, err := gethostbyname(h)
		if port != "" || err != nil || !localNames()[addr] {
			return &uriURLError{"file not on local host"}
		}
	}
	return errNoStatus
}

// openLocalPath stats and opens a local file as open_local_file does.
func openLocalPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return &uriURLError{pyOSErrorStr(err)}
	}
	f, err := os.Open(path)
	if err == nil {
		var st os.FileInfo
		if st, err = f.Stat(); err == nil && st.IsDir() {
			err = &os.PathError{Op: "open", Path: path, Err: syscall.EISDIR}
		}
		f.Close()
	}
	if err != nil {
		return &uriURLError{pyOSErrorStr(err)}
	}
	return errNoStatus
}

// ftpError is an ftplib error_reply/error_temp/error_perm/error_proto;
// str() is the reply, repr() the class name around it.
type ftpError struct {
	class string
	reply string
}

func (e *ftpError) Error() string { return e.reply }
func (e *ftpError) repr() string  { return e.class + "(" + pyStrRepr(e.reply) + ")" }

type ftpConn struct {
	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

func (f *ftpConn) deadline() {
	if f.timeout > 0 {
		f.conn.SetDeadline(time.Now().Add(f.timeout))
	}
}

// getline is ftplib's getline (CRLF stripped).
func (f *ftpConn) getline() (string, error) {
	f.deadline()
	line, err := f.r.ReadString('\n')
	if err != nil {
		if line == "" {
			return "", &ftpEOFError{}
		}
	}
	return strings.TrimRight(line, "\r\n"), nil
}

type ftpEOFError struct{}

func (e *ftpEOFError) Error() string { return "" }

// getresp is ftplib's getresp: the reply, or the error its first digit
// classifies.
func (f *ftpConn) getresp() (string, error) {
	line, err := f.getline()
	if err != nil {
		return "", err
	}
	if len(line) > 3 && line[3] == '-' {
		code := line[:3]
		for {
			next, err := f.getline()
			if err != nil {
				return "", err
			}
			line += "\n" + next
			if len(next) >= 4 && next[:3] == code && next[3] != '-' {
				break
			}
		}
	}
	if line == "" {
		return "", &ftpError{"error_proto", line}
	}
	switch line[0] {
	case '1', '2', '3':
		return line, nil
	case '4':
		return "", &ftpError{"error_temp", line}
	case '5':
		return "", &ftpError{"error_perm", line}
	}
	return "", &ftpError{"error_proto", line}
}

func (f *ftpConn) sendcmd(cmd string) (string, error) {
	f.deadline()
	if _, err := io.WriteString(f.conn, cmd+"\r\n"); err != nil {
		return "", err
	}
	return f.getresp()
}

func (f *ftpConn) voidcmd(cmd string) error {
	resp, err := f.sendcmd(cmd)
	if err != nil {
		return err
	}
	if resp[0] != '2' {
		return &ftpError{"error_reply", resp}
	}
	return nil
}

func (f *ftpConn) login(user, passwd string) error {
	if user == "" {
		user = "anonymous"
	}
	if passwd == "" && (user == "anonymous" || user == "-anonymous") {
		passwd = "anonymous@"
	}
	resp, err := f.sendcmd("USER " + user)
	if err == nil && resp[0] == '3' {
		resp, err = f.sendcmd("PASS " + passwd)
	}
	if err == nil && resp[0] == '3' {
		resp, err = f.sendcmd("ACCT ")
	}
	if err != nil {
		return err
	}
	if resp[0] != '2' {
		return &ftpError{"error_reply", resp}
	}
	return nil
}

func (f *ftpConn) cwd(dir string) error {
	if dir == "" {
		dir = "."
	}
	return f.voidcmd("CWD " + dir)
}

var ftp227 = regexp.MustCompile(`(\d+),(\d+),(\d+),(\d+),(\d+),(\d+)`)

// transfercmd is ntransfercmd in passive mode: the data connection.
func (f *ftpConn) transfercmd(cmd, host string) (net.Conn, error) {
	resp, err := f.sendcmd("PASV")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp, "227") {
		return nil, &ftpError{"error_reply", resp}
	}
	m := ftp227.FindStringSubmatch(resp)
	if m == nil {
		return nil, &ftpError{"error_proto", resp}
	}
	hi, _ := strconv.Atoi(m[5])
	lo, _ := strconv.Atoi(m[6])
	// ftplib does not trust the PASV address: it reuses the control host.
	d := &net.Dialer{Timeout: f.timeout}
	data, err := d.Dial("tcp", resolv.Addr(net.JoinHostPort(host, strconv.Itoa(hi<<8|lo))))
	if err != nil {
		return nil, err
	}
	resp, err = f.sendcmd(cmd)
	if err == nil && resp[0] == '2' {
		resp, err = f.getresp()
	}
	if err == nil && resp[0] != '1' {
		err = &ftpError{"error_reply", resp}
	}
	if err != nil {
		data.Close()
		return nil, err
	}
	return data, nil
}

// openFTPURL is FTPHandler.ftp_open: errNoStatus once the transfer
// started, or the URLError.
func (c *uriClient) openFTPURL(u *url.URL) error {
	if u.Host == "" {
		return &uriURLError{"ftp error: no host given"}
	}
	host, port := u.Hostname(), 21
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return &uriValueError{fmt.Sprintf("invalid literal for int() with base 10: %s", pyStrRepr(p))}
		}
		port = n
	}
	user, passwd := "", ""
	if u.User != nil {
		user = u.User.Username()
		passwd, _ = u.User.Password()
	}
	addr, err := gethostbyname(host)
	if err != nil {
		return &uriURLError{pyNetErr(err)}
	}
	path := u.EscapedPath()
	var attrs []string
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if parts := strings.Split(path, ";"); len(parts) > 1 {
		path, attrs = parts[0], parts[1:]
	}
	var dirs []string
	for _, d := range strings.Split(path, "/") {
		s, err := url.PathUnescape(d)
		if err != nil {
			s = d
		}
		dirs = append(dirs, s)
	}
	dirs, file := dirs[:len(dirs)-1], dirs[len(dirs)-1]
	if len(dirs) > 0 && dirs[0] == "" {
		dirs = dirs[1:]
	}

	wrap := func(err error) error {
		var ue *uriURLError
		switch {
		case errors.As(err, &ue):
			return &uriURLError{"ftp error: " + ue.Error()}
		case isFTPError(err):
			return &uriURLError{"ftp error: " + err.Error()}
		}
		return &uriURLError{"ftp error: " + pyNetErr(err)}
	}

	d := &net.Dialer{Timeout: c.timeout}
	conn, err := d.Dial("tcp", resolv.Addr(net.JoinHostPort(addr, strconv.Itoa(port))))
	if err != nil {
		return wrap(err)
	}
	defer conn.Close()
	f := &ftpConn{conn: conn, r: bufio.NewReader(conn), timeout: c.timeout}
	if _, err := f.getresp(); err != nil {
		return wrap(err)
	}
	if err := f.login(user, passwd); err != nil {
		return wrap(err)
	}
	if err := f.cwd(strings.Join(dirs, "/")); err != nil {
		return wrap(err)
	}
	typ := "D"
	if file != "" {
		typ = "I"
	}
	for _, a := range attrs {
		k, v, _ := strings.Cut(a, "=")
		if strings.EqualFold(k, "type") && len(v) == 1 && strings.Contains("aAiIdD", v) {
			typ = strings.ToUpper(v)
		}
	}
	isDir := typ == "D"
	cmd := "TYPE " + typ
	if isDir {
		cmd = "TYPE A"
	}
	if err := f.voidcmd(cmd); err != nil {
		return wrap(err)
	}
	var data net.Conn
	if file != "" && !isDir {
		data, err = f.transfercmd("RETR "+file, addr)
		if err != nil {
			var fe *ftpError
			if !errors.As(err, &fe) || fe.class != "error_perm" {
				return wrap(err)
			}
			if !strings.HasPrefix(fe.reply, "550") {
				return wrap(&uriURLError{"ftp error: " + fe.reply})
			}
			data = nil
		}
	}
	if data == nil {
		if err := f.voidcmd("TYPE A"); err != nil {
			return wrap(err)
		}
		cmd := "LIST"
		if file != "" {
			pwd, err := f.sendcmd("PWD")
			if err != nil {
				return wrap(err)
			}
			cerr := f.cwd(file)
			var fe *ftpError
			if errors.As(cerr, &fe) && fe.class == "error_perm" {
				f.cwd(pyParse257(pwd))
				return wrap(&uriURLError{"ftp error: " + fe.repr()})
			} else if cerr != nil {
				return wrap(cerr)
			}
			if err := f.cwd(pyParse257(pwd)); err != nil {
				return wrap(err)
			}
			cmd = "LIST " + file
		}
		if data, err = f.transfercmd(cmd, addr); err != nil {
			return wrap(err)
		}
	}
	// uri reads the body before int(None) fails.
	if c.timeout > 0 {
		data.SetDeadline(time.Now().Add(c.timeout))
	}
	io.Copy(io.Discard, data)
	data.Close()
	return errNoStatus
}

func isFTPError(err error) bool {
	var fe *ftpError
	var eof *ftpEOFError
	return errors.As(err, &fe) || errors.As(err, &eof)
}

// pyParse257 is ftplib.parse257: the directory of a PWD reply.
func pyParse257(resp string) string {
	if !strings.HasPrefix(resp, "257") || len(resp) < 5 || resp[3:5] != ` "` {
		return ""
	}
	var b strings.Builder
	for i := 5; i < len(resp); i++ {
		c := resp[i]
		if c == '"' {
			if i+1 >= len(resp) || resp[i+1] != '"' {
				break
			}
			i++
		}
		b.WriteByte(c)
	}
	return b.String()
}
