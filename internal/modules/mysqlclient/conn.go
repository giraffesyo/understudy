// Package mysqlclient is a minimal MySQL/MariaDB client speaking the wire
// protocol directly (stdlib only), for the community.mysql module ports.
//
// It deliberately mirrors PyMySQL — the driver the Ansible modules run on —
// rather than the mysql CLI: the same handshake capabilities, the same
// default character set (utf8mb4 + SET NAMES), the same autocommit
// handling, the same client-side parameter interpolation (Mogrify) and the
// same error rendering, so module results (executed queries, error
// messages) come out identical.
//
// Supported: TCP and unix sockets, TLS via crypto/tls (required when SSL
// options are given, opportunistic otherwise), and the
// mysql_native_password, caching_sha2_password (fast path, and full auth
// over TLS/socket or RSA-OAEP with the server's public key),
// sha256_password, mysql_clear_password and dialog authentication plugins.
package mysqlclient

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Capability flags (CLIENT_*).
const (
	clientLongPassword     = 1
	clientLongFlag         = 4
	clientConnectWithDB    = 8
	clientProtocol41       = 512
	clientSSL              = 2048
	clientTransactions     = 8192
	clientSecureConn       = 32768
	clientMultiResults     = 1 << 17
	clientPluginAuth       = 1 << 19
	clientConnectAttrs     = 1 << 20
	clientPluginAuthLenenc = 1 << 21

	// PyMySQL's CLIENT.CAPABILITIES.
	capabilities = clientLongPassword | clientLongFlag | clientProtocol41 | clientTransactions |
		clientSecureConn | clientMultiResults | clientPluginAuth | clientPluginAuthLenenc | clientConnectAttrs
)

// Server status flags.
const (
	statusInTrans            = 1
	statusAutocommit         = 2
	statusMoreResultsExists  = 8
	statusNoBackslashEscapes = 512
)

const maxPacketLen = 1<<24 - 1

// charsetUTF8MB4 is utf8mb4_general_ci, the collation id PyMySQL sends for
// its default charset.
const charsetUTF8MB4 = 45

// SSL holds the TLS options of a connection (PyMySQL's ssl dict).
type SSL struct {
	CA, Cert, Key string
	// CheckHostname: nil means "verify when a CA is given".
	CheckHostname *bool
}

// Config describes a connection.
type Config struct {
	User       string
	Password   string
	Host       string // default "localhost"
	Port       int    // default 3306
	UnixSocket string // takes precedence over Host/Port
	Database   string
	// ConnectTimeout bounds the socket connect (zero: none).
	ConnectTimeout time.Duration
	// SSL, when set, makes TLS required. When nil, TLS is used
	// opportunistically (unverified) if the server offers it on TCP.
	SSL *SSL
	// NoTLS disables opportunistic TLS (PyMySQL's ssl_disabled).
	NoTLS bool
	// Autocommit, when set, is applied after connecting (SET AUTOCOMMIT).
	Autocommit *bool
	// ClientName/ClientVersion are the _client_name/_client_version
	// connection attributes (default: ConnectorName, ConnectorVersion).
	ClientName, ClientVersion string
}

// Conn is one client connection. It is not safe for concurrent use.
type Conn struct {
	nc  net.Conn
	br  *bufio.Reader
	seq byte

	secure        bool // TLS or unix socket: cleartext auth is allowed
	host          string
	password      []byte
	salt          []byte
	serverCaps    uint32
	status        uint16
	serverVersion string
	pubKey        []byte
}

// ServerVersion is the version string from the server handshake.
func (c *Conn) ServerVersion() string { return c.serverVersion }

// Close closes the connection (sending COM_QUIT).
func (c *Conn) Close() error {
	if c.nc == nil {
		return nil
	}
	c.seq = 0
	_ = c.writePacket([]byte{0x01})
	err := c.nc.Close()
	c.nc = nil
	return err
}

// Connect opens a connection and authenticates.
func Connect(cfg Config) (*Conn, error) {
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	c := &Conn{host: host, password: []byte(cfg.Password)}
	var err error
	if cfg.UnixSocket != "" {
		d := net.Dialer{Timeout: cfg.ConnectTimeout}
		c.nc, err = d.Dial("unix", cfg.UnixSocket)
		c.secure = true
	} else {
		d := net.Dialer{Timeout: cfg.ConnectTimeout}
		c.nc, err = d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if err != nil {
		return nil, &Error{Code: 2003, Msg: fmt.Sprintf("Can't connect to MySQL server on %s (%s)", PyRepr(host), pyOSError(err))}
	}
	c.br = bufio.NewReader(c.nc)
	if err := c.handshake(cfg); err != nil {
		c.nc.Close()
		c.nc = nil
		return nil, err
	}
	// PyMySQL sends SET NAMES after authenticating, then aligns
	// autocommit with the requested mode.
	if _, err := c.Query("SET NAMES utf8mb4"); err != nil {
		c.Close()
		return nil, err
	}
	if cfg.Autocommit != nil && *cfg.Autocommit != (c.status&statusAutocommit != 0) {
		v := "0"
		if *cfg.Autocommit {
			v = "1"
		}
		if _, err := c.Query("SET AUTOCOMMIT = " + v); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// pyOSError renders a dial error as Python's OSError str: "[Errno 111]
// Connection refused", or "timed out".
func pyOSError(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		s := errno.Error()
		if s != "" {
			s = strings.ToUpper(s[:1]) + s[1:]
		}
		return fmt.Sprintf("[Errno %d] %s", int(errno), s)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// The C library's gai_strerror text: glibc's, or musl's (Alpine).
		musl, _ := filepath.Glob("/lib/ld-musl-*.so.1")
		switch {
		case dnsErr.IsNotFound && len(musl) > 0:
			return "[Errno -2] Name does not resolve"
		case dnsErr.IsNotFound:
			return "[Errno -2] Name or service not known"
		case len(musl) > 0:
			return "[Errno -3] Try again"
		}
		return "[Errno -3] Temporary failure in name resolution"
	}
	return err.Error()
}

func (c *Conn) handshake(cfg Config) error {
	data, err := c.readPacket()
	if err != nil {
		return err
	}
	if len(data) > 0 && data[0] == 0xff {
		return parseErrPacket(data)
	}
	if len(data) < 1 || data[0] != 10 {
		return &Error{Code: 2013, Msg: "Lost connection to MySQL server during query"}
	}
	i := 1
	end := indexByteFrom(data, 0, i)
	if end < 0 {
		return malformed()
	}
	c.serverVersion = string(data[i:end])
	i = end + 1 + 4 // thread id
	if len(data) < i+8+1+2 {
		return malformed()
	}
	c.salt = append([]byte{}, data[i:i+8]...)
	i += 9
	c.serverCaps = uint32(binary.LittleEndian.Uint16(data[i:]))
	i += 2
	saltLen := 0
	if len(data) >= i+6 {
		c.status = binary.LittleEndian.Uint16(data[i+1:])
		c.serverCaps |= uint32(binary.LittleEndian.Uint16(data[i+3:])) << 16
		saltLen = int(data[i+5])
		i += 6
		saltLen = max(12, saltLen-9)
	}
	i += 10
	if len(data) >= i+saltLen {
		c.salt = append(c.salt, data[i:i+saltLen]...)
		i += saltLen
	}
	i++
	plugin := ""
	if c.serverCaps&clientPluginAuth != 0 && len(data) >= i {
		if e := indexByteFrom(data, 0, i); e < 0 {
			plugin = string(data[i:])
		} else {
			plugin = string(data[i:e])
		}
	}

	flags := uint32(capabilities)
	if cfg.Database != "" {
		flags |= clientConnectWithDB
	}
	doTLS := false
	if cfg.SSL != nil {
		if c.serverCaps&clientSSL == 0 {
			return &Error{Code: 2026, Msg: "SSL is required but the server doesn't support it"}
		}
		doTLS = true
	} else if !cfg.NoTLS && cfg.UnixSocket == "" && c.serverCaps&clientSSL != 0 {
		doTLS = true
	}
	var head [32]byte
	if doTLS {
		flags |= clientSSL
	}
	binary.LittleEndian.PutUint32(head[0:], flags)
	binary.LittleEndian.PutUint32(head[4:], maxPacketLen)
	head[8] = charsetUTF8MB4
	if doTLS {
		if err := c.writePacket(head[:]); err != nil {
			return err
		}
		tcfg, err := tlsConfig(cfg.SSL, c.host)
		if err != nil {
			return err
		}
		tc := tls.Client(c.nc, tcfg)
		if err := tc.Handshake(); err != nil {
			return &Error{Code: 2026, Msg: "SSL connection error: " + err.Error()}
		}
		c.nc = tc
		c.br = bufio.NewReader(tc)
		c.secure = true
	}

	pkt := append([]byte{}, head[:]...)
	user := cfg.User
	pkt = append(pkt, user...)
	pkt = append(pkt, 0)

	var authResp []byte
	switch plugin {
	case "", "mysql_native_password":
		authResp = scrambleNative(c.password, c.salt)
	case "caching_sha2_password":
		authResp = scrambleSHA256(c.password, c.salt)
	case "sha256_password":
		switch {
		case c.secure:
			authResp = append(append([]byte{}, c.password...), 0)
		case len(c.password) > 0:
			authResp = []byte{1}
		default:
			authResp = []byte{0}
		}
	}
	switch {
	case c.serverCaps&clientPluginAuthLenenc != 0:
		pkt = appendLenenc(pkt, uint64(len(authResp)))
		pkt = append(pkt, authResp...)
	case c.serverCaps&clientSecureConn != 0:
		pkt = append(pkt, byte(len(authResp)))
		pkt = append(pkt, authResp...)
	default:
		pkt = append(pkt, authResp...)
		pkt = append(pkt, 0)
	}
	if cfg.Database != "" && c.serverCaps&clientConnectWithDB != 0 {
		pkt = append(pkt, cfg.Database...)
		pkt = append(pkt, 0)
	}
	if c.serverCaps&clientPluginAuth != 0 {
		pkt = append(pkt, plugin...)
		pkt = append(pkt, 0)
	}
	if c.serverCaps&clientConnectAttrs != 0 {
		var attrs []byte
		name, version := cfg.ClientName, cfg.ClientVersion
		if name == "" {
			name, version = ConnectorName, ConnectorVersion
		}
		for _, kv := range [][2]string{{"_client_name", name}, {"_client_version", version},
			{"_pid", strconv.Itoa(os.Getpid())}} {
			attrs = appendLenencStr(attrs, kv[0])
			attrs = appendLenencStr(attrs, kv[1])
		}
		pkt = appendLenenc(pkt, uint64(len(attrs)))
		pkt = append(pkt, attrs...)
	}
	if err := c.writePacket(pkt); err != nil {
		return err
	}
	return c.authLoop(plugin)
}

// ConnectorName/ConnectorVersion identify this client by the Python
// driver it stands in for: PyMySQL, at the release whose behavior it
// reproduces. Modules report the target's own driver when it has one.
const (
	ConnectorName    = "pymysql"
	ConnectorVersion = "1.1.2"
)

func (c *Conn) authLoop(plugin string) error {
	switched := false
	for {
		data, err := c.readPacket()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return malformed()
		}
		switch data[0] {
		case 0x00:
			c.readOK(data)
			return nil
		case 0xff:
			return parseErrPacket(data)
		case 0xfe:
			if switched {
				return &Error{Code: -1, Msg: "received multiple auth switch requests", Single: true}
			}
			switched = true
			rest := data[1:]
			e := indexByteFrom(rest, 0, 0)
			if e < 0 {
				return &Error{Code: -1, Msg: "received unknown auth switch request", Single: true}
			}
			plugin = string(rest[:e])
			salt := rest[e+1:]
			if n := len(salt); n > 0 && salt[n-1] == 0 {
				salt = salt[:n-1]
			}
			if err := c.authSwitch(plugin, salt); err != nil {
				return err
			}
		case 0x01:
			if err := c.extraAuth(plugin, data); err != nil {
				return err
			}
		default:
			return &Error{Code: -1, Msg: "unexpected packet during authentication", Single: true}
		}
	}
}

func (c *Conn) authSwitch(plugin string, salt []byte) error {
	switch plugin {
	case "mysql_native_password":
		c.salt = salt
		return c.writePacket(scrambleNative(c.password, salt))
	case "caching_sha2_password":
		c.salt = salt
		if len(c.password) == 0 {
			return c.writePacket(nil)
		}
		return c.writePacket(scrambleSHA256(c.password, salt))
	case "sha256_password":
		c.salt = salt
		switch {
		case c.secure:
			return c.writePacket(append(append([]byte{}, c.password...), 0))
		case len(c.password) > 0:
			return c.writePacket([]byte{1})
		default:
			return c.writePacket(nil)
		}
	case "mysql_clear_password":
		return c.writePacket(append(append([]byte{}, c.password...), 0))
	case "dialog":
		return c.writePacket(append(append([]byte{}, c.password...), 0))
	}
	return &Error{Code: 2059, Msg: fmt.Sprintf("Authentication plugin '%s' not configured", pyBytesRepr(plugin))}
}

func (c *Conn) extraAuth(plugin string, data []byte) error {
	switch plugin {
	case "caching_sha2_password":
		if len(data) < 2 {
			return malformed()
		}
		// A PEM key response to an earlier public key request.
		if len(data) > 2 && data[1] == '-' {
			c.pubKey = data[1:]
			return c.sendRSA()
		}
		switch data[1] {
		case 3: // fast auth succeeded; OK follows
			return nil
		case 4:
			if c.secure {
				return c.writePacket(append(append([]byte{}, c.password...), 0))
			}
			if c.pubKey != nil {
				return c.sendRSA()
			}
			return c.writePacket([]byte{2})
		}
		return &Error{Code: -1, Msg: fmt.Sprintf("caching sha2: Unknown result for fast auth: %d", data[1]), Single: true}
	case "sha256_password":
		c.pubKey = data[1:]
		return c.sendRSA()
	}
	return &Error{Code: -1, Msg: fmt.Sprintf("Received extra packet for auth method %s", pyBytesRepr(plugin)), Single: true}
}

func (c *Conn) sendRSA() error {
	enc, err := rsaEncryptPassword(c.password, c.salt, c.pubKey)
	if err != nil {
		return &Error{Code: -1, Msg: err.Error(), Single: true}
	}
	return c.writePacket(enc)
}

func tlsConfig(s *SSL, host string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if s == nil {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	if s.Cert != "" {
		key := s.Key
		if key == "" {
			key = s.Cert
		}
		pair, err := tls.LoadX509KeyPair(s.Cert, key)
		if err != nil {
			return nil, &Error{Code: -1, Msg: err.Error(), Single: true}
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	if s.CA == "" {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	pem, err := os.ReadFile(s.CA)
	if err != nil {
		return nil, &Error{Code: -1, Msg: pyOSErrorPath(err), Single: true}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, &Error{Code: -1, Msg: "[X509] no certificate or crl found", Single: true}
	}
	cfg.RootCAs = pool
	if s.CheckHostname == nil || *s.CheckHostname {
		return cfg, nil
	}
	// Verify the chain but not the host name.
	cfg.InsecureSkipVerify = true
	cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no server certificate")
		}
		certs := make([]*x509.Certificate, len(raw))
		for i, r := range raw {
			cert, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs[i] = cert
		}
		inter := x509.NewCertPool()
		for _, ic := range certs[1:] {
			inter.AddCert(ic)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter})
		return err
	}
	return cfg, nil
}

func pyOSErrorPath(err error) string {
	var pe *os.PathError
	var errno syscall.Errno
	if errors.As(err, &pe) && errors.As(err, &errno) {
		s := errno.Error()
		if s != "" {
			s = strings.ToUpper(s[:1]) + s[1:]
		}
		return fmt.Sprintf("[Errno %d] %s", int(errno), s)
	}
	return err.Error()
}

func malformed() error {
	return &Error{Code: 2013, Msg: "Lost connection to MySQL server during query"}
}

func indexByteFrom(b []byte, c byte, from int) int {
	if from > len(b) {
		return -1
	}
	if i := strings.IndexByte(string(b[from:]), c); i >= 0 {
		return from + i
	}
	return -1
}

// --- packet I/O ---

func (c *Conn) readPacket() ([]byte, error) {
	var out []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			return nil, &Error{Code: 2013, Msg: "Lost connection to MySQL server during query"}
		}
		n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		if hdr[3] != c.seq {
			if hdr[3] == 0 {
				return nil, &Error{Code: 2013, Msg: "Lost connection to MySQL server during query"}
			}
			return nil, &Error{Code: -1, Msg: fmt.Sprintf("Packet sequence number wrong - got %d expected %d", hdr[3], c.seq), Single: true}
		}
		c.seq++
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			return nil, &Error{Code: 2013, Msg: "Lost connection to MySQL server during query"}
		}
		out = append(out, buf...)
		if n < maxPacketLen {
			return out, nil
		}
	}
}

func (c *Conn) writePacket(data []byte) error {
	for {
		n := min(len(data), maxPacketLen)
		buf := make([]byte, 4+n)
		buf[0], buf[1], buf[2] = byte(n), byte(n>>8), byte(n>>16)
		buf[3] = c.seq
		copy(buf[4:], data[:n])
		c.seq++
		if _, err := c.nc.Write(buf); err != nil {
			return &Error{Code: 2006, Msg: fmt.Sprintf("MySQL server has gone away (%s)", err)}
		}
		data = data[n:]
		if n < maxPacketLen {
			return nil
		}
	}
}

func appendLenenc(b []byte, n uint64) []byte {
	switch {
	case n < 251:
		return append(b, byte(n))
	case n < 1<<16:
		return append(b, 0xfc, byte(n), byte(n>>8))
	case n < 1<<24:
		return append(b, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	}
	b = append(b, 0xfe)
	return binary.LittleEndian.AppendUint64(b, n)
}

func appendLenencStr(b []byte, s string) []byte {
	return append(appendLenenc(b, uint64(len(s))), s...)
}

// readLenenc decodes a length-encoded integer at b[i:]; null reports the
// 0xfb NULL marker.
func readLenenc(b []byte, i int) (n uint64, next int, null bool, err error) {
	if i >= len(b) {
		return 0, i, false, malformed()
	}
	switch c := b[i]; {
	case c < 0xfb:
		return uint64(c), i + 1, false, nil
	case c == 0xfb:
		return 0, i + 1, true, nil
	case c == 0xfc:
		if i+3 > len(b) {
			return 0, i, false, malformed()
		}
		return uint64(binary.LittleEndian.Uint16(b[i+1:])), i + 3, false, nil
	case c == 0xfd:
		if i+4 > len(b) {
			return 0, i, false, malformed()
		}
		return uint64(b[i+1]) | uint64(b[i+2])<<8 | uint64(b[i+3])<<16, i + 4, false, nil
	case c == 0xfe:
		if i+9 > len(b) {
			return 0, i, false, malformed()
		}
		return binary.LittleEndian.Uint64(b[i+1:]), i + 9, false, nil
	}
	return 0, i, false, malformed()
}

func readLenencBytes(b []byte, i int) ([]byte, int, bool, error) {
	n, i, null, err := readLenenc(b, i)
	if err != nil || null {
		return nil, i, null, err
	}
	if i+int(n) > len(b) {
		return nil, i, false, malformed()
	}
	return b[i : i+int(n)], i + int(n), false, nil
}
