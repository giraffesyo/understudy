package dnspy

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Vectors from dnspython 2.8.

func TestParseName(t *testing.T) {
	cases := []struct {
		in, text, err string
		abs           bool
		n             int
	}{
		{in: "a.b.", text: "a.b.", abs: true, n: 3},
		{in: `a\.b.c`, text: `a\.b.c`, n: 2},
		{in: `\065bc.`, text: "Abc.", abs: true, n: 2},
		{in: `a\000.`, text: `a\000.`, abs: true, n: 2},
		{in: "@", text: "@", n: 0},
		{in: ".", text: ".", abs: true, n: 1},
		{in: "bücher.test.", text: "xn--bcher-kva.test.", abs: true, n: 3},
		{in: "MÜNCHEN.de", text: "xn--mnchen-3ya.de", n: 2},
		{in: "a..b", err: "A DNS label is empty."},
		{in: strings.Repeat("x", 64), err: "A DNS label is > 63 octets long."},
		{in: `\1`, err: "An escaped code in a text format of DNS name is invalid."},
		{in: "a.b", text: "a.b", n: 2},
	}
	for _, c := range cases {
		n, err := ParseName(c.in, nil)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("ParseName(%q) error = %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseName(%q): %v", c.in, err)
			continue
		}
		if n.String() != c.text || n.Absolute() != c.abs || n.Len() != c.n {
			t.Errorf("ParseName(%q) = %q abs=%v len=%d, want %q abs=%v len=%d", c.in, n, n.Absolute(), n.Len(), c.text, c.abs, c.n)
		}
	}
}

func TestIPv6Text(t *testing.T) {
	for in, want := range map[string]string{
		"::": "::", "::1": "::1", "1::": "1::", "2001:db8::1:0:0:1": "2001:db8::1:0:0:1",
		"::ffff:1.2.3.4": "::ffff:1.2.3.4", "::1.2.3.4": "::1.2.3.4", "fe80::1:2": "fe80::1:2",
		"1:0:0:2:0:0:0:3": "1:0:0:2::3", "0:0:0:0:0:1:0:0": "::1:0:0",
	} {
		b, err := ipv6Aton(in, false)
		if err != nil {
			t.Errorf("ipv6Aton(%q): %v", in, err)
			continue
		}
		if got := ipv6Ntoa(b); got != want {
			t.Errorf("ipv6Ntoa(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReverseName(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3.4":         "4.3.2.1.in-addr.arpa.",
		"::1":             "1." + strings.Repeat("0.", 31) + "ip6.arpa.",
		"::ffff:10.0.0.1": "1.0.0.10.in-addr.arpa.",
		"01.2.3.4":        "",
		"1.2.3":           "",
	} {
		n, err := ReverseName(in)
		if want == "" {
			if err == nil {
				t.Errorf("ReverseName(%q) = %s, want an error", in, n)
			}
			continue
		}
		if err != nil || n.String() != want {
			t.Errorf("ReverseName(%q) = %s, %v; want %s", in, n, err, want)
		}
	}
}

func TestTypeText(t *testing.T) {
	for in, want := range map[string]any{
		"a": uint16(1), "nsap-ptr": uint16(23), "TYPE300": uint16(300), "type0": uint16(0), "NONE": uint16(0),
		"TYPE65536": "type must be an int between >= 0 and <= 65535", "xx": "DNS resource record type is unknown.",
	} {
		v, err := TypeFromText(in)
		if s, ok := want.(string); ok {
			if err == nil || err.Error() != s {
				t.Errorf("TypeFromText(%q) error = %v, want %q", in, err, s)
			}
		} else if err != nil || v != want {
			t.Errorf("TypeFromText(%q) = %d, %v", in, v, err)
		}
	}
	for v, want := range map[uint16]string{23: "NSAP-PTR", 300: "TYPE300", 65280: "TYPE65280", 0: "TYPE0"} {
		if got := TypeText(v); got != want {
			t.Errorf("TypeText(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestSocketInetAton(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1": true, "127.1": true, "1": true, "0x7f.1": true, "010.0.0.1": true,
		"256.1.1.1": false, "1.2.3.4.5": false, "::1": false, "a.b": false, "": false, "1.2.3.256": false,
	} {
		if got := SocketInetAton(in); got != want {
			t.Errorf("SocketInetAton(%q) = %v, want %v", in, got, want)
		}
	}
}

// testServer answers each query with respond's message (nil: none),
// over UDP and TCP on one loopback port.
func testServer(t *testing.T, respond func(q []byte, tcp bool) []byte) int {
	t.Helper()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp4", udp.LocalAddr().String())
	if err != nil {
		udp.Close()
		t.Skipf("no TCP port: %v", err)
	}
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		for {
			n, addr, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := respond(append([]byte(nil), buf[:n]...), false); r != nil {
				udp.WriteTo(r, addr)
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			var hdr [2]byte
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Read(hdr[:]); err == nil {
				q := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := c.Read(q); err == nil {
					if r := respond(q, true); r != nil {
						c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(r))), r...))
					}
				}
			}
			c.Close()
		}
	}()
	t.Cleanup(func() {
		udp.Close()
		tcp.Close()
		<-done
		<-done
	})
	return port
}

// reply builds a response to q: rcode and flags, and the answers (each
// owner-compressed to the question, type A, the given address bytes).
func reply(q []byte, rcode int, tc bool, answers ...[]byte) []byte {
	qend := 12
	for q[qend] != 0 {
		qend += int(q[qend]) + 1
	}
	qend += 5
	m := append([]byte(nil), q[:2]...)
	flags := 0x8180 | rcode
	if tc {
		flags |= 0x0200
	}
	m = binary.BigEndian.AppendUint16(m, uint16(flags))
	m = binary.BigEndian.AppendUint16(m, 1)
	m = binary.BigEndian.AppendUint16(m, uint16(len(answers)))
	m = append(m, 0, 0, 0, 0)
	m = append(m, q[12:qend]...)
	for _, a := range answers {
		m = append(m, 0xC0, 12, 0, 1, 0, 1, 0, 0, 0, 60)
		m = binary.BigEndian.AppendUint16(m, uint16(len(a)))
		m = append(m, a...)
	}
	return m
}

func testResolver(port int) *Resolver {
	return &Resolver{Nameservers: []string{"127.0.0.1"}, Port: port, Ndots: -1, Timeout: 2, Lifetime: 5,
		Domain: Root, EDNS: true, Payload: 4096, EDNSFlags: 0x8000, sleep: func(time.Duration) {}}
}

func TestResolverTruncationRetriesOverTCP(t *testing.T) {
	port := testServer(t, func(q []byte, tcp bool) []byte {
		if !tcp {
			return reply(q, 0, true)
		}
		return reply(q, 0, false, []byte{192, 0, 2, 1}, []byte{192, 0, 2, 1}, []byte{192, 0, 2, 2})
	})
	a, err := testResolver(port).Query("x.test.", "A", ClassIN, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rd := range a.RRset.Rdatas {
		s, _ := rd.Text()
		got = append(got, s)
	}
	if strings.Join(got, ",") != "192.0.2.1,192.0.2.2" || a.CanonicalName.String() != "x.test." {
		t.Errorf("answer %v for %s", got, a.CanonicalName)
	}
}

func TestResolverServfail(t *testing.T) {
	var calls atomic.Int32 // the server answers on its own goroutines
	port := testServer(t, func(q []byte, tcp bool) []byte {
		if calls.Add(1)%2 == 1 {
			return reply(q, 2, false)
		}
		return reply(q, 0, false, []byte{192, 0, 2, 7})
	})
	r := testResolver(port)
	_, err := r.Query("flaky.test.", "A", ClassIN, false)
	want := "All nameservers failed to answer the query flaky.test. IN A: Server Do53:127.0.0.1@" + strconv.Itoa(port) + " answered SERVFAIL"
	if err == nil || err.Error() != want || !Is(err, "NoNameservers") {
		t.Errorf("without retry_servfail: %v, want %q", err, want)
	}
	r.RetryServfail = true
	calls.Store(0)
	if _, err := r.Query("flaky.test.", "A", ClassIN, false); err != nil {
		t.Errorf("with retry_servfail: %v", err)
	}
}

func TestResolverSearchList(t *testing.T) {
	port := testServer(t, func(q []byte, tcp bool) []byte { return reply(q, 3, false) })
	r := testResolver(port)
	r.Domain = Name{Labels: [][]byte{[]byte("local"), {}}}
	for qname, want := range map[string]string{
		"nx":      "None of DNS query names exist: nx.local., nx.",
		"nx.test": "None of DNS query names exist: nx.test., nx.test.local.",
		"nx.":     "The DNS query name does not exist: nx.",
	} {
		_, err := r.Query(qname, "A", ClassIN, false)
		if err == nil || err.Error() != want {
			t.Errorf("Query(%q) = %v, want %q", qname, err, want)
		}
	}
	if _, err := r.Query("a.", "ANY", ClassIN, false); err == nil || err.Error() != "DNS metaqueries are not allowed." {
		t.Errorf("ANY: %v", err)
	}
}

func TestResolverTimeout(t *testing.T) {
	port := testServer(t, func(q []byte, tcp bool) []byte { return nil })
	r := testResolver(port)
	r.Timeout, r.Lifetime = 0.2, 0.5
	_, err := r.Query("drop.test.", "A", ClassIN, false)
	if !Is(err, "LifetimeTimeout") || !strings.Contains(err.Error(), "answered The DNS operation timed out.") {
		t.Errorf("Query = %v", err)
	}
}

func TestLOCText(t *testing.T) {
	// south.test.'s records in the dig golden's fixture.
	loc := func(lat, long, alt uint32, sizes ...byte) string {
		w := append([]byte{0}, sizes...)
		w = binary.BigEndian.AppendUint32(w, lat)
		w = binary.BigEndian.AppendUint32(w, long)
		w = binary.BigEndian.AppendUint32(w, alt)
		rd, err := parseRdata(ClassIN, 29, &parser{wire: w, end: len(w)})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := rd.Text()
		return s
	}
	if got := loc(0x80000000-(33*3600000+51*60000+35*1000+999), 0x80000000-(151*3600000+12*60000+40*1000+1), 10000000+4200, 0x12, 0x16, 0x13); got != "33 51 35.999 S 151 12 40.001 W 42.00m" {
		t.Errorf("LOC = %q", got)
	}
	if got := loc(0x80000000, 0x80000000+180*3600000, 0, 0x25, 0x34, 0x99); got != "0 0 0.000 N 180 0 0.000 E -100000.00m 2000.00m 300.00m 90000000.00m" {
		t.Errorf("LOC = %q", got)
	}
}
