package zotigodclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

// Delta is a volatile streaming preview. The durable item with the same
// ItemID replaces it; deltas may be dropped and are never replayed.
type Delta struct {
	BeforeInputID string                   `json:"before_input_id,omitempty"`
	ItemID        string                   `json:"item_id"`
	Role          string                   `json:"role"`
	PartType      string                   `json:"part_type"`
	Delta         string                   `json:"delta"`
	ToolCallID    string                   `json:"tool_call_id,omitempty"`
	ToolName      string                   `json:"tool_name,omitempty"`
	Subagent      *session.DisplaySubagent `json:"subagent,omitempty"`
}

// Event is one element of a session event stream. Exactly one of Item and
// Delta is set for data events. Connection-state events set Reconnecting
// (with the error that caused it) or, as the final event before the channel
// closes, Err.
type Event struct {
	Item         *session.DisplayItem
	Delta        *Delta
	Reconnecting bool
	Err          error
}

// StreamOptions tunes Subscribe. Zero values select defaults.
type StreamOptions struct {
	// GiveUpAfter bounds how long consecutive connection failures are
	// retried before the stream ends with an error.
	GiveUpAfter time.Duration
}

// Subscribe streams durable items with sequence > after, followed by live
// items and deltas, until ctx ends. After a transient failure it reconnects
// from the last delivered durable sequence, so items are delivered once and
// in order; deltas lost during the gap are not recovered (their durable
// items still arrive). The channel is closed when the stream ends.
func (c *Client) Subscribe(ctx context.Context, id string, after uint64, opts StreamOptions) <-chan Event {
	if opts.GiveUpAfter <= 0 {
		opts.GiveUpAfter = time.Minute
	}
	events := make(chan Event, 256)
	go func() {
		defer close(events)
		cursor := after
		var failingSince time.Time
		backoff := 100 * time.Millisecond
		for {
			delivered, err := c.streamOnce(ctx, id, &cursor, events)
			if ctx.Err() != nil {
				return
			}
			if delivered {
				failingSince = time.Time{}
				backoff = 100 * time.Millisecond
			}
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
				send(ctx, events, Event{Err: err})
				return
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) > opts.GiveUpAfter {
				send(ctx, events, Event{Err: fmt.Errorf("session event stream lost: %w", err)})
				return
			}
			if !send(ctx, events, Event{Reconnecting: true, Err: err}) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 2*time.Second)
		}
	}()
	return events
}

func send(ctx context.Context, events chan<- Event, event Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamOnce runs one SSE connection. delivered reports whether the
// connection was established, which resets the reconnect budget.
func (c *Client) streamOnce(ctx context.Context, id string, cursor *uint64, events chan<- Event) (bool, error) {
	path := c.baseURL + "/sessions/" + url.PathEscape(id) + "/events"
	if *cursor > 0 {
		path += "?after=" + strconv.FormatUint(*cursor, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return false, decodeAPIError(resp.StatusCode, data)
	}

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var eventName string
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return true, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if eventName != "" || data.Len() > 0 {
				if !dispatch(ctx, eventName, data.String(), cursor, events) {
					return true, ctx.Err()
				}
			}
			eventName = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// Heartbeat or comment.
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func dispatch(ctx context.Context, name string, payload string, cursor *uint64, events chan<- Event) bool {
	switch name {
	case "item":
		var item session.DisplayItem
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return true
		}
		// Reconnects replay from the cursor; drop anything already seen.
		if item.Sequence <= *cursor {
			return true
		}
		*cursor = item.Sequence
		return send(ctx, events, Event{Item: &item})
	case "delta":
		var delta Delta
		if err := json.Unmarshal([]byte(payload), &delta); err != nil {
			return true
		}
		return send(ctx, events, Event{Delta: &delta})
	default:
		return true
	}
}
