//go:build golden

package e2e

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strings"
	"sync"
	"testing"
)

// dnsFixture is a deterministic authoritative DNS server for the dig
// golden (UDP and TCP on one loopback port). Besides its zone (see
// dnsFixtureZone) a few names misbehave on purpose:
//
//	nx.test., *.nx.test.   NXDOMAIN (with the zone's SOA)
//	servfail.test.         SERVFAIL
//	refused.test.          REFUSED
//	notimp.test.           NOTIMP
//	yx.test.               YXDOMAIN
//	drop.test.             never answered (a timeout)
//	flaky.test.            SERVFAIL, then an answer, alternately
//	tc.test.               truncated over UDP, answered over TCP
//	loop1.test.            a CNAME loop with loop2.test.
type dnsFixture struct {
	udp   net.PacketConn
	tcp   net.Listener
	port  int
	wg    sync.WaitGroup
	mu    sync.Mutex
	flaky int
	conns map[net.Conn]bool
	once  sync.Once
}

// dnsPiece is one field of a fixture record's rdata.
type dnsPiece any

// dnsName is a domain name in rdata, written compressed when the type
// allows it (cname) or not (rawName).
type (
	dnsName    string
	dnsRawName string
	dnsU8      uint8
	dnsU16     uint16
	dnsU32     uint32
)

type dnsRR struct {
	name  string
	typ   uint16
	class uint16
	ttl   uint32
	rdata []dnsPiece
}

func hexBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func b64Bytes(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// charStr is a <character-string>: a length byte and the bytes.
func charStr(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func ipv4(s string) []byte { return net.ParseIP(s).To4() }
func ipv6(s string) []byte { return net.ParseIP(s).To16() }

const (
	tA          = 1
	tNS         = 2
	tCNAME      = 5
	tSOA        = 6
	tPTR        = 12
	tHINFO      = 13
	tMX         = 15
	tTXT        = 16
	tRP         = 17
	tAFSDB      = 18
	tAAAA       = 28
	tLOC        = 29
	tSRV        = 33
	tNAPTR      = 35
	tKX         = 36
	tCERT       = 37
	tDNAME      = 39
	tDS         = 43
	tSSHFP      = 44
	tRRSIG      = 46
	tNSEC       = 47
	tDNSKEY     = 48
	tDHCID      = 49
	tNSEC3      = 50
	tNSEC3PARAM = 51
	tTLSA       = 52
	tSMIMEA     = 53
	tCDS        = 59
	tCDNSKEY    = 60
	tOPENPGPKEY = 61
	tCSYNC      = 62
	tZONEMD     = 63
	tSVCB       = 64
	tHTTPS      = 65
	tSPF        = 99
	tEUI48      = 108
	tEUI64      = 109
	tURI        = 256
	tCAA        = 257
	tOPT        = 41
	classIN     = 1
	classCH     = 3
)

var soaRdata = []dnsPiece{dnsName("ns1.test."), dnsName("hostmaster.test."), dnsU32(2024010101), dnsU32(3600), dnsU32(900), dnsU32(604800), dnsU32(300)}

// dnsFixtureZone is the fixture's data, in the order it answers it.
var dnsFixtureZone = []dnsRR{
	{"test.", tSOA, classIN, 3600, soaRdata},
	{"test.", tNS, classIN, 3600, []dnsPiece{dnsName("ns1.test.")}},
	{"test.", tNS, classIN, 3600, []dnsPiece{dnsName("ns2.test.")}},
	{"test.", tMX, classIN, 300, []dnsPiece{dnsU16(10), dnsName("mail.test.")}},
	{"test.", tMX, classIN, 300, []dnsPiece{dnsU16(20), dnsName("mail2.test.")}},
	{"test.", tCAA, classIN, 300, []dnsPiece{dnsU8(0), charStr("issue"), []byte("letsencrypt.org")}},
	{"test.", tCAA, classIN, 300, []dnsPiece{dnsU8(128), charStr("iodef"), []byte(`mailto:"sec"@test`)}},
	{"test.", tDNSKEY, classIN, 3600, []dnsPiece{dnsU16(257), dnsU8(3), dnsU8(13), b64Bytes("mdsswUyr3DPW132mOi8V9xESWE8jTo0dxCjjnopKl+GqJxpVXckHAeF+KkxLbxILfDLUT0rAK9iUzy1L53eKGQ==")}},
	{"test.", tDNSKEY, classIN, 3600, []dnsPiece{dnsU16(256), dnsU8(3), dnsU8(8), b64Bytes("AwEAAcTRaOyqTUwhq0KTGdrIElDdzvBNfU+9K1D0+ZGsnBZ3VDGwk7PC8dsyFsNNvgkH6w0vG7o+mQWMR+AFsT8s5bg=")}},
	{"test.", tDS, classIN, 3600, []dnsPiece{dnsU16(12345), dnsU8(13), dnsU8(2), hexBytes("3490a6806d47f17a34c29e2ce80e8a999ffb4be0c2f3d5e6a7b8c9d0e1f2a3b4")}},
	{"test.", tNSEC3PARAM, classIN, 0, []dnsPiece{dnsU8(1), dnsU8(0), dnsU16(10), charStr("\xaa\xbb\xcc\xdd")}},
	{"test.", tZONEMD, classIN, 3600, []dnsPiece{dnsU32(2024010101), dnsU8(1), dnsU8(1), hexBytes("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")}},
	{"test.", tCDS, classIN, 3600, []dnsPiece{dnsU16(12345), dnsU8(13), dnsU8(2), hexBytes("3490a6806d47f17a34c29e2ce80e8a993490a6806d47f17a34c29e2ce80e8a99")}},
	{"test.", tCDNSKEY, classIN, 3600, []dnsPiece{dnsU16(257), dnsU8(3), dnsU8(13), b64Bytes("AQID")}},
	{"test.", tCSYNC, classIN, 3600, []dnsPiece{dnsU32(66), dnsU16(3), []byte{0, 3, 0x62, 0, 0x80}}},
	{"test.", tNSEC, classIN, 300, []dnsPiece{dnsRawName("a.test."), []byte{0, 7, 0x62, 0x01, 0x80, 0x08, 0x00, 0x03, 0x80, 1, 1, 0x40}}},
	{"test.", tTXT, classIN, 300, []dnsPiece{charStr("v=spf1 -all")}},
	{"test.", tSPF, classIN, 300, []dnsPiece{charStr("v=spf1 -all")}},

	{"a.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.1")}},
	{"a.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.2")}},
	{"a.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.1")}},
	{"a.test.", tAAAA, classIN, 600, []dnsPiece{ipv6("2001:db8::1")}},
	{"a.test.", tAAAA, classIN, 600, []dnsPiece{ipv6("2001:db8:0:0:1:0:0:1")}},
	{"a.test.", tAAAA, classIN, 600, []dnsPiece{ipv6("::ffff:192.0.2.9")}},
	{"a.test.", tTXT, classIN, 120, []dnsPiece{charStr("hello world")}},
	{"a.test.", tTXT, classIN, 120, []dnsPiece{charStr("multi"), charStr("part"), charStr("")}},
	{"a.test.", tTXT, classIN, 120, []dnsPiece{charStr(`quote" back\ tab` + "\t" + "\xc3\xa9\x7f")}},
	{"a.test.", tHINFO, classIN, 300, []dnsPiece{charStr("INTEL"), charStr("Linux \"x\"")}},
	{"a.test.", tLOC, classIN, 300, []dnsPiece{dnsU8(0), dnsU8(0x12), dnsU8(0x16), dnsU8(0x13), dnsU32(0x80000000 + 52*3600000 + 22*60000 + 23*1000), dnsU32(0x80000000 + 4*3600000 + 53*60000 + 32*1000 + 5), dnsU32(10000000 - 200)}},
	{"a.test.", tRP, classIN, 300, []dnsPiece{dnsRawName("admin.test."), dnsRawName("txt.test.")}},
	{"a.test.", tSSHFP, classIN, 300, []dnsPiece{dnsU8(1), dnsU8(1), hexBytes("dd465c09cfa51fb45020cc83316fff21b9ec74ac")}},
	{"a.test.", tSSHFP, classIN, 300, []dnsPiece{dnsU8(4), dnsU8(2), hexBytes("c9a6e7d3f1b2a4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d")}},
	{"a.test.", tRRSIG, classIN, 300, []dnsPiece{dnsU16(tA), dnsU8(13), dnsU8(2), dnsU32(300), dnsU32(1735689600), dnsU32(1704067200), dnsU16(12345), dnsRawName("test."), b64Bytes("oJB1W6WNGv+ldvQ3WDG0MQkg5IEhjRip8WTrPYGv07h108dUKGMeDPKijVCHX3DDKdfb+v6oB9wfuh3DTJXUAg==")}},
	{"a.test.", tAFSDB, classIN, 300, []dnsPiece{dnsU16(1), dnsRawName("afs.test.")}},
	{"a.test.", tKX, classIN, 300, []dnsPiece{dnsU16(5), dnsRawName("kx.test.")}},
	{"a.test.", tURI, classIN, 300, []dnsPiece{dnsU16(10), dnsU16(1), []byte("https://www.test/path")}},
	{"a.test.", tEUI48, classIN, 300, []dnsPiece{hexBytes("00005e0053ff")}},
	{"a.test.", tEUI64, classIN, 300, []dnsPiece{hexBytes("00005efffe0053ff")}},
	{"a.test.", tOPENPGPKEY, classIN, 300, []dnsPiece{b64Bytes("mQINBFit2jsBEADrbl5vjVxYeAE0g0IDYCBpHirv1Sjlqxx5gjtPhb2YhvyDMXjq")}},
	{"a.test.", tDHCID, classIN, 300, []dnsPiece{b64Bytes("AAIBY2/AuCccgoJbsaxcQc9TUapptP69lOjxfNuVAA2kjEA=")}},
	{"a.test.", tCERT, classIN, 300, []dnsPiece{dnsU16(1), dnsU16(12345), dnsU8(8), b64Bytes("MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA")}},
	{"a.test.", tSMIMEA, classIN, 300, []dnsPiece{dnsU8(3), dnsU8(0), dnsU8(1), hexBytes("abcdef0123")}},
	{"a.test.", 65280, classIN, 300, []dnsPiece{[]byte{1, 2, 3, 4}}},
	{"a.test.", 65281, classIN, 300, []dnsPiece{}},
	{"a.test.", tNSEC3, classIN, 300, []dnsPiece{dnsU8(1), dnsU8(1), dnsU16(10), charStr("\xaa\xbb"), charStr("\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14"), []byte{0, 2, 0x62, 0x01}}},
	{"a.test.", tSVCB, classIN, 300, []dnsPiece{dnsU16(1), dnsRawName("svc.test."), dnsU16(1), dnsU16(6), charStr("h2"), charStr("h3"), dnsU16(3), dnsU16(2), dnsU16(8443), dnsU16(4), dnsU16(8), ipv4("192.0.2.1"), ipv4("192.0.2.2")}},
	{"a.test.", tHTTPS, classIN, 300, []dnsPiece{dnsU16(0), dnsRawName("www.test.")}},
	{"a.test.", tHTTPS, classIN, 300, []dnsPiece{dnsU16(1), dnsRawName("."), dnsU16(0), dnsU16(2), dnsU16(1), dnsU16(1), dnsU16(3), charStr("h2"), dnsU16(2), dnsU16(0), dnsU16(5), dnsU16(3), []byte{1, 2, 3}, dnsU16(6), dnsU16(16), ipv6("2001:db8::1"), dnsU16(9), dnsU16(2), []byte("x,")}},

	{"_sip._tcp.test.", tSRV, classIN, 300, []dnsPiece{dnsU16(10), dnsU16(60), dnsU16(5060), dnsRawName("sip.test.")}},
	{"_sip._tcp.test.", tSRV, classIN, 300, []dnsPiece{dnsU16(20), dnsU16(0), dnsU16(5061), dnsRawName(".")}},
	{"_443._tcp.a.test.", tTLSA, classIN, 300, []dnsPiece{dnsU8(3), dnsU8(1), dnsU8(1), hexBytes("0d6fce3397de4b9a3f4e0a0e8c22f1e4e3a5d9a1a7d7a6e3d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d")}},
	{"naptr.test.", tNAPTR, classIN, 300, []dnsPiece{dnsU16(100), dnsU16(10), charStr("U"), charStr("E2U+sip"), charStr(`!^.*$!sip:info@test!`), dnsRawName(".")}},
	{"naptr.test.", tNAPTR, classIN, 300, []dnsPiece{dnsU16(102), dnsU16(10), charStr("S"), charStr("SIP+D2U"), charStr(""), dnsRawName("_sip._udp.test.")}},
	{"south.test.", tLOC, classIN, 300, []dnsPiece{dnsU8(0), dnsU8(0x12), dnsU8(0x16), dnsU8(0x13), dnsU32(0x80000000 - (33*3600000 + 51*60000 + 35*1000 + 999)), dnsU32(0x80000000 - (151*3600000 + 12*60000 + 40*1000 + 1)), dnsU32(10000000 + 4200)}},
	{"south.test.", tLOC, classIN, 300, []dnsPiece{dnsU8(0), dnsU8(0x25), dnsU8(0x34), dnsU8(0x99), dnsU32(0x80000000), dnsU32(0x80000000 + 180*3600000), dnsU32(0)}},
	{"empty.test.", tTXT, classIN, 60, []dnsPiece{charStr("")}},
	{"bin.test.", tTXT, classIN, 60, []dnsPiece{charStr("caf\xc3\xa9 \xff\xfe ok"), charStr("\x00\x01")}},
	{"bin.test.", tHINFO, classIN, 60, []dnsPiece{charStr("\xff"), charStr("\xe2\x82\xac")}},
	{"bin.test.", tCAA, classIN, 60, []dnsPiece{dnsU8(0), charStr("issuewild"), []byte(";")}},
	{"nsec3salt.test.", tNSEC3PARAM, classIN, 0, []dnsPiece{dnsU8(1), dnsU8(0), dnsU16(0), charStr("")}},

	{"alias.test.", tCNAME, classIN, 60, []dnsPiece{dnsName("a.test.")}},
	{"alias2.test.", tCNAME, classIN, 30, []dnsPiece{dnsName("alias.test.")}},
	{"dangling.test.", tCNAME, classIN, 30, []dnsPiece{dnsName("nx.test.")}},
	{"loop1.test.", tCNAME, classIN, 30, []dnsPiece{dnsName("loop2.test.")}},
	{"loop2.test.", tCNAME, classIN, 30, []dnsPiece{dnsName("loop1.test.")}},
	{"dn.test.", tDNAME, classIN, 300, []dnsPiece{dnsRawName("a.test.")}},
	{"www.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.80")}},
	{"mixed.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.3")}},
	{"mixed.test.", tA, classIN, 100, []dnsPiece{ipv4("192.0.2.4")}},
	{"Case.Test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.5")}},
	{"odd\\.name.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.6")}},
	{"flaky.test.", tA, classIN, 300, []dnsPiece{ipv4("192.0.2.7")}},
	{"tc.test.", tTXT, classIN, 300, []dnsPiece{charStr(strings.Repeat("x", 250)), charStr(strings.Repeat("y", 250))}},
	{"tc.test.", tTXT, classIN, 300, []dnsPiece{charStr(strings.Repeat("z", 250))}},

	{"1.2.0.192.in-addr.arpa.", tPTR, classIN, 300, []dnsPiece{dnsName("a.test.")}},
	{"80.2.0.192.in-addr.arpa.", tPTR, classIN, 300, []dnsPiece{dnsName("www.test.")}},
	{"80.2.0.192.in-addr.arpa.", tPTR, classIN, 300, []dnsPiece{dnsName("web.test.")}},
	{"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", tPTR, classIN, 300, []dnsPiece{dnsName("a.test.")}},

	{"version.bind.", tTXT, classCH, 0, []dnsPiece{charStr("understudy-fixture")}},
}

// startDNSFixture serves the fixture on a free loopback port until the
// test ends.
func startDNSFixture(t *testing.T) *dnsFixture {
	t.Helper()
	var f *dnsFixture
	for range 20 {
		udp, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("dns fixture: %v", err)
		}
		port := udp.LocalAddr().(*net.UDPAddr).Port
		tcp, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			udp.Close()
			continue
		}
		f = &dnsFixture{udp: udp, tcp: tcp, port: port}
		break
	}
	if f == nil {
		t.Fatal("dns fixture: no port free for both UDP and TCP")
	}
	f.wg.Add(2)
	go f.serveUDP()
	go f.serveTCP()
	t.Cleanup(f.close)
	return f
}

func (f *dnsFixture) close() {
	f.once.Do(func() {
		f.udp.Close()
		f.tcp.Close()
		f.mu.Lock()
		for c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
}

func (f *dnsFixture) serveUDP() {
	defer f.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := f.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := f.answer(buf[:n], false); resp != nil {
			f.udp.WriteTo(resp, addr)
		}
	}
}

func (f *dnsFixture) serveTCP() {
	defer f.wg.Done()
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.conns == nil {
			f.conns = map[net.Conn]bool{}
		}
		f.conns[c] = true
		f.mu.Unlock()
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer func() {
				c.Close()
				f.mu.Lock()
				delete(f.conns, c)
				f.mu.Unlock()
			}()
			for {
				var hdr [2]byte
				if _, err := readFull(c, hdr[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := readFull(c, msg); err != nil {
					return
				}
				resp := f.answer(msg, true)
				if resp == nil {
					return
				}
				out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
				if _, err := c.Write(append(out, resp...)); err != nil {
					return
				}
			}
		}()
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// readName reads an uncompressed query name.
func readName(msg []byte, off int) (string, int, bool) {
	var labels []string
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l > 63 || off+l > len(msg) {
			return "", 0, false
		}
		var b strings.Builder
		for _, c := range msg[off : off+l] {
			switch {
			case c == '.' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		}
		labels = append(labels, b.String())
		off += l
	}
	return strings.Join(labels, ".") + ".", off, true
}

// answer builds the response to one query (nil: no answer at all).
func (f *dnsFixture) answer(q []byte, tcp bool) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:]) != 1 {
		return nil
	}
	qname, off, ok := readName(q, 12)
	if !ok || off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off:])
	qclass := binary.BigEndian.Uint16(q[off+2:])
	question := q[12 : off+4]
	edns, do := false, false
	if binary.BigEndian.Uint16(q[10:]) == 1 && off+4+11 <= len(q) {
		opt := q[off+4:]
		if opt[0] == 0 && binary.BigEndian.Uint16(opt[1:]) == tOPT {
			edns = true
			do = binary.BigEndian.Uint32(opt[5:])&0x8000 != 0
		}
	}
	lower := strings.ToLower(qname)
	rcode := 0
	var answers, authority []dnsRR
	switch {
	case lower == "drop.test.":
		return nil
	case lower == "servfail.test.":
		rcode = 2
	case lower == "notimp.test.":
		rcode = 4
	case lower == "refused.test.":
		rcode = 5
	case lower == "yx.test.":
		rcode = 6
	case lower == "flaky.test.":
		f.mu.Lock()
		f.flaky++
		fail := f.flaky%2 == 1
		f.mu.Unlock()
		if fail {
			rcode = 2
			break
		}
		answers, authority, rcode = lookupFixture(lower, qtype, qclass)
	default:
		answers, authority, rcode = lookupFixture(lower, qtype, qclass)
	}
	truncated := false
	if lower == "tc.test." && !tcp {
		answers, authority, truncated = nil, nil, true
	}

	flags := uint16(0x8000 | 0x0400) // QR, AA
	flags |= binary.BigEndian.Uint16(q[2:]) & 0x0100
	flags |= 0x0080 // RA
	if truncated {
		flags |= 0x0200
	}
	flags |= uint16(rcode)
	msg := make([]byte, 12, 512)
	copy(msg, q[:2])
	binary.BigEndian.PutUint16(msg[2:], flags)
	binary.BigEndian.PutUint16(msg[4:], 1)
	binary.BigEndian.PutUint16(msg[6:], uint16(len(answers)))
	binary.BigEndian.PutUint16(msg[8:], uint16(len(authority)))
	if edns {
		binary.BigEndian.PutUint16(msg[10:], 1)
	}
	msg = append(msg, question...)
	comp := map[string]int{lower: 12}
	for _, rr := range answers {
		msg = appendRR(msg, rr, comp)
	}
	for _, rr := range authority {
		msg = appendRR(msg, rr, comp)
	}
	if edns {
		ttl := uint32(0)
		if do {
			ttl = 0x8000
		}
		msg = append(msg, 0)
		msg = binary.BigEndian.AppendUint16(msg, tOPT)
		msg = binary.BigEndian.AppendUint16(msg, 1232)
		msg = binary.BigEndian.AppendUint32(msg, ttl)
		msg = binary.BigEndian.AppendUint16(msg, 0)
	}
	return msg
}

// lookupFixture finds the answer to qname/qtype, following CNAMEs.
func lookupFixture(qname string, qtype, qclass uint16) (answers, authority []dnsRR, rcode int) {
	if qname == "nx.test." || strings.HasSuffix(qname, ".nx.test.") {
		return nil, []dnsRR{{"test.", tSOA, classIN, 3600, soaRdata}}, 3
	}
	name := qname
	for hops := 0; hops < 20; hops++ {
		exists := false
		var cname *dnsRR
		var found []dnsRR
		for _, rr := range dnsFixtureZone {
			if strings.ToLower(rr.name) != name || rr.class != qclass {
				continue
			}
			exists = true
			if rr.typ == qtype {
				found = append(found, rr)
			} else if rr.typ == tCNAME && qtype != tCNAME {
				cname = &rr
			}
		}
		if len(found) > 0 {
			return append(answers, found...), nil, 0
		}
		if cname != nil {
			answers = append(answers, *cname)
			name = strings.ToLower(string(cname.rdata[0].(dnsName)))
			if name == "nx.test." {
				return answers, []dnsRR{{"test.", tSOA, classIN, 3600, soaRdata}}, 3
			}
			continue
		}
		if !exists && name != "test." && !isFixtureParent(name) {
			if !strings.HasSuffix(name, ".test.") && !strings.HasSuffix(name, ".arpa.") {
				return answers, nil, 5 // not this server's zone
			}
			return answers, []dnsRR{{"test.", tSOA, classIN, 3600, soaRdata}}, 3
		}
		return answers, []dnsRR{{"test.", tSOA, classIN, 3600, soaRdata}}, 0
	}
	return answers, nil, 0
}

// isFixtureParent reports whether some fixture name lies below name
// (an empty non-terminal: it exists, without records).
func isFixtureParent(name string) bool {
	for _, rr := range dnsFixtureZone {
		if strings.HasSuffix(strings.ToLower(rr.name), "."+name) {
			return true
		}
	}
	return false
}

// appendName writes a name, compressing it against comp when compress is
// set (and recording its suffixes either way).
func appendName(msg []byte, name string, comp map[string]int, compress bool) []byte {
	if name == "." {
		return append(msg, 0)
	}
	labels := splitLabels(name)
	for i := range labels {
		suffix := strings.ToLower(strings.Join(labels[i:], ".")) + "."
		if off, ok := comp[suffix]; ok && compress {
			return binary.BigEndian.AppendUint16(msg, uint16(0xC000|off))
		}
		if len(msg) < 0x3FFF {
			comp[suffix] = len(msg)
		}
		l := strings.ReplaceAll(strings.ReplaceAll(labels[i], `\.`, "."), `\\`, `\`)
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	return append(msg, 0)
}

// splitLabels splits a presentation name on its unescaped dots.
func splitLabels(name string) []string {
	name = strings.TrimSuffix(name, ".")
	var labels []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '\\' {
			i++
			continue
		}
		if name[i] == '.' {
			labels = append(labels, name[start:i])
			start = i + 1
		}
	}
	return append(labels, name[start:])
}

func appendRR(msg []byte, rr dnsRR, comp map[string]int) []byte {
	msg = appendName(msg, rr.name, comp, true)
	msg = binary.BigEndian.AppendUint16(msg, rr.typ)
	msg = binary.BigEndian.AppendUint16(msg, rr.class)
	msg = binary.BigEndian.AppendUint32(msg, rr.ttl)
	lenAt := len(msg)
	msg = append(msg, 0, 0)
	for _, p := range rr.rdata {
		switch v := p.(type) {
		case dnsName:
			msg = appendName(msg, string(v), comp, true)
		case dnsRawName:
			msg = appendName(msg, string(v), comp, false)
		case dnsU8:
			msg = append(msg, byte(v))
		case dnsU16:
			msg = binary.BigEndian.AppendUint16(msg, uint16(v))
		case dnsU32:
			msg = binary.BigEndian.AppendUint32(msg, uint32(v))
		case []byte:
			msg = append(msg, v...)
		}
	}
	binary.BigEndian.PutUint16(msg[lenAt:], uint16(len(msg)-lenAt-2))
	return msg
}
