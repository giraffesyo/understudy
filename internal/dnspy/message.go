package dnspy

import (
	"encoding/binary"
	"strconv"
)

type question struct {
	name       Name
	typ, class uint16
}

// RRset is a dnspython RRset: one name, class and type (and covered type),
// its rdatas deduplicated in wire order, its TTL their minimum.
type RRset struct {
	Name   Name
	Class  uint16
	Type   uint16
	Covers uint16
	TTL    uint32
	Rdatas []*Rdata
	keys   map[string]bool
}

func (s *RRset) add(rd *Rdata, ttl uint32) {
	if len(s.Rdatas) == 0 || ttl < s.TTL {
		s.TTL = ttl
	}
	if isSingleton(rd.Type) && len(s.Rdatas) > 0 {
		s.Rdatas, s.keys = nil, nil
	}
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	if s.keys[rd.key] {
		return
	}
	s.keys[rd.key] = true
	s.Rdatas = append(s.Rdatas, rd)
}

func isSingleton(t uint16) bool {
	switch t {
	case TypeSOA, 30, TypeDNAME, 47, TypeCNAME:
		return true
	}
	return false
}

// message is a parsed DNS response.
type message struct {
	id        uint16
	flags     uint16
	ednsFlags uint32
	question  []question
	answer    []*RRset
	authority []*RRset
}

func (m *message) rcode() int {
	return int(m.flags&0xF) | int((m.ednsFlags>>20)&0xFF0)
}

var rcodeNames = map[int]string{
	0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN", 4: "NOTIMP", 5: "REFUSED",
	6: "YXDOMAIN", 7: "YXRRSET", 8: "NXRRSET", 9: "NOTAUTH", 10: "NOTZONE", 11: "DSOTYPENI",
	16: "BADVERS", 17: "BADKEY", 18: "BADTIME", 19: "BADMODE", 20: "BADNAME", 21: "BADALG",
	22: "BADTRUNC", 23: "BADCOOKIE",
}

func rcodeText(rc int) string {
	if s, ok := rcodeNames[rc]; ok {
		return s
	}
	return strconv.Itoa(rc)
}

// makeQuery is dns.message.make_query: a recursive query, with an OPT
// record when edns is set.
func makeQuery(id uint16, q question, edns bool, payload uint16, ednsFlags uint32) []byte {
	msg := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(msg, id)
	binary.BigEndian.PutUint16(msg[2:], 0x0100) // RD
	binary.BigEndian.PutUint16(msg[4:], 1)
	msg = appendWireName(msg, q.name)
	msg = binary.BigEndian.AppendUint16(msg, q.typ)
	msg = binary.BigEndian.AppendUint16(msg, q.class)
	if !edns {
		return msg
	}
	binary.BigEndian.PutUint16(msg[10:], 1)
	msg = append(msg, 0) // root
	msg = binary.BigEndian.AppendUint16(msg, TypeOPT)
	msg = binary.BigEndian.AppendUint16(msg, payload)
	msg = binary.BigEndian.AppendUint32(msg, ednsFlags)
	msg = binary.BigEndian.AppendUint16(msg, 0)
	return msg
}

func appendWireName(msg []byte, n Name) []byte {
	for _, l := range n.Labels {
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	return msg
}

// parser is dns.wire.Parser.
type parser struct {
	wire     []byte
	cur, end int
}

func (p *parser) remaining() int { return p.end - p.cur }

func (p *parser) bytes(n int) ([]byte, error) {
	if n < 0 || n > p.remaining() {
		return nil, errFormError
	}
	b := p.wire[p.cur : p.cur+n]
	p.cur += n
	return b, nil
}

func (p *parser) u8() (int, error) {
	b, err := p.bytes(1)
	if err != nil {
		return 0, err
	}
	return int(b[0]), nil
}

func (p *parser) u16() (int, error) {
	b, err := p.bytes(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b)), nil
}

func (p *parser) u32() (uint32, error) {
	b, err := p.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (p *parser) counted() ([]byte, error) {
	n, err := p.u8()
	if err != nil {
		return nil, err
	}
	return p.bytes(n)
}

func (p *parser) rest() []byte {
	b := p.wire[p.cur:p.end]
	p.cur = p.end
	return b
}

// name is dns.name.from_wire_parser.
func (p *parser) name() (Name, error) {
	var labels [][]byte
	biggest := p.cur
	cur, furthest := p.cur, -1
	for {
		if cur >= len(p.wire) || (furthest < 0 && cur >= p.end) {
			return Name{}, errFormError
		}
		count := int(p.wire[cur])
		cur++
		if count == 0 {
			break
		}
		switch {
		case count < 64:
			if cur+count > len(p.wire) || (furthest < 0 && cur+count > p.end) {
				return Name{}, errFormError
			}
			labels = append(labels, append([]byte{}, p.wire[cur:cur+count]...))
			cur += count
		case count >= 192:
			if cur >= len(p.wire) || (furthest < 0 && cur >= p.end) {
				return Name{}, errFormError
			}
			ptr := (count&0x3F)*256 + int(p.wire[cur])
			cur++
			if ptr >= biggest {
				return Name{}, errBadPointer
			}
			biggest = ptr
			if furthest < 0 {
				furthest = cur
			}
			cur = ptr
		default:
			return Name{}, errBadLabelType
		}
	}
	if furthest < 0 {
		furthest = cur
	}
	p.cur = furthest
	labels = append(labels, []byte{})
	if err := validateLabels(labels); err != nil {
		return Name{}, err
	}
	return Name{Labels: labels}, nil
}

// parseMessage is dns.message.from_wire for a response: Truncated (with
// the message as far as it was read) when its TC flag is set.
func parseMessage(wire []byte) (*message, error) {
	p := &parser{wire: wire, end: len(wire)}
	m := &message{}
	hdr, err := p.bytes(12)
	if err != nil {
		return nil, err
	}
	m.id = binary.BigEndian.Uint16(hdr)
	m.flags = binary.BigEndian.Uint16(hdr[2:])
	counts := [4]int{}
	for i := range counts {
		counts[i] = int(binary.BigEndian.Uint16(hdr[4+2*i:]))
	}
	err = m.read(p, counts)
	if m.flags&0x0200 != 0 {
		return m, errTruncated
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (m *message) read(p *parser, counts [4]int) error {
	for i := 0; i < counts[0]; i++ {
		n, err := p.name()
		if err != nil {
			return err
		}
		t, err := p.u16()
		if err != nil {
			return err
		}
		c, err := p.u16()
		if err != nil {
			return err
		}
		q := question{n, uint16(t), uint16(c)}
		dup := false
		for _, o := range m.question {
			if o.name.Equal(q.name) && o.typ == q.typ && o.class == q.class {
				dup = true
			}
		}
		if !dup {
			m.question = append(m.question, q)
		}
	}
	optSeen := false
	for section := 1; section <= 3; section++ {
		for i := 0; i < counts[section]; i++ {
			owner, err := p.name()
			if err != nil {
				return err
			}
			hdr, err := p.bytes(10)
			if err != nil {
				return err
			}
			typ := binary.BigEndian.Uint16(hdr)
			class := binary.BigEndian.Uint16(hdr[2:])
			ttl := binary.BigEndian.Uint32(hdr[4:])
			rdlen := int(binary.BigEndian.Uint16(hdr[8:]))
			if rdlen > p.remaining() {
				return errFormError
			}
			if typ == TypeOPT {
				if section != 3 || optSeen || !owner.Equal(Root) {
					return &Error{Class: "BadEDNS", Msg: "An OPT record occurs somewhere other than the additional data section.", Form: true}
				}
				optSeen = true
				m.ednsFlags = ttl
				p.cur += rdlen
				continue
			}
			if typ == 250 { // TSIG: no keyring to check it with
				return &Error{Class: "UnknownTSIGKey", Msg: "got signed message without keyring"}
			}
			sub := &parser{wire: p.wire, cur: p.cur, end: p.cur + rdlen}
			rd, err := parseRdata(class, typ, sub)
			if err != nil {
				return err
			}
			if sub.cur != sub.end {
				return errFormError
			}
			p.cur += rdlen
			if section == 3 {
				continue
			}
			sec := &m.answer
			if section == 2 {
				sec = &m.authority
			}
			var set *RRset
			for _, s := range *sec {
				if s.Name.Equal(owner) && s.Class == class && s.Type == typ && s.Covers == rd.covers {
					set = s
					break
				}
			}
			if set == nil {
				set = &RRset{Name: owner, Class: class, Type: typ, Covers: rd.covers}
				*sec = append(*sec, set)
			}
			set.add(rd, ttl)
		}
	}
	if p.remaining() != 0 {
		return &Error{Class: "TrailingJunk", Msg: "The DNS message has trailing junk.", Form: true}
	}
	return nil
}

// isResponse is Message.is_response for our query.
func (m *message) isResponse(id uint16, q question) bool {
	if m.flags&0x8000 == 0 || m.id != id || (m.flags>>11)&0xF != 0 {
		return false
	}
	switch m.rcode() {
	case 1, 2, 4, 5:
		if len(m.question) == 0 {
			return true
		}
	}
	if len(m.question) != 1 {
		return false
	}
	o := m.question[0]
	return o.name.Equal(q.name) && o.typ == q.typ && o.class == q.class
}

func findRRset(sec []*RRset, name Name, class, typ uint16) *RRset {
	for _, s := range sec {
		if s.Name.Equal(name) && s.Class == class && s.Type == typ && s.Covers == 0 {
			return s
		}
	}
	return nil
}

// chainingResult is QueryMessage.resolve_chaining.
type chainingResult struct {
	canonical Name
	answer    *RRset
}

func (m *message) resolveChaining() (chainingResult, error) {
	if m.flags&0x8000 == 0 {
		return chainingResult{}, &Error{Class: "NotQueryResponse", Msg: "Message is not a response to a query."}
	}
	if len(m.question) != 1 {
		return chainingResult{}, errFormError
	}
	q := m.question[0]
	qname := q.name
	var answer *RRset
	count := 0
	for count < 16 {
		if answer = findRRset(m.answer, qname, q.class, q.typ); answer != nil {
			break
		}
		if q.typ == TypeCNAME {
			break
		}
		c := findRRset(m.answer, qname, q.class, TypeCNAME)
		if c == nil {
			break
		}
		for _, rd := range c.Rdatas {
			qname = rd.target
			break
		}
		count++
	}
	if count >= 16 {
		return chainingResult{}, errChainTooLng
	}
	if m.rcode() == 3 && answer != nil {
		return chainingResult{}, errAnswerForNX
	}
	return chainingResult{canonical: qname, answer: answer}, nil
}
