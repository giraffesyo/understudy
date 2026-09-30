package sshkey

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures in testdata/ were written by python-cryptography (the
// openssh_keypair cryptography backend's library); index.json maps each
// to its OpenSSH public key.
func TestParseCryptographyKeys(t *testing.T) {
	data, err := os.ReadFile("testdata/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]string
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	for file, pub := range index {
		pem, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		encrypted := strings.Contains(file, "_enc")
		pw := []byte("pw")
		k, err := ParsePrivate(pem, map[bool][]byte{true: pw}[encrypted])
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if got := AuthorizedKey(k); got != pub {
			t.Errorf("%s: public key\n got %s\nwant %s", file, got, pub)
		}
		// The other passphrase state is cryptography's TypeError.
		if _, err := ParsePrivate(pem, map[bool][]byte{false: pw}[encrypted]); !errors.Is(err, ErrPassphrase) {
			t.Errorf("%s: wrong passphrase presence: %v", file, err)
		}
		if encrypted {
			var inv *ErrInvalid
			if _, err := ParsePrivate(pem, []byte("bad")); !errors.As(err, &inv) {
				t.Errorf("%s: bad passphrase: %v", file, err)
			}
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		size int
	}{{"rsa", 1024}, {"dsa", 1024}, {"ecdsa", 256}, {"ecdsa", 384}, {"ecdsa", 521}, {"ed25519", 256}} {
		k, err := Generate(tc.typ, tc.size)
		if err != nil {
			t.Fatal(err)
		}
		if typ, size := TypeAndSize(k); typ != tc.typ || size != tc.size {
			t.Errorf("TypeAndSize = %s %d, want %s %d", typ, size, tc.typ, tc.size)
		}
		for _, format := range []string{FormatSSH, FormatPKCS8, FormatPKCS1} {
			for _, pw := range [][]byte{nil, []byte("secret")} {
				pem, err := MarshalPrivate(k, format, pw)
				if tc.typ == "ed25519" && format == FormatPKCS1 {
					if !errors.Is(err, ErrPKCS1Ed25519) {
						t.Errorf("ed25519 PKCS1: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s %s: %v", tc.typ, format, err)
				}
				back, err := ParsePrivate(pem, pw)
				if err != nil {
					t.Fatalf("%s %s %q: %v\n%s", tc.typ, format, pw, err, pem)
				}
				if AuthorizedKey(back) != AuthorizedKey(k) {
					t.Errorf("%s %s: public key changed", tc.typ, format)
				}
				crossCheck(t, pem, pw, AuthorizedKey(k))
			}
		}
	}
}

// crossCheck loads pem with python-cryptography when it is installed
// (UNDERSTUDY_TEST_PYTHON or python3), which must derive the same public
// key.
func crossCheck(t *testing.T, pem, pw []byte, want string) {
	t.Helper()
	py := os.Getenv("UNDERSTUDY_TEST_PYTHON")
	if py == "" {
		py = "python3"
	}
	if err := exec.Command(py, "-c", "import cryptography").Run(); err != nil {
		return
	}
	f := filepath.Join(t.TempDir(), "key")
	os.WriteFile(f, pem, 0o600)
	script := `import sys
from cryptography.hazmat.primitives import serialization as s
data = open(sys.argv[1], 'rb').read()
pw = sys.argv[2].encode() or None
try:
    k = s.load_ssh_private_key(data, pw)
except ValueError:
    k = s.load_pem_private_key(data, pw)
sys.stdout.write(k.public_key().public_bytes(s.Encoding.OpenSSH, s.PublicFormat.OpenSSH).decode())`
	out, err := exec.Command(py, "-W", "ignore", "-c", script, f, string(pw)).CombinedOutput()
	if err != nil || string(out) != want {
		t.Errorf("cryptography cannot load our key (%v):\n%s\n%s", err, out, pem)
	}
}

func TestFingerprint(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	for _, typ := range []string{"rsa", "ecdsa", "ed25519"} {
		size := map[string]int{"rsa": 1024, "ecdsa": 256, "ed25519": 256}[typ]
		k, err := Generate(typ, size)
		if err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(t.TempDir(), "key.pub")
		os.WriteFile(f, []byte(AuthorizedKey(k)+" c\n"), 0o644)
		out, err := exec.Command("ssh-keygen", "-lf", f).Output()
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Fields(string(out))[1]; Fingerprint(AuthorizedKey(k)) != want {
			t.Errorf("%s: Fingerprint = %s, ssh-keygen says %s", typ, Fingerprint(AuthorizedKey(k)), want)
		}
	}
}
