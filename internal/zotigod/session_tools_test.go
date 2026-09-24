package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	session "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

func newSessionToolFixture(t *testing.T) (*handler, sessionToolCaller) {
	t.Helper()
	h, store, catalog, workspace := newChannelProvisionFixture(t)
	h.items = storedDisplayItemSource{store: store}
	h.workers = newWorkerRegistry()
	source := newSession(workspace.RootPath, "channel-default")
	if err := h.persistSession(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AssignSession(context.Background(), source.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	h.registry.Add(source)
	h.workers.workers[source.ID] = newWorkerConnection(source.ID, "generation", nil, h.workers)
	stored, err := store.Get(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []session.DisplayItem{
		{ID: "input", Type: session.DisplayItemUserMessage, Command: &session.DisplayCommand{Type: sessionCommandMessage, Text: "test"}},
		{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "turn"}},
	} {
		if _, err := store.AppendDisplayItem(context.Background(), source.ID, item); err != nil {
			t.Fatal(err)
		}
	}
	return h, sessionToolCaller{SessionID: source.ID, TurnID: "turn", Generation: "generation", WorkspaceID: workspace.ID, Stored: stored}
}

func TestSessionToolsResolveTrustedInput(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	req := workerRuntimeToolRequest{Namespace: "zotigo", Name: "list_sessions", TurnID: "turn", CallID: "call", Arguments: json.RawMessage(`{}`), RequestContext: &protocol.RequestContext{Source: "forged"}}
	text, err := h.executeRuntimeTool(ctx, caller.SessionID, "generation", req)
	if err != nil || !strings.Contains(text, caller.SessionID) {
		t.Fatalf("list = %s, %v", text, err)
	}
	if _, err = h.executeRuntimeTool(ctx, caller.SessionID, "stale", req); err == nil {
		t.Fatal("stale worker authorized")
	}
	req.TurnID = "old"
	if _, err = h.executeRuntimeTool(ctx, caller.SessionID, "generation", req); err == nil {
		t.Fatal("old turn authorized")
	}
	req.TurnID = "turn"
	req.Arguments = json.RawMessage(`{"actor":"owner"}`)
	if _, err = h.executeRuntimeTool(ctx, caller.SessionID, "generation", req); err == nil {
		t.Fatal("identity argument authorized")
	}
	if _, err = h.store.AppendDisplayItem(ctx, caller.SessionID, session.DisplayItem{Type: session.DisplayItemSessionCommand, Command: &session.DisplayCommand{Type: sessionCommandSteering, TurnID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	req.Arguments = json.RawMessage(`{}`)
	if _, err = h.executeRuntimeTool(ctx, caller.SessionID, "generation", req); err == nil {
		t.Fatal("mixed-principal turn authorized")
	}
}

func TestSessionToolsRejectDelegatedAndMissingOrigin(t *testing.T) {
	for _, id := range []string{"session_tool:parent:call", "channel:connection:message"} {
		t.Run(id, func(t *testing.T) {
			h, caller := newSessionToolFixture(t)
			ctx := context.Background()
			for _, item := range []session.DisplayItem{
				{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn"}},
				{ID: id, Type: session.DisplayItemUserMessage, Command: &session.DisplayCommand{Type: sessionCommandMessage}},
				{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "next"}},
			} {
				if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, item); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.resolveSessionToolCaller(ctx, caller.SessionID, "generation", "next"); err == nil {
				t.Fatal("untrusted/delegated origin allowed")
			}
		})
	}
}

func TestSessionToolsReadProjectionAndScope(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	secret := "RAW_TOOL_SECRET"
	if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, session.DisplayItem{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{
		{Type: "text", Text: strings.Repeat("\x01", 10000)},
		{Type: "tool_call", ToolCall: &session.DisplayToolCall{Name: "shell", Arguments: secret}},
		{Type: "tool_result", ToolResult: &session.DisplayToolResult{Text: secret}},
	}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"session_id": caller.SessionID, "limit": 100})
	result, err := h.readToolSession(ctx, caller, raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > 64*1024 || strings.Contains(string(encoded), secret) || !strings.Contains(string(encoded), `"truncated":true`) {
		t.Fatalf("unsafe projection size=%d", len(encoded))
	}
	caller.Origin = &protocol.RequestContext{Source: "feishu"}
	if err := h.authorizeToolTarget(ctx, caller, "other", true); err == nil {
		t.Fatal("Channel read escaped bound session")
	}
	if _, err := h.readToolSession(ctx, caller, json.RawMessage(`{"session_id":"other","after":1,"before":2}`)); err == nil {
		t.Fatal("ambiguous pagination accepted")
	}
}

func TestSessionToolsCreateRecoversSameSessionAndConflictingCall(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	operation := sessionToolOperation{TargetID: "sess_tool_test", Input: sessionToolWriteInput{Title: "child"}}
	caller.Stored.PromptConfig = session.PromptConfig{AgentInstructions: "constrained", Revision: 1}
	for i := 0; i < 2; i++ {
		if err := h.createToolSession(ctx, caller, operation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.catalog.SetSessionTitle(ctx, operation.TargetID, "user renamed"); err != nil {
		t.Fatal(err)
	}
	if err := h.createToolSession(ctx, caller, operation); err != nil {
		t.Fatal(err)
	}
	renamed, err := h.catalog.GetSessionOrganization(ctx, operation.TargetID)
	if err != nil || renamed.Title == nil || *renamed.Title != "user renamed" {
		t.Fatalf("retry overwrote title: %+v %v", renamed, err)
	}
	child, err := h.store.Get(ctx, operation.TargetID)
	if err != nil || child.PromptConfig != caller.Stored.PromptConfig || child.ConversationID != "" || child.Capabilities.SessionToolsVersion != 1 {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	org, err := h.catalog.GetSessionOrganization(ctx, operation.TargetID)
	if err != nil || org.WorkspaceID == nil || *org.WorkspaceID != caller.WorkspaceID {
		t.Fatalf("organization=%+v err=%v", org, err)
	}
	dir := filepath.Join(h.sessionStoreRoot(), "runtime-tool-operations", toolDigest(caller.SessionID, caller.TurnID))
	path := filepath.Join(dir, toolDigest("call")+".json")
	op := sessionToolOperation{SourceSessionID: caller.SessionID, SourceTurnID: caller.TurnID, CallID: "call", Name: "send_message", Input: sessionToolWriteInput{SessionID: "target", Text: "original"}, TargetID: "target"}
	if err := saveSessionToolOperation(dir, path, op); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	_, err = h.writeToolSession(ctx, caller, workerRuntimeToolRequest{Name: "send_message", CallID: "call", Arguments: json.RawMessage(`{"session_id":"target","text":"changed"}`)})
	if !errors.Is(err, errCommandIDConflict) {
		t.Fatalf("conflicting retry err=%v", err)
	}
}

func TestCodexSessionToolsStartResumeAndCallback(t *testing.T) {
	cfg := codexWorkerConfig{SessionToolsVersion: 1, ChannelToolsVersion: 1}
	specs := codexThreadParams(cfg, true)["dynamicTools"].([]any)
	if len(specs) != 2 {
		t.Fatalf("namespaces=%v", specs)
	}
	if _, ok := codexThreadParams(cfg, false)["dynamicTools"]; ok {
		t.Fatal("resume reinjected tools")
	}
	if _, ok := codexThreadParams(codexWorkerConfig{}, true)["dynamicTools"]; ok {
		t.Fatal("legacy session upgraded without binding")
	}
	rpc := &codexWorkerRPC{}
	results := make(chan workerRuntimeToolResult, 1)
	runtime := &codexWorkerRuntime{cfg: cfg, app: rpc, threadID: "thread", activeTurnID: "turn", writer: &workerClientWriter{sendCh: make(chan workerMessage, 1), done: make(chan struct{})}, runtimeToolResults: results}
	done := make(chan error, 1)
	go func() {
		done <- runtime.executeCodexRuntimeTool(context.Background(), codexapp.Message{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","callId":"call","namespace":"zotigo","tool":"list_sessions","arguments":{}}`)})
	}()
	message := <-runtime.writer.sendCh
	if message.RuntimeToolRequest.Namespace != "zotigo" || message.RuntimeToolRequest.TurnID != "turn" || message.RuntimeToolRequest.CallID != "call" {
		t.Fatalf("request=%+v", message)
	}
	results <- workerRuntimeToolResult{RequestID: "call", Text: `{"sessions":[]}`}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rpc.response.(map[string]any)["success"] != true {
		t.Fatalf("response=%v", rpc.response)
	}
}

func TestSessionToolsSendDurableAdmissionAndRetry(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := newSession(caller.Stored.WorkingDirectory, caller.Stored.ProfileName)
	target.State = SessionStateRunning
	if err := h.persistSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	h.registry.Add(target)
	worker := newWorkerConnection(target.ID, "target-generation", nil, h.workers)
	h.workers.workers[target.ID] = worker
	workerDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			workerDone <- ctx.Err()
		case message := <-worker.sendCh:
			request := message.InputRequest
			if request == nil || !request.StartOnly || request.SteeringOnly {
				workerDone <- errors.New("delegated input was not start-only")
				return
			}
			accepted, err := appendAcceptedSessionInput(ctx, h.items, target.ID, request.Command)
			if err != nil {
				workerDone <- err
				return
			}
			worker.resolveInput(workerInputResult{RequestID: request.RequestID, Command: &accepted})
			workerDone <- nil
		}
	}()
	raw, _ := json.Marshal(map[string]any{"session_id": target.ID, "text": "delegated work"})
	request := workerRuntimeToolRequest{Namespace: "zotigo", Name: "send_message", TurnID: caller.TurnID, CallID: "call-send", Arguments: raw}
	first, err := h.executeRuntimeTool(ctx, caller.SessionID, "generation", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	second, err := h.executeRuntimeTool(ctx, caller.SessionID, "generation", request)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("retry differs %s %s", first, second)
	}
	items, _, err := h.items.LoadItems(ctx, target.ID)
	if err != nil || len(items) != 1 || !strings.HasPrefix(items[0].ID, "session_tool:") {
		t.Fatalf("durable input=%+v err=%v", items, err)
	}
	select {
	case message := <-worker.sendCh:
		t.Fatalf("retry resubmitted %+v", message)
	default:
	}
	go func() {
		select {
		case <-ctx.Done():
		case message := <-worker.sendCh:
			worker.resolveInput(workerInputResult{RequestID: message.InputRequest.RequestID, ErrorCode: "input_failed", Error: "connection lost after dispatch"})
		}
	}()
	request.CallID = "first-uncertain"
	uncertain, err := h.executeRuntimeTool(ctx, caller.SessionID, "generation", request)
	if err != nil || !strings.Contains(uncertain, `"status":"outcome_unknown"`) {
		t.Fatalf("first uncertain response=%s err=%v", uncertain, err)
	}
}

func TestSessionToolTransportCancellation(t *testing.T) {
	writer := &workerClientWriter{sendCh: make(chan workerMessage, 1), done: make(chan struct{})}
	client := &workerRuntimeToolClient{writer: writer, results: make(chan workerRuntimeToolResult)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.request(ctx, workerRuntimeToolRequest{Namespace: "zotigo", Name: "list_sessions", CallID: "stable-call"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestSessionToolsUncertainDispatchDoesNotResubmit(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	target := newSession(caller.Stored.WorkingDirectory, caller.Stored.ProfileName)
	if err := h.persistSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.sessionStoreRoot(), "runtime-tool-operations", toolDigest(caller.SessionID, caller.TurnID))
	operation := sessionToolOperation{ApprovalPolicy: caller.Stored.ApprovalPolicy, PromptConfig: caller.Stored.PromptConfig, WorkspaceID: caller.WorkspaceID, SourceSessionID: caller.SessionID, SourceTurnID: caller.TurnID, CallID: "uncertain", Name: "send_message", TargetID: target.ID, Input: sessionToolWriteInput{SessionID: target.ID, Text: "work"}, DispatchStarted: true}
	if err := saveSessionToolOperation(dir, filepath.Join(dir, toolDigest(operation.CallID)+".json"), operation); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(operation.Input)
	result, err := h.writeToolSession(ctx, caller, workerRuntimeToolRequest{Name: operation.Name, CallID: operation.CallID, Arguments: raw})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "outcome_unknown" {
		t.Fatalf("result=%v", result)
	}
	if _, live := h.registry.Get(target.ID); live {
		t.Fatal("uncertain retry started a worker")
	}
}

func TestSessionToolsDelegatedReplayRevalidatesDurableOrigin(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	target := newSession(caller.Stored.WorkingDirectory, caller.Stored.ProfileName)
	if err := h.persistSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	turnKey, callKey := toolDigest(caller.SessionID, caller.TurnID), toolDigest("replay")
	commandID := "session_tool:" + turnKey + ":" + callKey
	operation := sessionToolOperation{ApprovalPolicy: caller.Stored.ApprovalPolicy, PromptConfig: caller.Stored.PromptConfig, WorkspaceID: caller.WorkspaceID, SourceSessionID: caller.SessionID, SourceTurnID: caller.TurnID, CallID: "replay", Name: "send_message", TargetID: target.ID, Input: sessionToolWriteInput{SessionID: target.ID, Text: "work"}, DispatchStarted: true}
	dir := filepath.Join(h.sessionStoreRoot(), "runtime-tool-operations", turnKey)
	if err := saveSessionToolOperation(dir, filepath.Join(dir, callKey+".json"), operation); err != nil {
		t.Fatal(err)
	}
	if _, err := appendAcceptedSessionInput(ctx, h.items, target.ID, commandResponse{ID: commandID, Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "work"}}); err != nil {
		t.Fatal(err)
	}
	// Original source turn has ended; replay authority must not depend on it.
	if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, session.DisplayItem{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: caller.TurnID}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"command_id": commandID})
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, raw); err != nil {
		t.Fatal(err)
	}
	stored, err := h.store.Get(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.PromptConfig.Revision++
	if err := h.store.Put(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, raw); err == nil {
		t.Fatal("replay ignored changed target policy")
	}
	caller.Stored.PromptConfig = stored.PromptConfig
	if err := h.store.Put(ctx, caller.Stored); err != nil {
		t.Fatal(err)
	}
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, raw); err == nil {
		t.Fatal("replay allowed both sides to expand beyond the original grant")
	}
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, json.RawMessage(`{"command_id":"session_tool:../../escape:bad"}`)); err == nil {
		t.Fatal("invalid operation path allowed")
	}
}

func TestSessionToolOriginEqualitySurvivesPersistence(t *testing.T) {
	original := &protocol.RequestContext{Source: "feishu", ConnectionID: "bot", Channel: &protocol.RequestIdentity{ID: "robot", DisplayName: "test"}, Actor: protocol.RequestActor{ID: "owner"}}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored protocol.RequestContext
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if !sameRequestContext(original, &restored) {
		t.Fatal("equal durable origin rejected after decode")
	}
	restored.Actor.ID = "other"
	if sameRequestContext(original, &restored) {
		t.Fatal("different actor accepted")
	}
}

func TestSessionToolReplayRejectsRevokedChannelOwner(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer channelStore.Close()
	connection, err := channelStore.PutConnection(ctx, channels.Connection{ID: "test-bot", Provider: channels.ProviderFeishu, AppID: "isolated-test", Enabled: true, OwnerSenderIDs: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversation(ctx, connection.ID, "test-chat", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.Enabled = true
	conversation.SessionID = caller.SessionID
	conversation.WorkspaceID = caller.WorkspaceID
	conversation.SenderPolicy = channels.SenderPolicyAll
	conversation, err = channelStore.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	h.channels = channels.NewService(channelStore, nil, nil, nil)
	origin := &protocol.RequestContext{Source: channels.ProviderFeishu, ConnectionID: connection.ID, ConversationID: conversation.ID, ExternalConversation: "test-chat", Actor: protocol.RequestActor{ID: "owner", Role: "owner"}}
	target := newSession(caller.Stored.WorkingDirectory, caller.Stored.ProfileName)
	if err := h.persistSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	turnKey, callKey := toolDigest(caller.SessionID, caller.TurnID), toolDigest("owner-replay")
	commandID := "session_tool:" + turnKey + ":" + callKey
	operation := sessionToolOperation{WorkspaceID: caller.WorkspaceID, SourceSessionID: caller.SessionID, SourceTurnID: caller.TurnID, CallID: "owner-replay", Name: "send_message", TargetID: target.ID, Input: sessionToolWriteInput{SessionID: target.ID, Text: "work"}, DispatchStarted: true, Origin: origin, ApprovalPolicy: caller.Stored.ApprovalPolicy, PromptConfig: caller.Stored.PromptConfig}
	dir := filepath.Join(h.sessionStoreRoot(), "runtime-tool-operations", turnKey)
	if err := saveSessionToolOperation(dir, filepath.Join(dir, callKey+".json"), operation); err != nil {
		t.Fatal(err)
	}
	if _, err := appendAcceptedSessionInput(ctx, h.items, target.ID, commandResponse{ID: commandID, Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "work", RequestContext: origin}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"command_id": commandID})
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, raw); err != nil {
		t.Fatal(err)
	}
	connection.OwnerSenderIDs = nil
	if _, err := channelStore.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	if _, err := h.authorizeDelegatedReplay(ctx, target.ID, raw); err == nil {
		t.Fatal("replay accepted a revoked owner")
	}
}

func TestChannelOwnerReadsAndForksWorkspaceSessions(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	target := newSession(caller.Stored.WorkingDirectory, "channel-default")
	if err := h.persistSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AppendDisplayItem(ctx, target.ID, session.DisplayItem{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "review result"}}}); err != nil {
		t.Fatal(err)
	}

	source, err := h.store.Get(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	source.AgentSnapshot = agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{protocol.NewUserMessage("review"), protocol.NewAssistantMessage("review result")}}
	source.ForkPoints = map[string]session.ForkPoint{"turn": {HistoryLength: 2, HistoryDigest: session.SnapshotDigest(source.AgentSnapshot)}}
	if err := h.store.Put(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AppendDisplayItem(ctx, target.ID, session.DisplayItem{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer channelStore.Close()
	connection, err := channelStore.PutConnection(ctx, channels.Connection{ID: "bot", Provider: channels.ProviderFeishu, AppID: "app", Enabled: true, OwnerSenderIDs: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversation(ctx, "bot", "chat", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.Enabled, conversation.SessionID, conversation.WorkspaceID, conversation.SenderPolicy = true, caller.SessionID, caller.WorkspaceID, channels.SenderPolicyAll
	conversation, err = channelStore.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	h.channels = channels.NewService(channelStore, nil, nil, nil)
	caller.Origin = &protocol.RequestContext{Source: channels.ProviderFeishu, ConnectionID: "bot", ConversationID: conversation.ID, ExternalConversation: "chat", Actor: protocol.RequestActor{ID: "owner", Role: "owner"}}
	for _, item := range []session.DisplayItem{
		{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn"}},
		{ID: "channel:bot:message", Type: session.DisplayItemUserMessage, Command: &session.DisplayCommand{Type: sessionCommandMessage, Text: "read history", RequestContext: caller.Origin}},
		{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "owner-turn"}},
	} {
		if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, item); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(name string, args json.RawMessage) (string, error) {
		return h.executeRuntimeTool(ctx, caller.SessionID, "generation", workerRuntimeToolRequest{Namespace: "zotigo", Name: name, TurnID: "owner-turn", CallID: name, Arguments: args})
	}
	raw, _ := json.Marshal(map[string]any{"session_id": target.ID})
	for _, owner := range []bool{false, true, false} {
		connection.OwnerSenderIDs = nil
		if owner {
			connection.OwnerSenderIDs = []string{"owner"}
		}
		if _, err := channelStore.PutConnection(ctx, connection); err != nil {
			t.Fatal(err)
		}
		listed, err := invoke("list_sessions", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(listed)
		if strings.Contains(string(encoded), target.ID) != owner {
			t.Fatalf("owner=%v list=%s", owner, encoded)
		}
		result, err := invoke("read_session", raw)
		if owner {
			if err != nil {
				t.Fatal(err)
			}
			content, _ := json.Marshal(result)
			if !strings.Contains(string(content), "review result") {
				t.Fatalf("missing history: %s", content)
			}
			forkArgs, _ := json.Marshal(map[string]any{"workspace_id": caller.WorkspaceID, "initial_message": "review", "fork_from": map[string]any{"session_id": target.ID, "through_turn_id": "turn"}})
			// No worker is launched in this fixture; the fork must be persisted before dispatch fails.
			if _, err := invoke("create_session", forkArgs); !errors.Is(err, errWorkerOffline) {
				t.Fatalf("owner fork dispatch: %v", err)
			}
			childID := "sess_tool_" + toolDigest(toolDigest(caller.SessionID, "owner-turn"), toolDigest("create_session"))
			child, err := h.store.Get(ctx, childID)
			if err != nil || child == nil || child.ForkedFrom == nil || child.ForkedFrom.SessionID != target.ID {
				t.Fatalf("fork child=%+v err=%v", child, err)
			}
			if child.ConversationID != "" || child.Capabilities.ChannelToolsVersion != 0 {
				t.Fatal("fork inherited Channel binding")
			}

		} else if err == nil {
			t.Fatal("non-owner read another session")
		}
		if !owner {
			forkArgs, _ := json.Marshal(map[string]any{"workspace_id": caller.WorkspaceID, "initial_message": "review", "fork_from": map[string]any{"session_id": target.ID, "through_turn_id": "turn"}})
			if _, err := invoke("create_session", forkArgs); err == nil || !strings.Contains(err.Error(), "only the robot connection owner") {
				t.Fatalf("non-owner fork: %v", err)
			}
		}

	}
	caller.CanReadWorkspaceSessions = true
	caller.WorkspaceID = "different-workspace"
	if err := h.authorizeToolTarget(ctx, caller, target.ID, true); err == nil {
		t.Fatal("owner escaped workspace")
	}
}
