package session

import (
	"fmt"
	"time"
)

type DisplayItemType string

const (
	DisplayItemUserMessage      DisplayItemType = "user_message"
	DisplayItemAssistantMessage DisplayItemType = "assistant_message"
	DisplayItemError            DisplayItemType = "error"
	DisplayItemTurnStarted      DisplayItemType = "turn_started"
	DisplayItemTurnPaused       DisplayItemType = "turn_paused"
	DisplayItemTurnCompleted    DisplayItemType = "turn_completed"
	DisplayItemTurnFailed       DisplayItemType = "turn_failed"
	DisplayItemTurnInterrupted  DisplayItemType = "turn_interrupted"
	DisplayItemContextCompacted DisplayItemType = "context_compacted"
)

type DisplayContentPart struct {
	Type       string             `json:"type"`
	Text       string             `json:"text,omitempty"`
	Summary    string             `json:"summary,omitempty"`
	ToolCall   *DisplayToolCall   `json:"tool_call,omitempty"`
	ToolResult *DisplayToolResult `json:"tool_result,omitempty"`
}

type DisplayToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type DisplayToolResult struct {
	ToolCallID string                         `json:"tool_call_id,omitempty"`
	ToolName   string                         `json:"tool_name,omitempty"`
	ResultType string                         `json:"result_type,omitempty"`
	Text       string                         `json:"text,omitempty"`
	JSON       any                            `json:"json,omitempty"`
	Reason     string                         `json:"reason,omitempty"`
	Content    []DisplayToolResultContentPart `json:"content,omitempty"`
	IsError    bool                           `json:"is_error,omitempty"`
	Metadata   map[string]any                 `json:"metadata,omitempty"`
}

type DisplayToolResultContentPart struct {
	Type  string            `json:"type"`
	Text  string            `json:"text,omitempty"`
	Image *DisplayMediaPart `json:"image,omitempty"`
}

type DisplayMediaPart struct {
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type DisplayTurn struct {
	ID                   string `json:"id,omitempty"`
	Reason               string `json:"reason,omitempty"`
	Status               string `json:"status,omitempty"`
	ProviderFinishReason string `json:"provider_finish_reason,omitempty"`
	LastAgentMessage     string `json:"last_agent_message,omitempty"`
	DurationMS           int64  `json:"duration_ms,omitempty"`
}

type DisplayItem struct {
	ID        string               `json:"id"`
	Sequence  uint64               `json:"sequence"`
	Type      DisplayItemType      `json:"type"`
	Role      string               `json:"role,omitempty"`
	Content   []DisplayContentPart `json:"content,omitempty"`
	Turn      *DisplayTurn         `json:"turn,omitempty"`
	Error     string               `json:"error,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
}

type DisplayPageQuery struct {
	Limit     int
	After     uint64
	Before    uint64
	HasAfter  bool
	HasBefore bool
}

type DisplayPage struct {
	Items      []DisplayItem
	NextCursor string
	PrevCursor string
	HasMore    bool
}

func PageDisplayItems(items []DisplayItem, query DisplayPageQuery) DisplayPage {
	total := len(items)
	start, end := displayWindow(items, query)
	pageItems := make([]DisplayItem, end-start)
	copy(pageItems, items[start:end])

	page := DisplayPage{Items: pageItems}
	if len(pageItems) == 0 {
		return page
	}

	first := pageItems[0].Sequence
	last := pageItems[len(pageItems)-1].Sequence
	if total > 0 && first > items[0].Sequence {
		page.PrevCursor = fmt.Sprintf("%d", first)
	}
	if last < items[total-1].Sequence {
		page.NextCursor = fmt.Sprintf("%d", last)
	}
	page.HasMore = page.PrevCursor != "" || page.NextCursor != ""
	return page
}

func displayWindow(items []DisplayItem, query DisplayPageQuery) (start int, end int) {
	total := len(items)
	if total == 0 {
		return 0, 0
	}
	switch {
	case query.HasAfter:
		start = firstDisplayIndexAfter(items, query.After)
		end = min(start+query.Limit, total)
	case query.HasBefore:
		end = firstDisplayIndexAtOrAfter(items, query.Before)
		start = end - query.Limit
		if start < 0 {
			start = 0
		}
	default:
		end = total
		start = end - query.Limit
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

func firstDisplayIndexAfter(items []DisplayItem, sequence uint64) int {
	for idx, item := range items {
		if item.Sequence > sequence {
			return idx
		}
	}
	return len(items)
}

func firstDisplayIndexAtOrAfter(items []DisplayItem, sequence uint64) int {
	for idx, item := range items {
		if item.Sequence >= sequence {
			return idx
		}
	}
	return len(items)
}
