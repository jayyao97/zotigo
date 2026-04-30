package observability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
)

// TestNoop_ReturnsCtxUnchanged is the load-bearing contract: agents
// always hold a non-nil Observer and call its hooks unconditionally,
// so Noop must be a perfect zero-cost passthrough — no allocation,
// no ctx mutation. If a future Observer field were added that Noop
// doesn't implement (compiler enforces interface conformance), this
// test stays as the "zero side effects" gate.
func TestNoop_ReturnsCtxUnchanged(t *testing.T) {
	var n observability.Noop
	parent := context.WithValue(context.Background(), testKey{}, "marker")

	ctx1 := n.StartTurn(parent, protocol.NewUserMessage("hi"), nil)
	if ctx1 != parent {
		t.Error("StartTurn should return the same ctx instance for Noop")
	}
	ctx2 := n.StartGeneration(parent, observability.GenerationMain, "model", nil, nil, nil)
	if ctx2 != parent {
		t.Error("StartGeneration should return the same ctx instance for Noop")
	}
	ctx3 := n.StartTool(parent, "tool", "{}")
	if ctx3 != parent {
		t.Error("StartTool should return the same ctx instance for Noop")
	}

	// All End* and Close are no-ops — exercise once to ensure they
	// don't panic on minimal input.
	n.EndTurn(parent, errors.New("any"))
	n.EndGeneration(parent, observability.GenerationOutput{}, nil, nil)
	n.EndTool(parent, "result", nil)
	if err := n.Close(parent); err != nil {
		t.Errorf("Close returned non-nil error: %v", err)
	}
}

type testKey struct{}
