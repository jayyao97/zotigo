package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestToolImageRemainsInsideResult(t *testing.T) {
	block := convertToolResult(&protocol.ToolResult{ToolCallID: "image-call", Type: protocol.ToolResultTypeContent, Content: []protocol.ToolResultContentPart{
		{Type: protocol.ContentTypeText, Text: "Image: sample.png"},
		{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("image"), MediaType: "image/png"}},
	}})
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Type    string `json:"type"`
		ID      string `json:"tool_use_id"`
		Content []struct {
			Type   string `json:"type"`
			Source struct {
				Data      string `json:"data"`
				MediaType string `json:"media_type"`
			} `json:"source"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Type != "tool_result" || wire.ID != "image-call" || len(wire.Content) != 2 || wire.Content[1].Type != "image" || wire.Content[1].Source.Data != "aW1hZ2U=" || wire.Content[1].Source.MediaType != "image/png" {
		t.Fatalf("image result malformed: %s", data)
	}
}
