package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/vault"
)

// vaultCmd implements `understudy vault <encrypt|decrypt|view|encrypt_string>`.
func vaultCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: understudy vault <encrypt|decrypt|view|encrypt_string> [options] [file]")
		return 1
	}
	sub := args[0]
	rest := args[1:]

	var passFile, output, name string
	var files []string
	var ask bool
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--vault-password-file" || a == "--vault-pass-file":
			i++
			if i < len(rest) {
				passFile = rest[i]
			}
		case a == "--ask-vault-pass":
			ask = true
		case a == "--output":
			i++
			if i < len(rest) {
				output = rest[i]
			}
		case a == "--name":
			i++
			if i < len(rest) {
				name = rest[i]
			}
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "understudy vault: unknown flag %q\n", a)
			return 1
		default:
			files = append(files, a)
		}
	}

	password, err := resolveVaultPassword(passFile, ask)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}

	switch sub {
	case "encrypt":
		return vaultEncryptFiles(files, password, output)
	case "decrypt":
		return vaultDecryptFiles(files, password, output)
	case "view":
		return vaultView(files, password)
	case "encrypt_string":
		return vaultEncryptString(files, password, name)
	}
	fmt.Fprintf(os.Stderr, "understudy vault: unknown subcommand %q\n", sub)
	return 1
}

func resolveVaultPassword(passFile string, ask bool) (string, error) {
	if passFile != "" {
		return vault.LoadPasswordFile(passFile)
	}
	if env := os.Getenv("ANSIBLE_VAULT_PASSWORD_FILE"); env != "" && !ask {
		return vault.LoadPasswordFile(env)
	}
	return promptSecret("Vault password")
}

func vaultEncryptFiles(files []string, password, output string) int {
	if len(files) == 0 {
		return vaultStreamEncrypt(password)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		if vault.IsEncrypted(data) {
			fmt.Fprintf(os.Stderr, "ERROR! %s is already encrypted\n", f)
			return 1
		}
		payload, err := vault.Encrypt(data, password)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		dest := f
		if output != "" {
			dest = output
		}
		if err := os.WriteFile(dest, []byte(payload), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		fmt.Printf("Encryption successful: %s\n", dest)
	}
	return 0
}

func vaultDecryptFiles(files []string, password, output string) int {
	if len(files) == 0 {
		return vaultStreamDecrypt(password)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		plain, err := vault.Decrypt(string(data), password)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		dest := f
		if output != "" {
			dest = output
		}
		if err := os.WriteFile(dest, plain, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		fmt.Printf("Decryption successful: %s\n", dest)
	}
	return 0
}

func vaultView(files []string, password string) int {
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		plain, err := vault.Decrypt(string(data), password)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
			return 1
		}
		os.Stdout.Write(plain)
	}
	return 0
}

// vaultEncryptString encrypts a value and prints it in the !vault YAML form.
func vaultEncryptString(values []string, password, name string) int {
	var plaintext string
	if len(values) > 0 {
		plaintext = values[0]
	} else {
		data, _ := io.ReadAll(os.Stdin)
		plaintext = strings.TrimRight(string(data), "\n")
	}
	payload, err := vault.Encrypt([]byte(plaintext), password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	if name == "" {
		name = "myvar"
	}
	fmt.Printf("%s: !vault |\n", name)
	for _, line := range strings.Split(strings.TrimRight(payload, "\n"), "\n") {
		fmt.Printf("          %s\n", line)
	}
	return 0
}

func vaultStreamEncrypt(password string) int {
	data, _ := io.ReadAll(os.Stdin)
	payload, err := vault.Encrypt(data, password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	fmt.Print(payload)
	return 0
}

func vaultStreamDecrypt(password string) int {
	data, _ := io.ReadAll(os.Stdin)
	plain, err := vault.Decrypt(string(data), password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR! %v\n", err)
		return 1
	}
	os.Stdout.Write(plain)
	return 0
}
