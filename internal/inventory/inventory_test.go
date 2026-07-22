package inventory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const sampleINI = `
mail.example.com ansible_port=2222

[web]
web[01:03].example.com http_port=80

[db]
db1.example.com
db2.example.com ansible_host=10.0.0.5

[site:children]
web
db

[site:vars]
env=prod
retries=3

[web:vars]
http_port=8080
env=staging
`

func loadSample(t *testing.T) *Inventory {
	t.Helper()
	inv := New()
	if err := LoadINI(inv, []byte(sampleINI), "hosts.ini"); err != nil {
		t.Fatal(err)
	}
	if err := inv.finalize(); err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestINIBasics(t *testing.T) {
	inv := loadSample(t)
	if len(inv.Hosts) != 6 {
		t.Fatalf("hosts = %v", inv.SortedHostNames())
	}
	// Range expansion with zero padding.
	if _, ok := inv.Hosts["web02.example.com"]; !ok {
		t.Error("web02.example.com missing (range expansion)")
	}
	// Inline host vars are coerced.
	if v := inv.Hosts["mail.example.com"].Vars["ansible_port"]; v != int64(2222) {
		t.Errorf("ansible_port = %#v, want int64", v)
	}
	// Ungrouped: mail is in no explicit group.
	mail := inv.Hosts["mail.example.com"]
	if _, ok := mail.groups["ungrouped"]; !ok {
		t.Error("mail should be in ungrouped")
	}
}

func TestGroupVarPrecedence(t *testing.T) {
	inv := loadSample(t)
	web1 := inv.Hosts["web01.example.com"]
	vars := inv.EffectiveVars(web1)
	// Host-line inline vars beat group vars.
	if vars["http_port"] != int64(80) {
		t.Errorf("http_port = %#v, want 80 (host var beats group var)", vars["http_port"])
	}
	// web is deeper than site (site -> web), so web:vars wins over site:vars.
	if vars["env"] != "staging" {
		t.Errorf("env = %#v, want staging (child group overrides parent)", vars["env"])
	}
	// Host var wins over group var.
	if vars["retries"] != int64(3) {
		t.Errorf("retries = %#v", vars["retries"])
	}
	// group_names order: depth then alpha.
	names := inv.GroupNames(web1)
	want := []string{"site", "web"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("group_names = %v, want %v", names, want)
	}
}

func TestPatterns(t *testing.T) {
	inv := loadSample(t)
	cases := []struct {
		pattern string
		want    int
	}{
		{"all", 6},
		{"web", 3},
		{"db", 2},
		{"site", 5},
		{"web:db", 5},
		{"web,db", 5},
		{"site:!db", 3},
		{"all:&web", 3},
		{"web*", 3},
		{"*.example.com", 6},
		{"db1.example.com", 1},
		{"nosuch", 0},
	}
	for _, c := range cases {
		hosts, err := inv.Match(c.pattern)
		if err != nil {
			t.Errorf("Match(%q): %v", c.pattern, err)
			continue
		}
		if len(hosts) != c.want {
			names := make([]string, len(hosts))
			for i, h := range hosts {
				names[i] = h.Name
			}
			t.Errorf("Match(%q) = %v, want %d hosts", c.pattern, names, c.want)
		}
	}
}

func TestYAMLInventory(t *testing.T) {
	src := `
all:
  children:
    web:
      hosts:
        w[1:2]:
          http_port: 80
      vars:
        tier: frontend
    db:
      hosts:
        d1:
          ansible_host: 10.0.0.9
`
	inv := New()
	if err := LoadYAML(inv, []byte(src), "inv.yml"); err != nil {
		t.Fatal(err)
	}
	if err := inv.finalize(); err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 3 {
		t.Fatalf("hosts = %v", inv.SortedHostNames())
	}
	vars := inv.EffectiveVars(inv.Hosts["w1"])
	if vars["tier"] != "frontend" || vars["http_port"] != int64(80) {
		t.Errorf("w1 vars = %#v", vars)
	}
	if inv.Hosts["d1"].Vars["ansible_host"] != "10.0.0.9" {
		t.Errorf("d1 ansible_host = %#v", inv.Hosts["d1"].Vars["ansible_host"])
	}
}

func TestLoadWithVarsDirs(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hosts", "[web]\nw1\nw2\n")
	write("group_vars/all.yml", "domain: example.com\ncommon: from_all\n")
	write("group_vars/web.yml", "common: from_web\nport: 80\n")
	write("host_vars/w1.yml", "port: 8080\n")
	write("group_vars/nosuchgroup.yml", "ignored: true\n")

	inv, err := Load([]string{filepath.Join(dir, "hosts")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w1 := inv.EffectiveVars(inv.Hosts["w1"])
	if w1["domain"] != "example.com" {
		t.Errorf("domain = %#v", w1["domain"])
	}
	if w1["common"] != "from_web" {
		t.Errorf("common = %#v, want group override", w1["common"])
	}
	if w1["port"] != int64(8080) {
		t.Errorf("port = %#v, want host_vars override", w1["port"])
	}
	w2 := inv.EffectiveVars(inv.Hosts["w2"])
	if w2["port"] != int64(80) {
		t.Errorf("w2 port = %#v", w2["port"])
	}
}

func TestLiteralHostList(t *testing.T) {
	inv, err := Load([]string{"h1,h2,"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 {
		t.Errorf("hosts = %v", inv.SortedHostNames())
	}
}

func TestEmptyInventoryImplicitLocalhost(t *testing.T) {
	inv, err := Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := inv.Hosts["localhost"]
	if !ok {
		t.Fatal("implicit localhost missing")
	}
	if h.Vars["ansible_connection"] != "local" {
		t.Error("implicit localhost should default to local connection")
	}
}

func TestRanges(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"web[1:3]", []string{"web1", "web2", "web3"}},
		{"web[01:03]", []string{"web01", "web02", "web03"}},
		{"db[a:c]", []string{"dba", "dbb", "dbc"}},
		{"n[1:5:2]", []string{"n1", "n3", "n5"}},
		{"plain", []string{"plain"}},
		{"w[1:2].x[1:2]", []string{"w1.x1", "w1.x2", "w2.x1", "w2.x2"}},
	}
	for _, c := range cases {
		got, err := ExpandRange(c.in)
		if err != nil {
			t.Errorf("ExpandRange(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExpandRange(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"w[3:1]", "w[1:", "w[1:2:0]"} {
		if _, err := ExpandRange(bad); err == nil {
			t.Errorf("ExpandRange(%q): expected error", bad)
		}
	}
}
