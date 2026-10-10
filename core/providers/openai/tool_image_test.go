package openai

import (
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

func TestToolImageRequestFormats(t *testing.T) {
	msgs := []protocol.Message{protocol.NewToolMessage([]protocol.ToolResult{
		{ToolCallID: "image-call", ToolName: "view_image", Type: protocol.ToolResultTypeContent, Content: []protocol.ToolResultContentPart{
			{Type: protocol.ContentTypeText, Text: "Image: sample.png"},
			{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("image"), MediaType: "image/png"}},
		}},
		protocol.NewTextToolResult("text-call", "done", false),
	})}
	params, err := buildResponseParams("gpt-5", 100, msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 2 || wire.Input[0].Type != "function_call_output" || wire.Input[0].CallID != "image-call" {
		t.Fatalf("wrong tool envelope: %s", data)
	}
	var output []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(wire.Input[0].Output, &output); err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || output[1].Type != "input_image" || output[1].ImageURL != "data:image/png;base64,aW1hZ2U=" {
		t.Fatalf("image was not structured: %s", data)
	}
	if string(wire.Input[1].Output) != `"done"` {
		t.Fatal("text result format changed")
	}
	chat, err := convertToChatParams(msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 || chat.Messages[0].OfTool.ToolCallID != "image-call" || chat.Messages[1].OfTool.ToolCallID != "text-call" || chat.Messages[2].OfUser == nil {
		t.Fatal("Chat image must follow all tool results")
	}
	image := chat.Messages[2].OfUser.Content.OfArrayOfContentParts[1].OfImageURL
	if image == nil || image.ImageURL.URL != "data:image/png;base64,aW1hZ2U=" {
		t.Fatal("Chat image missing")
	}
	if msgs[0].Role != protocol.RoleTool {
		t.Fatal("conversion mutated history")
	}
}
