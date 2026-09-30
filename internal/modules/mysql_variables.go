package modules

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

func init() {
	registerMySQL(mysqlVariablesModule, "mysql_variables")
}

var mysqlVariablesSpec = mysqlCommonSpec(args.Spec{
	"variable": {Required: true},
	"value":    {},
	"mode":     {Default: "global", Choices: []string{"global", "persist", "persist_only"}},
})

var mysqlVarNameRe = regexp.MustCompile(`^[0-9A-Za-z_.]+$`)

// pyTypedValue is the module's typedvalue(): int(value), else
// float(value), else the string itself.
func pyTypedValue(v string) any {
	s := strings.TrimSpace(v)
	if n, ok := pyIntLiteral(s); ok {
		return n
	}
	if f, ok := pyFloatLiteral(s); ok {
		return f
	}
	return v
}

// pyIntLiteral is int(str): optional sign, digits with single
// underscores between them.
func pyIntLiteral(s string) (int64, bool) {
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 || body == "" || body[0] == '_' || body[len(body)-1] == '_' || strings.Contains(body, "__") {
		return 0, false
	}
	for _, c := range body {
		if (c < '0' || c > '9') && c != '_' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	return n, err == nil
}

// pyFloatLiteral is float(str).
func pyFloatLiteral(s string) (float64, bool) {
	low := strings.ToLower(strings.TrimLeft(s, "+-"))
	switch low {
	case "inf", "infinity", "nan":
		f, _ := strconv.ParseFloat(strings.Replace(strings.ToLower(s), "infinity", "inf", 1), 64)
		if low == "nan" {
			f = math.NaN()
		}
		return f, true
	}
	if strings.ContainsAny(low, "xp") || low == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// pyEqual is Python == between typedvalue results.
func pyEqual(a, b any) bool {
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case float64:
			return float64(x) == y
		}
	case float64:
		switch y := b.(type) {
		case int64:
			return x == float64(y)
		case float64:
			return x == y
		}
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

// mysqlVariablesModule ports ansible.mysql.mysql_variables.
func mysqlVariablesModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if v, ok := rawArgs["value"].(bool); ok {
		// type='str' converts with str(): True, not yes.
		rawArgs = copyArgs(rawArgs)
		rawArgs["value"] = pyValueRepr(v)
	}
	p, err := mysqlVariablesSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (mysql_variables) does not support check mode"}
	}
	m := newMySQLModule(env, p, "ansible.mysql.mysql_variables")
	mysqlvar := p.Str("variable")
	mode := p.Str("mode")
	if !mysqlVarNameRe.MatchString(mysqlvar) {
		return m.failf("invalid variable name \"%s\"", mysqlvar)
	}
	configFile := m.configFile()
	conn, err := m.connect(connectOpts{user: m.optStr("login_user"), password: m.optStr("login_password"), db: "mysql"})
	if err != nil {
		if fileExists(configFile) {
			return m.failf("unable to connect to database, check login_user and "+
				"login_password are correct or %s has the credentials. "+
				"Exception message: %s", configFile, err)
		}
		return m.failf("unable to find %s. Exception message: %s", configFile, err)
	}
	defer conn.Close()
	if _, err := m.serverImplementation(conn); err != nil {
		return m.failf("%s", err)
	}

	res, _, err := conn.Exec("SHOW VARIABLES WHERE Variable_name = %s", []any{mysqlvar})
	if err != nil {
		return m.failf("%s", err)
	}
	var current any
	if len(res.Rows) == 1 {
		current = res.Fields[1].Convert(res.Rows[0][1])
	}
	if current == nil {
		return m.fail("Variable not available \""+mysqlvar+"\"", map[string]any{"changed": false})
	}
	if !p.Has("value") {
		return m.exit(false, map[string]any{"msg": cellString(current)})
	}

	var inAutoCnf any
	actualRaw := current
	if mode == "persist" || mode == "persist_only" {
		res, _, err := conn.Exec("SELECT VARIABLE_VALUE FROM performance_schema.persisted_variables WHERE VARIABLE_NAME = %s", []any{mysqlvar})
		if err != nil {
			if strings.Contains(err.Error(), "Table 'performance_schema.persisted_variables' doesn't exist") {
				return m.failf("Server version must be 8.0 or greater.")
			}
			return m.failf("local variable 'res' referenced before assignment")
		}
		if row := fetchone(res); row != nil {
			inAutoCnf = row[0]
		}
		if mode == "persist_only" {
			if inAutoCnf == nil {
				actualRaw = false
			} else {
				actualRaw = inAutoCnf
			}
		}
	}
	valueWanted := pyTypedValue(p.Str("value"))
	var valueActual any = actualRaw
	if s, ok := actualRaw.(string); ok {
		valueActual = pyTypedValue(s)
	}
	if s, ok := valueActual.(string); ok && (s == "ON" || s == "OFF") {
		if w, ok := valueWanted.(string); !ok || (w != "ON" && w != "OFF") {
			switch {
			case pyEqual(valueWanted, "on") || pyEqual(valueWanted, int64(1)):
				valueWanted = "ON"
			case pyEqual(valueWanted, "off") || pyEqual(valueWanted, int64(0)):
				valueWanted = "OFF"
			}
		}
	}
	var valueInAutoCnf any
	if inAutoCnf != nil {
		valueInAutoCnf = pyTypedValue(cellString(inAutoCnf))
	}
	if pyEqual(valueWanted, valueActual) && (mode == "global" || mode == "persist") {
		if mode == "persist" && valueInAutoCnf != nil && pyEqual(valueWanted, valueInAutoCnf) {
			return m.exit(false, map[string]any{"msg": "Variable is already set to requested value globally" +
				"and stored into mysqld-auto.cnf file."})
		} else if mode == "global" {
			return m.exit(false, map[string]any{"msg": "Variable is already set to requested value."})
		}
	}
	if mode == "persist_only" && valueInAutoCnf != nil && pyEqual(valueWanted, valueInAutoCnf) {
		return m.exit(false, map[string]any{"msg": "Variable is already stored into mysqld-auto.cnf " +
			"with requested value."})
	}

	var query string
	switch mode {
	case "persist":
		query = "SET PERSIST "
	case "persist_only":
		query = "SET PERSIST_ONLY "
	default:
		query = "SET GLOBAL "
	}
	quoted, err := mysqlQuoteIdentifier(mysqlvar, "vars")
	if err != nil {
		return m.fail(err.Error(), map[string]any{"changed": false})
	}
	query += quoted + " = "
	if _, _, err := conn.Exec(query+"%s", []any{valueWanted}); err != nil {
		return m.fail(err.Error(), map[string]any{"changed": false})
	}
	return m.exit(true, map[string]any{
		"msg":     "Variable change succeeded prev_value=" + pyStrValue(valueActual),
		"queries": []any{query + pyStrValue(valueWanted)},
	})
}

func copyArgs(a map[string]any) map[string]any {
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

var _ = mysqlclient.PyRepr
