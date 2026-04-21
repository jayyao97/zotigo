package openai

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// marshalJSON is a test helper that captures the shape of the
// serialized params payload. Responses API unions marshal by inlining
// whichever variant is set, so checking substrings in the JSON is the
// most robust way to assert we chose the right shape.
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestBuildResponseParams_TextOnlyUser(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewUserMessage("Hello there"),
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	// Text-only user messages should use the string shorthand for
	// content, not a content list.
	if !strings.Contains(js, `"content":"Hello there"`) {
		t.Errorf("expected text shorthand, got %s", js)
	}
	if strings.Contains(js, `"input_image"`) || strings.Contains(js, `"input_text"`) {
		t.Errorf("pure text should not promote to a content list, got %s", js)
	}
}

func TestBuildResponseParams_UserWithImageData(t *testing.T) {
	data := []byte("fake-png-bytes")
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "look at this"},
				{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{
					Data:      data,
					MediaType: "image/jpeg",
				}},
			},
		},
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)

	if !strings.Contains(js, `"input_text"`) || !strings.Contains(js, "look at this") {
		t.Errorf("expected input_text entry, got %s", js)
	}
	if !strings.Contains(js, `"input_image"`) {
		t.Errorf("expected input_image entry, got %s", js)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(js, b64) {
		t.Errorf("expected base64-encoded image payload, got %s", js)
	}
	if !strings.Contains(js, "data:image/jpeg;base64,") {
		t.Errorf("expected data URI with jpeg mime, got %s", js)
	}
}

func TestBuildResponseParams_UserWithImageURL(t *testing.T) {
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{
					URL: "https://example.com/cat.png",
				}},
			},
		},
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if !strings.Contains(js, "https://example.com/cat.png") {
		t.Errorf("expected URL passthrough, got %s", js)
	}
}

func TestBuildResponseParams_UserWithFileID(t *testing.T) {
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{
					FileID: "file-abc123",
				}},
			},
		},
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if !strings.Contains(js, `"file_id":"file-abc123"`) {
		t.Errorf("expected file_id entry, got %s", js)
	}
}

func TestBuildResponseParams_ImageWithNoPayloadErrors(t *testing.T) {
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{}},
			},
		},
	}
	_, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err == nil {
		t.Fatal("expected error for empty image part")
	}
}

type schemaTool struct{}

func (schemaTool) Name() string        { return "echo" }
func (schemaTool) Description() string { return "echo input" }
func (schemaTool) Schema() any {
	return map[string]any{
		"type":     "object",
		"required": []string{"msg"},
		"properties": map[string]any{
			"msg": map[string]any{"type": "string"},
		},
	}
}
func (schemaTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe}
}
func (schemaTool) Execute(_ any, _ any, _ string) (any, error) { return nil, nil }

func TestBuildResponseParams_SystemMessageBecomesInstructions(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewSystemMessage("Be concise."),
		protocol.NewUserMessage("hi"),
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if !strings.Contains(js, `"instructions":"Be concise."`) {
		t.Errorf("expected system prompt in instructions, got %s", js)
	}
	// User message still present.
	if !strings.Contains(js, `"content":"hi"`) {
		t.Errorf("expected user content, got %s", js)
	}
}
