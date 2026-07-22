package modules

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(mysqlDBModule, "mysql_db", "community.mysql.mysql_db")
	Register(mysqlUserModule, "mysql_user", "community.mysql.mysql_user")
}

// mysql modules shell out to the `mysql` client rather than embedding a
// wire-protocol driver — a MySQL Go driver would be a third-party
// dependency, and hosts running these tasks already have the client.

func mysqlLoginArgs(p *args.Parsed) []string {
	var argv []string
	if u := p.Str("login_user"); u != "" {
		argv = append(argv, "-u", u)
	}
	if pw := p.Str("login_password"); pw != "" {
		argv = append(argv, "-p"+pw)
	}
	if h := p.Str("login_host"); h != "" {
		argv = append(argv, "-h", h)
	}
	if s := p.Str("login_unix_socket"); s != "" {
		argv = append(argv, "-S", s)
	}
	return argv
}

func mysqlQuery(env *RunEnv, p *args.Parsed, sql string) (string, error) {
	argv := append(mysqlLoginArgs(p), "-N", "-B", "-e", sql)
	return runOut(env, "mysql", argv...)
}

var mysqlDBSpec = args.Spec{
	"name":              {Required: true, Aliases: []string{"db"}},
	"state":             {Default: "present", Choices: []string{"present", "absent", "dump", "import"}},
	"encoding":          {},
	"collation":         {},
	"login_user":        {},
	"login_password":    {},
	"login_host":        {},
	"login_unix_socket": {},
}

func mysqlDBModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mysqlDBSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	state := p.Str("state")
	if state == "dump" || state == "import" {
		return agentproto.Fail("mysql_db state=%s is not supported yet", state)
	}
	res := &agentproto.Result{Extra: map[string]any{"db": name}}

	out, err := mysqlQuery(env, p, "SHOW DATABASES LIKE '"+escapeSQL(name)+"'")
	if err != nil {
		return agentproto.Fail("mysql connection failed: %v: %s", err, tail(out))
	}
	exists := strings.TrimSpace(out) != ""

	if state == "absent" {
		if !exists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if out, err := mysqlQuery(env, p, "DROP DATABASE `"+name+"`"); err != nil {
			return agentproto.Fail("DROP DATABASE failed: %v: %s", err, tail(out))
		}
		return res
	}

	if exists {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	stmt := "CREATE DATABASE `" + name + "`"
	if enc := p.Str("encoding"); enc != "" {
		stmt += " CHARACTER SET " + enc
	}
	if col := p.Str("collation"); col != "" {
		stmt += " COLLATE " + col
	}
	if out, err := mysqlQuery(env, p, stmt); err != nil {
		return agentproto.Fail("CREATE DATABASE failed: %v: %s", err, tail(out))
	}
	return res
}

var mysqlUserSpec = args.Spec{
	"name":              {Required: true, Aliases: []string{"user"}},
	"password":          {},
	"host":              {Default: "localhost"},
	"priv":              {}, // "db.table:PRIV1,PRIV2/db2.*:ALL"
	"state":             {Default: "present", Choices: []string{"present", "absent"}},
	"append_privs":      {Type: "bool", Default: false},
	"login_user":        {},
	"login_password":    {},
	"login_host":        {},
	"login_unix_socket": {},
}

func mysqlUserModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mysqlUserSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	host := p.Str("host")
	res := &agentproto.Result{Extra: map[string]any{"user": name}}

	out, err := mysqlQuery(env, p,
		fmt.Sprintf("SELECT 1 FROM mysql.user WHERE User='%s' AND Host='%s'",
			escapeSQL(name), escapeSQL(host)))
	if err != nil {
		return agentproto.Fail("mysql connection failed: %v: %s", err, tail(out))
	}
	exists := strings.TrimSpace(out) != ""

	if p.Str("state") == "absent" {
		if !exists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if out, err := mysqlQuery(env, p, fmt.Sprintf("DROP USER '%s'@'%s'", name, host)); err != nil {
			return agentproto.Fail("DROP USER failed: %v: %s", err, tail(out))
		}
		return res
	}

	// v0.1 keeps this simple: create the user (with password) if missing,
	// then apply privileges. Password diffing needs mysql.user hash access;
	// we only set the password on creation. Documented limitation.
	if !exists {
		res.Changed = true
		if !env.CheckMode {
			stmt := fmt.Sprintf("CREATE USER '%s'@'%s'", name, host)
			if pw := p.Str("password"); pw != "" {
				stmt += fmt.Sprintf(" IDENTIFIED BY '%s'", escapeSQL(pw))
			}
			if out, err := mysqlQuery(env, p, stmt); err != nil {
				return agentproto.Fail("CREATE USER failed: %v: %s", err, tail(out))
			}
		}
	}

	if priv := p.Str("priv"); priv != "" && !env.CheckMode {
		if err := applyMySQLPrivs(env, p, name, host, priv); err != nil {
			return agentproto.Fail("%v", err)
		}
		res.Changed = true // grant application is not diffed in v0.1
	}
	return res
}

func applyMySQLPrivs(env *RunEnv, p *args.Parsed, name, host, priv string) error {
	for _, spec := range strings.Split(priv, "/") {
		dbTable, privs, ok := strings.Cut(spec, ":")
		if !ok {
			return fmt.Errorf("invalid priv spec %q (want db.table:PRIVS)", spec)
		}
		grant := fmt.Sprintf("GRANT %s ON %s TO '%s'@'%s'",
			privs, normalizeDBTable(dbTable), name, host)
		if out, err := mysqlQuery(env, p, grant); err != nil {
			return fmt.Errorf("GRANT failed: %v: %s", err, tail(out))
		}
	}
	_, _ = mysqlQuery(env, p, "FLUSH PRIVILEGES")
	return nil
}

// normalizeDBTable turns "db.table" into `db`.`table`, leaving wildcards.
func normalizeDBTable(s string) string {
	db, table, ok := strings.Cut(s, ".")
	quote := func(x string) string {
		if x == "*" {
			return "*"
		}
		return "`" + x + "`"
	}
	if !ok {
		return quote(db) + ".*"
	}
	return quote(db) + "." + quote(table)
}

func escapeSQL(s string) string {
	return strings.NewReplacer("'", "''", "\\", "\\\\").Replace(s)
}
