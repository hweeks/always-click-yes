package codex

import (
	"encoding/json"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// ApprovalRequest is a server-initiated request codex is blocking on: one of
// item/commandExecution/requestApproval, item/fileChange/requestApproval, or
// item/permissions/requestApproval (docs/codex-cli-findings.md §3). This
// package never decides one — it only surfaces it and lets a caller answer it
// with Approve; wiring that caller to acy's own countdown gate is a separate
// task.
//
// Until something calls Approve for a given request's ID, the request — and
// the codex turn that raised it — stays blocked indefinitely. That is
// observable, not silent: PendingApprovals reflects every request not yet
// answered regardless of whether anything is listening on the Approvals
// channel, so "is anything stuck" is always a cheap poll away.
type ApprovalRequest struct {
	ID     int64
	Method string
	Params json.RawMessage
}

// handleServerRequest records a server-initiated request and makes it visible
// two ways: a best-effort push on the Approvals channel for a listener that's
// already waiting, and an entry in the outstanding map for a caller that polls
// PendingApprovals instead (or that starts listening after the push already
// happened, or that was too slow and the channel was full — see below).
func (d *Driver) handleServerRequest(env wireEnvelope) {
	id, ok := parseID(env.ID)
	if !ok {
		alog.Printf("codex: server request %q with unparsable id: %s", env.Method, env.ID)
		return
	}
	req := ApprovalRequest{ID: id, Method: env.Method, Params: env.Params}

	d.approvalsMu.Lock()
	d.approvalsOut[id] = req
	d.approvalsMu.Unlock()

	alog.Printf("codex: approval requested: %s (id=%d)", env.Method, id)
	select {
	case d.approvals <- req:
	default:
		// The channel is an at-most-once notification, not the source of
		// truth: dropping this send never loses the request, since it is
		// already recorded in approvalsOut above. A caller relying solely on
		// Approvals() without ever draining it would miss this one, though —
		// PendingApprovals is the fallback for exactly that case.
		alog.Printf("codex: approvals channel full; id=%d still outstanding, see PendingApprovals", id)
	}
}

// Approvals delivers server-initiated approval requests as they arrive. It is
// buffered and best-effort: see handleServerRequest for why PendingApprovals,
// not this channel, is the authoritative way to notice a stuck request.
func (d *Driver) Approvals() <-chan ApprovalRequest { return d.approvals }

// PendingApprovals lists every approval request codex is still blocked on,
// i.e. every one not yet answered via Approve.
func (d *Driver) PendingApprovals() []ApprovalRequest {
	d.approvalsMu.Lock()
	defer d.approvalsMu.Unlock()
	out := make([]ApprovalRequest, 0, len(d.approvalsOut))
	for _, r := range d.approvalsOut {
		out = append(out, r)
	}
	return out
}

// Approve answers a server-initiated approval request by writing a JSON-RPC
// response keyed by the server's own id back onto codex's stdin. decision is
// passed through verbatim — a bare "accept"/"decline"/"cancel" string, or a
// structured variant like {"acceptWithExecpolicyAmendment":{...}} — this
// package does not interpret or validate it: deciding what to send belongs to
// whatever wires this to acy's gate, not to this package (see the package
// doc).
func (d *Driver) Approve(id int64, decision any) error {
	payload := map[string]any{
		"id":     id,
		"result": map[string]any{"decision": decision},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	d.approvalsMu.Lock()
	delete(d.approvalsOut, id)
	d.approvalsMu.Unlock()

	alog.Raw("TX", string(b))
	b = append(b, '\n')
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err = d.stdin.Write(b)
	return err
}
