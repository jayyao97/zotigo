package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/transport"
)

type spawnApprovalRequesterFunc func(context.Context, SpawnApprovalRequest) ([]transport.ApprovalResult, error)

func (f spawnApprovalRequesterFunc) RequestSpawnApproval(ctx context.Context, request SpawnApprovalRequest) ([]transport.ApprovalResult, error) {
	return f(ctx, request)
}

type spawnRecordingProvider struct {
	mu       sync.Mutex
	name     string
	response string
	messages []protocol.Message
	tools    []string
}

func (p *spawnRecordingProvider) StreamChat(_ context.Context, messages []protocol.Message, toolList []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.mu.Lock()
	p.messages = append([]protocol.Message(nil), messages...)
	p.tools = p.tools[:0]
	for _, tool := range toolList {
		p.tools = append(p.tools, tool.Name())
	}
	p.mu.Unlock()

	ch := make(chan protocol.Event, 2)
	go func() {
		defer close(ch)
		ch <- protocol.NewTextDeltaEvent(p.response)
		ch <- protocol.Event{
			Type:         protocol.EventTypeFinish,
			FinishReason: protocol.FinishReasonStop,
			Usage:        &protocol.Usage{InputTokens: 3, OutputTokens: 4},
		}
	}()
	return ch, nil
}

func (p *spawnRecordingProvider) Name() string { return p.name }

func (p *spawnRecordingProvider) snapshot() ([]protocol.Message, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.Message(nil), p.messages...), append([]string(nil), p.tools...)
}

type spawnTraceProvider struct {
	mu       sync.Mutex
	name     string
	toolName string
	calls    int
}

func (p *spawnTraceProvider) StreamChat(_ context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()

	ch := make(chan protocol.Event, 3)
	go func() {
		defer close(ch)
		if call == 1 {
			toolName := p.toolName
			if toolName == "" {
				toolName = "read_file"
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_1",
					Name:      toolName,
					Arguments: `{"path":"foo.go"}`,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
			return
		}
		ch <- protocol.NewTextDeltaEvent("trace report")
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

func (p *spawnTraceProvider) Name() string { return p.name }

type namedTool struct {
	name string
}

type approvalSpawnTool struct {
	calls int
}

func (t *approvalSpawnTool) Name() string        { return "approval_tool" }
func (t *approvalSpawnTool) Description() string { return "approval tool" }
func (t *approvalSpawnTool) Schema() any         { return map[string]any{"type": "object"} }
func (t *approvalSpawnTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelLow, RequiresApproval: true}
}
func (t *approvalSpawnTool) Execute(context.Context, executor.Executor, string) (any, error) {
	t.calls++
	return "executed", nil
}

type lowRiskSpawnTool struct {
	calls int
}

func (t *lowRiskSpawnTool) Name() string        { return "low_risk_tool" }
func (t *lowRiskSpawnTool) Description() string { return "low risk tool" }
func (t *lowRiskSpawnTool) Schema() any         { return map[string]any{"type": "object"} }
func (t *lowRiskSpawnTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelLow}
}
func (t *lowRiskSpawnTool) Execute(context.Context, executor.Executor, string) (any, error) {
	t.calls++
	return "executed", nil
}

func (t namedTool) Name() string        { return t.name }
func (t namedTool) Description() string { return t.name }
func (t namedTool) Schema() any         { return map[string]any{"type": "object"} }
func (t namedTool) Execute(context.Context, executor.Executor, string) (any, error) {
	return nil, nil
}
func (t namedTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe}
}

func TestSpawnToolRunsChildAgent(t *testing.T) {
	recorder := registerSpawnTestProvider(t, "child report")
	exec := newSpawnTestExecutor(t)
	spawn := NewSpawnTool(config.ProfileConfig{Provider: recorder.name, Model: "mock"}, []tools.Tool{
		namedTool{name: "read_file"},
		namedTool{name: "write_file"},
	})

	result, err := spawn.Execute(context.Background(), exec, `{"name":"inspect-code","description":"inspect code","prompt":"Inspect foo.go and report findings"}`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	output := spawnTestOutputText(t, result)
	if !strings.Contains(output, "Subagent completed") || !strings.Contains(output, "child report") {
		t.Fatalf("unexpected spawn output:\n%s", output)
	}
	if !strings.Contains(output, "Name: inspect-code") || !strings.Contains(output, "Workdir: "+exec.WorkDir()) {
		t.Fatalf("spawn output should include name and workdir:\n%s", output)
	}
	metadata := result.(interface{ ToolResultMetadata() map[string]any }).ToolResultMetadata()
	if _, ok := metadata["usage"].(protocol.Usage); !ok {
		t.Fatalf("spawn output should expose usage metadata, got %#v", metadata)
	}

	messages, toolNames := recorder.snapshot()
	if !containsTool(toolNames, "write_file") {
		t.Fatalf("general-purpose child should receive write_file, got %v", toolNames)
	}
	if len(messages) == 0 || !strings.Contains(messageText(messages[len(messages)-1]), "Inspect foo.go") {
		t.Fatalf("child prompt was not passed through, messages=%v", messages)
	}
}

func TestSpawnToolExploreFiltersTools(t *testing.T) {
	recorder := registerSpawnTestProvider(t, "explore report")
	exec := newSpawnTestExecutor(t)
	spawn := NewSpawnTool(config.ProfileConfig{Provider: recorder.name, Model: "mock"}, []tools.Tool{
		namedTool{name: "read_file"},
		namedTool{name: "grep"},
		namedTool{name: "glob"},
		namedTool{name: "web_search"},
		namedTool{name: "write_file"},
		namedTool{name: "edit"},
		namedTool{name: "shell"},
		namedTool{name: "spawn"},
	})

	_, err := spawn.Execute(context.Background(), exec, `{"description":"map code","prompt":"Find the relevant files","agent_type":"explore"}`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	_, toolNames := recorder.snapshot()
	for _, name := range []string{"read_file", "grep", "glob", "web_search"} {
		if !containsTool(toolNames, name) {
			t.Fatalf("explore child should receive %s, got %v", name, toolNames)
		}
	}
	for _, name := range []string{"write_file", "edit", "shell", "spawn"} {
		if containsTool(toolNames, name) {
			t.Fatalf("explore child should not receive %s, got %v", name, toolNames)
		}
	}
}

func TestSpawnToolClassify(t *testing.T) {
	spawn := NewSpawnTool(config.ProfileConfig{}, nil)
	parentDir := t.TempDir()
	outsideDir := t.TempDir()

	general := spawn.Classify(tools.SafetyCall{Arguments: `{"description":"change code","prompt":"Implement it"}`})
	if general.Level != tools.LevelLow {
		t.Fatalf("general-purpose spawn should be low risk, got %s", general.Level.String())
	}

	explore := spawn.Classify(tools.SafetyCall{Arguments: `{"description":"map code","prompt":"Find files","agent_type":"explore"}`})
	if explore.Level != tools.LevelSafe {
		t.Fatalf("explore spawn should be safe, got %s", explore.Level.String())
	}

	insideScope := spawn.Classify(tools.SafetyCall{
		Arguments: `{"description":"map code","prompt":"Find files","agent_type":"explore","workdir":"child"}`,
		WorkDir:   parentDir,
		SafeDirs:  []string{parentDir},
	})
	if insideScope.RequiresApproval || insideScope.Level != tools.LevelSafe {
		t.Fatalf("explore spawn inside safe scope should be safe, got level=%s requiresApproval=%v", insideScope.Level.String(), insideScope.RequiresApproval)
	}

	outsideScope := spawn.Classify(tools.SafetyCall{
		Arguments: `{"description":"map code","prompt":"Find files","agent_type":"explore","workdir":"` + outsideDir + `"}`,
		WorkDir:   parentDir,
		SafeDirs:  []string{parentDir},
	})
	if !outsideScope.RequiresApproval {
		t.Fatalf("spawn workdir outside safe scope should require approval, got %#v", outsideScope)
	}
	if outsideScope.Level != tools.LevelLow {
		t.Fatalf("spawn workdir outside safe scope should be low risk gated by approval, got %s", outsideScope.Level.String())
	}
}

func TestSpawnToolUsesRequestedWorkDir(t *testing.T) {
	recorder := registerSpawnTestProvider(t, "child report")
	exec := newSpawnTestExecutor(t)
	childDir := filepath.Join(exec.WorkDir(), "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("Mkdir returned error: %v", err)
	}
	spawn := NewSpawnTool(config.ProfileConfig{Provider: recorder.name, Model: "mock"}, []tools.Tool{
		namedTool{name: "read_file"},
	})

	_, err := spawn.Execute(context.Background(), exec, `{"description":"inspect child","prompt":"Inspect from child workdir","agent_type":"explore","workdir":"child"}`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	messages, _ := recorder.snapshot()
	if len(messages) == 0 || !strings.Contains(messagesText(messages), childDir) {
		t.Fatalf("child prompt did not use requested workdir %q, messages=%v", childDir, messages)
	}
}

func TestSpawnToolReturnsCompactTrace(t *testing.T) {
	name := "spawn_test_trace_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	provider := &spawnTraceProvider{name: name}
	providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
		return provider, nil
	})
	exec := newSpawnTestExecutor(t)
	spawn := NewSpawnTool(config.ProfileConfig{Provider: name, Model: "mock"}, []tools.Tool{
		namedTool{name: "read_file"},
	})

	result, err := spawn.Execute(context.Background(), exec, `{"name":"trace-agent","description":"trace child","prompt":"Read foo.go","agent_type":"explore"}`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	output := spawnTestOutputText(t, result)
	if !strings.Contains(output, "Trace:\n[trace-agent] read_file(path=foo.go)") {
		t.Fatalf("spawn output should include compact subagent trace:\n%s", output)
	}
}

func TestSpawnToolPropagatesBypassPolicy(t *testing.T) {
	tests := []struct {
		name        string
		policy      agent.ApprovalPolicy
		wantErr     string
		wantExecute int
	}{
		{name: "auto pauses", policy: agent.ApprovalPolicyAuto, wantErr: "subagent paused for approval"},
		{name: "bypass executes", policy: agent.ApprovalPolicyBypass, wantExecute: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := "spawn_policy_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
			provider := &spawnTraceProvider{name: name, toolName: "approval_tool"}
			providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
				return provider, nil
			})
			approvalTool := &approvalSpawnTool{}
			spawn := NewSpawnTool(
				config.ProfileConfig{Provider: name, Model: "mock"},
				[]tools.Tool{approvalTool},
				WithApprovalPolicySource(func() agent.ApprovalPolicy { return test.policy }),
			)

			_, err := spawn.Execute(context.Background(), newSpawnTestExecutor(t), `{"description":"run gated tool","prompt":"Use the approval tool"}`)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Execute error = %v, want containing %q", err, test.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if approvalTool.calls != test.wantExecute {
				t.Fatalf("tool executions = %d, want %d", approvalTool.calls, test.wantExecute)
			}
		})
	}
}

func TestSpawnToolReadsApprovalPolicyAtSpawnTime(t *testing.T) {
	name := "spawn_live_policy_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
		return &spawnTraceProvider{name: name, toolName: "approval_tool"}, nil
	})
	policy := agent.ApprovalPolicyAuto
	approvalTool := &approvalSpawnTool{}
	spawn := NewSpawnTool(
		config.ProfileConfig{Provider: name, Model: "mock"},
		[]tools.Tool{approvalTool},
		WithApprovalPolicySource(func() agent.ApprovalPolicy { return policy }),
	)
	exec := newSpawnTestExecutor(t)

	_, err := spawn.Execute(context.Background(), exec, `{"description":"run gated tool","prompt":"Use the approval tool"}`)
	if err == nil || !strings.Contains(err.Error(), "subagent paused for approval") {
		t.Fatalf("Auto Execute error = %v, want approval pause", err)
	}

	policy = agent.ApprovalPolicyBypass
	if _, err := spawn.Execute(context.Background(), exec, `{"description":"run gated tool","prompt":"Use the approval tool"}`); err != nil {
		t.Fatalf("Bypass Execute: %v", err)
	}
	if approvalTool.calls != 1 {
		t.Fatalf("tool executions = %d, want 1", approvalTool.calls)
	}
}

func TestSpawnToolRoutesApprovalThroughParent(t *testing.T) {
	tests := []struct {
		name        string
		approved    bool
		omitResult  bool
		wantExecute int
	}{
		{name: "approved", approved: true, wantExecute: 1},
		{name: "denied", approved: false},
		{name: "missing decision is denied", omitResult: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := "spawn_parent_approval_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
			providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
				return &spawnTraceProvider{name: name, toolName: "approval_tool"}, nil
			})
			approvalTool := &approvalSpawnTool{}
			requests := 0
			requester := spawnApprovalRequesterFunc(func(_ context.Context, request SpawnApprovalRequest) ([]transport.ApprovalResult, error) {
				requests++
				if request.AgentName != "reviewer" || len(request.Actions) != 1 {
					t.Fatalf("unexpected approval request: %+v", request)
				}
				if test.omitResult {
					return nil, nil
				}
				return []transport.ApprovalResult{{
					ToolCallID: request.Actions[0].ToolCallID,
					Approved:   test.approved,
					Reason:     "test decision",
				}}, nil
			})
			spawn := NewSpawnTool(
				config.ProfileConfig{Provider: name, Model: "mock"},
				[]tools.Tool{approvalTool},
				WithApprovalPolicySource(func() agent.ApprovalPolicy { return agent.ApprovalPolicyAuto }),
				WithSpawnApprovalRequester(requester),
			)

			if _, err := spawn.Execute(context.Background(), newSpawnTestExecutor(t), `{"name":"reviewer","description":"run gated tool","prompt":"Use the approval tool"}`); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if requests != 1 {
				t.Fatalf("approval requests = %d, want 1", requests)
			}
			if approvalTool.calls != test.wantExecute {
				t.Fatalf("tool executions = %d, want %d", approvalTool.calls, test.wantExecute)
			}
		})
	}
}

func TestSpawnToolKeepsAutoChildPolicyForManualParent(t *testing.T) {
	name := "spawn_manual_parent_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	provider := &spawnTraceProvider{name: name, toolName: "low_risk_tool"}
	providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
		return provider, nil
	})
	tool := &lowRiskSpawnTool{}
	spawn := NewSpawnTool(
		config.ProfileConfig{Provider: name, Model: "mock"},
		[]tools.Tool{tool},
		WithApprovalPolicySource(func() agent.ApprovalPolicy { return agent.ApprovalPolicyManual }),
	)

	if _, err := spawn.Execute(context.Background(), newSpawnTestExecutor(t), `{"description":"run low risk tool","prompt":"Use the low risk tool"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if tool.calls != 1 {
		t.Fatalf("tool executions = %d, want 1", tool.calls)
	}
}

func TestSpawnToolPropagatesManualPolicyWhenParentCanApprove(t *testing.T) {
	name := "spawn_manual_broker_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
		return &spawnTraceProvider{name: name, toolName: "low_risk_tool"}, nil
	})
	tool := &lowRiskSpawnTool{}
	requests := 0
	requester := spawnApprovalRequesterFunc(func(_ context.Context, request SpawnApprovalRequest) ([]transport.ApprovalResult, error) {
		requests++
		return []transport.ApprovalResult{{ToolCallID: request.Actions[0].ToolCallID, Approved: true}}, nil
	})
	spawn := NewSpawnTool(
		config.ProfileConfig{Provider: name, Model: "mock"},
		[]tools.Tool{tool},
		WithApprovalPolicySource(func() agent.ApprovalPolicy { return agent.ApprovalPolicyManual }),
		WithSpawnApprovalRequester(requester),
	)

	if _, err := spawn.Execute(context.Background(), newSpawnTestExecutor(t), `{"description":"run low risk tool","prompt":"Use the low risk tool"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if requests != 1 || tool.calls != 1 {
		t.Fatalf("approval requests = %d, tool executions = %d; want 1 each", requests, tool.calls)
	}
}

func spawnTestOutputText(t *testing.T, result any) string {
	t.Helper()
	withText, ok := result.(interface{ ToolOutputText() string })
	if !ok {
		t.Fatalf("spawn result should expose tool output text, got %T", result)
	}
	return withText.ToolOutputText()
}

func registerSpawnTestProvider(t *testing.T, response string) *spawnRecordingProvider {
	t.Helper()
	name := "spawn_test_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	recorder := &spawnRecordingProvider{name: name, response: response}
	providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) {
		return recorder, nil
	})
	return recorder
}

func newSpawnTestExecutor(t *testing.T) executor.Executor {
	t.Helper()
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalExecutor returned error: %v", err)
	}
	return exec
}

func containsTool(toolNames []string, name string) bool {
	for _, toolName := range toolNames {
		if toolName == name {
			return true
		}
	}
	return false
}

func messageText(msg protocol.Message) string {
	var parts []string
	for _, part := range msg.Content {
		if part.Type == protocol.ContentTypeText {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func messagesText(messages []protocol.Message) string {
	var parts []string
	for _, msg := range messages {
		parts = append(parts, messageText(msg))
	}
	return strings.Join(parts, "\n")
}
