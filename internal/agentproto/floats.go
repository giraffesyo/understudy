package agentproto

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
)

// wireFloats returns v with every float as a JSON number that still reads
// as a float: encoding/json writes an integral float64 (65536.0) as
// "65536", which the other side decodes as an int (numbers), so a
// module's float would come back over the agent wire as an int. Integral
// floats become "65536.0" (Python's repr); other floats already carry a
// '.' or an exponent. Containers are copied, not modified; values with
// their own JSON encoding pass through as they are.
func wireFloats(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int, int64, json.Number:
		return v
	case float64:
		return wireFloat(t)
	case float32:
		return wireFloat(float64(t))
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = wireFloats(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = wireFloats(e)
		}
		return out
	case json.Marshaler:
		return v
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		return wireFloat(rv.Float())
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String || rv.IsNil() {
			return v
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = wireFloats(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 || (rv.Kind() == reflect.Slice && rv.IsNil()) {
			return v // []byte encodes as base64; a nil slice as null
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = wireFloats(rv.Index(i).Interface())
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return v
		}
		if k := rv.Elem().Kind(); k == reflect.Map || k == reflect.Slice || k == reflect.Array || k == reflect.Float64 || k == reflect.Float32 {
			return wireFloats(rv.Elem().Interface())
		}
	}
	return v
}

// wireFloat is f as a JSON number that decodes back to a float.
func wireFloat(f float64) any {
	if math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || math.Abs(f) >= 1e21 {
		return f // encoding/json writes these with a '.' or an exponent (or rejects them)
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64) + ".0")
}
