package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/tools"
)

type compactionProvider struct {
	calls      int
	fail       bool
	requests   [][]protocol.Message
	toolLists  [][]tools.Tool
	rejectFull bool
	toolCall   bool
}

func (*compactionProvider) Name() string { return "compaction-wiring-test" }
func (p *compactionProvider) StreamChat(_ context.Context, msgs []protocol.Message, toolset []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.calls++
	p.requests = append(p.requests, msgs)
	p.toolLists = append(p.toolLists, toolset)
	if p.rejectFull && len(msgs) > 1 {
		return nil, providers.ErrContextLengthExceeded
	}
	ch := make(chan protocol.Event, 3)
	ch <- protocol.NewTextDeltaEvent("LLM summary: preserve the agreed metric and pending work")
	if p.toolCall {
		ch <- protocol.Event{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{Name: "view_image"}}
	}
	if p.fail {
		ch <- protocol.NewErrorEvent(errors.New("summary unavailable"))
	} else {
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}
	close(ch)
	return ch, nil
}
func TestAgentDefaultCompactionUsesLLMAndTracksProfile(t *testing.T) {
	first := &compactionProvider{}
	providers.Register("compaction-wiring-test", func(config.ProfileConfig) (providers.Provider, error) { return first, nil })
	cfg := config.ProfileConfig{Provider: "compaction-wiring-test", Model: "first", ContextWindow: 1000}
	ag, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := []protocol.Message{protocol.NewUserMessage("old task"), protocol.NewAssistantMessage(strings.Repeat("important findings ", 2000)), protocol.NewUserMessage("latest request")}
	for _, manual := range []bool{false, true} {
		ag.conversation.history = append([]protocol.Message(nil), seed...)
		before := first.calls
		if manual {
			r, err := ag.ForceCompress(context.Background())
			if err != nil || !r.Compressed {
				t.Fatalf("manual: %+v %v", r, err)
			}
		} else {
			_, _, r, err := ag.buildContext(context.Background())
			if err != nil || r == nil {
				t.Fatalf("auto: %+v %v", r, err)
			}
		}
		if first.calls != before+1 || !strings.Contains(ag.conversation.history[0].Content[0].Text, "LLM summary:") {
			t.Fatal("LLM summary not used")
		}
	}
	second := &compactionProvider{}
	ag.applyRuntimeProfileLocked(RuntimeProfile{Name: "second", Config: cfg, Provider: second})
	ag.conversation.history = append([]protocol.Message(nil), seed...)
	if _, err := ag.ForceCompress(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.calls != 1 || first.calls != 2 {
		t.Fatalf("profile switch used wrong model: %d/%d", first.calls, second.calls)
	}
	second.fail = true
	for _, manual := range []bool{false, true} {
		ag.conversation.history = append([]protocol.Message(nil), seed...)
		before := services.ContextDigest(ag.conversation.history)
		if manual {
			_, err = ag.ForceCompress(context.Background())
		} else {
			_, _, _, err = ag.buildContext(context.Background())
		}
		if err == nil || services.ContextDigest(ag.conversation.history) != before {
			t.Fatalf("failed summary changed history or succeeded: %v", err)
		}
	}
}

func TestCompactionPreservesMainPromptPrefix(t *testing.T) {
	for _, mode := range []string{"automatic", "manual", "reactive", "summary-overflow", "tool-call"} {
		t.Run(mode, func(t *testing.T) {
			p := &compactionProvider{rejectFull: mode == "summary-overflow", toolCall: mode == "tool-call"}
			a := calibratedTestAgent()
			local, err := executor.NewLocalExecutor(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a.executor = local
			a.provider = p
			a.promptBuilder = prompt.NewSystemPromptBuilder(prompt.WithStaticPrompt("stable system"))
			a.tools = map[string]tools.Tool{"view_image": &imageResultTestTool{}}
			a.configureCompressorSummarizer()
			a.conversation.history[1].Content = append(a.conversation.history[1].Content,
				protocol.ContentPart{Type: protocol.ContentTypeReasoning, Text: "reasoning evidence"},
				protocol.ContentPart{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("image"), MediaType: "image/png"}})
			a.conversation.history[1].Content = append(a.conversation.history[1].Content,
				protocol.ContentPart{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{ID: "image-call", Name: "view_image", Arguments: `{}`}})
			a.conversation.history = append(a.conversation.history[:2:2], append([]protocol.Message{
				protocol.NewToolMessage([]protocol.ToolResult{{ToolCallID: "image-call", ToolName: "view_image", Text: strings.Repeat("large result ", 4000)}}),
			}, a.conversation.history[2:]...)...)
			// Capture the actual main prompt below the threshold, then trigger
			// compaction without changing any of the model-visible messages.
			a.compressor.SetContextWindow(10000000)
			prefix, _, _, err := a.buildContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a.compressor.SetContextWindow(1000)
			before := services.ContextDigest(a.conversation.history)
			switch mode {
			case "automatic":
				_, _, _, err = a.buildContext(context.Background())
			case "reactive":
				retries := 0
				if !a.tryReactiveRecover(context.Background(), providers.ErrContextLengthExceeded, &retries, 1, make(chan protocol.Event, 2)) {
					t.Fatal("reactive recovery failed")
				}
			default:
				_, err = a.ForceCompress(context.Background())
			}
			if mode == "tool-call" {
				if err == nil || services.ContextDigest(a.conversation.history) != before {
					t.Fatal("tool call must reject summary and preserve history")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if mode == "summary-overflow" {
				wantCalls = 2
			}
			if p.calls != wantCalls {
				t.Fatalf("calls = %d, want %d", p.calls, wantCalls)
			}
			if mode != "reactive" {
				got := p.requests[0]
				if len(got) != len(prefix)+1 {
					t.Fatal("compaction changed main request prefix")
				}
				for i := range prefix {
					// System message creation timestamps are local bookkeeping;
					// providers consume the role and content, not CreatedAt.
					if got[i].Role != prefix[i].Role || !reflect.DeepEqual(got[i].Content, prefix[i].Content) {
						t.Fatalf("compaction changed main request message %d", i)
					}
				}
				if !reflect.DeepEqual(p.toolLists[0], sortedToolList(a.tools)) {
					t.Fatal("compaction changed tools")
				}
				if !strings.Contains(got[len(got)-1].Content[0].Text, "Do not call any tools") {
					t.Fatal("missing text-only instruction")
				}
			}
			if mode == "reactive" || mode == "summary-overflow" {
				last := len(p.requests) - 1
				if len(p.requests[last]) != 1 || len(p.toolLists[last]) != 0 || !strings.Contains(p.requests[last][0].Content[0].Text, "Conversation to summarize:") {
					t.Fatal("fallback did not use flattened history")
				}
			}
		})
	}
}

type blockedCompactionProvider struct{ started chan struct{} }

func (*blockedCompactionProvider) Name() string { return "blocked-compact" }
func (p *blockedCompactionProvider) StreamChat(ctx context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestCompactionAdmissionAndCancellation(t *testing.T) {
	for _, mode := range []string{"automatic", "manual", "reactive"} {
		t.Run(mode, func(t *testing.T) {
			a := calibratedTestAgent()
			a.state = StateRunning
			p := &blockedCompactionProvider{started: make(chan struct{})}
			a.provider = p
			a.compressor.SetSummarizer(services.NewProviderSummarizer(p))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch mode {
				case "automatic":
					a.mu.Lock()
					_, _, _, _ = a.buildContext(ctx)
					a.mu.Unlock()
				case "manual":
					_, _ = a.ForceCompress(ctx)
				case "reactive":
					retries := 0
					a.tryReactiveRecover(ctx, providers.ErrContextLengthExceeded, &retries, 1, make(chan protocol.Event, 1))
				}
			}()
			select {
			case <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("summary never started")
			}
			admitted := false
			inputDone := make(chan error, 1)
			go func() {
				inputDone <- a.QueueTurnUserMessageWithAdmission(protocol.NewUserMessage("steer"), func() error { admitted = true; return nil })
			}()
			select {
			case err := <-inputDone:
				if !errors.Is(err, ErrCompacting) || admitted {
					t.Fatalf("admission=%v err=%v", admitted, err)
				}
			case <-time.After(time.Second):
				t.Fatal("input blocked behind summary")
			}
			policyDone := make(chan error, 1)
			go func() { _, err := a.RequestedApprovalPolicyForInput(); policyDone <- err }()
			select {
			case err := <-policyDone:
				if !errors.Is(err, ErrCompacting) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("policy check blocked behind summary")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("summary did not stop")
			}
			if a.compacting.Load() {
				t.Fatal("compacting flag not cleared")
			}
		})
	}
}
