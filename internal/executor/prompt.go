package executor

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// promptVars resolves a play's vars_prompt entries into play vars, as
// ansible-playbook does before the play starts: -e values win; without a
// terminal on stdin nothing is prompted (the default is used, else the
// string "None").
func (r *Runner) promptVars(play *playbook.Play) error {
	if len(play.VarsPrompt) == 0 {
		return nil
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if play.Vars == nil {
		play.Vars = map[string]any{}
	}
	in := bufio.NewReader(os.Stdin)
	for _, vp := range play.VarsPrompt {
		if _, given := r.Opts.ExtraVars[vp.Name]; given {
			continue
		}
		var value string
		if !interactive {
			r.warn("Not prompting as we are not in interactive mode")
			value = template.PyStr(vp.Default)
			if vp.Default == nil {
				value = "None"
			}
		} else {
			v, err := askVar(in, vp)
			if err != nil {
				return err
			}
			value = v
		}
		if vp.Encrypt != "" {
			salt := vp.Salt
			if salt == "" {
				salt = randomSalt(max(vp.SaltSize, 16))
			}
			h, err := template.CryptHash(vp.Encrypt, value, salt, 0)
			if err != nil {
				return fmt.Errorf("vars_prompt %s: %v", vp.Name, err)
			}
			value = h
		}
		if vp.Unsafe {
			play.Vars[vp.Name] = yaml.UnsafeString(value)
		} else {
			play.Vars[vp.Name] = value
		}
	}
	return nil
}

func askVar(in *bufio.Reader, vp playbook.VarPrompt) (string, error) {
	msg := vp.Prompt + ": "
	if vp.Default != nil {
		msg = fmt.Sprintf("%s [%s]: ", vp.Prompt, template.PyStr(vp.Default))
	}
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stdout, prompt)
		if vp.Private {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stdout)
			return string(b), err
		}
		line, err := in.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	for {
		v, err := read(msg)
		if err != nil && v == "" {
			return "", fmt.Errorf("vars_prompt %s: %v", vp.Name, err)
		}
		if vp.Confirm {
			again, err := read("confirm " + msg)
			if err != nil && again == "" {
				return "", fmt.Errorf("vars_prompt %s: %v", vp.Name, err)
			}
			if again != v {
				fmt.Fprintln(os.Stdout, "***** VALUES ENTERED DO NOT MATCH ****")
				continue
			}
		}
		if v == "" && vp.Default != nil {
			v = template.PyStr(vp.Default)
		}
		return v, nil
	}
}

const saltChars = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomSalt(n int) string {
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(saltChars))))
		b[i] = saltChars[k.Int64()]
	}
	return string(b)
}

// warn prints an ansible-style warning to stderr, once per message.
func (r *Runner) warn(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if r.warned[msg] {
		return
	}
	r.warned[msg] = true
	fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", msg)
}
