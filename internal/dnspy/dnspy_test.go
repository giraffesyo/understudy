package dnspy

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Vectors from dnspython 2.8 (and 2.9.1 where noted).

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
		s := rd.Text()
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
		s := rd.Text()
		return s
	}
	if got := loc(0x80000000-(33*3600000+51*60000+35*1000+999), 0x80000000-(151*3600000+12*60000+40*1000+1), 10000000+4200, 0x12, 0x16, 0x13); got != "33 51 35.999 S 151 12 40.001 W 42.00m" {
		t.Errorf("LOC = %q", got)
	}
	if got := loc(0x80000000, 0x80000000+180*3600000, 0, 0x25, 0x34, 0x99); got != "0 0 0.000 N 180 0 0.000 E -100000.00m 2000.00m 300.00m 90000000.00m" {
		t.Errorf("LOC = %q", got)
	}
}

// Vectors from dnspython 2.9.1: each a response to a.test. (the answer's
// owner compressed to the question), from_wire()'s exception or its
// answer RRsets' to_text().
func TestMessageWire(t *testing.T) {
	cases := []struct {
		label, wire, err string
		answer           [][]string
	}{
		{"tlsa mtype 1 short", "123481800001000100000000016104746573740000340001c00c003400010000012c0008030101abcdef0123", "FormError: certificate association data length inconsistent with matching type", nil},
		{"tlsa mtype 1 32", "123481800001000100000000016104746573740000340001c00c003400010000012c0023030101000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", "", [][]string{{"3 1 1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}}},
		{"tlsa mtype 2 32", "123481800001000100000000016104746573740000340001c00c003400010000012c0023030102000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", "FormError: certificate association data length inconsistent with matching type", nil},
		{"tlsa mtype 2 64", "123481800001000100000000016104746573740000340001c00c003400010000012c0043030102000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f", "", [][]string{{"3 1 2 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"}}},
		{"tlsa mtype 0 any", "123481800001000100000000016104746573740000340001c00c003400010000012c0008030000abcdef0123", "", [][]string{{"3 0 0 abcdef0123"}}},
		{"tlsa mtype 3 any", "123481800001000100000000016104746573740000340001c00c003400010000012c0004030003ab", "", [][]string{{"3 0 3 ab"}}},
		{"smimea mtype 1 short", "123481800001000100000000016104746573740000350001c00c003500010000012c0008030001abcdef0123", "FormError: certificate association data length inconsistent with matching type", nil},
		{"sshfp type 1 32", "1234818000010001000000000161047465737400002c0001c00c002c00010000012c002201010000000000000000000000000000000000000000000000000000000000000000", "FormError: fingerprint length inconsistent with fingerprint type", nil},
		{"sshfp type 2 20", "1234818000010001000000000161047465737400002c0001c00c002c00010000012c001601020000000000000000000000000000000000000000", "FormError: fingerprint length inconsistent with fingerprint type", nil},
		{"sshfp type 2 empty", "1234818000010001000000000161047465737400002c0001c00c002c00010000012c00020102", "FormError: fingerprint length inconsistent with fingerprint type", nil},
		{"sshfp type 3 any", "1234818000010001000000000161047465737400002c0001c00c002c00010000012c00030103ab", "", [][]string{{"1 3 ab"}}},
		{"zonemd short", "1234818000010001000000000161047465737400003f0001c00c003f00010000012c00110000000101f00000000000000000000000", "FormError: digest must be at least 12 octets", nil},
		{"zonemd 12", "1234818000010001000000000161047465737400003f0001c00c003f00010000012c00120000000101f0000102030405060708090a0b", "", [][]string{{"1 1 240 000102030405060708090a0b"}}},
		{"nsec trailing zero", "1234818000010001000000000161047465737400002f0001c00c002f00010000012c000701620000024000", "FormError: bad NSEC octets: trailing zero octet", nil},
		{"nsec order then zero", "1234818000010001000000000161047465737400002f0001c00c002f00010000012c0009016200010140000100", "FormError: bad NSEC window order", nil},
		{"nsec truncated after bad order", "1234818000010001000000000161047465737400002f0001c00c002f00010000012c000b0162000101400001400205", "FormError: DNS message is malformed.", nil},
		{"csync trailing zero", "1234818000010001000000000161047465737400003e0001c00c003e00010000012c0009000000010000000100", "FormError: bad CSYNC octets: trailing zero octet", nil},
		{"nsec next compressed", "1234818000010001000000000161047465737400002f0001c00c002f00010000012c0005c00c000140", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"nsec3 empty next", "123481800001000100000000016104746573740000320001c00c003200010000012c0009010000000000000140", "FormError: empty bytes not allowed", nil},
		{"nsec3 empty next bad bitmap", "123481800001000100000000016104746573740000320001c00c003200010000012c0009010000000000000100", "FormError: bad NSEC3 octets: trailing zero octet", nil},
		{"loc size zero exp", "1234818000010001000000000161047465737400001d0001c00c001d00010000012c001000011613800000008000000000989680", "FormError: bad size: non-canonical zero", nil},
		{"loc hprec zero exp", "1234818000010001000000000161047465737400001d0001c00c001d00010000012c001000120913800000008000000000989680", "FormError: bad horizontal precision: non-canonical zero", nil},
		{"loc vprec zero exp", "1234818000010001000000000161047465737400001d0001c00c001d00010000012c001000121605800000008000000000989680", "FormError: bad vertical precision: non-canonical zero", nil},
		{"loc zero", "1234818000010001000000000161047465737400001d0001c00c001d00010000012c001000000000800000008000000000989680", "", [][]string{{"0 0 0.000 N 0 0 0.000 E 0.00m 0.00m 0.00m 0.00m"}}},
		{"apl trailing zero", "1234818000010001000000000161047465737400002a0001c00c002a00010000012c000700011803c00000", "FormError: APL address has trailing zero octet", nil},
		{"apl family 3", "1234818000010001000000000161047465737400002a0001c00c002a00010000012c000500030481e0", "", [][]string{{"!3:e0/4"}}},
		{"apl family 3 empty", "1234818000010001000000000161047465737400002a0001c00c002a00010000012c000400070000", "", [][]string{{"7:/0"}}},
		{"uri escaped", "123481800001000100000000016104746573740001000001c00c010000010000012c000d000100026122625c63ff012064", "", [][]string{{"1 2 \"a\\\"b\\\\c\\255\\001 d\""}}},
		{"svcb dup key", "123481800001000100000000016104746573740000400001c00c004000010000012c000f000100000300020001000300020002", "FormError: keys not in strictly increasing order", nil},
		{"svcb key order", "123481800001000100000000016104746573740000400001c00c004000010000012c001000010000030002000100010003026832", "FormError: keys not in strictly increasing order", nil},
		{"svcb empty alpn", "123481800001000100000000016104746573740000400001c00c004000010000012c000700010000010000", "FormError: key 1 cannot be empty", nil},
		{"svcb empty port", "123481800001000100000000016104746573740000400001c00c004000010000012c000700010000030000", "FormError: key 3 cannot be empty", nil},
		{"svcb empty ech", "123481800001000100000000016104746573740000400001c00c004000010000012c000700010000050000", "FormError: key 5 cannot be empty", nil},
		{"svcb empty mandatory", "123481800001000100000000016104746573740000400001c00c004000010000012c000700010000000000", "FormError: key 0 cannot be empty", nil},
		{"svcb ech short", "123481800001000100000000016104746573740000400001c00c004000010000012c000a00010000050003010203", "FormError: invalid ECHConfigList", nil},
		{"svcb ech bad prefix", "123481800001000100000000016104746573740000400001c00c004000010000012c000d00010000050006000501020304", "FormError: invalid ECHConfigList", nil},
		{"svcb ech prefix 3", "123481800001000100000000016104746573740000400001c00c004000010000012c000c000100000500050003010203", "FormError: invalid ECHConfigList", nil},
		{"svcb ech ok", "123481800001000100000000016104746573740000400001c00c004000010000012c000d00010000050006000401020304", "", [][]string{{"1 . ech=\"AAQBAgME\""}}},
		{"svcb mandatory dup", "123481800001000100000000016104746573740000400001c00c004000010000012c00110001000000000400030003000300020001", "FormError: mandatory keys not in strictly increasing order", nil},
		{"svcb mandatory order", "123481800001000100000000016104746573740000400001c00c004000010000012c001900010000000004000400030003000200010004000401020304", "FormError: mandatory keys not in strictly increasing order", nil},
		{"svcb mandatory mandatory", "123481800001000100000000016104746573740000400001c00c004000010000012c00110001000000000400000003000300020001", "FormError: listed the mandatory key as mandatory", nil},
		{"svcb docpath", "123481800001000100000000016104746573740000400001c00c004000010000012c0013000100000a000c03612c6204635c22640201ff", "", [][]string{{"1 . docpath=\"a\\\\,b,c\\\\\\\\\\\"d,\\001\\255\""}}},
		{"svcb docpath empty", "123481800001000100000000016104746573740000400001c00c004000010000012c0007000100000a0000", "", [][]string{{"1 . docpath"}}},
		{"svcb docpath empty segment", "123481800001000100000000016104746573740000400001c00c004000010000012c000a000100000a0003016100", "FormError: empty bytes not allowed", nil},
		{"svcb alpn escapes", "123481800001000100000000016104746573740000400001c00c004000010000012c00130001000001000c0468322c7803612262025c07", "", [][]string{{"1 . alpn=\"h2\\\\,x,a\\\"b,\\\\\\\\\\007\""}}},
		{"svcb dohpath key9 key65535", "123481800001000100000000016104746573740000400001c00c004000010000012c0018000100000700082f717b3f646e737d00090000ffff000122", "", [][]string{{"1 . dohpath=\"/q{?dns}\" key9 key65535=\"\\\"\""}}},
		{"svcb ohttp nonempty", "123481800001000100000000016104746573740000400001c00c004000010000012c00080001000008000100", "FormError: DNS message is malformed.", nil},
		{"svcb target compressed", "123481800001000100000000016104746573740000400001c00c004000010000012c00040001c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"https alias compressed", "123481800001000100000000016104746573740000410001c00c004100010000012c00040000c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"hhit", "123481800001000100000000016104746573740000430001c00c004300010000012c0028000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021222324252627", "", [][]string{{"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJw=="}}},
		{"hhit empty", "123481800001000100000000016104746573740000430001c00c004300010000012c0000", "", [][]string{{""}}},
		{"brid", "123481800001000100000000016104746573740000440001c00c004400010000012c0003000102", "", [][]string{{"AAEC"}}},
		{"key", "123481800001000100000000016104746573740000190001c00c001900010000012c002c01000305000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021222324252627", "", [][]string{{"256 3 5 AAECAwQFBgcICQoLDA0ODxAREhMUFRYX GBkaGxwdHh8gISIjJCUmJw=="}}},
		{"key empty", "123481800001000100000000016104746573740000190001c00c001900010000012c0004c0000305", "", [][]string{{"49152 3 5 "}}},
		{"sig compressed signer", "123481800001000100000000016104746573740000180001c00c001800010000012c005a000108020000012c67748580659200801092c00c000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445", "", [][]string{{"A 8 2 300 20250101000000 20240101000000 4242 a.test. AAECAwQFBgcICQoLDA0ODxAREhMUFRYX GBkaGxwdHh8gISIjJCUmJygpKissLS4v MDEyMzQ1Njc4OTo7PD0+P0BBQkNERQ=="}}},
		{"rrsig compressed signer", "1234818000010001000000000161047465737400002e0001c00c002e00010000012c0018000108020000012c67748580659200801092c00c00010203", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"rrsig plain signer", "1234818000010001000000000161047465737400002e0001c00c002e00010000012c001c000108020000012c6774858065920080109204746573740000010203", "", [][]string{{"A 8 2 300 20250101000000 20240101000000 4242 test. AAECAw=="}}},
		{"mx compressed", "1234818000010001000000000161047465737400000f0001c00c000f00010000012c0004000ac00c", "", [][]string{{"10 a.test."}}},
		{"kx compressed", "123481800001000100000000016104746573740000240001c00c002400010000012c0004000ac00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"afsdb compressed", "123481800001000100000000016104746573740000120001c00c001200010000012c00040001c00c", "", [][]string{{"1 a.test."}}},
		{"rt compressed", "123481800001000100000000016104746573740000150001c00c001500010000012c00040001c00c", "", [][]string{{"1 a.test."}}},
		{"dname compressed", "123481800001000100000000016104746573740000270001c00c002700010000012c0002c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"cname compressed", "123481800001000100000000016104746573740000050001c00c000500010000012c0002c00c", "", [][]string{{"a.test."}}},
		{"srv compressed", "123481800001000100000000016104746573740000210001c00c002100010000012c0008000100020003c00c", "", [][]string{{"1 2 3 a.test."}}},
		{"naptr compressed", "123481800001000100000000016104746573740000230001c00c002300010000012c000a0001000201550000c00c", "", [][]string{{"1 2 \"U\" \"\" \"\" a.test."}}},
		{"px compressed", "1234818000010001000000000161047465737400001a0001c00c001a00010000012c00060001c00cc00c", "", [][]string{{"1 a.test. a.test."}}},
		{"rp compressed", "123481800001000100000000016104746573740000110001c00c001100010000012c0004c00cc00c", "", [][]string{{"a.test. a.test."}}},
		{"soa compressed", "123481800001000100000000016104746573740000060001c00c000600010000012c0018c00cc00c0000000000000000000000000000000000000000", "", [][]string{{"a.test. a.test. 0 0 0 0 0"}}},
		{"ch a compressed", "123481800001000100000000016104746573740000010003c00c000100030000012c0004c00c0010", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"lp compressed", "1234818000010001000000000161047465737400006b0001c00c006b00010000012c00040001c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"dsync compressed", "123481800001000100000000016104746573740000420001c00c004200010000012c00070001010035c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"ipseckey gateway compressed", "1234818000010001000000000161047465737400002d0001c00c002d00010000012c00070a0302c00c0102", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"amtrelay relay compressed", "123481800001000100000000016104746573740001040001c00c010400010000012c00040a03c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"hip server compressed", "123481800001000100000000016104746573740000370001c00c003700010000012c000a02020002abcd0102c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"hip idn server", "123481800001000100000000016104746573740000370001c00c003700010000012c001c02020002abcd01020d786e2d2d62636865722d6b7661047465737400", "", [][]string{{"2 abcd AQI= xn--bcher-kva.test."}}},
		{"nsap-ptr compressed", "123481800001000100000000016104746573740000170001c00c001700010000012c0002c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"tkey compressed", "123481800001000100000000016104746573740000f90001c00c00f900010000012c0014c00c0000000100000002000300000002abcd0000", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"tkey", "123481800001000100000000016104746573740000f90001c00c00f900010000012c001f0b686d61632d736861323536000000000100000002000300000002abcd0000", "", [][]string{{"hmac-sha256. 1 2 3 0 q80="}}},
		{"tkey other", "123481800001000100000000016104746573740000f90001c00c00f900010000012c001e0b686d61632d7368613235360000000001000000020003000000000001ff", "", [][]string{{"hmac-sha256. 1 2 3 0 \"\" /w=="}}},
		{"ecs ok", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000b0008000700011800c00002", "", [][]string{{"192.0.2.1"}}},
		{"ecs src too long", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000d0008000900012100c000020100", "FormError: invalid ECS prefix length", nil},
		{"ecs scope too long", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000f0008000b0002388100000000000000", "FormError: invalid ECS prefix length", nil},
		{"ecs padding", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000b0008000700011700c00003", "FormError: non-zero ECS address padding bits", nil},
		{"ecs family", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c000020100002910000000000000080008000400030000", "FormError: unsupported family", nil},
		{"ecs extra", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000a0008000600010800c000", "FormError: DNS message is malformed.", nil},
		{"ede ok", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000c000f00080012636166c3a900", "", [][]string{{"192.0.2.1"}}},
		{"ede bad utf8", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c00002010000291000000000000009000f000500126162ff", "FormError: 'utf-8' codec can't decode byte 0xff in position 2: invalid start byte", nil},
		{"ede truncated utf8", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000a000f000600126162e282", "FormError: 'utf-8' codec can't decode bytes in position 2-3: unexpected end of data", nil},
		{"ede-extra-text-language bad", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c000020100002910000000000000060016000265c3", "FormError: 'utf-8' codec can't decode byte 0xc3 in position 1: unexpected end of data", nil},
		{"filtering-contact bad", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c000020100002910000000000000050017000180", "FormError: 'utf-8' codec can't decode byte 0x80 in position 0: invalid start byte", nil},
		{"filtering-organization ok", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c00002010000291000000000000007001800036f7267", "", [][]string{{"192.0.2.1"}}},
		{"filtering-db bad", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c000020100002910000000000000080019000478eda080", "FormError: 'utf-8' codec can't decode byte 0xed in position 1: invalid continuation byte", nil},
		{"cookie short server", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c00002010000291000000000000010000a000c000000000000000000000000", "FormError: server cookie must be empty or between 8 and 32 bytes", nil},
		{"cookie short client", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000b000a000700000000000000", "FormError: DNS message is malformed.", nil},
		{"cookie ok", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c00002010000291000000000000014000a001000000000000000000000000000000000", "", [][]string{{"192.0.2.1"}}},
		{"reportchannel compressed", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000600120002c00c", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"reportchannel ok", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c000020100002910000000000000100012000c056167656e74047465737400", "", [][]string{{"192.0.2.1"}}},
		{"option overflow", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201000029100000000000000400030005", "FormError: DNS message is malformed.", nil},
		{"tsig", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201036b65790000fa00ff00000000001f0b686d61632d73686132353600000000000001012c0002abcd123400000000", "UnknownTSIGKey: got signed message without keyring", nil},
		{"tsig compressed alg", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201036b65790000fa00ff000000000014c00c000000000001012c0002abcd123400000000", "DecompressionForbidden: A name has a compression pointer but decompression is forbidden.", nil},
		{"tsig bad error", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201036b65790000fa00ff00000000001f0b686d61632d73686132353600000000000001012c0002abcd123413880000", "FormError: rcode must be an int between >= 0 and <= 4095", nil},
		{"tsig class", "123481800001000100000001016104746573740000010001c00c000100010000012c0004c0000201036b65790000fa000100000000001f0b686d61632d73686132353600000000000001012c0002abcd123400000000", "BadTSIG: A TSIG record occurred somewhere other than the end of\nthe additional data section or the class is not ANY.", nil},
		{"tsig not last", "123481800001000100000002016104746573740000010001c00c000100010000012c0004c0000201036b65790000fa00ff00000000001f0b686d61632d73686132353600000000000001012c0002abcd1234000000000000291000000000000000", "BadTSIG: A TSIG record occurred somewhere other than the end of\nthe additional data section or the class is not ANY.", nil},
		{"opt twice", "123481800001000100000002016104746573740000010001c00c000100010000012c0004c000020100002910000000000000000000291000000000000000", "BadEDNS: An OPT record occurred somewhere other than\nthe additional data section.", nil},
		{"pointer chain 16", "123481800001000200000000016104746573740000010001c00cff0000010000012c001ec00cc024c026c028c02ac02cc02ec030c032c034c036c038c03ac03cc03ec040000100010000012c0004c0000201", "", [][]string{{"\\# 30 c00cc024c026c028c02ac02cc02ec030c032c034c036c038c03ac03cc03e"}, {"192.0.2.1"}}},
		{"pointer chain 17", "123481800001000200000000016104746573740000010001c00cff0000010000012c0020c00cc024c026c028c02ac02cc02ec030c032c034c036c038c03ac03cc03ec040c042000100010000012c0004c0000201", "PointerChainTooLong: A DNS compression pointer chain was too long.", nil},
		{"pointer into itself", "123481800001000200000000016104746573740000010001c00cff0000010000012c000400000178c026000100010000012c0004c0000201", "FormError: DNS message is malformed.", nil},
	}
	for _, c := range cases {
		wire, err := hex.DecodeString(c.wire)
		if err != nil {
			t.Fatal(err)
		}
		m, err := parseMessage(wire)
		if c.err != "" {
			e, ok := err.(*Error)
			if !ok || e.Class+": "+e.Msg != c.err {
				t.Errorf("%s: error %v, want %q", c.label, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.label, err)
			continue
		}
		var got [][]string
		for _, s := range m.answer {
			var texts []string
			for _, rd := range s.Rdatas {
				text := rd.Text()
				texts = append(texts, text)
			}
			got = append(got, texts)
		}
		if !reflect.DeepEqual(got, c.answer) {
			t.Errorf("%s: answer %q, want %q", c.label, got, c.answer)
		}
	}
}

func TestUTF8Decode(t *testing.T) {
	for _, c := range []struct{ in, err string }{
		{"\x6f\x6b", ""},
		{"\xff", "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte"},
		{"\x61\x80\x62", "'utf-8' codec can't decode byte 0x80 in position 1: invalid start byte"},
		{"\xc3", "'utf-8' codec can't decode byte 0xc3 in position 0: unexpected end of data"},
		{"\xc3\x28", "'utf-8' codec can't decode byte 0xc3 in position 0: invalid continuation byte"},
		{"\xe2\x82", "'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xe2\x82\x28", "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"},
		{"\xe2\x28\xa1", "'utf-8' codec can't decode byte 0xe2 in position 0: invalid continuation byte"},
		{"\xe0\x80\x80", "'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte"},
		{"\xe0\xa0", "'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xed\xa0\x80", "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"},
		{"\xf0\x80\x80\x80", "'utf-8' codec can't decode byte 0xf0 in position 0: invalid continuation byte"},
		{"\xf0\x90\x80", "'utf-8' codec can't decode bytes in position 0-2: unexpected end of data"},
		{"\xf4\x90\x80\x80", "'utf-8' codec can't decode byte 0xf4 in position 0: invalid continuation byte"},
		{"\xf5\x80", "'utf-8' codec can't decode byte 0xf5 in position 0: invalid start byte"},
		{"\xc0\xaf", "'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"\xf0\x9f\x98\x80", ""},
		{"\x78\xf0\x9f\x98", "'utf-8' codec can't decode bytes in position 1-3: unexpected end of data"},
	} {
		err := utf8Decode([]byte(c.in))
		if (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) {
			t.Errorf("utf8Decode(%q) = %v, want %q", c.in, err, c.err)
		}
	}
}

func TestBadEscapeOver255(t *testing.T) {
	for _, in := range []string{`a\256.test.`, `a\999`, `ü\256.test.`} {
		if _, err := ParseName(in, nil); !Is(err, "BadEscape") || err.Error() != "An escaped code in a text format of DNS name is invalid." {
			t.Errorf("ParseName(%q) = %v", in, err)
		}
	}
}

func TestTypes291(t *testing.T) {
	for in, want := range map[string]uint16{"NXNAME": 128, "nxname": 128, "HHIT": 67, "BRID": 68} {
		if v, err := TypeFromText(in); err != nil || v != want {
			t.Errorf("TypeFromText(%q) = %d, %v", in, v, err)
		}
	}
	for v, want := range map[uint16]string{67: "HHIT", 68: "BRID", 128: "NXNAME"} {
		if got := TypeText(v); got != want {
			t.Errorf("TypeText(%d) = %q, want %q", v, got, want)
		}
	}
	if !isMetatype(128) {
		t.Error("NXNAME is a metatype")
	}
}
