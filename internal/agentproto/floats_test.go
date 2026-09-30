package agentproto

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

// A float a module returns must come back over the agent wire as a float,
// integral or not, as it does on the in-process path; ints stay ints.
func TestResultFloatsSurviveTheWire(t *testing.T) {
	type named float64
	res := &Result{Changed: true, Extra: map[string]any{
		"size":    65536.0,
		"ratio":   0.5,
		"neg":     -2.0,
		"big":     1e22,
		"count":   int64(3),
		"plain":   7,
		"nested":  map[string]any{"f": 1.0, "list": []any{2.0, int64(2), "x"}},
		"typed":   []float64{3.0, 3.5},
		"floats":  map[string]float64{"a": 4.0},
		"named":   named(5),
		"f32":     float32(6),
		"strings": []string{"a"},
		"bytes":   []byte("hi"),
	}}
	var buf bytes.Buffer
	if err := WriteResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got, err := ParseResult(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"size":    65536.0,
		"ratio":   0.5,
		"neg":     -2.0,
		"big":     1e22,
		"count":   int64(3),
		"plain":   int64(7),
		"nested":  map[string]any{"f": 1.0, "list": []any{2.0, int64(2), "x"}},
		"typed":   []any{3.0, 3.5},
		"floats":  map[string]any{"a": 4.0},
		"named":   5.0,
		"f32":     6.0,
		"strings": []any{"a"},
		"bytes":   "aGk=",
	}
	if !reflect.DeepEqual(got.Extra, want) {
		t.Errorf("round trip:\n got  %#v\n want %#v", got.Extra, want)
	}
	if res.Extra["size"] != 65536.0 {
		t.Errorf("the result was modified: %#v", res.Extra["size"])
	}
}

// Request args cross the other way: a float argument stays a float.
func TestRequestFloatsSurviveTheWire(t *testing.T) {
	req := &TaskRequest{Proto: ProtoVersion, Op: "task", Module: "m",
		Args: map[string]any{"timeout": 30.0, "n": int64(1), "l": []any{1.0}}}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, req, nil); err != nil {
		t.Fatal(err)
	}
	got, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, payload)
	want := map[string]any{"timeout": 30.0, "n": int64(1), "l": []any{1.0}}
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("args: got %#v, want %#v", got.Args, want)
	}
}
