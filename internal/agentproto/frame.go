package agentproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteFrame writes the request header line and, if req.PayloadLen > 0,
// copies exactly that many bytes from payload.
func WriteFrame(w io.Writer, req *TaskRequest, payload io.Reader) error {
	header, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(header, '\n')); err != nil {
		return err
	}
	if req.PayloadLen > 0 {
		if payload == nil {
			return fmt.Errorf("payload_len=%d but no payload reader", req.PayloadLen)
		}
		n, err := io.Copy(w, io.LimitReader(payload, req.PayloadLen))
		if err != nil {
			return err
		}
		if n != req.PayloadLen {
			return fmt.Errorf("payload short write: %d of %d bytes", n, req.PayloadLen)
		}
	}
	return nil
}

// ReadFrame reads a request header line and returns a reader limited to the
// declared payload length. A first line that is not valid JSON usually means
// stdin pollution (a sudo prompt consumed as input) — name that in the error.
func ReadFrame(r io.Reader) (*TaskRequest, io.Reader, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return nil, nil, fmt.Errorf("reading task header: %w", err)
	}
	var req TaskRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return nil, nil, fmt.Errorf(
			"task header is not valid JSON (a become/sudo prompt may have corrupted stdin): %w", err)
	}
	if req.Proto != ProtoVersion {
		return nil, nil, fmt.Errorf("protocol version mismatch: control speaks %d, agent speaks %d", req.Proto, ProtoVersion)
	}
	return &req, io.LimitReader(br, req.PayloadLen), nil
}

// WriteResult emits the sentinel line and the result JSON to w.
func WriteResult(w io.Writer, res *Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		// A result that cannot marshal must still produce a frame.
		data, _ = json.Marshal(Fail("result serialization failed: %v", err))
	}
	_, err = fmt.Fprintf(w, "\n%s\n%s\n", ResultSentinel, data)
	return err
}

// ParseResult extracts the result JSON from agent stdout, tolerating noise
// before the sentinel line.
func ParseResult(stdout []byte) (*Result, error) {
	s := string(stdout)
	idx := strings.LastIndex(s, ResultSentinel)
	if idx < 0 {
		return nil, fmt.Errorf("no result sentinel in agent output: %s", excerpt(s))
	}
	rest := s[idx+len(ResultSentinel):]
	rest = strings.TrimLeft(rest, "\r\n")
	var res Result
	dec := json.NewDecoder(strings.NewReader(rest))
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("malformed agent result JSON: %w (output: %s)", err, excerpt(rest))
	}
	return &res, nil
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
