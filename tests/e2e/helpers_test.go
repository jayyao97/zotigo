//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// ============ String Helpers ============

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ============ Provider Profile Helpers ============

func shouldSkipProviderError(providerName string, err error) bool {
	if err == nil {
		return false
	}
	if providerName != "openai" {
		return false
	}
	msg := strings.ToLower(fmt.Sprintf("%v", err))
	return strings.Contains(msg, "only supported in v1/responses")
}

func isOpenRouterBaseURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}

// ============ Agent Helpers ============

func newTestAgent(t *testing.T, profile config.ProfileConfig) *agent.Agent {
	t.Helper()
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	pb := prompt.NewSystemPromptBuilder()
	pb.SetStaticPrompt("You are a concise assistant. Always reply in as few words as possible.")

	ag, err := agent.New(profile, exec,
		agent.WithSystemPromptBuilder(pb),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	return ag
}

func newCachingTestAgent(t *testing.T, profile config.ProfileConfig) *agent.Agent {
	t.Helper()
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	pb := prompt.NewSystemPromptBuilder()
	var largePrompt strings.Builder
	largePrompt.WriteString("You are a concise software engineering assistant. Always reply in as few words as possible.\n\n")
	largePrompt.WriteString("# Guidelines\n\n")
	for i := 0; i < 150; i++ {
		largePrompt.WriteString(fmt.Sprintf("Rule %d: When working with code, always consider readability, maintainability, "+
			"performance, security, and testability. Follow established patterns and conventions. "+
			"Document complex logic with clear comments. Use meaningful variable and function names. "+
			"Write unit tests for all new code. Ensure proper error handling and logging. "+
			"Use dependency injection for testability. Keep functions small and focused.\n", i+1))
	}
	pb.SetStaticPrompt(largePrompt.String())

	ag, err := agent.New(profile, exec,
		agent.WithSystemPromptBuilder(pb),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	toolNames := []string{
		"read_file", "write_file", "list_dir", "search_code",
		"run_command", "git_status", "git_diff", "edit_file",
	}
	for _, name := range toolNames {
		ag.RegisterTool(&dummyTool{name: name})
	}

	return ag
}

func runAndDrain(t *testing.T, ctx context.Context, ag *agent.Agent, input string) string {
	t.Helper()
	events, err := ag.Run(ctx, input)
	if err != nil {
		t.Fatalf("Run(%q) error: %v", input, err)
	}

	var content string
	for e := range events {
		if e.Type == protocol.EventTypeError {
			t.Fatalf("Stream error: %v", e.Error)
		}
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
			content += e.ContentPartDelta.Text
		}
	}
	t.Logf("  Response: %s", truncate(content, 80))
	return content
}

// ============ Mock Providers ============

type e2eVerboseProvider struct{}

func (p *e2eVerboseProvider) Name() string { return "e2e-verbose" }

func (p *e2eVerboseProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		response := "Thank you for your question. Let me provide a detailed explanation. " +
			"This topic has several important aspects to consider. " +
			"First, we should understand the fundamental concepts. " +
			"Second, we need to examine the practical applications. " +
			"Third, let's look at some real-world examples. " +
			"In conclusion, this is a fascinating subject with many nuances. " +
			"I hope this explanation was helpful and comprehensive!"
		ch <- protocol.NewTextDeltaEvent(response)
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

type e2eToolProvider struct {
	step int
}

func (p *e2eToolProvider) Name() string { return "e2e-tools" }

func (p *e2eToolProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		hasToolResult := false
		for _, msg := range messages {
			if msg.Role == protocol.RoleTool {
				hasToolResult = true
				break
			}
		}
		if !hasToolResult && len(t) > 0 {
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_" + string(rune('A'+p.step)),
					Name: "read_file",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_" + string(rune('A'+p.step)),
					Name:      "read_file",
					Arguments: `{"path": "test.go"}`,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
			p.step++
		} else {
			ch <- protocol.NewTextDeltaEvent("I've read the file and analyzed its contents. The code looks well-structured.")
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()
	return ch, nil
}

type e2eLargeResponseProvider struct{}

func (p *e2eLargeResponseProvider) Name() string { return "e2e-memory" }

func (p *e2eLargeResponseProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		largeResponse := strings.Repeat("This is a test response with substantial content. ", 50)
		ch <- protocol.NewTextDeltaEvent(largeResponse)
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

// ============ Mock Tools ============

type mockReadFileTool struct{}

func (t *mockReadFileTool) Name() string        { return "read_file" }
func (t *mockReadFileTool) Description() string { return "Reads a file" }
func (t *mockReadFileTool) Schema() any         { return nil }
func (t *mockReadFileTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true, PathArgs: []string{"path"}}
}

func (t *mockReadFileTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "// Mock file content\npackage main\n\nfunc main() {\n\tfmt.Println(\"Hello\")\n}", nil
}

type dummyTool struct {
	name string
}

func (d *dummyTool) Name() string        { return d.name }
func (d *dummyTool) Description() string { return "A dummy tool for testing" }
func (d *dummyTool) Schema() any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (d *dummyTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "ok", nil
}
func (d *dummyTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

// ============ Conversation Builders (for compressor tests) ============

func buildRealisticConversation() []protocol.Message {
	return []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "You are an expert Go developer helping with code review and debugging."},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'm working on a context compression feature for my AI agent CLI. Can you help me review the implementation?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'd be happy to help review your context compression implementation! Could you share the code you'd like me to look at? I'm particularly interested in seeing how you handle the compression algorithm, token counting, and preservation of important context."},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Let me show you the main compressor file. Can you read it?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
				ID: "call_1", Name: "read_file", Arguments: `{"path": "compressor.go"}`,
			}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
				ToolCallID: "call_1",
				Type:       protocol.ToolResultTypeText,
				Text:       "package services\n\n// Compressor manages context...\ntype Compressor struct {\n\tconfig CompressorConfig\n\t// ... more code ...\n}\n\nfunc (c *Compressor) Compress(...) {\n\t// Implementation\n}",
			}},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I've reviewed the compressor code. Here are my observations:\n\n1. The structure looks clean with good separation of concerns\n2. The token counting approach is reasonable\n3. I noticed you're using a configurable threshold - that's good for flexibility\n\nA few suggestions:\n- Consider adding more detailed logging for debugging\n- The partition algorithm could be optimized for edge cases\n- You might want to add benchmarks for performance testing"},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Good points. Can you also check the test file to see if we have good coverage?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
				ID: "call_2", Name: "read_file", Arguments: `{"path": "compressor_test.go"}`,
			}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
				ToolCallID: "call_2",
				Type:       protocol.ToolResultTypeText,
				Text:       "package services\n\nfunc TestCompressor_Compress(t *testing.T) {\n\t// Test implementation\n}\n\nfunc TestCompressor_TokenCount(t *testing.T) {\n\t// More tests\n}",
			}},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "The test coverage looks good! I see tests for the main compression logic and token counting. To improve further, you could add:\n\n1. Edge case tests (empty input, single message, etc.)\n2. Benchmark tests for performance\n3. Integration tests with the agent\n\nWould you like me to help write any of these additional tests?"},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Yes, please help me add the edge case tests first."},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'll create comprehensive edge case tests for you. Here's what I'm planning to cover:\n\n1. Empty message slice\n2. Only system message\n3. Only tool messages without user messages\n4. Single user message\n5. Messages that don't need compression\n6. Messages at exactly the threshold\n\nLet me write these tests now..."},
		}},
	}
}

func buildConversationWithToolChains() []protocol.Message {
	msgs := []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "You are a helpful coding assistant."},
		}},
	}
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("Please help with task ", 10) + string(rune('A'+i))},
			}},
			protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
					ID: "call_" + string(rune('a'+i)), Name: "read_file", Arguments: `{"path": "file.go"}`,
				}},
			}},
			protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
					ToolCallID: "call_" + string(rune('a'+i)),
					Type:       protocol.ToolResultTypeText,
					Text:       strings.Repeat("file content line ", 20),
				}},
			}},
			protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("Here's my analysis ", 15)},
			}},
		)
	}
	return msgs
}

func generateLargeCodeOutput() string {
	var sb strings.Builder
	sb.WriteString("package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n")
	for i := 0; i < 50; i++ {
		sb.WriteString("// Function " + string(rune('A'+i%26)) + " does something important\n")
		sb.WriteString("func function" + string(rune('A'+i%26)) + "(x int) int {\n")
		sb.WriteString("\tresult := x * 2\n")
		sb.WriteString("\tif result > 100 {\n")
		sb.WriteString("\t\treturn result - 50\n")
		sb.WriteString("\t}\n")
		sb.WriteString("\treturn result\n")
		sb.WriteString("}\n\n")
	}
	return sb.String()
}
