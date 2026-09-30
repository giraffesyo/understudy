package modules

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	names := []string{"deb822_repository", "ansible.builtin.deb822_repository"}
	Register(deb822RepositoryModule, names...)
	for _, n := range names {
		specs[n] = deb822Spec
		pathInfoModules[n] = true
	}
}

const aptKeyringsDir = "/etc/apt/keyrings"

var deb822Spec = args.Spec{
	"allow_downgrade_to_insecure": {Type: "bool"},
	"allow_insecure":              {Type: "bool"},
	"allow_weak":                  {Type: "bool"},
	"architectures":               {Type: "list"},
	"by_hash":                     {Type: "bool"},
	"check_date":                  {Type: "bool"},
	"check_valid_until":           {Type: "bool"},
	"components":                  {Type: "list"},
	"date_max_future":             {Type: "int"},
	"enabled":                     {Type: "bool"},
	"exclude":                     {Type: "list"},
	"include":                     {Type: "list"},
	"inrelease_path":              {},
	"install_python_debian":       {Type: "bool", Default: false},
	"languages":                   {Type: "list"},
	"name":                        {Required: true},
	"pdiffs":                      {Type: "bool"},
	"signed_by":                   {},
	"suites":                      {Type: "list"},
	"targets":                     {Type: "list"},
	"trusted":                     {Type: "bool"},
	"types":                       {Type: "list", Default: []any{"deb"}},
	"uris":                        {Type: "list"},
	"mode":                        {Type: "any", Default: "0644"},
	"state":                       {Default: "present", Choices: []string{"present", "absent"}},
}

// deb822Fields are the options rendered into the .sources stanza, in the
// module's sorted(params) order.
var deb822Fields = func() []string {
	var out []string
	for k := range deb822Spec {
		switch k {
		case "mode", "state", "install_python_debian":
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}()

// strElems converts list elements to strings (elements='str'), keeping
// order.
func strElems(items []any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch t := it.(type) {
		case string:
			out = append(out, t)
		case bool:
			if t {
				out = append(out, "True")
			} else {
				out = append(out, "False")
			}
		default:
			out = append(out, fmt.Sprint(t))
		}
	}
	return out
}

// pyTitle is str.title(): upper-case the first letter of every run of
// letters, lower-case the rest.
func pyTitle(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		switch {
		case isLetter && !prevLetter:
			b.WriteString(strings.ToUpper(string(r)))
		case isLetter:
			b.WriteString(strings.ToLower(string(r)))
		default:
			b.WriteRune(r)
		}
		prevLetter = isLetter
	}
	return b.String()
}

func deb822FieldName(k string) string {
	switch k {
	case "name":
		return "X-Repolib-Name"
	case "uris":
		return "URIs"
	}
	return pyTitle(strings.ReplaceAll(k, "_", "-"))
}

// deb822Multiline is the module's format_multiline.
func deb822Multiline(v string) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(v), "\n") {
		l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		if l == "" {
			l = "."
		}
		lines = append(lines, "    "+l)
	}
	return "\n" + strings.Join(lines, "\n")
}

var urlSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func deb822RepositoryModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := deb822Spec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if err := deb822Spec.MutuallyExclusive(rawArgs, []string{"exclude", "include"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	var bad []string
	for _, t := range strElems(p.List("types")) {
		if t != "deb" && t != "deb-src" {
			bad = append(bad, t)
		}
	}
	if len(bad) > 0 {
		return agentproto.Fail("value of types must be one or more of: deb, deb-src. Got no match for: %s", strings.Join(bad, ", "))
	}

	check := env.CheckMode
	changed := false
	name := p.Str("name")
	legacy := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(
		regexp.MustCompile(`[_\s]+`).ReplaceAllString(strings.ToLower(name), "-"), "")
	slug := strings.ReplaceAll(name, " ", "-")
	if _, err := os.Stat(deb822SourcesFile(legacy)); err == nil {
		slug = legacy
	}
	sources := deb822SourcesFile(slug)

	if p.Str("state") == "absent" {
		if _, err := os.Stat(sources); err == nil {
			if !check {
				if err := os.Remove(sources); err != nil {
					return agentproto.Fail("%v", err)
				}
			}
			changed = true
		}
		var keyFile string
		for _, ext := range []string{"asc", "gpg"} {
			keyFile = aptKeyringsDir + "/" + slug + "." + ext
			if _, err := os.Stat(keyFile); err == nil {
				if !check {
					if err := os.Remove(keyFile); err != nil {
						return agentproto.Fail("%v", err)
					}
				}
				changed = true
			}
		}
		return &agentproto.Result{Changed: changed, Extra: map[string]any{
			"repo": nil, "dest": sources, "key_filename": keyFile,
		}}
	}

	var keyFilename any
	var b strings.Builder
	for _, k := range deb822Fields {
		if !p.Has(k) {
			continue
		}
		var value string
		switch deb822Spec[k].Type {
		case "bool":
			value = "no"
			if p.Bool(k) {
				value = "yes"
			}
		case "int":
			value = fmt.Sprint(p.Int(k))
		case "list":
			value = strings.Join(strElems(p.List(k)), " ")
		default:
			value = p.Str(k)
			if k == "signed_by" {
				kc, filename, data, err := deb822SignedBy(check, value, slug)
				if err != nil {
					return agentproto.Fail("%v", err)
				}
				changed = changed || kc
				if filename != "" {
					value = filename
					keyFilename = filename
				} else {
					value = data
				}
			}
		}
		if strings.Contains(value, "\n") {
			value = deb822Multiline(value)
		}
		if value == "" || value[0] == '\n' {
			fmt.Fprintf(&b, "%s:%s\n", deb822FieldName(k), value)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", deb822FieldName(k), value)
		}
	}
	repo := b.String()
	c, err := writeIfDifferent(check, sources, []byte(repo), p.Any("mode"))
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	return &agentproto.Result{Changed: changed || c, Extra: map[string]any{
		"repo": repo, "dest": sources, "key_filename": keyFilename,
	}}
}

func deb822SourcesFile(slug string) string {
	return "/etc/apt/sources.list.d/" + slug + ".sources"
}

// deb822SignedBy is write_signed_by_key: an existing file is referenced,
// a URL is downloaded into /etc/apt/keyrings/<slug>.{asc,gpg}, anything
// else (inline armor, fingerprints) passes through as data.
func deb822SignedBy(check bool, v, slug string) (changed bool, filename, data string, err error) {
	if st, err := os.Stat(v); err == nil && st.Mode().IsRegular() {
		return false, v, "", nil
	}
	if !urlSchemeRe.MatchString(v) {
		return false, "", v, nil
	}
	body, _, ferr := fetchURLStatus(v, true, 10*time.Second)
	if ferr != nil {
		return false, "", "", fmt.Errorf("Could not fetch signed_by key.")
	}
	if len(body) == 0 {
		return false, v, "", nil
	}
	ext := "gpg"
	if strings.Contains(string(body), "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		ext = "asc"
	}
	filename = aptKeyringsDir + "/" + slug + "." + ext
	cur, rerr := os.ReadFile(filename)
	if rerr != nil || string(cur) != string(body) {
		dc, err := ensureAptKeyringsDir(check)
		if err != nil {
			return changed, "", "", err
		}
		changed = changed || dc
		if !check {
			if err := fsutil.AtomicRewrite(filename, strings.NewReader(string(body)), 0o600); err != nil {
				return changed, "", "", err
			}
		}
		changed = true
	}
	mc, err := writeIfDifferentModeOnly(check, filename, 0o644)
	if err != nil {
		return changed, "", "", err
	}
	return changed || mc, filename, "", nil
}

// writeIfDifferentModeOnly is set_mode_if_different for an octal mode.
func writeIfDifferentModeOnly(check bool, path string, mode os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if check && os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if info.Mode().Perm() == mode && fsutil.UnixBits(info.Mode())&0o7000 == 0 {
		return false, nil
	}
	if !check {
		if err := os.Chmod(path, mode); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ensureAptKeyringsDir creates /etc/apt/keyrings root:root 0755.
func ensureAptKeyringsDir(check bool) (bool, error) {
	changed := false
	if st, err := os.Stat(aptKeyringsDir); err != nil || !st.IsDir() {
		changed = true
		if check {
			return true, nil
		}
		if err := os.Mkdir(aptKeyringsDir, 0o755); err != nil {
			return changed, err
		}
	}
	if check {
		st, _ := os.Stat(aptKeyringsDir)
		uid, gid, ok := statIDsOf(st)
		return changed || fsutil.UnixBits(st.Mode()) != 0o755 || (ok && (uid != 0 || gid != 0)), nil
	}
	c, err := fsutil.ApplyFileAttrs(aptKeyringsDir, "0755", "root", "root", true)
	return changed || c, err
}
