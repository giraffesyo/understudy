package connection

import (
	"context"
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/embedded"
)

// Bootstrap detects the target platform and ensures the agent binary is
// present, returning its remote path. Checksum-named paths make version
// handshakes unnecessary: a new build is a new filename.
func Bootstrap(ctx context.Context, conn Connection, remoteTmp string) (string, error) {
	// One probe resolves platform and the concrete home/user (candidate
	// paths must be literal so quoting is consistent everywhere).
	res, err := conn.Exec(ctx, `uname -sm && echo "$HOME" && echo "$USER"`, ExecOptions{})
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) < 3 {
		return "", fmt.Errorf("unexpected platform-probe output %q", string(res.Stdout))
	}
	goos, goarch, err := parseUname(strings.TrimSpace(lines[0]))
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(lines[1])
	user := strings.TrimSpace(lines[2])
	ag, err := embedded.Agent(goos, goarch)
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("agent-%s-%s-%s", ag.Sha12, goos, goarch)
	var candidates []string
	if remoteTmp != "" {
		candidates = append(candidates, remoteTmp)
	}
	if home != "" {
		candidates = append(candidates, home+"/.understudy")
	}
	if user != "" {
		candidates = append(candidates, "/tmp/.understudy-"+user)
	}
	candidates = append(candidates, "/var/tmp/.understudy")

	var lastErr error
	for _, dir := range candidates {
		path := dir + "/" + name
		q := ShellQuote(path)
		// Present and executable? The checksum in the name vouches for
		// integrity (verified at upload time).
		probe, err := conn.Exec(ctx, fmt.Sprintf(`test -x %s && %s version`, q, q), ExecOptions{})
		if err != nil {
			return "", err
		}
		if probe.RC == 0 && strings.Contains(string(probe.Stdout), "understudy-agent") {
			return path, nil
		}

		if err := uploadAgent(ctx, conn, ag, dir, path); err != nil {
			lastErr = err
			continue // try the next candidate dir (read-only home, noexec...)
		}
		// The real proof: it executes (catches noexec mounts that accept
		// writes happily).
		probe, err = conn.Exec(ctx, q+" version", ExecOptions{})
		if err != nil {
			return "", err
		}
		if probe.RC == 0 && strings.Contains(string(probe.Stdout), "understudy-agent") {
			return path, nil
		}
		lastErr = fmt.Errorf("agent at %s does not execute (noexec mount?): rc=%d %s",
			path, probe.RC, strings.TrimSpace(string(probe.Stderr)))
	}
	return "", fmt.Errorf("could not install the agent on the target: %v", lastErr)
}

func uploadAgent(ctx context.Context, conn Connection, ag *embedded.AgentBinary, dir, path string) error {
	stream, err := embedded.GunzipReader(ag.GzData)
	if err != nil {
		return err
	}
	defer stream.Close()

	tmp := path + ".upload"
	if err := conn.WriteFile(ctx, tmp, stream, 0o700); err != nil {
		return fmt.Errorf("uploading agent to %s: %w", dir, err)
	}

	// Verify the upload before moving into the checksum-named path.
	verify := fmt.Sprintf(
		`sum=$(sha256sum %s 2>/dev/null || shasum -a 256 %s 2>/dev/null) && echo "$sum" | grep -q "^%s" && mv -f %s %s`,
		ShellQuote(tmp), ShellQuote(tmp), sha12Prefix(ag), ShellQuote(tmp), ShellQuote(path))
	res, err := conn.Exec(ctx, verify, ExecOptions{})
	if err != nil {
		return err
	}
	if res.RC != 0 {
		// No checksum tool on the target: fall back to executing the binary
		// and matching its self-reported identity.
		fallback := fmt.Sprintf(`%s version && mv -f %s %s`, ShellQuote(tmp), ShellQuote(tmp), ShellQuote(path))
		res, err = conn.Exec(ctx, fallback, ExecOptions{})
		if err != nil {
			return err
		}
		if res.RC != 0 || !strings.Contains(string(res.Stdout), "understudy-agent") {
			conn.Exec(ctx, "rm -f "+ShellQuote(tmp), ExecOptions{})
			return fmt.Errorf("agent verification failed after upload: %s", strings.TrimSpace(string(res.Stderr)))
		}
	}
	return nil
}

// sha12Prefix returns the full-sha prefix used in the grep (the name holds
// 12 chars; sha256sum output starts with the full hash, so the 12-char
// prefix anchors correctly).
func sha12Prefix(ag *embedded.AgentBinary) string { return ag.Sha12 }

func parseUname(out string) (goos, goarch string, err error) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected `uname -sm` output %q", out)
	}
	switch fields[0] {
	case "Linux":
		goos = "linux"
	default:
		return "", "", fmt.Errorf(
			"target OS %q is not supported by the agent yet (only Linux); the raw module still works", fields[0])
	}
	switch fields[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf(
			"target architecture %q is not supported yet (amd64/arm64 only); the raw module still works", fields[1])
	}
	return goos, goarch, nil
}
