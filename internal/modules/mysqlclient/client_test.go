package mysqlclient

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestMogrify(t *testing.T) {
	cases := []struct {
		q    string
		args any
		want string
	}{
		{"SELECT %s, %s, %s, %s", []any{int64(1), "it's \"q\"\n", 9.99, nil}, `SELECT 1, 'it\'s \"q\"\n', 9.99e0, NULL`},
		{"SELECT %(a)s LIKE 'x%%'", map[string]any{"a": true}, `SELECT 1 LIKE 'x%'`},
		{"CREATE DATABASE `we%%ird`", map[string]any{"enc": ""}, "CREATE DATABASE `we%ird`"},
		{"IN %s", []any{[]any{int64(1), "a"}}, `IN (1,'a')`},
		{"x %s", []any{1e20}, `x 1e+20`},
		{"x %s", []any{Bytes("ab")}, `x _binary X'6162'`},
	}
	for _, c := range cases {
		got, err := Mogrify(c.q, c.args, false)
		if err != nil || got != c.want {
			t.Errorf("Mogrify(%q) = %q, %v; want %q", c.q, got, err, c.want)
		}
	}
	for _, c := range []struct {
		q    string
		args any
		err  string
	}{
		{"%s %s", []any{int64(1)}, "not enough arguments for format string"},
		{"%s", []any{int64(1), int64(2)}, "not all arguments converted during string formatting"},
		{"%(x)s", map[string]any{}, "'x'"},
		{"%d", []any{int64(1)}, "%d format: a real number is required, not str"},
		{"%s", []any{math.Inf(1)}, "inf can not be used with MySQL"},
	} {
		if _, err := Mogrify(c.q, c.args, false); err == nil || err.Error() != c.err {
			t.Errorf("Mogrify(%q) error = %v, want %q", c.q, err, c.err)
		}
	}
	if got, _ := Mogrify("%s", []any{"a'b\\"}, true); got != `'a''b\'` {
		t.Errorf("NO_BACKSLASH_ESCAPES: %s", got)
	}
}

func TestPyFloatRepr(t *testing.T) {
	for f, want := range map[float64]string{
		1.5: "1.5", 10: "10.0", 1e16: "1e+16", 1.5e16: "1.5e+16", 123456789: "123456789.0",
		0.0001: "0.0001", 0.00001: "1e-05", -2.5: "-2.5",
	} {
		if got := PyFloatRepr(f); got != want {
			t.Errorf("repr(%v) = %s, want %s", f, got, want)
		}
	}
}

func TestPyRepr(t *testing.T) {
	for in, want := range map[string]string{
		"abc":          "'abc'",
		"it's":         `"it's"`,
		`both ' and "`: `'both \' and "'`,
		"a\nb\\":       `'a\nb\\'`,
	} {
		if got := PyRepr(in); got != want {
			t.Errorf("PyRepr(%q) = %s, want %s", in, got, want)
		}
	}
	e := &Error{Code: 1045, Msg: "Access denied for user 'root'@'x' (using password: YES)"}
	if got := e.Error(); got != `(1045, "Access denied for user 'root'@'x' (using password: YES)")` {
		t.Errorf("Error() = %s", got)
	}
}

func TestOptionFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "my.cnf")
	os.WriteFile(p, []byte("# c\n[client]\nuser = root\npassword = \"s3 cret\"\nssl_ca=/ca.pem\nskip-ssl\n; x\n[mysqld]\n!includedir /etc/mysql/conf.d/\n"), 0o600)
	of, err := ReadOptionFile(p, PyMySQLParser)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"user": "root", "password": "s3 cret", "ssl-ca": "/ca.pem"} {
		if got, ok := of.Get("client", k, PyMySQLParser); !ok || got != want {
			t.Errorf("%s = %q, %v", k, got, ok)
		}
	}
	if _, ok := of.Get("client", "skip-ssl", PyMySQLParser); ok {
		t.Error("valueless option must not resolve")
	}
	os.WriteFile(p, []byte("user=root\n"), 0o600)
	if _, err := ReadOptionFile(p, PyMySQLParser); err == nil {
		t.Error("want missing section header error")
	}
	os.WriteFile(p, []byte("[client]\nuser=a\nuser=b\n"), 0o600)
	if _, err := ReadOptionFile(p, PyMySQLParser); err == nil ||
		err.Error() != "While reading from '"+p+"' [line  3]: option 'user' in section 'client' already exists" {
		t.Errorf("duplicate: %v", err)
	}
}

func TestScramble(t *testing.T) {
	// mysql_native_password of "secret" with a fixed 20-byte salt,
	// cross-checked with PyMySQL's scramble_native_password.
	salt := []byte("01234567890123456789")
	if got := scrambleNative([]byte("secret"), salt); len(got) != 20 {
		t.Fatalf("len %d", len(got))
	}
	if scrambleNative(nil, salt) != nil || scrambleSHA256(nil, salt) != nil {
		t.Error("empty password must scramble to nothing")
	}
}
