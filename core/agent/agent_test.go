package agent_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/sandbox"
	"github.com/jayyao97/zotigo/core/tools"
	builtin "github.com/jayyao97/zotigo/core/tools/builtin"
)

// --- Mock Provider ---

type StepMockProvider struct {
	Step int
}

func (p *StepMockProvider) Name() string { return "mock" }

func (p *StepMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)

	go func() {
		defer close(ch)
		p.Step++

		if p.Step == 1 {
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_1",
					Name: "get_time",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_1",
					Name:      "get_time",
					Arguments: "{}",
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		} else {
			ch <- protocol.NewTextDeltaEvent("It is 12:00")
			ch <- protocol.Event{
				Type:        protocol.EventTypeContentEnd,
				ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "It is 12:00"},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()

	return ch, nil
}

// --- Mock Tool ---

type TimeTool struct{}

func (t *TimeTool) Name() string             { return "get_time" }
func (t *TimeTool) Description() string      { return "Returns current time" }
func (t *TimeTool) Schema() any              { return nil }
func (t *TimeTool) Safety() tools.ToolSafety { return tools.ToolSafety{ReadOnly: false} }
func (t *TimeTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "12:00", nil
}

type ShellCallProvider struct {
	Command string
	Step    int
	Tool    string
	Args    string
}

type StaticSafetyClassifier struct {
	Response agent.SafetyClassifierResponse
	Err      error
}

func (c *StaticSafetyClassifier) Classify(req agent.SafetyClassifierRequest) (agent.SafetyClassifierResponse, error) {
	if c.Err != nil {
		return agent.SafetyClassifierResponse{}, c.Err
	}
	return c.Response, nil
}

type SnapshotTestExecutor struct {
	base             executor.Executor
	isGitRepo        bool
	snapshotStdout   string
	snapshotErr      error
	snapshotExitCode int
	snapshotCalls    int
	recordedCommands []string
}

func NewSnapshotTestExecutor(base executor.Executor, isGitRepo bool) *SnapshotTestExecutor {
	return &SnapshotTestExecutor{
		base:             base,
		isGitRepo:        isGitRepo,
		snapshotStdout:   "snap-123",
		snapshotExitCode: 0,
	}
}

func (e *SnapshotTestExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return e.base.ReadFile(ctx, path)
}

func (e *SnapshotTestExecutor) WriteFile(ctx context.Context, path string, content []byte, perm fs.FileMode) error {
	return e.base.WriteFile(ctx, path, content, perm)
}

func (e *SnapshotTestExecutor) ListDir(ctx context.Context, path string) ([]executor.FileInfo, error) {
	return e.base.ListDir(ctx, path)
}

func (e *SnapshotTestExecutor) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	return e.base.Stat(ctx, path)
}

func (e *SnapshotTestExecutor) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	return e.base.MkdirAll(ctx, path, perm)
}

func (e *SnapshotTestExecutor) Remove(ctx context.Context, path string) error {
	return e.base.Remove(ctx, path)
}

func (e *SnapshotTestExecutor) Exec(ctx context.Context, cmd string, opts executor.ExecOptions) (*executor.ExecResult, error) {
	e.recordedCommands = append(e.recordedCommands, cmd)
	switch cmd {
	case "git rev-parse --is-inside-work-tree":
		if e.isGitRepo {
			return &executor.ExecResult{ExitCode: 0, Stdout: []byte("true\n")}, nil
		}
		return &executor.ExecResult{ExitCode: 1, Stderr: []byte("fatal")}, nil
	case `snap-commit store -m "zotigo pre-action snapshot"`:
		e.snapshotCalls++
		if e.snapshotErr != nil {
			return nil, e.snapshotErr
		}
		return &executor.ExecResult{
			ExitCode: e.snapshotExitCode,
			Stdout:   []byte(e.snapshotStdout),
			Stderr:   []byte("snapshot failed"),
		}, nil
	default:
		return e.base.Exec(ctx, cmd, opts)
	}
}

func (e *SnapshotTestExecutor) WorkDir() string                { return e.base.WorkDir() }
func (e *SnapshotTestExecutor) Platform() string               { return e.base.Platform() }
func (e *SnapshotTestExecutor) Init(ctx context.Context) error { return e.base.Init(ctx) }
func (e *SnapshotTestExecutor) Close() error                   { return e.base.Close() }

func (p *ShellCallProvider) Name() string { return "shell-mock" }

func (p *ShellCallProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		p.Step++
		if p.Step == 1 {
			toolName := p.Tool
			if toolName == "" {
				toolName = "shell"
			}
			args := p.Args
			if args == "" {
				args = `{"command":"` + p.Command + `"}`
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_1",
					Name: toolName,
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_1",
					Name:      toolName,
					Arguments: args,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
			return
		}

		ch <- protocol.NewTextDeltaEvent("done")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "done"},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

// --- Test ---

func TestAgentReActLoop(t *testing.T) {
	providers.Register("mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})

	// Create a temp directory for executor
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "What time is it?")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	if string(lastEvent.FinishReason) != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	toolRes := protocol.NewTextToolResult("call_1", "12:00", false)

	events2, err := ag.SubmitToolOutputs(context.Background(), []protocol.ToolResult{toolRes})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}

	var content string
	for e := range events2 {
		if e.Type == protocol.EventTypeContentDelta {
			content += e.ContentPartDelta.Text
		}
		lastEvent = e
	}

	if lastEvent.FinishReason != protocol.FinishReasonStop {
		t.Errorf("Expected stop, got %s", lastEvent.FinishReason)
	}
	if content != "It is 12:00" {
		t.Errorf("Expected 'It is 12:00', got '%s'", content)
	}
}

// ============ Compression Integration Tests ============

func TestAgentGetContextStats(t *testing.T) {
	providers.Register("mock-stats", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StatsMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-stats"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Initial stats
	stats := ag.GetContextStats()
	if stats["message_count"] != 0 {
		t.Errorf("Expected 0 messages initially, got %d", stats["message_count"])
	}

	// Run a conversation
	events, err := ag.Run(context.Background(), "Hello")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
		// Drain events
	}

	// Stats should update
	stats = ag.GetContextStats()
	if stats["message_count"] < 2 {
		t.Errorf("Expected at least 2 messages (user + assistant), got %d", stats["message_count"])
	}
	if stats["estimated_tokens"] <= 0 {
		t.Errorf("Expected positive token count, got %d", stats["estimated_tokens"])
	}

	t.Logf("Context stats: messages=%d, tokens=%d", stats["message_count"], stats["estimated_tokens"])
}

func TestAgentForceCompress(t *testing.T) {
	providers.Register("mock-compress", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &VerboseMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-compress"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Run multiple conversations to build up history
	for i := 0; i < 5; i++ {
		events, err := ag.Run(context.Background(), "Tell me a long story about topic "+string(rune('A'+i)))
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		for range events {
			// Drain events
		}
	}

	// Get stats before compression
	statsBefore := ag.GetContextStats()
	t.Logf("Before compression: messages=%d, tokens=%d",
		statsBefore["message_count"], statsBefore["estimated_tokens"])

	// Force compress
	result, err := ag.ForceCompress(context.Background())
	if err != nil {
		t.Fatalf("ForceCompress error: %v", err)
	}

	t.Logf("Compression result: compressed=%v, before=%d, after=%d tokens",
		result.Compressed, result.OriginalTokens, result.CompressedTokens)

	// If compression happened, verify tokens reduced
	if result.Compressed {
		if result.CompressedTokens >= result.OriginalTokens {
			t.Errorf("Compression should reduce tokens: %d >= %d",
				result.CompressedTokens, result.OriginalTokens)
		}

		statsAfter := ag.GetContextStats()
		t.Logf("After compression: messages=%d, tokens=%d",
			statsAfter["message_count"], statsAfter["estimated_tokens"])
	}
}

func TestAgentResetLoopDetector(t *testing.T) {
	providers.Register("mock-loop", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StatsMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-loop"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Reset should not panic even with no history
	ag.ResetLoopDetector()

	// Run a conversation
	events, _ := ag.Run(context.Background(), "Test")
	for range events {
	}

	// Reset again
	ag.ResetLoopDetector()

	// Loop stats should be reset
	stats := ag.GetContextStats()
	if loopCalls, ok := stats["loop_total_calls"]; ok && loopCalls > 0 {
		t.Logf("Loop stats after reset: %v", stats)
	}
}

// ============ Reminder Provider Tests ============

func TestAgent_ReminderAppendedToLastToolResult(t *testing.T) {
	providers.Register("mock-reminder", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-reminder"}
	ag, err := agent.New(cfg, exec,
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return "Remember: stay focused on the task."
		}),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "What time is it?")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	// Inspect history for the tool message
	snap := ag.Snapshot()
	found := false
	for _, msg := range snap.History {
		if msg.Role != protocol.RoleTool {
			continue
		}
		for _, cp := range msg.Content {
			if cp.ToolResult == nil {
				continue
			}
			if strings.Contains(cp.ToolResult.Text, "<system-reminder>") &&
				strings.Contains(cp.ToolResult.Text, "Remember: stay focused on the task.") &&
				strings.Contains(cp.ToolResult.Text, "</system-reminder>") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("Expected <system-reminder> with reminder text in the last tool result")
	}
}

func TestAgent_ReminderSkipsEmpty(t *testing.T) {
	providers.Register("mock-reminder-empty", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-reminder-empty"}
	ag, err := agent.New(cfg, exec,
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return ""
		}),
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return "   "
		}),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "What time is it?")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	snap := ag.Snapshot()
	for _, msg := range snap.History {
		if msg.Role != protocol.RoleTool {
			continue
		}
		for _, cp := range msg.Content {
			if cp.ToolResult == nil {
				continue
			}
			if strings.Contains(cp.ToolResult.Text, "<system-reminder>") {
				t.Fatal("Expected no <system-reminder> when all providers return empty")
			}
		}
	}
}

func TestAgent_MultipleReminders(t *testing.T) {
	providers.Register("mock-reminder-multi", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-reminder-multi"}
	ag, err := agent.New(cfg, exec,
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return "Reminder A"
		}),
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return "" // should be skipped
		}),
		agent.WithReminder(func(_ prompt.PromptContext, _ []prompt.ToolCallResult) string {
			return "Reminder B"
		}),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "What time is it?")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	snap := ag.Snapshot()
	found := false
	for _, msg := range snap.History {
		if msg.Role != protocol.RoleTool {
			continue
		}
		for _, cp := range msg.Content {
			if cp.ToolResult == nil {
				continue
			}
			if strings.Contains(cp.ToolResult.Text, "<system-reminder>") {
				// Verify both reminders in single tag
				if !strings.Contains(cp.ToolResult.Text, "Reminder A") {
					t.Error("Expected 'Reminder A' in system-reminder")
				}
				if !strings.Contains(cp.ToolResult.Text, "Reminder B") {
					t.Error("Expected 'Reminder B' in system-reminder")
				}
				// Count occurrences of <system-reminder> — should be exactly 1
				count := strings.Count(cp.ToolResult.Text, "<system-reminder>")
				if count != 1 {
					t.Errorf("Expected exactly 1 <system-reminder> tag, got %d", count)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("Expected <system-reminder> with multiple reminders in the last tool result")
	}
}

// ============ SafeDirs & AddSafeDirs Tests ============

// ReadFileMockTool simulates a read-only tool with path arguments.
type ReadFileMockTool struct {
	LastPath string
}

func (t *ReadFileMockTool) Name() string        { return "read_file" }
func (t *ReadFileMockTool) Description() string { return "Read a file" }
func (t *ReadFileMockTool) Schema() any         { return nil }
func (t *ReadFileMockTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true, PathArgs: []string{"path"}}
}
func (t *ReadFileMockTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	t.LastPath = args
	return "file content", nil
}

// PathCallMockProvider generates a tool call to read_file with a specific path.
type PathCallMockProvider struct {
	Path string
	Step int
}

func (p *PathCallMockProvider) Name() string { return "path-call-mock" }

func (p *PathCallMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		p.Step++
		if p.Step == 1 {
			args := `{"path":"` + p.Path + `"}`
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_read",
					Name: "read_file",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_read",
					Name:      "read_file",
					Arguments: args,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		} else {
			ch <- protocol.NewTextDeltaEvent("Done")
			ch <- protocol.Event{
				Type:        protocol.EventTypeContentEnd,
				ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Done"},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()
	return ch, nil
}

func TestAgent_SafeDir_AutoApprovesReadInWorkDir(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := tmpDir + "/test.txt"

	providers.Register("mock-safedir-workdir", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &PathCallMockProvider{Path: filePath}, nil
	})

	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-safedir-workdir"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&ReadFileMockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "Read the file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Read-only tool in workDir should be auto-approved, not paused
	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	// Should complete without needing approval (auto-approved + response)
	if lastEvent.FinishReason == "need_approval" {
		t.Error("Read-only tool in workDir should be auto-approved, not need_approval")
	}
}

func TestAgent_SafeDir_BlocksReadOutsideWorkDir(t *testing.T) {
	tmpDir := t.TempDir()
	outsidePath := "/etc/passwd"

	providers.Register("mock-safedir-outside", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &PathCallMockProvider{Path: outsidePath}, nil
	})

	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-safedir-outside"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&ReadFileMockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "Read the file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	// Path outside workDir should require approval
	if lastEvent.FinishReason != "need_approval" {
		t.Errorf("Read-only tool outside workDir should need approval, got %s", lastEvent.FinishReason)
	}
}

func TestAgent_AutoApprovePausesHighRiskShell(t *testing.T) {
	providers.Register("mock-high-risk-shell", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "sudo ls"}, nil
	})

	tmpDir := t.TempDir()
	localExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	guard, err := sandbox.NewGuard(localExec, nil)
	if err != nil {
		t.Fatalf("Failed to create guard: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-high-risk-shell"}
	ag, err := agent.New(cfg, guard)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "List files with sudo")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	snap := ag.Snapshot()
	if len(snap.Turns) != 1 || len(snap.Turns[0].SafetyEvents) == 0 {
		t.Fatalf("Expected safety events to be recorded for the turn")
	}
	event := snap.Turns[0].SafetyEvents[0]
	if event.Decision != agent.SafetyClassifierDecisionAskUser {
		t.Fatalf("Expected ask_user decision, got %s", event.Decision)
	}
}

func TestAgent_AutoApprovePausesProtectedWriteTool(t *testing.T) {
	providers.Register("mock-write-file", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-write-file"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "Write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}
}

func TestAgent_BlockedShellReturnsDeniedToolResult(t *testing.T) {
	providers.Register("mock-blocked-shell", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "rm -rf /"}, nil
	})

	tmpDir := t.TempDir()
	localExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	guard, err := sandbox.NewGuard(localExec, nil)
	if err != nil {
		t.Fatalf("Failed to create guard: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-blocked-shell"}
	ag, err := agent.New(cfg, guard)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "Delete root")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != protocol.FinishReasonStop {
		t.Fatalf("Expected stop, got %s", lastEvent.FinishReason)
	}

	snap := ag.Snapshot()
	foundDenied := false
	for _, msg := range snap.History {
		if msg.Role != protocol.RoleTool {
			continue
		}
		for _, cp := range msg.Content {
			if cp.ToolResult != nil && cp.ToolResult.Type == protocol.ToolResultTypeExecutionDenied {
				foundDenied = true
			}
		}
	}
	if !foundDenied {
		t.Fatal("Expected execution denied tool result in history")
	}
}

func TestAgent_ClassifierAllowAutoExecutesWhenConfigured(t *testing.T) {
	providers.Register("mock-classifier-allow", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "touch note.txt"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider: "mock-classifier-allow",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled:                 config.BoolPtr(true),
				AllowAutoExecuteOnAllow: true,
			},
		},
	}
	ag, err := agent.New(cfg, exec, agent.WithSafetyClassifier(&StaticSafetyClassifier{
		Response: agent.SafetyClassifierResponse{
			Decision: agent.SafetyClassifierDecisionAllow,
			Reason:   "classifier allowed command",
		},
	}))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "touch a file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason == "need_approval" {
		t.Fatal("Expected classifier allow to auto-execute when configured")
	}

	turns := ag.AuditTurns()
	if len(turns) == 0 || len(turns[0].SafetyEvents) == 0 {
		t.Fatal("Expected safety events")
	}
	if turns[0].SafetyEvents[0].DecisionSource != agent.SafetyDecisionSourceClassifier {
		t.Fatalf("Expected classifier decision source, got %s", turns[0].SafetyEvents[0].DecisionSource)
	}
}

func TestAgent_ClassifierDenyReturnsDeniedResult(t *testing.T) {
	providers.Register("mock-classifier-deny", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "touch note.txt"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider: "mock-classifier-deny",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec, agent.WithSafetyClassifier(&StaticSafetyClassifier{
		Response: agent.SafetyClassifierResponse{
			Decision: agent.SafetyClassifierDecisionDeny,
			Reason:   "denied by classifier",
		},
	}))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "touch a file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	snap := ag.Snapshot()
	foundDenied := false
	for _, msg := range snap.History {
		if msg.Role != protocol.RoleTool {
			continue
		}
		for _, cp := range msg.Content {
			if cp.ToolResult != nil && cp.ToolResult.Type == protocol.ToolResultTypeExecutionDenied &&
				strings.Contains(cp.ToolResult.Reason, "denied by classifier") {
				foundDenied = true
			}
		}
	}
	if !foundDenied {
		t.Fatal("Expected denied tool result from classifier deny")
	}
}

func TestAgent_ClassifierAskUserPauses(t *testing.T) {
	providers.Register("mock-classifier-ask", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "touch note.txt"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider: "mock-classifier-ask",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec, agent.WithSafetyClassifier(&StaticSafetyClassifier{
		Response: agent.SafetyClassifierResponse{
			Decision: agent.SafetyClassifierDecisionAskUser,
			Reason:   "need explicit approval",
		},
	}))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "touch a file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}
}

func TestAgent_ClassifierEnabledWithoutImplementationFallsBackToApproval(t *testing.T) {
	providers.Register("mock-classifier-missing", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "touch note.txt"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider: "mock-classifier-missing",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec,
		agent.WithClassifierUnavailableReason(`classifier profile "missing-mini" not found`),
		agent.WithClassifierProfile("gpt-4o", config.ProfileConfig{Provider: "openai", Model: "gpt-4o"}),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.ShellTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "touch a file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	turns := ag.AuditTurns()
	if len(turns) == 0 || len(turns[0].SafetyEvents) == 0 {
		t.Fatal("Expected safety event")
	}
	ev := turns[0].SafetyEvents[0]
	if ev.DecisionSource != agent.SafetyDecisionSourceClassifier {
		t.Fatalf("Expected classifier decision source, got %s", ev.DecisionSource)
	}
	if !strings.Contains(ev.Reason, `classifier profile "missing-mini" not found`) {
		t.Fatalf("Expected missing profile reason, got %q", ev.Reason)
	}
	if ev.ClassifierProvider != "openai" || ev.ClassifierModel != "gpt-4o" {
		t.Fatalf("Expected classifier metadata from resolved/current profile, got %s/%s", ev.ClassifierProvider, ev.ClassifierModel)
	}
}

func TestAgent_FirstProtectedActionCreatesSingleSnapshot(t *testing.T) {
	providers.Register("mock-snapshot-once", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	baseExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	exec := NewSnapshotTestExecutor(baseExec, true)

	cfg := config.ProfileConfig{Provider: "mock-snapshot-once"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	if exec.snapshotCalls != 0 {
		t.Fatalf("Snapshot should not run before approval, got %d", exec.snapshotCalls)
	}

	resumed, err := ag.ApproveAndExecutePendingActions(context.Background())
	if err != nil {
		t.Fatalf("ApproveAndExecutePendingActions error: %v", err)
	}
	for range resumed {
	}

	if exec.snapshotCalls != 1 {
		t.Fatalf("Expected exactly one snapshot call, got %d", exec.snapshotCalls)
	}

	turns := ag.AuditTurns()
	if len(turns) != 1 {
		t.Fatalf("Expected one turn, got %d", len(turns))
	}
	if turns[0].SnapshotStatus != agent.SnapshotStatusCreated {
		t.Fatalf("Expected created snapshot status, got %s", turns[0].SnapshotStatus)
	}
}

func TestAgent_SnapshotFailureStopsExecution(t *testing.T) {
	providers.Register("mock-snapshot-fail", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	baseExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	exec := NewSnapshotTestExecutor(baseExec, true)
	exec.snapshotErr = context.DeadlineExceeded

	cfg := config.ProfileConfig{Provider: "mock-snapshot-fail"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	resumed, err := ag.ApproveAndExecutePendingActions(context.Background())
	if err == nil {
		for range resumed {
		}
		t.Fatal("Expected snapshot failure to stop execution")
	}

	turns := ag.AuditTurns()
	if turns[0].SnapshotStatus != agent.SnapshotStatusFailed {
		t.Fatalf("Expected failed snapshot status, got %s", turns[0].SnapshotStatus)
	}
}

func TestAgent_NonGitWorkspaceDoesNotSnapshot(t *testing.T) {
	providers.Register("mock-snapshot-nongit", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	baseExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	exec := NewSnapshotTestExecutor(baseExec, false)

	cfg := config.ProfileConfig{Provider: "mock-snapshot-nongit"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	resumed, err := ag.ApproveAndExecutePendingActions(context.Background())
	if err != nil {
		t.Fatalf("ApproveAndExecutePendingActions error: %v", err)
	}
	for range resumed {
	}

	if exec.snapshotCalls != 0 {
		t.Fatalf("Expected no snapshot calls in non-git workspace, got %d", exec.snapshotCalls)
	}

	turns := ag.AuditTurns()
	if turns[0].SnapshotStatus != agent.SnapshotStatusMissingGitRepo {
		t.Fatalf("Expected missing_git_repo snapshot status, got %s", turns[0].SnapshotStatus)
	}
}

func TestAgent_DenyOutputsRecordedAsUserApprovalAudit(t *testing.T) {
	providers.Register("mock-deny-audit", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-deny-audit"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
	}

	_, err = ag.SubmitToolOutputs(context.Background(), []protocol.ToolResult{
		{
			ToolCallID: "call_1",
			ToolName:   "write_file",
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     "User denied in test",
		},
	})
	if err != nil {
		t.Fatalf("SubmitToolOutputs error: %v", err)
	}

	turns := ag.AuditTurns()
	found := false
	for _, ev := range turns[0].SafetyEvents {
		if ev.DecisionSource == agent.SafetyDecisionSourceUserApproval &&
			ev.Decision == agent.SafetyClassifierDecisionDeny &&
			strings.Contains(ev.Reason, "User denied in test") {
			found = true
		}
	}
	if !found {
		t.Fatal("Expected deny audit event")
	}
}

func TestAgent_AddSafeDirs_ExpandsAutoApproval(t *testing.T) {
	workDir := t.TempDir()
	extraDir := t.TempDir()
	filePath := extraDir + "/data.txt"

	providers.Register("mock-safedir-extra", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &PathCallMockProvider{Path: filePath}, nil
	})

	exec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-safedir-extra"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&ReadFileMockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	// Register the extra dir as safe
	ag.AddSafeDirs(extraDir)

	events, err := ag.Run(context.Background(), "Read the file")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	// Should be auto-approved because extraDir was registered
	if lastEvent.FinishReason == "need_approval" {
		t.Error("Read-only tool in AddSafeDirs directory should be auto-approved")
	}
}

func TestAgent_WithTranscriptDir_RegistersSafeDir(t *testing.T) {
	workDir := t.TempDir()
	transcriptDir := t.TempDir()
	filePath := transcriptDir + "/transcript_123.jsonl"

	providers.Register("mock-safedir-transcript", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &PathCallMockProvider{Path: filePath}, nil
	})

	exec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-safedir-transcript"}
	ag, err := agent.New(cfg, exec,
		agent.WithTranscriptDir(transcriptDir),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&ReadFileMockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "Read the transcript")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	// Should be auto-approved because WithTranscriptDir registers the dir as safe
	if lastEvent.FinishReason == "need_approval" {
		t.Error("Read-only tool in transcript dir should be auto-approved via WithTranscriptDir")
	}
}

// --- Additional Mock Providers ---

type StatsMockProvider struct{}

func (p *StatsMockProvider) Name() string { return "stats-mock" }

func (p *StatsMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		ch <- protocol.NewTextDeltaEvent("Hello! I'm a helpful assistant.")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Hello! I'm a helpful assistant."},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

type VerboseMockProvider struct{}

func (p *VerboseMockProvider) Name() string { return "verbose-mock" }

func (p *VerboseMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		// Generate a long response to build up token count
		longResponse := "This is a very detailed and verbose response. " +
			"Let me explain in great detail about the topic you mentioned. " +
			"Here are some important points to consider: First, we need to understand the basics. " +
			"Second, we should explore the implications. Third, let's look at some examples. " +
			"Finally, we can draw some conclusions from our analysis. " +
			"I hope this explanation was helpful and comprehensive!"
		ch <- protocol.NewTextDeltaEvent(longResponse)
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: longResponse},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}
