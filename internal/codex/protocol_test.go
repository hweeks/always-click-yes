package codex

import "testing"

func TestClassifyLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want lineShape
	}{
		{"response with result", `{"id":1,"result":{"ok":true}}`, lineResponse},
		{"response with error", `{"id":2,"error":{"code":-1,"message":"boom"}}`, lineResponse},
		{"notification", `{"method":"turn/completed","params":{}}`, lineNotification},
		{"server request", `{"method":"item/commandExecution/requestApproval","id":0,"params":{}}`, lineServerRequest},
		{"server request id after method", `{"id":0,"method":"item/commandExecution/requestApproval","params":{}}`, lineServerRequest},
		{"neither method nor id", `{"foo":"bar"}`, lineUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, err := classifyLine([]byte(tt.line))
			if err != nil {
				t.Fatalf("classifyLine: %v", err)
			}
			if got != tt.want {
				t.Errorf("classifyLine(%s) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestClassifyLineDistinguishesResponseFromServerRequestSameID is the
// collision the two id namespaces invite: codex's own approval requests use
// an id sequence starting at 0, entirely separate from the client's own
// outgoing 1.. sequence — the fixture literally has both a client response
// and (later) a server request numbered independently, and nothing stops
// them from sharing a number. classifyLine must tell them apart by shape
// (does "method" appear), never by comparing the id value.
func TestClassifyLineDistinguishesResponseFromServerRequestSameID(t *testing.T) {
	respLine := []byte(`{"id":0,"result":{"decision":"whatever"}}`)
	reqLine := []byte(`{"method":"item/commandExecution/requestApproval","id":0,"params":{"reason":"write a file?"}}`)

	_, kind, err := classifyLine(respLine)
	if err != nil {
		t.Fatal(err)
	}
	if kind != lineResponse {
		t.Errorf("response sharing id 0 classified as %v, want lineResponse", kind)
	}

	_, kind, err = classifyLine(reqLine)
	if err != nil {
		t.Fatal(err)
	}
	if kind != lineServerRequest {
		t.Errorf("server request sharing id 0 classified as %v, want lineServerRequest", kind)
	}
}

// TestResponseAndServerRequestSameIDNotConflated drives both lines through the
// real Driver.handleLine dispatch (not just classifyLine) and proves the
// response goes to the pending caller waiting on id 0 while the server
// request is recorded as an outstanding approval — and that neither line
// leaks into the other's handling path.
func TestResponseAndServerRequestSameIDNotConflated(t *testing.T) {
	d := New(Options{})

	ch := make(chan rpcResult, 1)
	d.pendingMu.Lock()
	d.pending[0] = pendingRequest{kind: kindGeneric, ch: ch}
	d.pendingMu.Unlock()

	d.handleLine([]byte(`{"id":0,"result":{"ok":true}}`))

	select {
	case res := <-ch:
		if res.Err != nil {
			t.Fatalf("unexpected rpc error: %+v", res.Err)
		}
	default:
		t.Fatal("response id 0 was not delivered to the pending caller")
	}

	d.handleLine([]byte(`{"method":"item/commandExecution/requestApproval","id":0,"params":{"reason":"write a file?"}}`))

	pending := d.PendingApprovals()
	if len(pending) != 1 {
		t.Fatalf("want 1 outstanding approval, got %d: %+v", len(pending), pending)
	}
	if pending[0].ID != 0 || pending[0].Method != "item/commandExecution/requestApproval" {
		t.Errorf("outstanding approval = %+v, want id=0 method=item/commandExecution/requestApproval", pending[0])
	}

	// The server request must not have been delivered to the response channel
	// a moment ago — that channel already received its one value and nothing
	// about handling the later server request should touch it again.
	select {
	case res := <-ch:
		t.Fatalf("server request was delivered as a second response: %+v", res)
	default:
	}
}
