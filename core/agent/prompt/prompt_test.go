package prompt

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestStaticSystemPrompt_Embedded(t *testing.T) {
	if StaticSystemPrompt == "" {
		t.Fatal("StaticSystemPrompt should not be empty")
	}
	if !strings.Contains(StaticSystemPrompt, "Zotigo") {
		t.Error("StaticSystemPrompt should contain 'Zotigo'")
	}
}

func TestDynamicContext_WithSection(t *testing.T) {
	dc := NewDynamicContext(
		WithSection("env", func(_ PromptContext) string { return "linux" }),
	)

	if len(dc.Sections) != 1 {
		t.Fatalf("Expected 1 section, got %d", len(dc.Sections))
	}
	if dc.Sections[0].Tag != "env" {
		t.Errorf("Expected tag 'env', got '%s'", dc.Sections[0].Tag)
	}
}

func TestDynamicContext_Build(t *testing.T) {
	dc := NewDynamicContext()
	if dc.Build(PromptContext{}) != "" {
		t.Error("Empty DynamicContext should build to empty string")
	}

	dc = NewDynamicContext(
		WithSection("environment", func(_ PromptContext) string { return "Working directory: /tmp" }),
		WithSection("project", func(_ PromptContext) string { return "Go project" }),
	)

	result := dc.Build(PromptContext{})
	if !strings.Contains(result, "<environment>") {
		t.Error("Expected <environment> tag")
	}
	if !strings.Contains(result, "</environment>") {
		t.Error("Expected </environment> closing tag")
	}
	if !strings.Contains(result, "<project>") {
		t.Error("Expected <project> tag")
	}
	if !strings.Contains(result, "Working directory: /tmp") {
		t.Error("Expected environment content")
	}
}

func TestSystemPromptBuilder_Default(t *testing.T) {
	pb := NewSystemPromptBuilder()
	if pb.StaticPrompt == "" {
		t.Fatal("Default builder should have StaticPrompt")
	}
	if pb.DynamicContext == nil {
		t.Fatal("Default builder should have DynamicContext")
	}
}

func TestSystemPromptBuilder_BuildMessages_StaticOnly(t *testing.T) {
	pb := NewSystemPromptBuilder()
	msgs := pb.BuildMessages(PromptContext{})
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message (static only), got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "Zotigo") {
		t.Error("Static message should contain 'Zotigo'")
	}
}

func TestSystemPromptBuilder_BuildMessages_WithDynamic(t *testing.T) {
	pb := NewSystemPromptBuilder(
		WithDynamicSection("env", func(_ PromptContext) string { return "cwd: /tmp" }),
	)

	msgs := pb.BuildMessages(PromptContext{})
	if len(msgs) != 2 {
		t.Fatalf("Expected 2 messages (static + dynamic), got %d", len(msgs))
	}
	if !strings.Contains(msgs[1], "<env>") {
		t.Error("Dynamic message should contain <env> tag")
	}
}

func TestSystemPromptBuilder_SetStaticPrompt(t *testing.T) {
	pb := NewSystemPromptBuilder()
	pb.SetStaticPrompt("Custom system prompt.")

	msgs := pb.BuildMessages(PromptContext{})
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(msgs))
	}
	if msgs[0] != "Custom system prompt." {
		t.Errorf("Expected custom prompt, got '%s'", msgs[0])
	}
}

func TestSystemPromptBuilder_WithStaticPromptOption(t *testing.T) {
	pb := NewSystemPromptBuilder(WithStaticPrompt("Option prompt."))

	msgs := pb.BuildMessages(PromptContext{})
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(msgs))
	}
	if msgs[0] != "Option prompt." {
		t.Errorf("Expected 'Option prompt.', got '%s'", msgs[0])
	}
}

func TestSystemPromptBuilder_EmptyStaticPrompt(t *testing.T) {
	pb := NewSystemPromptBuilder()
	pb.SetStaticPrompt("")

	msgs := pb.BuildMessages(PromptContext{})
	if len(msgs) != 0 {
		t.Fatalf("Expected 0 messages with empty static and no dynamic, got %d", len(msgs))
	}
}

func TestUserContextBuilderNoContext(t *testing.T) {
	w := NewUserContextBuilder()
	result := w.BuildMetaUserContext(PromptContext{})
	if result != "" {
		t.Errorf("Expected empty context, got '%s'", result)
	}
}

func TestUserContextBuilderWithContext(t *testing.T) {
	w := NewUserContextBuilder(
		WithContext("file", func(_ PromptContext) string { return "main.go contents here" }),
	)

	result := w.BuildMetaUserContext(PromptContext{})
	if !strings.Contains(result, "<user_context>") {
		t.Errorf("context should be wrapped in <user_context>, got %q", result)
	}
	if !strings.Contains(result, "<file>") || !strings.Contains(result, "main.go contents here") {
		t.Error("context section should be rendered as <file>...</file>")
	}
	if strings.Contains(result, "Fix the bug") {
		t.Errorf("real user input should not be part of meta context: %q", result)
	}
}

func TestUserContextBuilderSkipEmpty(t *testing.T) {
	w := NewUserContextBuilder(
		WithContext("empty", func(_ PromptContext) string { return "" }),
		WithContext("whitespace", func(_ PromptContext) string { return "   " }),
	)

	result := w.BuildMetaUserContext(PromptContext{})
	if result != "" {
		t.Errorf("Expected empty context, got '%s'", result)
	}
}

func TestDynamicContext_LazyEvaluation(t *testing.T) {
	var callCount int64
	dc := NewDynamicContext(
		WithSection("counter", func(_ PromptContext) string {
			n := atomic.AddInt64(&callCount, 1)
			if n == 1 {
				return "first"
			}
			return "second"
		}),
	)

	r1 := dc.Build(PromptContext{})
	if !strings.Contains(r1, "first") {
		t.Errorf("First call should contain 'first', got '%s'", r1)
	}

	r2 := dc.Build(PromptContext{})
	if !strings.Contains(r2, "second") {
		t.Errorf("Second call should contain 'second', got '%s'", r2)
	}

	if atomic.LoadInt64(&callCount) != 2 {
		t.Errorf("Provider should have been called twice, got %d", atomic.LoadInt64(&callCount))
	}
}

func TestDynamicContext_EmptyProvider(t *testing.T) {
	dc := NewDynamicContext(
		WithSection("present", func(_ PromptContext) string { return "data" }),
		WithSection("absent", func(_ PromptContext) string { return "" }),
		WithSection("blank", func(_ PromptContext) string { return "   " }),
	)

	result := dc.Build(PromptContext{})
	if !strings.Contains(result, "<present>") {
		t.Error("Expected <present> tag")
	}
	if strings.Contains(result, "<absent>") {
		t.Error("Empty provider section should be skipped")
	}
	if strings.Contains(result, "<blank>") {
		t.Error("Whitespace-only provider section should be skipped")
	}
}

func TestUserContextBuilderLazyContext(t *testing.T) {
	var receivedCtx PromptContext
	w := NewUserContextBuilder(
		WithContext("info", func(ctx PromptContext) string {
			receivedCtx = ctx
			return "dir=" + ctx.WorkDir
		}),
	)

	pctx := PromptContext{WorkDir: "/my/project", SessionID: "sess-123"}
	result := w.BuildMetaUserContext(pctx)

	if receivedCtx.WorkDir != "/my/project" {
		t.Errorf("Provider should receive WorkDir '/my/project', got '%s'", receivedCtx.WorkDir)
	}
	if receivedCtx.SessionID != "sess-123" {
		t.Errorf("Provider should receive SessionID 'sess-123', got '%s'", receivedCtx.SessionID)
	}
	if !strings.Contains(result, "dir=/my/project") {
		t.Errorf("Context result should contain provider output, got '%s'", result)
	}
}

func TestUserContextBuilderAttributedSection(t *testing.T) {
	w := NewUserContextBuilder(
		WithAttributedContext("project_instructions", `source="AGENTS.md"`, func(_ PromptContext) string {
			return "project rules"
		}),
	)

	result := w.BuildMetaUserContext(PromptContext{})
	if !strings.Contains(result, `<project_instructions source="AGENTS.md">`) {
		t.Fatalf("expected attributed opening tag, got %q", result)
	}
	if !strings.Contains(result, "</project_instructions>") {
		t.Fatalf("expected normal closing tag, got %q", result)
	}
}
