package modules

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/mysqlclient"
)

func init() {
	registerMySQL(mysqlUserModule, "mysql_user")
}

var mysqlUserSpec = mysqlCommonSpec(args.Spec{
	"name":                     {Required: true},
	"password":                 {},
	"encrypted":                {Type: "bool", Default: false},
	"host":                     {Default: "localhost"},
	"host_all":                 {Type: "bool", Default: false},
	"state":                    {Default: "present", Choices: []string{"absent", "present"}},
	"priv":                     {Type: "any"},
	"tls_requires":             {Type: "dict"},
	"append_privs":             {Type: "bool", Default: false},
	"subtract_privs":           {Type: "bool", Default: false},
	"attributes":               {Type: "dict"},
	"check_implicit_admin":     {Type: "bool", Default: false},
	"update_password":          {Default: "always", Choices: []string{"always", "on_create", "on_new_username"}},
	"sql_log_bin":              {Type: "bool", Default: true},
	"plugin":                   {},
	"plugin_hash_string":       {},
	"plugin_auth_string":       {},
	"salt":                     {},
	"resource_limits":          {Type: "dict"},
	"force_context":            {Type: "bool", Default: false},
	"session_vars":             {Type: "dict"},
	"column_case_sensitive":    {Type: "bool", Default: true},
	"password_expire":          {Choices: []string{"now", "never", "default", "interval"}},
	"password_expire_interval": {Type: "int"},
	"locked":                   {Type: "bool"},
})

// errInvalidPrivs marks the InvalidPrivsError/SQLParseError family that
// mysql_user reports as a plain fail_json(msg).
type userResult struct {
	changed         bool
	msg             string
	passwordChanged any
	attributes      any
}

// mysqlUserModule ports ansible.mysql.mysql_user.
func mysqlUserModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if err := mysqlUserSpec.MutuallyExclusive(rawArgs, []string{"append_privs", "subtract_privs"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := mysqlUserSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	m := newMySQLModule(env, p, "ansible.mysql.mysql_user", "password", "password_expire", "password_expire_interval")
	user := p.Str("name")
	password := p.Str("password")
	encrypted := p.Bool("encrypted")
	host := strings.ToLower(p.Str("host"))
	hostAll := p.Bool("host_all")
	state := p.Str("state")
	tlsRequires := sanitizeRequires(p.Dict("tls_requires"))
	appendPrivs := p.Bool("append_privs")
	subtractPrivs := p.Bool("subtract_privs")
	updatePassword := p.Str("update_password")
	attributes := p.Dict("attributes")
	plugin := p.Str("plugin")
	pluginHash := p.Str("plugin_hash_string")
	pluginAuth := p.Str("plugin_auth_string")
	salt := p.Str("salt")
	passwordExpire := p.Str("password_expire")
	passwordExpireInterval := p.Int("password_expire_interval")
	var locked *bool
	if p.Has("locked") {
		b := p.Bool("locked")
		locked = &b
	}
	db := ""
	if p.Bool("force_context") {
		db = "mysql"
	}

	var priv *string
	if raw := p.Any("priv"); raw != nil {
		switch t := raw.(type) {
		case string:
			priv = &t
		case map[string]any:
			if len(t) > 0 {
				s := convertPrivDictToStr(t)
				priv = &s
			} else {
				s := ""
				priv = &s
			}
		default:
			if pyTruthy(raw) {
				return m.failf("priv parameter must be str or dict but %s was passed", pyTypeRepr(raw))
			}
		}
	}
	if p.Has("password_expire_interval") && passwordExpireInterval != 0 && passwordExpireInterval < 1 {
		return m.failf("password_expire_interval value                              should be positive number")
	}
	if salt != "" {
		switch {
		case pluginAuth == "":
			return m.failf("salt requires plugin_auth_string")
		case len([]rune(salt)) != 20:
			return m.failf("salt must be 20 characters long")
		case plugin != "caching_sha2_password" && plugin != "sha256_password":
			return m.failf("salt requires caching_sha2_password or sha256_password plugin")
		}
	}

	configFile := m.configFile()
	var conn *mysqlclient.Conn
	if p.Bool("check_implicit_admin") {
		conn, _ = m.connect(connectOpts{user: strPtr("root"), password: strPtr(""), db: db, autocommit: true})
	}
	if conn == nil {
		conn, err = m.connect(connectOpts{user: m.optStr("login_user"), password: m.optStr("login_password"), db: db, autocommit: true})
		if err != nil {
			return m.failf("unable to connect to database, check login_user and login_password are correct or %s has the credentials. "+
				"Exception message: %s", configFile, err)
		}
	}
	defer conn.Close()
	s, err := newMySQLSession(m, conn)
	if err != nil {
		return m.failf("%s", err)
	}

	if !p.Bool("sql_log_bin") {
		if _, err := conn.Query("SET SQL_LOG_BIN=0;"); err != nil {
			return m.failf("%s", err)
		}
	}
	if vars := p.Dict("session_vars"); len(vars) > 0 {
		if r := m.setSessionVars(conn, vars); r != nil {
			return r
		}
	}

	var newPriv *privMap
	if priv != nil {
		res, err := conn.Query("SELECT @@sql_mode")
		if err != nil {
			return m.failf("%s", err)
		}
		mode := "NOTANSI"
		if row := fetchone(res); row != nil && strings.Contains(cellString(row[0]), "ANSI") {
			mode = "ANSI"
		}
		newPriv, err = privilegesUnpack(*priv, mode, p.Bool("column_case_sensitive"), !subtractPrivs)
		if err != nil {
			return m.failf("%s", err)
		}
	}

	var ur userResult
	msg := ""
	switch state {
	case "present":
		exists, err := s.userExists(user, host, hostAll)
		if err != nil {
			return m.failf("%s", err)
		}
		if exists {
			o := userModOpts{user: user, host: host, hostAll: hostAll, password: password, encrypted: encrypted,
				plugin: plugin, pluginHash: pluginHash, pluginAuth: pluginAuth, salt: salt, newPriv: newPriv,
				appendPrivs: appendPrivs, subtractPrivs: subtractPrivs, attributes: attributes, tlsRequires: tlsRequires,
				passwordExpire: passwordExpire, passwordExpireInterval: passwordExpireInterval, locked: locked}
			if updatePassword != "always" {
				o.password, o.plugin, o.pluginHash, o.pluginAuth, o.salt = "", "", "", "", ""
			}
			var fail *agentproto.Result
			ur, fail, err = s.userMod(o)
			if fail != nil {
				return fail
			}
			if err != nil {
				return m.failf("%s", err)
			}
			msg = ur.msg
		} else {
			if hostAll {
				return m.failf("host_all parameter cannot be used when adding a user")
			}
			if subtractPrivs {
				newPriv = nil
			}
			var fail *agentproto.Result
			ur, fail, err = s.userAdd(user, host, password, encrypted, plugin, pluginHash, pluginAuth, salt, newPriv,
				attributes, tlsRequires, updatePassword == "on_new_username", passwordExpire, passwordExpireInterval, locked)
			if fail != nil {
				return fail
			}
			if err != nil {
				return m.failf("%s", err)
			}
			if ur.changed {
				msg = "User added"
			}
		}
		if limits := p.Dict("resource_limits"); len(limits) > 0 {
			changed, fail := s.limitResources(user, host, limits, env.CheckMode)
			if fail != nil {
				return fail
			}
			ur.changed = changed || ur.changed
		}
	case "absent":
		exists, err := s.userExists(user, host, hostAll)
		if err != nil {
			return m.failf("%s", err)
		}
		if exists {
			ur.changed = true
			if !env.CheckMode {
				hosts := []string{host}
				if hostAll {
					if hosts, err = s.userHostnames(user); err != nil {
						return m.failf("%s", err)
					}
				}
				for _, h := range hosts {
					if _, err := s.exec("DROP USER IF EXISTS %s@%s", []any{user, h}); err != nil {
						if _, err := s.exec("DROP USER %s@%s", []any{user, h}); err != nil {
							return m.failf("%s", err)
						}
					}
				}
			}
			msg = "User deleted"
		} else {
			msg = "User doesn't exist"
		}
		ur.passwordChanged = false
	}
	if ur.passwordChanged == nil && state == "present" && !env.CheckMode {
		ur.passwordChanged = false
	}
	return m.exit(ur.changed, map[string]any{
		"user": user, "msg": msg, "password_changed": ur.passwordChanged, "attributes": ur.attributes,
	})
}

func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// setSessionVars is set_session_vars().
func (m *mysqlModule) setSessionVars(conn *mysqlclient.Conn, vars map[string]any) *agentproto.Result {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q, err := mysqlQuoteIdentifier(k, "vars")
		if err != nil {
			return m.failf("%s", err)
		}
		query := "SET SESSION " + q + " = "
		if _, _, err := conn.Exec(query+"%s", []any{vars[k]}); err != nil {
			return m.failf("Failed to execute %s%s: %s", query, pyStrValue(vars[k]), err)
		}
	}
	return nil
}

func (s *mysqlSession) userExists(user, host string, hostAll bool) (bool, error) {
	var res *mysqlclient.Result
	var err error
	if hostAll {
		res, err = s.exec("SELECT count(*) FROM mysql.user WHERE user = %s", []any{user})
	} else {
		res, err = s.exec("SELECT count(*) FROM mysql.user WHERE user = %s AND host = %s", []any{user, host})
	}
	if err != nil {
		return false, err
	}
	row := fetchone(res)
	n, _ := row[0].(int64)
	return n > 0, nil
}

func (s *mysqlSession) userHostnames(user string) ([]string, error) {
	res, err := s.exec("SELECT Host FROM mysql.user WHERE user = %s", []any{user})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range fetchall(res) {
		out = append(out, cellString(r[0]))
	}
	return out, nil
}

// execTLS runs query (with %s params) plus a REQUIRE clause.
func (s *mysqlSession) execRequires(query string, params []any, req any, mogrify bool) error {
	if mogrify {
		query, params = mogrifyRequires(query, params, req)
	}
	_, err := s.exec(query, params)
	return err
}

func isNativeHash(pw string) bool {
	if len(pw) != 41 || pw[0] != '*' {
		return false
	}
	_, err := hex.DecodeString(pw[1:])
	return err == nil
}

// userAdd is user_add().
func (s *mysqlSession) userAdd(user, host, password string, encrypted bool, plugin, pluginHash, pluginAuth, salt string,
	newPriv *privMap, attributes map[string]any, tlsRequires any, reuseExisting bool, passwordExpire string,
	passwordExpireInterval int64, locked *bool) (userResult, *agentproto.Result, error) {
	m := s.m
	if len(attributes) > 0 && !s.getAttributeSupport() {
		return userResult{}, m.failf("user attributes were specified but the server does not support user attributes"), nil
	}
	var attrsOut any
	if attributes != nil {
		attrsOut = attributes
	}
	if m.env.CheckMode {
		return userResult{changed: true, passwordChanged: nil, attributes: attrsOut}, nil, nil
	}
	s.userImpl()
	oldMgmt := s.useOldUserMgmt()
	usedExisting := false
	if reuseExisting {
		existing, err := s.getExistingAuthentication(user, "")
		if err != nil {
			return userResult{}, nil, err
		}
		if len(existing) > 0 {
			if len(existing) != 1 {
				m.warn(fmt.Sprintf("An account with the username %s has a different "+
					"password than the others existing accounts. Thus "+
					"on_new_username can't decide which password to "+
					"reuse so it will use your provided password "+
					"instead. If no password is provided, the account "+
					"will have an empty password!", user))
			} else {
				pluginHash = cellString(existing[0]["plugin_hash_string"])
				password = ""
				usedExisting = true
				plugin = cellString(existing[0]["plugin"])
			}
		}
	}
	var query string
	var params []any
	switch {
	case password != "" && encrypted:
		switch {
		case s.supportsIdentifiedByPassword():
			query, params = "CREATE USER %s@%s IDENTIFIED BY PASSWORD %s", []any{user, host, password}
		case s.supportsNativePassword():
			query, params = "CREATE USER %s@%s IDENTIFIED WITH mysql_native_password AS %s", []any{user, host, password}
		default:
			return userResult{}, m.failf("The 'encrypted' option is not supported on MySQL 9.7.0+ " +
				"because the mysql_native_password plugin has been removed. " +
				"Use a plaintext password instead."), nil
		}
	case password != "":
		if oldMgmt || !s.supportsNativePassword() {
			query, params = "CREATE USER %s@%s IDENTIFIED BY %s", []any{user, host, password}
		} else {
			res, err := s.exec("SELECT CONCAT('*', UCASE(SHA1(UNHEX(SHA1(%s)))))", []any{password})
			if err != nil {
				return userResult{}, nil, err
			}
			query, params = "CREATE USER %s@%s IDENTIFIED WITH mysql_native_password AS %s", []any{user, host, cellString(fetchone(res)[0])}
		}
	case plugin != "" && pluginHash != "":
		query, params = "CREATE USER %s@%s IDENTIFIED WITH %s AS %s", []any{user, host, plugin, pluginHash}
	case plugin != "" && pluginAuth != "":
		switch {
		case plugin == "pam":
			query, params = "CREATE USER %s@%s IDENTIFIED WITH %s USING %s", []any{user, host, plugin, pluginAuth}
		case plugin == "ed25519" || plugin == "parsec":
			query, params = "CREATE USER %s@%s IDENTIFIED WITH %s USING PASSWORD(%s)", []any{user, host, plugin, pluginAuth}
		case salt != "":
			query, params = "CREATE USER %s@%s IDENTIFIED WITH %s AS 0x"+mysqlSHA256PasswordHashHex(pluginAuth, salt), []any{user, host, plugin}
		default:
			query, params = "CREATE USER %s@%s IDENTIFIED WITH %s BY %s", []any{user, host, plugin, pluginAuth}
		}
	case plugin != "":
		query, params = "CREATE USER %s@%s IDENTIFIED WITH %s", []any{user, host, plugin}
	default:
		query, params = "CREATE USER %s@%s", []any{user, host}
	}
	if err := s.execRequires(query, params, tlsRequires, !oldMgmt); err != nil {
		return userResult{}, nil, err
	}
	if passwordExpire != "" {
		if !s.supportsPasswordExpire() {
			return userResult{}, m.failf("The server version does not match the requirements " +
				"for password_expire parameter. See module's documentation."), nil
		}
		if err := s.setPasswordExpire(user, host, passwordExpire, passwordExpireInterval); err != nil {
			return userResult{}, nil, err
		}
	}
	if newPriv != nil {
		for _, dbTable := range newPriv.keys {
			if err := s.privilegesGrant(user, host, dbTable, newPriv.m[dbTable], tlsRequires, false); err != nil {
				return userResult{}, nil, err
			}
		}
	}
	if tlsRequires != nil {
		grants, err := s.getGrants(user, host)
		if err != nil {
			return userResult{}, nil, err
		}
		if err := s.privilegesGrant(user, host, "*.*", grants, tlsRequires, false); err != nil {
			return userResult{}, nil, err
		}
	}
	var finalAttrs any
	if len(attributes) > 0 {
		if _, err := s.exec("ALTER USER %s@%s ATTRIBUTE %s", []any{user, host, pyJSONDumps(attributes)}); err != nil {
			return userResult{}, nil, err
		}
		a, err := s.attributesGet(user, host)
		if err != nil {
			return userResult{}, nil, err
		}
		if a != nil {
			finalAttrs = a
		}
	}
	if locked != nil && *locked {
		if _, err := s.exec("ALTER USER %s@%s ACCOUNT LOCK", []any{user, host}); err != nil {
			return userResult{}, nil, err
		}
	}
	return userResult{changed: true, passwordChanged: !usedExisting, attributes: finalAttrs}, nil, nil
}

type userModOpts struct {
	user, host                     string
	hostAll                        bool
	password                       string
	encrypted                      bool
	plugin, pluginHash, pluginAuth string
	salt                           string
	newPriv                        *privMap
	appendPrivs, subtractPrivs     bool
	attributes                     map[string]any
	tlsRequires                    any
	passwordExpire                 string
	passwordExpireInterval         int64
	locked                         *bool
}

// userMod is user_mod() for accounts (not roles).
func (s *mysqlSession) userMod(o userModOpts) (userResult, *agentproto.Result, error) {
	m := s.m
	check := m.env.CheckMode
	changed := false
	msg := "User unchanged"
	grantOption := false
	s.userImpl()
	oldMgmt := s.useOldUserMgmt()
	hostnames := []string{o.host}
	if o.hostAll {
		var err error
		if hostnames, err = s.userHostnames(o.user); err != nil {
			return userResult{}, nil, err
		}
	}
	user := o.user
	passwordChanged := false
	var finalAttributes any = map[string]any{}
	for _, host := range hostnames {
		// Passwords (clear text or mysql_native_password hashes).
		if o.password != "" {
			if !s.supportsNativePassword() {
				if o.encrypted {
					return userResult{}, m.failf("The 'encrypted' option is not supported by your database server " +
						"version because the mysql_native_password plugin has been removed. " +
						"Use a plaintext password instead."), nil
				}
				passwordChanged = true
				msg = "Password updated"
				if !check {
					if _, err := s.exec("ALTER USER %s@%s IDENTIFIED BY %s", []any{user, host, o.password}); err != nil {
						return userResult{}, nil, err
					}
				}
				changed = true
			} else {
				colQuery := `
                        SELECT COLUMN_NAME FROM information_schema.COLUMNS
                        WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'user' AND COLUMN_NAME IN ('Password', 'authentication_string')
                        ORDER BY COLUMN_NAME %s LIMIT 1
                    `
				resA, err := s.c.Query(fmt.Sprintf(colQuery, "DESC"))
				if err != nil {
					return userResult{}, nil, err
				}
				resB, err := s.c.Query(fmt.Sprintf(colQuery, "ASC "))
				if err != nil {
					return userResult{}, nil, err
				}
				colA, colB := cellString(fetchone(resA)[0]), cellString(fetchone(resB)[0])
				res, err := s.exec(fmt.Sprintf(`
                        SELECT COALESCE(
                                CASE WHEN %s = '' THEN NULL ELSE %s END,
                                CASE WHEN %s = '' THEN NULL ELSE %s END
                            )
                        FROM mysql.user WHERE user = %%s AND host = %%s
                        `, colA, colA, colB, colB), []any{user, host})
				if err != nil {
					return userResult{}, nil, err
				}
				var current any
				if row := fetchone(res); row != nil {
					current = row[0]
				}
				var encryptedPassword string
				if o.encrypted {
					encryptedPassword = o.password
					if !isNativeHash(encryptedPassword) {
						return userResult{}, m.failf("encrypted was specified however it does not appear to be a valid hash expecting: *SHA1(SHA1(your_password))"), nil
					}
				} else {
					q := "SELECT CONCAT('*', UCASE(SHA1(UNHEX(SHA1(%s)))))"
					if oldMgmt {
						q = "SELECT PASSWORD(%s)"
					}
					res, err := s.exec(q, []any{o.password})
					if err != nil {
						return userResult{}, nil, err
					}
					encryptedPassword = cellString(fetchone(res)[0])
				}
				if current == nil || cellString(current) != encryptedPassword {
					passwordChanged = true
					msg = "Password updated"
					if !check {
						if oldMgmt {
							if _, err := s.exec("SET PASSWORD FOR %s@%s = %s", []any{user, host, encryptedPassword}); err != nil {
								return userResult{}, nil, err
							}
							msg = "Password updated (old style)"
						} else {
							_, err := s.exec("ALTER USER %s@%s IDENTIFIED WITH mysql_native_password AS %s", []any{user, host, encryptedPassword})
							var me *mysqlclient.Error
							switch {
							case err == nil:
								msg = "Password updated (new style)"
							case errors.As(err, &me) && me.Code == 1396:
								if _, err := s.exec("UPDATE mysql.user SET plugin = %s, authentication_string = %s, Password = '' WHERE User = %s AND Host = %s",
									[]any{"mysql_native_password", encryptedPassword, user, host}); err != nil {
									return userResult{}, nil, err
								}
								if _, err := s.c.Query("FLUSH PRIVILEGES"); err != nil {
									return userResult{}, nil, err
								}
								msg = "Password forced update"
							default:
								return userResult{}, nil, err
							}
						}
					}
					changed = true
				}
			}
		}

		// Password expiration.
		if o.passwordExpire != "" {
			if !s.supportsPasswordExpire() {
				return userResult{}, m.failf("The server version does not match the requirements " +
					"for password_expire parameter. See module's documentation."), nil
			}
			policy, err := s.passwordExpirationPolicy(user, host)
			if err != nil {
				return userResult{}, nil, err
			}
			expired, err := s.isPasswordExpired(user, host)
			if err != nil {
				return userResult{}, nil, err
			}
			pe := o.passwordExpire
			if !((policy == -1 && pe == "default") || (policy == 0 && pe == "never") ||
				(policy == o.passwordExpireInterval && pe == "interval") || (pe == "now" && expired)) {
				if !check {
					if err := s.setPasswordExpire(user, host, pe, o.passwordExpireInterval); err != nil {
						return userResult{}, nil, err
					}
					passwordChanged = true
					changed = true
				}
			}
		}

		// Authentication plugin.
		if o.plugin != "" {
			res, err := s.exec("SELECT plugin, authentication_string FROM mysql.user WHERE user = %s AND host = %s", []any{user, host})
			if err != nil {
				return userResult{}, nil, err
			}
			cur := fetchone(res)
			curPlugin, curAuth := cellString(cur[0]), cellString(cur[1])
			update := curPlugin != o.plugin
			if o.pluginHash != "" && curAuth != o.pluginHash {
				update = true
			}
			if o.salt != "" {
				if o.plugin == "caching_sha2_password" || o.plugin == "sha256_password" {
					if curAuth != mysqlSHA256PasswordHash(o.pluginAuth, o.salt) {
						update = true
					}
				}
			} else if o.pluginAuth != "" && curAuth != o.pluginAuth {
				update = true
			}
			if update {
				var query string
				var params []any
				switch {
				case o.pluginHash != "":
					query, params = "ALTER USER %s@%s IDENTIFIED WITH %s AS %s", []any{user, host, o.plugin, o.pluginHash}
				case o.pluginAuth != "":
					switch {
					case o.plugin == "pam":
						query, params = "ALTER USER %s@%s IDENTIFIED WITH %s USING %s", []any{user, host, o.plugin, o.pluginAuth}
					case o.plugin == "ed25519":
						query, params = "ALTER USER %s@%s IDENTIFIED WITH %s USING PASSWORD(%s)", []any{user, host, o.plugin, o.pluginAuth}
					case o.salt != "":
						if o.plugin != "caching_sha2_password" && o.plugin != "sha256_password" {
							return userResult{}, m.failf("salt not handled for %s authentication plugin", o.plugin), nil
						}
						query, params = "ALTER USER %s@%s IDENTIFIED WITH %s AS 0x"+mysqlSHA256PasswordHashHex(o.pluginAuth, o.salt), []any{user, host, o.plugin}
					default:
						query, params = "ALTER USER %s@%s IDENTIFIED WITH %s BY %s", []any{user, host, o.plugin, o.pluginAuth}
					}
				default:
					query, params = "ALTER USER %s@%s IDENTIFIED WITH %s", []any{user, host, o.plugin}
				}
				if !check {
					if _, err := s.exec(query, params); err != nil {
						return userResult{}, nil, err
					}
				}
				passwordChanged = true
				changed = true
			}
		}

		// Privileges.
		if o.newPriv != nil {
			curPriv, err := s.privilegesGet(user, host, false)
			if err != nil {
				return userResult{}, nil, err
			}
			if !o.appendPrivs && !o.subtractPrivs {
				for _, dbTable := range curPriv.keys {
					priv := curPriv.m[dbTable]
					if containsStr(priv, "GRANT") {
						grantOption = true
					}
					if !o.newPriv.has(dbTable) && user != "root" && !containsStr(priv, "PROXY") {
						msg = "Privileges updated"
						if !check {
							if err := s.privilegesRevoke(user, host, dbTable, priv, grantOption); err != nil {
								return userResult{}, nil, err
							}
						}
						changed = true
					}
				}
			}
			if !o.subtractPrivs {
				for _, dbTable := range o.newPriv.keys {
					if !curPriv.has(dbTable) {
						msg = "New privileges granted"
						if !check {
							if err := s.privilegesGrant(user, host, dbTable, o.newPriv.m[dbTable], o.tlsRequires, false); err != nil {
								return userResult{}, nil, err
							}
						}
						changed = true
					}
				}
			}
			for _, dbTable := range o.newPriv.keys {
				if !curPriv.has(dbTable) {
					continue
				}
				want, have := o.newPriv.m[dbTable], curPriv.m[dbTable]
				var grantPrivs, revokePrivs []string
				switch {
				case o.appendPrivs:
					grantPrivs = setMinus(want, have)
				case o.subtractPrivs:
					revokePrivs = setIntersect(want, have)
				default:
					grantPrivs = setMinus(want, have)
					revokePrivs = setMinus(have, want)
					if containsStr(grantPrivs, "ALL") || containsStr(grantPrivs, "ALL PRIVILEGES") {
						revokePrivs = setIntersect([]string{"GRANT", "PROXY"}, revokePrivs)
					}
					grantOption = containsStr(revokePrivs, "GRANT") && !containsStr(grantPrivs, "GRANT")
				}
				if len(grantPrivs) == 1 && grantPrivs[0] == "GRANT" {
					grantPrivs = append(grantPrivs, "USAGE")
				}
				if len(grantPrivs)+len(revokePrivs) > 0 {
					msg = fmt.Sprintf("Privileges updated: granted %s, revoked %s", pyValueRepr(listOrEmpty(grantPrivs)), pyValueRepr(listOrEmpty(revokePrivs)))
					if !check {
						if len(revokePrivs) > 0 {
							if err := s.privilegesRevoke(user, host, dbTable, revokePrivs, grantOption); err != nil {
								return userResult{}, nil, err
							}
						}
						if len(grantPrivs) > 0 {
							if err := s.privilegesGrant(user, host, dbTable, grantPrivs, o.tlsRequires, false); err != nil {
								return userResult{}, nil, err
							}
						}
					} else {
						changed = true
					}
				}
			}
			afterPriv, err := s.privilegesGet(user, host, false)
			if err != nil {
				return userResult{}, nil, err
			}
			changed = changed || !curPriv.equal(afterPriv)
		}

		// Attributes.
		attributeSupport := s.getAttributeSupport()
		finalAttributes = map[string]any{}
		if len(o.attributes) > 0 {
			if !attributeSupport {
				return userResult{}, m.failf("user attributes were specified but the server does not support user attributes"), nil
			}
			current, err := s.attributesGet(user, host)
			if err != nil {
				return userResult{}, nil, err
			}
			if current == nil {
				current = map[string]any{}
			}
			toChange := map[string]any{}
			var keys []string
			for k := range o.attributes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				v := o.attributes[k]
				cv, ok := current[k]
				if !ok || pyJSONDumps(cv) != pyJSONDumps(v) {
					toChange[k] = v
					parts = append(parts, k+": "+pyStrValue(v))
				}
			}
			if len(toChange) > 0 {
				msg = "Attributes updated: " + strings.Join(parts, ", ")
				if !check {
					if _, err := s.exec("ALTER USER %s@%s ATTRIBUTE %s", []any{user, host, pyJSONDumps(toChange)}); err != nil {
						return userResult{}, nil, err
					}
					a, err := s.attributesGet(user, host)
					if err != nil {
						return userResult{}, nil, err
					}
					finalAttributes = mapOrNil(a)
				} else {
					merged := map[string]any{}
					for k, v := range current {
						if _, ch := toChange[k]; !ch {
							merged[k] = v
						}
					}
					for k, v := range toChange {
						if v != nil {
							merged[k] = v
						}
					}
					finalAttributes = mapOrNil(merged)
				}
				changed = true
			} else {
				finalAttributes = current
			}
		} else if attributeSupport {
			a, err := s.attributesGet(user, host)
			if err != nil {
				return userResult{}, nil, err
			}
			finalAttributes = mapOrNil(a)
		}

		// Account locking.
		if o.locked != nil {
			isLocked, err := s.userIsLocked(user, host)
			if err != nil {
				return userResult{}, nil, err
			}
			if isLocked != *o.locked {
				if !check {
					stmt, word := "ALTER USER %s@%s ACCOUNT UNLOCK", "User unlocked"
					if *o.locked {
						stmt, word = "ALTER USER %s@%s ACCOUNT LOCK", "User locked"
					}
					if _, err := s.exec(stmt, []any{user, host}); err != nil {
						return userResult{}, nil, err
					}
					msg = word
				} else if *o.locked {
					msg = "User will be locked"
				} else {
					msg = "User will be unlocked"
				}
				changed = true
			}
		}

		// TLS requirements.
		s.userImpl()
		cur, err := s.getTLSRequires(user, host)
		if err != nil {
			return userResult{}, nil, err
		}
		var curReq any
		if cur != nil {
			curReq = sanitizeRequires(cur)
		}
		if !requiresEqual(curReq, o.tlsRequires) {
			msg = "TLS requires updated"
			if !check {
				pre := "ALTER USER"
				if oldMgmt {
					grants, err := s.getGrants(user, host)
					if err != nil {
						return userResult{}, nil, err
					}
					pre = "GRANT " + strings.Join(grants, ",") + " ON *.* TO"
				}
				var err error
				if o.tlsRequires != nil {
					err = s.execRequires(pre+" %s@%s", []any{user, host}, o.tlsRequires, true)
				} else {
					_, err = s.exec(pre+" %s@%s REQUIRE NONE", []any{user, host})
				}
				if err != nil {
					return userResult{}, nil, err
				}
			}
			changed = true
		}
	}
	return userResult{changed: changed, msg: msg, passwordChanged: passwordChanged, attributes: finalAttributes}, nil, nil
}

func mapOrNil(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func listOrEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func containsStr(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

// setMinus is list(set(a) - set(b)), in a's order.
func setMinus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !containsStr(b, x) && !containsStr(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func setIntersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if containsStr(b, x) && !containsStr(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// privilegesGrant is privileges_grant().
func (s *mysqlSession) privilegesGrant(user, host, dbTable string, priv []string, tlsRequires any, mariaRole bool) error {
	dbTable = strings.ReplaceAll(dbTable, "%", "%%")
	var ps []string
	for _, p := range priv {
		if p != "GRANT" {
			ps = append(ps, p)
		}
	}
	privString := strings.Join(ps, ",")
	query := fmt.Sprintf("GRANT %s ON %s", privString, dbTable)
	var params []any
	if !mariaRole {
		query += " TO %s@%s"
		params = []any{user, host}
	} else {
		query += " TO %s"
		params = []any{user}
	}
	s.userImpl()
	if tlsRequires != nil && s.useOldUserMgmt() {
		query, params = mogrifyRequires(query, params, tlsRequires)
	}
	if containsStr(priv, "GRANT") {
		query += " WITH GRANT OPTION"
	}
	if _, err := s.exec(query, params); err != nil {
		return fmt.Errorf("Error granting privileges, invalid priv string: %s , params: %s, query: %s , exception: %s.",
			privString, pyTupleRepr(params), query, err)
	}
	return nil
}

func pyTupleRepr(params []any) string {
	if len(params) == 1 {
		return "(" + pyValueRepr(params[0]) + ",)"
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = pyValueRepr(p)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// privilegesRevoke is privileges_revoke().
func (s *mysqlSession) privilegesRevoke(user, host, dbTable string, priv []string, grantOption bool) error {
	dbTable = strings.ReplaceAll(dbTable, "%", "%%")
	if grantOption {
		if _, err := s.exec("REVOKE GRANT OPTION ON "+dbTable+" FROM %s@%s", []any{user, host}); err != nil {
			return err
		}
	}
	var ps []string
	for _, p := range priv {
		if p != "GRANT" {
			ps = append(ps, p)
		}
	}
	privString := strings.Join(ps, ",")
	if privString != "" && !(grantOption && privString == "USAGE") {
		if _, err := s.exec("REVOKE "+privString+" ON "+dbTable+" FROM %s@%s", []any{user, host}); err != nil {
			return err
		}
	}
	_, err := s.c.Query("FLUSH PRIVILEGES")
	return err
}

// setPasswordExpire is set_password_expire().
func (s *mysqlSession) setPasswordExpire(user, host, pe string, interval int64) error {
	var stmt string
	switch strings.ToLower(pe) {
	case "never":
		stmt = "PASSWORD EXPIRE NEVER"
	case "default":
		stmt = "PASSWORD EXPIRE DEFAULT"
	case "interval":
		stmt = fmt.Sprintf("PASSWORD EXPIRE INTERVAL %d DAY", interval)
	case "now":
		stmt = "PASSWORD EXPIRE"
	}
	_, err := s.exec("ALTER USER %s@%s "+stmt, []any{user, host})
	return err
}

func (s *mysqlSession) passwordExpirationPolicy(user, host string) (int64, error) {
	var res *mysqlclient.Result
	var err error
	if !s.isMaria() {
		res, err = s.exec("SELECT IFNULL(password_lifetime, -1) FROM mysql.user             WHERE User = %s AND Host = %s", []any{user, host})
	} else {
		res, err = s.exec("SELECT JSON_EXTRACT(Priv, '$.password_lifetime') AS password_lifetime             FROM mysql.global_priv             WHERE User = %s AND Host = %s", []any{user, host})
	}
	if err != nil {
		return 0, err
	}
	row := fetchone(res)
	if row == nil {
		return 0, fmt.Errorf("'NoneType' object is not subscriptable")
	}
	switch v := row[0].(type) {
	case int64:
		return v, nil
	case nil:
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not 'NoneType'")
	default:
		n, err := strconv.ParseInt(strings.TrimSpace(cellString(v)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", mysqlclient.PyRepr(cellString(v)))
		}
		return n, nil
	}
}

func (s *mysqlSession) isPasswordExpired(user, host string) (bool, error) {
	res, err := s.exec("SELECT password_expired FROM mysql.user             WHERE User = %s AND Host = %s", []any{user, host})
	if err != nil {
		return false, err
	}
	row := fetchone(res)
	return row != nil && cellString(row[0]) == "Y", nil
}

// limitResources is limit_resources().
func (s *mysqlSession) limitResources(user, host string, limits map[string]any, check bool) (bool, *agentproto.Result) {
	m := s.m
	s.userImpl()
	if !s.supportsAlterUser() {
		return false, m.failf("The server version does not match the requirements " +
			"for resource_limits parameter. See module's documentation.")
	}
	if _, ok := limits["MAX_STATEMENT_TIME"]; ok && !s.isMaria() {
		return false, m.failf("MAX_STATEMENT_TIME resource limit is only supported by MariaDB.")
	}
	current, _, err := s.getResourceLimits(user, host)
	if err != nil {
		return false, m.failf("%s", err)
	}
	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var tmp []string
	for _, k := range keys {
		v := limits[k]
		if current != nil {
			cur, ok := current[k]
			if !ok {
				return false, m.failf("resource_limits: key '%s' is unsupported.", k)
			}
			n, ok := pyInt64(v)
			if !ok {
				return false, m.failf("Can't convert value '%s' to integer.", pyStrValue(v))
			}
			if pyValueRepr(n) == pyValueRepr(normalizeLimit(cur)) {
				continue
			}
			tmp = append(tmp, fmt.Sprintf("%s %d", k, n))
		} else {
			tmp = append(tmp, fmt.Sprintf("%s %s", k, pyStrValue(v)))
		}
	}
	if len(tmp) == 0 {
		return false, nil
	}
	if check {
		return true, nil
	}
	if _, err := s.exec("ALTER USER %s@%s WITH "+strings.Join(tmp, " "), []any{user, host}); err != nil {
		return false, m.failf("%s", err)
	}
	return true, nil
}

// normalizeLimit maps a current limit (int, or MariaDB's DECIMAL
// max_statement_time) to what int(desired) compares equal to.
func normalizeLimit(v any) any {
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int64(f)) {
			return int64(f)
		}
	}
	return v
}

// pyInt64 is int(v) for a module value.
func pyInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(t, "_", "")), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// mysqlSHA256PasswordHash is the caching_sha2_password/sha256_password
// hash for a fixed salt ($A$005$<salt><digest>): SHA-crypt, 5000 rounds.
func mysqlSHA256PasswordHash(password, salt string) string {
	const rounds = 5000
	key, sl := []byte(password), []byte(salt)
	sum := func(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
	digestB := sum(append(append(append([]byte{}, key...), sl...), key...))
	tmp := append(append([]byte{}, key...), sl...)
	for i := len(key); i > 0; i -= 32 {
		if i > 32 {
			tmp = append(tmp, digestB...)
		} else {
			tmp = append(tmp, digestB[:i]...)
		}
	}
	for i := len(key); i > 0; i >>= 1 {
		if i&1 != 0 {
			tmp = append(tmp, digestB...)
		} else {
			tmp = append(tmp, key...)
		}
	}
	digestA := sum(tmp)
	tmp = nil
	for range key {
		tmp = append(tmp, key...)
	}
	digestDP := sum(tmp)
	var p []byte
	for i := len(key); i > 0; i -= 32 {
		if i > 32 {
			p = append(p, digestDP...)
		} else {
			p = append(p, digestDP[:i]...)
		}
	}
	tmp = nil
	for i := 0; i < 16+int(digestA[0]); i++ {
		tmp = append(tmp, sl...)
	}
	digestDS := sum(tmp)
	var sseq []byte
	for i := len(sl); i > 0; i -= 32 {
		if i > 32 {
			sseq = append(sseq, digestDS...)
		} else {
			sseq = append(sseq, digestDS[:i]...)
		}
	}
	c := digestA
	for i := 0; i < rounds; i++ {
		var t []byte
		if i&1 != 0 {
			t = append([]byte{}, p...)
		} else {
			t = append([]byte{}, c...)
		}
		if i%3 != 0 {
			t = append(t, sseq...)
		}
		if i%7 != 0 {
			t = append(t, p...)
		}
		if i&1 != 0 {
			t = append(t, c...)
		} else {
			t = append(t, p...)
		}
		c = sum(t)
	}
	const i64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	to64 := func(v uint32, n int) string {
		var b strings.Builder
		for ; n > 0; n-- {
			b.WriteByte(i64[v&0x3f])
			v >>= 6
		}
		return b.String()
	}
	var out strings.Builder
	for i := 0; ; {
		out.WriteString(to64(uint32(c[i])<<16|uint32(c[(i+10)%30])<<8|uint32(c[(i+20)%30]), 4))
		i = (i + 21) % 30
		if i == 0 {
			break
		}
	}
	out.WriteString(to64(uint32(c[31])<<8|uint32(c[30]), 3))
	return fmt.Sprintf("$A$%03d$%s%s", 5, salt, out.String())
}

func mysqlSHA256PasswordHashHex(password, salt string) string {
	return strings.ToUpper(hex.EncodeToString([]byte(mysqlSHA256PasswordHash(password, salt))))
}
