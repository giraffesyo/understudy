package pyre

import (
	"fmt"
	"strconv"
	"strings"
)

// TemplatePart is a literal (Group < 0) or a group reference.
type TemplatePart struct {
	Lit   string
	Group int
}

// ParseTemplate is _parser.parse_template: \1..\99, \g<n>, \g<name>,
// octal and character escapes, with Python's errors (*Error: re.error
// with its position, or IndexError for an unknown group name).
func ParseTemplate(p *Pattern, repl string) (parts []TemplatePart, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			parts, err = nil, e
		}
	}()
	s := newTokenizer([]rune(repl), p.bytes)
	var lit strings.Builder
	addLiteral := func() {
		parts = append(parts, TemplatePart{Lit: lit.String(), Group: -1})
		lit.Reset()
	}
	addGroup := func(index uint64, pos int) {
		if index > uint64(p.groups) {
			panic(s.error(fmt.Sprintf("invalid group reference %d", index), pos))
		}
		addLiteral()
		parts = append(parts, TemplatePart{Group: int(index)})
	}
	for {
		this, ok := s.get()
		if !ok {
			break
		}
		if this[0] != '\\' {
			lit.WriteString(this)
			continue
		}
		c := this[1]
		switch {
		case c == 'g':
			if !s.match("<") {
				panic(s.error("missing <", 0))
			}
			name := s.getuntil(">", "group name")
			if !isASCIIDecimal(name) {
				s.checkgroupname(name, 1)
				g, ok := p.index[name]
				if !ok {
					panic(&Error{Msg: "unknown group name " + pyRepr(name), Pos: -1, Exc: "IndexError"})
				}
				addGroup(uint64(g), len([]rune(name))+1)
			} else {
				v, err := strconv.ParseUint(name, 10, 64)
				if err != nil || v >= maxGroups {
					panic(s.error("invalid group reference "+trimZeros(name), len(name)+1))
				}
				addGroup(v, len(name)+1)
			}
		case c == '0':
			if s.nextIn(octdigits) {
				d, _ := s.get()
				this += d
				if s.nextIn(octdigits) {
					d, _ := s.get()
					this += d
				}
			}
			v, _ := strconv.ParseUint(this[1:], 8, 32)
			lit.WriteRune(rune(v & 0xff))
		case c >= '0' && c <= '9':
			isOctal := false
			if s.nextIn(digits) {
				d, _ := s.get()
				this += d
				if c <= '7' && this[2] <= '7' && s.nextIn(octdigits) {
					d, _ := s.get()
					this += d
					isOctal = true
					v, _ := strconv.ParseUint(this[1:], 8, 32)
					if v > 0o377 {
						panic(s.error(fmt.Sprintf("octal escape value %s outside of range 0-0o377", this), len(this)))
					}
					lit.WriteRune(rune(v))
				}
			}
			if !isOctal {
				v, _ := strconv.ParseUint(this[1:], 10, 64)
				addGroup(v, len(this)-1)
			}
		default:
			if r, ok := escapes[this]; ok {
				lit.WriteRune(r)
			} else {
				if strings.IndexByte(asciiLets, c) >= 0 {
					panic(s.error("bad escape "+this, len([]rune(this))))
				}
				lit.WriteString(this)
			}
		}
	}
	addLiteral()
	return parts, nil
}

func trimZeros(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" {
		return "0"
	}
	return t
}

// ExpandTemplate renders a parsed template for one match (byte spans as
// the matching functions return them; unmatched groups expand to "").
func ExpandTemplate(parts []TemplatePart, s string, m []int) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Group < 0 {
			b.WriteString(p.Lit)
			continue
		}
		if 2*p.Group+1 < len(m) && m[2*p.Group] >= 0 {
			b.WriteString(s[m[2*p.Group]:m[2*p.Group+1]])
		}
	}
	return b.String()
}
