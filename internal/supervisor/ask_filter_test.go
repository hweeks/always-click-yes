package supervisor

import (
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/mcp"
)

func TestFilterAskReqsInterceptsAskWhenClaimed(t *testing.T) {
	in := make(chan *mcp.Pending, 1)
	out := filterAskReqs(in, func(p *mcp.Pending) bool {
		p.Resolve(mcp.Answer{Text: "intercepted"})
		return true
	})

	p, _ := mcp.NewPending(mcp.Request{Tool: mcp.ToolAsk})
	in <- p
	close(in)

	select {
	case got, ok := <-out:
		if ok {
			t.Fatalf("intercepted Ask reached the output channel: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("output channel never closed")
	}
}

func TestFilterAskReqsForwardsDispatch(t *testing.T) {
	in := make(chan *mcp.Pending, 1)
	out := filterAskReqs(in, func(p *mcp.Pending) bool {
		t.Fatal("interceptor should never be offered a non-Ask tool")
		return true
	})

	p, _ := mcp.NewPending(mcp.Request{Tool: mcp.ToolDispatch})
	in <- p
	close(in)

	select {
	case got, ok := <-out:
		if !ok || got != p {
			t.Fatalf("Dispatch Pending did not reach the output channel unchanged")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the Dispatch Pending")
	}
}

func TestFilterAskReqsForwardsDeclinedAsk(t *testing.T) {
	in := make(chan *mcp.Pending, 1)
	out := filterAskReqs(in, func(p *mcp.Pending) bool { return false })

	p, _ := mcp.NewPending(mcp.Request{Tool: mcp.ToolAsk})
	in <- p
	close(in)

	select {
	case got, ok := <-out:
		if !ok || got != p {
			t.Fatalf("declined Ask did not reach the output channel unchanged")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the declined Ask")
	}
}

func TestFilterAskReqsClosesAfterInputCloses(t *testing.T) {
	in := make(chan *mcp.Pending)
	out := filterAskReqs(in, func(p *mcp.Pending) bool { return false })

	close(in)

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected the output channel to be closed with no values")
		}
	case <-time.After(time.Second):
		t.Fatal("output channel never closed")
	}
}
