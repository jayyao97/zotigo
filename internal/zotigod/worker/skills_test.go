package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestNativeRuntimeInjectsCompleteSelectedSkill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	path := writeDaemonTestSkill(t, root, "review-taste", "review", "review carefully")
	manager := skills.NewSkillManager("", skills.WithUserDir(root))
	message, err := messageFromCommandWithSkills("command-1", &wire.MessageCommandPayload{
		Text: "review this", Skills: []string{"review-taste"},
	}, manager)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := message.Content[0].Text
	if !strings.Contains(text, string(raw)) || !strings.HasSuffix(text, "review this") {
		t.Fatalf("native message did not include complete skill: %q", text)
	}
	if got := message.DisplayString(); got != "review this" {
		t.Fatalf("display text leaked injected skill: %q", got)
	}
	if got := sessionadapter.LastUserPrompt([]protocol.Message{message}); got != "review this" {
		t.Fatalf("last prompt leaked injected skill: %q", got)
	}
}
