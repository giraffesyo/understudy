package mysqlclient

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestLive exercises a real server when UNDERSTUDY_MYSQL_TEST is set to
// "host:port:user:password" (e.g. a docker mysql/mariadb container).
func TestLive(t *testing.T) {
	spec := os.Getenv("UNDERSTUDY_MYSQL_TEST")
	if spec == "" {
		t.Skip("UNDERSTUDY_MYSQL_TEST not set")
	}
	for _, one := range strings.Split(spec, ",") {
		parts := strings.SplitN(one, ":", 4)
		port, _ := strconv.Atoi(parts[1])
		for _, ssl := range []*SSL{nil, {}} {
			cfg := Config{Host: parts[0], Port: port, User: parts[2], Password: parts[3], SSL: ssl}
			c, err := Connect(cfg)
			if err != nil {
				t.Fatalf("%s: connect: %v", one, err)
			}
			res, err := c.Query("SELECT VERSION(), 1+1, NULL, CAST('2024-01-02 03:04:05' AS DATETIME), 1.50, @@autocommit")
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != 1 || res.Rows[0][2] != nil || string(res.Rows[0][1]) != "2" {
				t.Fatalf("bad row: %q", res.Rows)
			}
			for i, f := range res.Fields {
				t.Logf("%s ssl=%v %s = %#v", one, ssl != nil, f.Name, f.Convert(res.Rows[0][i]))
			}
			if _, err := c.Query("SELEC 1"); err == nil || !strings.HasPrefix(err.Error(), "(1064, ") {
				t.Fatalf("want syntax error, got %v", err)
			}
			c.Close()
		}
		// caching_sha2_password full authentication over a plain
		// connection (RSA public key exchange): flush the auth cache first.
		c, err := Connect(Config{Host: parts[0], Port: port, User: parts[2], Password: parts[3]})
		if err != nil {
			t.Fatal(err)
		}
		c.Query("FLUSH PRIVILEGES")
		c.Close()
		c, err = Connect(Config{Host: parts[0], Port: port, User: parts[2], Password: parts[3], NoTLS: true})
		if err != nil {
			t.Fatalf("plain connect after flush: %v", err)
		}
		c.Close()
		// wrong password
		_, err = Connect(Config{Host: parts[0], Port: port, User: parts[2], Password: "nope"})
		if err == nil || !strings.HasPrefix(err.Error(), "(1045, ") {
			t.Fatalf("want access denied, got %v", err)
		}
		t.Log(err)
	}
}
