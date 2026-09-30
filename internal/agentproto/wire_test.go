package agentproto

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/giraffesyo/understudy/internal/omap"
)

func ordered(kv ...any) *omap.OMap {
	m := omap.NewOMap()
	for i := 0; i < len(kv); i += 2 {
		m.Set(kv[i].(string), kv[i+1])
	}
	return m
}

// TestWireKeepsOrder: ordered maps cross the wire in their key order, in
// results and in task arguments, at any depth; plain maps stay plain.
func TestWireKeepsOrder(t *testing.T) {
	body := ordered("zeta", int64(1), "alpha", ordered("y", 2.0, "b", []any{ordered("k2", "v", "k1", nil)}), "mid", map[string]any{"b": 1.5, "a": int64(2)})
	res := &Result{Changed: true, Msg: "done", Extra: map[string]any{"json": body, "count": int64(3), "ratio": 4.0}}

	var out bytes.Buffer
	if err := WriteResult(&out, res); err != nil {
		t.Fatal(err)
	}
	got, err := ParseResult(append([]byte("noise before\n"), out.Bytes()...))
	if err != nil {
		t.Fatal(err)
	}
	js, ok := got.Extra["json"].(*omap.OMap)
	if !ok {
		t.Fatalf("json = %T, want *omap.OMap", got.Extra["json"])
	}
	if !reflect.DeepEqual(js.Keys(), []string{"zeta", "alpha", "mid"}) {
		t.Errorf("keys = %v", js.Keys())
	}
	alpha := js.Get("alpha").(*omap.OMap)
	if !reflect.DeepEqual(alpha.Keys(), []string{"y", "b"}) || alpha.Get("y") != 2.0 {
		t.Errorf("alpha = %v %#v", alpha.Keys(), alpha.Get("y"))
	}
	inner := alpha.Get("b").([]any)[0].(*omap.OMap)
	if !reflect.DeepEqual(inner.Keys(), []string{"k2", "k1"}) {
		t.Errorf("inner keys = %v", inner.Keys())
	}
	if mid, ok := js.Get("mid").(map[string]any); !ok || mid["b"] != 1.5 || mid["a"] != int64(2) {
		t.Errorf("mid = %#v", js.Get("mid"))
	}
	if got.Extra["count"] != int64(3) || got.Extra["ratio"] != 4.0 || !got.Changed || got.Msg != "done" {
		t.Errorf("result = %+v", got)
	}

	var frame bytes.Buffer
	req := &TaskRequest{Proto: ProtoVersion, Op: "task", Module: "uri", Args: map[string]any{"body": body, "mode": int64(420), "names": []string{"b", "a"}}}
	if err := WriteFrame(&frame, req, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(frame.String(), "\n") != 1 {
		t.Fatalf("header = %q", frame.String())
	}
	back, _, err := ReadFrame(&frame)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := back.Args["body"].(*omap.OMap); !ok || !reflect.DeepEqual(b.Keys(), []string{"zeta", "alpha", "mid"}) {
		t.Errorf("body = %#v", back.Args["body"])
	}
	if back.Args["mode"] != int64(420) || !reflect.DeepEqual(back.Args["names"], []any{"b", "a"}) || back.Module != "uri" {
		t.Errorf("request = %+v", back)
	}
}
