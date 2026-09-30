package executor

import (
	"fmt"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
)

// Deprecated values ansible-core tags in what it hands to templates.
var (
	// A registered empty loop's skipped_reason (UnifiedTaskResult.set_skipped).
	deprecatedSkippedReason = template.Deprecated{Msg: "The 'skipped_reason' value is deprecated.",
		Help: "Use 'skip_reason' instead.", Version: "2.24"}
	// The play_hosts magic variable (VariableManager).
	deprecatedPlayHosts = template.Deprecated{Msg: "The `play_hosts` magic variable is deprecated.",
		Help: "Use `ansible_play_batch` instead.", Version: "2.23"}
	// Facts injected as top-level variables while INJECT_FACTS_AS_VARS is
	// left at its default.
	deprecatedTopLevelFact = template.Deprecated{
		Msg:     "INJECT_FACTS_AS_VARS default to `True` is deprecated, top-level facts will not be auto injected after the change.",
		Help:    "Use `ansible_facts[\"fact_name\"]` (no `ansible_` prefix) instead.",
		Version: "2.24"}
)

// deprecate wraps v in the deprecation d.
func deprecate(d template.Deprecated, v any) template.Deprecated {
	d.Value = v
	return d
}

// deprecatedFacts is facts as injected at top level: each value but
// ansible_local tagged deprecated, unless the configuration sets
// INJECT_FACTS_AS_VARS explicitly.
func (r *Runner) deprecatedFacts(facts map[string]any) map[string]any {
	if r.Opts.InjectFactsSet {
		return facts
	}
	out := make(map[string]any, len(facts))
	for k, v := range facts {
		if k == "ansible_local" {
			out[k] = v
			continue
		}
		out[k] = deprecate(deprecatedTopLevelFact, v)
	}
	return out
}

// deprecation prints ansible-core's deprecation warning for a deprecated
// value a template read at pos (Display.deprecated): a one-time hint on
// how to silence them, then the message, its origin with the source
// excerpt, and the help text. Identical warnings print once.
func (r *Runner) deprecation(pos template.Position, d template.Deprecated) {
	if r.Opts.NoDeprecationWarnings {
		return
	}
	var b strings.Builder
	if pos == template.ContainerOrigin {
		// Found while finishing a templated container: ansible-core
		// names the container, whose origin it does not know.
		fmt.Fprintf(&b, "[DEPRECATION WARNING]: While processing '%s': %s This feature will be removed from ansible-core version %s.\n",
			pos.File, d.Msg, d.Version)
		fmt.Fprintf(&b, "Origin: <unknown>\n\n%s\n\n", pos.File)
	} else {
		fmt.Fprintf(&b, "[DEPRECATION WARNING]: %s This feature will be removed from ansible-core version %s.\n", d.Msg, d.Version)
	}
	if pos.File != "" && pos.Line > 0 {
		fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n%s\n", pos.File, pos.Line, pos.Col, template.SourceExcerpt(pos.File, pos.Line, pos.Col))
	}
	if d.Help != "" {
		b.WriteString(d.Help + "\n")
	}
	b.WriteString("\n")
	msg := b.String()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if r.warned[msg] {
		return
	}
	r.warned[msg] = true
	const hint = "[WARNING]: Deprecation warnings can be disabled by setting `deprecation_warnings=False` in ansible.cfg.\n"
	if !r.warned[hint] {
		r.warned[hint] = true
		fmt.Fprint(os.Stderr, hint)
	}
	fmt.Fprint(os.Stderr, msg)
}
