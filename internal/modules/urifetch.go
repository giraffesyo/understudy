package modules

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/resolv"
)

// This file ports module_utils.urls' fetch_url/open_url on net/http:
// urllib's opener (HTTPRedirectHandler with ansible's follow_redirects
// policies, the basic/digest auth handlers, a cookie jar, proxies and a
// unix-socket connection) and fetch_url's info dict, errors included.

func sortStrings(s []string) { sort.Strings(s) }

// uriHeaders is an ordered header dict keyed case-insensitively (urllib
// capitalizes names, so "Content-Type" and "content-type" collide).
type uriHeaders struct {
	names  []string
	values map[string]string
	orig   map[string]string
}

func newURIHeaders() *uriHeaders {
	return &uriHeaders{values: map[string]string{}, orig: map[string]string{}}
}

func (h *uriHeaders) set(name, value string) {
	k := strings.ToLower(name)
	if _, ok := h.values[k]; !ok {
		h.names = append(h.names, k)
	}
	h.values[k] = value
	h.orig[k] = name
}

func (h *uriHeaders) has(name string) bool {
	_, ok := h.values[strings.ToLower(name)]
	return ok
}

func (h *uriHeaders) get(name string) string { return h.values[strings.ToLower(name)] }

func (h *uriHeaders) del(name string) {
	k := strings.ToLower(name)
	if _, ok := h.values[k]; !ok {
		return
	}
	delete(h.values, k)
	delete(h.orig, k)
	for i, n := range h.names {
		if n == k {
			h.names = append(h.names[:i], h.names[i+1:]...)
			break
		}
	}
}

func (h *uriHeaders) clone() *uriHeaders {
	c := newURIHeaders()
	for _, k := range h.names {
		c.set(h.orig[k], h.values[k])
	}
	return c
}

// uriRequest is a urllib.request.Request.
type uriRequest struct {
	method       string
	url          string
	data         []byte
	hasData      bool
	headers      *uriHeaders // add_header: survive redirects
	unredirected *uriHeaders // add_unredirected_header
	visited      map[string]int
}

// uriResponse is the response (or the HTTPError) fetch_url returns.
type uriResponse struct {
	code      int
	reason    string
	header    http.Header
	order     []string // lowercased header names in arrival order
	body      []byte
	url       string
	httpError bool
}

// uriInfo is fetch_url's info dict.
type uriInfo struct {
	fields  map[string]any
	body    []byte
	hasBody bool
	fatal   string // fail_json(msg=...) raised inside fetch_url
	crash   string // the exception uri raises on the response
}

func (i *uriInfo) status() int {
	switch t := i.fields["status"].(type) {
	case int:
		return t
	case int64:
		return int(t)
	}
	return -1
}

func (i *uriInfo) popBody() []byte {
	delete(i.fields, "body")
	if !i.hasBody {
		return []byte{}
	}
	return i.body
}

// uriHTTPError is urllib.error.HTTPError raised inside the opener.
type uriHTTPError struct {
	resp   *uriResponse
	msg    string
	noBody bool // raised without a response body (fp=None)
}

func (e *uriHTTPError) Error() string { return e.msg }

// uriURLError is urllib.error.URLError (a request that never got a
// response); reason is str(reason).
type uriURLError struct{ reason string }

func (e *uriURLError) Error() string { return "<urlopen error " + e.reason + ">" }

// uriConnError is an OSError raised while reading the response.
type uriConnError struct{ msg string }

func (e *uriConnError) Error() string { return e.msg }

// uriValueError is a ValueError from urllib (a malformed URL, an
// unsupported auth scheme): fetch_url fails the module with it.
type uriValueError struct{ msg string }

func (e *uriValueError) Error() string { return e.msg }

// uriDialError carries a connect failure already rendered like Python's
// OSError.
type uriDialError struct{ msg string }

func (e *uriDialError) Error() string { return e.msg }

type uriClient struct {
	env              *RunEnv
	p                *args.Parsed
	timeout          time.Duration
	unixSocket       string
	followRedirects  string
	username         string
	password         string
	authNetloc       string
	forceBasicAuth   bool
	useProxy         bool
	decompress       bool
	unredirectedList map[string]bool
	jar              *uriCookieJar
	digestNonce      string
	digestCount      int
	tlsConf          *tls.Config
	tlsErr           string
	validate         bool
	roots            *x509.CertPool
}

func newURIClient(env *RunEnv, p *args.Parsed) *uriClient {
	c := &uriClient{
		env:              env,
		p:                p,
		timeout:          time.Duration(p.Int("timeout")) * time.Second,
		followRedirects:  p.Str("follow_redirects"),
		forceBasicAuth:   p.Bool("force_basic_auth"),
		useProxy:         p.Bool("use_proxy"),
		decompress:       p.Bool("decompress"),
		unredirectedList: map[string]bool{},
		jar:              &uriCookieJar{},
	}
	if p.Has("unix_socket") {
		c.unixSocket = pyExpandPath(p.Str("unix_socket"))
	}
	for _, h := range strList(p.List("unredirected_headers")) {
		c.unredirectedList[strings.ToLower(h)] = true
	}
	return c
}

// fetch is fetch_url: the response (nil when none arrived) and info.
func (c *uriClient) fetch(rawURL, method string, data []byte, hasData bool, userHeaders *uriHeaders, lastMod *time.Time) (*uriResponse, *uriInfo) {
	info := &uriInfo{fields: map[string]any{"url": MaskURL(rawURL), "status": -1}}
	if c.p.Bool("use_gssapi") {
		info.fields = map[string]any{}
		info.fatal = missingRequiredLib(c.env, "gssapi", "for use_gssapi=True", "https://pypi.org/project/gssapi/")
		return nil, info
	}

	reqURL, authHeader, err := c.configureAuth(rawURL)
	if err != nil {
		info.fatal = err.Error()
		return nil, info
	}
	c.tlsConf, c.tlsErr = c.makeTLSConfig()

	req := &uriRequest{method: method, url: reqURL, data: data, hasData: hasData,
		headers: newURIHeaders(), unredirected: newURIHeaders()}
	if agent := c.p.Str("http_agent"); agent != "" {
		req.headers.set("User-agent", agent)
	}
	if c.p.Bool("force") {
		req.headers.set("cache-control", "no-cache")
	} else if lastMod != nil {
		req.headers.set("If-Modified-Since", lastMod.Format("Mon, 02 Jan 2006 15:04:05")+" GMT")
	}
	all := userHeaders.clone()
	if authHeader != "" {
		all.set("Authorization", authHeader)
	}
	for _, k := range all.names {
		if c.unredirectedList[k] {
			req.unredirected.set(all.orig[k], all.values[k])
		} else {
			req.headers.set(all.orig[k], all.values[k])
		}
	}

	resp, err := c.open(req)
	if err == errNoStatus {
		// file:// and ftp:// responses have no status: uri's
		// int(resp['status']) raises.
		info.crash = pyIntNoneMsg(c.env)
		return nil, info
	}
	switch e := err.(type) {
	case nil:
		for _, k := range resp.order {
			info.fields[k] = strings.Join(resp.header.Values(k), ", ")
		}
		cookies := map[string]any{}
		var parts []string
		for _, ck := range c.jar.sorted() {
			cookies[ck.name] = ck.value
			parts = append(parts, ck.name+"="+ck.value)
		}
		info.fields["cookies_string"] = strings.Join(parts, "; ")
		info.fields["cookies"] = cookies
		length := resp.header.Get("Content-Length")
		if _, ok := resp.header["Content-Length"]; !ok {
			length = "unknown"
		}
		info.fields["msg"] = "OK (" + length + " bytes)"
		info.fields["url"] = MaskURL(resp.url)
		info.fields["status"] = resp.code
		return resp, info
	case *uriHTTPError:
		e.resp.httpError = true
		if e.noBody {
			e.resp.body = []byte{}
		}
		for _, k := range e.resp.order {
			vals := e.resp.header.Values(k)
			info.fields[k] = vals[len(vals)-1]
		}
		info.fields["msg"] = e.msg
		info.fields["body"] = e.resp.body
		info.body, info.hasBody = e.resp.body, true
		info.fields["status"] = e.resp.code
		return e.resp, info
	case *uriValueError:
		info.fatal = e.msg
	case *uriURLError:
		info.fields["msg"] = "Request failed: " + e.Error()
		info.fields["status"] = -1
	case *uriConnError:
		info.fields["msg"] = "Connection failure: " + e.msg
		info.fields["status"] = -1
	default:
		info.fields["msg"] = "An unknown error occurred: " + err.Error()
		info.fields["status"] = -1
	}
	return nil, info
}

// configureAuth is _configure_auth: credentials from the URL, the auth
// handlers' password, a forced basic header or ~/.netrc.
func (c *uriClient) configureAuth(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "ftp" {
		// ftp:// keeps its credentials for FTPHandler's login.
		return rawURL, "", nil
	}
	username, password := c.p.Str("url_username"), c.p.Str("url_password")
	netloc := u.Host
	if username == "" && u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
		rawURL = u.String()
	}
	switch {
	case username != "" && !c.forceBasicAuth:
		c.username, c.password, c.authNetloc = username, password, netloc
	case username != "" && c.forceBasicAuth:
		return rawURL, basicAuth(username, password), nil
	case c.p.Bool("use_netrc"):
		if login, pw, ok := netrcLookup(c.envVar("NETRC"), u.Hostname()); ok && login != "" && pw != "" {
			return rawURL, basicAuth(login, pw), nil
		}
	}
	return rawURL, "", nil
}

func basicAuth(user, pw string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
}

// envVar reads the module process environment (task environment first).
func (c *uriClient) envVar(name string) string {
	if v, ok := c.env.Env[name]; ok {
		return v
	}
	return os.Getenv(name)
}

// netrcLookup is netrc.netrc(file).authenticators(host).
func netrcLookup(file, host string) (string, string, bool) {
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		file = home + "/.netrc"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", "", false
	}
	toks := strings.Fields(string(data))
	type entry struct{ login, password string }
	hosts := map[string]entry{}
	var def *entry
	for i := 0; i < len(toks); i++ {
		var cur entry
		name := ""
		switch toks[i] {
		case "machine":
			if i+1 < len(toks) {
				name = toks[i+1]
				i++
			}
		case "default":
		case "macdef":
			continue
		default:
			continue
		}
		isDefault := name == ""
		for i+1 < len(toks) && toks[i+1] != "machine" && toks[i+1] != "default" && toks[i+1] != "macdef" {
			key := toks[i+1]
			if i+2 >= len(toks) {
				i++
				break
			}
			val := toks[i+2]
			switch key {
			case "login", "user":
				cur.login = val
			case "password":
				cur.password = val
			}
			i += 2
		}
		if isDefault {
			e := cur
			def = &e
		} else if _, ok := hosts[name]; !ok {
			hosts[name] = cur
		}
	}
	if e, ok := hosts[host]; ok {
		return e.login, e.password, true
	}
	if def != nil {
		return def.login, def.password, true
	}
	return "", "", false
}

// makeTLSConfig is make_context; its failures surface on every request
// (as OSErrors from open_url), plain http included.
func (c *uriClient) makeTLSConfig() (*tls.Config, string) {
	conf := &tls.Config{NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true}
	validate := c.p.Bool("validate_certs")
	var roots *x509.CertPool
	if c.p.Has("ca_path") {
		data, err := os.ReadFile(pyExpandPath(c.p.Str("ca_path")))
		if err != nil {
			return nil, pyErrnoOnly(err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, "[X509: NO_CERTIFICATE_OR_CRL_FOUND] no certificate or crl found" + sslSuffix(c.env, sslCAFile)
		}
	}
	if list := strList(c.p.List("ciphers")); len(list) > 0 {
		suites, ok := opensslCipherSuites(strings.Join(list, ":"))
		if !ok {
			return nil, "('No cipher can be selected.',)"
		}
		conf.CipherSuites = suites
	}
	if c.p.Has("client_cert") {
		certFile := pyExpandPath(c.p.Str("client_cert"))
		keyFile := certFile
		if c.p.Has("client_key") {
			keyFile = pyExpandPath(c.p.Str("client_key"))
		}
		for _, f := range []string{certFile, keyFile} {
			if _, err := os.Stat(f); err != nil {
				return nil, pyErrnoOnly(err)
			}
		}
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, "[SSL] PEM lib" + sslSuffix(c.env, sslCertChain)
		}
		conf.Certificates = []tls.Certificate{pair}
	}
	c.validate, c.roots = validate, roots
	return conf, ""
}

// pyErrnoOnly is str(OSError(errno, strerror)) without a filename.
func pyErrnoOnly(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s", int(errno), pyStrerror(errno))
	}
	return err.Error()
}

// verifyPeer checks the server certificate like OpenSSL with
// check_hostname: chain errors win over a name mismatch.
func verifyPeer(cs tls.ConnectionState, roots *x509.CertPool, host string) error {
	certs := cs.PeerCertificates
	if len(certs) == 0 {
		return &uriSSLError{"certificate verify failed: unable to get local issuer certificate"}
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
	for _, ic := range certs[1:] {
		opts.Intermediates.AddCert(ic)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		leaf, last := certs[0], certs[len(certs)-1]
		now := time.Now()
		var ci x509.CertificateInvalidError
		switch {
		case errors.As(err, &ci) && ci.Reason == x509.Expired && now.Before(ci.Cert.NotBefore):
			return &uriSSLError{"certificate verify failed: certificate is not yet valid"}
		case errors.As(err, &ci) && ci.Reason == x509.Expired:
			return &uriSSLError{"certificate verify failed: certificate has expired"}
		case selfSigned(leaf):
			return &uriSSLError{"certificate verify failed: self-signed certificate"}
		case len(certs) > 1 && selfSigned(last):
			return &uriSSLError{"certificate verify failed: self-signed certificate in certificate chain"}
		}
		return &uriSSLError{"certificate verify failed: unable to get local issuer certificate"}
	}
	if err := certs[0].VerifyHostname(host); err != nil {
		if net.ParseIP(host) != nil {
			return &uriSSLError{fmt.Sprintf("certificate verify failed: IP address mismatch, certificate is not valid for '%s'.", host)}
		}
		return &uriSSLError{fmt.Sprintf("certificate verify failed: Hostname mismatch, certificate is not valid for '%s'.", host)}
	}
	return nil
}

func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil
}

// uriSSLError is an ssl.SSLCertVerificationError.
type uriSSLError struct{ reason string }

func (e *uriSSLError) Error() string {
	return "[SSL: CERTIFICATE_VERIFY_FAILED] " + e.reason
}

// opensslCiphers maps OpenSSL cipher names to the TLS 1.2 suites Go
// implements.
var opensslCiphers = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-AES128-SHA256":     tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-RSA-AES128-SHA256":       tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-ECDSA-AES128-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"ECDHE-RSA-AES128-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"ECDHE-ECDSA-AES256-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	"ECDHE-RSA-AES256-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	"AES128-GCM-SHA256":             tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"AES256-GCM-SHA384":             tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"AES128-SHA256":                 tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
	"AES128-SHA":                    tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	"AES256-SHA":                    tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	"DES-CBC3-SHA":                  tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

// opensslCipherSuites resolves an OpenSSL cipher string: known names are
// used, keywords (DEFAULT, HIGH, !aNULL, @SECLEVEL=...) keep Go's
// defaults; nothing selectable is an error.
func opensslCipherSuites(spec string) ([]uint16, bool) {
	var out []uint16
	keyword := false
	for _, tok := range strings.FieldsFunc(spec, func(r rune) bool { return r == ':' || r == ',' || r == ' ' }) {
		if id, ok := opensslCiphers[tok]; ok {
			out = append(out, id)
			continue
		}
		t := strings.TrimLeft(tok, "!-+")
		if t == tok && (strings.ToUpper(t) == t || strings.HasPrefix(t, "@")) && !strings.Contains(t, "-") {
			keyword = true
		}
	}
	if len(out) == 0 && !keyword {
		return nil, false
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// open is OpenerDirector.open: request, cookies, auth retries and
// redirects until a final response or an error.
func (c *uriClient) open(req *uriRequest) (*uriResponse, error) {
	authTried := ""
	for {
		resp, err := c.roundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.code >= 200 && resp.code < 300 {
			return resp, nil
		}
		switch resp.code {
		case 401:
			retry, err := c.authRetry(req, resp, &authTried)
			if err != nil {
				return nil, err
			}
			if retry {
				continue
			}
		case 301, 302, 303, 307, 308:
			next, err := c.redirect(req, resp)
			if err != nil {
				return nil, err
			}
			if next != nil {
				req = next
				continue
			}
		}
		resp.httpError = true
		return nil, &uriHTTPError{resp: resp, msg: fmt.Sprintf("HTTP Error %d: %s", resp.code, resp.reason)}
	}
}

// authRetry is the digest (handler_order 490) then basic auth handlers'
// http_error_401.
func (c *uriClient) authRetry(req *uriRequest, resp *uriResponse, tried *string) (bool, error) {
	if c.username == "" {
		return false, nil // no auth handlers installed
	}
	chal := resp.header.Get("Www-Authenticate")
	if strings.TrimSpace(chal) == "" {
		return false, nil
	}
	scheme := strings.Fields(chal)[0]
	switch strings.ToLower(scheme) {
	case "digest":
		if !c.hostMatches(req.url) {
			return false, nil
		}
		c.digestCount++
		if c.digestCount > 5 {
			return false, &uriHTTPError{resp: resp, msg: fmt.Sprintf("HTTP Error %d: digest auth failed", resp.code), noBody: true}
		}
		auth := c.digestAuth(req, chal)
		if auth == "" || req.unredirected.get("Authorization") == auth {
			return false, nil
		}
		req.unredirected.set("Authorization", auth)
		return true, nil
	case "basic":
		if !c.hostMatches(req.url) {
			return false, nil
		}
		auth := basicAuth(c.username, c.password)
		if req.headers.get("Authorization") == auth || req.unredirected.get("Authorization") == auth || *tried == auth {
			return false, nil
		}
		*tried = auth
		req.unredirected.set("Authorization", auth)
		return true, nil
	}
	return false, &uriValueError{fmt.Sprintf("AbstractDigestAuthHandler does not support the following scheme: '%s'", scheme)}
}

func (c *uriClient) hostMatches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return hostPort(u.Scheme, u.Host) == hostPort(u.Scheme, c.authNetloc)
}

func hostPort(scheme, host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(host)
	}
	port := "80"
	if scheme == "https" {
		port = "443"
	}
	return strings.ToLower(host) + ":" + port
}

var digestParamRe = regexp.MustCompile(`([a-zA-Z]+)=(?:"([^"]*)"|([^,\s]*))`)

// digestAuth is AbstractDigestAuthHandler.get_authorization.
func (c *uriClient) digestAuth(req *uriRequest, chalHeader string) string {
	chal := map[string]string{}
	for _, m := range digestParamRe.FindAllStringSubmatch(chalHeader[len("Digest"):], -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		chal[strings.ToLower(m[1])] = v
	}
	realm, nonce, qop, opaque := chal["realm"], chal["nonce"], chal["qop"], chal["opaque"]
	algorithm := chal["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}
	var newHash func() hash.Hash
	switch strings.ToUpper(algorithm) {
	case "MD5":
		newHash = md5.New
	case "SHA":
		newHash = sha1.New
	case "SHA-256":
		newHash = sha256.New
	default:
		return ""
	}
	H := func(s string) string {
		h := newHash()
		h.Write([]byte(s))
		return hex.EncodeToString(h.Sum(nil))
	}
	u, _ := url.Parse(req.url)
	selector := u.RequestURI()
	a1 := c.username + ":" + realm + ":" + c.password
	a2 := req.method + ":" + selector
	var respdig, ncvalue, cnonce string
	switch {
	case qop == "":
		respdig = H(H(a1) + ":" + nonce + ":" + H(a2))
	case qopHas(strings.Split(qop, ","), "auth"):
		if nonce == c.digestNonce {
			c.digestCount++
		} else {
			c.digestCount = 1
			c.digestNonce = nonce
		}
		ncvalue = fmt.Sprintf("%08x", c.digestCount)
		b := make([]byte, 8)
		rand.Read(b)
		h := sha1.Sum([]byte(fmt.Sprintf("%d:%s:%s:%x", c.digestCount, nonce, time.Now().Format(time.ANSIC), b)))
		cnonce = hex.EncodeToString(h[:])[:16]
		respdig = H(H(a1) + ":" + nonce + ":" + ncvalue + ":" + cnonce + ":auth:" + H(a2))
	default:
		return ""
	}
	base := fmt.Sprintf(`username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, c.username, realm, nonce, selector, respdig)
	if opaque != "" {
		base += fmt.Sprintf(`, opaque="%s"`, opaque)
	}
	base += fmt.Sprintf(`, algorithm="%s"`, algorithm)
	if qop != "" {
		base += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, ncvalue, cnonce)
	}
	return "Digest " + base
}

func qopHas(list []string, w string) bool {
	for _, s := range list {
		if strings.TrimSpace(s) == w {
			return true
		}
	}
	return false
}

const redirectLoopMsg = "The HTTP server returned a redirect error that would lead to an infinite loop.\nThe last 30x error message was:\n"

// redirect is HTTPRedirectHandler.http_error_302 with ansible's
// redirect_request: the next request, nil to stop, or an HTTPError.
func (c *uriClient) redirect(req *uriRequest, resp *uriResponse) (*uriRequest, error) {
	loc := resp.header.Get("Location")
	if _, ok := resp.header["Location"]; !ok {
		if _, ok := resp.header["Uri"]; !ok {
			return nil, nil
		}
		loc = resp.header.Get("Uri")
	}
	httpErr := func(msg string) error {
		resp.httpError = true
		return &uriHTTPError{resp: resp, msg: fmt.Sprintf("HTTP Error %d: %s", resp.code, msg)}
	}
	if lu, err := url.Parse(loc); err == nil {
		switch strings.ToLower(lu.Scheme) {
		case "http", "https", "ftp", "":
		default:
			return nil, httpErr(fmt.Sprintf("%s - Redirection to url '%s' is not allowed", resp.reason, loc))
		}
		if lu.Path == "" && lu.Host != "" {
			lu.Path = "/"
			loc = lu.String()
		}
	}
	newURL := pyQuoteURL(urlJoin(req.url, loc))

	method := req.method
	switch c.followRedirects {
	case "urllib2", "urllib":
		ok := (method == "GET" || method == "HEAD") ||
			(method == "POST" && (resp.code == 301 || resp.code == 302 || resp.code == 303))
		if !ok {
			return nil, httpErr(resp.reason)
		}
	case "no", "none":
		return nil, httpErr(resp.reason)
	case "all", "yes":
	case "safe":
		if method != "GET" && method != "HEAD" {
			return nil, httpErr(resp.reason)
		}
	default:
		return nil, httpErr(resp.reason)
	}

	visited := req.visited
	if visited == nil {
		visited = map[string]int{}
	} else if visited[newURL] >= 4 || len(visited) >= 10 {
		return nil, httpErr(redirectLoopMsg + resp.reason)
	}
	visited[newURL]++

	next := &uriRequest{url: strings.ReplaceAll(newURL, " ", "%20"), unredirected: newURIHeaders(), visited: visited}
	if c.followRedirects == "urllib2" || c.followRedirects == "urllib" {
		next.method = "GET"
		if method == "HEAD" {
			next.method = "HEAD"
		}
		next.headers = req.headers.clone()
		next.headers.del("content-length")
		next.headers.del("content-type")
		return next, nil
	}
	next.method = method
	if resp.code == 307 || resp.code == 308 {
		next.headers = req.headers.clone()
		next.data, next.hasData = req.data, req.hasData
		return next, nil
	}
	next.headers = req.headers.clone()
	for _, h := range []string{"content-length", "content-type", "transfer-encoding"} {
		next.headers.del(h)
	}
	if (resp.code == 303 || resp.code == 302) && method != "HEAD" {
		next.method = "GET"
	}
	if resp.code == 301 && method == "POST" {
		next.method = "GET"
	}
	return next, nil
}

// pyQuoteURL is quote(url, encoding="iso-8859-1", safe=string.punctuation).
func pyQuoteURL(s string) string {
	const punct = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(punct, r)):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}

// urlJoin is urllib.parse.urljoin.
func urlJoin(base, ref string) string {
	if scheme, netloc, _, _, _, ok := pyURLSplit(base); ok && strings.Contains(netloc, "@") {
		// net/url re-escapes userinfo ("****" as "%2A%2A%2A%2A"), where
		// urljoin keeps the base's netloc verbatim: join against the bare
		// host, then put the netloc back on a result relative to it.
		host := netloc[strings.LastIndexByte(netloc, '@')+1:]
		out := urlJoin(strings.Replace(base, netloc, host, 1), ref)
		refScheme, _, _, _, _, _ := pyURLSplit(ref)
		prefix := scheme + "://" + host
		if refScheme == "" && !strings.HasPrefix(ref, "//") && strings.HasPrefix(out, prefix) &&
			(len(out) == len(prefix) || strings.ContainsRune("/?#", rune(out[len(prefix)]))) {
			return scheme + "://" + netloc + out[len(prefix):]
		}
		return out
	}
	bu, err := url.Parse(base)
	if err != nil {
		return ref
	}
	ru, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return bu.ResolveReference(ru).String()
}

// deadlineConn applies the socket timeout to every read and write, as a
// Python socket's settimeout does.
type deadlineConn struct {
	net.Conn
	d time.Duration
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	if c.d > 0 {
		c.Conn.SetReadDeadline(time.Now().Add(c.d))
	}
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	if c.d > 0 {
		c.Conn.SetWriteDeadline(time.Now().Add(c.d))
	}
	return c.Conn.Write(b)
}

// unixPathMax is sizeof(sockaddr_un.sun_path).
func unixPathMax() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
}

func (c *uriClient) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: c.timeout}
	if c.unixSocket != "" {
		if len(c.unixSocket) > unixPathMax() {
			return nil, &uriDialError{fmt.Sprintf("Invalid Socket File (%s): AF_UNIX path too long", c.unixSocket)}
		}
		conn, err := d.DialContext(ctx, "unix", c.unixSocket)
		if err != nil {
			return nil, &uriDialError{fmt.Sprintf("Invalid Socket File (%s): %s", c.unixSocket, pyNetErr(err))}
		}
		return &deadlineConn{conn, c.timeout}, nil
	}
	conn, err := d.DialContext(ctx, network, resolv.Addr(addr))
	if err != nil {
		return nil, &uriDialError{pyNetErr(err)}
	}
	return &deadlineConn{conn, c.timeout}, nil
}

// pyGaiError is str(socket.gaierror) for a failed lookup: getaddrinfo's
// code and the C library's gai_strerror text (glibc's, musl's or macOS's,
// whichever the target's Python is linked with).
func pyGaiError(dnsErr *net.DNSError) string {
	if runtime.GOOS == "darwin" {
		return "[Errno 8] nodename nor servname provided, or not known"
	}
	return resolv.GaiError(dnsErr)
}

// pyNetErr renders a socket error like str(OSError).
func pyNetErr(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return pyGaiError(dnsErr)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s", int(errno), pyStrerror(errno))
	}
	return err.Error()
}

// proxyFor is urllib's ProxyHandler over the environment (getproxies +
// proxy_bypass_environment).
func (c *uriClient) proxyFor(req *http.Request) (*url.URL, error) {
	if !c.useProxy || c.unixSocket != "" {
		return nil, nil
	}
	get := func(name string) string {
		if v := c.envVar(strings.ToLower(name)); v != "" {
			return v
		}
		return c.envVar(strings.ToUpper(name))
	}
	proxy := get(req.URL.Scheme + "_proxy")
	if proxy == "" {
		return nil, nil
	}
	if noProxy := get("no_proxy"); noProxy != "" {
		if noProxy == "*" {
			return nil, nil
		}
		host := strings.ToLower(req.URL.Hostname())
		hostPort := strings.ToLower(req.URL.Host)
		for _, name := range strings.Split(noProxy, ",") {
			name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), ".")
			if name == "" {
				continue
			}
			if host == name || strings.HasSuffix(host, "."+name) || hostPort == name || strings.HasSuffix(hostPort, "."+name) {
				return nil, nil
			}
		}
	}
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	return url.Parse(proxy)
}

// roundTrip performs one HTTP exchange (no redirects, no auth retries).
func (c *uriClient) roundTrip(req *uriRequest) (*uriResponse, error) {
	u, err := url.Parse(req.url)
	if err != nil || u.Scheme == "" {
		return nil, &uriValueError{fmt.Sprintf("unknown url type: %s", pyStrRepr(req.url))}
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "file", "ftp":
	default:
		return nil, &uriURLError{"unknown url type: " + strings.ToLower(u.Scheme)}
	}
	if c.tlsErr != "" {
		return nil, &uriConnError{c.tlsErr}
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		return nil, c.openFileURL(u)
	case "ftp":
		return nil, c.openFTPURL(u)
	}
	if u.Host == "" {
		return nil, &uriURLError{"no host given"}
	}

	var body io.Reader
	if req.hasData {
		body = bytes.NewReader(req.data)
	}
	hr, err := http.NewRequest(req.method, req.url, body)
	if err != nil {
		return nil, &uriValueError{err.Error()}
	}
	hr.Close = true
	for _, h := range []*uriHeaders{req.headers, req.unredirected} {
		for _, k := range h.names {
			switch k {
			case "host":
				hr.Host = h.values[k]
			case "content-length":
				if n, err := strconv.ParseInt(h.values[k], 10, 64); err == nil {
					hr.ContentLength = n
				}
			default:
				hr.Header.Set(h.orig[k], h.values[k])
			}
		}
	}
	if req.hasData && hr.Header.Get("Content-Type") == "" {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if hr.Header.Get("Accept-Encoding") == "" {
		hr.Header.Set("Accept-Encoding", "identity")
	}
	if hr.Header.Get("User-Agent") == "" {
		hr.Header.Set("User-Agent", "Python-urllib/3")
	}
	if ck := c.jar.header(u); ck != "" && hr.Header.Get("Cookie") == "" {
		hr.Header.Set("Cookie", ck)
	}

	wrote := false
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true }}
	hr = hr.WithContext(httptrace.WithClientTrace(context.Background(), trace))

	tlsConf := c.tlsConf.Clone()
	if c.validate {
		// OpenSSL order: the chain first, then the host name.
		host := u.Hostname()
		tlsConf.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyPeer(cs, c.roots, host)
		}
	}
	tr := &http.Transport{
		Proxy:               c.proxyFor,
		DialContext:         c.dial,
		TLSClientConfig:     tlsConf,
		TLSHandshakeTimeout: c.timeout,
		DisableCompression:  true,
		DisableKeepAlives:   true,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	hresp, err := client.Do(hr)
	if err != nil {
		return nil, c.classifyTransportErr(err, wrote)
	}
	defer hresp.Body.Close()
	data, err := io.ReadAll(hresp.Body)
	if err != nil {
		return nil, c.classifyTransportErr(err, true)
	}
	resp := &uriResponse{code: hresp.StatusCode, reason: httpReason(hresp), header: hresp.Header.Clone(),
		body: data, url: req.url}
	if len(hresp.TransferEncoding) > 0 && resp.header.Get("Transfer-Encoding") == "" {
		resp.header.Set("Transfer-Encoding", strings.Join(hresp.TransferEncoding, ", "))
	}
	resp.order = headerOrder(resp.header)
	c.jar.extract(u, hresp)
	if c.decompress && strings.ToLower(resp.header.Get("Content-Encoding")) == "gzip" {
		if plain, err := uriGunzip(data); err == nil {
			resp.body = plain
		}
	}
	return resp, nil
}

// headerOrder lists the (lowercased) header names; the result dict is
// sorted when displayed, so any stable order does.
func headerOrder(h http.Header) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	return names
}

func (c *uriClient) classifyTransportErr(err error, wrote bool) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var de *uriDialError
	if errors.As(err, &de) {
		return &uriURLError{de.msg}
	}
	var se *uriSSLError
	if errors.As(err, &se) {
		return &uriURLError{se.Error() + sslSuffix(c.env, sslHandshake)}
	}
	var rhe tls.RecordHeaderError
	if errors.As(err, &rhe) {
		return &uriURLError{"[SSL: WRONG_VERSION_NUMBER] wrong version number" + sslSuffix(c.env, sslHandshake)}
	}
	var ne net.Error
	timeout := errors.As(err, &ne) && ne.Timeout()
	if !wrote {
		if timeout {
			return &uriURLError{"timed out"}
		}
		if strings.Contains(err.Error(), "tls:") {
			return &uriURLError{"[SSL] " + strings.TrimPrefix(err.Error(), "remote error: ") + sslSuffix(c.env, sslHandshake)}
		}
		return &uriURLError{pyNetErr(err)}
	}
	switch {
	case timeout:
		return &uriConnError{"timed out"}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return &uriConnError{"Remote end closed connection without response"}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return &uriConnError{fmt.Sprintf("[Errno %d] %s", int(errno), pyStrerror(errno))}
	}
	if strings.Contains(err.Error(), "malformed HTTP") {
		return &uriConnError{"connection was closed before a valid response was received: " + err.Error()}
	}
	return errors.New(err.Error())
}

// uriCookieJar is a minimal http.cookiejar.CookieJar.
type uriCookieJar struct{ cookies []*uriCookie }

type uriCookie struct {
	name, value, domain, path string
	hostOnly, secure          bool
}

// cookieDomain is the jar's domain for a request host: eff_request_host
// (a dotless name gets ".local").
func cookieDomain(host string) string {
	host = strings.ToLower(host)
	if !strings.Contains(host, ".") {
		return host + ".local"
	}
	return host
}

func (j *uriCookieJar) extract(u *url.URL, resp *http.Response) {
	for _, ck := range resp.Cookies() {
		domain := cookieDomain(u.Hostname())
		hostOnly := true
		if ck.Domain != "" {
			domain = strings.ToLower(ck.Domain)
			if !strings.HasPrefix(domain, ".") {
				domain = "." + domain
			}
			hostOnly = false
		}
		p := ck.Path
		if p == "" || !strings.HasPrefix(p, "/") {
			p = u.EscapedPath()
			if i := strings.LastIndexByte(p, '/'); i >= 0 {
				p = p[:i]
			}
			if p == "" {
				p = "/"
			}
		}
		expired := ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now()))
		kept := j.cookies[:0]
		for _, old := range j.cookies {
			if !(old.name == ck.Name && old.domain == domain && old.path == p) {
				kept = append(kept, old)
			}
		}
		j.cookies = kept
		if !expired {
			j.cookies = append(j.cookies, &uriCookie{name: ck.Name, value: ck.Value, domain: domain, path: p,
				hostOnly: hostOnly, secure: ck.Secure})
		}
	}
}

func (j *uriCookieJar) sorted() []*uriCookie {
	out := append([]*uriCookie(nil), j.cookies...)
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.domain != y.domain {
			return x.domain < y.domain
		}
		if x.path != y.path {
			return x.path < y.path
		}
		return x.name < y.name
	})
	return out
}

func (j *uriCookieJar) header(u *url.URL) string {
	host := cookieDomain(u.Hostname())
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	var match []*uriCookie
	for _, ck := range j.sorted() {
		if ck.secure && u.Scheme != "https" {
			continue
		}
		if ck.hostOnly && ck.domain != host {
			continue
		}
		if !ck.hostOnly && !(host == strings.TrimPrefix(ck.domain, ".") || strings.HasSuffix(host, ck.domain)) {
			continue
		}
		if !strings.HasPrefix(p, ck.path) {
			continue
		}
		match = append(match, ck)
	}
	sort.SliceStable(match, func(a, b int) bool { return len(match[a].path) > len(match[b].path) })
	parts := make([]string, len(match))
	for i, ck := range match {
		parts[i] = ck.name + "=" + ck.value
	}
	return strings.Join(parts, "; ")
}

// uriParseContentType is parse_content_type over email.message's
// get_content_type/get_param: (type, subtype, charset).
func uriParseContentType(h http.Header) (string, string, string) {
	raw, ok := h["Content-Type"]
	ctype := "text/plain"
	charset := "utf-8"
	if ok && len(raw) > 0 {
		v := raw[len(raw)-1]
		if len(raw) > 1 {
			v = raw[0]
		}
		t := strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
		if strings.Count(t, "/") == 1 {
			ctype = t
		}
		if cs, ok := mimeHeaderParam(v, "charset"); ok && cs != "" {
			charset = cs
		}
	}
	ctype = strings.Split(ctype, ",")[0]
	charset = strings.Split(charset, ",")[0]
	_, sub, _ := strings.Cut(ctype, "/")
	return ctype, sub, charset
}

// mimeHeaderParam is Message.get_param(name, header=...).
func mimeHeaderParam(v, name string) (string, bool) {
	parts := strings.Split(v, ";")
	for _, p := range parts[1:] {
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.TrimSpace(val)
		if k == name+"*" {
			if _, rest, ok := strings.Cut(val, "''"); ok {
				return pyUnquote(rest), true
			}
		}
		if k != name {
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = strings.ReplaceAll(strings.ReplaceAll(val[1:len(val)-1], `\"`, `"`), `\\`, `\`)
		}
		return val, true
	}
	return "", false
}
