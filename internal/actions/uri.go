package actions

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

func init() {
	Register("uri", actionFunc(runURI))
}

// Keys shared with the uri module (internal/modules/uri*.go).
const (
	uriSrcPayloadKey = "_understudy_src_payload"
	multipartFileKey = "_understudy_content_b64"
)

// runURI is ansible.builtin.uri's action plugin: without remote_src a
// src file (or a form-multipart field's filename) is found on the
// controller and shipped to the target. Bodies whose key order matters
// (json, form-urlencoded) are rendered here, where the task's dict order
// is still known.
func runURI(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	if actx.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "This action (uri) does not support check mode."}
	}
	fwd := make(map[string]any, len(args))
	for k, v := range args {
		fwd[k] = v
	}
	bodyFormat := "raw"
	if s, ok := args["body_format"].(string); ok {
		bodyFormat = s
	}
	body := args["body"]
	src, _ := args["src"].(string)
	remoteSrc := isTruthy(args["remote_src"])

	var payload []byte
	if !remoteSrc {
		switch {
		case src != "":
			found, fail := uriFindNeedle(actx, src)
			if fail != nil {
				return fail
			}
			data, err := os.ReadFile(found)
			if err != nil {
				return actionRaise("%v", err)
			}
			payload = data
			fwd["src"] = path.Join(actx.RemoteTmp, path.Base(found))
			fwd[uriSrcPayloadKey] = true
		case bodyFormat == "form-multipart":
			m, ok := yaml.PlainMap(body)
			if !ok {
				return actionRaise("body must be mapping, cannot be type %s", taggedTypeName(body))
			}
			newBody := make(map[string]any, len(m))
			for field, value := range m {
				newBody[field] = value
				vm, ok := yaml.PlainMap(value)
				if !ok {
					continue
				}
				filename, _ := vm["filename"].(string)
				if filename == "" || pyTruthy(vm["content"]) {
					continue
				}
				found, fail := uriFindNeedle(actx, filename)
				if fail != nil {
					return fail
				}
				data, err := os.ReadFile(found)
				if err != nil {
					return actionRaise("%v", err)
				}
				nv := make(map[string]any, len(vm)+1)
				for k, v := range vm {
					nv[k] = v
				}
				nv["filename"] = path.Join(actx.RemoteTmp, path.Base(found))
				nv[multipartFileKey] = base64.StdEncoding.EncodeToString(data)
				newBody[field] = nv
			}
			fwd["body"] = newBody
		}
	}

	switch strings.ToLower(bodyFormat) {
	case "json":
		if _, isStr := body.(string); !isStr {
			fwd["body"] = template.PyJSON(body, 0, false, true)
		}
	case "form-urlencoded":
		// Keep the dict's order: hand the module a list of pairs.
		if mp, ok := body.(template.Mapping); ok {
			pairs := make([]any, 0, mp.Len())
			for _, k := range mp.Keys() {
				v, _ := mp.GetItem(k)
				pairs = append(pairs, []any{k, v})
			}
			fwd["body"] = pairs
		}
	}

	req := &agentproto.TaskRequest{
		Proto:        agentproto.ProtoVersion,
		Op:           "task",
		Module:       "uri",
		Args:         fwd,
		CheckMode:    actx.CheckMode,
		Diff:         actx.Diff,
		Background:   actx.Background,
		AsyncTimeout: actx.AsyncTimeout,
		PayloadLen:   int64(len(payload)),
	}
	res, err := actx.RunModule(ctx, req, bytes.NewReader(payload))
	if err != nil {
		return agentproto.Fail("module execution failed: %v", err)
	}
	return res
}

// uriFindNeedle is the uri action's _find_needle('files', ...): a miss
// raises AnsibleFileNotFound through the action.
func uriFindNeedle(actx *Context, source string) (string, *agentproto.Result) {
	found, searched := searchNeedle(actx, "files", source)
	if found != "" {
		return found, nil
	}
	cause := fileNotFound(source, searched)
	res := actionRaise("Task failed: %s", cause)
	res.ErrorChain = &agentproto.ErrorChain{
		Outer: "Task failed.",
		Inner: cause,
		Help:  "If you are using a module and expect the file to exist on the remote, see the remote_src option.",
	}
	return "", res
}

// taggedTypeName is the class name ansible-core reports for a templated
// task argument (data-tagged native types).
func taggedTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string, yaml.UnsafeString:
		return "_AnsibleTaggedStr"
	case int, int64:
		return "_AnsibleTaggedInt"
	case float64:
		return "_AnsibleTaggedFloat"
	case []any:
		return "_AnsibleTaggedList"
	}
	return "_AnsibleTaggedDict"
}

// pyTruthy is Python truthiness for a task argument value.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case yaml.UnsafeString:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	}
	if m, ok := v.(template.Mapping); ok {
		return m.Len() > 0
	}
	return true
}
