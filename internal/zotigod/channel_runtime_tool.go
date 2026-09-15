package zotigod

import (
	"context"
	"errors"
	"sync"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/internal/channels"
)

type workerRuntimeToolClient struct {
	writer  *workerClientWriter
	results <-chan workerRuntimeToolResult
	mu      sync.Mutex
}

func (c *workerRuntimeToolClient) call(ctx context.Context, requestContext *protocol.RequestContext, name, arguments string) (string, error) {
	if c == nil || c.writer == nil || c.results == nil {
		return "", errors.New("channel runtime tool transport is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	requestID := newZotigodID("runtime_tool")
	if err := c.writer.SendRuntimeToolRequest(ctx, workerRuntimeToolRequest{
		RequestID: requestID, Name: name, Arguments: []byte(arguments), RequestContext: requestContext,
	}); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result, ok := <-c.results:
			if !ok {
				return "", errors.New("channel runtime tool transport closed")
			}
			if result.RequestID != requestID {
				continue
			}
			if result.Error != "" {
				return "", errors.New(result.Error)
			}
			return result.Text, nil
		}
	}
}

type channelRuntimeToolState struct {
	mu             sync.RWMutex
	requestContext *protocol.RequestContext
	client         *workerRuntimeToolClient
}

func (s *channelRuntimeToolState) setRequestContext(value *protocol.RequestContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == nil {
		s.requestContext = nil
		return
	}
	copy := *value
	s.requestContext = &copy
}

func (s *channelRuntimeToolState) currentRequestContext() *protocol.RequestContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.requestContext == nil {
		return nil
	}
	copy := *s.requestContext
	return &copy
}

type channelReadMessagesTool struct{ state *channelRuntimeToolState }

func (*channelReadMessagesTool) Name() string { return "channel_read_messages" }
func (*channelReadMessagesTool) Description() string {
	return "Read recent messages from the Channel conversation for the current request. Use this when the user refers to group messages that were not included in the request."
}
func (*channelReadMessagesTool) Schema() any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 20}},
	}
}
func (*channelReadMessagesTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "reads retained messages from the current authorized Channel conversation"}
}
func (t *channelReadMessagesTool) Execute(ctx context.Context, _ executor.Executor, arguments string) (any, error) {
	requestContext := t.state.currentRequestContext()
	if requestContext == nil {
		return nil, errors.New("channel_read_messages requires an active Channel request")
	}
	return t.state.client.call(ctx, requestContext, channels.RuntimeToolReadMessages, arguments)
}
