package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type runtimeProfileTestProvider struct{ name string }

func (p runtimeProfileTestProvider) Name() string { return p.name }

func (p runtimeProfileTestProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event)
	close(events)
	return events, nil
}

func TestQueueRuntimeProfileSupersedesPendingBeforeImmediateApply(t *testing.T) {
	oldResult := make(chan error, 1)
	ag := &Agent{
		tools: make(map[string]tools.Tool),
		pendingProfile: &runtimeProfileRequest{
			profile: RuntimeProfile{Name: "old-pending", Provider: runtimeProfileTestProvider{name: "old"}},
			result:  oldResult,
		},
	}
	newResult := ag.QueueRuntimeProfile(RuntimeProfile{
		Name:     "new-profile",
		Config:   config.ProfileConfig{Provider: "new"},
		Provider: runtimeProfileTestProvider{name: "new"},
	})
	if err := <-oldResult; !errors.Is(err, ErrRuntimeProfileSuperseded) {
		t.Fatalf("expected old pending profile to be superseded, got %v", err)
	}
	if err := <-newResult; err != nil {
		t.Fatalf("apply new profile: %v", err)
	}
	if got := ag.ActiveProfileName(); got != "new-profile" {
		t.Fatalf("expected new profile, got %q", got)
	}
}
