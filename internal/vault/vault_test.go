package vault

import (
	"strings"
	"testing"
)

// A payload produced by real `ansible-vault encrypt_string` (password "secret").
const realPayload = `$ANSIBLE_VAULT;1.1;AES256
PLACEHOLDER`

func TestRoundTrip(t *testing.T) {
	for _, plaintext := range []string{"", "s", "hello vault", strings.Repeat("x", 100), "multi\nline\nsecret\n"} {
		payload, err := Encrypt([]byte(plaintext), "secret")
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plaintext, err)
		}
		if !IsEncrypted([]byte(payload)) {
			t.Errorf("IsEncrypted false for our own output")
		}
		got, err := Decrypt(payload, "secret")
		if err != nil {
			t.Fatalf("Decrypt: %v\npayload:\n%s", err, payload)
		}
		if string(got) != plaintext {
			t.Errorf("round trip: got %q, want %q", got, plaintext)
		}
	}
}

func TestWrongPassword(t *testing.T) {
	payload, _ := Encrypt([]byte("secret data"), "right")
	if _, err := Decrypt(payload, "wrong"); err == nil || !strings.Contains(err.Error(), "HMAC") {
		t.Errorf("wrong password should fail HMAC, got %v", err)
	}
}

func TestNotEncrypted(t *testing.T) {
	if IsEncrypted([]byte("plain text")) {
		t.Error("plain text detected as encrypted")
	}
	if !IsEncrypted([]byte("  $ANSIBLE_VAULT;1.1;AES256\nabcd")) {
		t.Error("leading-whitespace vault not detected")
	}
}
