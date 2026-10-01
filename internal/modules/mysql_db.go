package modules

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

func init() {
	registerMySQL(mysqlDBModule, "mysql_db")
}

var mysqlDBSpec = mysqlCommonSpec(args.Spec{
	"name":                      {Type: "list", Required: true, Aliases: []string{"db"}},
	"encoding":                  {Default: ""},
	"collation":                 {Default: ""},
	"target":                    {},
	"state":                     {Default: "present", Choices: []string{"absent", "dump", "import", "present"}},
	"single_transaction":        {Type: "bool", Default: false},
	"quick":                     {Type: "bool", Default: true},
	"ignore_tables":             {Type: "list", Default: []any{}},
	"hex_blob":                  {Type: "bool", Default: false},
	"force":                     {Type: "bool", Default: false},
	"master_data":               {Type: "int", Default: 0},
	"skip_lock_tables":          {Type: "bool", Default: false},
	"dump_extra_args":           {},
	"use_shell":                 {Type: "bool", Default: false},
	"unsafe_login_password":     {Type: "bool", Default: false},
	"restrict_config_file":      {Type: "bool", Default: false},
	"check_implicit_admin":      {Type: "bool", Default: false},
	"config_overrides_defaults": {Type: "bool", Default: false},
	"chdir":                     {},
	"pipefail":                  {Type: "bool", Default: true},
	"sql_log_bin":               {Type: "bool", Default: true},
})

// mysqlDBModule ports ansible.mysql.mysql_db.
func mysqlDBModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mysqlDBSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	m := newMySQLModule(env, p, "ansible.mysql.mysql_db")
	if fail := m.driverMissing(); fail != nil {
		return fail
	}
	if md := p.Int("master_data"); md < 0 || md > 2 {
		return m.failf("value of master_data must be one of: 0, 1, 2, got: %d", md)
	}
	var db []string
	for _, v := range p.List("name") {
		db = append(db, pyStrValue(v))
	}
	if len(db) == 0 {
		return m.exit(false, map[string]any{"db": []any{}, "db_list": []any{}})
	}
	for i := range db {
		db[i] = strings.TrimSpace(db[i])
	}
	dbList := make([]any, len(db))
	for i, d := range db {
		dbList[i] = d
	}

	encoding := p.Str("encoding")
	collation := p.Str("collation")
	state := p.Str("state")
	target := pyExpandPath(p.Str("target"))
	loginPort := p.Int("login_port")
	if loginPort < 0 || loginPort > 65535 {
		return m.failf("login_port must be a valid unix port number (0-65535)")
	}
	for _, t := range p.List("ignore_tables") {
		if pyStrValue(t) == "" {
			return m.failf("Name of ignored table cannot be empty")
		}
	}
	chdir := pyExpandPath(p.Str("chdir"))
	if chdir != "" {
		if info, err := os.Stat(chdir); err != nil {
			return m.failf("Cannot change the current directory to %s: %s", chdir, pyStrOSError(err, chdir))
		} else if !info.IsDir() {
			return m.failf("Cannot change the current directory to %s: [Errno 20] Not a directory: %s", chdir, pyStrRepr(chdir))
		}
		if target != "" && !filepath.IsAbs(target) {
			target = filepath.Join(chdir, target)
		}
	}
	if len(db) > 1 && state == "import" {
		return m.failf("Multiple databases are not supported with state=import")
	}
	dbName := strings.Join(db, " ")

	allDatabases := false
	if state == "dump" || state == "import" {
		if !p.Has("target") {
			return m.failf("with state=%s target is required", state)
		}
		if len(db) == 1 && db[0] == "all" {
			allDatabases = true
		}
	} else if len(db) == 1 && db[0] == "all" {
		return m.failf("name is not allowed to equal 'all' unless state equals import, or dump.")
	}

	configFile := m.configFile()
	checkImplicitAdmin := p.Bool("check_implicit_admin")
	overrides := p.Bool("config_overrides_defaults")
	var conn *mysqlclient.Conn
	if checkImplicitAdmin {
		conn, err = m.connect(connectOpts{user: strPtr("root"), password: strPtr(""), overridesDefaults: overrides})
		var fatal mysqlFatal
		if errors.As(err, &fatal) {
			return m.failf("%s", fatal.msg)
		}
		if err != nil {
			checkImplicitAdmin = false
		}
	}
	if conn == nil {
		conn, err = m.connect(connectOpts{user: m.optStr("login_user"), password: m.optStr("login_password"), overridesDefaults: overrides})
		var fatal mysqlFatal
		if errors.As(err, &fatal) {
			return m.failf("%s", fatal.msg)
		}
		if err != nil {
			if fileExists(configFile) {
				return m.failf("unable to connect to database, check login_user and login_password are correct or %s has the credentials. "+
					"Exception message: %s", configFile, err)
			}
			return m.failf("unable to find %s. Exception message: %s", configFile, err)
		}
	}
	defer conn.Close()

	if (state == "absent" || state == "present") && !p.Bool("sql_log_bin") {
		if _, err := conn.Query("SET SQL_LOG_BIN=0;"); err != nil {
			return m.failf("%s", err)
		}
	}
	impl, err := m.serverImplementation(conn)
	if err != nil {
		return m.failf("%s", err)
	}
	version, err := mysqlServerVersion(conn)
	if err != nil {
		return m.failf("%s", err)
	}
	if !fileExists(configFile) {
		configFile = ""
	}

	var existing, missing []string
	if !allDatabases {
		for _, each := range db {
			res, _, err := conn.Exec("SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = %s", []any{each})
			if err != nil {
				return m.failf("%s", err)
			}
			if res.RowCount() == 1 {
				existing = append(existing, each)
			} else {
				missing = append(missing, each)
			}
		}
	}
	var executed []any
	out := func(changed bool, extra map[string]any) *agentproto.Result {
		kv := map[string]any{"db": dbName, "db_list": dbList}
		for k, v := range extra {
			kv[k] = v
		}
		return m.exit(changed, kv)
	}

	switch state {
	case "absent":
		if env.CheckMode {
			return out(len(existing) > 0, nil)
		}
		for _, each := range existing {
			q, err := mysqlQuoteIdentifier(each, "database")
			if err != nil {
				return m.failf("error deleting database: %s", err)
			}
			query := "DROP DATABASE " + q
			executed = append(executed, query)
			if _, err := conn.Query(query); err != nil {
				return m.failf("error deleting database: %s", err)
			}
		}
		return out(len(existing) > 0, map[string]any{"executed_commands": nonNil(executed)})
	case "present":
		if env.CheckMode {
			return out(len(missing) > 0, nil)
		}
		changed := false
		if len(missing) > 0 {
			c, cmds, err := mysqlDBCreate(conn, missing, encoding, collation)
			executed = append(executed, cmds...)
			if err != nil {
				return m.failf("error creating database: %s", err)
			}
			changed = c
		}
		return out(changed, map[string]any{"executed_commands": nonNil(executed)})
	case "dump":
		if len(missing) > 0 && !allDatabases {
			return m.failf("Cannot dump database(s) %s - not found", mysqlclient.PyRepr(strings.Join(missing, ", ")))
		}
		if env.CheckMode {
			return out(true, nil)
		}
		rc, stdout, stderr, cmds, fail := m.dbDump(db, target, allDatabases, configFile, impl, version, chdir, checkImplicitAdmin)
		if fail != nil {
			return fail
		}
		executed = append(executed, cmds...)
		if rc != 0 {
			return m.failf("%s", stderr)
		}
		return out(true, map[string]any{"msg": stdout, "executed_commands": nonNil(executed)})
	default: // import
		if env.CheckMode {
			return out(true, nil)
		}
		if len(missing) > 0 && !allDatabases {
			_, cmds, err := mysqlDBCreate(conn, missing, encoding, collation)
			executed = append(executed, cmds...)
			if err != nil {
				return m.failf("error creating database: %s", err)
			}
		}
		rc, stdout, stderr, cmds, fail := m.dbImport(db, target, allDatabases, configFile, impl, version, chdir, checkImplicitAdmin)
		if fail != nil {
			return fail
		}
		executed = append(executed, cmds...)
		if rc != 0 {
			return m.failf("%s", stderr)
		}
		return out(true, map[string]any{"msg": stdout, "executed_commands": nonNil(executed)})
	}
}

func nonNil(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

// mysqlDBCreate is db_create(): CREATE DATABASE with the optional
// character set and collation bound as parameters; the executed
// statements are returned mogrified.
func mysqlDBCreate(conn *mysqlclient.Conn, dbs []string, encoding, collation string) (bool, []any, error) {
	params := map[string]any{"enc": encoding, "collate": collation}
	var cmds []any
	n := int64(0)
	for _, each := range dbs {
		q, err := mysqlQuoteIdentifier(each, "database")
		if err != nil {
			return false, cmds, err
		}
		query := "CREATE DATABASE " + strings.ReplaceAll(q, "%", "%%")
		if encoding != "" {
			query += " CHARACTER SET %(enc)s"
		}
		if collation != "" {
			query += " COLLATE %(collate)s"
		}
		res, executed, err := conn.Exec(query, params)
		if err != nil {
			return false, cmds, err
		}
		n += res.RowCount()
		cmds = append(cmds, executed)
	}
	return n > 0, cmds, nil
}

var shlexUnsafe = regexp.MustCompile(`[^\w@%+=:,./-]`)

// shlexQuote is Python's shlex.quote.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shlexUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (m *mysqlModule) clientCredentialArgs(configFile string, checkImplicitAdmin, dump bool) []string {
	p := m.p
	var cmd []string
	if configFile != "" {
		if p.Bool("restrict_config_file") {
			cmd = append(cmd, "--defaults-file="+shlexQuote(configFile))
		} else {
			cmd = append(cmd, "--defaults-extra-file="+shlexQuote(configFile))
		}
	}
	if checkImplicitAdmin {
		cmd = append(cmd, "--user=root --password=''")
	} else {
		user, password := p.Str("login_user"), p.Str("login_password")
		// dump tests "is not None", import tests truthiness.
		if dump && p.Has("login_user") || !dump && user != "" {
			cmd = append(cmd, "--user="+shlexQuote(user))
		}
		if dump && p.Has("login_password") || !dump && password != "" {
			if p.Bool("unsafe_login_password") {
				cmd = append(cmd, "--password="+password)
			} else {
				cmd = append(cmd, "--password="+shlexQuote(password))
			}
		}
	}
	if p.Has("client_cert") {
		cmd = append(cmd, "--ssl-cert="+shlexQuote(pyExpandPath(p.Str("client_cert"))))
	}
	if p.Has("client_key") {
		cmd = append(cmd, "--ssl-key="+shlexQuote(pyExpandPath(p.Str("client_key"))))
	}
	if p.Has("ca_cert") {
		cmd = append(cmd, "--ssl-ca="+shlexQuote(pyExpandPath(p.Str("ca_cert"))))
	}
	return cmd
}

// compressor returns the (de)compression program a target's extension
// calls for.
func compressor(target string) (string, error) {
	var prog string
	switch filepath.Ext(target) {
	case ".gz":
		prog = "gzip"
	case ".bz2":
		prog = "bzip2"
	case ".xz":
		prog = "xz"
	case ".zst":
		prog = "zstd"
	default:
		return "", nil
	}
	return getBinPath(prog)
}

// dbDump is db_dump(): mysqldump (mariadb-dump on MariaDB >= 10.4.6) run
// through the shell into target, compressed by extension.
func (m *mysqlModule) dbDump(db []string, target string, allDatabases bool, configFile, impl, version, chdir string,
	checkImplicitAdmin bool) (int, string, string, []any, *agentproto.Result) {
	p := m.p
	cmdStr := "mysqldump"
	if impl == "mariadb" && !looseVersionLess(version, "10.4.6") {
		cmdStr = "mariadb-dump"
	}
	bin, err := getBinPath(cmdStr)
	if err != nil {
		return 0, "", "", nil, m.failf("%s", err)
	}
	cmd := append([]string{bin}, m.clientCredentialArgs(configFile, checkImplicitAdmin, true)...)
	if p.Bool("force") {
		cmd = append(cmd, "--force")
	}
	if p.Has("login_unix_socket") {
		cmd = append(cmd, "--socket="+shlexQuote(p.Str("login_unix_socket")))
	} else {
		cmd = append(cmd, fmt.Sprintf("--host=%s --port=%d", shlexQuote(p.Str("login_host")), p.Int("login_port")))
	}
	switch {
	case allDatabases:
		cmd = append(cmd, "--all-databases")
	case len(db) > 1:
		cmd = append(cmd, "--databases "+strings.Join(db, " "))
	default:
		cmd = append(cmd, shlexQuote(strings.Join(db, " ")))
	}
	if p.Bool("skip_lock_tables") {
		cmd = append(cmd, "--skip-lock-tables")
	}
	if enc := p.Str("encoding"); enc != "" {
		cmd = append(cmd, "--default-character-set="+shlexQuote(enc))
	}
	if p.Bool("single_transaction") {
		cmd = append(cmd, "--single-transaction=true")
	}
	if p.Bool("quick") {
		cmd = append(cmd, "--quick")
	}
	for _, t := range p.List("ignore_tables") {
		cmd = append(cmd, "--ignore-table="+pyStrValue(t))
	}
	if p.Bool("hex_blob") {
		cmd = append(cmd, "--hex-blob")
	}
	if md := p.Int("master_data"); md != 0 {
		if impl == "mysql" && !looseVersionLess(version, "8.2.0") {
			cmd = append(cmd, fmt.Sprintf("--source-data=%d", md))
		} else {
			cmd = append(cmd, fmt.Sprintf("--master-data=%d", md))
		}
	}
	if p.Has("dump_extra_args") {
		cmd = append(cmd, p.Str("dump_extra_args"))
	}
	comp, err := compressor(target)
	if err != nil {
		return 0, "", "", nil, m.failf("%s", err)
	}
	line := strings.Join(cmd, " ")
	pipefail := p.Bool("pipefail")
	if comp != "" {
		line = fmt.Sprintf("%s | %s > %s", line, comp, shlexQuote(target))
		if pipefail {
			line = "set -o pipefail && " + line
		}
	} else {
		line += " > " + shlexQuote(target)
	}
	shell := "/bin/sh"
	if pipefail {
		if b, err := getBinPath("bash"); err == nil {
			shell = b
		} else {
			return 0, "", "", []any{line}, m.failf("%s", err)
		}
	}
	rc, stdout, stderr := runCommand(m.env, []string{shell, "-c", line}, cmdOpts{Cwd: chdir})
	return rc, stdout, stderr, []any{line}, nil
}

// dbImport is db_import(): the mysql (or mariadb) client fed the target,
// decompressed by extension through a pipe (or the shell with use_shell).
func (m *mysqlModule) dbImport(db []string, target string, allDatabases bool, configFile, impl, version, chdir string,
	checkImplicitAdmin bool) (int, string, string, []any, *agentproto.Result) {
	p := m.p
	if !fileExists(target) {
		return 0, "", "", nil, m.failf("target %s does not exist on the host", target)
	}
	cmdStr := "mysql"
	if impl == "mariadb" && !looseVersionLess(version, "10.4.6") {
		cmdStr = "mariadb"
	}
	bin, err := getBinPath(cmdStr)
	if err != nil {
		return 0, "", "", nil, m.failf("%s", err)
	}
	cmd := append([]string{bin}, m.clientCredentialArgs(configFile, checkImplicitAdmin, false)...)
	if p.Bool("force") {
		cmd = append(cmd, "-f")
	}
	if p.Has("login_unix_socket") {
		cmd = append(cmd, "--socket="+shlexQuote(p.Str("login_unix_socket")))
	} else {
		cmd = append(cmd, "--host="+shlexQuote(p.Str("login_host")), fmt.Sprintf("--port=%d", p.Int("login_port")))
	}
	if enc := p.Str("encoding"); enc != "" {
		cmd = append(cmd, "--default-character-set="+shlexQuote(enc))
	}
	if !allDatabases {
		cmd = append(cmd, "--one-database", shlexQuote(strings.Join(db, "")))
	}
	comp, err := compressor(target)
	if err != nil {
		return 0, "", "", nil, m.failf("%s", err)
	}
	if comp == "" {
		line := strings.Join(cmd, " ") + " < " + shlexQuote(target)
		rc, stdout, stderr := runCommand(m.env, []string{"/bin/sh", "-c", line}, cmdOpts{Cwd: chdir})
		return rc, stdout, stderr, []any{line}, nil
	}
	executed := []any{fmt.Sprintf("%s -dc %s | %s", comp, target, pyValueRepr(cmd))}
	if p.Bool("use_shell") {
		line := fmt.Sprintf("%s -dc %s | %s", comp, shlexQuote(target), strings.Join(cmd, " "))
		rc, stdout, stderr := runCommand(m.env, []string{"/bin/sh", "-c", line}, cmdOpts{Cwd: chdir})
		return rc, stdout, stderr, executed, nil
	}
	// Two processes joined by a pipe, as subprocess.Popen does; their
	// output is bytes, which the module formats with %s (b'...').
	c1 := m.env.Command(comp, "-dc", target)
	c2 := m.env.Command(cmd[0], cmd[1:]...)
	c1.Dir, c2.Dir = chdir, chdir
	applyEnv(c1, m.env)
	applyEnv(c2, m.env)
	var err1, out2, err2 strings.Builder
	pipe, err := c1.StdoutPipe()
	if err != nil {
		return 1, "", err.Error(), executed, nil
	}
	c1.Stderr = &err1
	c2.Stdin = pipe
	c2.Stdout, c2.Stderr = &out2, &err2
	if err := c1.Start(); err != nil {
		return 1, "", err.Error(), executed, nil
	}
	rc2 := 0
	if err := c2.Run(); err != nil {
		rc2 = exitCode(err)
	}
	rc1 := 0
	if err := c1.Wait(); err != nil {
		rc1 = exitCode(err)
	}
	if rc1 != 0 {
		return rc1, "", pyBytesRepr(err1.String()), executed, nil
	}
	return rc2, out2.String(), pyBytesRepr(err2.String()), executed, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
		return 1
	}
	return 1
}
