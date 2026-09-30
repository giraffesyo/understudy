package modules

import (
	"math"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

func init() {
	registerMySQL(mysqlQueryModule, "mysql_query")
}

var mysqlQuerySpec = mysqlCommonSpec(args.Spec{
	"query":              {Type: "any", Required: true},
	"login_db":           {},
	"positional_args":    {Type: "list"},
	"named_args":         {Type: "dict"},
	"single_transaction": {Type: "bool", Default: false},
	"session_vars":       {Type: "dict"},
})

// dictRows is DictCursor.fetchall(): one map per row, keyed by column
// name (a repeated name is qualified with its table, as PyMySQL does).
func dictRows(res *mysqlclient.Result) []map[string]any {
	if res == nil || res.Fields == nil {
		return nil
	}
	names := make([]string, len(res.Fields))
	seen := map[string]bool{}
	for i, f := range res.Fields {
		name := f.Name
		if seen[name] {
			name = f.Table + "." + name
		}
		seen[name] = true
		names[i] = name
	}
	out := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := make(map[string]any, len(names))
		for i, f := range res.Fields {
			row[names[i]] = f.Convert(r[i])
		}
		out = append(out, row)
	}
	return out
}

var (
	mysqlDMLKeywords = []string{"INSERT", "UPDATE", "DELETE", "REPLACE"}
	mysqlDDLKeywords = []string{"CREATE", "DROP", "ALTER", "RENAME", "TRUNCATE"}
)

// mysqlQueryModule ports ansible.mysql.mysql_query.
func mysqlQueryModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if err := mysqlQuerySpec.MutuallyExclusive(rawArgs, []string{"positional_args", "named_args"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := mysqlQuerySpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (mysql_query) does not support check mode"}
	}
	m := newMySQLModule(env, p, "ansible.mysql.mysql_query")
	var queries []string
	switch q := p.Any("query").(type) {
	case string:
		queries = []string{q}
	case []any:
		for _, e := range q {
			s, ok := e.(string)
			if !ok {
				return m.failf("the elements in query list must be strings, passed '%s' %s", pyStrValue(e), pyTypeRepr(e))
			}
			queries = append(queries, s)
		}
	default:
		return m.failf("the query option value must be a string or list, passed %s", pyTypeRepr(q))
	}
	autocommit := !p.Bool("single_transaction")
	var arguments any
	if l := p.List("positional_args"); len(l) > 0 {
		arguments = l
	} else if d := p.Dict("named_args"); len(d) > 0 {
		arguments = d
	}

	conn, err := m.connect(connectOpts{user: m.optStr("login_user"), password: m.optStr("login_password"),
		db: p.Str("login_db"), autocommit: autocommit})
	if err != nil {
		return m.failf("unable to connect to database, check login_user and "+
			"login_password are correct or %s has the credentials. "+
			"Exception message: %s", m.configFile(), err)
	}
	defer conn.Close()
	if _, err := m.serverImplementation(conn); err != nil {
		return m.failf("%s", err)
	}
	if vars := p.Dict("session_vars"); len(vars) > 0 {
		if r := m.setSessionVars(conn, vars); r != nil {
			return r
		}
	}

	changed := false
	queryResult := []any{}
	executed := []any{}
	rowcount := []any{}
	timings := []any{}
	for _, q := range queries {
		start := time.Now()
		res, sent, err := conn.Exec(q, arguments)
		if err != nil {
			if !autocommit {
				conn.Rollback()
			}
			return m.failf("Cannot execute SQL '%s' args [%s]: %s", q, pyValueRepr(arguments), err)
		}
		ms := float64(time.Since(start).Nanoseconds()) / 1e6
		timings = append(timings, math.Round(ms*1e4)/1e4)
		rows := []any{}
		for _, r := range dictRows(res) {
			row := make(map[string]any, len(r))
			for k, v := range r {
				row[k] = mysqlJSONValue(v)
			}
			rows = append(rows, row)
		}
		queryResult = append(queryResult, rows)

		head := strings.TrimLeft(q, " \t\n\r\f\v")
		if len(head) > 8 {
			head = head[:8]
		}
		head = strings.ToUpper(head)
		for _, k := range mysqlDMLKeywords {
			if strings.Contains(head, k) && res.RowCount() > 0 {
				changed = true
			}
		}
		for _, k := range mysqlDDLKeywords {
			if strings.Contains(head, k) {
				changed = true
			}
		}
		executed = append(executed, sent)
		rowcount = append(rowcount, res.RowCount())
	}
	if !autocommit {
		if err := conn.Commit(); err != nil {
			return m.failf("%s", err)
		}
	}
	return m.exit(changed, map[string]any{
		"executed_queries":  executed,
		"query_result":      queryResult,
		"rowcount":          rowcount,
		"execution_time_ms": timings,
	})
}
