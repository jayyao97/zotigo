package codexworker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func newTestStoreRoot(t *testing.T) string {
	t.Helper()
	return catalogtest.NewRoot(t)
}

func tinyPNGBase64() string {
	return "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
}

type capturingHookDispatcher struct {
	events []hooks.Event
}

func (d *capturingHookDispatcher) Dispatch(_ context.Context, event hooks.Event) hooks.DispatchResult {
	d.events = append(d.events, event)
	return hooks.DispatchResult{}
}

func writeDaemonTestSkill(t *testing.T, root, name, description, instructions string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + instructions + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
