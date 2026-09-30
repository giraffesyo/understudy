package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// chainError is an ansible-core error event with the chain of its causes,
// rendered as its Display renders events.
type chainError struct {
	msg, help string
	ctx       string // formatted source context ("Origin: ..." and excerpt)
	cause     *chainError
	// hiddenCause: the error was raised while handling another one, a
	// link that is never shown but keeps the error from collapsing into
	// the one it causes.
	hiddenCause bool
}

func (e *chainError) Error() string { return e.brief() }

// asChain converts an error into an event: a YAML error keeps its origin
// and source excerpt.
func asChain(err error) *chainError {
	var ce *chainError
	if errors.As(err, &ce) {
		return ce
	}
	var ye *yaml.Error
	if errors.As(err, &ye) {
		out := &chainError{msg: ye.Message(), help: strings.TrimRight(ye.HelpText(), "\n")}
		if file, line, col := ye.Origin(); line > 0 {
			out.ctx = fmt.Sprintf("Origin: %s:%d:%d\n\n%s", file, line, col, strings.TrimRight(template.SourceExcerpt(file, line, col), "\n"))
		} else if ye.HasOrigin() {
			out.ctx = "Origin: " + file
		}
		return out
	}
	return &chainError{msg: err.Error()}
}

// concatMessage is ansible-core's _text_utils.concat_message.
func concatMessage(left, right string) string {
	return strings.TrimRight(left, ". ") + ": " + right
}

// dedupParts is _event_utils.deduplicate_message_parts.
func dedupParts(parts []string) string {
	msg := parts[len(parts)-1]
	for i := len(parts) - 2; i >= 0; i-- {
		if strings.HasSuffix(parts[i], msg) {
			msg = parts[i]
		} else {
			msg = concatMessage(parts[i], msg)
		}
	}
	return msg
}

// brief is format_event_brief_message.
func (e *chainError) brief() string {
	var parts []string
	for c := e; c != nil; c = c.cause {
		parts = append(parts, c.msg)
	}
	return dedupParts(parts)
}

func messageLines(msg, help, ctx string) string {
	if help != "" && ctx == "" && !strings.Contains(msg, "\n") && !strings.Contains(help, "\n") {
		return msg + " " + help
	}
	lines := []string{msg}
	if ctx != "" {
		lines = append(lines, ctx)
	}
	if help != "" {
		lines = append(lines, "", help)
	}
	return strings.Join(lines, "\n")
}

// format is format_event: the verbose message, each cause that cannot be
// collapsed into its parent after "<<< caused by >>>".
func (e *chainError) format() string {
	var segs []string
	for ev := e; ev != nil; {
		msgs := []string{ev.msg}
		c := ev.cause
		for c != nil {
			if (c.ctx != "" || c.help != "") && (c.ctx != ev.ctx || c.help != ev.help) {
				break
			}
			if c.hiddenCause {
				break
			}
			msgs = append(msgs, c.msg)
			c = c.cause
		}
		segs = append(segs, messageLines(dedupParts(msgs), ev.help, ev.ctx)+"\n")
		if c != nil {
			segs = append(segs, "\n<<< caused by >>>\n\n")
		}
		ev = c
	}
	if len(segs) > 1 {
		segs = append([]string{e.brief() + "\n\n"}, segs...)
	}
	msg := strings.TrimSpace(strings.Join(segs, ""))
	if strings.Contains(msg, "\n") {
		return msg + "\n\n"
	}
	return msg + "\n"
}

// warnEvent shows an error event as a warning (Display.error_as_warning).
func (inv *Inventory) warnEvent(e *chainError) {
	if inv.warn != nil {
		inv.warn(strings.TrimSuffix(e.format(), "\n"))
	}
}

// addHost is InventoryData.add_host: a new host records the source it
// came from (inventory_file, inventory_dir) and its port; the host joins
// group when one is given.
func (inv *Inventory) addHost(name string, g *Group, port int) *Host {
	h, existed := inv.Hosts[name]
	if !existed {
		h = inv.ensureHost(name)
		if inv.currentSource != "" {
			h.Vars["inventory_file"] = inv.currentSource
			h.Vars["inventory_dir"] = basedir(inv.currentSource)
		} else {
			h.Vars["inventory_file"] = nil
			h.Vars["inventory_dir"] = nil
		}
		if port >= 0 {
			h.Vars["ansible_port"] = int64(port)
		}
	}
	if g != nil {
		addHostToGroup(g, h)
	}
	return h
}

// basedir is ansible-core's utils.path.basedir for an inventory source.
func basedir(src string) string {
	info, err := os.Stat(src)
	switch {
	case err == nil && info.IsDir():
		return absPath(src)
	case err == nil && info.Mode().IsRegular():
		return absPath(filepath.Dir(src))
	}
	return "None" // str(None)
}

// asMapping returns a mapping's plain form.
func asMapping(v any) (map[string]any, bool) {
	return yaml.PlainMap(v)
}

// mappingKeys lists a mapping's keys in order (sorted for plain maps).
func mappingKeys(v any) []string {
	keys, _, _ := orderedMap(v)
	return keys
}

// pyTypeName is ansible-core's native_type_name.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "'NoneType'"
	case bool:
		return "'bool'"
	case int, int64:
		return "'int'"
	case float64:
		return "'float'"
	case string:
		return "'str'"
	case []any:
		return "'list'"
	}
	if _, ok := asMapping(v); ok {
		return "'dict'"
	}
	return fmt.Sprintf("'%T'", v)
}
