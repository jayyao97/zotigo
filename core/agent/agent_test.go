package agent_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	builtin "github.com/jayyao97/zotigo/core/tools/builtin"
)

// --- Mock Provider ---

type StepMockProvider struct {
	Step int
}

func (p *StepMockProvider) Name() string { return "mock" }

func (p *StepMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

func (t *TimeTool) Name() string        { return "get_time" }
func (t *TimeTool) Description() string { return "Returns current time" }
func (t *TimeTool) Schema() any         { return nil }
func (t *TimeTool) Classify(_ tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelLow}
}
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
	snapshotStderr   string
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

func (e *SnapshotTestExecutor) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	return e.base.Stat(ctx, path)
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
		stderr := e.snapshotStderr
		if stderr == "" && e.snapshotExitCode != 0 {
			stderr = "snapshot failed"
		}
		return &executor.ExecResult{
			ExitCode: e.snapshotExitCode,
			Stdout:   []byte(e.snapshotStdout),
			Stderr:   []byte(stderr),
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

func (p *ShellCallProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

	cfg := config.ProfileConfig{
		Provider: "mock-reminder",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec,
		agent.WithSafetyClassifier(&StaticSafetyClassifier{
			Response: agent.SafetyClassifierResponse{Decision: agent.SafetyClassifierDecisionAllow},
		}),
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

	cfg := config.ProfileConfig{
		Provider: "mock-reminder-multi",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec,
		agent.WithSafetyClassifier(&StaticSafetyClassifier{
			Response: agent.SafetyClassifierResponse{Decision: agent.SafetyClassifierDecisionAllow},
		}),
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
func (t *ReadFileMockTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	if tools.IsInSafeScope(call, []string{"path"}) && !tools.IsSensitivePath(call, []string{"path"}) {
		return tools.SafetyDecision{Level: tools.LevelSafe}
	}
	return tools.SafetyDecision{Level: tools.LevelMedium}
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

func (p *PathCallMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-high-risk-shell"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	// Attach a shell policy whose high-risk list flags `sudo` so the
	// test exercises the LevelHigh → classifier path (ask_user here,
	// since no classifier is wired up).
	shell, err := builtin.NewShellTool(builtin.WithPolicy(&builtin.ShellPolicy{
		HighRiskPatterns: []string{`sudo\s+`},
	}))
	if err != nil {
		t.Fatalf("shell tool: %v", err)
	}
	ag.RegisterTool(shell)
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

func TestAgent_AutoModeAutoExecutesWriteInWorkDir(t *testing.T) {
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

	cfg := config.ProfileConfig{
		Provider: "mock-write-file",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{Enabled: config.BoolPtr(true)},
		},
	}
	ag, err := agent.New(cfg, exec, agent.WithSafetyClassifier(&StaticSafetyClassifier{
		Response: agent.SafetyClassifierResponse{Decision: agent.SafetyClassifierDecisionAllow},
	}))
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
	// Auto mode + write in working dir + classifier=allow → auto-execute
	if lastEvent.FinishReason == "need_approval" {
		t.Fatal("Auto mode + classifier=allow should auto-execute write_file in working directory")
	}

	// File should actually be written
	if _, err := os.Stat(filepath.Join(tmpDir, "note.txt")); err != nil {
		t.Errorf("Expected note.txt to be created: %v", err)
	}
}

func TestAgent_AutoModePausesWriteOutsideWorkDir(t *testing.T) {
	// Write to an absolute path outside working directory → should still pause
	providers.Register("mock-write-outside", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"/tmp/evil.txt","content":"hack"}`,
		}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-write-outside"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "Write outside")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	// Auto mode but path is outside working dir → still requires approval
	if lastEvent.FinishReason != "need_approval" {
		t.Fatal("Auto mode should still require approval for writes outside working directory")
	}
}

func TestAgent_ManualModePausesWriteInWorkDir(t *testing.T) {
	// Manual mode → always pause for write_file, even in working dir
	providers.Register("mock-write-manual", func(cfg config.ProfileConfig) (providers.Provider, error) {
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

	cfg := config.ProfileConfig{Provider: "mock-write-manual"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "Write a note")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatal("Manual mode should require approval for write_file")
	}
}

func TestAgent_BlockedShellReturnsDeniedToolResult(t *testing.T) {
	providers.Register("mock-blocked-shell", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "rm -rf /"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-blocked-shell"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	shell, err := builtin.NewShellTool(builtin.WithPolicy(builtin.DefaultShellPolicy()))
	if err != nil {
		t.Fatalf("shell tool: %v", err)
	}
	ag.RegisterTool(shell)
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
				Enabled: config.BoolPtr(true),
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

// TestAgent_ReviewThresholdOffStillApprovesHighRisk verifies that setting
// review_threshold=off disables the classifier but does NOT silently
// auto-execute high-risk tool calls — they should fall back to user
// approval. "off" means "don't spend a classifier round-trip", not
// "approve everything that isn't hard-blocked".
func TestAgent_ReviewThresholdOffStillApprovesHighRisk(t *testing.T) {
	providers.Register("mock-off-high-risk", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "sudo rm /tmp/foo"}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider: "mock-off-high-risk",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled:         config.BoolPtr(false),
				ReviewThreshold: "off",
			},
		},
	}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	// Shell policy that flags sudo as high-risk so the tool call reaches
	// LevelHigh. Threshold=off should still promote it to approval.
	shell, err := builtin.NewShellTool(builtin.WithPolicy(&builtin.ShellPolicy{
		HighRiskPatterns: []string{`sudo\s+`},
	}))
	if err != nil {
		t.Fatalf("shell tool: %v", err)
	}
	ag.RegisterTool(shell)
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "run a sudo command")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("threshold=off should still require approval for LevelHigh, got finish=%s", lastEvent.FinishReason)
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

// TestAgent_SnapshotNotInstalledDegradesGracefully verifies that when the
// optional snap-commit binary is not on PATH, the agent marks the turn as
// failed-with-reason but lets the user's action proceed instead of aborting.
//
// Subtests cover both the err!=nil path (some executors surface missing-binary
// as an error) and the err=nil + exit=127 path (LocalExecutor swallows
// *exec.ExitError and returns only the result).
func TestAgent_SnapshotNotInstalledDegradesGracefully(t *testing.T) {
	run := func(t *testing.T, setup func(e *SnapshotTestExecutor)) {
		t.Helper()
		providers.Register(fmt.Sprintf("mock-snapshot-notfound-%s", t.Name()),
			func(cfg config.ProfileConfig) (providers.Provider, error) {
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
		setup(exec)

		cfg := config.ProfileConfig{Provider: fmt.Sprintf("mock-snapshot-notfound-%s", t.Name())}
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
			t.Fatalf("Approve should succeed when snap-commit is missing; got %v", err)
		}
		for range resumed {
		}

		// The write should still have happened.
		if _, err := os.Stat(filepath.Join(tmpDir, "note.txt")); err != nil {
			t.Errorf("Expected note.txt to be written despite missing snap-commit: %v", err)
		}

		turns := ag.AuditTurns()
		if len(turns) == 0 {
			t.Fatal("Expected at least one turn")
		}
		if turns[0].SnapshotStatus != agent.SnapshotStatusNotInstalled {
			t.Fatalf("Expected not_installed snapshot status, got %s", turns[0].SnapshotStatus)
		}
		if turns[0].SnapshotID != "" {
			t.Errorf("Expected empty SnapshotID for not_installed case, got %q", turns[0].SnapshotID)
		}
	}

	// Some executors may return a non-nil Go error (err message contains
	// "executable file not found").
	t.Run("err_nonnil_path", func(t *testing.T) {
		run(t, func(e *SnapshotTestExecutor) {
			e.snapshotErr = fmt.Errorf(`exec: "snap-commit": executable file not found in $PATH`)
		})
	})

	// Production LocalExecutor path: sh -c returns exit 127 with "command not
	// found" on stderr; exec.ExitError is swallowed so err is nil.
	t.Run("err_nil_exit_127_path", func(t *testing.T) {
		run(t, func(e *SnapshotTestExecutor) {
			e.snapshotExitCode = 127
			e.snapshotStderr = "sh: snap-commit: command not found"
		})
	})
}

// TestAgent_SnapshotNotInstalled_LocalExecutor exercises the graceful fallback
// against a real LocalExecutor end-to-end. Skips if snap-commit is actually
// installed on the test machine (in which case the missing-binary path isn't
// reachable).
func TestAgent_SnapshotNotInstalled_LocalExecutor(t *testing.T) {
	if _, err := osexec.LookPath("snap-commit"); err == nil {
		t.Skip("snap-commit is installed on this machine; cannot test missing-binary path")
	}

	// Set up a real git repo so isGitRepository returns true.
	tmpDir := t.TempDir()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not available; cannot set up test repo")
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := osexec.Command("git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	providers.Register("mock-snapshot-localexec", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{
			Tool: "write_file",
			Args: `{"path":"note.txt","content":"hello"}`,
		}, nil
	})

	localExec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-snapshot-localexec"}
	ag, err := agent.New(cfg, localExec)
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
		t.Fatalf("Approve should succeed when snap-commit is missing; got %v", err)
	}
	for range resumed {
	}

	// User's action must have completed.
	if _, err := os.Stat(filepath.Join(tmpDir, "note.txt")); err != nil {
		t.Errorf("Expected note.txt to be written despite missing snap-commit: %v", err)
	}

	// Audit should show the distinct "not installed" status, not generic "failed".
	turns := ag.AuditTurns()
	if len(turns) == 0 {
		t.Fatal("Expected at least one turn")
	}
	if turns[0].SnapshotStatus != agent.SnapshotStatusNotInstalled {
		t.Errorf("Expected SnapshotStatusNotInstalled, got %s", turns[0].SnapshotStatus)
	}
	// SnapshotID should be empty for not-installed case (not overloaded with a reason).
	if turns[0].SnapshotID != "" {
		t.Errorf("Expected empty SnapshotID for not-installed case, got %q", turns[0].SnapshotID)
	}
}

// TestAgent_CaptureRawAuditContext verifies that when the operator opts in
// via config, classifier-sourced audit events carry a bounded raw context
// dump; otherwise the field stays empty.
func TestAgent_CaptureRawAuditContext(t *testing.T) {
	providers.Register("mock-raw-audit", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &ShellCallProvider{Command: "touch a file"}, nil
	})

	run := func(t *testing.T, capture bool) []agent.AuditEvent {
		t.Helper()
		tmpDir := t.TempDir()
		exec, err := executor.NewLocalExecutor(tmpDir)
		if err != nil {
			t.Fatalf("Failed to create executor: %v", err)
		}

		cfg := config.ProfileConfig{
			Provider: "mock-raw-audit",
			Safety: config.SafetyProfileConfig{
				Classifier: config.SafetyClassifierConfig{
					Enabled:                config.BoolPtr(true),
					CaptureRawAuditContext: capture,
					MaxAuditContextChars:   500,
				},
			},
		}
		ag, err := agent.New(cfg, exec, agent.WithSafetyClassifier(&StaticSafetyClassifier{
			Response: agent.SafetyClassifierResponse{
				Decision: agent.SafetyClassifierDecisionAskUser,
				Reason:   "review before executing",
			},
		}))
		if err != nil {
			t.Fatalf("Failed to create agent: %v", err)
		}
		ag.RegisterTool(&builtin.ShellTool{})
		// Classifier is only called in Auto mode now.
		ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

		events, err := ag.Run(context.Background(), "please touch a file")
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		for range events {
		}

		turns := ag.AuditTurns()
		if len(turns) == 0 || len(turns[0].SafetyEvents) == 0 {
			t.Fatal("Expected safety events")
		}
		return turns[0].SafetyEvents
	}

	t.Run("disabled keeps raw context empty", func(t *testing.T) {
		events := run(t, false)
		for _, e := range events {
			if e.RawContext != "" {
				t.Errorf("RawContext should be empty when CaptureRawAuditContext=false, got %q", e.RawContext)
			}
		}
	})

	t.Run("enabled captures bounded raw context on classifier events", func(t *testing.T) {
		events := run(t, true)
		var found bool
		for _, e := range events {
			if e.DecisionSource != agent.SafetyDecisionSourceClassifier {
				continue
			}
			found = true
			if e.RawContext == "" {
				t.Error("Expected RawContext to be populated for classifier event")
			}
			if !strings.Contains(e.RawContext, "tool:") {
				t.Errorf("RawContext should contain tool name, got: %q", e.RawContext)
			}
			if len(e.RawContext) > 520 {
				t.Errorf("RawContext exceeds bounded limit: len=%d", len(e.RawContext))
			}
		}
		if !found {
			t.Fatal("Expected at least one classifier-sourced safety event")
		}
	})
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

func (p *StatsMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

func (p *VerboseMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

// EmptyThenTextProvider emits an empty response on the first call (no text,
// no tool call — simulating the "all output tokens went to thinking"
// failure mode) and a normal text response on subsequent calls.
type EmptyThenTextProvider struct {
	Step          int
	ReceivedMsgs  [][]protocol.Message
	SecondCallTxt string
}

func (p *EmptyThenTextProvider) Name() string { return "empty-then-text" }

func (p *EmptyThenTextProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.Step++
	snap := make([]protocol.Message, len(messages))
	copy(snap, messages)
	p.ReceivedMsgs = append(p.ReceivedMsgs, snap)

	ch := make(chan protocol.Event, 4)
	go func() {
		defer close(ch)
		if p.Step == 1 {
			// Empty: no content deltas, no tool calls.
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
			return
		}
		txt := p.SecondCallTxt
		if txt == "" {
			txt = "done"
		}
		ch <- protocol.NewTextDeltaEvent(txt)
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: txt},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

// AlwaysEmptyProvider always returns an empty response. Used to verify
// the retry cap prevents an infinite loop.
type AlwaysEmptyProvider struct{ Step int }

func (p *AlwaysEmptyProvider) Name() string { return "always-empty" }

func (p *AlwaysEmptyProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.Step++
	ch := make(chan protocol.Event, 1)
	go func() {
		defer close(ch)
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

func TestAgentEmptyResponseRecovery(t *testing.T) {
	prov := &EmptyThenTextProvider{SecondCallTxt: "recovered"}
	providers.Register("empty-then-text", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return prov, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	cfg := config.ProfileConfig{Provider: "empty-then-text"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	events, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var content string
	for e := range events {
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
			content += e.ContentPartDelta.Text
		}
	}

	if prov.Step != 2 {
		t.Errorf("expected provider called 2x (empty + recovery), got %d", prov.Step)
	}
	if content != "recovered" {
		t.Errorf("expected recovered content, got %q", content)
	}

	// The second call should have seen the injected system-reminder
	// nudge in its context.
	if len(prov.ReceivedMsgs) < 2 {
		t.Fatalf("expected 2 provider calls recorded, got %d", len(prov.ReceivedMsgs))
	}
	secondCtx := prov.ReceivedMsgs[1]
	foundNudge := false
	for _, m := range secondCtx {
		if m.Role != protocol.RoleUser {
			continue
		}
		for _, p := range m.Content {
			if p.Type == protocol.ContentTypeText && strings.Contains(p.Text, "<system-reminder>") && strings.Contains(p.Text, "no visible text") {
				foundNudge = true
			}
		}
	}
	if !foundNudge {
		t.Errorf("expected injected system-reminder nudge in second call context")
	}
}

// SequencedProvider emits a caller-controlled sequence of responses.
// Each element: if ToolName is set, emit a tool call; else if Text is set,
// emit that text; else emit nothing (empty response).
type SequencedStep struct {
	Text     string
	ToolName string
	ToolArgs string
	ToolID   string
}

type SequencedProvider struct {
	Steps []SequencedStep
	Step  int
}

func (p *SequencedProvider) Name() string { return "sequenced" }

func (p *SequencedProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	idx := p.Step
	p.Step++
	ch := make(chan protocol.Event, 6)
	go func() {
		defer close(ch)
		if idx >= len(p.Steps) {
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
			return
		}
		s := p.Steps[idx]
		switch {
		case s.ToolName != "":
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        s.ToolID,
					Name:      s.ToolName,
					Arguments: s.ToolArgs,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		case s.Text != "":
			ch <- protocol.NewTextDeltaEvent(s.Text)
			ch <- protocol.Event{
				Type:        protocol.EventTypeContentEnd,
				ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: s.Text},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		default:
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()
	return ch, nil
}

// TestAgentEmptyResponseRecoveryResetsAfterProgress verifies that the
// recovery budget refills after legit output — so a long multi-step turn
// can tolerate more than one empty response as long as they aren't
// consecutive.
func TestAgentEmptyResponseRecoveryResetsAfterProgress(t *testing.T) {
	prov := &SequencedProvider{
		Steps: []SequencedStep{
			{}, // 1: empty
			{ToolName: "get_time", ToolID: "call_1", ToolArgs: "{}"}, // 2: recovered, tool call
			// (tool result injected by runtime; agent loop re-enters model)
			{},              // 3: empty again
			{Text: "final"}, // 4: recovered
		},
	}
	providers.Register("sequenced-reset", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return prov, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	cfg := config.ProfileConfig{Provider: "sequenced-reset"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	events, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var content string
	for e := range events {
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
			content += e.ContentPartDelta.Text
		}
	}

	if prov.Step != 4 {
		t.Errorf("expected 4 provider calls (empty, tool, empty, text), got %d", prov.Step)
	}
	if content != "final" {
		t.Errorf("expected final content 'final', got %q", content)
	}
}

func TestAgentEmptyResponseRecoveryCapped(t *testing.T) {
	prov := &AlwaysEmptyProvider{}
	providers.Register("always-empty", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return prov, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	cfg := config.ProfileConfig{Provider: "always-empty"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	events, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}

	// Cap is 1 retry: expect exactly 2 provider calls, then give up.
	if prov.Step != 2 {
		t.Errorf("expected exactly 2 calls (original + 1 retry), got %d", prov.Step)
	}
}

func TestAgent_Describe(t *testing.T) {
	providers.Register("mock-describe", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}

	cfg := config.ProfileConfig{
		Provider:      "mock-describe",
		Model:         "mock-1",
		ThinkingLevel: "low",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled:         config.BoolPtr(true),
				ReviewThreshold: "high",
			},
		},
	}
	ag, err := agent.New(cfg, exec,
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
		agent.WithClassifierProfile("mini", config.ProfileConfig{Provider: "openai", Model: "gpt-4o-mini"}),
		agent.WithSafetyClassifier(&StaticSafetyClassifier{}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	d := ag.Describe()
	if d.Provider != "mock" {
		t.Errorf("expected provider=mock, got %q", d.Provider)
	}
	if d.Model != "mock-1" {
		t.Errorf("expected model=mock-1, got %q", d.Model)
	}
	if d.ThinkingLevel != "low" {
		t.Errorf("expected thinking=low, got %q", d.ThinkingLevel)
	}
	if d.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Errorf("expected auto policy, got %q", d.ApprovalPolicy)
	}
	if !d.ClassifierAvailable {
		t.Error("expected classifier available")
	}
	if d.ClassifierModel != "gpt-4o-mini" {
		t.Errorf("expected classifier model gpt-4o-mini, got %q", d.ClassifierModel)
	}
	if d.ReviewThreshold != "high" {
		t.Errorf("expected threshold=high, got %q", d.ReviewThreshold)
	}
}

func TestAgent_Describe_ClassifierDisabled(t *testing.T) {
	providers.Register("mock-describe-noclf", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	cfg := config.ProfileConfig{
		Provider: "mock-describe-noclf",
		Model:    "mock-1",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled: config.BoolPtr(false),
			},
		},
	}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	d := ag.Describe()
	if d.ClassifierEnabled {
		t.Error("expected ClassifierEnabled=false")
	}
	if d.ClassifierAvailable {
		t.Error("expected ClassifierAvailable=false")
	}
}

// reactiveMockProvider returns a context-length error on the first
// StreamChat and plain text on subsequent calls — exercises the
// agent's force-compact-and-retry path.
type reactiveMockProvider struct {
	calls          int
	failTimes      int // how many of the first calls return ErrContextLengthExceeded
	contextErrText string
}

func (p *reactiveMockProvider) Name() string { return "reactive-mock" }

func (p *reactiveMockProvider) StreamChat(_ context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 4)
	p.calls++
	go func() {
		defer close(ch)
		if p.calls <= p.failTimes {
			ch <- protocol.NewErrorEvent(providers.WrapIfContextLength(fmt.Errorf("HTTP 400: %s", p.contextErrText)))
			return
		}
		ch <- protocol.NewTextDeltaEvent("recovered")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "recovered"},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

func TestAgent_ReactiveCompact_RetriesOnContextLengthError(t *testing.T) {
	mock := &reactiveMockProvider{
		failTimes:      1,
		contextErrText: "prompt is too long: 250000 tokens",
	}
	providers.Register("reactive-mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return mock, nil
	})

	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("executor: %v", err)
	}

	ag, err := agent.New(config.ProfileConfig{Provider: "reactive-mock"}, exec)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	// Pre-populate history so ForceCompress has something to actually
	// partition. Without enough conversation messages the partitioner
	// returns 0 (nothing to compact) and recovery reports failure.
	seed := agent.Snapshot{History: buildSeedHistory(40)}
	ag.Restore(seed)

	events, err := ag.Run(context.Background(), "go on")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var sawText bool
	var lastFinish protocol.FinishReason
	for e := range events {
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil &&
			e.ContentPartDelta.Type != protocol.ContentTypeReasoning &&
			strings.Contains(e.ContentPartDelta.Text, "recovered") {
			sawText = true
		}
		if e.Type == protocol.EventTypeFinish {
			lastFinish = e.FinishReason
		}
		if e.Type == protocol.EventTypeError {
			t.Fatalf("error event leaked through reactive recovery: %v", e.Error)
		}
	}

	if mock.calls != 2 {
		t.Errorf("expected 2 StreamChat calls (initial + retry), got %d", mock.calls)
	}
	if !sawText {
		t.Error("did not see recovered content delta")
	}
	if lastFinish != protocol.FinishReasonStop {
		t.Errorf("expected final FinishReasonStop, got %q", lastFinish)
	}
}

func TestAgent_ReactiveCompact_OnlyRetriesOnce(t *testing.T) {
	mock := &reactiveMockProvider{
		failTimes:      99, // every call fails
		contextErrText: "maximum context length exceeded",
	}
	providers.Register("reactive-mock-loop", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return mock, nil
	})

	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	ag, err := agent.New(config.ProfileConfig{Provider: "reactive-mock-loop"}, exec)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ag.Restore(agent.Snapshot{History: buildSeedHistory(40)})

	events, err := ag.Run(context.Background(), "go on")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var sawError bool
	for e := range events {
		if e.Type == protocol.EventTypeError {
			sawError = true
		}
	}

	if !sawError {
		t.Error("expected error to surface after retry budget exhausted")
	}
	if mock.calls > 2 {
		t.Errorf("retry should be capped at 1 (2 total calls), got %d", mock.calls)
	}
}

// buildSeedHistory creates a synthetic conversation with enough size +
// alternation that the compressor's safe-partition routine returns a
// non-zero index. Each user message is padded to push token estimates
// past the preserve ratio.
func buildSeedHistory(turns int) []protocol.Message {
	pad := strings.Repeat("words ", 50)
	var hist []protocol.Message
	for i := 0; i < turns; i++ {
		hist = append(hist, protocol.NewUserMessage(fmt.Sprintf("turn %d %s", i, pad)))
		hist = append(hist, protocol.Message{
			Role:    protocol.RoleAssistant,
			Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: fmt.Sprintf("response %d %s", i, pad)}},
		})
	}
	return hist
}

// recordingObserver is a minimal observability.Observer that captures
// the sequence of method calls. Used to verify the agent fires hooks
// in the expected order.
type recordingObserver struct {
	calls []string
}

func (r *recordingObserver) StartTurn(ctx context.Context, _ protocol.Message) context.Context {
	r.calls = append(r.calls, "StartTurn")
	return ctx
}
func (r *recordingObserver) EndTurn(_ context.Context, _ error) {
	r.calls = append(r.calls, "EndTurn")
}
func (r *recordingObserver) StartGeneration(ctx context.Context, kind observability.GenerationKind, _ string, _ []protocol.Message) context.Context {
	r.calls = append(r.calls, "StartGeneration:"+string(kind))
	return ctx
}
func (r *recordingObserver) EndGeneration(_ context.Context, _ observability.GenerationOutput, _ *protocol.Usage, _ error) {
	r.calls = append(r.calls, "EndGeneration")
}
func (r *recordingObserver) StartTool(ctx context.Context, name, _ string) context.Context {
	r.calls = append(r.calls, "StartTool:"+name)
	return ctx
}
func (r *recordingObserver) EndTool(_ context.Context, _ any, _ error) {
	r.calls = append(r.calls, "EndTool")
}
func (r *recordingObserver) Close(_ context.Context) error { return nil }

func TestAgent_ObserverFiresExpectedHookSequence(t *testing.T) {
	providers.Register("plain-mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &plainMockProvider{}, nil
	})

	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	rec := &recordingObserver{}
	ag, err := agent.New(config.ProfileConfig{Provider: "plain-mock"}, exec,
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	events, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for range events {
	}

	// The mock returns a simple text reply with no tool calls, so we
	// expect: StartTurn → StartGeneration:main → EndGeneration → EndTurn.
	want := []string{"StartTurn", "StartGeneration:main", "EndGeneration", "EndTurn"}
	if got := rec.calls; !equalStringSlice(got, want) {
		t.Errorf("hook order = %v, want %v", got, want)
	}
}

// plainMockProvider returns a single text reply, no tool calls — used
// to exercise the simplest happy-path through the agent loop.
type plainMockProvider struct{}

func (p *plainMockProvider) Name() string { return "plain-mock" }
func (p *plainMockProvider) StreamChat(_ context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 4)
	go func() {
		defer close(ch)
		ch <- protocol.NewTextDeltaEvent("ok")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "ok"},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
