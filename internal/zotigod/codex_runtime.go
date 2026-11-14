package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type codexWorkerRuntime struct {
	cfg                  codexWorkerConfig
	store                zotigosession.Store
	writer               *workerClientWriter
	app                  codexapp.RPC
	threadID             string
	activeTurnID         string
	commandSequence      uint64
	messagePending       bool
	turnStarted          time.Time
	turnModel            string
	turnUsage            protocol.Usage
	totalUsage           protocol.Usage
	hasTotalUsage        bool
	messages             map[string]string
	messageOrder         []string
	displayItems         map[string]codexActiveDisplayItem
	toolNames            map[string]string
	toolArguments        map[string]string
	toolNativeNames      map[string]string
	toolOrder            []string
	hooks                hookEventDispatcher
	skills               *skills.SkillManager
	skillTempDir         string
	skillPaths           map[string]string
	interactions         map[string]codexPendingInteraction
	approvals            map[string]codexPendingApproval
	runtimeToolResults   <-chan wire.RuntimeToolResult
	activeRequestContext *protocol.RequestContext
}

type codexActiveDisplayItem struct {
	partType      string
	beforeInputID string
}

func (r *codexWorkerRuntime) trackDisplayItem(id, partType string) {
	if r.displayItems == nil {
		r.displayItems = make(map[string]codexActiveDisplayItem)
	}
	if _, exists := r.displayItems[id]; !exists {
		r.displayItems[id] = codexActiveDisplayItem{partType: partType}
	}
}

type codexPendingInteraction struct {
	request wire.InteractionRequest
	rpcID   json.RawMessage
}

type codexPendingApproval struct {
	request     approvalRequest
	rpcID       json.RawMessage
	kind        codexApprovalKind
	permissions json.RawMessage
	approve     string
	deny        string
}

type codexApprovalKind string

const (
	codexApprovalDecision    codexApprovalKind = "decision"
	codexApprovalPermissions codexApprovalKind = "permissions"
)

type codexApprovalRequestParams struct {
	ThreadID               string            `json:"threadId"`
	TurnID                 string            `json:"turnId"`
	ItemID                 string            `json:"itemId"`
	ApprovalID             string            `json:"approvalId"`
	Kind                   string            `json:"kind"`
	EnvironmentID          string            `json:"environmentId"`
	Reason                 string            `json:"reason"`
	Command                string            `json:"command"`
	CWD                    string            `json:"cwd"`
	GrantRoot              string            `json:"grantRoot"`
	NetworkApprovalContext json.RawMessage   `json:"networkApprovalContext"`
	AdditionalPermissions  json.RawMessage   `json:"additionalPermissions"`
	AvailableDecisions     []json.RawMessage `json:"availableDecisions"`
	Permissions            json.RawMessage   `json:"permissions"`
}

type codexThreadItem struct {
	ID               string               `json:"id"`
	ClientID         *string              `json:"clientId,omitempty"`
	Type             string               `json:"type"`
	Text             string               `json:"text,omitempty"`
	Summary          []string             `json:"summary,omitempty"`
	Content          any                  `json:"content,omitempty"`
	Command          string               `json:"command,omitempty"`
	CWD              string               `json:"cwd,omitempty"`
	CommandActions   []codexCommandAction `json:"commandActions,omitempty"`
	AggregatedOutput *string              `json:"aggregatedOutput,omitempty"`
	ExitCode         *int                 `json:"exitCode,omitempty"`
	DurationMS       *int64               `json:"durationMs,omitempty"`
	Changes          any                  `json:"changes,omitempty"`
	Status           string               `json:"status,omitempty"`
	Server           string               `json:"server,omitempty"`
	Namespace        *string              `json:"namespace,omitempty"`
	Tool             string               `json:"tool,omitempty"`
	Arguments        any                  `json:"arguments,omitempty"`
	Result           any                  `json:"result,omitempty"`
	RevisedPrompt    *string              `json:"revisedPrompt,omitempty"`
	SavedPath        *string              `json:"savedPath,omitempty"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	ContentItems      any                        `json:"contentItems,omitempty"`
	Success           *bool                      `json:"success,omitempty"`
	Prompt            *string                    `json:"prompt,omitempty"`
	Model             *string                    `json:"model,omitempty"`
	ReasoningEffort   *string                    `json:"reasoningEffort,omitempty"`
	ReceiverThreadIDs []string                   `json:"receiverThreadIds,omitempty"`
	AgentsStates      map[string]codexAgentState `json:"agentsStates,omitempty"`
}

type codexAgentState struct {
	Status  string  `json:"status"`
	Message *string `json:"message,omitempty"`
}

type codexCommandAction struct {
	Type    string  `json:"type"`
	Command string  `json:"command,omitempty"`
	Name    string  `json:"name,omitempty"`
	Path    *string `json:"path,omitempty"`
	Query   *string `json:"query,omitempty"`
}

type codexItemNotification struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Item     codexThreadItem `json:"item"`
}

func (r *codexWorkerRuntime) Close() error {
	if r == nil || r.activeTurnID == "" {
		return nil
	}
	turnID := r.activeTurnID
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var response any
	interruptErr := r.app.Call(closeCtx, "turn/interrupt", map[string]any{
		"threadId": r.threadID, "turnId": turnID,
	}, &response)
	for _, itemID := range r.messageOrder {
		if err := r.append(zotigosession.DisplayItem{
			ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: r.messages[itemID]}},
		}); err != nil {
			return errors.Join(interruptErr, err)
		}
	}
	if err := r.finishPendingTools("codex worker disconnected"); err != nil {
		return errors.Join(interruptErr, err)
	}
	duration := int64(0)
	if !r.turnStarted.IsZero() {
		duration = time.Since(r.turnStarted).Milliseconds()
	}
	appendErr := r.append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted, Error: "codex worker disconnected",
		Turn: &zotigosession.DisplayTurn{ID: turnID, Status: "interrupted", DurationMS: duration},
	})
	if appendErr == nil {
		dispatchTurnEndHook(r.hooks, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory, turnID, "interrupted", r.turnModel, r.turnUsage)
	}
	r.activeTurnID = ""
	r.activeRequestContext = nil
	return errors.Join(interruptErr, appendErr)
}

func (r *codexWorkerRuntime) append(item zotigosession.DisplayItem) error {
	if item.Type == zotigosession.DisplayItemAssistantMessage {
		item.BeforeInputID = r.displayItems[item.ID].beforeInputID
		for _, part := range item.Content {
			if part.ToolResult != nil {
				item.BeforeInputID = r.displayItems[part.ToolResult.ToolCallID].beforeInputID
				break
			}
		}
	}
	stored, err := r.store.AppendDisplayItem(context.Background(), r.cfg.SessionID, item)
	if err != nil {
		return err
	}
	if r.writer != nil {
		r.writer.SendDisplayItem(stored)
	}
	return nil
}
