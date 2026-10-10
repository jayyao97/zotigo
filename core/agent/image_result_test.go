package agent

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

type imageResultTestTool struct{}

func (*imageResultTestTool) Name() string        { return "view_image" }
func (*imageResultTestTool) Description() string { return "test image output" }
func (*imageResultTestTool) Schema() any         { return nil }
func (*imageResultTestTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe}
}

func (*imageResultTestTool) Execute(context.Context, executor.Executor, string) (any, error) {
	return protocol.ToolResult{ToolCallID: "untrusted", ToolName: "untrusted", Type: protocol.ToolResultTypeContent,
		Content: []protocol.ToolResultContentPart{{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("image"), MediaType: "image/png"}}}}, nil
}

func TestToolExecutionPreservesImageResult(t *testing.T) {
	ag := &Agent{tools: map[string]tools.Tool{"view_image": &imageResultTestTool{}}}
	result := ag.executePendingAction(context.Background(), nil, &PendingAction{ToolCallID: "call-image", Name: "view_image"}, "loop warning", nil, func() {})
	if result.ToolCallID != "call-image" || result.ToolName != "view_image" || result.Type != protocol.ToolResultTypeContent {
		t.Fatalf("result identity/type: %+v", result)
	}
	if len(result.Content) != 2 || result.Content[0].Text != "loop warning" || string(result.Content[1].Image.Data) != "image" {
		t.Fatalf("image/warning lost: %+v", result)
	}
	if result.Text != "" {
		t.Fatal("image was flattened into text")
	}
}
