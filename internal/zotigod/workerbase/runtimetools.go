package workerbase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type RuntimeToolClient struct {
	Writer   *Writer
	Results  <-chan wire.RuntimeToolResult
	gateOnce sync.Once
	gate     chan struct{}
}

func (c *RuntimeToolClient) call(ctx context.Context, requestContext *protocol.RequestContext, name, arguments string) (string, error) {
	return c.request(ctx, wire.RuntimeToolRequest{Name: name, Arguments: []byte(arguments), RequestContext: requestContext})
}

func (c *RuntimeToolClient) request(ctx context.Context, request wire.RuntimeToolRequest) (string, error) {
	if c == nil || c.Writer == nil || c.Results == nil {
		return "", errors.New("channel runtime tool transport is unavailable")
	}
	c.gateOnce.Do(func() { c.gate = make(chan struct{}, 1) })
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	requestID := sessionlog.NewID("runtime_tool")
	request.RequestID = requestID
	if err := c.Writer.SendRuntimeToolRequest(ctx, request); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result, ok := <-c.Results:
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

type ChannelRuntimeToolState struct {
	mu             sync.RWMutex
	requestContext *protocol.RequestContext
	Client         *RuntimeToolClient
}

func (s *ChannelRuntimeToolState) SetRequestContext(value *protocol.RequestContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == nil {
		s.requestContext = nil
		return
	}
	copy := *value
	s.requestContext = &copy
}

func (s *ChannelRuntimeToolState) currentRequestContext() *protocol.RequestContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.requestContext == nil {
		return nil
	}
	copy := *s.requestContext
	return &copy
}

type ChannelReadMessagesTool struct{ State *ChannelRuntimeToolState }

func (*ChannelReadMessagesTool) Name() string { return "channel_read_messages" }
func (*ChannelReadMessagesTool) Description() string {
	return "Read recent messages from the Channel conversation for the current request, including message IDs and the provider-fetched message directly referenced by the current request. Use this when the user refers to a quoted/replied-to message or earlier group messages that were not included in the request."
}
func (*ChannelReadMessagesTool) Schema() any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 20}},
	}
}
func (*ChannelReadMessagesTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "reads retained messages from the current authorized Channel conversation"}
}
func (t *ChannelReadMessagesTool) Execute(ctx context.Context, _ executor.Executor, arguments string) (any, error) {
	requestContext := t.State.currentRequestContext()
	if requestContext == nil {
		return nil, errors.New("channel_read_messages requires an active Channel request")
	}
	return t.State.Client.call(ctx, requestContext, channels.RuntimeToolReadMessages, arguments)
}

// Live admission already holds the source Channel configuration fence. Only
// durable command replay performs this second RPC, avoiding recursive RLocks
// while the daemon waits for a live worker admission.
func AuthorizeRuntimeToolReplay(ctx context.Context, client *RuntimeToolClient, commandID string) error {
	if !strings.HasPrefix(commandID, "session_tool:") && !strings.HasPrefix(commandID, "completion:") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	arguments, _ := json.Marshal(map[string]string{"command_id": commandID})
	_, err := client.request(ctx, wire.RuntimeToolRequest{Namespace: "host", Name: "authorize_input", Arguments: arguments})
	return err
}
