package prompt

import (
	"strings"
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

func TestDynamicContext_AddSection(t *testing.T) {
	dc := &DynamicContext{}
	dc.AddSection("env", "linux")
	dc.AddSection("empty", "")
	dc.AddSection("whitespace", "   ")

	if len(dc.Sections) != 1 {
		t.Fatalf("Expected 1 section (empty/whitespace skipped), got %d", len(dc.Sections))
	}
	if dc.Sections[0].Tag != "env" {
		t.Errorf("Expected tag 'env', got '%s'", dc.Sections[0].Tag)
	}
}

func TestDynamicContext_Build(t *testing.T) {
	dc := &DynamicContext{}
	if dc.Build() != "" {
		t.Error("Empty DynamicContext should build to empty string")
	}

	dc.AddSection("environment", "Working directory: /tmp")
	dc.AddSection("project", "Go project")

	result := dc.Build()
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
	msgs := pb.BuildMessages()
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message (static only), got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "Zotigo") {
		t.Error("Static message should contain 'Zotigo'")
	}
}

func TestSystemPromptBuilder_BuildMessages_WithDynamic(t *testing.T) {
	pb := NewSystemPromptBuilder()
	pb.DynamicContext.AddSection("env", "cwd: /tmp")

	msgs := pb.BuildMessages()
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

	msgs := pb.BuildMessages()
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(msgs))
	}
	if msgs[0] != "Custom system prompt." {
		t.Errorf("Expected custom prompt, got '%s'", msgs[0])
	}
}

func TestSystemPromptBuilder_EmptyStaticPrompt(t *testing.T) {
	pb := NewSystemPromptBuilder()
	pb.SetStaticPrompt("")

	msgs := pb.BuildMessages()
	if len(msgs) != 0 {
		t.Fatalf("Expected 0 messages with empty static and no dynamic, got %d", len(msgs))
	}
}

func TestUserPromptWrapper_NoContext(t *testing.T) {
	w := NewUserPromptWrapper()
	result := w.Wrap("Hello world")
	if result != "Hello world" {
		t.Errorf("Expected raw input unchanged, got '%s'", result)
	}
}

func TestUserPromptWrapper_WithContext(t *testing.T) {
	w := NewUserPromptWrapper()
	w.AddContext("file", "main.go contents here")

	result := w.Wrap("Fix the bug")
	if !strings.Contains(result, "<user_query>") {
		t.Error("Expected <user_query> tag")
	}
	if !strings.Contains(result, "Fix the bug") {
		t.Error("Expected original input in result")
	}
	if !strings.Contains(result, "<file>") {
		t.Error("Expected <file> context tag")
	}
	if !strings.Contains(result, "main.go contents here") {
		t.Error("Expected file content in result")
	}
}

func TestUserPromptWrapper_SkipEmpty(t *testing.T) {
	w := NewUserPromptWrapper()
	w.AddContext("empty", "")
	w.AddContext("whitespace", "   ")

	result := w.Wrap("Hello")
	if result != "Hello" {
		t.Errorf("Expected raw input (no context added), got '%s'", result)
	}
}
