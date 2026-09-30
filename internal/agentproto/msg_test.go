package agentproto

import (
	"bytes"
	"reflect"
	"testing"
)

// A structured msg (the dnf action's tuple) stays one in the result,
// while Msg, its display text, crosses the wire beside it.
func TestStructuredMsgSurvivesTheWire(t *testing.T) {
	msg := []any{"first.", "second"}
	res := &Result{Failed: true, Msg: "('first.', 'second')", Origin: "action", Extra: map[string]any{"msg": msg}}
	var buf bytes.Buffer
	if err := WriteResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got, err := ParseResult(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg != res.Msg || got.Origin != "action" {
		t.Errorf("display text %q (origin %q)", got.Msg, got.Origin)
	}
	if v := got.ToVars()["msg"]; !reflect.DeepEqual(v, msg) {
		t.Errorf("result msg %#v, want %#v", v, msg)
	}
	if _, leaked := got.Extra[msgTextKey]; leaked {
		t.Errorf("the display text leaked into the result: %#v", got.Extra)
	}
	// A plain msg is unchanged.
	plain := &Result{Failed: true, Msg: "boom"}
	buf.Reset()
	WriteResult(&buf, plain)
	got, _ = ParseResult(buf.Bytes())
	if got.Msg != "boom" || got.ToVars()["msg"] != "boom" {
		t.Errorf("plain msg: %#v", got)
	}
}

// Module stdout/stderr get their *_lines, an explicitly empty one too.
func TestOutputLines(t *testing.T) {
	res := &Result{Stdout: "a\nb", Extra: map[string]any{"stderr": ""}}
	v := res.ToVars()
	if !reflect.DeepEqual(v["stdout_lines"], []any{"a", "b"}) || !reflect.DeepEqual(v["stderr_lines"], []any{}) {
		t.Errorf("lines: %#v / %#v", v["stdout_lines"], v["stderr_lines"])
	}
	if _, ok := (&Result{}).ToVars()["stdout_lines"]; ok {
		t.Errorf("no stdout, no stdout_lines")
	}
}
