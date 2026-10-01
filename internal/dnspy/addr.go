package dnspy

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// ipv4Aton is dns.ipv4.inet_aton: exactly four decimal parts, no leading
// zeros.
func ipv4Aton(text string) ([]byte, error) {
	parts := strings.Split(text, ".")
	if len(parts) != 4 {
		return nil, errSyntax
	}
	out := make([]byte, 4)
	for i, p := range parts {
		if !isDecimal(p) || (len(p) > 1 && p[0] == '0') {
			return nil, errSyntax
		}
		n, err := strconv.Atoi(p)
		if err != nil || n > 255 {
			return nil, errSyntax
		}
		out[i] = byte(n)
	}
	return out, nil
}

func ipv4Ntoa(b []byte) string {
	return strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2])) + "." + strconv.Itoa(int(b[3]))
}

var v4Ending = regexp.MustCompile(`^(.*):(\d+\.\d+\.\d+\.\d+)$`)

// ipv6Aton is dns.ipv6.inet_aton.
func ipv6Aton(text string, ignoreScope bool) ([]byte, error) {
	if ignoreScope {
		parts := strings.Split(text, "%")
		if len(parts) == 2 {
			text = parts[0]
		} else if len(parts) > 2 {
			return nil, errSyntax
		}
	}
	switch {
	case text == "":
		return nil, errSyntax
	case strings.HasSuffix(text, ":") && !strings.HasSuffix(text, "::"):
		return nil, errSyntax
	case strings.HasPrefix(text, ":") && !strings.HasPrefix(text, "::"):
		return nil, errSyntax
	case text == "::":
		text = "0::"
	}
	if m := v4Ending.FindStringSubmatch(text); m != nil {
		b, err := ipv4Aton(m[2])
		if err != nil {
			return nil, err
		}
		text = m[1] + ":" + hex.EncodeToString(b[:2]) + ":" + hex.EncodeToString(b[2:])
	}
	if strings.HasPrefix(text, "::") {
		text = text[1:]
	} else if strings.HasSuffix(text, "::") {
		text = text[:len(text)-1]
	}
	chunks := strings.Split(text, ":")
	if len(chunks) > 8 {
		return nil, errSyntax
	}
	seenEmpty := false
	var canonical strings.Builder
	for _, c := range chunks {
		if c == "" {
			if seenEmpty {
				return nil, errSyntax
			}
			seenEmpty = true
			for i := 0; i < 8-len(chunks)+1; i++ {
				canonical.WriteString("0000")
			}
			continue
		}
		if len(c) > 4 {
			return nil, errSyntax
		}
		canonical.WriteString(strings.Repeat("0", 4-len(c)) + c)
	}
	if len(chunks) < 8 && !seenEmpty {
		return nil, errSyntax
	}
	out, err := hex.DecodeString(canonical.String())
	if err != nil || len(out) != 16 {
		return nil, errSyntax
	}
	return out, nil
}

// ipv6Ntoa is dns.ipv6.inet_ntoa.
func ipv6Ntoa(b []byte) string {
	chunks := make([]string, 8)
	for i := range chunks {
		chunks[i] = strconv.FormatUint(uint64(b[2*i])<<8|uint64(b[2*i+1]), 16)
	}
	bestStart, bestLen, start, lastZero := 0, 0, -1, false
	for i := 0; i < 8; i++ {
		if chunks[i] != "0" {
			if lastZero {
				if cur := i - start; cur > bestLen {
					bestStart, bestLen = start, cur
				}
				lastZero = false
			}
		} else if !lastZero {
			start, lastZero = i, true
		}
	}
	if lastZero {
		if cur := 8 - start; cur > bestLen {
			bestStart, bestLen = start, cur
		}
	}
	if bestLen > 1 {
		if bestStart == 0 && (bestLen == 6 || bestLen == 5 && chunks[5] == "ffff") {
			prefix := "::"
			if bestLen == 5 {
				prefix = "::ffff:"
			}
			return prefix + ipv4Ntoa(b[12:])
		}
		return strings.Join(chunks[:bestStart], ":") + "::" + strings.Join(chunks[bestStart+bestLen:], ":")
	}
	return strings.Join(chunks, ":")
}

// IsAddress is dns.inet.is_address.
func IsAddress(text string) bool {
	if _, err := ipv4Aton(text); err == nil {
		return true
	}
	_, err := ipv6Aton(text, true)
	return err == nil
}

// SocketInetAton reports whether the C library's inet_aton accepts text
// (Python's socket.inet_aton): one to four parts, each decimal, octal
// (leading 0) or hex (0x), the last filling the remaining bytes.
func SocketInetAton(text string) bool {
	parts := strings.Split(text, ".")
	if len(parts) > 4 {
		return false
	}
	for i, p := range parts {
		if p == "" {
			return false
		}
		base := 10
		digits := p
		switch {
		case len(p) > 1 && (p[:2] == "0x" || p[:2] == "0X"):
			base, digits = 16, p[2:]
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		n, err := strconv.ParseUint(digits, base, 64)
		if digits == "" && base == 16 {
			n, err = 0, nil
		}
		if err != nil {
			return false
		}
		limit := uint64(255)
		if i == len(parts)-1 {
			limit = uint64(1)<<(8*(5-len(parts))) - 1
		}
		if n > limit {
			return false
		}
	}
	return true
}

var ipv4Reverse = Name{Labels: [][]byte{[]byte("in-addr"), []byte("arpa"), {}}}
var ipv6Reverse = Name{Labels: [][]byte{[]byte("ip6"), []byte("arpa"), {}}}

// ReverseName is dns.reversename.from_address.
func ReverseName(text string) (Name, error) {
	var parts []string
	origin := ipv4Reverse
	if v6, err := ipv6Aton(text, false); err == nil {
		if string(v6[:12]) == "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff\xff" {
			for _, b := range v6[12:] {
				parts = append(parts, strconv.Itoa(int(b)))
			}
		} else {
			for _, c := range hex.EncodeToString(v6) {
				parts = append(parts, string(c))
			}
			origin = ipv6Reverse
		}
	} else {
		v4, err := ipv4Aton(text)
		if err != nil {
			return Name{}, err
		}
		for _, b := range v4 {
			parts = append(parts, strconv.Itoa(int(b)))
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return ParseName(strings.Join(parts, "."), &origin)
}
