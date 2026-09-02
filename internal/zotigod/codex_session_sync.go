package zotigod

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

const (
	codexSessionSyncInterval = 10 * time.Second
	codexSessionSyncVersion  = 2
)

type codexSessionSyncer struct {
	host       codexapp.HostProvider
	store      zotigosession.Store
	items      displayItemSource
	catalog    *zotigoworkspace.Store
	registry   *sessionRegistry
	sessionOps *sessionOperationLocks
	mu         sync.Mutex
	lastCheck  time.Time
}

type codexThreadSnapshot struct {
	Thread codexThread `json:"thread"`
}

type codexThread struct {
	ID        string      `json:"id"`
	UpdatedAt int64       `json:"updatedAt"`
	Turns     []codexTurn `json:"turns"`
}

type codexTurnList struct {
	Data       []codexTurn `json:"data"`
	NextCursor *string     `json:"nextCursor"`
}

type codexThreadItemEntry struct {
	TurnID string          `json:"turnId"`
	Item   codexThreadItem `json:"item"`
}

type codexThreadItemList struct {
	Data       []codexThreadItemEntry `json:"data"`
	NextCursor *string                `json:"nextCursor"`
}

type codexThreadList struct {
	Data       []codexThread `json:"data"`
	NextCursor *string       `json:"nextCursor"`
}

type codexTurn struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMS  *int64            `json:"durationMs"`
	Error       *codexTurnError   `json:"error"`
	Items       []codexThreadItem `json:"items"`
}

type codexTurnError struct {
	Message string `json:"message"`
}

type codexUserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

type backendActivityUpdater interface {
	UpdateBackendActivity(context.Context, string, time.Time, int) error
}

func newCodexSessionSyncer(host codexapp.HostProvider, store zotigosession.Store, items displayItemSource, catalog *zotigoworkspace.Store, registry *sessionRegistry, sessionOps *sessionOperationLocks) *codexSessionSyncer {
	return &codexSessionSyncer{host: host, store: store, items: items, catalog: catalog, registry: registry, sessionOps: sessionOps}
}

func (s *codexSessionSyncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastCheck) < codexSessionSyncInterval {
		return nil
	}
	metadata, err := s.store.List(ctx, zotigosession.ListFilter{OrderBy: zotigosession.OrderByUpdatedDesc})
	if err != nil {
		return fmt.Errorf("list sessions for Codex sync: %w", err)
	}
	candidates := make([]zotigosession.Metadata, 0)
	for _, meta := range metadata {
		if meta.Agent != "codex" || meta.ConversationID == "" {
			continue
		}
		candidates = append(candidates, meta)
	}
	if len(candidates) == 0 {
		s.lastCheck = time.Now()
		return nil
	}

	lease, err := s.host.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire Codex metadata connection: %w", err)
	}
	defer func() { _ = lease.Release() }()
	activity, err := listCodexThreadActivity(ctx, lease.RPC, candidates)
	if err != nil {
		return err
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return activity[candidates[i].ConversationID].Before(activity[candidates[j].ConversationID])
	})
	var syncErrors []error
	for _, meta := range candidates {
		backendUpdatedAt, ok := activity[meta.ConversationID]
		if !ok || (!backendUpdatedAt.After(meta.BackendUpdatedAt) && meta.BackendSyncVersion >= codexSessionSyncVersion) {
			continue
		}
		if err := s.syncCandidate(ctx, lease.RPC, meta); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("sync session %s: %w", meta.ID, err))
		}
	}
	result := errors.Join(syncErrors...)
	if result == nil {
		s.lastCheck = time.Now()
	}
	return result
}

func (s *codexSessionSyncer) syncCandidate(ctx context.Context, rpc codexapp.RPC, meta zotigosession.Metadata) (returnErr error) {
	unlockOperation := func() {}
	if s.sessionOps != nil {
		unlockOperation = s.sessionOps.lock(meta.ID)
	}
	defer unlockOperation()
	if s.registry != nil {
		if runtime, ok := s.registry.Get(meta.ID); ok && sessionIsActive(runtime) {
			return nil
		}
	}
	if err := s.store.Lock(ctx, meta.ID); err != nil {
		if errors.Is(err, zotigosession.ErrSessionLocked) {
			return nil
		}
		return fmt.Errorf("lock session for sync: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, s.store.Unlock(context.Background(), meta.ID)) }()
	return s.syncSession(ctx, rpc, meta)
}

func (s *codexSessionSyncer) syncSession(ctx context.Context, rpc codexapp.RPC, meta zotigosession.Metadata) error {
	var history codexThreadSnapshot
	if err := rpc.Call(ctx, "thread/read", map[string]any{
		"threadId": meta.ConversationID, "includeTurns": false,
	}, &history); err != nil {
		return fmt.Errorf("read changed thread: %w", err)
	}
	if history.Thread.ID != meta.ConversationID {
		return fmt.Errorf("read changed thread: got thread %q, want %q", history.Thread.ID, meta.ConversationID)
	}
	if history.Thread.UpdatedAt <= 0 {
		return fmt.Errorf("read changed thread: missing updatedAt")
	}
	snapshotUpdatedAt := time.Unix(history.Thread.UpdatedAt, 0).UTC()
	turns, err := readCodexThreadHistory(ctx, rpc, meta.ConversationID)
	if err != nil {
		return err
	}
	if err := syncCompletedCodexTurns(ctx, s.items, meta.ID, turns); err != nil {
		return err
	}
	updater, ok := s.store.(backendActivityUpdater)
	if !ok {
		return fmt.Errorf("session store does not support backend activity updates")
	}
	if s.catalog != nil {
		if _, err := s.catalog.RecordSessionActivity(ctx, meta.ID, snapshotUpdatedAt); err != nil {
			return fmt.Errorf("record catalog activity: %w", err)
		}
	}
	if err := updater.UpdateBackendActivity(ctx, meta.ID, snapshotUpdatedAt, codexSessionSyncVersion); err != nil {
		return fmt.Errorf("persist backend activity: %w", err)
	}
	return nil
}

func readCodexThreadHistory(ctx context.Context, rpc codexapp.RPC, threadID string) ([]codexTurn, error) {
	turns := make([]codexTurn, 0)
	var cursor string
	for {
		params := map[string]any{"threadId": threadID, "limit": 100, "sortDirection": "asc", "itemsView": "notLoaded"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response codexTurnList
		if err := rpc.Call(ctx, "thread/turns/list", params, &response); err != nil {
			return nil, fmt.Errorf("list Codex thread turns: %w", err)
		}
		turns = append(turns, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	turnByID := make(map[string]*codexTurn, len(turns))
	for index := range turns {
		turnByID[turns[index].ID] = &turns[index]
		turns[index].Items = nil
	}
	cursor = ""
	for {
		params := map[string]any{"threadId": threadID, "limit": 100, "sortDirection": "asc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response codexThreadItemList
		if err := rpc.Call(ctx, "thread/items/list", params, &response); err != nil {
			return nil, fmt.Errorf("list Codex thread items: %w", err)
		}
		for _, entry := range response.Data {
			turn := turnByID[entry.TurnID]
			if turn == nil {
				return nil, fmt.Errorf("codex item %q references unknown turn %q", entry.Item.ID, entry.TurnID)
			}
			turn.Items = append(turn.Items, entry.Item)
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			return turns, nil
		}
		cursor = *response.NextCursor
	}
}

func listCodexThreadActivity(ctx context.Context, rpc codexapp.RPC, candidates []zotigosession.Metadata) (map[string]time.Time, error) {
	wanted := make(map[string]bool, len(candidates))
	for _, meta := range candidates {
		wanted[meta.ConversationID] = true
	}
	activity := make(map[string]time.Time, len(candidates))
	for _, archived := range []bool{false, true} {
		var cursor string
		for {
			params := map[string]any{
				"limit": 100, "sortKey": "updated_at", "sortDirection": "desc", "archived": archived,
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var response codexThreadList
			if err := rpc.Call(ctx, "thread/list", params, &response); err != nil {
				return nil, fmt.Errorf("list Codex thread activity: %w", err)
			}
			for _, thread := range response.Data {
				if wanted[thread.ID] && thread.UpdatedAt > 0 {
					activity[thread.ID] = time.Unix(thread.UpdatedAt, 0).UTC()
				}
			}
			if len(activity) == len(wanted) {
				return activity, nil
			}
			if response.NextCursor == nil || *response.NextCursor == "" {
				break
			}
			cursor = *response.NextCursor
		}
	}
	return activity, nil
}

type codexDisplayIndex struct {
	itemIDs              map[string]bool
	toolResults          map[string]bool
	startedTurns         map[string]bool
	finishedTurns        map[string]codexTerminalState
	localUserKeysByTurn  map[string]map[string]bool
	pendingLocalUserKeys []string
}

type codexTerminalState struct {
	itemType       zotigosession.DisplayItemType
	status         string
	durationMS     int64
	errorText      string
	completedAt    int64
	hasCompletedAt bool
}

func (s codexTerminalState) matches(desired codexTerminalState) bool {
	if s.itemType != desired.itemType || s.status != desired.status || s.durationMS != desired.durationMS || s.errorText != desired.errorText {
		return false
	}
	return !desired.hasCompletedAt || s.completedAt == desired.completedAt
}

func syncCompletedCodexTurns(ctx context.Context, items displayItemSource, sessionID string, turns []codexTurn) error {
	existing, _, err := items.LoadItems(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load display items: %w", err)
	}
	seen := indexCodexDisplayItems(existing)
	associatePendingCodexUsers(&seen, turns)
	for _, turn := range turns {
		if turn.Status == "inProgress" {
			continue
		}
		createdAt := codexUnixTime(turn.StartedAt)
		if !seen.startedTurns[turn.ID] {
			if err := appendSyncedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
				ID: "codex-turn-started-" + turn.ID, Type: zotigosession.DisplayItemTurnStarted,
				Turn: &zotigosession.DisplayTurn{ID: turn.ID, Status: "in_progress"}, CreatedAt: createdAt,
			}); err != nil {
				return err
			}
			seen.startedTurns[turn.ID] = true
		}
		for _, item := range turn.Items {
			if err := syncCompletedCodexItem(ctx, items, sessionID, turn.ID, createdAt, item, &seen); err != nil {
				return err
			}
		}
		itemType := zotigosession.DisplayItemTurnCompleted
		switch strings.ToLower(turn.Status) {
		case "failed":
			itemType = zotigosession.DisplayItemTurnFailed
		case "interrupted":
			itemType = zotigosession.DisplayItemTurnInterrupted
		}
		duration := int64(0)
		if turn.DurationMS != nil {
			duration = *turn.DurationMS
		}
		errorText := ""
		if turn.Error != nil {
			errorText = turn.Error.Message
		}
		desiredTerminal := codexTerminalState{
			itemType: itemType, status: strings.ToLower(turn.Status), durationMS: duration,
			errorText: errorText,
		}
		if turn.CompletedAt != nil && *turn.CompletedAt > 0 {
			desiredTerminal.completedAt = *turn.CompletedAt
			desiredTerminal.hasCompletedAt = true
		}
		if seen.finishedTurns[turn.ID].matches(desiredTerminal) {
			continue
		}
		terminalID := "codex-turn-finished-" + turn.ID
		if seen.finishedTurns[turn.ID].itemType != "" {
			terminalID += "-" + strings.ToLower(turn.Status)
		}
		if err := appendSyncedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: terminalID, Type: itemType, Error: errorText,
			Turn:      &zotigosession.DisplayTurn{ID: turn.ID, Status: strings.ToLower(turn.Status), DurationMS: duration},
			CreatedAt: codexUnixTime(turn.CompletedAt),
		}); err != nil {
			return err
		}
		seen.finishedTurns[turn.ID] = desiredTerminal
	}
	return nil
}

func indexCodexDisplayItems(items []zotigosession.DisplayItem) codexDisplayIndex {
	index := codexDisplayIndex{
		itemIDs: make(map[string]bool), toolResults: make(map[string]bool),
		startedTurns: make(map[string]bool), finishedTurns: make(map[string]codexTerminalState),
		localUserKeysByTurn: make(map[string]map[string]bool),
	}
	pendingLocalUserKeys := make([]string, 0)
	for itemIndex, item := range items {
		index.itemIDs[item.ID] = true
		if item.Type == zotigosession.DisplayItemUserMessage && item.Turn == nil {
			if key := codexUserContentKey(item.Content); key != "" {
				pendingLocalUserKeys = append(pendingLocalUserKeys, key)
			}
		}
		if item.Turn != nil {
			if item.Type == zotigosession.DisplayItemUserMessage || item.Type == zotigosession.DisplayItemSteeringMessage {
				index.addLocalUserKey(item.Turn.ID, codexUserContentKey(item.Content))
			}
			switch item.Type {
			case zotigosession.DisplayItemTurnStarted:
				index.startedTurns[item.Turn.ID] = true
				if itemIndex > 0 && len(pendingLocalUserKeys) > 0 {
					index.addLocalUserKey(item.Turn.ID, pendingLocalUserKeys[0])
					pendingLocalUserKeys = pendingLocalUserKeys[1:]
				}
			case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
				index.finishedTurns[item.Turn.ID] = codexTerminalState{
					itemType: item.Type, status: item.Turn.Status, durationMS: item.Turn.DurationMS,
					errorText: item.Error, completedAt: item.CreatedAt.Unix(),
				}
			}
		}
		for _, part := range item.Content {
			if part.ToolResult != nil {
				index.toolResults[part.ToolResult.ToolCallID] = true
			}
		}
	}
	index.pendingLocalUserKeys = pendingLocalUserKeys
	return index
}

func associatePendingCodexUsers(index *codexDisplayIndex, turns []codexTurn) {
	usedTurns := make(map[string]bool)
	for pendingIndex := len(index.pendingLocalUserKeys) - 1; pendingIndex >= 0; pendingIndex-- {
		key := index.pendingLocalUserKeys[pendingIndex]
		for turnIndex := len(turns) - 1; turnIndex >= 0; turnIndex-- {
			turn := turns[turnIndex]
			if usedTurns[turn.ID] {
				continue
			}
			matched := false
			for _, item := range turn.Items {
				if item.Type != "userMessage" {
					continue
				}
				content, err := codexUserMessageContent(item.Content)
				if err == nil && codexUserContentKey(content) == key {
					matched = true
					break
				}
			}
			if matched {
				index.addLocalUserKey(turn.ID, key)
				usedTurns[turn.ID] = true
				break
			}
		}
	}
}

func (i *codexDisplayIndex) addLocalUserKey(turnID string, key string) {
	if turnID == "" || key == "" {
		return
	}
	if i.localUserKeysByTurn[turnID] == nil {
		i.localUserKeysByTurn[turnID] = make(map[string]bool)
	}
	i.localUserKeysByTurn[turnID][key] = true
}

func syncCompletedCodexItem(ctx context.Context, items displayItemSource, sessionID string, turnID string, createdAt time.Time, item codexThreadItem, seen *codexDisplayIndex) error {
	if item.ID == "" {
		return nil
	}
	switch item.Type {
	case "userMessage":
		if seen.itemIDs[item.ID] {
			return nil
		}
		if item.ClientID != nil && *item.ClientID != "" && seen.itemIDs[*item.ClientID] {
			seen.itemIDs[item.ID] = true
			return nil
		}
		content, err := codexUserMessageContent(item.Content)
		if err != nil {
			return err
		}
		if key := codexUserContentKey(content); key != "" && seen.localUserKeysByTurn[turnID][key] {
			seen.itemIDs[item.ID] = true
			return nil
		}
		return appendIndexedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: item.ID, Type: zotigosession.DisplayItemUserMessage, Role: string(protocol.RoleUser),
			Content: content, Turn: &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
		}, seen)
	case "agentMessage":
		if seen.itemIDs[item.ID] || item.Text == "" {
			return nil
		}
		return appendIndexedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content:   []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: item.Text}},
			Turn:      &zotigosession.DisplayTurn{ID: turnID},
			CreatedAt: createdAt,
		}, seen)
	case "reasoning":
		text := codexReasoningText(item)
		if seen.itemIDs[item.ID] || text == "" {
			return nil
		}
		return appendIndexedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content:   []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeReasoning), Text: text}},
			Turn:      &zotigosession.DisplayTurn{ID: turnID},
			CreatedAt: createdAt,
		}, seen)
	case "contextCompaction":
		if seen.itemIDs[item.ID] {
			return nil
		}
		return appendIndexedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: item.ID, Type: zotigosession.DisplayItemContextCompacted,
			Turn: &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
		}, seen)
	default:
		return syncCompletedCodexTool(ctx, items, sessionID, turnID, createdAt, item, seen)
	}
}

func codexUserContentKey(content []zotigosession.DisplayContentPart) string {
	parts := make([]string, 0, len(content))
	for _, part := range content {
		switch part.Type {
		case string(protocol.ContentTypeText):
			parts = append(parts, "text\x00"+part.Text)
		case string(protocol.ContentTypeImage):
			if part.Image == nil {
				return ""
			}
			identity := part.Image.FileID
			if identity == "" {
				identity = part.Image.URL
				if query := strings.IndexByte(identity, '?'); query >= 0 {
					identity = identity[:query]
				}
				if slash := strings.LastIndexAny(identity, `/\\`); slash >= 0 {
					identity = identity[slash+1:]
				}
			}
			if identity == "" {
				return ""
			}
			parts = append(parts, "image\x00"+identity)
		default:
			return ""
		}
	}
	return strings.Join(parts, "\x1e")
}

func syncCompletedCodexTool(ctx context.Context, items displayItemSource, sessionID string, turnID string, createdAt time.Time, item codexThreadItem, seen *codexDisplayIndex) error {
	name, arguments, ok, err := codexToolCall(item)
	if err != nil || !ok {
		return err
	}
	if !seen.itemIDs[item.ID] {
		if err := appendIndexedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
			ID: item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{
				ID: item.ID, Name: name, Arguments: arguments,
			}}}, Turn: &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
		}, seen); err != nil {
			return err
		}
	}
	if seen.toolResults[item.ID] {
		return nil
	}
	result, ok := codexToolResult(item, name)
	if !ok {
		return nil
	}
	if err := appendSyncedCodexItem(ctx, items, sessionID, zotigosession.DisplayItem{
		ID: "codex-tool-result-" + item.ID, Type: zotigosession.DisplayItemAssistantMessage,
		Role: string(protocol.RoleAssistant), Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: result}},
		Turn: &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
	}); err != nil {
		return err
	}
	seen.toolResults[item.ID] = true
	return nil
}

func codexUserMessageContent(value any) ([]zotigosession.DisplayContentPart, error) {
	encoded, err := sonic.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Codex user message: %w", err)
	}
	var inputs []codexUserInput
	if err := sonic.Unmarshal(encoded, &inputs); err != nil {
		return nil, fmt.Errorf("decode Codex user message: %w", err)
	}
	content := make([]zotigosession.DisplayContentPart, 0, len(inputs))
	for _, input := range inputs {
		switch input.Type {
		case "text":
			content = append(content, zotigosession.DisplayContentPart{Type: string(protocol.ContentTypeText), Text: input.Text})
		case "image", "localImage":
			url := input.URL
			if url == "" {
				url = input.Path
			}
			content = append(content, zotigosession.DisplayContentPart{
				Type: string(protocol.ContentTypeImage), Image: &zotigosession.DisplayMediaPart{URL: url},
			})
		}
	}
	return content, nil
}

func appendIndexedCodexItem(ctx context.Context, items displayItemSource, sessionID string, item zotigosession.DisplayItem, seen *codexDisplayIndex) error {
	if err := appendSyncedCodexItem(ctx, items, sessionID, item); err != nil {
		return err
	}
	seen.itemIDs[item.ID] = true
	return nil
}

func appendSyncedCodexItem(ctx context.Context, items displayItemSource, sessionID string, item zotigosession.DisplayItem) error {
	if _, err := items.AppendItem(ctx, sessionID, item); err != nil {
		return fmt.Errorf("append Codex display item: %w", err)
	}
	return nil
}

func codexUnixTime(value *int64) time.Time {
	if value == nil || *value <= 0 {
		return time.Time{}
	}
	return time.Unix(*value, 0).UTC()
}
