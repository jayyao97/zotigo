package zotigod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
)

type workerPromptCaptureProvider struct {
	mu       sync.Mutex
	messages [][]protocol.Message
	step     int
	classify bool
}

func (p *workerPromptCaptureProvider) Name() string { return "worker-prompt-capture" }

func (p *workerPromptCaptureProvider) StreamChat(_ context.Context, messages []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.mu.Lock()
	p.messages = append(p.messages, messages)
	p.step++
	step := p.step
	p.mu.Unlock()

	events := make(chan protocol.Event, 4)
	if p.classify {
		events <- protocol.NewTextDeltaEvent(`{"decision":"deny","reason":"test policy","requires_snapshot":false}`)
		events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	} else if step == 1 {
		events <- protocol.Event{Type: protocol.EventTypeToolCallDelta, Index: 0, ToolCallDelta: &protocol.ToolCallDelta{ID: "call-1", Name: "read_file"}}
		events <- protocol.Event{Type: protocol.EventTypeToolCallEnd, Index: 0, ToolCall: &protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"note.txt"}`}}
		events <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
	} else {
		events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}
	close(events)
	return events, nil
}

func (p *workerPromptCaptureProvider) joinedPrompts() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var text strings.Builder
	for _, messages := range p.messages {
		for _, message := range messages {
			for _, part := range message.Content {
				text.WriteString(part.Text)
				text.WriteByte('\n')
			}
		}
	}
	return text.String()
}

func TestNativeWorkerReadsSessionPromptConfig(t *testing.T) {
	const mainProviderName = "worker-session-prompt-main"
	const classifierProviderName = "worker-session-prompt-classifier"
	mainProvider := &workerPromptCaptureProvider{}
	classifierProvider := &workerPromptCaptureProvider{classify: true}
	providers.Register(mainProviderName, func(config.ProfileConfig) (providers.Provider, error) { return mainProvider, nil })
	providers.Register(classifierProviderName, func(config.ProfileConfig) (providers.Provider, error) { return classifierProvider, nil })

	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "note.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	projectConfig := fmt.Sprintf(`default_profile: main
profiles:
  main:
    provider: %s
    model: main
    safety:
      classifier:
        enabled: true
        profile: reviewer
  reviewer:
    provider: %s
    model: reviewer
`, mainProviderName, classifierProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-prompt-config", WorkingDirectory: workDir, ProfileName: "main", CreatedAt: now, UpdatedAt: now,
		PromptConfig: zotigosession.PromptConfig{
			AgentInstructions:    "Reply using the channel style.",
			ApprovalInstructions: "Deny access outside the bound workspace.",
			ReviewAllTools:       true,
			Revision:             3,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{SessionID: "session-prompt-config", Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.acceptInput(context.Background(), workerInputRequest{RequiredApprovalPolicy: agent.ApprovalPolicyAuto, RequiredPromptRevision: 2, RequirePromptRevision: true}); !errors.Is(err, errInputPolicyMismatch) {
		t.Fatalf("prompt revision mismatch was admitted: %v", err)
	}
	runtime.agent.SetApprovalPolicy(agent.ApprovalPolicyBypass)
	if _, err := runtime.acceptInput(context.Background(), workerInputRequest{RequiredApprovalPolicy: agent.ApprovalPolicyAuto, RequiredPromptRevision: 3, RequirePromptRevision: true}); !errors.Is(err, errInputPolicyMismatch) {
		t.Fatalf("approval policy mismatch was admitted: %v", err)
	}
	runtime.agent.SetApprovalPolicy(agent.ApprovalPolicyAuto)
	if err := runtime.startMessageTurn(context.Background(), "command-1", 1, &messageCommandPayload{Text: "read the note"}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	done := runtime.turnDone
	runtime.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("native worker turn did not finish")
	}
	if !strings.Contains(mainProvider.joinedPrompts(), "Reply using the channel style.") {
		t.Fatal("native worker did not append session agent instructions")
	}
	classifierPrompt := classifierProvider.joinedPrompts()
	if !strings.Contains(classifierPrompt, "You are a strict safety reviewer") ||
		!strings.Contains(classifierPrompt, "Deny access outside the bound workspace.") {
		t.Fatalf("native worker classifier prompt did not combine base and session instructions: %q", classifierPrompt)
	}
}
