package zotigod

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/internal/channels"
)

func TestChannelPolicyChangesOnlyAtIdleBoundary(t *testing.T) {
	h, store, _, _ := newChannelProvisionFixture(t)
	ctx := context.Background()
	session := newSession(t.TempDir(), "default")
	if err := h.persistSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	h.items.(*fakeDisplayItemSource).items[session.ID] = nil
	for _, policy := range []string{"bypass_permissions", "auto"} {
		h.registry.Add(Session{ID: session.ID, State: SessionStateRunning, Working: true})
		if _, err := h.EnsureChannelSessionPrompt(ctx, session.ID, channels.SessionPromptConfig{ApprovalPolicy: policy}); !errors.Is(err, errSessionBusy) {
			t.Fatalf("busy change: %v", err)
		}
		before, err := store.Get(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if string(before.ApprovalPolicy) == policy {
			t.Fatal("changed policy while busy")
		}
		h.registry.Add(Session{ID: session.ID, State: SessionStateRunning})
		if _, err := h.EnsureChannelSessionPrompt(ctx, session.ID, channels.SessionPromptConfig{ApprovalPolicy: policy}); err != nil {
			t.Fatal(err)
		}
		after, err := store.Get(ctx, session.ID)
		if err != nil || after.ApprovalPolicy != agent.ApprovalPolicy(policy) || after.PromptConfig.Revision != before.PromptConfig.Revision+1 {
			t.Fatalf("updated=%+v err=%v", after, err)
		}
		if _, err := h.EnsureChannelSessionPrompt(ctx, session.ID, channels.SessionPromptConfig{ApprovalPolicy: policy}); err != nil {
			t.Fatal(err)
		}
		repeated, _ := store.Get(ctx, session.ID)
		if repeated.PromptConfig.Revision != after.PromptConfig.Revision {
			t.Fatal("unchanged policy advanced revision")
		}
	}
}

func TestProvisionChannelSessionWithFullAccess(t *testing.T) {
	h, store, _, workspace := newChannelProvisionFixture(t)
	var id string
	err := h.ProvisionChannelSession(context.Background(), workspace.ID, "full access", channels.SessionPromptConfig{ApprovalPolicy: "bypass_permissions"}, channels.SessionRuntimeConfig{Agent: channels.SessionAgentZotigo}, func(sessionID string, _ channels.SessionRuntimeConfig) error { id = sessionID; return nil })
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), id)
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}
