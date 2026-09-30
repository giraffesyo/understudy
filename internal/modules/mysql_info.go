package modules

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

func init() {
	registerMySQL(mysqlInfoModule, "mysql_info")
}

var mysqlInfoSpec = mysqlCommonSpec(args.Spec{
	"login_db":         {},
	"filter":           {Type: "list"},
	"exclude_fields":   {Type: "list"},
	"return_empty_dbs": {Type: "bool", Default: false},
})

// mysqlInfoSubsets are MySQL_Info.info's keys, in order.
var mysqlInfoSubsets = []string{"version", "databases", "settings", "global_status", "engines",
	"users", "users_info", "master_status", "slave_hosts", "slave_status"}

// mysqlInfoConvert is MySQL_Info.__convert: Decimal to float, anything
// int() accepts to int, the rest unchanged.
func mysqlInfoConvert(v any) any {
	switch t := v.(type) {
	case mysqlclient.Decimal:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return string(t)
		}
		return f
	case float64:
		return int64(t)
	case int64, uint64:
		return mysqlJSONValue(t)
	case string:
		if n, ok := pyIntLiteral(strings.TrimSpace(t)); ok {
			return n
		}
		return t
	case mysqlclient.Bytes:
		if n, ok := pyIntLiteral(strings.TrimSpace(string(t))); ok {
			return n
		}
		return pyToText(t)
	}
	return mysqlInfoJSON(v)
}

// mysqlInfoJSON renders the remaining driver types as the module's JSON
// output shows them (bytes as text, datetimes in ISO format).
func mysqlInfoJSON(v any) any {
	switch t := v.(type) {
	case mysqlclient.Bytes:
		return pyToText(t)
	case mysqlclient.DateTime:
		return t.ISO()
	case mysqlclient.Date:
		return t.String()
	case mysqlclient.Decimal:
		return string(t)
	case mysqlclient.Timedelta:
		return t.String()
	case uint64:
		return mysqlJSONValue(t)
	}
	return v
}

type mysqlInfo struct {
	s    *mysqlSession
	info map[string]map[string]any
}

// mysqlCommand is CommandResolver.resolve_command for the replication
// statements mysql_info issues.
func (s *mysqlSession) mysqlCommand(cmd string) string {
	type alt struct {
		impl, version, cmd string
	}
	table := map[string][]alt{
		"SHOW MASTER STATUS": {{"mysql", "8.2.0", "SHOW BINARY LOG STATUS"}, {"mariadb", "10.5.2", "SHOW BINLOG STATUS"}},
		"SHOW SLAVE STATUS":  {{"mysql", "8.0.22", "SHOW REPLICA STATUS"}, {"mariadb", "10.5.1", "SHOW REPLICA STATUS"}},
		"SHOW SLAVE HOSTS":   {{"mysql", "8.0.22", "SHOW REPLICAS"}, {"mariadb", "10.5.1", "SHOW REPLICA HOSTS"}},
	}
	for _, a := range table[cmd] {
		if a.impl == s.impl && !looseVersionLess(s.version, a.version) {
			return a.cmd
		}
	}
	return cmd
}

func (mi *mysqlInfo) execSQL(query string) ([]map[string]any, *agentproto.Result) {
	res, err := mi.s.c.Query(query)
	if err != nil {
		return nil, mi.s.m.failf("Cannot execute SQL '%s': %s", query, err)
	}
	return dictRows(res), nil
}

// mysqlInfoModule ports ansible.mysql.mysql_info.
func mysqlInfoModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mysqlInfoSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	m := newMySQLModule(env, p, "ansible.mysql.mysql_info")
	var filter []string
	for _, f := range p.List("filter") {
		filter = append(filter, strings.TrimSpace(pyStrValue(f)))
	}
	exclude := map[string]bool{}
	for _, f := range p.List("exclude_fields") {
		exclude[strings.TrimSpace(pyStrValue(f))] = true
	}
	conn, err := m.connect(connectOpts{user: m.optStr("login_user"), password: m.optStr("login_password"), db: p.Str("login_db")})
	if err != nil {
		return m.failf("unable to connect to database using %s %s, check login_user "+
			"and login_password are correct or %s has the credentials. "+
			"Exception message: %s", mysqlclient.ConnectorName, mysqlclient.ConnectorVersion, m.configFile(), err)
	}
	defer conn.Close()
	if _, err := m.serverImplementation(conn); err != nil {
		return m.failf("%s", err)
	}
	s, err := newMySQLSession(m, conn)
	if err != nil {
		return m.failf("%s", err)
	}
	s.userImpl()
	mi := &mysqlInfo{s: s, info: map[string]map[string]any{}}
	for _, k := range mysqlInfoSubsets {
		mi.info[k] = map[string]any{}
	}

	valid := map[string]bool{}
	for _, k := range mysqlInfoSubsets {
		valid[k] = true
	}
	wanted := map[string]bool{}
	var inc, exc []string
	if len(filter) > 0 {
		for _, f := range filter {
			if !valid[strings.TrimLeft(f, "!")] {
				m.warn(fmt.Sprintf("filter element: %s is not allowable, ignored", f))
				continue
			}
			if strings.HasPrefix(f, "!") {
				exc = append(exc, strings.TrimLeft(f, "!"))
			} else {
				inc = append(inc, f)
			}
		}
		if len(inc) > 0 {
			for _, f := range inc {
				wanted[f] = true
			}
		} else {
			for _, k := range mysqlInfoSubsets {
				if !containsStr(exc, k) {
					wanted[k] = true
				}
			}
		}
	} else {
		for _, k := range mysqlInfoSubsets {
			wanted[k] = true
		}
	}
	if fail := mi.collect(wanted, exclude, p.Bool("return_empty_dbs")); fail != nil {
		return fail
	}
	out := map[string]any{
		"connector_name":    mysqlclient.ConnectorName,
		"connector_version": mysqlclient.ConnectorVersion,
		"server_engine":     "MySQL",
	}
	if s.isMaria() {
		out["server_engine"] = "MariaDB"
	}
	for _, k := range mysqlInfoSubsets {
		if len(filter) == 0 || (len(inc) > 0 && containsStr(inc, k)) || (len(inc) == 0 && !containsStr(exc, k)) {
			if k == "users_info" {
				if l, ok := mi.info[k]["\x00list"]; ok {
					out[k] = l
					continue
				}
			}
			out[k] = mi.info[k]
		}
	}
	return m.exit(false, out)
}

func (mi *mysqlInfo) collect(wanted, exclude map[string]bool, returnEmptyDBs bool) *agentproto.Result {
	if wanted["version"] || wanted["settings"] {
		rows, fail := mi.execSQL("SHOW GLOBAL VARIABLES")
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			mi.info["settings"][cellString(r["Variable_name"])] = mysqlInfoConvert(r["Value"])
		}
		if len(rows) > 0 {
			full := pyStrValue(mi.info["settings"]["version"])
			parts := strings.Split(full, ".")
			if len(parts) >= 3 {
				rel := strings.SplitN(parts[2], "-", 2)
				suffix := ""
				if len(rel) > 1 {
					suffix = rel[1]
				}
				major, _ := strconv.ParseInt(parts[0], 10, 64)
				minor, _ := strconv.ParseInt(parts[1], 10, 64)
				release, _ := strconv.ParseInt(rel[0], 10, 64)
				mi.info["version"] = map[string]any{"major": major, "minor": minor, "release": release, "suffix": suffix, "full": full}
			}
		}
	}
	if wanted["databases"] {
		included := func(f string) bool { return len(exclude) == 0 || !exclude["db_"+f] }
		parts := []string{`SELECT table_schema AS "name"`}
		if included("size") {
			parts = append(parts, `SUM(data_length + index_length) AS "size"`)
		}
		if included("table_count") {
			parts = append(parts, `COUNT(table_name) as "tables"`)
		}
		rows, fail := mi.execSQL(strings.Join(parts, ", ") + " FROM information_schema.TABLES GROUP BY table_schema")
		if fail != nil {
			return fail
		}
		dbInfo := func(r map[string]any) map[string]any {
			out := map[string]any{}
			num := func(v any) int64 {
				switch t := v.(type) {
				case int64:
					return t
				case mysqlclient.Decimal:
					f, _ := strconv.ParseFloat(string(t), 64)
					return int64(f)
				case float64:
					return int64(t)
				}
				return 0
			}
			if included("size") {
				out["size"] = num(r["size"])
			}
			if included("table_count") {
				out["tables"] = num(r["tables"])
			}
			return out
		}
		for _, r := range rows {
			mi.info["databases"][cellString(r["name"])] = dbInfo(r)
		}
		if returnEmptyDBs {
			rows, fail := mi.execSQL("SHOW DATABASES")
			if fail != nil {
				return fail
			}
			for _, r := range rows {
				name := cellString(r["Database"])
				if _, ok := mi.info["databases"][name]; !ok {
					mi.info["databases"][name] = dbInfo(map[string]any{})
				}
			}
		}
	}
	if wanted["global_status"] {
		rows, fail := mi.execSQL("SHOW GLOBAL STATUS")
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			mi.info["global_status"][cellString(r["Variable_name"])] = mysqlInfoConvert(r["Value"])
		}
	}
	if wanted["engines"] {
		rows, fail := mi.execSQL("SHOW ENGINES")
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			e := map[string]any{}
			for k, v := range r {
				if k != "Engine" {
					e[k] = mysqlInfoJSON(v)
				}
			}
			mi.info["engines"][cellString(r["Engine"])] = e
		}
	}
	if wanted["users"] {
		rows, fail := mi.execSQL("SELECT * FROM mysql.user")
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			host, user := cellString(r["Host"]), cellString(r["User"])
			hm, _ := mi.info["users"][host].(map[string]any)
			if hm == nil {
				hm = map[string]any{}
				mi.info["users"][host] = hm
			}
			u := map[string]any{}
			for k, v := range r {
				if k != "Host" && k != "User" {
					u[k] = mysqlInfoConvert(v)
				}
			}
			hm[user] = u
		}
	}
	if wanted["users_info"] {
		if fail := mi.usersInfo(); fail != nil {
			return fail
		}
	}
	if wanted["master_status"] {
		rows, fail := mi.execSQL(mi.s.mysqlCommand("SHOW MASTER STATUS"))
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			for k, v := range r {
				mi.info["master_status"][k] = mysqlInfoConvert(v)
			}
		}
	}
	if wanted["slave_status"] {
		rows, fail := mi.execSQL(mi.s.mysqlCommand("SHOW SLAVE STATUS"))
		if fail != nil {
			return fail
		}
		pick := func(r map[string]any, a, b string) string {
			if v := r[a]; pyTruthy(mysqlInfoJSON(v)) {
				return pyStrValue(mysqlInfoJSON(v))
			}
			return pyStrValue(mysqlInfoJSON(r[b]))
		}
		for _, r := range rows {
			host := pick(r, "Master_Host", "Source_Host")
			port := pick(r, "Master_Port", "Source_Port")
			user := pick(r, "Master_User", "Source_User")
			hm, _ := mi.info["slave_status"][host].(map[string]any)
			if hm == nil {
				hm = map[string]any{}
				mi.info["slave_status"][host] = hm
			}
			pm, _ := hm[port].(map[string]any)
			if pm == nil {
				pm = map[string]any{}
				hm[port] = pm
			}
			um, _ := pm[user].(map[string]any)
			if um == nil {
				um = map[string]any{}
				pm[user] = um
			}
			for k, v := range r {
				switch k {
				case "Master_Host", "Master_Port", "Master_User", "Source_Host", "Source_Port", "Source_User":
					continue
				}
				um[k] = mysqlInfoConvert(v)
			}
		}
	}
	if wanted["slave_hosts"] {
		rows, fail := mi.execSQL(mi.s.mysqlCommand("SHOW SLAVE HOSTS"))
		if fail != nil {
			return fail
		}
		for _, r := range rows {
			id := pyStrValue(mysqlInfoJSON(r["Server_id"]))
			hm, _ := mi.info["slave_hosts"][id].(map[string]any)
			if hm == nil {
				hm = map[string]any{}
				mi.info["slave_hosts"][id] = hm
			}
			for k, v := range r {
				if k != "Server_id" {
					hm[k] = mysqlInfoConvert(v)
				}
			}
		}
	}
	return nil
}

// usersInfo is __get_users_info(): per account, the priv string, limits,
// TLS requirements and authentication mysql_user would recreate it from.
func (mi *mysqlInfo) usersInfo() *agentproto.Result {
	s := mi.s
	m := s.m
	res, err := s.c.Query("SELECT * FROM mysql.user")
	if err != nil {
		return m.failf("Cannot execute SQL '%s': %s", "SELECT * FROM mysql.user", err)
	}
	rows := dictRows(res)
	if len(rows) == 0 {
		return nil
	}
	output := []any{}
	for _, line := range rows {
		user, host := cellString(line["User"]), cellString(line["Host"])
		isRole := s.isMaria() && host == ""
		userPriv, err := s.privilegesGet(user, host, isRole)
		if err != nil {
			return m.failf("%s", err)
		}
		if len(userPriv.keys) == 0 {
			m.warn(fmt.Sprintf("No privileges found for %s on host %s", user, host))
			continue
		}
		var privString []string
		for _, dbTable := range userPriv.keys {
			priv := userPriv.m[dbTable]
			set := map[string]bool{}
			for _, x := range priv {
				set[x] = true
			}
			if (len(set) == 2 && set["PROXY"] && set["GRANT"]) || (len(set) == 1 && set["PROXY"]) {
				continue
			}
			unquoted := strings.NewReplacer("`", "", "'", "").Replace(dbTable)
			privString = append(privString, unquoted+":"+strings.Join(priv, ","))
		}
		if len(privString) > 1 {
			for i, x := range privString {
				if x == "*.*:USAGE" {
					privString = append(privString[:i], privString[i+1:]...)
					break
				}
			}
		}
		limits, _, err := s.getResourceLimits(user, host)
		if err != nil {
			return m.failf("%s", err)
		}
		tls, err := s.getTLSRequires(user, host)
		if err != nil {
			return m.failf("%s", err)
		}
		od := map[string]any{"name": user, "host": host, "priv": strings.Join(privString, "/")}
		rl := map[string]any{}
		for k, v := range limits {
			if !isZeroLimit(v) {
				rl[k] = v
			}
		}
		if len(rl) > 0 {
			od["resource_limits"] = rl
		}
		if len(tls) > 0 {
			od["tls_requires"] = tls
		}
		auths, err := s.getExistingAuthentication(user, host)
		if err != nil {
			return m.failf("%s", err)
		}
		if len(auths) > 0 {
			od["plugin"] = auths[0]["plugin"]
			od["plugin_hash_string"] = auths[0]["plugin_hash_string"]
		}
		if r, ok := line["is_role"]; ok && cellString(r) == "N" {
			locked, err := s.userIsLocked(user, host)
			if err != nil {
				return m.failf("%s", err)
			}
			od["locked"] = locked
		}
		output = append(output, od)
	}
	mi.info["users_info"]["\x00list"] = output
	return nil
}

func isZeroLimit(v any) bool {
	switch t := v.(type) {
	case int64:
		return t == 0
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return err == nil && f == 0
	case float64:
		return t == 0
	}
	return false
}
