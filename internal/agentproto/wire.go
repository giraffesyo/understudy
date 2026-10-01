package agentproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/giraffesyo/understudy/internal/omap"
)

// orderedKey, as the first key of a JSON object on the agent wire, marks
// an ordered map: the object decodes to an *omap.OMap in the order its
// keys were written, where a plain object decodes to a map[string]any.
// Dicts in module results (uri's json, nested result dicts) keep their
// order across the wire, as they do when a module runs in process.
const orderedKey = "_understudy_ordered"

// encodeWire writes v as agent-wire JSON: *omap.OMap values in their key
// order (marked with orderedKey), maps with sorted keys, and floats that
// read back as floats (see wireFloat).
func encodeWire(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		return writeJSON(b, t)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case json.Number:
		b.WriteString(string(t))
	case float64:
		return writeJSON(b, wireFloat(t))
	case float32:
		return writeJSON(b, wireFloat(float64(t)))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeWire(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSON(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encodeWire(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case *omap.OMap:
		b.WriteString(`{"` + orderedKey + `":1`)
		for _, k := range t.Keys() {
			b.WriteByte(',')
			if err := writeJSON(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encodeWire(b, t.Get(k)); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		// Other containers (map[string]string, []string, ...) and number
		// types, generically.
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Map:
			if rv.Type().Key().Kind() == reflect.String && !rv.IsNil() {
				m := make(map[string]any, rv.Len())
				iter := rv.MapRange()
				for iter.Next() {
					m[iter.Key().String()] = iter.Value().Interface()
				}
				return encodeWire(b, m)
			}
		case reflect.Slice, reflect.Array:
			if rv.Type().Elem().Kind() != reflect.Uint8 && !(rv.Kind() == reflect.Slice && rv.IsNil()) {
				items := make([]any, rv.Len())
				for i := range items {
					items[i] = rv.Index(i).Interface()
				}
				return encodeWire(b, items)
			}
		case reflect.Float32, reflect.Float64:
			return writeJSON(b, wireFloat(rv.Float()))
		case reflect.Pointer, reflect.Interface:
			if !rv.IsNil() {
				switch rv.Elem().Kind() {
				case reflect.Map, reflect.Slice, reflect.Array, reflect.Float32, reflect.Float64:
					return encodeWire(b, rv.Elem().Interface())
				}
			}
		}
		return writeJSON(b, v)
	}
	return nil
}

func writeJSON(b *bytes.Buffer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.Write(data)
	return nil
}

// decodeWire parses the agent-wire JSON value data starts with (what
// follows it is ignored): objects marked with orderedKey become
// *omap.OMap, other objects map[string]any; integer literals int64 and
// other numbers float64, as Python's json module reads them.
func decodeWire(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return decodeWireValue(dec)
}

func decodeWireValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		var m map[string]any
		var om *omap.OMap
		first := true
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := kt.(string)
			v, err := decodeWireValue(dec)
			if err != nil {
				return nil, err
			}
			if first && key == orderedKey {
				om = omap.NewOMap()
				first = false
				continue
			}
			first = false
			if om != nil {
				om.Set(key, v)
				continue
			}
			if m == nil {
				m = map[string]any{}
			}
			m[key] = v
		}
		if _, err := dec.Token(); err != nil { // '}'
			return nil, err
		}
		if om != nil {
			return om, nil
		}
		if m == nil {
			m = map[string]any{}
		}
		return m, nil
	case json.Delim('['):
		out := []any{}
		for dec.More() {
			v, err := decodeWireValue(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if _, err := dec.Token(); err != nil { // ']'
			return nil, err
		}
		return out, nil
	}
	if n, ok := tok.(json.Number); ok {
		return numbers(n), nil
	}
	return tok, nil
}

// wireRequest is TaskRequest on the wire, its Args in the wire encoding.
type wireRequest struct {
	*TaskRequest
	Args json.RawMessage `json:"args,omitempty"`
}

func marshalRequest(req *TaskRequest) ([]byte, error) {
	w := wireRequest{TaskRequest: req}
	if req.Args != nil {
		var b bytes.Buffer
		// Modules see plain JSON-shaped arguments, as on the in-process
		// path (order-sensitive values, such as a uri body, are
		// serialized by their action before they get here).
		if err := encodeWire(&b, omap.AsMap(req.Args)); err != nil {
			return nil, err
		}
		w.Args = b.Bytes()
	}
	return json.Marshal(&w)
}

func unmarshalRequest(line string) (*TaskRequest, error) {
	req := &TaskRequest{}
	w := wireRequest{TaskRequest: req}
	dec := json.NewDecoder(bytes.NewReader([]byte(line)))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, err
	}
	req.Args = nil
	if len(w.Args) > 0 {
		v, err := decodeWire(w.Args)
		if err != nil {
			return nil, err
		}
		args, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("task args are not an object")
		}
		req.Args = args
	}
	return req, nil
}

// marshalWire is MarshalJSON in the wire encoding.
func (r *Result) marshalWire() ([]byte, error) {
	var b bytes.Buffer
	if err := encodeWire(&b, r.fields()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// unmarshalWire is UnmarshalJSON from the wire encoding.
func (r *Result) unmarshalWire(data []byte) error {
	v, err := decodeWire(data)
	if err != nil {
		return err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("result is not an object")
	}
	r.fromFields(m)
	return nil
}
