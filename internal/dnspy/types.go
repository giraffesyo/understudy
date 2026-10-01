package dnspy

import (
	"strconv"
	"strings"
)

// Record types and classes, as dns.rdatatype and dns.rdataclass name
// them.
const (
	TypeA          = 1
	TypeNS         = 2
	TypeCNAME      = 5
	TypeSOA        = 6
	TypePTR        = 12
	TypeHINFO      = 13
	TypeMX         = 15
	TypeTXT        = 16
	TypeRP         = 17
	TypeAAAA       = 28
	TypeLOC        = 29
	TypeSRV        = 33
	TypeNAPTR      = 35
	TypeDNAME      = 39
	TypeOPT        = 41
	TypeDS         = 43
	TypeSSHFP      = 44
	TypeRRSIG      = 46
	TypeDNSKEY     = 48
	TypeNSEC3PARAM = 51
	TypeTLSA       = 52
	TypeSPF        = 99
	TypeANY        = 255
	TypeCAA        = 257

	ClassIN = 1
	ClassCH = 3
)

// typeNames is RdataType's members in definition order (the first name
// of a value is its text).
var typeNames = []struct {
	name  string
	value uint16
}{
	{"TYPE0", 0}, {"NONE", 0}, {"A", 1}, {"NS", 2}, {"MD", 3}, {"MF", 4}, {"CNAME", 5},
	{"SOA", 6}, {"MB", 7}, {"MG", 8}, {"MR", 9}, {"NULL", 10}, {"WKS", 11}, {"PTR", 12},
	{"HINFO", 13}, {"MINFO", 14}, {"MX", 15}, {"TXT", 16}, {"RP", 17}, {"AFSDB", 18},
	{"X25", 19}, {"ISDN", 20}, {"RT", 21}, {"NSAP", 22}, {"NSAP_PTR", 23}, {"SIG", 24},
	{"KEY", 25}, {"PX", 26}, {"GPOS", 27}, {"AAAA", 28}, {"LOC", 29}, {"NXT", 30},
	{"SRV", 33}, {"NAPTR", 35}, {"KX", 36}, {"CERT", 37}, {"A6", 38}, {"DNAME", 39},
	{"OPT", 41}, {"APL", 42}, {"DS", 43}, {"SSHFP", 44}, {"IPSECKEY", 45}, {"RRSIG", 46},
	{"NSEC", 47}, {"DNSKEY", 48}, {"DHCID", 49}, {"NSEC3", 50}, {"NSEC3PARAM", 51},
	{"TLSA", 52}, {"SMIMEA", 53}, {"HIP", 55}, {"NINFO", 56}, {"CDS", 59}, {"CDNSKEY", 60},
	{"OPENPGPKEY", 61}, {"CSYNC", 62}, {"ZONEMD", 63}, {"SVCB", 64}, {"HTTPS", 65},
	{"DSYNC", 66}, {"SPF", 99}, {"UNSPEC", 103}, {"NID", 104}, {"L32", 105}, {"L64", 106},
	{"LP", 107}, {"EUI48", 108}, {"EUI64", 109}, {"TKEY", 249}, {"TSIG", 250},
	{"IXFR", 251}, {"AXFR", 252}, {"MAILB", 253}, {"MAILA", 254}, {"ANY", 255},
	{"URI", 256}, {"CAA", 257}, {"AVC", 258}, {"AMTRELAY", 260}, {"RESINFO", 261},
	{"WALLET", 262}, {"TA", 32768}, {"DLV", 32769},
}

var classNames = []struct {
	name  string
	value uint16
}{
	{"RESERVED0", 0}, {"IN", 1}, {"INTERNET", 1}, {"CH", 3}, {"CHAOS", 3}, {"HS", 4},
	{"HESIOD", 4}, {"NONE", 254}, {"ANY", 255},
}

// enumFromText is dns.enum.IntEnum.from_text: a member name (upper
// cased), or prefix+number. bad is the class's unknown exception.
func enumFromText(text, prefix, short string, max int, lookup func(string) (uint16, bool), bad *Error) (uint16, error) {
	text = strings.ToUpper(text)
	if v, ok := lookup(text); ok {
		return v, nil
	}
	if strings.HasPrefix(text, prefix) && isDecimal(text[len(prefix):]) {
		digits := text[len(prefix):]
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil || n > uint64(max) {
			return 0, valueError("%s must be an int between >= 0 and <= %d", short, max)
		}
		return uint16(n), nil
	}
	return 0, bad
}

// isDecimal is str.isdigit for ASCII text (non-empty, all digits).
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// TypeFromText is dns.rdatatype.from_text.
func TypeFromText(text string) (uint16, error) {
	return enumFromText(text, "TYPE", "type", 65535, func(s string) (uint16, bool) {
		for _, t := range typeNames {
			if t.name == s {
				return t.value, true
			}
		}
		if strings.Contains(s, "-") {
			s = strings.ReplaceAll(s, "-", "_")
			for _, t := range typeNames {
				if t.name == s {
					return t.value, true
				}
			}
		}
		return 0, false
	}, errUnknownType)
}

// TypeText is dns.rdatatype.to_text.
func TypeText(v uint16) string {
	for _, t := range typeNames {
		if t.value == v {
			return strings.ReplaceAll(t.name, "_", "-")
		}
	}
	return "TYPE" + strconv.Itoa(int(v))
}

// ClassFromText is dns.rdataclass.from_text.
func ClassFromText(text string) (uint16, error) {
	return enumFromText(text, "CLASS", "class", 65535, func(s string) (uint16, bool) {
		for _, c := range classNames {
			if c.name == s {
				return c.value, true
			}
		}
		return 0, false
	}, errUnknownCls)
}

// ClassText is dns.rdataclass.to_text.
func ClassText(v uint16) string {
	for _, c := range classNames {
		if c.value == v {
			return c.name
		}
	}
	return "CLASS" + strconv.Itoa(int(v))
}

// isMetatype is dns.rdatatype.is_metatype.
func isMetatype(t uint16) bool { return (t >= 128 && t < 256) || t == TypeOPT }

// isMetaclass is dns.rdataclass.is_metaclass.
func isMetaclass(c uint16) bool { return c == 254 || c == 255 }

// algorithmNames is dns.dnssectypes.Algorithm.
var algorithmNames = map[int]string{
	1: "RSAMD5", 2: "DH", 3: "DSA", 4: "ECC", 5: "RSASHA1", 6: "DSANSEC3SHA1",
	7: "RSASHA1NSEC3SHA1", 8: "RSASHA256", 10: "RSASHA512", 12: "ECCGOST",
	13: "ECDSAP256SHA256", 14: "ECDSAP384SHA384", 15: "ED25519", 16: "ED448",
	252: "INDIRECT", 253: "PRIVATEDNS", 254: "PRIVATEOID",
}

func algorithmText(v int) string {
	if s, ok := algorithmNames[v]; ok {
		return s
	}
	return strconv.Itoa(v)
}
