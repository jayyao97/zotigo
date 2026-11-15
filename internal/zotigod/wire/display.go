package wire

import zotigosession "github.com/jayyao97/zotigo/core/session"

// Display payloads a worker streams to the daemon: a live delta of an
// unfinished block, and the notice that a durable item replaced a preview.

type DisplayDelta struct {
	BeforeInputID string                         `json:"before_input_id,omitempty"`
	ItemID        string                         `json:"item_id"`
	Role          string                         `json:"role"`
	PartType      string                         `json:"part_type"`
	Delta         string                         `json:"delta"`
	ToolCallID    string                         `json:"tool_call_id,omitempty"`
	ToolName      string                         `json:"tool_name,omitempty"`
	Subagent      *zotigosession.DisplaySubagent `json:"subagent,omitempty"`
}

type PreviewRetirement struct {
	ItemID      string   `json:"item_id"`
	ToolCallIDs []string `json:"tool_call_ids,omitempty"`
	Terminal    bool     `json:"terminal,omitempty"`
}
