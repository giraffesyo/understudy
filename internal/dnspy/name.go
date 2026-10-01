// Package dnspy is a port of the parts of dnspython (2.8) the dig lookup
// stands on: domain names, the stub resolver (resolv.conf, search list,
// retries, UDP with TCP fallback, its errors) and the presentation (and
// attribute) form of the record types dnspython knows.
package dnspy

import (
	"bytes"
	"fmt"
	"strings"
)

// Name is a domain name: its labels, the last empty for an absolute name.
type Name struct {
	Labels [][]byte
}

// Root is the root name, ".".
var Root = Name{Labels: [][]byte{{}}}

// Absolute reports whether n ends in the root label.
func (n Name) Absolute() bool {
	return len(n.Labels) > 0 && len(n.Labels[len(n.Labels)-1]) == 0
}

// Len is len(name): its number of labels (the root label included).
func (n Name) Len() int { return len(n.Labels) }

// Concat is n + o.
func (n Name) Concat(o Name) (Name, error) {
	if n.Absolute() && o.Len() > 0 {
		return Name{}, &Error{Class: "AbsoluteConcatenation", Msg: "An attempt was made to append anything other than the empty name to an absolute DNS name."}
	}
	labels := append(append([][]byte{}, n.Labels...), o.Labels...)
	if err := validateLabels(labels); err != nil {
		return Name{}, err
	}
	return Name{Labels: labels}, nil
}

// Equal compares names as dnspython does: case-insensitively.
func (n Name) Equal(o Name) bool {
	if len(n.Labels) != len(o.Labels) {
		return false
	}
	for i := range n.Labels {
		if !bytes.EqualFold(n.Labels[i], o.Labels[i]) {
			return false
		}
	}
	return true
}

const nameEscaped = "\"().;\\@$"

// String is Name.to_text().
func (n Name) String() string {
	if len(n.Labels) == 0 {
		return "@"
	}
	if len(n.Labels) == 1 && len(n.Labels[0]) == 0 {
		return "."
	}
	parts := make([]string, len(n.Labels))
	for i, l := range n.Labels {
		var b strings.Builder
		for _, c := range l {
			switch {
			case strings.IndexByte(nameEscaped, c) >= 0:
				b.WriteByte('\\')
				b.WriteByte(c)
			case c > 0x20 && c < 0x7f:
				b.WriteByte(c)
			default:
				fmt.Fprintf(&b, "\\%03d", c)
			}
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, ".")
}

// validateLabels is dns.name._validate_labels.
func validateLabels(labels [][]byte) error {
	total, empty := 0, -1
	for j, l := range labels {
		total += len(l) + 1
		if len(l) > 63 {
			return errLabelTooLong
		}
		if empty < 0 && len(l) == 0 {
			empty = j
		}
	}
	if total > 255 {
		return errNameTooLong
	}
	if empty >= 0 && empty != len(labels)-1 {
		return errEmptyLabel
	}
	return nil
}

var (
	errEmptyLabel   = &Error{Class: "EmptyLabel", Msg: "A DNS label is empty.", Syntax: true}
	errBadEscape    = &Error{Class: "BadEscape", Msg: "An escaped code in a text format of DNS name is invalid.", Syntax: true}
	errLabelTooLong = &Error{Class: "LabelTooLong", Msg: "A DNS label is > 63 octets long.", Syntax: true}
	errNameTooLong  = &Error{Class: "NameTooLong", Msg: "A DNS name is > 255 octets long.", Form: true}
	errBadPointer   = &Error{Class: "BadPointer", Msg: "A DNS compression pointer points forward instead of backward.", Form: true}
	errBadLabelType = &Error{Class: "BadLabelType", Msg: "The label type in DNS name wire format is unknown.", Form: true}
)

// ParseName is dns.name.from_text(text, origin): a name without a final
// dot gets origin's labels (none for a nil origin: a relative name).
func ParseName(text string, origin *Name) (Name, error) {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return parseUnicodeName(text, origin)
		}
	}
	if text == "@" {
		text = ""
	}
	var labels [][]byte
	if text != "" {
		if text == "." {
			return Name{Labels: [][]byte{{}}}, nil
		}
		var label []byte
		escaping, edigits, total := false, 0, 0
		for i := 0; i < len(text); i++ {
			c := text[i]
			switch {
			case escaping:
				if edigits == 0 {
					if isDigit(c) {
						total = int(c - '0')
						edigits++
					} else {
						label = append(label, c)
						escaping = false
					}
				} else {
					if !isDigit(c) {
						return Name{}, errBadEscape
					}
					total = total*10 + int(c-'0')
					edigits++
					if edigits == 3 {
						escaping = false
						if total > 255 {
							return Name{}, &Error{Class: "error", Msg: "ubyte format requires 0 <= number <= 255", Python: true}
						}
						label = append(label, byte(total))
					}
				}
			case c == '.':
				if len(label) == 0 {
					return Name{}, errEmptyLabel
				}
				labels = append(labels, label)
				label = nil
			case c == '\\':
				escaping, edigits, total = true, 0, 0
			default:
				label = append(label, c)
			}
		}
		if escaping {
			return Name{}, errBadEscape
		}
		if len(label) > 0 {
			labels = append(labels, label)
		} else {
			labels = append(labels, []byte{})
		}
	}
	if (len(labels) == 0 || len(labels[len(labels)-1]) != 0) && origin != nil {
		labels = append(labels, origin.Labels...)
	}
	if err := validateLabels(labels); err != nil {
		return Name{}, err
	}
	return Name{Labels: labels}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseUnicodeName is dns.name.from_unicode with the IDNA 2008 codec
// (dnspython's default with the idna package installed), for the
// common case: labels lowercased and punycoded, non-ASCII ones prefixed
// "xn--".
func parseUnicodeName(text string, origin *Name) (Name, error) {
	var labels [][]byte
	if text == "." {
		return Name{Labels: [][]byte{{}}}, nil
	}
	// The ideographic and fullwidth full stops separate labels too.
	for _, dot := range []string{"。", "．", "｡"} {
		text = strings.ReplaceAll(text, dot, ".")
	}
	var label []rune
	escaping, edigits, total := false, 0, 0
	flush := func() error {
		if len(label) == 0 {
			return errEmptyLabel
		}
		enc, err := idnaEncodeLabel(string(label))
		if err != nil {
			return err
		}
		labels = append(labels, enc)
		label = nil
		return nil
	}
	for _, c := range text {
		switch {
		case escaping:
			if edigits == 0 {
				if c >= '0' && c <= '9' {
					total = int(c - '0')
					edigits++
				} else {
					label = append(label, c)
					escaping = false
				}
			} else {
				if c < '0' || c > '9' {
					return Name{}, errBadEscape
				}
				total = total*10 + int(c-'0')
				edigits++
				if edigits == 3 {
					escaping = false
					label = append(label, rune(total))
				}
			}
		case c == '.':
			if err := flush(); err != nil {
				return Name{}, err
			}
		case c == '\\':
			escaping, edigits, total = true, 0, 0
		default:
			label = append(label, c)
		}
	}
	if escaping {
		return Name{}, errBadEscape
	}
	if len(label) > 0 {
		if err := flush(); err != nil {
			return Name{}, err
		}
	} else {
		labels = append(labels, []byte{})
	}
	if (len(labels) == 0 || len(labels[len(labels)-1]) != 0) && origin != nil {
		labels = append(labels, origin.Labels...)
	}
	if err := validateLabels(labels); err != nil {
		return Name{}, err
	}
	return Name{Labels: labels}, nil
}

// idnaEncodeLabel is a label's ASCII form: lowercased, and punycode with
// the "xn--" prefix when it has non-ASCII characters.
func idnaEncodeLabel(label string) ([]byte, error) {
	label = strings.ToLower(label)
	ascii := true
	for i := 0; i < len(label); i++ {
		if label[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return []byte(label), nil
	}
	enc, ok := punycodeEncode(label)
	if !ok {
		return nil, &Error{Class: "IDNAException", Msg: "IDNA processing raised an exception."}
	}
	return []byte("xn--" + enc), nil
}

// punycodeEncode is RFC 3492's encoding.
func punycodeEncode(s string) (string, bool) {
	const (
		base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
	)
	runes := []rune(s)
	var out []byte
	for _, r := range runes {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tmin)*tmax)/2 {
			delta /= base - tmin
			k += base
		}
		return k + (base-tmin+1)*delta/(delta+skew)
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(runes) {
		m := int(^uint(0) >> 1)
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := base; ; k += base {
					t := k - bias
					if t < tmin {
						t = tmin
					} else if t > tmax {
						t = tmax
					}
					if q < t {
						break
					}
					out = append(out, digit(t+(q-t)%(base-t)))
					q = (q - t) / (base - t)
				}
				out = append(out, digit(q))
				bias = adapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out), true
}
