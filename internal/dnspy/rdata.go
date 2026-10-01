package dnspy

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Rdata is one parsed record: its presentation form (to_text()) and,
// for the types the dig lookup turns into dicts, its attributes.
type Rdata struct {
	Class, Type uint16
	text        string
	textErr     error
	attrs       map[string]any
	rdclass     string // the dnspython class, for attribute errors
	covers      uint16
	target      Name // CNAME's target, for chaining
	key         string
}

// Text is rdata.to_text().
func (r *Rdata) Text() (string, error) { return r.text, r.textErr }

// Attr is getattr(rdata, name): a str (names, text), int64, float64,
// Bytes, Enum or []any (a tuple).
func (r *Rdata) Attr(name string) (any, error) {
	if v, ok := r.attrs[name]; ok {
		return v, nil
	}
	return nil, &Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", r.rdclass, name), Python: true}
}

// Bytes is a Python bytes attribute.
type Bytes []byte

// Enum is a dnspython IntEnum (or IntFlag) attribute: its class and value.
type Enum struct {
	Class string
	Value int64
}

// rdataClassOf is the dnspython class implementing typ in class (none:
// GenericRdata).
func rdataClassOf(class, typ uint16) string {
	name := TypeText(typ)
	if strings.HasPrefix(name, "TYPE") {
		return ""
	}
	py := strings.ReplaceAll(name, "-", "_")
	if class == ClassIN && inTypes[py] {
		return py
	}
	if class == ClassCH && py == "A" {
		return "A"
	}
	if anyTypes[py] {
		return py
	}
	return ""
}

var inTypes = map[string]bool{
	"A": true, "AAAA": true, "APL": true, "DHCID": true, "HTTPS": true, "IPSECKEY": true,
	"KX": true, "NAPTR": true, "NSAP": true, "NSAP_PTR": true, "PX": true, "SRV": true,
	"SVCB": true, "WKS": true,
}

var anyTypes = map[string]bool{
	"AFSDB": true, "AMTRELAY": true, "AVC": true, "CAA": true, "CDNSKEY": true, "CDS": true,
	"CERT": true, "CNAME": true, "CSYNC": true, "DLV": true, "DNAME": true, "DNSKEY": true,
	"DS": true, "DSYNC": true, "EUI48": true, "EUI64": true, "GPOS": true, "HINFO": true,
	"HIP": true, "ISDN": true, "L32": true, "L64": true, "LOC": true, "LP": true, "MX": true,
	"NID": true, "NINFO": true, "NS": true, "NSEC": true, "NSEC3": true, "NSEC3PARAM": true,
	"OPENPGPKEY": true, "PTR": true, "RESINFO": true, "RP": true, "RRSIG": true, "RT": true,
	"SMIMEA": true, "SOA": true, "SPF": true, "SSHFP": true, "TLSA": true, "TXT": true,
	"URI": true, "WALLET": true, "X25": true, "ZONEMD": true,
}

// escapify is dns.rdata._escapify.
func escapify(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		switch {
		case c == '"' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			s.WriteByte(c)
		default:
			fmt.Fprintf(&s, "\\%03d", c)
		}
	}
	return s.String()
}

// wordbreak is dns.rdata._wordbreak.
func wordbreak(s string, chunk int, sep string) string {
	if chunk <= 0 {
		return s
	}
	var parts []string
	for i := 0; i < len(s); i += chunk {
		parts = append(parts, s[i:min(i+chunk, len(s))])
	}
	return strings.Join(parts, sep)
}

func hexify(b []byte, chunk int) string { return wordbreak(hex.EncodeToString(b), chunk, " ") }

func base64ify(b []byte, chunk int) string {
	return wordbreak(base64.StdEncoding.EncodeToString(b), chunk, " ")
}

// bitmapText is dns.rdtypes.util.Bitmap: read from the rest of p,
// validated, and in its text form (each type with a leading space).
func bitmapText(p *parser, typeName string) (string, error) {
	var b strings.Builder
	last := -1
	for p.remaining() > 0 {
		window, err := p.u8()
		if err != nil {
			return "", err
		}
		bitmap, err := p.counted()
		if err != nil {
			return "", err
		}
		if window <= last {
			return "", valueError("bad %s window order", typeName)
		}
		last = window
		if len(bitmap) == 0 || len(bitmap) > 32 {
			return "", valueError("bad %s octets", typeName)
		}
		var bits []string
		for i, by := range bitmap {
			for j := 0; j < 8; j++ {
				if by&(0x80>>j) != 0 {
					bits = append(bits, TypeText(uint16(window*256+i*8+j)))
				}
			}
		}
		b.WriteString(" " + strings.Join(bits, " "))
	}
	return b.String(), nil
}

type rdataBuilder struct {
	rd *Rdata
	p  *parser
}

func (b *rdataBuilder) attr(name string, v any) { b.rd.attrs[name] = v }

// parseRdata is dns.rdata.from_wire_parser: the record's class from its
// type and class, its fields read and validated, and its text.
func parseRdata(class, typ uint16, p *parser) (*Rdata, error) {
	start := p.cur
	rd := &Rdata{Class: class, Type: typ, attrs: map[string]any{}}
	py := rdataClassOf(class, typ)
	rd.rdclass = py
	if py == "" {
		rd.rdclass = "GenericRdata"
		data := p.rest()
		rd.text = `\# ` + strconv.Itoa(len(data)) + " " + hexify(data, 32)
		rd.key = string(data)
		return rd, nil
	}
	b := &rdataBuilder{rd: rd, p: p}
	if err := b.parse(py); err != nil {
		return nil, err
	}
	if rd.key == "" {
		rd.key = string(p.wire[start:p.cur])
	}
	return rd, nil
}

func (b *rdataBuilder) name() (Name, error) { return b.p.name() }

// nameKey adds a name, lowercased, to the rdata's identity.
func (b *rdataBuilder) keyName(n Name) {
	b.rd.key += strings.ToLower(n.String()) + "\x00"
}

func (b *rdataBuilder) parse(py string) error {
	p, rd := b.p, b.rd
	switch py {
	case "A":
		if rd.Class == ClassCH {
			domain, err := b.name()
			if err != nil {
				return err
			}
			addr, err := p.u16()
			if err != nil {
				return err
			}
			b.attr("domain", domain.String())
			b.attr("address", int64(addr))
			rd.text = fmt.Sprintf("%s %o", domain, addr)
			b.keyName(domain)
			rd.key += strconv.Itoa(addr)
			return nil
		}
		data := p.rest()
		if len(data) != 4 {
			return valueError("IPv4 addresses are 4 bytes long")
		}
		rd.text = ipv4Ntoa(data)
		b.attr("address", rd.text)
	case "AAAA":
		data := p.rest()
		if len(data) != 16 {
			return valueError("IPv6 addresses are 16 bytes long")
		}
		rd.text = ipv6Ntoa(data)
		b.attr("address", rd.text)
	case "NS", "CNAME", "PTR", "DNAME", "NSAP_PTR":
		n, err := b.name()
		if err != nil {
			return err
		}
		rd.text = n.String()
		rd.target = n
		b.attr("target", rd.text)
		b.keyName(n)
	case "MX", "KX", "RT", "AFSDB":
		pref, err := p.u16()
		if err != nil {
			return err
		}
		n, err := b.name()
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf("%d %s", pref, n)
		b.attr("preference", int64(pref))
		b.attr("exchange", n.String())
		rd.key = strconv.Itoa(pref) + " "
		b.keyName(n)
	case "SOA":
		mname, err := b.name()
		if err != nil {
			return err
		}
		rname, err := b.name()
		if err != nil {
			return err
		}
		var v [5]uint32
		for i := range v {
			if v[i], err = p.u32(); err != nil {
				return err
			}
		}
		rd.text = fmt.Sprintf("%s %s %d %d %d %d %d", mname, rname, v[0], v[1], v[2], v[3], v[4])
		b.attr("mname", mname.String())
		b.attr("rname", rname.String())
		for i, f := range []string{"serial", "refresh", "retry", "expire", "minimum"} {
			b.attr(f, int64(v[i]))
		}
		rd.key = strings.ToLower(rd.text)
	case "TXT", "SPF", "AVC", "NINFO", "RESINFO", "WALLET":
		var strs []any
		var parts []string
		for p.remaining() > 0 {
			s, err := p.counted()
			if err != nil {
				return err
			}
			strs = append(strs, Bytes(s))
			parts = append(parts, `"`+escapify(s)+`"`)
		}
		if len(strs) == 0 {
			return valueError("the list of strings must not be empty")
		}
		rd.text = strings.Join(parts, " ")
		b.attr("strings", strs)
	case "HINFO":
		cpu, err := p.counted()
		if err != nil {
			return err
		}
		os, err := p.counted()
		if err != nil {
			return err
		}
		rd.text = `"` + escapify(cpu) + `" "` + escapify(os) + `"`
		b.attr("cpu", Bytes(cpu))
		b.attr("os", Bytes(os))
	case "RP":
		mbox, err := b.name()
		if err != nil {
			return err
		}
		txt, err := b.name()
		if err != nil {
			return err
		}
		rd.text = mbox.String() + " " + txt.String()
		b.attr("mbox", mbox.String())
		b.attr("txt", txt.String())
		b.keyName(mbox)
		b.keyName(txt)
	case "SRV":
		var v [3]int
		var err error
		for i := range v {
			if v[i], err = p.u16(); err != nil {
				return err
			}
		}
		n, err := b.name()
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf("%d %d %d %s", v[0], v[1], v[2], n)
		b.attr("priority", int64(v[0]))
		b.attr("weight", int64(v[1]))
		b.attr("port", int64(v[2]))
		b.attr("target", n.String())
		rd.key = fmt.Sprintf("%d %d %d ", v[0], v[1], v[2])
		b.keyName(n)
	case "NAPTR":
		order, err := p.u16()
		if err != nil {
			return err
		}
		pref, err := p.u16()
		if err != nil {
			return err
		}
		var s [3][]byte
		for i := range s {
			if s[i], err = p.counted(); err != nil {
				return err
			}
		}
		n, err := b.name()
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf(`%d %d "%s" "%s" "%s" %s`, order, pref, escapify(s[0]), escapify(s[1]), escapify(s[2]), n)
		b.attr("order", int64(order))
		b.attr("preference", int64(pref))
		b.attr("flags", Bytes(s[0]))
		b.attr("service", Bytes(s[1]))
		b.attr("regexp", Bytes(s[2]))
		b.attr("replacement", n.String())
		rd.key = rd.text
	case "CAA":
		flags, err := p.u8()
		if err != nil {
			return err
		}
		tag, err := p.counted()
		if err != nil {
			return err
		}
		value := p.rest()
		if !isAlnum(tag) {
			return valueError("tag is not alphanumeric")
		}
		rd.text = fmt.Sprintf(`%d %s "%s"`, flags, escapify(tag), escapify(value))
		b.attr("flags", int64(flags))
		b.attr("tag", Bytes(tag))
		b.attr("value", Bytes(value))
	case "DNSKEY", "CDNSKEY":
		flags, err := p.u16()
		if err != nil {
			return err
		}
		proto, err := p.u8()
		if err != nil {
			return err
		}
		alg, err := p.u8()
		if err != nil {
			return err
		}
		key := p.rest()
		rd.text = fmt.Sprintf("%d %d %d %s", flags, proto, alg, base64ify(key, 32))
		b.attr("flags", Enum{"Flag", int64(flags)})
		b.attr("protocol", int64(proto))
		b.attr("algorithm", Enum{"Algorithm", int64(alg)})
		b.attr("key", Bytes(key))
	case "DS", "CDS", "DLV":
		tag, err := p.u16()
		if err != nil {
			return err
		}
		alg, err := p.u8()
		if err != nil {
			return err
		}
		dt, err := p.u8()
		if err != nil {
			return err
		}
		digest := p.rest()
		lengths := map[int]int{1: 20, 2: 32, 3: 32, 4: 48}
		if py == "CDS" {
			lengths[0] = 1
		}
		if want, ok := lengths[dt]; ok {
			if len(digest) != want {
				return valueError("digest length inconsistent with digest type")
			}
		} else if dt == 0 {
			return valueError("digest type 0 is reserved")
		}
		rd.text = fmt.Sprintf("%d %d %d %s", tag, alg, dt, hexify(digest, 128))
		b.attr("key_tag", int64(tag))
		b.attr("algorithm", Enum{"Algorithm", int64(alg)})
		b.attr("digest_type", Enum{"DSDigest", int64(dt)})
		b.attr("digest", Bytes(digest))
	case "SSHFP":
		alg, err := p.u8()
		if err != nil {
			return err
		}
		fpt, err := p.u8()
		if err != nil {
			return err
		}
		fp := p.rest()
		rd.text = fmt.Sprintf("%d %d %s", alg, fpt, hexify(fp, 128))
		b.attr("algorithm", int64(alg))
		b.attr("fp_type", int64(fpt))
		b.attr("fingerprint", Bytes(fp))
	case "TLSA", "SMIMEA":
		var v [3]int
		var err error
		for i := range v {
			if v[i], err = p.u8(); err != nil {
				return err
			}
		}
		cert := p.rest()
		rd.text = fmt.Sprintf("%d %d %d %s", v[0], v[1], v[2], hexify(cert, 128))
		b.attr("usage", int64(v[0]))
		b.attr("selector", int64(v[1]))
		b.attr("mtype", int64(v[2]))
		b.attr("cert", Bytes(cert))
	case "NSEC3PARAM":
		alg, err := p.u8()
		if err != nil {
			return err
		}
		flags, err := p.u8()
		if err != nil {
			return err
		}
		iter, err := p.u16()
		if err != nil {
			return err
		}
		salt, err := p.counted()
		if err != nil {
			return err
		}
		s := "-"
		if len(salt) > 0 {
			s = hex.EncodeToString(salt)
		}
		rd.text = fmt.Sprintf("%d %d %d %s", alg, flags, iter, s)
		b.attr("algorithm", int64(alg))
		b.attr("flags", int64(flags))
		b.attr("iterations", int64(iter))
		b.attr("salt", Bytes(salt))
	case "NSEC3":
		alg, err := p.u8()
		if err != nil {
			return err
		}
		flags, err := p.u8()
		if err != nil {
			return err
		}
		iter, err := p.u16()
		if err != nil {
			return err
		}
		salt, err := p.counted()
		if err != nil {
			return err
		}
		next, err := p.counted()
		if err != nil {
			return err
		}
		bits, err := bitmapText(p, "NSEC3")
		if err != nil {
			return err
		}
		s := "-"
		if len(salt) > 0 {
			s = hex.EncodeToString(salt)
		}
		nt := strings.ToLower(strings.TrimRight(base32.HexEncoding.EncodeToString(next), "="))
		rd.text = fmt.Sprintf("%d %d %d %s %s%s", alg, flags, iter, s, nt, bits)
	case "NSEC":
		next, err := b.name()
		if err != nil {
			return err
		}
		bits, err := bitmapText(p, "NSEC")
		if err != nil {
			return err
		}
		rd.text = next.String() + bits
		rd.key = strings.ToLower(rd.text)
	case "CSYNC":
		serial, err := p.u32()
		if err != nil {
			return err
		}
		flags, err := p.u16()
		if err != nil {
			return err
		}
		bits, err := bitmapText(p, "CSYNC")
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf("%d %d%s", serial, flags, bits)
	case "ZONEMD":
		serial, err := p.u32()
		if err != nil {
			return err
		}
		scheme, err := p.u8()
		if err != nil {
			return err
		}
		alg, err := p.u8()
		if err != nil {
			return err
		}
		digest := p.rest()
		if scheme == 0 {
			return valueError("scheme 0 is reserved")
		}
		if alg == 0 {
			return valueError("hash_algorithm 0 is reserved")
		}
		if size, ok := map[int]int{1: 48, 2: 64}[alg]; ok && size != len(digest) {
			return valueError("digest length inconsistent with hash algorithm")
		}
		rd.text = fmt.Sprintf("%d %d %d %s", serial, scheme, alg, hexify(digest, 128))
	case "LOC":
		return b.parseLOC()
	case "RRSIG":
		var hdr [7]uint32
		widths := []int{2, 1, 1, 4, 4, 4, 2}
		for i, w := range widths {
			raw, err := p.bytes(w)
			if err != nil {
				return err
			}
			var v uint32
			for _, c := range raw {
				v = v<<8 | uint32(c)
			}
			hdr[i] = v
		}
		signer, err := b.name()
		if err != nil {
			return err
		}
		sig := p.rest()
		rd.covers = uint16(hdr[0])
		rd.text = fmt.Sprintf("%s %d %d %d %s %s %d %s %s", TypeText(uint16(hdr[0])), hdr[1], hdr[2], hdr[3],
			sigtime(hdr[4]), sigtime(hdr[5]), hdr[6], signer, base64ify(sig, 32))
	case "URI":
		prio, err := p.u16()
		if err != nil {
			return err
		}
		weight, err := p.u16()
		if err != nil {
			return err
		}
		target := p.rest()
		if len(target) == 0 {
			return formError("URI target may not be empty")
		}
		if !utf8.Valid(target) {
			rd.textErr = &Error{Class: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode bytes", Python: true}
		}
		rd.text = fmt.Sprintf(`%d %d "%s"`, prio, weight, target)
	case "EUI48", "EUI64":
		n := 6
		if py == "EUI64" {
			n = 8
		}
		eui, err := p.bytes(n)
		if err != nil {
			return err
		}
		rd.text = wordbreak(hex.EncodeToString(eui), 2, "-")
	case "OPENPGPKEY":
		rd.text = base64ify(p.rest(), 0)
	case "DHCID":
		rd.text = base64ify(p.rest(), 32)
	case "CERT":
		ct, err := p.u16()
		if err != nil {
			return err
		}
		tag, err := p.u16()
		if err != nil {
			return err
		}
		alg, err := p.u8()
		if err != nil {
			return err
		}
		cert := p.rest()
		ctext, ok := map[int]string{1: "PKIX", 2: "SPKI", 3: "PGP", 4: "IPKIX", 5: "ISPKI", 6: "IPGP", 7: "ACPKIX", 8: "IACPKIX", 253: "URI", 254: "OID"}[ct]
		if !ok {
			ctext = strconv.Itoa(ct)
		}
		rd.text = fmt.Sprintf("%s %d %s %s", ctext, tag, algorithmText(alg), base64ify(cert, 32))
	case "GPOS":
		var s [3][]byte
		var err error
		for i := range s {
			if s[i], err = p.counted(); err != nil {
				return err
			}
			if !gposFloat(s[i]) {
				return errFormError
			}
		}
		lat, _ := strconv.ParseFloat(string(s[0]), 64)
		long, _ := strconv.ParseFloat(string(s[1]), 64)
		if lat < -90 || lat > 90 {
			return formError("bad latitude")
		}
		if long < -180 || long > 180 {
			return formError("bad longitude")
		}
		rd.text = fmt.Sprintf("%s %s %s", s[0], s[1], s[2])
	case "ISDN":
		addr, err := p.counted()
		if err != nil {
			return err
		}
		var sub []byte
		if p.remaining() > 0 {
			if sub, err = p.counted(); err != nil {
				return err
			}
		}
		rd.text = `"` + escapify(addr) + `"`
		if len(sub) > 0 {
			rd.text += ` "` + escapify(sub) + `"`
		}
	case "X25":
		addr, err := p.counted()
		if err != nil {
			return err
		}
		rd.text = `"` + escapify(addr) + `"`
	case "WKS":
		addr, err := p.bytes(4)
		if err != nil {
			return err
		}
		proto, err := p.u8()
		if err != nil {
			return err
		}
		var bits []string
		for i, by := range p.rest() {
			for j := 0; j < 8; j++ {
				if by&(0x80>>j) != 0 {
					bits = append(bits, strconv.Itoa(i*8+j))
				}
			}
		}
		rd.text = fmt.Sprintf("%s %d %s", ipv4Ntoa(addr), proto, strings.Join(bits, " "))
	case "APL":
		var items []string
		for p.remaining() > 0 {
			family, err := p.u16()
			if err != nil {
				return err
			}
			prefix, err := p.u8()
			if err != nil {
				return err
			}
			afdlen, err := p.u8()
			if err != nil {
				return err
			}
			neg := ""
			if afdlen > 127 {
				neg, afdlen = "!", afdlen-128
			}
			addr, err := p.bytes(afdlen)
			if err != nil {
				return err
			}
			var text string
			switch family {
			case 1:
				if len(addr) > 4 {
					return valueError("IPv4 addresses are 4 bytes long")
				}
				if prefix > 32 {
					return valueError("value too large")
				}
				text = ipv4Ntoa(append(append([]byte{}, addr...), make([]byte, 4-len(addr))...))
			case 2:
				if len(addr) > 16 {
					return valueError("IPv6 addresses are 16 bytes long")
				}
				if prefix > 128 {
					return valueError("value too large")
				}
				text = ipv6Ntoa(append(append([]byte{}, addr...), make([]byte, 16-len(addr))...))
			default:
				text = "b'" + hex.EncodeToString(addr) + "'"
			}
			items = append(items, fmt.Sprintf("%s%d:%s/%d", neg, family, text, prefix))
		}
		rd.text = strings.Join(items, " ")
	case "IPSECKEY", "AMTRELAY":
		prec, err := p.u8()
		if err != nil {
			return err
		}
		gtype, err := p.u8()
		if err != nil {
			return err
		}
		dflag := 0
		if py == "AMTRELAY" {
			dflag = gtype >> 7
			gtype &= 0x7f
		}
		alg := 0
		if py == "IPSECKEY" {
			if alg, err = p.u8(); err != nil {
				return err
			}
		}
		gname := "IPSECKEY gateway"
		if py == "AMTRELAY" {
			gname = "AMTRELAY relay"
		}
		var gw string
		switch gtype {
		case 0:
			gw = "."
		case 1:
			a, err := p.bytes(4)
			if err != nil {
				return err
			}
			gw = ipv4Ntoa(a)
		case 2:
			a, err := p.bytes(16)
			if err != nil {
				return err
			}
			gw = ipv6Ntoa(a)
		case 3:
			n, err := b.name()
			if err != nil {
				return err
			}
			gw = n.String()
		default:
			return formError(fmt.Sprintf("invalid %s type: %d", gname, gtype))
		}
		if py == "AMTRELAY" {
			rd.text = fmt.Sprintf("%d %d %d %s", prec, dflag, gtype, gw)
		} else {
			rd.text = fmt.Sprintf("%d %d %d %s %s", prec, gtype, alg, gw, base64ify(p.rest(), 32))
		}
	case "HIP":
		lh, err := p.u8()
		if err != nil {
			return err
		}
		alg, err := p.u8()
		if err != nil {
			return err
		}
		lk, err := p.u16()
		if err != nil {
			return err
		}
		hit, err := p.bytes(lh)
		if err != nil {
			return err
		}
		key, err := p.bytes(lk)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("%d %s %s", alg, hex.EncodeToString(hit), base64.StdEncoding.EncodeToString(key))
		var servers []string
		for p.remaining() > 0 {
			n, err := b.name()
			if err != nil {
				return err
			}
			servers = append(servers, n.String())
		}
		if len(servers) > 0 {
			text += " " + strings.Join(servers, " ")
		}
		rd.text = text
	case "NID", "L64":
		pref, err := p.u16()
		if err != nil {
			return err
		}
		v := p.rest()
		if len(v) != 8 {
			return valueError("invalid %s", map[string]string{"NID": "nodeid", "L64": "locator64"}[py])
		}
		rd.text = fmt.Sprintf("%d %s", pref, wordbreak(hex.EncodeToString(v), 4, ":"))
	case "L32":
		pref, err := p.u16()
		if err != nil {
			return err
		}
		v := p.rest()
		if len(v) != 4 {
			return valueError("IPv4 addresses are 4 bytes long")
		}
		rd.text = fmt.Sprintf("%d %s", pref, ipv4Ntoa(v))
	case "LP":
		pref, err := p.u16()
		if err != nil {
			return err
		}
		n, err := b.name()
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf("%d %s", pref, n)
	case "DSYNC":
		rrtype, err := p.u16()
		if err != nil {
			return err
		}
		scheme, err := p.u8()
		if err != nil {
			return err
		}
		port, err := p.u16()
		if err != nil {
			return err
		}
		n, err := b.name()
		if err != nil {
			return err
		}
		st := strconv.Itoa(scheme)
		if scheme == 1 {
			st = "NOTIFY"
		}
		rd.text = fmt.Sprintf("%s %s %d %s", TypeText(uint16(rrtype)), st, port, n)
	case "NSAP":
		rd.text = "0x" + hex.EncodeToString(p.rest())
	case "PX":
		pref, err := p.u16()
		if err != nil {
			return err
		}
		m822, err := b.name()
		if err != nil {
			return err
		}
		x400, err := b.name()
		if err != nil {
			return err
		}
		rd.text = fmt.Sprintf("%d %s %s", pref, m822, x400)
	case "SVCB", "HTTPS":
		return b.parseSVCB()
	default:
		return fmt.Errorf("dnspy: no parser for %s", py)
	}
	return nil
}

func isAlnum(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// gposFloat is GPOS's _validate_float_string.
func gposFloat(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if isDecimal(string(s)) {
		return true
	}
	left, right, ok := bytes.Cut(s, []byte("."))
	if !ok || bytes.Contains(right, []byte(".")) {
		return false
	}
	if len(left) == 0 && len(right) == 0 {
		return false
	}
	return (len(left) == 0 || isDecimal(string(left))) && (len(right) == 0 || isDecimal(string(right)))
}

// sigtime is RRSIG's posixtime_to_sigtime.
func sigtime(t uint32) string {
	days := int64(t) / 86400
	secs := int64(t) % 86400
	// civil from days (Howard Hinnant's algorithm)
	z := days + 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if m > 12 {
		m -= 12
	}
	if m <= 2 {
		y++
	}
	return fmt.Sprintf("%04d%02d%02d%02d%02d%02d", y, m, d, secs/3600, secs%3600/60, secs%60)
}

func (b *rdataBuilder) parseLOC() error {
	p, rd := b.p, b.rd
	var hdr [4]int
	var err error
	for i := range hdr {
		if hdr[i], err = p.u8(); err != nil {
			return err
		}
	}
	var coords [3]uint32
	for i := range coords {
		if coords[i], err = p.u32(); err != nil {
			return err
		}
	}
	if hdr[0] != 0 {
		return formError("LOC version not zero")
	}
	const base = 0x80000000
	toFloat := func(v uint32, limit uint32, what string) (float64, error) {
		if v < base-limit || v > base+limit {
			return 0, formError("bad " + what)
		}
		if v > base {
			return float64(v-base) / 3600000, nil
		}
		return -1 * float64(base-v) / 3600000, nil
	}
	lat, err := toFloat(coords[0], 90*3600000, "latitude")
	if err != nil {
		return err
	}
	long, err := toFloat(coords[1], 180*3600000, "longitude")
	if err != nil {
		return err
	}
	alt := float64(coords[2]) - 10000000.0
	var sizes [3]float64
	for i, what := range []string{"size", "horizontal precision", "vertical precision"} {
		v := hdr[i+1]
		exp, mant := v&0x0F, (v&0xF0)>>4
		if exp > 9 {
			return formError("bad " + what + " exponent")
		}
		if mant > 9 {
			return formError("bad " + what + " base")
		}
		sizes[i] = float64(mant) * math.Pow10(exp)
	}
	latT, longT := floatToTuple(lat), floatToTuple(long)
	if latT[0] > 90 || longT[0] > 180 {
		return valueError("not in range")
	}
	hemi := func(t [5]int64, pos, neg string) string {
		if t[4] > 0 {
			return pos
		}
		return neg
	}
	text := fmt.Sprintf("%d %d %d.%03d %s %d %d %d.%03d %s %sm", latT[0], latT[1], latT[2], latT[3], hemi(latT, "N", "S"),
		longT[0], longT[1], longT[2], longT[3], hemi(longT, "E", "W"), pyFixed2(alt/100.0))
	if sizes[0] != 100.0 || sizes[1] != 1000000.0 || sizes[2] != 1000.0 {
		text += fmt.Sprintf(" %sm %sm %sm", pyFixed2(sizes[0]/100.0), pyFixed2(sizes[1]/100.0), pyFixed2(sizes[2]/100.0))
	}
	rd.text = text
	tuple := func(t [5]int64) []any { return []any{t[0], t[1], t[2], t[3], t[4]} }
	b.attr("latitude", tuple(latT))
	b.attr("longitude", tuple(longT))
	b.attr("altitude", alt)
	b.attr("size", sizes[0])
	b.attr("horizontal_precision", sizes[1])
	b.attr("vertical_precision", sizes[2])
	return nil
}

// floatToTuple is LOC's _float_to_tuple.
func floatToTuple(what float64) [5]int64 {
	sign := int64(1)
	if what < 0 {
		sign = -1
		what *= -1
	}
	w := int64(math.RoundToEven(what * 3600000))
	deg := w / 3600000
	w -= deg * 3600000
	minutes := w / 60000
	w -= minutes * 60000
	secs := w / 1000
	w -= secs * 1000
	return [5]int64{deg, minutes, secs, w, sign}
}

// pyFixed2 is format(f, "0.2f").
func pyFixed2(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// svcbKeyText is SVCB's key_to_text.
func svcbKeyText(k int) string {
	names := []string{"mandatory", "alpn", "no-default-alpn", "port", "ipv4hint", "ech", "ipv6hint", "dohpath", "ohttp"}
	if k < len(names) {
		return names[k]
	}
	return "key" + strconv.Itoa(k)
}

func svcbEscapify(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		switch {
		case c == '"' || c == ',' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			s.WriteByte(c)
		default:
			fmt.Fprintf(&s, "\\%03d", c)
		}
	}
	return s.String()
}

func (b *rdataBuilder) parseSVCB() error {
	p, rd := b.p, b.rd
	prio, err := p.u16()
	if err != nil {
		return err
	}
	target, err := b.name()
	if err != nil {
		return err
	}
	if prio == 0 && p.remaining() != 0 {
		return formError("parameters in AliasMode")
	}
	params := map[int]*string{}
	var mandatory []int
	prior := -1
	for p.remaining() > 0 {
		key, err := p.u16()
		if err != nil {
			return err
		}
		if key < prior {
			return formError("keys not in order")
		}
		prior = key
		vlen, err := p.u16()
		if err != nil {
			return err
		}
		val, err := p.bytes(vlen)
		if err != nil {
			return err
		}
		vp := &parser{wire: val, end: len(val)}
		var text *string
		set := func(s string) { text = &s }
		switch key {
		case 0:
			var keys []int
			last := -1
			for vp.remaining() > 0 {
				k, err := vp.u16()
				if err != nil {
					return err
				}
				if k < last {
					return formError("manadatory keys not ascending")
				}
				last = k
				keys = append(keys, k)
			}
			sort.Ints(keys)
			var names []string
			for i, k := range keys {
				if i > 0 && keys[i-1] == k {
					return valueError("duplicate key %d", k)
				}
				if k == 0 {
					return valueError("listed the mandatory key as mandatory")
				}
				names = append(names, svcbKeyText(k))
			}
			mandatory = keys
			set(`"` + strings.Join(names, ",") + `"`)
		case 1:
			var ids []string
			for vp.remaining() > 0 {
				id, err := vp.counted()
				if err != nil {
					return err
				}
				if len(id) == 0 {
					return valueError("empty bytes not allowed")
				}
				ids = append(ids, svcbEscapify(id))
			}
			set(`"` + escapify([]byte(strings.Join(ids, ","))) + `"`)
		case 2, 8:
			if len(val) != 0 {
				return errFormError
			}
		case 3:
			port, err := vp.u16()
			if err != nil {
				return err
			}
			if vp.remaining() != 0 {
				return errFormError
			}
			set(`"` + strconv.Itoa(port) + `"`)
		case 4, 6:
			n := 4
			if key == 6 {
				n = 16
			}
			var addrs []string
			for vp.remaining() > 0 {
				a, err := vp.bytes(n)
				if err != nil {
					return err
				}
				if n == 4 {
					addrs = append(addrs, ipv4Ntoa(a))
				} else {
					addrs = append(addrs, ipv6Ntoa(a))
				}
			}
			set(`"` + strings.Join(addrs, ",") + `"`)
		case 5:
			set(`"` + base64.StdEncoding.EncodeToString(val) + `"`)
		default:
			if len(val) > 0 {
				set(`"` + escapify(val) + `"`)
			}
		}
		params[key] = text
	}
	for _, k := range mandatory {
		if _, ok := params[k]; !ok {
			return valueError("key %d declared mandatory but not present", k)
		}
	}
	if _, ok := params[2]; ok {
		if _, ok := params[1]; !ok {
			return valueError("no-default-alpn present, but alpn missing")
		}
	}
	keys := make([]int, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var parts []string
	for _, k := range keys {
		if params[k] == nil {
			parts = append(parts, svcbKeyText(k))
		} else {
			parts = append(parts, svcbKeyText(k)+"="+*params[k])
		}
	}
	rd.text = fmt.Sprintf("%d %s", prio, target)
	if len(parts) > 0 {
		rd.text += " " + strings.Join(parts, " ")
	}
	return nil
}
