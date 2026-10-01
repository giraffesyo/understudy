package factcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An ansible-core jsonfile cache file (tags included) reads back and is
// written again byte for byte.
const ansibleFile = `{"__payload__": "{\"module_setup\": true, \"foo\": {\"value\": \"bar\", \"tags\": [{\"path\": \"/x/p.yml\", \"line_num\": 6, \"col_num\": 14, \"__ansible_type\": \"Origin\"}, {\"__ansible_type\": \"TrustedAsTemplate\"}], \"__ansible_type\": \"_AnsibleTaggedStr\"}, \"n\": {\"value\": 1.5, \"tags\": [{\"path\": \"/x/p.yml\", \"line_num\": 7, \"col_num\": 12, \"__ansible_type\": \"Origin\"}], \"__ansible_type\": \"_AnsibleTaggedFloat\"}, \"u\": \"\\u00e9\"}"}`

func TestJSONFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pre_s1_h1"), []byte(ansibleFile), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Open(Settings{Plugin: "jsonfile", URI: dir, Prefix: "pre_", Timeout: 86400}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Get("h1")
	if err != nil || f == nil {
		t.Fatalf("get: %v %v", f, err)
	}
	if v, _ := f.Get("foo"); v != "bar" {
		t.Errorf("foo = %#v", v)
	}
	if v, _ := f.Get("u"); v != "é" {
		t.Errorf("u = %#v", v)
	}
	if err := c.Set("h1", f); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pre_s1_h1"))
	if string(data) != ansibleFile {
		t.Errorf("rewritten:\n%s\nwant:\n%s", data, ansibleFile)
	}
	st, _ := os.Stat(filepath.Join(dir, "pre_s1_h1"))
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %o", st.Mode().Perm())
	}
}

func TestJSONFileExpiryAndKeys(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Settings{Plugin: "jsonfile", URI: dir, Timeout: 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFacts()
	f.Set("a", int64(1), int64(1))
	c.Set("h/1", f)
	// A key with path characters is stored under its hash (sha256 of
	// "s1_h/1", as ansible-core names it).
	name := filepath.Join(dir, "cb25303cc5d1")
	if _, err := os.Stat(name); err != nil {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("hashed file missing: %v", entries)
	}
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(name, old, old)
	// A new instance (another run) finds the file expired.
	c2, _ := Open(Settings{Plugin: "jsonfile", URI: dir, Timeout: 60}, nil)
	if c2.Contains("h/1") {
		t.Error("expired file is still contained")
	}
	if f, err := c2.Get("h/1"); f != nil || err != nil {
		t.Errorf("expired get: %v %v", f, err)
	}
	// Timeout 0 never expires.
	c3, _ := Open(Settings{Plugin: "jsonfile", URI: dir, Timeout: 0}, nil)
	if f, _ := c3.Get("h/1"); f == nil {
		t.Error("timeout 0 expired the file")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(Settings{Plugin: "redis"}, nil); err == nil || err.Error() != "Unable to load the cache plugin 'redis'." {
		t.Errorf("unknown plugin: %v", err)
	}
	if _, err := Open(Settings{Plugin: "ansible.builtin.jsonfile"}, nil); err == nil || err.Error() != "Required config '_uri' for 'jsonfile' cache plugin not provided." {
		t.Errorf("no uri: %v", err)
	}
}
