package vault

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Secrets holds one or more vault passwords to try when decrypting. A
// payload may have been encrypted with any of several vault IDs, so
// decryption tries each in turn.
type Secrets struct {
	passwords []string
}

// NewSecrets builds a Secrets set from passwords (order preserved).
func NewSecrets(passwords ...string) *Secrets {
	return &Secrets{passwords: passwords}
}

// Empty reports whether any password is available.
func (s *Secrets) Empty() bool { return s == nil || len(s.passwords) == 0 }

// Add appends a password.
func (s *Secrets) Add(pw string) { s.passwords = append(s.passwords, pw) }

// Decrypt tries each password against the payload.
func (s *Secrets) Decrypt(payload string) ([]byte, error) {
	if s.Empty() {
		return nil, fmt.Errorf("vault: an encrypted value was found but no vault password was provided (use --vault-password-file or --ask-vault-pass)")
	}
	var lastErr error
	for _, pw := range s.passwords {
		out, err := Decrypt(payload, pw)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("vault: no configured password could decrypt the payload: %w", lastErr)
}

// DecryptFileContent decrypts data if it is a vault payload; otherwise
// returns it unchanged. Used when loading vars_files, group_vars, and
// extra-vars @files that may be whole-file encrypted.
func (s *Secrets) MaybeDecryptFile(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return data, nil
	}
	return s.Decrypt(string(data))
}

// LoadPasswordFile reads a vault password from a file. If the file is
// executable, it is run and its first stdout line is used (Ansible's
// vault-password-script convention).
func LoadPasswordFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&0o111 != 0 {
		out, err := exec.Command(path).Output()
		if err != nil {
			return "", fmt.Errorf("vault password script %s failed: %w", path, err)
		}
		return firstLine(string(out)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return firstLine(string(data)), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "\r\n")
}
