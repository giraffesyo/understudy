package modules

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

// Ports of ansible.mysql's module_utils/user.py helpers shared by
// mysql_user and mysql_info.

// privMap is an insertion-ordered {db_table: [privileges]} (the Python
// dicts privileges_get/privileges_unpack return).
type privMap struct {
	keys []string
	m    map[string][]string
}

func newPrivMap() *privMap { return &privMap{m: map[string][]string{}} }

func (p *privMap) has(k string) bool { _, ok := p.m[k]; return ok }

func (p *privMap) set(k string, v []string) {
	if !p.has(k) {
		p.keys = append(p.keys, k)
	}
	p.m[k] = v
}

func (p *privMap) extend(k string, v []string) {
	p.set(k, append(p.m[k], v...))
}

func (p *privMap) equal(o *privMap) bool {
	if len(p.m) != len(o.m) {
		return false
	}
	for k, v := range p.m {
		w, ok := o.m[k]
		if !ok || len(v) != len(w) {
			return false
		}
		for i := range v {
			if v[i] != w[i] {
				return false
			}
		}
	}
	return true
}

// mysqlSession caches per-connection facts the Python helpers re-query
// (server version and implementation).
type mysqlSession struct {
	m       *mysqlModule
	c       *mysqlclient.Conn
	version string
	impl    string
}

func newMySQLSession(m *mysqlModule, c *mysqlclient.Conn) (*mysqlSession, error) {
	s := &mysqlSession{m: m, c: c}
	v, err := mysqlServerVersion(c)
	if err != nil {
		return nil, err
	}
	s.version = v
	s.impl = "mysql"
	if strings.Contains(strings.ToLower(v), "mariadb") {
		s.impl = "mariadb"
	}
	return s, nil
}

// userImpl is get_user_implementation(): it warns on MariaDB, as
// get_server_implementation does on every call.
func (s *mysqlSession) userImpl() string {
	if s.impl == "mariadb" {
		s.m.warn("MariaDB has been detected: its support will be dropped in 6.0.0. " +
			"For MariaDB automation, please use the ansible.mariadb collection instead.")
	}
	return s.impl
}

func (s *mysqlSession) isMaria() bool { return s.impl == "mariadb" }

func (s *mysqlSession) useOldUserMgmt() bool {
	if s.isMaria() {
		return looseVersionLess(s.version, "10.2")
	}
	return looseVersionLess(s.version, "5.7")
}

func (s *mysqlSession) supportsIdentifiedByPassword() bool {
	return s.isMaria() || looseVersionLess(s.version, "8")
}

func (s *mysqlSession) supportsNativePassword() bool {
	return s.isMaria() || looseVersionLess(s.version, "9.7")
}

func (s *mysqlSession) supportsAlterUser() bool {
	if s.isMaria() {
		return !looseVersionLess(s.version, "10.2")
	}
	return !looseVersionLess(s.version, "5.6")
}

func (s *mysqlSession) supportsPasswordExpire() bool {
	if s.isMaria() {
		return !looseVersionLess(s.version, "10.4.3")
	}
	return !looseVersionLess(s.version, "5.7")
}

// exec runs a query with PyMySQL parameter binding.
func (s *mysqlSession) exec(query string, args any) (*mysqlclient.Result, error) {
	res, _, err := s.c.Exec(query, args)
	return res, err
}

// fetchone returns the first row decoded, or nil.
func fetchone(res *mysqlclient.Result) []any {
	if res == nil || len(res.Rows) == 0 {
		return nil
	}
	row := make([]any, len(res.Fields))
	for i, f := range res.Fields {
		row[i] = f.Convert(res.Rows[0][i])
	}
	return row
}

func fetchall(res *mysqlclient.Result) [][]any {
	var out [][]any
	for _, r := range res.Rows {
		row := make([]any, len(res.Fields))
		for i, f := range res.Fields {
			row[i] = f.Convert(r[i])
		}
		out = append(out, row)
	}
	return out
}

func cellString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case mysqlclient.Bytes:
		return string(t)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// privilegesGet is privileges_get(): SHOW GRANTS parsed into a privMap.
func (s *mysqlSession) privilegesGet(user, host string, mariaRole bool) (*privMap, error) {
	var res *mysqlclient.Result
	var err error
	if !mariaRole {
		res, err = s.exec("SHOW GRANTS FOR %s@%s", []any{user, host})
	} else {
		res, err = s.exec("SHOW GRANTS FOR %s", []any{user})
	}
	if err != nil {
		return nil, err
	}
	out := newPrivMap()
	for _, row := range fetchall(res) {
		grant := cellString(row[0])
		var privsStr, db, tail string
		ok := false
		if !mariaRole {
			privsStr, db, tail, ok = matchGrantLine(grant)
		} else if strings.HasPrefix(grant, "GRANT ") {
			if i := strings.LastIndex(grant, " TO "); i > len("GRANT ") {
				head := grant[len("GRANT "):i]
				if j := strings.LastIndex(head, " ON "); j > 0 {
					privsStr, db, ok = head[:j], head[j+4:], true
				}
			}
		}
		if !ok {
			if roleGrantRe.MatchString(grant) && !mariaRole {
				continue
			}
			return nil, fmt.Errorf("unable to parse the MySQL grant string: %s", grant)
		}
		var privileges []string
		for _, x := range strings.Split(privsStr, ",") {
			x = strings.TrimSpace(x)
			if x == "ALL PRIVILEGES" {
				x = "ALL"
			}
			privileges = append(privileges, x)
		}
		privileges = normalizeColGrants(privileges)
		if !mariaRole && strings.Contains(tail, "WITH GRANT OPTION") {
			privileges = append(privileges, "GRANT")
		}
		out.extend(db, privileges)
	}
	return out, nil
}

var roleGrantRe = regexp.MustCompile("^(?:GRANT (.+) TO (['`\"]).*|SET DEFAULT ROLE (.+) FOR (['`\"]).*)")

func isQuote(c byte) bool { return c == '\'' || c == '`' || c == '"' }

// matchGrantLine emulates re.match of
//
//	GRANT (.+) ON (.+) TO (['`"]).*\3@(['`"]).*\4( IDENTIFIED BY PASSWORD (['`"]).+\6)? ?(.*)
//
// (backreferences RE2 lacks), trying the greedy groups' candidates in
// Python's backtracking order. It returns groups 1, 2 and 7.
func matchGrantLine(s string) (string, string, string, bool) {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if !strings.HasPrefix(s, "GRANT ") {
		return "", "", "", false
	}
	body := s[len("GRANT "):]
	ons := allIndices(body, " ON ")
	for x := len(ons) - 1; x >= 0; x-- {
		i1 := ons[x]
		if i1 < 1 {
			continue
		}
		after := body[i1+4:]
		tos := allIndices(after, " TO ")
		for y := len(tos) - 1; y >= 0; y-- {
			i2 := tos[y]
			if i2 < 1 {
				continue
			}
			rest := after[i2+4:]
			if rest == "" || !isQuote(rest[0]) {
				continue
			}
			q3 := rest[0]
			r := rest[1:]
			ats := allIndices(r, string(q3)+"@")
			for z := len(ats) - 1; z >= 0; z-- {
				r2 := r[ats[z]+2:]
				if r2 == "" || !isQuote(r2[0]) {
					continue
				}
				q4 := r2[0]
				r3 := r2[1:]
				b := strings.LastIndexByte(r3, q4)
				if b < 0 {
					continue
				}
				tail := strings.TrimPrefix(r3[b+1:], " ")
				return body[:i1], after[:i2], tail, true
			}
		}
	}
	return "", "", "", false
}

// allIndices lists every (possibly overlapping) occurrence of sub in s.
func allIndices(s, sub string) []int {
	var out []int
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			out = append(out, i)
		}
	}
	return out
}

// normalizeColGrants fixes and sorts column grants split by ", ".
func normalizeColGrants(privileges []string) []string {
	for _, grant := range []string{"SELECT", "UPDATE", "INSERT", "REFERENCES"} {
		start, end := -1, -1
		for n, priv := range privileges {
			if strings.Contains(priv, grant+" (") {
				start = n
			}
			if start >= 0 && strings.Contains(priv, ")") {
				end = n
				break
			}
		}
		if start < 0 || end < 0 {
			continue
		}
		if start != end {
			out := append([]string{}, privileges[:start]...)
			out = append(out, sortColumnOrder(strings.Join(privileges[start:end+1], ", ")))
			privileges = append(out, privileges[end+1:]...)
		} else {
			out := append([]string{}, privileges...)
			out[start] = sortColumnOrder(out[start])
			privileges = out
		}
	}
	return privileges
}

func sortColumnOrder(statement string) string {
	tmp := strings.Split(statement, "(")
	privName := tmp[0]
	columns := strings.TrimRight(tmp[1], ")")
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = strings.Trim(strings.TrimSpace(c), "`")
	}
	sort.Strings(cols)
	return fmt.Sprintf("%s(%s)", privName, strings.Join(cols, ", "))
}

// splitColPrivs is re.split(r',\s*(?=[^)]*(?:\(|$))', s): split at commas
// that are not inside a column list.
func splitColPrivs(s string) []string {
	var out []string
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ',' {
			continue
		}
		j := i + 1
		for j < len(s) && strings.IndexByte(" \t\n\r\f\v", s[j]) >= 0 {
			j++
		}
		rest := s[j:]
		k := strings.IndexByte(rest, ')')
		if k < 0 || strings.IndexByte(rest[:k], '(') >= 0 {
			out = append(out, s[last:i])
			last = j
			i = j - 1
		}
	}
	return append(out, s[last:])
}

var colSuffixRe = regexp.MustCompile(`\s*\(.*\)`)

// privilegesUnpack is privileges_unpack(): "db.tbl:PRIV1,PRIV2/..." into
// a privMap, db and table quoted with the sql_mode's identifier quote.
func privilegesUnpack(priv, mode string, columnCaseSensitive, ensureUsage bool) (*privMap, error) {
	quote := "`"
	if mode == "ANSI" {
		quote = `"`
	}
	out := newPrivMap()
	for _, item := range strings.Split(strings.TrimSpace(priv), "/") {
		item = strings.TrimSpace(item)
		var pieces []string
		if i := strings.LastIndexByte(item, ':'); i >= 0 {
			pieces = []string{item[:i], item[i+1:]}
		} else {
			return nil, fmt.Errorf("list index out of range")
		}
		var dbpriv []string
		if i := strings.LastIndexByte(pieces[0], '.'); i >= 0 {
			dbpriv = []string{pieces[0][:i], pieces[0][i+1:]}
		} else {
			dbpriv = []string{pieces[0]}
		}
		objectType := ""
		if parts := strings.SplitN(dbpriv[0], " ", 2); len(parts) > 1 && (parts[0] == "FUNCTION" || parts[0] == "PROCEDURE") {
			objectType = parts[0] + " "
			dbpriv[0] = parts[1]
		}
		for i, side := range dbpriv {
			if strings.Trim(side, "`") != "*" {
				dbpriv[i] = quote + strings.Trim(side, "`") + quote
			}
		}
		key := objectType + strings.Join(dbpriv, ".")
		var privs []string
		if strings.Contains(pieces[1], "(") {
			src := pieces[1]
			if !columnCaseSensitive {
				src = strings.ToUpper(src)
			}
			privs = splitColPrivs(src)
			_ = colSuffixRe
		} else {
			privs = strings.Split(strings.ToUpper(pieces[1]), ",")
		}
		out.set(key, normalizeColGrants(privs))
	}
	if ensureUsage && !out.has("*.*") {
		out.set("*.*", []string{"USAGE"})
	}
	return out, nil
}

// convertPrivDictToStr is convert_priv_dict_to_str().
func convertPrivDictToStr(priv map[string]any) string {
	keys := make([]string, 0, len(priv))
	for k := range priv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + pyStrValue(priv[k])
	}
	return strings.Join(parts, "/")
}

// sanitizeRequires is sanitize_requires(): nil, "SSL", "X509", or the
// upper-cased CIPHER/ISSUER/SUBJECT dict.
func sanitizeRequires(req map[string]any) any {
	if len(req) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range req {
		out[strings.ToUpper(k)] = v
	}
	for _, k := range []string{"CIPHER", "ISSUER", "SUBJECT"} {
		if _, ok := out[k]; ok {
			delete(out, "SSL")
			delete(out, "X509")
			return out
		}
	}
	if _, ok := out["X509"]; ok {
		return "X509"
	}
	return "SSL"
}

func requiresEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok != bok {
		return false
	}
	if !aok {
		return a == b
	}
	if len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		w, ok := bm[k]
		if !ok || pyStrValue(v) != pyStrValue(w) {
			return false
		}
	}
	return true
}

// mogrifyRequires is mogrify_requires(): append REQUIRE ... to query.
func mogrifyRequires(query string, params []any, req any) (string, []any) {
	switch t := req.(type) {
	case nil:
		return query, params
	case string:
		return query + " REQUIRE " + t, params
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + " %s"
			params = append(params, t[k])
		}
		return query + " REQUIRE " + strings.Join(parts, " AND "), params
	}
	return query, params
}

// getTLSRequires is the implementation's get_tls_requires().
func (s *mysqlSession) getTLSRequires(user, host string) (map[string]any, error) {
	if s.isMaria() {
		res, err := s.exec("SELECT ssl_type, ssl_cipher, x509_issuer, x509_subject FROM mysql.user WHERE User = %s AND Host = %s", []any{user, host})
		if err != nil {
			return nil, err
		}
		row := fetchone(res)
		if row == nil {
			return nil, fmt.Errorf("'NoneType' object is not iterable")
		}
		set := false
		for _, v := range row {
			if cellString(v) != "" {
				set = true
			}
		}
		if !set {
			return nil, nil
		}
		out := map[string]any{}
		switch cellString(row[0]) {
		case "ANY":
			out["SSL"] = nil
		case "X509":
			out["X509"] = nil
		}
		for i, k := range []string{"CIPHER", "ISSUER", "SUBJECT"} {
			if v := cellString(row[i+1]); v != "" {
				out[k] = v
			}
		}
		return out, nil
	}
	var q string
	if !s.useOldUserMgmt() {
		q = fmt.Sprintf("SHOW CREATE USER '%s'@'%s'", user, host)
	} else {
		q = fmt.Sprintf("SHOW GRANTS for '%s'@'%s'", user, host)
	}
	res, err := s.c.Query(q)
	if err != nil {
		return nil, err
	}
	row := fetchone(res)
	var b strings.Builder
	for _, v := range row {
		b.WriteString(cellString(v))
	}
	return parseRequireClause(b.String()), nil
}

var (
	requireWordRe  = regexp.MustCompile(`\bREQUIRE\b`)
	passwordWordRe = regexp.MustCompile(`\bPASSWORD\b`)
)

// parseRequireClause extracts the REQUIRE clause of SHOW CREATE USER.
func parseRequireClause(s string) map[string]any {
	loc := requireWordRe.FindStringIndex(s)
	if loc == nil {
		return nil
	}
	rest := s[loc[1]:]
	if p := passwordWordRe.FindStringIndex(rest); p != nil {
		rest = rest[:p[0]]
	} else {
		rest = strings.TrimSuffix(rest, "\n")
	}
	req := strings.TrimSpace(rest)
	switch {
	case strings.HasPrefix(req, "NONE"):
		return nil
	case strings.HasPrefix(req, "SSL"):
		return map[string]any{"SSL": nil}
	case strings.HasPrefix(req, "X509"):
		return map[string]any{"X509": nil}
	}
	items, _ := shlexSplit(req)
	out := map[string]any{}
	for i := 0; i+1 < len(items); i += 2 {
		out[items[i]] = items[i+1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// getGrants is get_grants(): the global (ON *.*) privilege list.
func (s *mysqlSession) getGrants(user, host string) ([]string, error) {
	res, err := s.exec("SHOW GRANTS FOR %s@%s", []any{user, host})
	if err != nil {
		return nil, err
	}
	for _, row := range fetchall(res) {
		line := cellString(row[0])
		if !strings.Contains(line, "ON *.*") {
			continue
		}
		g := regexp.MustCompile(`\bGRANT\b`).FindStringIndex(line)
		o := regexp.MustCompile(`\bON\b`).FindStringIndex(line)
		if g == nil || o == nil || o[0] < g[1] {
			return nil, fmt.Errorf("'NoneType' object has no attribute 'group'")
		}
		return strings.Split(strings.TrimSpace(line[g[1]:o[0]]), ", "), nil
	}
	return nil, fmt.Errorf("list index out of range")
}

// getAttributeSupport reports INFORMATION_SCHEMA.USER_ATTRIBUTES support.
func (s *mysqlSession) getAttributeSupport() bool {
	_, err := s.c.Query("SELECT attribute FROM INFORMATION_SCHEMA.USER_ATTRIBUTES LIMIT 0")
	return err == nil
}

// attributesGet returns a user's attributes, or nil when none.
func (s *mysqlSession) attributesGet(user, host string) (map[string]any, error) {
	res, err := s.exec("SELECT attribute FROM INFORMATION_SCHEMA.USER_ATTRIBUTES WHERE user = %s AND host = %s", []any{user, host})
	if err != nil {
		return nil, err
	}
	row := fetchone(res)
	if row == nil || row[0] == nil || cellString(row[0]) == "" {
		return nil, nil
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(cellString(row[0])), &j); err != nil {
		return nil, err
	}
	if len(j) == 0 {
		return nil, nil
	}
	return normalizeJSON(j).(map[string]any), nil
}

// normalizeJSON converts encoding/json numbers to int64 where integral.
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeJSON(e)
		}
	case []any:
		for i, e := range t {
			t[i] = normalizeJSON(e)
		}
	}
	return v
}

// pyJSONDumps is json.dumps() with Python's default separators.
func pyJSONDumps(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		b, _ := json.Marshal(t)
		s := string(b)
		// Python escapes non-ASCII (ensure_ascii) and not <, >, &.
		s = strings.NewReplacer(`<`, "<", `>`, ">", `&`, "&").Replace(s)
		var out strings.Builder
		for _, r := range s {
			if r > 0x7e {
				if r > 0xffff {
					r -= 0x10000
					fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
				} else {
					fmt.Fprintf(&out, `\u%04x`, r)
				}
				continue
			}
			out.WriteRune(r)
		}
		return out.String()
	case float64:
		return mysqlclient.PyFloatRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyJSONDumps(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyJSONDumps(k) + ": " + pyJSONDumps(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// getResourceLimits is get_resource_limits(); nil when the user is absent.
func (s *mysqlSession) getResourceLimits(user, host string) (map[string]any, []string, error) {
	res, err := s.exec("SELECT max_questions AS MAX_QUERIES_PER_HOUR, max_updates AS MAX_UPDATES_PER_HOUR, "+
		"max_connections AS MAX_CONNECTIONS_PER_HOUR, max_user_connections AS MAX_USER_CONNECTIONS "+
		"FROM mysql.user WHERE User = %s AND Host = %s", []any{user, host})
	if err != nil {
		return nil, nil, err
	}
	row := fetchone(res)
	if row == nil {
		return nil, nil, nil
	}
	keys := []string{"MAX_QUERIES_PER_HOUR", "MAX_UPDATES_PER_HOUR", "MAX_CONNECTIONS_PER_HOUR", "MAX_USER_CONNECTIONS"}
	out := map[string]any{}
	for i, k := range keys {
		out[k] = mysqlJSONValue(row[i])
	}
	if s.isMaria() {
		res, err := s.exec("SELECT max_statement_time AS MAX_STATEMENT_TIME FROM mysql.user WHERE User = %s AND Host = %s", []any{user, host})
		if err != nil {
			return nil, nil, err
		}
		r := fetchone(res)
		if r != nil {
			v := mysqlJSONValue(r[0])
			// max_statement_time is DECIMAL: Decimal('0.000000').
			out["MAX_STATEMENT_TIME"] = v
		}
		keys = append(keys, "MAX_STATEMENT_TIME")
	}
	return out, keys, nil
}

// getExistingAuthentication is get_existing_authentication().
func (s *mysqlSession) getExistingAuthentication(user, host string) ([]map[string]any, error) {
	var res *mysqlclient.Result
	var err error
	params := map[string]any{"user": user, "host": host}
	switch {
	case s.isMaria() && host != "":
		res, err = s.exec(`select plugin, auth from (
                select plugin, password as auth from mysql.user where user=%(user)s
                and host=%(host)s
                union select plugin, authentication_string as auth from mysql.user where user=%(user)s
                and host=%(host)s) x group by plugin, auth
            `, params)
	case s.isMaria():
		res, err = s.exec(`select plugin, auth from (
                select plugin, password as auth from mysql.user where user=%(user)s
                union select plugin, authentication_string as auth from mysql.user where user=%(user)s
                ) x group by plugin, auth
            `, params)
	case host != "":
		res, err = s.exec(`select plugin, authentication_string as auth
                from mysql.user where user=%(user)s and host=%(host)s
                group by plugin, authentication_string`, params)
	default:
		res, err = s.exec(`select plugin, authentication_string as auth
                from mysql.user where user=%(user)s
                group by plugin, authentication_string`, params)
	}
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, r := range fetchall(res) {
		out = append(out, map[string]any{
			"plugin":             mysqlJSONValue(r[0]),
			"plugin_auth_string": mysqlJSONValue(r[1]),
			"plugin_hash_string": mysqlJSONValue(r[1]),
		})
	}
	return out, nil
}

// userIsLocked is user_is_locked().
func (s *mysqlSession) userIsLocked(user, host string) (bool, error) {
	res, err := s.exec("SHOW CREATE USER %s@%s", []any{user, host})
	if err != nil {
		return false, err
	}
	row := fetchone(res)
	if row == nil {
		return false, nil
	}
	return strings.Index(cellString(row[0]), "ACCOUNT LOCK") > 0, nil
}
