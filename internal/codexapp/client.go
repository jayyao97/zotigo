package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type callResult struct {
	result json.RawMessage
	err    error
}

type Client struct {
	conn          *websocket.Conn
	writeMu       sync.Mutex
	pendingMu     sync.Mutex
	pending       map[uint64]chan callResult
	notifications chan Message
	done          chan struct{}
	closeOnce     sync.Once
	nextID        atomic.Uint64
}

type RPC interface {
	Call(context.Context, string, any, any) error
	Notify(string, any) error
}

func (c *Client) Done() <-chan struct{} {
	return c.done
}

func Dial(ctx context.Context, socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("codex socket path is required")
	}
	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	conn, response, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("dial codex app-server UDS: http %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("dial codex app-server UDS: %w", err)
	}
	client := &Client{
		conn:          conn,
		pending:       make(map[uint64]chan callResult),
		notifications: make(chan Message, 128),
		done:          make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if method == "" {
		return fmt.Errorf("codex app-server method is required")
	}
	id := c.nextID.Add(1)
	waiter := make(chan callResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = waiter
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("codex app-server connection closed")
	case response := <-waiter:
		if response.err != nil {
			return response.err
		}
		if result == nil || len(response.result) == 0 || string(response.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.writeJSON(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) RespondError(id json.RawMessage, code int, message string) error {
	if len(id) == 0 {
		return fmt.Errorf("codex app-server request id is required")
	}
	var decodedID any
	if err := json.Unmarshal(id, &decodedID); err != nil {
		return fmt.Errorf("decode codex app-server request id: %w", err)
	}
	return c.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      decodedID,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (c *Client) Notifications() <-chan Message {
	return c.notifications
}

func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		close(c.done)
		closeErr = c.conn.Close()
	})
	return closeErr
}

func (c *Client) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return fmt.Errorf("codex app-server connection closed")
	default:
	}
	if err := c.conn.WriteJSON(value); err != nil {
		return fmt.Errorf("write codex app-server message: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer func() {
		_ = c.Close()
		c.failPending(errors.New("codex app-server connection closed"))
		close(c.notifications)
	}()
	for {
		var message Message
		if err := c.conn.ReadJSON(&message); err != nil {
			return
		}
		if len(message.ID) > 0 && message.Method == "" {
			var id uint64
			if err := json.Unmarshal(message.ID, &id); err != nil {
				continue
			}
			c.pendingMu.Lock()
			waiter := c.pending[id]
			c.pendingMu.Unlock()
			if waiter != nil {
				response := callResult{result: message.Result}
				if message.Error != nil {
					response.err = message.Error
				}
				waiter <- response
			}
			continue
		}
		select {
		case c.notifications <- message:
		case <-c.done:
			return
		}
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for _, waiter := range c.pending {
		select {
		case waiter <- callResult{err: err}:
		default:
		}
	}
}
