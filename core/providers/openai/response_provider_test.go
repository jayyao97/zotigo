package openai

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
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

func TestBuildResponseParams_IncludesEncryptedContentFlag(t *testing.T) {
	// Every Responses API request should ask for encrypted reasoning
	// blobs. Without this flag, the server won't include
	// encrypted_content in reasoning items and stateless multi-turn
	// chain-of-thought won't work.
	params, err := buildResponseParams("gpt-5", []protocol.Message{protocol.NewUserMessage("hi")}, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if !strings.Contains(js, `"reasoning.encrypted_content"`) {
		t.Errorf("expected include to request reasoning.encrypted_content, got %s", js)
	}
}

func TestBuildResponseParams_PassesReasoningItemsBack(t *testing.T) {
	// An assistant message with a reasoning ContentPart carrying
	// ReasoningID + EncryptedContent should flow into the request as
	// a typed reasoning input item — this is how stateless
	// chain-of-thought continuity works.
	asst := protocol.Message{Role: protocol.RoleAssistant}
	asst.Content = []protocol.ContentPart{
		{
			Type:             protocol.ContentTypeReasoning,
			Text:             "was thinking about X",
			ReasoningID:      "rs_abc123",
			EncryptedContent: "ENC_BLOB_XYZ",
		},
		{Type: protocol.ContentTypeText, Text: "plan: do the thing"},
	}
	msgs := []protocol.Message{
		protocol.NewUserMessage("start"),
		asst,
	}
	params, err := buildResponseParams("gpt-5", msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if !strings.Contains(js, `"reasoning"`) {
		t.Errorf("expected reasoning input item type, got %s", js)
	}
	if !strings.Contains(js, `"rs_abc123"`) {
		t.Errorf("expected reasoning id in input, got %s", js)
	}
	if !strings.Contains(js, `"ENC_BLOB_XYZ"`) {
		t.Errorf("expected encrypted_content in input, got %s", js)
	}
	// Assistant text should still be sent as its own message item.
	if !strings.Contains(js, `"plan: do the thing"`) {
		t.Errorf("expected text plan in input, got %s", js)
	}
}

func TestBuildResponseParams_DropsReasoningWithoutEncryptedContent(t *testing.T) {
	// A reasoning part with no ID / encrypted_content is the
	// "historical reasoning, never got server-issued continuity
	// token" case (e.g. loaded from an older session). We drop it
	// rather than emit a malformed reasoning input item.
	asst := protocol.Message{Role: protocol.RoleAssistant}
	asst.Content = []protocol.ContentPart{
		{Type: protocol.ContentTypeReasoning, Text: "no blob"},
		{Type: protocol.ContentTypeText, Text: "hi"},
	}
	params, err := buildResponseParams("gpt-5", []protocol.Message{asst}, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	js := marshalJSON(t, params)
	if strings.Contains(js, `"type":"reasoning"`) {
		t.Errorf("did not expect reasoning input item, got %s", js)
	}
}
