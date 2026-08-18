package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
)

// sendRequest writes one JSON-RPC-shaped request (id/method/params, no
// "jsonrpc" field) to codex's stdin and registers a pending entry so its
// response — matched purely by id, once classifyLine has already told us the
// line is a response and not a same-numbered server request — can be
// delivered back. kind records what, if anything, the response should do
// besides unblock a waiting caller; see applyResponseSideEffects.
func (d *Driver) sendRequest(method string, params any, kind requestKind) (int64, <-chan rpcResult, error) {
	id := d.reqSeq.Add(1)
	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return id, nil, fmt.Errorf("codex: marshal %s: %w", method, err)
	}

	ch := make(chan rpcResult, 1)
	d.pendingMu.Lock()
	d.pending[id] = pendingRequest{kind: kind, ch: ch}
	d.pendingMu.Unlock()

	alog.Raw("TX", string(b))
	b = append(b, '\n')
	d.writeMu.Lock()
	_, werr := d.stdin.Write(b)
	d.writeMu.Unlock()
	if werr != nil {
		d.pendingMu.Lock()
		delete(d.pending, id)
		d.pendingMu.Unlock()
		return id, nil, werr
	}
	return id, ch, nil
}

// call sends a request and blocks for its response, its context being
// cancelled, or the process exiting first — whichever comes first.
func (d *Driver) call(ctx context.Context, method string, params any, kind requestKind) (json.RawMessage, error) {
	_, ch, err := d.sendRequest(method, params, kind)
	if err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, fmt.Errorf("codex: %s: %s (code %d)", method, res.Err.Message, res.Err.Code)
		}
		return res.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.exited:
		return nil, fmt.Errorf("codex: process exited before %s responded", method)
	}
}

// deliverResponse matches a response line to the pending request it answers
// and hands it the result, after first applying any side effects the result
// itself implies (see applyResponseSideEffects). Those side effects run
// unconditionally here, in the read loop, rather than in the blocked caller —
// so a fixture replay with no live caller waiting still produces the right
// event sequence, and so a caller that gave up (ctx cancelled) doesn't leave
// the thread id or turn id uncaptured.
func (d *Driver) deliverResponse(env wireEnvelope) {
	id, ok := parseID(env.ID)
	if !ok {
		alog.Printf("codex: response with unparsable id: %s", env.ID)
		return
	}
	d.pendingMu.Lock()
	req, found := d.pending[id]
	if found {
		delete(d.pending, id)
	}
	d.pendingMu.Unlock()
	if !found {
		alog.Printf("codex: response for unknown request id %d", id)
		return
	}

	var rpcErr *rpcError
	if len(env.Error) > 0 {
		rpcErr = &rpcError{}
		if err := json.Unmarshal(env.Error, rpcErr); err != nil {
			alog.Printf("codex: decode error object for id %d: %v", id, err)
		}
	}
	if rpcErr == nil {
		d.applyResponseSideEffects(req.kind, env.Result)
	}

	req.ch <- rpcResult{Result: env.Result, Err: rpcErr}
}

// applyResponseSideEffects updates driver state and emits any events implied
// purely by a response's content. thread/start and thread/resume carry the
// thread id and model that arm the init event; turn/start carries the new
// turn id that Interrupt needs. Nothing else currently needs this.
func (d *Driver) applyResponseSideEffects(kind requestKind, result json.RawMessage) {
	switch kind {
	case kindThreadStart, kindThreadResume:
		var parsed struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(result, &parsed); err != nil {
			alog.Printf("codex: parse thread response: %v", err)
			return
		}
		d.mu.Lock()
		d.threadID = parsed.Thread.ID
		d.model = parsed.Model
		d.mu.Unlock()
		// thread/started (the notification) carries the same information; see
		// handleNotification's comment on why only this response emits init.
		d.events <- driver.Event{
			Type:      driver.TypeSystem,
			Subtype:   "init",
			SessionID: parsed.Thread.ID,
			Model:     parsed.Model,
		}
	case kindTurnStart:
		var parsed struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(result, &parsed); err != nil {
			alog.Printf("codex: parse turn/start response: %v", err)
			return
		}
		d.mu.Lock()
		d.activeTurnID = parsed.Turn.ID
		d.mu.Unlock()
	}
}
