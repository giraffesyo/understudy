package inventory

import (
	"fmt"
	"strconv"
	"strings"
)

// ExpandRange expands Ansible host ranges: web[01:20].example.com,
// db[a:f], node[1:20:2]. Zero-padding of the start token is preserved.
// A name with no range expands to itself.
func ExpandRange(name string) ([]string, error) {
	open := strings.IndexByte(name, '[')
	if open < 0 {
		return []string{name}, nil
	}
	closeIdx := strings.IndexByte(name[open:], ']')
	if closeIdx < 0 {
		return nil, fmt.Errorf("unbalanced '[' in host name %q", name)
	}
	closeIdx += open
	prefix, spec, suffix := name[:open], name[open+1:closeIdx], name[closeIdx+1:]

	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("invalid range %q in host name (expected [start:end] or [start:end:step])", spec)
	}
	step := 1
	if len(parts) == 3 {
		var err error
		step, err = strconv.Atoi(parts[2])
		if err != nil || step <= 0 {
			return nil, fmt.Errorf("invalid range step %q", parts[2])
		}
	}

	var expanded []string
	startS, endS := parts[0], parts[1]
	if isAlphaRange(startS, endS) {
		start, end := startS[0], endS[0]
		if start > end {
			return nil, fmt.Errorf("alphabetic range %q is reversed", spec)
		}
		for c := start; c <= end; c += byte(step) {
			expanded = append(expanded, prefix+string(c)+suffix)
		}
	} else {
		start, err := strconv.Atoi(startS)
		if err != nil {
			return nil, fmt.Errorf("invalid range start %q", startS)
		}
		end, err := strconv.Atoi(endS)
		if err != nil {
			return nil, fmt.Errorf("invalid range end %q", endS)
		}
		if start > end {
			return nil, fmt.Errorf("numeric range %q is reversed", spec)
		}
		width := 0
		if len(startS) > 1 && startS[0] == '0' {
			width = len(startS)
		}
		for i := start; i <= end; i += step {
			num := strconv.Itoa(i)
			for len(num) < width {
				num = "0" + num
			}
			expanded = append(expanded, prefix+num+suffix)
		}
	}

	// A suffix may contain another range (rare but legal).
	var out []string
	for _, e := range expanded {
		more, err := ExpandRange(e)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func isAlphaRange(start, end string) bool {
	return len(start) == 1 && len(end) == 1 &&
		isAlpha(start[0]) && isAlpha(end[0])
}

func isAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
