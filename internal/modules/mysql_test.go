package modules

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Expectations below were produced by ansible.mysql's module_utils (5.2.0).

func TestPrivilegesUnpack(t *testing.T) {
	cases := []struct {
		priv, mode string
		cs         bool
		want       string // JSON of the ordered pairs
	}{
		{"mydb.*:INSERT,UPDATE/anotherdb.*:SELECT/yetanother.*:ALL", "NOTANSI", true,
			`[["` + "`mydb`" + `.*",["INSERT","UPDATE"]],["` + "`anotherdb`" + `.*",["SELECT"]],["` + "`yetanother`" + `.*",["ALL"]],["*.*",["USAGE"]]]`},
		{"mydb.*:INSERT,UPDATE/anotherdb.*:SELECT/yetanother.*:ALL", "ANSI", true,
			`[["\"mydb\".*",["INSERT","UPDATE"]],["\"anotherdb\".*",["SELECT"]],["\"yetanother\".*",["ALL"]],["*.*",["USAGE"]]]`},
		{"db.tbl:SELECT (b, a),insert (c),UPDATE", "NOTANSI", true,
			`[["` + "`db`.`tbl`" + `",["SELECT (a, b)","insert (c)","UPDATE"]],["*.*",["USAGE"]]]`},
		{"db.tbl:SELECT (b, a),insert (c),UPDATE", "NOTANSI", false,
			`[["` + "`db`.`tbl`" + `",["SELECT (A, B)","INSERT (C)","UPDATE"]],["*.*",["USAGE"]]]`},
		{"`weird`.`t`:select/FUNCTION db.fn:EXECUTE", "NOTANSI", true,
			`[["` + "`weird`.`t`" + `",["SELECT"]],["FUNCTION ` + "`db`.`fn`" + `",["EXECUTE"]],["*.*",["USAGE"]]]`},
		{"*.*:ALL,GRANT", "NOTANSI", true, `[["*.*",["ALL","GRANT"]]]`},
		{"db.*:SELECT, INSERT", "NOTANSI", true, `[["` + "`db`" + `.*",["SELECT"," INSERT"]],["*.*",["USAGE"]]]`},
	}
	for _, c := range cases {
		pm, err := privilegesUnpack(c.priv, c.mode, c.cs, true)
		if err != nil {
			t.Fatalf("%s: %v", c.priv, err)
		}
		var pairs [][]any
		for _, k := range pm.keys {
			pairs = append(pairs, []any{k, pm.m[k]})
		}
		got, _ := json.Marshal(pairs)
		var a, b any
		json.Unmarshal(got, &a)
		json.Unmarshal([]byte(c.want), &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("privilegesUnpack(%q, %s, %v)\n got  %s\n want %s", c.priv, c.mode, c.cs, got, c.want)
		}
	}
}

func TestMatchGrantLine(t *testing.T) {
	cases := []struct{ line, privs, db, tail string }{
		{"GRANT USAGE ON *.* TO `u`@`localhost`", "USAGE", "*.*", ""},
		{"GRANT SELECT, INSERT ON `db`.* TO `u`@`%` WITH GRANT OPTION", "SELECT, INSERT", "`db`.*", "WITH GRANT OPTION"},
		{"GRANT ALL PRIVILEGES ON `db`.`t` TO 'u'@'localhost' IDENTIFIED BY PASSWORD '*ABC' WITH GRANT OPTION", "ALL PRIVILEGES", "`db`.`t`", "WITH GRANT OPTION"},
		{"GRANT SELECT (`b`, `a`), INSERT ON `db`.`t` TO `u`@`localhost`", "SELECT (`b`, `a`), INSERT", "`db`.`t`", ""},
		{"GRANT PROXY ON ''@'' TO 'root'@'localhost' WITH GRANT OPTION", "PROXY", "''@''", "WITH GRANT OPTION"},
		{"GRANT APPLICATION_PASSWORD_ADMIN,AUDIT_ADMIN ON *.* TO `root`@`localhost` WITH GRANT OPTION", "APPLICATION_PASSWORD_ADMIN,AUDIT_ADMIN", "*.*", "WITH GRANT OPTION"},
		{"GRANT EXECUTE ON FUNCTION `db`.`fn` TO `u`@`h`", "EXECUTE", "FUNCTION `db`.`fn`", ""},
	}
	for _, c := range cases {
		p, db, tail, ok := matchGrantLine(c.line)
		if !ok || p != c.privs || db != c.db || tail != c.tail {
			t.Errorf("%s: got (%q, %q, %q, %v)", c.line, p, db, tail, ok)
		}
	}
	if _, _, _, ok := matchGrantLine("GRANT `admin`@`%` TO `u`@`localhost`"); ok {
		t.Error("role grant must not match")
	}
}

func TestMySQLSHA256PasswordHash(t *testing.T) {
	for _, c := range []struct{ pw, salt, want string }{
		{"secret", "abcdefghijklmnopqrst", "$A$005$abcdefghijklmnopqrstYy1cVJ5jT.fk4HyAGlowRhOkI55As4SAesbspXHQvFD"},
		{"a much longer password that exceeds thirty-two bytes!!", "ABCDEFGHIJKLMNOPQRST",
			"$A$005$ABCDEFGHIJKLMNOPQRSTjLQiMijnDrj9xUui3e9pLfgjG94O3LZM9oJYtR/gCi1"},
	} {
		if got := mysqlSHA256PasswordHash(c.pw, c.salt); got != c.want {
			t.Errorf("hash(%q) = %s, want %s", c.pw, got, c.want)
		}
	}
}

func TestMySQLQuoteIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"foo":       "`foo`",
		"fo`o":      "`fo``o`",
		"db.tbl":    "`db`.`tbl`",
		"`a.b`":     "`a.b`",
		"we%ird":    "`we%ird`",
		".x":        "`.x`",
		"max_conns": "`max_conns`",
		"`q`.`t`":   "`q`.`t`",
		"trailing.": "`trailing.`",
		"a.b":       "`a`.`b`",
	} {
		got, err := mysqlQuoteIdentifier(in, "table")
		if err != nil || got != want {
			t.Errorf("quote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := mysqlQuoteIdentifier("a.b.c", "database"); err == nil ||
		err.Error() != "MySQL does not support database with more than 1 dots" {
		t.Errorf("want dots error, got %v", err)
	}
}

func TestLooseVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{
		{"8.4.11", "9.7", true}, {"8.4.11", "8", false}, {"5.6.51", "5.7", true},
		{"11.4.13-MariaDB-ubu2404", "10.4.6", false}, {"10.3.39-MariaDB", "10.4.3", true},
		{"8.0.22", "8.0.22", false},
	} {
		if got := looseVersionLess(c.a, c.b); got != c.less {
			t.Errorf("%s < %s = %v", c.a, c.b, got)
		}
	}
}

func TestPyTypedValue(t *testing.T) {
	for in, want := range map[string]any{
		"3": int64(3), " 3 ": int64(3), "1_000": int64(1000), "3.0": 3.0, "1e3": 1000.0,
		"ON": "ON", "foo": "foo", "": "", "0x10": "0x10",
	} {
		if got := pyTypedValue(in); got != want {
			t.Errorf("typedvalue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestParseRequireClause(t *testing.T) {
	got := parseRequireClause("CREATE USER `u`@`%` IDENTIFIED WITH 'caching_sha2_password' REQUIRE ISSUER '/C=US' SUBJECT 'CN=x y' PASSWORD EXPIRE DEFAULT")
	want := map[string]any{"ISSUER": "/C=US", "SUBJECT": "CN=x y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	if got := parseRequireClause("CREATE USER `u`@`%` REQUIRE NONE PASSWORD EXPIRE"); got != nil {
		t.Errorf("NONE: got %#v", got)
	}
	if got := parseRequireClause("CREATE USER `u`@`%` REQUIRE SSL PASSWORD EXPIRE"); !reflect.DeepEqual(got, map[string]any{"SSL": nil}) {
		t.Errorf("SSL: got %#v", got)
	}
}
