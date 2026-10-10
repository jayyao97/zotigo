package gemini

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

func TestToolImageFollowsFunctionResponse(t *testing.T) {
	msgs := []protocol.Message{protocol.NewToolMessage([]protocol.ToolResult{{ToolCallID: "image-call", ToolName: "view_image", Type: protocol.ToolResultTypeContent, Content: []protocol.ToolResultContentPart{
		{Type: protocol.ContentTypeText, Text: "Image: sample.png"},
		{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("image"), MediaType: "image/png"}},
	}}})}
	contents, _, err := convertToGeminiParams(msgs, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 3 {
		t.Fatal("unexpected message split")
	}
	result := contents[0].Parts[0].FunctionResponse
	if result == nil || result.ID != "image-call" || result.Name != "view_image" || result.Response["output"] != "Image: sample.png" {
		t.Fatalf("result: %+v", result)
	}
	media := contents[0].Parts[2].InlineData
	if media == nil || string(media.Data) != "image" || media.MIMEType != "image/png" {
		t.Fatalf("image: %+v", media)
	}
}
