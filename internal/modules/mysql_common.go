package modules

import (
	"fmt"
	"math"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

// The community.mysql modules (now ansible.mysql) talk to the server with
// PyMySQL. They are ported onto internal/modules/mysqlclient, a stdlib-only
// wire-protocol client that reproduces PyMySQL's behavior, rather than
// onto the mysql CLI: the CLI is often absent where only a server port is
// reachable, its output needs fragile parsing (NULL vs 'NULL', embedded
// tabs/newlines, binary columns), and quoting SQL through a shell argument
// is exactly the class of bug parameterized execution avoids. Only
// mysql_db's dump/import states shell out, to mysqldump/mysql, as the
// Python module does.

// mysqlCommonSpec is mysql_common_argument_spec() plus a module's own
// arguments.
func mysqlCommonSpec(extra args.Spec) args.Spec {
	s := args.Spec{
		"login_user":        {},
		"login_password":    {},
		"login_host":        {Default: "localhost"},
		"login_port":        {Type: "int", Default: 3306},
		"login_unix_socket": {},
		"config_file":       {Default: "~/.my.cnf"},
		"connect_timeout":   {Type: "int", Default: 30},
		"client_cert":       {Aliases: []string{"ssl_cert"}},
		"client_key":        {Aliases: []string{"ssl_key"}},
		"ca_cert":           {Aliases: []string{"ssl_ca"}},
		"check_hostname":    {Type: "bool"},
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// mysqlModule wraps one invocation's shared state: parsed arguments, the
// module's warnings and the no_log values to censor from its result.
type mysqlModule struct {
	env      *RunEnv
	p        *args.Parsed
	warnings []any
	noLog    []string
	// the resolved FQCN, for "remote module (...)" messages
	name string
	// the Python driver ansible's module would use (see mysqlConnector)
	connName, connVersion string
	hasDriver             bool
}

// mysqlDriverFailMsg is module_utils.mysql's mysql_driver_fail_msg.
const mysqlDriverFailMsg = "A MySQL module is required: for Python 2.7 either PyMySQL, or " +
	"MySQL-python, or for Python 3.X mysqlclient or PyMySQL. " +
	"Consider setting ansible_python_interpreter to use " +
	"the intended Python version."

// mysqlConnector is the driver ansible's mysql modules import on this
// host (PyMySQL, else mysqlclient's MySQLdb), named and versioned as
// get_connector_name/get_connector_version report it, read from the
// target Python's package metadata. ok is false when that Python has
// neither: ansible's modules fail there (mysql_driver_fail_msg). A target
// without Python gets the client's own identity, PyMySQL at the release
// it reproduces.
func mysqlConnector(env *RunEnv) (name, version string, ok bool) {
	if !targetHasPython(env) {
		return mysqlclient.ConnectorName, mysqlclient.ConnectorVersion, true
	}
	for _, d := range []struct{ dist, name string }{{"PyMySQL", "pymysql"}, {"mysqlclient", "MySQLdb"}} {
		if v := pyDistVersion(env, d.dist); v != "" {
			return d.name, pyVersionTriple(v), true
		}
	}
	return "", "", false
}

// pyVersionTriple is '.'.join(map(str, VERSION[:3])) for a release
// string: its first three numeric components ("1.0.2", "2.2.4").
func pyVersionTriple(v string) string {
	var out []string
	for _, part := range strings.Split(v, ".") {
		i := 0
		for i < len(part) && part[i] >= '0' && part[i] <= '9' {
			i++
		}
		if i == 0 {
			break
		}
		n, _ := strconv.Atoi(part[:i])
		out = append(out, strconv.Itoa(n))
		if len(out) == 3 || i < len(part) {
			break
		}
	}
	return strings.Join(out, ".")
}

// driverMissing is the modules' "if mysql_driver is None" check.
func (m *mysqlModule) driverMissing() *agentproto.Result {
	if m.hasDriver {
		return nil
	}
	return m.failf("%s", mysqlDriverFailMsg)
}

func newMySQLModule(env *RunEnv, p *args.Parsed, name string, noLogParams ...string) *mysqlModule {
	m := &mysqlModule{env: env, p: p, name: name}
	m.connName, m.connVersion, m.hasDriver = mysqlConnector(env)
	for _, k := range append([]string{"login_password"}, noLogParams...) {
		m.noLog = append(m.noLog, noLogStrings(p.Any(k))...)
	}
	return m
}

// noLogStrings is _return_datastructure_name: the strings a no_log value
// contributes to the censor list.
func noLogStrings(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case int64:
		return []string{strconv.FormatInt(t, 10)}
	case int:
		return []string{strconv.Itoa(t)}
	case float64:
		return []string{mysqlclient.PyFloatRepr(t)}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, noLogStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range t {
			out = append(out, noLogStrings(e)...)
		}
		return out
	}
	return nil
}

func (m *mysqlModule) warn(msg string) {
	for _, w := range m.warnings {
		if w == msg {
			return
		}
	}
	m.warnings = append(m.warnings, msg)
}

// censor is remove_values(): a string equal to a no_log value becomes the
// placeholder; occurrences inside strings become ********.
func (m *mysqlModule) censor(v any) any {
	if len(m.noLog) == 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		for _, s := range m.noLog {
			if t == s {
				return "VALUE_SPECIFIED_IN_NO_LOG_PARAMETER"
			}
		}
		for _, s := range m.noLog {
			t = strings.ReplaceAll(t, s, "********")
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = m.censor(e)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = m.censor(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = m.censor(e)
		}
		return out
	}
	return v
}

// exit builds the module result (exit_json).
func (m *mysqlModule) exit(changed bool, kv map[string]any) *agentproto.Result {
	res := &agentproto.Result{Changed: changed, Extra: map[string]any{}}
	for k, v := range kv {
		if k == "msg" {
			s, _ := m.censor(v).(string)
			res.Msg = s
			if s == "" {
				res.Extra["msg"] = "" // an empty msg is still a key
			}
			continue
		}
		if k == "changed" {
			continue
		}
		res.Extra[k] = m.censor(v)
	}
	if len(m.warnings) > 0 {
		res.Extra["warnings"] = m.warnings
	}
	return res
}

// fail builds a failed result (fail_json).
func (m *mysqlModule) fail(msg string, kv map[string]any) *agentproto.Result {
	res := m.exit(false, kv)
	res.Failed = true
	s, _ := m.censor(msg).(string)
	res.Msg = s
	return res
}

func (m *mysqlModule) failf(format string, a ...any) *agentproto.Result {
	return m.fail(fmt.Sprintf(format, a...), nil)
}

// configFile is the config_file argument as a 'path': user and variables
// expanded.
func (m *mysqlModule) configFile() string {
	return pyExpandPath(m.p.Str("config_file"))
}

// connectOpts are mysql_connect()'s keyword arguments.
type connectOpts struct {
	user, password    *string
	db                string
	autocommit        bool
	overridesDefaults bool
}

func strPtr(s string) *string { return &s }

func (m *mysqlModule) optStr(name string) *string {
	if !m.p.Has(name) {
		return nil
	}
	return strPtr(m.p.Str(name))
}

// connect is mysql_connect(): it assembles PyMySQL's connection kwargs
// (including read_default_file handling of ~/.my.cnf) and connects.
func (m *mysqlModule) connect(o connectOpts) (*mysqlclient.Conn, error) {
	p := m.p
	configFile := m.configFile()
	loginHost := p.Str("login_host")
	loginPort := int(p.Int("login_port"))
	readDefault := configFile != "" && fileExists(configFile)

	if readDefault && o.overridesDefaults {
		cp, err := mysqlclient.ReadOptionFile(configFile, mysqlclient.ModuleParser)
		if err != nil {
			return nil, mysqlFatal{fmt.Sprintf("Failed to parse %s: %s", configFile, err)}
		}
		if cp.HasSection("client") {
			if h, ok := cp.Get("client", "host", mysqlclient.ModuleParser); ok {
				loginHost = h
			}
			if ps, ok := cp.Get("client", "port", mysqlclient.ModuleParser); ok {
				n, err := strconv.Atoi(strings.TrimSpace(ps))
				if err != nil {
					return nil, fmt.Errorf("invalid literal for int() with base 10: %s", mysqlclient.PyRepr(ps))
				}
				loginPort = n
			}
		}
	}

	cfg := mysqlclient.Config{}
	var (
		user, password, host, database, socket string
		port                                   = 0
	)
	socket = p.Str("login_unix_socket")
	if socket == "" {
		host = loginHost
		port = loginPort
	}
	if o.user != nil {
		user = *o.user
	}
	if o.password != nil {
		password = *o.password
	}
	database = o.db
	var ssl *mysqlclient.SSL
	sslGiven := p.Has("ca_cert") || p.Has("client_key") || p.Has("client_cert") || p.Has("check_hostname")
	sslMap := map[string]string{}
	if p.Has("client_cert") {
		sslMap["cert"] = pyExpandPath(p.Str("client_cert"))
	}
	if p.Has("client_key") {
		sslMap["key"] = pyExpandPath(p.Str("client_key"))
	}
	if p.Has("ca_cert") {
		sslMap["ca"] = pyExpandPath(p.Str("ca_cert"))
	}
	if readDefault {
		cfgFile, err := mysqlclient.ReadOptionFile(configFile, mysqlclient.PyMySQLParser)
		if err != nil {
			return nil, err
		}
		get := func(key, cur string) string {
			if cur != "" {
				return cur
			}
			if v, ok := cfgFile.Get("client", key, mysqlclient.PyMySQLParser); ok {
				return v
			}
			return cur
		}
		user = get("user", user)
		password = get("password", password)
		host = get("host", host)
		database = get("database", database)
		socket = get("socket", socket)
		if port == 0 {
			if v, ok := cfgFile.Get("client", "port", mysqlclient.PyMySQLParser); ok {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					return nil, fmt.Errorf("invalid literal for int() with base 10: %s", mysqlclient.PyRepr(v))
				}
				port = n
			}
		}
		for _, k := range []string{"ca", "cert", "key"} {
			if v := get("ssl-"+k, sslMap[k]); v != "" {
				sslMap[k] = v
			}
		}
	}
	if sslGiven || len(sslMap) > 0 {
		ssl = &mysqlclient.SSL{CA: sslMap["ca"], Cert: sslMap["cert"], Key: sslMap["key"]}
		if p.Has("check_hostname") {
			b := p.Bool("check_hostname")
			ssl.CheckHostname = &b
		}
		// An ssl dict holding only check_hostname=None is falsy in
		// PyMySQL: no TLS requirement.
		if len(sslMap) == 0 && !p.Has("check_hostname") {
			ssl = nil
		}
	}
	if user == "" {
		user = defaultMySQLUser()
	}
	timeout := p.Int("connect_timeout")
	if !(timeout > 0 && timeout <= 31536000) {
		return nil, fmt.Errorf("connect_timeout should be >0 and <=31536000")
	}
	cfg.User, cfg.Password = user, password
	cfg.Host, cfg.Port, cfg.UnixSocket = host, port, socket
	cfg.Database = database
	cfg.ConnectTimeout = time.Duration(timeout) * time.Second
	cfg.SSL = ssl
	ac := o.autocommit
	cfg.Autocommit = &ac
	cfg.ClientName, cfg.ClientVersion = m.connName, m.connVersion
	if m.connName == "MySQLdb" {
		m.warn("Support of mysqlcline/MySQLdb connector is deprecated. " +
			"We'll stop testing against it in collection version 4.0.0 " +
			"and remove the related code in 5.0.0. Use PyMySQL connector instead.")
	}
	return mysqlclient.Connect(cfg)
}

// defaultMySQLUser is getpass.getuser(): LOGNAME, USER, LNAME, USERNAME,
// then the password database.
func defaultMySQLUser() string {
	for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// mysqlFatal is a failure mysql_connect reports itself (fail_json)
// rather than raising to the module's connection error handler.
type mysqlFatal struct{ msg string }

func (f mysqlFatal) Error() string { return f.msg }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// mysqlServerVersion is get_server_version(): SELECT VERSION().
func mysqlServerVersion(c *mysqlclient.Conn) (string, error) {
	res, err := c.Query("SELECT VERSION() AS version")
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 || res.Rows[0][0] == nil {
		return "", nil
	}
	return string(res.Rows[0][0]), nil
}

// serverImplementation is get_server_implementation(): "mariadb" (with
// the collection's MariaDB deprecation warning) or "mysql".
func (m *mysqlModule) serverImplementation(c *mysqlclient.Conn) (string, error) {
	v, err := mysqlServerVersion(c)
	if err != nil {
		return "", err
	}
	if strings.Contains(strings.ToLower(v), "mariadb") {
		m.warn("MariaDB has been detected: its support will be dropped in 6.0.0. " +
			"For MariaDB automation, please use the ansible.mariadb collection instead.")
		return "mariadb", nil
	}
	return "mysql", nil
}

// looseVersionLess compares like distutils LooseVersion: numeric runs as
// integers, lowercase letter runs as strings, everything else literal.
func looseVersionLess(a, b string) bool { return looseVersionCmp(a, b) < 0 }

var looseRe = regexp.MustCompile(`\d+|[a-z]+|\.`)

func looseComponents(v string) []any {
	var out []any
	last := 0
	add := func(s string) {
		if s == "" || s == "." {
			return
		}
		if n, err := strconv.Atoi(s); err == nil && s[0] >= '0' && s[0] <= '9' {
			out = append(out, n)
			return
		}
		out = append(out, s)
	}
	for _, loc := range looseRe.FindAllStringIndex(v, -1) {
		add(v[last:loc[0]])
		add(v[loc[0]:loc[1]])
		last = loc[1]
	}
	add(v[last:])
	return out
}

func looseVersionCmp(a, b string) int {
	ca, cb := looseComponents(a), looseComponents(b)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		x, y := ca[i], cb[i]
		xi, xInt := x.(int)
		yi, yInt := y.(int)
		switch {
		case xInt && yInt:
			if xi != yi {
				if xi < yi {
					return -1
				}
				return 1
			}
		case xInt != yInt:
			if xInt {
				return -1
			}
			return 1
		default:
			xs, ys := x.(string), y.(string)
			if xs != ys {
				if xs < ys {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(ca) < len(cb):
		return -1
	case len(ca) > len(cb):
		return 1
	}
	return 0
}

// mysqlQuoteIdentifier is database.mysql_quote_identifier.
func mysqlQuoteIdentifier(identifier, idType string) (string, error) {
	frags, err := identifierParse(identifier, '`')
	if err != nil {
		return "", err
	}
	levels := map[string]int{"database": 1, "table": 2, "column": 3, "role": 1, "vars": 1}
	if len(frags)-1 > levels[idType] {
		return "", fmt.Errorf("MySQL does not support %s with more than %d dots", idType, levels[idType])
	}
	for i, f := range frags {
		if f == "`*`" {
			frags[i] = "*"
		}
	}
	return strings.Join(frags, "."), nil
}

func findEndQuote(identifier string, q byte) (int, bool) {
	accumulate := 0
	for {
		idx := strings.IndexByte(identifier, q)
		if idx < 0 {
			return 0, false
		}
		accumulate += idx
		if idx+1 >= len(identifier) {
			return accumulate, true
		}
		if identifier[idx+1] == q {
			identifier = identifier[idx+2:]
			accumulate += 2
			continue
		}
		return accumulate, true
	}
}

func identifierParse(identifier string, q byte) ([]string, error) {
	if identifier == "" {
		return nil, fmt.Errorf("Identifier name unspecified or unquoted trailing dot")
	}
	qs := string(q)
	quote := func(s string) string { return qs + strings.ReplaceAll(s, qs, qs+qs) + qs }
	if identifier[0] == q {
		if end, ok := findEndQuote(identifier[1:], q); ok {
			end++
			if end < len(identifier)-1 {
				if identifier[end+1] == '.' {
					dot := end + 1
					rest, err := identifierParse(identifier[dot+1:], q)
					if err != nil {
						return nil, err
					}
					return append([]string{identifier[:dot]}, rest...), nil
				}
				return nil, fmt.Errorf("User escaped identifiers must escape extra quotes")
			}
			return []string{identifier}, nil
		}
	}
	dot := strings.IndexByte(identifier, '.')
	if dot < 0 || dot == 0 || dot >= len(identifier)-1 {
		return []string{quote(identifier)}, nil
	}
	rest, err := identifierParse(identifier[dot+1:], q)
	if err != nil {
		return nil, err
	}
	return append([]string{quote(identifier[:dot])}, rest...), nil
}

// pyValueRepr is repr() of a module-level value (str, int, float, bool,
// None, list, dict).
func pyValueRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return mysqlclient.PyRepr(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return mysqlclient.PyFloatRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyValueRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = mysqlclient.PyRepr(e)
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
			parts[i] = mysqlclient.PyRepr(k) + ": " + pyValueRepr(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyStrValue is str() of a module-level value.
func pyStrValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyValueRepr(v)
}

// pyTypeRepr is repr(type(v)): "<class 'int'>".
func pyTypeRepr(v any) string { return "<class '" + pyTypeName(v) + "'>" }

// mysqlJSONValue turns a PyMySQL-decoded cell into what json.dumps(...,
// default=str) round-trips to: Decimal, temporal types and bytes become
// their str().
func mysqlJSONValue(v any) any {
	switch t := v.(type) {
	case mysqlclient.Decimal:
		return string(t)
	case mysqlclient.Date:
		return t.String()
	case mysqlclient.DateTime:
		return t.String()
	case mysqlclient.Timedelta:
		return t.String()
	case mysqlclient.Bytes:
		return pyBytesRepr(string(t))
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return mysqlclient.PyFloatRepr(t)
		}
	case uint64:
		if t <= math.MaxInt64 {
			return int64(t)
		}
	}
	return v
}

// registerMySQL registers a module under its ansible.mysql FQCN and the
// community.mysql / short names that route to it (the routing's
// deprecation is recorded control-side, see executor/routing.go).
func registerMySQL(fn ModuleFunc, name string) {
	Register(fn, name, "ansible.mysql."+name, "community.mysql."+name)
}
