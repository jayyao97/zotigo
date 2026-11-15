package agent_test

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type reasoningThenTextProvider struct{}

func (reasoningThenTextProvider) Name() string { return "reasoning-origin" }

func (reasoningThenTextProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 4)
	ch <- protocol.NewReasoningDeltaEvent("thinking")
	ch <- protocol.Event{Type: protocol.EventTypeContentEnd, ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeReasoning, Text: "thinking", Signature: "sig"}}
	ch <- protocol.NewTextDeltaEvent("done")
	ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	close(ch)
	return ch, nil
}

// Reasoning is stored with the service that produced it, so a later request to
// a different service can leave it out.
func TestRecordedReasoningCarriesItsOrigin(t *testing.T) {
	providers.Register("reasoning-origin", func(config.ProfileConfig) (providers.Provider, error) {
		return reasoningThenTextProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	cfg := config.ProfileConfig{Provider: "reasoning-origin"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	var reasoning []protocol.ContentPart
	for _, message := range ag.Snapshot().History {
		for _, part := range message.Content {
			if part.Type == protocol.ContentTypeReasoning {
				reasoning = append(reasoning, part)
			}
		}
	}
	if len(reasoning) != 1 {
		t.Fatalf("reasoning parts in history = %d, want 1", len(reasoning))
	}
	if want := providers.ReasoningOrigin(cfg); reasoning[0].Origin != want || reasoning[0].Signature != "sig" {
		t.Fatalf("reasoning part = %+v, want origin %q with its signature", reasoning[0], want)
	}
}
