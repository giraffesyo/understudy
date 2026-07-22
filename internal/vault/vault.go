// Package vault implements Ansible Vault 1.1 (AES256) decryption and
// encryption. The format is entirely standard-library:
//
//	$ANSIBLE_VAULT;1.1;AES256
//	<hex body>
//
// The hex body decodes to an ASCII string of three newline-separated hex
// fields — salt, HMAC, ciphertext. A key is derived with PBKDF2-HMAC-SHA256
// (10000 iterations, 80 bytes = 32 cipher key + 32 HMAC key + 16 IV); the
// HMAC authenticates the ciphertext; AES-256-CTR decrypts it; PKCS7 padding
// is stripped.
package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	header     = "$ANSIBLE_VAULT"
	cipherName = "AES256"
	iterations = 10000
	keyLen     = 32
	ivLen      = 16
	saltLen    = 32
)

// IsEncrypted reports whether data is an Ansible Vault payload.
func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(data), []byte(header))
}

// Decrypt decrypts a vault payload with the given password.
func Decrypt(payload string, password string) ([]byte, error) {
	lines := strings.Split(strings.TrimSpace(payload), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("vault: payload is too short")
	}
	head := strings.Split(strings.TrimSpace(lines[0]), ";")
	if len(head) < 3 || strings.TrimSpace(head[0]) != header {
		return nil, fmt.Errorf("vault: missing or malformed %s header", header)
	}
	if c := strings.TrimSpace(head[2]); c != cipherName {
		return nil, fmt.Errorf("vault: unsupported cipher %q (only %s)", c, cipherName)
	}

	bodyHex := strings.Join(trimAll(lines[1:]), "")
	body, err := hex.DecodeString(bodyHex)
	if err != nil {
		return nil, fmt.Errorf("vault: body is not valid hex: %w", err)
	}
	fields := strings.SplitN(string(body), "\n", 3)
	if len(fields) != 3 {
		return nil, fmt.Errorf("vault: expected 3 hex fields, got %d", len(fields))
	}
	salt, err := hex.DecodeString(fields[0])
	if err != nil {
		return nil, fmt.Errorf("vault: bad salt: %w", err)
	}
	expectedHMAC, err := hex.DecodeString(fields[1])
	if err != nil {
		return nil, fmt.Errorf("vault: bad hmac: %w", err)
	}
	ciphertext, err := hex.DecodeString(fields[2])
	if err != nil {
		return nil, fmt.Errorf("vault: bad ciphertext: %w", err)
	}

	cipherKey, hmacKey, iv, err := deriveKeys([]byte(password), salt)
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(ciphertext)
	if subtle.ConstantTimeCompare(mac.Sum(nil), expectedHMAC) != 1 {
		return nil, fmt.Errorf("vault: HMAC verification failed (wrong password or corrupt data)")
	}

	block, err := aes.NewCipher(cipherKey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("vault: ciphertext is not a multiple of the block size")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCTR(block, iv).XORKeyStream(plaintext, ciphertext)

	unpadded, err := pkcs7Unpad(plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	return unpadded, nil
}

// Encrypt produces a vault payload for the given plaintext and password.
// Output lines are wrapped at 80 columns like Ansible's.
func Encrypt(plaintext []byte, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	cipherKey, hmacKey, iv, err := deriveKeys([]byte(password), salt)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	block, err := aes.NewCipher(cipherKey)
	if err != nil {
		return "", err
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCTR(block, iv).XORKeyStream(ciphertext, padded)

	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(ciphertext)

	body := hex.EncodeToString(salt) + "\n" +
		hex.EncodeToString(mac.Sum(nil)) + "\n" +
		hex.EncodeToString(ciphertext)
	bodyHex := hex.EncodeToString([]byte(body))

	var b strings.Builder
	fmt.Fprintf(&b, "%s;1.1;%s\n", header, cipherName)
	for i := 0; i < len(bodyHex); i += 80 {
		end := min(i+80, len(bodyHex))
		b.WriteString(bodyHex[i:end])
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func deriveKeys(password, salt []byte) (cipherKey, hmacKey, iv []byte, err error) {
	derived, err := pbkdf2.Key(sha256.New, string(password), salt, iterations, keyLen*2+ivLen)
	if err != nil {
		return nil, nil, nil, err
	}
	return derived[:keyLen], derived[keyLen : keyLen*2], derived[keyLen*2:], nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty plaintext")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > len(data) || pad > aes.BlockSize {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return data[:len(data)-pad], nil
}

func trimAll(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSpace(l)
	}
	return out
}
