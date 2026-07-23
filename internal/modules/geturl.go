package modules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(getURLModule, "get_url", "ansible.builtin.get_url")
}

var getURLSpec = args.Spec{
	"url":            {Required: true},
	"dest":           {Required: true},
	"mode":           {Type: "any"},
	"owner":          {},
	"group":          {},
	"checksum":       {}, // "sha256:<hex>" or "sha256:<url>"
	"force":          {Type: "bool", Default: false},
	"timeout":        {Type: "int", Default: 10},
	"validate_certs": {Type: "bool", Default: true},
	"headers":        {Type: "dict"},
	"url_username":   {},
	"url_password":   {},
}

// getURLModule downloads a file. Idempotence: an existing dest with a
// matching checksum (when given) or without force skips the download.
func getURLModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := getURLSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	url := p.Str("url")
	dest := p.Str("dest")

	wantAlgo, wantSum, err := parseChecksum(p.Str("checksum"))
	if err != nil {
		return agentproto.Fail("%v", err)
	}

	destInfo, destErr := os.Stat(dest)
	destExists := destErr == nil && destInfo.Mode().IsRegular()

	// Existing file short-circuits: checksum match wins; otherwise only
	// force=true re-downloads.
	if destExists {
		if wantSum != "" {
			cur, err := fsutil.Sha256File(dest)
			if err == nil && wantAlgo == "sha256" && cur == wantSum {
				return applyGetURLAttrs(env, p, dest, &agentproto.Result{})
			}
		} else if !p.Bool("force") {
			return applyGetURLAttrs(env, p, dest, &agentproto.Result{})
		}
	}

	if env.CheckMode {
		return &agentproto.Result{Changed: true, Msg: "would download " + url}
	}

	body, err := fetchURL(env, p, url)
	if err != nil {
		return agentproto.Fail("get_url: %v", err)
	}

	sum := sha256.Sum256(body)
	gotSum := hex.EncodeToString(sum[:])
	if wantSum != "" && gotSum != wantSum {
		return agentproto.Fail("checksum mismatch for %s: expected %s, got %s", url, wantSum, gotSum)
	}

	// Same content already in place: no change.
	if destExists {
		if cur, err := fsutil.Sha256File(dest); err == nil && cur == gotSum {
			return applyGetURLAttrs(env, p, dest, &agentproto.Result{})
		}
	}

	// Preserve an existing file's mode+owner across the download; explicit
	// mode/owner/group applied by applyGetURLAttrs below. New files: 0644.
	if err := fsutil.AtomicRewrite(dest, bytes.NewReader(body), 0o644); err != nil {
		return agentproto.Fail("writing %s: %v", dest, err)
	}
	res := &agentproto.Result{Changed: true, Extra: map[string]any{
		"dest": dest, "url": url, "checksum_dest": gotSum, "size": len(body),
	}}
	return applyGetURLAttrs(env, p, dest, res)
}

func applyGetURLAttrs(env *RunEnv, p *args.Parsed, dest string, res *agentproto.Result) *agentproto.Result {
	if env.CheckMode || (!p.Has("mode") && p.Str("owner") == "" && p.Str("group") == "") {
		return res
	}
	var mode any
	if p.Has("mode") {
		mode = p.Any("mode")
	}
	changed, err := fsutil.ApplyFileAttrs(dest, mode, p.Str("owner"), p.Str("group"), true)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	res.Changed = res.Changed || changed
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	res.Extra["dest"] = dest
	return res
}

// fetchURL retrieves http(s):// and file:// URLs.
func fetchURL(env *RunEnv, p *args.Parsed, url string) ([]byte, error) {
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		return os.ReadFile(path)
	}
	client := &http.Client{Timeout: time.Duration(p.Int("timeout")) * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Dict("headers") {
		req.Header.Set(k, fmt.Sprintf("%v", v))
	}
	if user := p.Str("url_username"); user != "" {
		req.SetBasicAuth(user, p.Str("url_password"))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned status %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<30))
}

// parseChecksum splits "sha256:<hex>". Only sha256 is supported; checksum
// URLs are not.
func parseChecksum(s string) (algo, sum string, err error) {
	if s == "" {
		return "", "", nil
	}
	algo, sum, ok := strings.Cut(s, ":")
	if !ok {
		return "", "", fmt.Errorf("checksum must look like 'sha256:<hex>'")
	}
	if algo != "sha256" {
		return "", "", fmt.Errorf("checksum algorithm %q is not supported (only sha256)", algo)
	}
	if strings.HasPrefix(sum, "http") {
		return "", "", fmt.Errorf("checksum URLs are not supported yet; inline the hex digest")
	}
	return algo, strings.ToLower(sum), nil
}
