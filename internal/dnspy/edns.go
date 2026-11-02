package dnspy

import "fmt"

// parseTSIG is the TSIG rdata's from_wire_parser (its algorithm name may
// not be compressed since 2.9), read before the missing keyring is
// noticed.
func parseTSIG(p *parser) error {
	if _, err := p.name(false); err != nil {
		return err
	}
	if _, err := p.bytes(8); err != nil { // time signed, fudge
		return err
	}
	if err := p.counted16(); err != nil { // mac
		return err
	}
	if _, err := p.u16(); err != nil { // original id
		return err
	}
	rcode, err := p.u16()
	if err != nil {
		return err
	}
	if err := p.counted16(); err != nil { // other
		return err
	}
	// The constructor's Rcode.make, under the ExceptionWrapper.
	if rcode > 4095 {
		return formError("rcode must be an int between >= 0 and <= 4095")
	}
	if p.remaining() != 0 {
		return errFormError
	}
	return nil
}

// counted16 skips get_counted_bytes(2).
func (p *parser) counted16() error {
	n, err := p.u16()
	if err != nil {
		return err
	}
	_, err = p.bytes(n)
	return err
}

// parseOptions is the OPT rdata's from_wire_parser: each option read by
// dns.edns.option_from_wire_parser within its length, its errors (under
// dns.rdata's ExceptionWrapper) FormErrors.
func parseOptions(p *parser) error {
	for p.remaining() > 0 {
		otype, err := p.u16()
		if err != nil {
			return err
		}
		olen, err := p.u16()
		if err != nil {
			return err
		}
		if olen > p.remaining() {
			return errFormError
		}
		op := &parser{wire: p.wire, cur: p.cur, end: p.cur + olen}
		if err := parseOption(otype, op); err != nil {
			if e, ok := err.(*Error); ok && !e.Form {
				return formError(e.Msg)
			}
			return err
		}
		if op.cur != op.end {
			return errFormError
		}
		p.cur += olen
	}
	return nil
}

func parseOption(otype int, p *parser) error {
	switch otype {
	case 8: // ECS
		family, err := p.u16()
		if err != nil {
			return err
		}
		src, err := p.u8()
		if err != nil {
			return err
		}
		scope, err := p.u8()
		if err != nil {
			return err
		}
		bits := 0
		switch family {
		case 1:
			bits = 32
		case 2:
			bits = 128
		default:
			return valueError("unsupported family")
		}
		// RFC 7871 section 6 (2.9): the prefix lengths fit the family and
		// the address bits past the source prefix are zero.
		if src > bits || scope > bits {
			return valueError("invalid ECS prefix length")
		}
		prefix, err := p.bytes((src + 7) / 8)
		if err != nil {
			return err
		}
		if rest := src % 8; rest != 0 && prefix[len(prefix)-1]&(0xFF>>rest) != 0 {
			return valueError("non-zero ECS address padding bits")
		}
	case 15: // EDE: a code, then text that may be null-terminated
		if _, err := p.u16(); err != nil {
			return err
		}
		text := p.rest()
		if len(text) > 0 && text[len(text)-1] == 0 {
			text = text[:len(text)-1]
		}
		return utf8Decode(text)
	case 10: // COOKIE
		if _, err := p.bytes(8); err != nil {
			return err
		}
		if server := p.rest(); len(server) != 0 && (len(server) < 8 || len(server) > 32) {
			return valueError("server cookie must be empty or between 8 and 32 bytes")
		}
	case 18: // REPORTCHANNEL
		_, err := p.name(false)
		return err
	case 22, 23, 24, 25: // EDE-EXTRA-TEXT-LANGUAGE and FILTERING-* (2.9.1): UTF-8 text
		return utf8Decode(p.rest())
	default:
		p.rest()
	}
	return nil
}

// utf8Decode is bytes.decode("utf8"): nil, or CPython's UnicodeDecodeError
// for the first invalid sequence.
func utf8Decode(b []byte) error {
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		n, lo, hi := 0, byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			n = 2
		case c == 0xE0:
			n, lo = 3, 0xA0
		case c == 0xED:
			n, hi = 3, 0x9F
		case c >= 0xE1 && c <= 0xEF:
			n = 3
		case c == 0xF0:
			n, lo = 4, 0x90
		case c == 0xF4:
			n, hi = 4, 0x8F
		case c >= 0xF1 && c <= 0xF3:
			n = 4
		default:
			return decodeError(b, i, i+1, "invalid start byte")
		}
		for j := 1; j < n; j++ {
			if i+j >= len(b) {
				return decodeError(b, i, len(b), "unexpected end of data")
			}
			l, h := byte(0x80), byte(0xBF)
			if j == 1 {
				l, h = lo, hi
			}
			if b[i+j] < l || b[i+j] > h {
				return decodeError(b, i, i+j, "invalid continuation byte")
			}
		}
		i += n
	}
	return nil
}

func decodeError(b []byte, start, end int, reason string) error {
	msg := fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", b[start], start, reason)
	if end-start > 1 {
		msg = fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", start, end-1, reason)
	}
	return &Error{Class: "UnicodeDecodeError", Msg: msg, Python: true}
}
