package vault

import (
	"os"
	"strings"
	"testing"
)

// Cross-check against payloads produced by real ansible-vault (password
// "secret"). Regenerate with:
//
//	ansible-vault encrypt_string --vault-password-file <(echo secret) ...
func TestRealAnsibleVaultCompat(t *testing.T) {
	cases := []struct{ file, want string }{
		{"testdata_var.txt", "my-real-secret-value"},
		{"testdata_file.txt", "top_secret: hunter2\n"},
	}
	for _, c := range cases {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Skipf("%s not present (regenerate with ansible-vault): %v", c.file, err)
		}
		got, err := Decrypt(string(data), "secret")
		if err != nil {
			t.Errorf("%s: decrypt failed: %v", c.file, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.file, got, c.want)
		}
	}
	// And our own encryption is decryptable by real ansible-vault (checked
	// by the shell harness, not here).
	_ = strings.TrimSpace
}
