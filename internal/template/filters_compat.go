package template

import (
	"fmt"
	"strconv"
	"strings"
)

// registerCompatFilters installs filters found missing by argscan across
// widely used roles: comment (ansible_managed headers), urlsplit and
// shuffle.
func registerCompatFilters(e *Engine) {
	e.Filters["comment"] = filterComment
	e.Filters["urlsplit"] = filterURLSplit
	e.Filters["shuffle"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// randomize_list: a seed makes the order deterministic within
		// understudy (it does not reproduce Python's PRNG sequence).
		items, err := iterate(in)
		if err != nil {
			return in, nil // Ansible swallows the error and returns the input
		}
		out := append([]any(nil), items...)
		seed := kwargs["seed"]
		if seed == nil && len(args) > 0 {
			seed = args[0]
		}
		rng := newRand(seed)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out, nil
	}
}

var commentStyles = map[string]map[string]string{
	"plain":  {"decoration": "# "},
	"erlang": {"decoration": "% "},
	"c":      {"decoration": "// "},
	"cblock": {"beginning": "/*", "decoration": " * ", "end": " */"},
	"xml":    {"beginning": "<!--", "decoration": " - ", "end": "-->"},
}

// filterComment is ansible-core's comment filter.
func filterComment(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	text := toStr(in)
	style := "plain"
	if len(args) > 0 {
		style = toStr(args[0])
	}
	if s, ok := kwargs["style"]; ok {
		style = toStr(s)
	}
	params, ok := commentStyles[style]
	if !ok {
		return nil, fmt.Errorf("Invalid style %s. Available styles: plain, erlang, c, cblock, xml", pyRepr(style))
	}
	prepostfix := params["decoration"]
	if d, ok := kwargs["decoration"]; ok {
		prepostfix = toStr(d)
	}
	p := map[string]any{
		"newline":       "\n",
		"beginning":     "",
		"prefix":        strings.TrimRight(prepostfix, " \t\n\r\f\v"),
		"prefix_count":  int64(1),
		"decoration":    "",
		"postfix":       strings.TrimRight(prepostfix, " \t\n\r\f\v"),
		"postfix_count": int64(1),
		"end":           "",
	}
	for k, v := range params {
		p[k] = v
	}
	for k, v := range kwargs {
		if k != "style" {
			p[k] = v
		}
	}
	str := func(k string) string { return toStr(p[k]) }
	count := func(k string) (int, error) {
		switch t := p[k].(type) {
		case int64:
			return int(t), nil
		case float64:
			return int(t), nil
		case bool:
			if t {
				return 1, nil
			}
			return 0, nil
		default:
			n, err := strconv.Atoi(strings.TrimSpace(toStr(t)))
			if err != nil {
				return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(toStr(t)))
			}
			return n, nil
		}
	}
	nl, deco := str("newline"), str("decoration")
	var b strings.Builder
	if s := str("beginning"); s != "" {
		b.WriteString(s + nl)
	}
	if pre := str("prefix"); pre != "" {
		n, err := count("prefix_count")
		if err != nil {
			return nil, err
		}
		unit := pre + nl
		if pre == nl {
			unit = nl
		}
		if n > 0 {
			b.WriteString(strings.Repeat(unit, n))
		}
	}
	body := deco + pyReplace(text, nl, nl+deco)
	body = pyReplace(body, deco+nl, strings.TrimRight(deco, " \t\n\r\f\v")+nl)
	b.WriteString(body)
	n, err := count("postfix_count")
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		b.WriteString(nl + str("postfix"))
	}
	if end := str("end"); end != "" {
		b.WriteString(nl + end)
	}
	return b.String(), nil
}

// pyReplace is str.replace (an empty old string inserts between chars).
func pyReplace(s, old, repl string) string {
	return strings.ReplaceAll(s, old, repl)
}

// filterURLSplit is the urlsplit filter over Python's urllib.parse.urlsplit.
func filterURLSplit(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	parts, err := pyURLSplit(toStr(in))
	if err != nil {
		return nil, err
	}
	query := ""
	if len(args) > 0 {
		query = toStr(args[0])
	}
	if q, ok := kwargs["query"]; ok {
		query = toStr(q)
	}
	if query == "" {
		return parts, nil
	}
	v, ok := parts[query]
	if !ok {
		return nil, fmt.Errorf("urlsplit: unknown URL component: %s", query)
	}
	return v, nil
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// pyURLSplit mirrors urllib.parse.urlsplit plus the SplitResult
// properties (hostname, port, username, password).
func pyURLSplit(url string) (map[string]any, error) {
	// Python strips leading C0 controls/space and removes tab/CR/LF.
	url = strings.TrimLeft(url, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	scheme, netloc, query, fragment := "", "", "", ""
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIIAlpha(url[0]) {
		ok := true
		for _, c := range url[:i] {
			if !strings.ContainsRune(schemeChars, c) {
				ok = false
				break
			}
		}
		if ok {
			scheme = strings.ToLower(url[:i])
			url = url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		rest := url[2:]
		end := len(rest)
		for _, d := range "/?#" {
			if j := strings.IndexRune(rest, d); j >= 0 && j < end {
				end = j
			}
		}
		netloc, url = rest[:end], rest[end:]
		if (strings.Contains(netloc, "[") && !strings.Contains(netloc, "]")) ||
			(strings.Contains(netloc, "]") && !strings.Contains(netloc, "[")) {
			return nil, fmt.Errorf("Invalid IPv6 URL")
		}
	}
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url, fragment = url[:i], url[i+1:]
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url, query = url[:i], url[i+1:]
	}
	out := map[string]any{
		"scheme": scheme, "netloc": netloc, "path": url, "query": query, "fragment": fragment,
		"username": nil, "password": nil, "hostname": nil, "port": nil,
	}
	userinfo, hostport := "", netloc
	if i := strings.LastIndexByte(netloc, '@'); i >= 0 {
		userinfo, hostport = netloc[:i], netloc[i+1:]
		user, pw, hasPw := strings.Cut(userinfo, ":")
		out["username"] = user
		if hasPw {
			out["password"] = pw
		}
	}
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		if j := strings.IndexByte(hostport, ']'); j >= 0 {
			host = hostport[1:j]
			if rest := hostport[j+1:]; strings.HasPrefix(rest, ":") {
				port = rest[1:]
			}
		}
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, port = hostport[:i], hostport[i+1:]
	}
	if host != "" {
		if i := strings.IndexByte(host, '%'); i >= 0 { // IPv6 zone keeps its case
			out["hostname"] = strings.ToLower(host[:i]) + host[i:]
		} else {
			out["hostname"] = strings.ToLower(host)
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || !isASCIIDigits(port) {
			return nil, fmt.Errorf("Port could not be cast to integer value as %s", pyRepr(port))
		}
		if n < 0 || n > 65535 {
			return nil, fmt.Errorf("Port out of range 0-65535")
		}
		out["port"] = int64(n)
	}
	return out, nil
}

func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
