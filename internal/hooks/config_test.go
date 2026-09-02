package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileDefaultsAndMatcherList(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	data := []byte(`version: 1
hooks:
  PreToolUse:
    - command: /tmp/check
      agents: [zotigo, codex]
      matchers: [shell, write_file, edit]
  PostToolUse:
    - command: /tmp/audit
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	config, issues, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	pre := config.Hooks[PreToolUse]
	if len(pre) != 1 || len(pre[0].Agents) != 2 || len(pre[0].Matchers) != 3 || pre[0].Async {
		t.Fatalf("unexpected PreToolUse handler: %#v", pre)
	}
	post := config.Hooks[PostToolUse]
	if len(post) != 1 || !post[0].Async {
		t.Fatalf("PostToolUse should default to async: %#v", post)
	}
}

func TestLoadFileSkipsInvalidHandlers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	data := []byte(`version: 1
hooks:
  SessionStart:
    - command: /tmp/valid
    - command: /tmp/invalid
      matchers: [shell]
  PreToolUse:
    - command: /tmp/unknown-field
      matcher: shell
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	config, issues, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Hooks[SessionStart]) != 1 {
		t.Fatalf("valid handler was not preserved: %#v", config.Hooks[SessionStart])
	}
	if len(config.Hooks[PreToolUse]) != 0 || len(issues) != 2 {
		t.Fatalf("invalid handlers were not skipped: hooks=%#v issues=%v", config.Hooks, issues)
	}
}

func TestLoadFileMissingIsEmpty(t *testing.T) {
	config, issues, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil || len(issues) != 0 || config.Version != ConfigVersion || len(config.Hooks) != 0 {
		t.Fatalf("missing config should be empty: config=%#v issues=%v err=%v", config, issues, err)
	}
}

func TestLoadFileRejectsExplicitZeroTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(`version: 1
hooks:
  PreToolUse:
    - command: /tmp/check
      timeout_ms: 0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, issues, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Hooks[PreToolUse]) != 0 || len(issues) != 1 {
		t.Fatalf("zero timeout should skip handler: hooks=%#v issues=%v", config.Hooks, issues)
	}
}

func TestLoadFileRejectsEmptyAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(`version: 1
hooks:
  SessionStart:
    - command: /tmp/audit
      agents: [zotigo, ""]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, issues, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Hooks[SessionStart]) != 0 || len(issues) != 1 {
		t.Fatalf("empty agent should skip handler: hooks=%#v issues=%v", config.Hooks, issues)
	}
}
