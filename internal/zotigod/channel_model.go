package zotigod

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

// ChannelSessionRuntime reports a Session's current runtime for /model.
func (h *handler) ChannelSessionRuntime(ctx context.Context, sessionID string) (channels.SessionRuntimeConfig, error) {
	session, err := h.channelSession(ctx, sessionID)
	if err != nil {
		return channels.SessionRuntimeConfig{}, err
	}
	return channels.SessionRuntimeConfig{Agent: session.Agent, ProfileName: session.ProfileName, Model: session.Model, ReasoningEffort: session.ReasoningEffort}, nil
}

// ChannelRuntimeChoices lists the profiles of a Workspace, or the Codex models
// with their reasoning efforts.
func (h *handler) ChannelRuntimeChoices(ctx context.Context, workspaceID, agent string) ([]string, error) {
	if zotigoruntime.AgentKind(agent) == zotigoruntime.AgentCodex {
		capabilities, err := h.codexCapabilities(ctx)
		if err != nil {
			return nil, channels.NewChatError(err, "codex is unavailable")
		}
		choices := make([]string, 0, len(capabilities.Models))
		for _, model := range capabilities.Models {
			choices = append(choices, fmt.Sprintf("%s (%s)", model.ID, strings.Join(model.SupportedReasoningEfforts, ", ")))
		}
		return choices, nil
	}
	root, err := h.channelWorkspaceRoot(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	appConfig, err := config.NewManager().LoadForDir(root)
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	choices := make([]string, 0, len(appConfig.Profiles))
	for name, profile := range appConfig.Profiles {
		choices = append(choices, fmt.Sprintf("%s (%s)", name, profile.Model))
	}
	sort.Strings(choices)
	return choices, nil
}

// ResolveChannelRuntime turns /model arguments into a runtime for the
// Sessions a group creates later, keeping the group's agent.
func (h *handler) ResolveChannelRuntime(ctx context.Context, workspaceID string, current channels.SessionRuntimeConfig, choice []string) (channels.SessionRuntimeConfig, error) {
	if zotigoruntime.AgentKind(current.Agent) == zotigoruntime.AgentCodex {
		model, effort, err := h.resolveCodexChoice(ctx, choice, current.ReasoningEffort)
		if err != nil {
			return current, err
		}
		return channels.SessionRuntimeConfig{Agent: current.Agent, Model: model, ReasoningEffort: effort}, nil
	}
	if len(choice) != 1 {
		return current, channels.NewChatError(nil, "use /model <profile>")
	}
	root, err := h.channelWorkspaceRoot(ctx, workspaceID)
	if err != nil {
		return current, err
	}
	profile, found, err := resolveProfileForDirectory(root, choice[0])
	if err != nil {
		return current, fmt.Errorf("load profiles: %w", err)
	}
	if !found {
		return current, channels.NewChatError(nil, "profile %q not found", choice[0])
	}
	return channels.SessionRuntimeConfig{Agent: current.Agent, ProfileName: profile}, nil
}

// SwitchChannelSessionRuntime changes a bound Session's model through the same
// path as the HTTP API. The Session must be idle so a running turn keeps its
// model. hold keeps the Channel binding stable from before the Session lock
// until the change is recorded, in place of the HTTP path's guard.
func (h *handler) SwitchChannelSessionRuntime(ctx context.Context, sessionID string, choice []string, hold func() (func(), error)) (channels.SessionRuntimeConfig, bool, error) {
	session, err := h.channelSession(ctx, sessionID)
	if err != nil {
		return channels.SessionRuntimeConfig{}, false, err
	}
	runtime := channels.SessionRuntimeConfig{Agent: session.Agent}
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		model, effort, err := h.resolveCodexChoice(ctx, choice, session.ReasoningEffort)
		if err != nil {
			return runtime, false, err
		}
		release, err := hold()
		if err != nil {
			return runtime, false, err
		}
		defer release()
		updated, err := h.changeSessionCodexSettings(ctx, sessionID, model, effort, true)
		if err != nil {
			return runtime, false, chatRuntimeError(err)
		}
		runtime.Model, runtime.ReasoningEffort = updated.Model, updated.ReasoningEffort
		return runtime, false, nil
	}
	if len(choice) != 1 {
		return runtime, false, channels.NewChatError(nil, "use /model <profile>")
	}
	response, err := h.changeSessionProfileGuarded(ctx, sessionID, choice[0], func() (bool, func(), error) {
		release, err := hold()
		return true, release, err
	})
	if err != nil {
		return runtime, false, chatRuntimeError(err)
	}
	runtime.ProfileName = response.Profile
	return runtime, response.Status == "pending", nil
}

// resolveCodexChoice reads "<model> [effort]". Without an effort it keeps the
// current one when the model supports it, else prefers medium.
func (h *handler) resolveCodexChoice(ctx context.Context, choice []string, currentEffort string) (string, string, error) {
	if len(choice) == 0 || len(choice) > 2 {
		return "", "", channels.NewChatError(nil, "use /model <model> [reasoning effort]")
	}
	capabilities, err := h.codexCapabilities(ctx)
	if err != nil {
		return "", "", channels.NewChatError(err, "codex is unavailable")
	}
	for _, model := range capabilities.Models {
		if !strings.EqualFold(model.ID, choice[0]) {
			continue
		}
		if len(model.SupportedReasoningEfforts) == 0 {
			return "", "", channels.NewChatError(nil, "model %q reports no reasoning efforts", model.ID)
		}
		efforts := model.SupportedReasoningEfforts
		switch {
		case len(choice) == 2:
			for _, effort := range efforts {
				if strings.EqualFold(effort, choice[1]) {
					return model.ID, effort, nil
				}
			}
			return "", "", channels.NewChatError(nil, "model %q supports reasoning effort %s", model.ID, strings.Join(efforts, ", "))
		case slices.Contains(efforts, currentEffort):
			return model.ID, currentEffort, nil
		case slices.Contains(efforts, "medium"):
			return model.ID, "medium", nil
		default:
			return model.ID, efforts[0], nil
		}
	}
	return "", "", channels.NewChatError(nil, "unknown codex model %q", choice[0])
}

func (h *handler) channelSession(ctx context.Context, sessionID string) (Session, error) {
	if session, ok := h.registry.Get(sessionID); ok {
		return session, nil
	}
	session, found, err := h.storedSession(ctx, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	if !found {
		return Session{}, channels.NewChatError(nil, "session %s was not found", sessionID)
	}
	return session, nil
}

func (h *handler) channelWorkspaceRoot(ctx context.Context, workspaceID string) (string, error) {
	if h.catalog == nil {
		return "", errors.New("workspace catalog is unavailable")
	}
	workspace, err := h.catalog.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("load workspace: %w", err)
	}
	return workspace.RootPath, nil
}

// chatRuntimeError lets the chat show why a model change was refused when the
// failure is marked public. Others stay private, unless they wrap a ChatError.
func chatRuntimeError(err error) error {
	var runtimeErr *sessionRuntimeError
	if errors.As(err, &runtimeErr) && runtimeErr.public {
		return channels.NewChatError(nil, "%s", runtimeErr.message)
	}
	return err
}

var (
	_ channels.ChannelBinder        = (*handler)(nil)
	_ channels.ChannelModelSwitcher = (*handler)(nil)
)
