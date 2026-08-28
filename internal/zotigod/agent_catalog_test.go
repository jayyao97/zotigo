package zotigod

import (
	"encoding/json"
	"strings"
	"testing"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func TestCodexCatalogDoesNotApplyNativeMaxOutputTokens(t *testing.T) {
	catalog := codexCatalog(zotigoruntime.Capabilities{
		Models: []zotigoruntime.Model{{ID: "codex-test", DisplayName: "Codex Test", Default: true}},
	})
	payload, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if strings.Contains(string(payload), "max_output_tokens") {
		t.Fatalf("Codex app-server catalog inherited native runtime output limit: %s", payload)
	}
	if catalog.Capabilities.PreToolUseHooks || !catalog.Capabilities.PostToolUseHooks {
		t.Fatalf("unexpected Codex hook capabilities: %#v", catalog.Capabilities)
	}
}
