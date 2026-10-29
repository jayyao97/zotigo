package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/tools"
)

type compactionProvider struct {
	calls int
	fail  bool
}

func (*compactionProvider) Name() string { return "compaction-wiring-test" }
func (p *compactionProvider) StreamChat(_ context.Context, msgs []protocol.Message, toolset []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.calls++
	if len(toolset) != 0 || !strings.Contains(msgs[0].Content[0].Text, "Conversation to summarize:") {
		return nil, errors.New("unexpected compaction request")
	}
	ch := make(chan protocol.Event, 2)
	ch <- protocol.NewTextDeltaEvent("LLM summary: preserve the agreed metric and pending work")
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
