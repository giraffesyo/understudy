package modules

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	names := []string{"rpm_key", "ansible.builtin.rpm_key"}
	Register(rpmKeyModule, names...)
	for _, n := range names {
		specs[n] = rpmKeySpec
	}
}

var rpmKeySpec = args.Spec{
	"state":          {Default: "present", Choices: []string{"absent", "present"}},
	"key":            {Required: true},
	"fingerprint":    {Type: "list"},
	"validate_certs": {Type: "bool", Default: true},
}

var rpmKeyIDRe = regexp.MustCompile(`(?i)^(0x)?[0-9a-f]{8}`)

// rpmKeyModule ports ansible.builtin.rpm_key: key IDs and fingerprints are
// computed from the armored key in-process (the module does the same via
// librpm packet parsing) and compared with every `rpm -q gpg-pubkey` key.
func rpmKeyModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := rpmKeySpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	rpm, err := lookPath("rpm")
	if err != nil {
		return agentproto.Fail("Failed to find required executable \"rpm\" in paths: %s", os.Getenv("PATH"))
	}
	if _, err := lookPath("rpmkeys"); err != nil {
		return agentproto.Fail("Failed to find required executable \"rpmkeys\" in paths: %s", os.Getenv("PATH"))
	}
	state, key := p.Str("state"), p.Str("key")

	fingerprints := map[string]bool{}
	for _, f := range strElems(p.List("fingerprint")) {
		fingerprints[strings.ToUpper(strings.ReplaceAll(f, " ", ""))] = true
	}

	var keyData []byte
	var keyid string
	switch {
	case strings.Contains(key, "://"):
		body, status, err := fetchURLStatus(key, p.Bool("validate_certs"), 10*time.Second)
		if err != nil || status != 200 {
			msg := "OK"
			if err != nil {
				msg = err.Error()
				if status < 0 {
					msg = "Request failed: " + msg
				}
			}
			return agentproto.Fail("failed to fetch key at %s , error was: %s", MaskURL(key), msg)
		}
		if !isPGPPubkey(body) {
			return agentproto.Fail("Not a public key: %s", MaskURL(key))
		}
		keyData = body
	case rpmKeyIDRe.MatchString(strings.ReplaceAll(key, " ", "")):
		keyid = key
	default:
		if st, err := os.Stat(key); err == nil && st.Mode().IsRegular() {
			keyData, err = os.ReadFile(key)
			if err != nil {
				return agentproto.Fail("%v", err)
			}
		} else {
			return agentproto.Fail("Not a valid key %s", key)
		}
	}
	if keyData != nil {
		infos, err := pgpIdentifyKeys(keyData)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		if len(infos) == 0 {
			return agentproto.Fail("Failed to get keyid")
		}
		keyid = infos[0].KeyID
	}
	keyid = strings.ToUpper(strings.TrimSpace(keyid))
	keyid = strings.TrimPrefix(keyid, "0X")

	installed, err := rpmInstalledKeys(env, rpm)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	imported := rpmKeyImported(installed, keyid)
	res := &agentproto.Result{}

	if state == "present" {
		if imported {
			return res
		}
		if keyData == nil {
			return agentproto.Fail("When importing a key, a valid file must be given")
		}
		if len(fingerprints) > 0 {
			infos, _ := pgpIdentifyKeys(keyData)
			have := map[string]bool{}
			for _, k := range infos {
				have[k.Fingerprint] = true
			}
			for f := range fingerprints {
				if !have[f] {
					return agentproto.Fail("The specified fingerprint, '%s', does not match any key fingerprints in '%s'",
						pySet(fingerprints), "frozenset("+pySet(have)+")")
				}
			}
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		keyfile := key
		if strings.Contains(key, "://") {
			tmp, err := os.CreateTemp("", "rpmkey")
			if err != nil {
				return agentproto.Fail("%v", err)
			}
			defer os.Remove(tmp.Name())
			tmp.Write(keyData)
			tmp.Close()
			keyfile = tmp.Name()
		}
		if out, err := runOutSplit(env, rpm, "--import", keyfile); err != nil {
			return agentproto.Fail("%s", out)
		}
		return res
	}

	if !imported {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if rpmVersionAtLeast6(env, rpm) {
		var fps []string
		for _, k := range installed {
			if strings.HasSuffix(k.KeyID, keyid) || k.Fingerprint == keyid {
				fps = append(fps, k.Fingerprint)
			}
		}
		switch len(fps) {
		case 0:
			return agentproto.Fail("Supplied key ID %s is not installed.", keyid)
		case 1:
			if out, err := runOutSplit(env, "rpmkeys", "--delete", fps[0]); err != nil {
				return agentproto.Fail("%s", out)
			}
		default:
			return agentproto.Fail("Supplied key ID %s matches more than one fingerprint. Try using the fingerprint instead.", keyid)
		}
		return res
	}
	for _, k := range installed {
		if k.Fingerprint == keyid {
			keyid = k.KeyID
			break
		}
	}
	short := keyid
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	if out, err := runOutSplit(env, rpm, "--erase", "--allmatches", "gpg-pubkey-"+strings.ToLower(short)); err != nil {
		return agentproto.Fail("%s", out)
	}
	return res
}

// runOutSplit runs a command and returns stderr (the module's fail_json
// message) on failure.
func runOutSplit(env *RunEnv, name string, argv ...string) (string, error) {
	out, err := runOut(env, name, argv...)
	return strings.TrimRight(out, "\n"), err
}

func rpmKeyImported(installed []pgpKeyInfo, keyid string) bool {
	for _, k := range installed {
		if strings.HasSuffix(k.KeyID, keyid) || k.Fingerprint == keyid {
			return true
		}
	}
	return false
}

// rpmInstalledKeys parses every armored gpg-pubkey description.
func rpmInstalledKeys(env *RunEnv, rpm string) ([]pgpKeyInfo, error) {
	if _, err := runOut(env, rpm, "-q", "gpg-pubkey"); err != nil {
		return nil, nil
	}
	out, err := runOut(env, rpm, "-q", "gpg-pubkey", "--qf", "%{description}")
	if err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	var keys []pgpKeyInfo
	var block []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "-----BEGIN PGP PUBLIC KEY BLOCK-----":
			in, block = true, []string{line}
		case "-----END PGP PUBLIC KEY BLOCK-----":
			block = append(block, line)
			infos, err := pgpIdentifyKeys([]byte(strings.Join(block, "\n")))
			if err != nil {
				return nil, err
			}
			keys = append(keys, infos...)
			in, block = false, nil
		default:
			if in {
				block = append(block, line)
			}
		}
	}
	return keys, nil
}

func rpmVersionAtLeast6(env *RunEnv, rpm string) bool {
	out, _ := runOut(env, rpm, "--version")
	f := strings.Fields(out)
	if len(f) == 0 {
		return false
	}
	major, _ := strconv.Atoi(strings.SplitN(f[len(f)-1], ".", 2)[0])
	return major >= 6
}

// pySet renders a set of strings like Python's repr ({'A', 'B'}).
func pySet(m map[string]bool) string {
	var items []string
	for k := range m {
		items = append(items, "'"+k+"'")
	}
	sort.Strings(items)
	return "{" + strings.Join(items, ", ") + "}"
}
