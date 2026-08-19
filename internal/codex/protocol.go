package codex

import "encoding/json"

// lineShape classifies one decoded NDJSON line from codex's app-server. The
// wire format is JSON-RPC-*shaped* (id/method/params, id/result-or-error) but
// carries no "jsonrpc":"2.0" field, and a request the server sends (chiefly an
// approval request) lives in a numeric id namespace entirely separate from the
// client's own outgoing ids — docs/codex-cli-findings.md §2/§3.
//
// The distinguishing feature is never the id's value, only whether "method" is
// present: a response never carries one; a notification and a server-initiated
// request always do, and only the latter also carries an id. Classifying on id
// alone would be exactly the bug this type exists to avoid — the fixture's own
// client ids 1..4 alongside the server's own id 0 is a real instance of that
// collision, not a hypothetical one (see wire_test.go).
type lineShape int

const (
	lineUnknown lineShape = iota
	lineResponse
	lineNotification
	lineServerRequest
)

// wireEnvelope is the union of every field any codex app-server line can
// carry. Only the subset matching its actual shape is populated.
type wireEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// classifyLine decodes one line and reports its shape without assuming
// anything about the numeric value of its id.
func classifyLine(line []byte) (wireEnvelope, lineShape, error) {
	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return env, lineUnknown, err
	}
	hasID := len(env.ID) > 0 && string(env.ID) != "null"
	switch {
	case env.Method != "" && hasID:
		return env, lineServerRequest, nil
	case env.Method != "":
		return env, lineNotification, nil
	case hasID:
		return env, lineResponse, nil
	default:
		return env, lineUnknown, nil
	}
}

// parseID extracts a plain integer id. Every id observed live on this
// protocol — the client's own 1.. sequence and the server's own 0.. sequence
// used for approval requests — is a bare JSON number, never a string.
func parseID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}
