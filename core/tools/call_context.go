package tools

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
)

// The agent attaches these values to the context it passes to Tool.Execute.
// They are part of the tool contract, so they live here rather than in the
// agent package: a tool must not need to import its caller.

type eventSinkKey struct{}
type callInfoKey struct{}

// CallInfo identifies the tool call a tool is currently executing.
type CallInfo struct {
	ID   string
	Name string
}

// EventSink emits observational events from long-running tools.
// It is optional: most tools ignore it and return a final result only.
type EventSink func(protocol.Event)

// WithEventSink returns ctx carrying sink. A nil sink leaves ctx unchanged.
func WithEventSink(ctx context.Context, sink EventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, eventSinkKey{}, sink)
}

func EventSinkFromContext(ctx context.Context) (EventSink, bool) {
	sink, ok := ctx.Value(eventSinkKey{}).(EventSink)
	return sink, ok && sink != nil
}

func WithCallInfo(ctx context.Context, call CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, call)
}

func CallInfoFromContext(ctx context.Context) (CallInfo, bool) {
	call, ok := ctx.Value(callInfoKey{}).(CallInfo)
	return call, ok && call.ID != ""
}
