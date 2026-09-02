package hooks

import (
	"encoding/json"
	"testing"
)

func TestSessionUsagePayloadJSONContract(t *testing.T) {
	event := NewEvent(SessionEnd, "sess-test", "zotigo", "/workspace")
	event.Session = &SessionPayload{
		Result: "ended",
		Model:  "gpt-test",
		Usage: &UsagePayload{
			InputTokens: 10, OutputTokens: 4, TotalTokens: 20,
			CacheCreationInputTokens: 2, CacheReadInputTokens: 4,
		},
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Session struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				TotalTokens              int `json:"total_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"session"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Session.Model != "gpt-test" || decoded.Session.Usage.InputTokens != 10 ||
		decoded.Session.Usage.OutputTokens != 4 || decoded.Session.Usage.TotalTokens != 20 ||
		decoded.Session.Usage.CacheCreationInputTokens != 2 || decoded.Session.Usage.CacheReadInputTokens != 4 {
		t.Fatalf("unexpected hook JSON: %s", payload)
	}
}
