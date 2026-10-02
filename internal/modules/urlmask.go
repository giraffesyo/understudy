package modules

import "strings"

// MaskURL is ansible-core's module_utils.urls.mask_url (2.21.4+): a URL
// with credentials in its netloc comes back with them replaced by "****"
// (re-assembled by urlunparse, so the scheme is lowercased and an empty
// query or fragment dropped); any other URL is returned unchanged.
func MaskURL(raw string) string {
	scheme, netloc, path, query, fragment, ok := pyURLSplit(raw)
	if !ok {
		// urlparse raises ValueError (an unbalanced IPv6 bracket).
		return raw
	}
	userinfo, at, _ := reverseCutAt(netloc)
	if !at {
		return raw
	}
	username, password, hasPassword := strings.Cut(userinfo, ":")
	if username == "" && (!hasPassword || password == "") {
		return raw
	}
	const mask = "****"
	if hasPassword && password != "" {
		netloc = strings.ReplaceAll(netloc, username+":"+password+"@", mask+":"+mask+"@")
	} else {
		netloc = strings.ReplaceAll(netloc, username+"@", mask+"@")
	}
	// urlunsplit (urlparse's params stay part of the path).
	out := "//" + netloc
	if path != "" && path[0] != '/' {
		out += "/"
	}
	out += path
	if scheme != "" {
		out = scheme + ":" + out
	}
	if query != "" {
		out += "?" + query
	}
	if fragment != "" {
		out += "#" + fragment
	}
	return out
}

// reverseCutAt splits netloc at its last '@' (str.rpartition) into the
// userinfo, whether there was one, and the host part.
func reverseCutAt(netloc string) (string, bool, string) {
	i := strings.LastIndexByte(netloc, '@')
	if i < 0 {
		return "", false, netloc
	}
	return netloc[:i], true, netloc[i+1:]
}

// pyURLSplit is urllib.parse.urlsplit's five components; ok is false where
// urlsplit raises (an unbalanced IPv6 bracket in the netloc).
func pyURLSplit(raw string) (scheme, netloc, path, query, fragment string, ok bool) {
	// urlsplit strips leading C0 controls and spaces, and removes tab/CR/LF.
	raw = strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	if i := strings.IndexByte(raw, ':'); i > 0 && isASCIIAlpha(raw[0]) {
		valid := true
		for j := 0; j < i; j++ {
			c := raw[j]
			if !(isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
				valid = false
				break
			}
		}
		if valid {
			scheme, raw = strings.ToLower(raw[:i]), raw[i+1:]
		}
	}
	if strings.HasPrefix(raw, "//") {
		rest := raw[2:]
		end := len(rest)
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			end = j
		}
		netloc, raw = rest[:end], rest[end:]
		if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
			return "", "", "", "", "", false
		}
	}
	raw, fragment, _ = strings.Cut(raw, "#")
	raw, query, _ = strings.Cut(raw, "?")
	return scheme, netloc, raw, query, fragment, true
}

func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
