package dnspy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Resolver is dns.resolver.Resolver: its configuration after reset()
// and read_resolv_conf().
type Resolver struct {
	Domain        Name
	Nameservers   []string
	Port          int
	Search        []Name
	Ndots         int // -1: unset (1)
	Timeout       float64
	Lifetime      float64
	Rotate        bool
	RetryServfail bool
	// EDNS: whether queries carry an OPT record, its payload and flags
	// (DO is 0x8000).
	EDNS      bool
	Payload   uint16
	EDNSFlags uint32

	// now and sleep are the clock (tests replace them).
	now   func() time.Time
	sleep func(time.Duration)
}

// NewResolver is Resolver(configure=True) on POSIX: the defaults, then
// /etc/resolv.conf (path) read. Without nameservers it fails as
// dnspython's NoResolverConfiguration does.
func NewResolver(path string) (*Resolver, error) {
	r := &Resolver{Port: 53, Ndots: -1, Timeout: 2.0, Lifetime: 5.0, Domain: Root}
	if host, err := os.Hostname(); err == nil {
		if n, err := ParseName(host, &Root); err == nil && len(n.Labels) > 1 {
			r.Domain = Name{Labels: n.Labels[1:]}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, &Error{Class: "NoResolverConfiguration", Msg: "cannot open " + path}
	}
	defer f.Close()
	var nameservers []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		tokens := strings.FieldsFunc(l, unicode.IsSpace)
		if len(tokens) < 2 {
			continue
		}
		switch tokens[0] {
		case "nameserver":
			nameservers = append(nameservers, tokens[1])
		case "domain":
			n, err := ParseName(tokens[1], &Root)
			if err != nil {
				return nil, err
			}
			r.Domain, r.Search = n, nil
		case "search":
			r.Search = nil
			for _, s := range tokens[1:] {
				n, err := ParseName(s, &Root)
				if err != nil {
					return nil, err
				}
				r.Search = append(r.Search, n)
			}
		case "options":
			for _, opt := range tokens[1:] {
				switch {
				case opt == "rotate":
					r.Rotate = true
				case opt == "edns0":
					r.EDNS, r.Payload, r.EDNSFlags = true, 1232, 0
				case strings.Contains(opt, "timeout"):
					if _, v, ok := strings.Cut(opt, ":"); ok {
						if n, err := pyInt(strings.SplitN(v, ":", 2)[0]); err == nil {
							r.Timeout = float64(n)
						}
					}
				case strings.Contains(opt, "ndots"):
					if _, v, ok := strings.Cut(opt, ":"); ok {
						if n, err := pyInt(strings.SplitN(v, ":", 2)[0]); err == nil {
							r.Ndots = n
						}
					}
				}
			}
		}
	}
	if len(nameservers) == 0 {
		return nil, &Error{Class: "NoResolverConfiguration", Msg: "no nameservers"}
	}
	if err := r.SetNameservers(nameservers); err != nil {
		return nil, err
	}
	return r, nil
}

// pyInt is int(s) for a decimal string (surrounding whitespace allowed).
func pyInt(s string) (int, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "_", ""))
	return strconv.Atoi(s)
}

// SetNameservers is the nameservers property's setter: each an IP
// address (dnspython's DoH URLs are not supported).
func (r *Resolver) SetNameservers(ns []string) error {
	for _, n := range ns {
		if !IsAddress(n) {
			return valueError("nameserver %s is not a dns.nameserver.Nameserver instance or text form, IP address, nor a valid https URL", n)
		}
	}
	r.Nameservers = append([]string(nil), ns...)
	return nil
}

// Answer is a successful resolution's dns.resolver.Answer.
type Answer struct {
	CanonicalName Name
	RRset         *RRset
}

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Resolver) pause(d time.Duration) {
	if r.sleep != nil {
		r.sleep(d)
		return
	}
	time.Sleep(d)
}

// qnamesToTry is Resolver._get_qnames_to_try with search=True.
func (r *Resolver) qnamesToTry(qname Name) ([]Name, error) {
	if qname.Absolute() {
		return []Name{qname}, nil
	}
	abs, err := qname.Concat(Root)
	if err != nil {
		return nil, err
	}
	var list []Name
	if len(r.Search) > 0 {
		list = r.Search
	} else if !r.Domain.Equal(Root) && r.Domain.Len() > 0 {
		list = []Name{r.Domain}
	}
	ndots := 1
	if r.Ndots >= 0 {
		ndots = r.Ndots
	}
	var out []Name
	for _, s := range list {
		n, err := qname.Concat(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if qname.Len() > ndots {
		return append([]Name{abs}, out...), nil
	}
	return append(out, abs), nil
}

type nameserver struct {
	addr string
	port int
}

func (n nameserver) String() string { return "Do53:" + n.addr + "@" + strconv.Itoa(n.port) }

// Query is Resolver.query (resolve with search=True): qname as text,
// rdtype as text (or a TYPEn), rdclass a class value.
func (r *Resolver) Query(qnameText, rdtypeText string, rdclass uint16, tcp bool) (*Answer, error) {
	qname, err := ParseName(qnameText, nil)
	if err != nil {
		return nil, err
	}
	rdtype, err := TypeFromText(rdtypeText)
	if err != nil {
		return nil, err
	}
	if isMetatype(rdtype) || isMetaclass(rdclass) {
		return nil, errMetaqueries
	}
	qnames, err := r.qnamesToTry(qname)
	if err != nil {
		return nil, err
	}
	start := r.clock()
	for _, qn := range qnames {
		q := question{qn, rdtype, rdclass}
		var servers []nameserver
		for _, a := range r.Nameservers {
			servers = append(servers, nameserver{a, r.Port})
		}
		if r.Rotate {
			rand.Shuffle(len(servers), func(i, j int) { servers[i], servers[j] = servers[j], servers[i] })
		}
		current := append([]nameserver(nil), servers...)
		var errs []serverError
		backoff := 0.10
		var ns nameserver
		retryTCP := false
		nx := false
		for !nx {
			// next_nameserver
			useTCP := false
			pauseFor := 0.0
			if retryTCP {
				useTCP, retryTCP = true, false
			} else {
				if len(current) == 0 {
					if len(servers) == 0 {
						return nil, noNameservers(q, errs)
					}
					current = append([]nameserver(nil), servers...)
					pauseFor = backoff
					backoff = min(backoff*2, 2)
				}
				ns, current = current[0], current[1:]
				useTCP = tcp
			}
			if pauseFor > 0 {
				r.pause(time.Duration(pauseFor * float64(time.Second)))
			}
			// _compute_timeout
			duration := r.clock().Sub(start).Seconds()
			if duration < 0 {
				if duration < -1 {
					return nil, lifetimeTimeout(duration, errs)
				}
				duration = 0
			}
			if duration >= r.Lifetime {
				return nil, lifetimeTimeout(duration, errs)
			}
			timeout := min(r.Lifetime-duration, r.Timeout)
			id := uint16(rand.Intn(65536))
			req := makeQuery(id, q, r.EDNS, r.Payload, r.EDNSFlags)
			var resp *message
			if useTCP {
				resp, err = queryTCP(req, id, q, ns, timeout)
			} else {
				resp, err = queryUDP(req, id, q, ns, timeout)
			}
			remove := func() {
				for i, s := range servers {
					if s == ns {
						servers = append(servers[:i:i], servers[i+1:]...)
						break
					}
				}
			}
			if err != nil {
				errs = append(errs, serverError{ns.String(), err.Error()})
				var e *Error
				if errors.As(err, &e) {
					switch {
					case e.Form || e.EOF || e.OS:
						remove()
					case e == errTruncated:
						if useTCP {
							remove()
						} else {
							retryTCP = true
						}
					}
				}
				continue
			}
			switch rc := resp.rcode(); rc {
			case 0:
				chain, err := resp.resolveChaining()
				if err != nil {
					errs = append(errs, serverError{ns.String(), err.Error()})
					remove()
					continue
				}
				if chain.answer == nil {
					return nil, noAnswer(resp.question[0])
				}
				return &Answer{CanonicalName: chain.canonical, RRset: chain.answer}, nil
			case 3:
				if _, err := resp.resolveChaining(); err != nil {
					errs = append(errs, serverError{ns.String(), err.Error()})
					remove()
					continue
				}
				nx = true
			case 6:
				return nil, errYXDOMAIN
			default:
				if rc != 2 || !r.RetryServfail {
					remove()
				}
				errs = append(errs, serverError{ns.String(), rcodeText(rc)})
			}
		}
	}
	return nil, nxdomain(qnames)
}

// osError is a socket error as Python's OSError shows it: "[Errno 61]
// Connection refused".
func osError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errTimeout
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		msg := errno.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return &Error{Class: "OSError", Msg: "[Errno " + strconv.Itoa(int(errno)) + "] " + msg, OS: true}
	}
	return &Error{Class: "OSError", Msg: err.Error(), OS: true}
}

func network(addr string, tcp bool) string {
	v6 := strings.Contains(addr, ":")
	switch {
	case tcp && v6:
		return "tcp6"
	case tcp:
		return "tcp4"
	case v6:
		return "udp6"
	}
	return "udp4"
}

// queryUDP is dns.query.udp(raise_on_truncation=True, ignore_errors=True,
// ignore_unexpected=True): datagrams from elsewhere, unparsable or
// answering another query are ignored until the timeout.
func queryUDP(req []byte, id uint16, q question, ns nameserver, timeout float64) (*message, error) {
	expiration := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	conn, err := net.ListenPacket(network(ns.addr, false), "")
	if err != nil {
		return nil, osError(err)
	}
	defer conn.Close()
	dest := &net.UDPAddr{IP: net.ParseIP(strings.SplitN(ns.addr, "%", 2)[0]), Port: ns.port}
	if i := strings.IndexByte(ns.addr, '%'); i >= 0 {
		dest.Zone = ns.addr[i+1:]
	}
	conn.SetDeadline(expiration)
	if _, err := conn.WriteTo(req, dest); err != nil {
		return nil, osError(err)
	}
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
				continue // an unconnected socket's ICMP errors are not reported
			}
			return nil, osError(err)
		}
		if u, ok := from.(*net.UDPAddr); !ok || !u.IP.Equal(dest.IP) || u.Port != dest.Port {
			continue
		}
		m, err := parseMessage(append([]byte(nil), buf[:n]...))
		if err == errTruncated {
			if !m.isResponse(id, q) {
				continue
			}
			return nil, err
		}
		if err != nil || !m.isResponse(id, q) {
			continue
		}
		return m, nil
	}
}

// queryTCP is dns.query.tcp.
func queryTCP(req []byte, id uint16, q question, ns nameserver, timeout float64) (*message, error) {
	d := time.Duration(timeout * float64(time.Second))
	expiration := time.Now().Add(d)
	ctx, cancel := context.WithDeadline(context.Background(), expiration)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network(ns.addr, true), net.JoinHostPort(ns.addr, strconv.Itoa(ns.port)))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errTimeout
		}
		return nil, osError(err)
	}
	defer conn.Close()
	conn.SetDeadline(expiration)
	out := binary.BigEndian.AppendUint16(nil, uint16(len(req)))
	if _, err := conn.Write(append(out, req...)); err != nil {
		return nil, osError(err)
	}
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, tcpReadError(err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, tcpReadError(err)
	}
	m, err := parseMessage(buf)
	if err != nil {
		return nil, err
	}
	if !m.isResponse(id, q) {
		return nil, &Error{Class: "BadResponse", Msg: "A DNS query response does not respond to the question asked.", Form: true}
	}
	return m, nil
}

func tcpReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Class: "EOFError", Msg: "EOF", EOF: true}
	}
	return osError(err)
}
