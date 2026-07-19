package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Handler answers a tools/call. name is the *bare* tool name — claude sends
// "AskUserQuestion", not "mcp__acy__AskUserQuestion", even though the assistant
// event stream shows the qualified form. toolUseID is lifted from the call's
// _meta and correlates with the tool_use block the supervisor already ingested.
//
// A returned error becomes an isError result rather than a JSON-RPC error: the
// model can read it and recover, whereas a protocol-level error just fails the
// turn.
type Handler func(name string, args json.RawMessage, toolUseID string) (string, error)

// maxLine bounds a single JSON-RPC message. A plan handed to PresentPlan is the
// biggest thing that crosses this, and it can be long.
const maxLine = 1 << 20

// Serve runs the JSON-RPC loop until in is exhausted (claude closing our stdin is
// the normal shutdown) or an unrecoverable read error occurs.
func Serve(in io.Reader, out io.Writer, h Handler) error {
	br := bufio.NewReaderSize(in, maxLine)
	enc := json.NewEncoder(out)

	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp, ok := handle(line, h); ok {
				if err := enc.Encode(resp); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// handle decodes one message and produces its response. ok is false when the
// message must not be answered at all.
func handle(line []byte, h Handler) (response, bool) {
	var req request
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		// No id could be recovered, so there is nobody to send an error to.
		return response{}, false
	}
	// A notification carries no id (claude sends notifications/initialized right
	// after the handshake). Answering one is a protocol violation, and it is the
	// single easiest bug to ship here.
	if len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null")) {
		return response{}, false
	}

	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = initResult(req.Params)
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefs()}
	case "tools/call":
		resp.Result = callResult(req.Params, h)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "unknown method: " + req.Method}
	}
	return resp, true
}

// initResult echoes the client's protocol version back at it.
func initResult(params json.RawMessage) map[string]any {
	ver := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		ver = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      serverInfo(),
	}
}

// callResult dispatches a tools/call to the handler.
func callResult(params json.RawMessage, h Handler) toolResult {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ToolUseID string `json:"claudecode/toolUseId"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errResult("malformed tools/call params: " + err.Error())
	}
	text, err := h(p.Name, p.Arguments, p.Meta.ToolUseID)
	if err != nil {
		return errResult(err.Error())
	}
	return okResult(text)
}
