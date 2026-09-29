package modules

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// Async jobs (async: N with poll: 0, or the job behind a poll > 0 wait)
// follow ansible's async_wrapper: the module runs in a detached re-exec of
// this binary and its result lands in ~/.ansible_async/<jid>, which
// async_status reads.

// AsyncRunArg is the hidden argv[1] both the agent and the control binary
// dispatch to RunAsyncJob.
const AsyncRunArg = "__understudy_async_job"

// AsyncReexec is set by binaries that dispatch AsyncRunArg (the agent and
// the understudy CLI): their jobs survive the run as detached processes.
// When understudy is embedded as a library the job runs in a goroutine of
// the host program instead.
var AsyncReexec bool

func init() {
	Register(asyncStatusModule, "async_status", "ansible.builtin.async_status")
}

func asyncDir() string {
	home := os.Getenv("HOME")
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	return filepath.Join(home, ".ansible_async")
}

func newJID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1e12))
	return fmt.Sprintf("j%d.%d", n.Int64(), os.Getpid())
}

// StartAsync launches req as a background job and returns ansible's
// immediate async result.
func StartAsync(req *agentproto.TaskRequest, payload io.Reader) *agentproto.Result {
	dir := asyncDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return agentproto.Fail("could not create async dir %s: %v", dir, err)
	}
	jid := newJID()
	jobFile := filepath.Join(dir, jid)
	req.Background = false
	var body []byte
	if payload != nil && req.PayloadLen > 0 {
		body, _ = io.ReadAll(io.LimitReader(payload, req.PayloadLen))
	}
	frame := struct {
		Req     *agentproto.TaskRequest `json:"req"`
		Payload []byte                  `json:"payload,omitempty"`
	}{req, body}
	data, err := json.Marshal(frame)
	if err != nil {
		return agentproto.Fail("async: %v", err)
	}
	if err := os.WriteFile(jobFile+".req", data, 0o600); err != nil {
		return agentproto.Fail("async: %v", err)
	}
	writeJobFile(jobFile, map[string]any{"started": 1, "finished": 0, "ansible_job_id": jid})

	if !AsyncReexec {
		go runJob(jobFile, frame.Req, body, req.AsyncTimeout, false)
		return &agentproto.Result{Changed: true, Extra: map[string]any{
			"ansible_job_id": jid, "started": true, "finished": false, "results_file": jobFile,
		}}
	}
	exe, err := os.Executable()
	if err != nil {
		return agentproto.Fail("async: %v", err)
	}
	cmd := exec.Command(exe, AsyncRunArg, jid, strconv.Itoa(req.AsyncTimeout))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return agentproto.Fail("async: could not start job: %v", err)
	}
	go cmd.Wait() // reap if we outlive it; the job keeps running otherwise
	return &agentproto.Result{Changed: true, Extra: map[string]any{
		"ansible_job_id": jid, "started": true, "finished": false, "results_file": jobFile,
	}}
}

// RunAsyncJob is the detached job process: run the module, record its
// result, and enforce the async time limit.
func RunAsyncJob(jid string, timeout int) int {
	jobFile := filepath.Join(asyncDir(), jid)
	data, err := os.ReadFile(jobFile + ".req")
	if err != nil {
		writeJobFile(jobFile, map[string]any{"failed": true, "finished": 1, "msg": err.Error()})
		return 1
	}
	os.Remove(jobFile + ".req")
	var frame struct {
		Req     *agentproto.TaskRequest `json:"req"`
		Payload []byte                  `json:"payload"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		writeJobFile(jobFile, map[string]any{"failed": true, "finished": 1, "msg": err.Error()})
		return 1
	}
	runJob(jobFile, frame.Req, frame.Payload, timeout, true)
	return 0
}

// runJob runs the module and records its result; past the time limit a
// detached job kills its process group (as async_wrapper does), while an
// in-process one just stops waiting.
func runJob(jobFile string, req *agentproto.TaskRequest, payload []byte, timeout int, detached bool) {
	done := make(chan *agentproto.Result, 1)
	go func() { done <- Run(req, bytes.NewReader(payload)) }()
	var limit <-chan time.Time
	if timeout > 0 {
		limit = time.After(time.Duration(timeout) * time.Second)
	}
	select {
	case res := <-done:
		out, _ := json.Marshal(res)
		var m map[string]any
		json.Unmarshal(out, &m)
		m["finished"] = 1
		writeJobFile(jobFile, m)
	case <-limit:
		if detached {
			syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		}
	}
}

func writeJobFile(path string, m map[string]any) {
	data, _ := json.Marshal(m)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// asyncStatusModule is ansible.builtin.async_status.
func asyncStatusModule(env *RunEnv, args map[string]any) *agentproto.Result {
	jid, _ := argString(args, "jid")
	if jid == "" {
		return agentproto.Fail("missing required arguments: jid")
	}
	mode, _ := argString(args, "mode")
	if mode == "" {
		mode = "status"
	}
	jobFile := filepath.Join(asyncDir(), jid)
	base := map[string]any{"ansible_job_id": jid, "results_file": jobFile, "started": true}
	if mode == "cleanup" {
		os.Remove(jobFile)
		return &agentproto.Result{Extra: map[string]any{"ansible_job_id": jid, "erased": jobFile}}
	}
	if mode != "status" {
		return agentproto.Fail("value of mode must be one of: status, cleanup, got: %s", mode)
	}
	data, err := os.ReadFile(jobFile)
	if err != nil {
		return &agentproto.Result{Failed: true, Msg: "could not find job", Extra: map[string]any{
			"ansible_job_id": jid, "started": true, "finished": true}}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		// Still being written: report as running.
		base["finished"] = false
		return &agentproto.Result{Extra: base}
	}
	if f, _ := m["finished"].(float64); f != 1 {
		base["finished"] = false
		return &agentproto.Result{Extra: base}
	}
	delete(m, "finished")
	delete(m, "started")
	raw, _ := json.Marshal(m)
	res := &agentproto.Result{}
	if err := json.Unmarshal(raw, res); err != nil {
		return agentproto.Fail("async job %s: unreadable result: %v", jid, err)
	}
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	for k, v := range base {
		res.Extra[k] = v
	}
	res.Extra["finished"] = true
	return res
}
