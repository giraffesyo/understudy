package executor

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/dnspy"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// community.general.dig, on dnspy (a port of the dnspython resolver and
// record presentation the plugin is built on).

const digPluginLabel = "'ansible_collections.community.general.plugins.lookup.dig' lookup plugin"

// digOptions are the plugin's options in its DOCUMENTATION order, the
// order set_options checks them in.
var digOptions = []struct {
	name, typ string
	def       any
	choices   []string
}{
	{"qtype", "str", "A", []string{"A", "ALL", "AAAA", "CAA", "CNAME", "DNAME", "DNSKEY", "DS", "HINFO", "LOC", "MX", "NAPTR", "NS", "NSEC3PARAM", "PTR", "RP", "RRSIG", "SOA", "SPF", "SRV", "SSHFP", "TLSA", "TXT"}},
	{"flat", "int", 1, nil},
	{"retry_servfail", "bool", false, nil},
	{"fail_on_error", "bool", false, nil},
	{"real_empty", "bool", false, nil},
	{"class", "str", "IN", nil},
	{"tcp", "bool", false, nil},
	{"port", "int", 53, nil},
}

// digRdataDicts is make_rdata_dict's supported_types: the attributes of
// each record type a flat=0 result holds.
var digRdataDicts = map[uint16][]string{
	dnspy.TypeA:          {"address"},
	dnspy.TypeAAAA:       {"address"},
	dnspy.TypeCAA:        {"flags", "tag", "value"},
	dnspy.TypeCNAME:      {"target"},
	dnspy.TypeDNAME:      {"target"},
	dnspy.TypeDNSKEY:     {"flags", "algorithm", "protocol", "key"},
	dnspy.TypeDS:         {"algorithm", "digest_type", "key_tag", "digest"},
	dnspy.TypeHINFO:      {"cpu", "os"},
	dnspy.TypeLOC:        {"latitude", "longitude", "altitude", "size", "horizontal_precision", "vertical_precision"},
	dnspy.TypeMX:         {"preference", "exchange"},
	dnspy.TypeNAPTR:      {"order", "preference", "flags", "service", "regexp", "replacement"},
	dnspy.TypeNS:         {"target"},
	dnspy.TypeNSEC3PARAM: {"algorithm", "flags", "iterations", "salt"},
	dnspy.TypePTR:        {"target"},
	dnspy.TypeRP:         {"mbox", "txt"},
	dnspy.TypeSOA:        {"mname", "rname", "serial", "refresh", "retry", "expire", "minimum"},
	dnspy.TypeSPF:        {"strings"},
	dnspy.TypeSRV:        {"priority", "weight", "port", "target"},
	dnspy.TypeSSHFP:      {"algorithm", "fp_type", "fingerprint"},
	dnspy.TypeTLSA:       {"usage", "selector", "mtype", "cert"},
	dnspy.TypeTXT:        {"strings"},
}

// resolvConf is the resolver configuration dnspython reads.
var resolvConf = "/etc/resolv.conf"

func digErr(format string, args ...any) error {
	return &template.LookupError{Msg: fmt.Sprintf(format, args...)}
}

// digOption is ConfigManager's checks of one direct option: ensure_type,
// then its choices.
func digOption(name, typ string, v any, choices []string) (any, error) {
	invalid := func() error {
		return digErr("Config '%s' for %s from 'Direct' has an invalid value: Invalid value provided for '%s': %s",
			name, digPluginLabel, typ, template.PyRepr(v))
	}
	var out any
	switch typ {
	case "bool":
		out = pyBooleanLoose(v)
	case "int":
		n, ok := ensureInt(v)
		if !ok {
			return nil, invalid()
		}
		out = n
	case "str":
		switch t := v.(type) {
		case nil:
			return nil, nil
		case bool, int, int64, float64, *big.Int:
			out = template.PyStr(t)
		default:
			s, ok := lookupString(v)
			if !ok {
				return nil, invalid()
			}
			out = s
		}
	}
	if choices != nil {
		s, _ := out.(string)
		found := false
		for _, c := range choices {
			if c == s {
				found = true
			}
		}
		if !found {
			msg := fmt.Sprintf("Invalid value %s for config '%s' for %s.", template.PyRepr(out), name, digPluginLabel)
			return nil, &template.LookupError{Msg: msg, Help: "Valid values are: " + strings.Join(choices, ", ")}
		}
	}
	return out, nil
}

// lookupString is a string-like template value's text.
func lookupString(v any) (string, bool) {
	switch t := template.Undeprecate(v).(type) {
	case string:
		return t, true
	case yaml.UnsafeString:
		return string(t), true
	case template.Markup:
		return string(t), true
	}
	return "", false
}

// pyBooleanLoose is boolean(v, strict=False).
func pyBooleanLoose(v any) bool {
	switch t := template.Undeprecate(v).(type) {
	case bool:
		return t
	case int:
		return t == 1
	case int64:
		return t == 1
	case float64:
		return t == 1
	}
	if s, ok := lookupString(v); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "y", "yes", "on", "1", "true", "t":
			return true
		}
	}
	return false
}

var pyDecimalLit = regexp.MustCompile(`^[+-]?(\d(_?\d)*(\.(\d(_?\d)*)?)?|\.\d(_?\d)*)([eE][+-]?\d(_?\d)*)?$`)

// ensureInt is ensure_type(v, 'int'): an int (or bool), or a float or
// str whose Decimal value is integral.
func ensureInt(v any) (int, bool) {
	switch t := template.Undeprecate(v).(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if t == float64(int64(t)) {
			return int(t), true
		}
		return 0, false
	}
	s, ok := lookupString(v)
	if !ok {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if !pyDecimalLit.MatchString(s) {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(s, "_", ""))
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return int(r.Num().Int64()), true
}

var pyIntLit = regexp.MustCompile(`^[+-]?\d(_?\d)*$`)

// pyIntText is int(s).
func pyIntText(s string) (int, error) {
	t := strings.TrimSpace(s)
	if !pyIntLit.MatchString(t) {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", template.PyRepr(s))
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(t, "_", ""), 10)
	if !ok || !n.IsInt64() {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", template.PyRepr(s))
	}
	return int(n.Int64()), nil
}

// pyBooleanStrict is boolean(v) (strict): the term options' parser.
func pyBooleanStrict(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "on", "1", "true", "t":
		return true, nil
	case "n", "no", "off", "0", "false", "f":
		return false, nil
	}
	return false, fmt.Errorf("The value '%s' is not a valid boolean. Valid booleans include: 'y', 'yes', 'on', '1', 'true', 't', 1, 1.0, True, 'n', 'no', 'off', '0', 'false', 'f', 0, 0.0, False", s)
}

func lookupDig(_ *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	opts := map[string]any{}
	for _, o := range digOptions {
		v, ok := kw[o.name]
		if !ok {
			opts[o.name] = o.def
			continue
		}
		val, err := digOption(o.name, o.typ, v, o.choices)
		if err != nil {
			return nil, err
		}
		opts[o.name] = val
	}
	if _, ok := kw["_terms"]; ok {
		return nil, digErr("The '_terms' keyword argument is not supported, you must provide terms as positional arguments: use lookup('community.general.dig', arg1, arg2) instead of lookup('community.general.dig', _terms=[arg1, arg2])")
	}

	res, err := dnspy.NewResolver(resolvConf)
	if err != nil {
		return nil, &template.LookupError{Msg: err.Error(), Separate: dnspy.Is(err, "ValueError")}
	}
	res.EDNS, res.Payload, res.EDNSFlags = true, 4096, 0x8000

	var domains, nameservers []string
	qtype, _ := opts["qtype"].(string)
	flat, _ := opts["flat"].(int)
	failOnError, _ := opts["fail_on_error"].(bool)
	realEmpty, _ := opts["real_empty"].(bool)
	tcp, _ := opts["tcp"].(bool)
	port, _ := opts["port"].(int)
	className, _ := opts["class"].(string)
	rdclass, err := dnspy.ClassFromText(className)
	if err != nil {
		return nil, digErr("dns lookup illegal CLASS: %s", err)
	}
	res.RetryServfail, _ = opts["retry_servfail"].(bool)

	for _, term := range terms {
		t, ok := lookupString(term)
		if !ok {
			return nil, digErr("'%s' object has no attribute 'startswith'", template.NativeTypeName(term))
		}
		if strings.HasPrefix(t, "@") {
			for _, ns := range strings.Split(t[1:], ",") {
				if dnspy.SocketInetAton(ns) {
					nameservers = append(nameservers, ns)
					continue
				}
				addr, err := digSystemAddress(ns)
				if err != nil {
					return nil, digErr("dns lookup NS: %s", err)
				}
				nameservers = append(nameservers, addr)
			}
			continue
		}
		if opt, arg, ok := strings.Cut(t, "="); ok {
			switch opt {
			case "qtype":
				qtype = strings.ToUpper(arg)
			case "flat":
				if flat, err = pyIntText(arg); err != nil {
					return nil, digErr("%s", err)
				}
			case "class":
				if rdclass, err = dnspy.ClassFromText(arg); err != nil {
					return nil, digErr("dns lookup illegal CLASS: %s", err)
				}
			case "retry_servfail", "fail_on_error", "real_empty", "tcp":
				b, err := pyBooleanStrict(arg)
				if err != nil {
					return nil, digErr("%s", err)
				}
				switch opt {
				case "retry_servfail":
					res.RetryServfail = b
				case "fail_on_error":
					failOnError = b
				case "real_empty":
					realEmpty = b
				case "tcp":
					tcp = b
				}
			}
			continue
		}
		if strings.Contains(t, "/") {
			if parts := strings.Split(t, "/"); len(parts) == 2 {
				domains = append(domains, parts[0])
				qtype = parts[1]
				continue
			}
		}
		domains = append(domains, t)
	}

	if port != 0 {
		res.Port = port
	}
	if len(nameservers) > 0 {
		if err := res.SetNameservers(nameservers); err != nil {
			// Raised while handling the address's NotImplementedError.
			return nil, &template.LookupError{Msg: err.Error(), Separate: true}
		}
	}
	if strings.ToUpper(qtype) == "PTR" {
		var reversed []string
		for _, d := range domains {
			n, err := dnspy.ReverseName(d)
			if err != nil {
				continue
			}
			reversed = append(reversed, n.String())
		}
		domains = reversed
	}
	if len(domains) > 1 {
		realEmpty = true
	}

	out := []any{}
	for _, domain := range domains {
		answer, err := res.Query(domain, qtype, rdclass, tcp)
		if err != nil {
			switch {
			case dnspy.Is(err, "NXDOMAIN"):
				if failOnError {
					return nil, digErr("Lookup failed: %s", err)
				}
				if !realEmpty {
					out = append(out, "NXDOMAIN")
				}
			case dnspy.Is(err, "NoAnswer"), dnspy.Is(err, "LifetimeTimeout"), dnspy.Is(err, "NoNameservers"):
				if failOnError {
					return nil, digErr("Lookup failed: %s", err)
				}
				if !realEmpty {
					out = append(out, "")
				}
			case dnspy.IsDNSException(err):
				return nil, digErr("dns.resolver unhandled exception %s", err)
			default:
				return nil, digErr("%s", err)
			}
			continue
		}
		for _, rd := range answer.RRset.Rdatas {
			s, err := rd.Text()
			if err != nil {
				return nil, digErr("%s", err)
			}
			if strings.ToUpper(qtype) == "TXT" && len(s) >= 2 {
				s = s[1 : len(s)-1]
			} else if strings.ToUpper(qtype) == "TXT" {
				s = ""
			}
			if flat != 0 {
				out = append(out, s)
				continue
			}
			d, err := digRdataDict(rd)
			if err != nil {
				if failOnError {
					return nil, digErr("Lookup failed: %s", err)
				}
				out = append(out, err.Error())
				continue
			}
			d.Set("owner", answer.CanonicalName.String())
			d.Set("type", dnspy.TypeText(rd.Type))
			d.Set("ttl", int(answer.RRset.TTL))
			d.Set("class", dnspy.ClassText(rd.Class))
			out = append(out, d)
		}
	}
	return out, nil
}

// digSystemAddress is dns.resolver.query(name)[0].address: name's first
// A record from the system's resolver configuration.
func digSystemAddress(name string) (string, error) {
	res, err := dnspy.NewResolver(resolvConf)
	if err != nil {
		return "", err
	}
	answer, err := res.Query(name, "A", dnspy.ClassIN, false)
	if err != nil {
		return "", err
	}
	if len(answer.RRset.Rdatas) == 0 {
		return "", fmt.Errorf("list index out of range")
	}
	v, err := answer.RRset.Rdatas[0].Attr("address")
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("list index out of range")
	}
	return s, nil
}

// digRdataDict is make_rdata_dict.
func digRdataDict(rd *dnspy.Rdata) (*yaml.OMap, error) {
	d := yaml.NewOMap()
	for _, f := range digRdataDicts[rd.Type] {
		v, err := rd.Attr(f)
		if err != nil {
			return nil, err
		}
		switch {
		case rd.Type == dnspy.TypeDS && f == "digest",
			rd.Type == dnspy.TypeNSEC3PARAM && f == "salt",
			rd.Type == dnspy.TypeSSHFP && f == "fingerprint",
			rd.Type == dnspy.TypeTLSA && f == "cert":
			v = fmt.Sprintf("%x", []byte(v.(dnspy.Bytes)))
		case rd.Type == dnspy.TypeDNSKEY && f == "algorithm":
			v = int(v.(dnspy.Enum).Value)
		case rd.Type == dnspy.TypeDNSKEY && f == "key":
			v = base64Std([]byte(v.(dnspy.Bytes)))
		}
		d.Set(f, digValue(v))
	}
	return d, nil
}

// digValue is a dnspython attribute as variable storage holds it: bytes
// as str (undecodable bytes kept, as surrogateescape keeps them), tuples
// as lists, enums as IntEnum (converted to int, with a warning, when
// stored).
func digValue(v any) any {
	switch t := v.(type) {
	case dnspy.Bytes:
		return string(t)
	case dnspy.Enum:
		return template.IntEnum{Class: t.Class, Value: t.Value}
	case int64:
		return int(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = digValue(x)
		}
		return out
	}
	return v
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
