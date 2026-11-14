package wire

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

// TestConstantValues pins every frame kind, command kind and shared constant.
// The values are read by already-deployed daemons and workers; a failure here
// means the wire format changed, not that the test needs updating.
func TestConstantValues(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"MessageCommand", string(MessageCommand), "command"},
		{"MessageWorking", string(MessageWorking), "working"},
		{"MessageDelta", string(MessageDelta), "display_delta"},
		{"MessageDisplayWake", string(MessageDisplayWake), "display_wake"},
		{"MessageDisplayBarrier", string(MessageDisplayBarrier), "display_barrier"},
		{"MessageDisplayBarrierOK", string(MessageDisplayBarrierOK), "display_barrier_ack"},
		{"MessageApprovalRequest", string(MessageApprovalRequest), "approval_request"},
		{"MessageApprovalDecision", string(MessageApprovalDecision), "approval_decision"},
		{"MessageApprovalResult", string(MessageApprovalResult), "approval_result"},
		{"MessageInteractionRequest", string(MessageInteractionRequest), "interaction_request"},
		{"MessageInteractionResponse", string(MessageInteractionResponse), "interaction_response"},
		{"MessageInteractionResult", string(MessageInteractionResult), "interaction_result"},
		{"MessageConversationBound", string(MessageConversationBound), "conversation_bound"},
		{"MessageConversationBoundResult", string(MessageConversationBoundResult), "conversation_bound_result"},
		{"MessageInputRequest", string(MessageInputRequest), "input_request"},
		{"MessageInputResult", string(MessageInputResult), "input_result"},
		{"MessageInterruptTurn", string(MessageInterruptTurn), "interrupt_turn"},
		{"MessageIdle", string(MessageIdle), "idle"},
		{"MessageRuntimeToolRequest", string(MessageRuntimeToolRequest), "runtime_tool_request"},
		{"MessageRuntimeToolResult", string(MessageRuntimeToolResult), "runtime_tool_result"},
		{"CommandMessage", CommandMessage, "message"},
		{"CommandPause", CommandPause, "pause"},
		{"CommandSteering", CommandSteering, "steering"},
		{"CommandProfile", CommandProfile, "profile"},
		{"CommandApprovalPolicy", CommandApprovalPolicy, "approval_policy"},
		{"UserPauseReason", UserPauseReason, "user_pause"},
		{"GenerationHeader", GenerationHeader, "X-Zotigo-Worker-Generation"},
		{"WorkspaceHeader", WorkspaceHeader, "X-Zotigo-Workspace-ID"},
		{"MaxCommandsLimit", strconv.Itoa(MaxCommandsLimit), "200"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// payloadValues returns one fully populated value of every wire struct, plus
// zero values of the ones with omitempty fields.
func payloadValues() map[string]any {
	created := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	resolved := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	autoResolve := uint64(1500)
	requestContext := &protocol.RequestContext{Source: "channel", ConnectionID: "connection-1", ConversationID: "conversation-1"}
	images := []CommandImageData{{MimeType: "image/png", SizeBytes: 12, Width: 3, Height: 4, DataBase64: "AAEC", BlobPath: "blobs/one"}}
	message := MessageCommandPayload{Text: "hello", Images: images, Skills: []string{"review"}, RequestContext: requestContext}
	steering := SteeringCommandPayload{Text: "steer", Images: images, Skills: []string{"review"}, TurnID: "turn-1", RequestContext: requestContext}
	pause := PauseCommandPayload{TurnID: "turn-1", Reason: "user_pause"}
	profile := ProfileCommandPayload{Name: "fast"}
	policy := ApprovalPolicyCommandPayload{Policy: agent.ApprovalPolicyManual}
	command := Command{
		ID: "command-1", Sequence: 7, Type: CommandMessage, CreatedAt: created,
		Message: &message, Steering: &steering, Pause: &pause, Profile: &profile, ApprovalPolicy: &policy,
	}
	pending := PendingApproval{
		ToolCallID: "call-1", ToolName: "shell", Arguments: `{"command":"ls"}`, Description: "list files",
		Reason: "not safe", RiskLevel: "medium", Source: "classifier", RequiresSnapshot: true,
	}
	toolDecision := ApprovalToolDecision{ToolCallID: "call-1", Approved: true, Reason: "ok", ModifiedArgs: `{"command":"pwd"}`}
	approval := ApprovalRequest{
		ID: "approval-1", SessionID: "session-1", TurnID: "turn-1", Status: "resolved",
		Pending: []PendingApproval{pending}, Decisions: []ApprovalToolDecision{toolDecision},
		CreatedAt: created, ResolvedAt: &resolved,
	}
	requester := InteractionRequester{Agent: "codex", ThreadID: "thread-1", Name: "child"}
	option := InteractionOption{Label: "Yes", Description: "go ahead"}
	question := InteractionQuestion{ID: "q1", Header: "Confirm", Question: "Proceed?", IsOther: true, IsSecret: true, Options: []InteractionOption{option}}
	interaction := InteractionRequest{
		ID: "interaction-1", SessionID: "session-1", TurnID: "turn-1", ItemID: "item-1", Kind: "user_input",
		Status: "resolved", Requester: requester, Questions: []InteractionQuestion{question},
		Answers: map[string][]string{"q1": {"Yes"}}, IsBlocking: true, AutoResolveMS: &autoResolve,
		CreatedAt: created, ResolvedAt: &resolved,
	}
	delta := DisplayDelta{
		BeforeInputID: "input-1", ItemID: "item-1", Role: "assistant", PartType: "text", Delta: "hi",
		ToolCallID: "call-1", ToolName: "shell",
		Subagent: &zotigosession.DisplaySubagent{ToolCallID: "spawn-1", Name: "child", AgentType: "explore", WorkDir: "/w", Description: "look", Status: "running"},
	}
	retirement := PreviewRetirement{ItemID: "item-1", ToolCallIDs: []string{"call-1"}, Terminal: true}
	barrier := DisplayBarrier{ID: "barrier-1"}
	approvalDecision := ApprovalDecision{
		RequestID: "request-1", ApprovalID: "approval-1",
		Decisions: []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true, Reason: "ok", ModifiedArgs: "{}"}},
		StopTurn:  true,
	}
	approvalResult := ApprovalResult{RequestID: "request-1", Approval: &approval, Error: "failed"}
	interactionResponse := InteractionResponse{RequestID: "request-1", InteractionID: "interaction-1", Answers: map[string][]string{"q1": {"Yes"}}}
	interactionResult := InteractionResult{RequestID: "request-1", Interaction: &interaction, Error: "failed"}
	bound := ConversationBound{ConversationID: "conversation-1"}
	boundResult := ConversationBoundResult{ConversationID: "conversation-1", ErrorCode: "conflict", Error: "already bound"}
	inputRequest := InputRequest{
		RequestID: "request-1", Command: command, SteeringOnly: true, StartOnly: true, ExpectedTurnID: "turn-1",
		RequiredApprovalPolicy: agent.ApprovalPolicyAuto, RequiredPromptRevision: 9, RequirePromptRevision: true,
	}
	inputResult := InputResult{RequestID: "request-1", Command: &command, ErrorCode: "conflict", Error: "busy"}
	interrupt := InterruptTurn{ThreadID: "thread-1", TurnID: "turn-1"}
	idle := Idle{CommandSequence: 7}
	toolRequest := RuntimeToolRequest{
		RequestID: "request-1", Namespace: "channel", TurnID: "turn-1", CallID: "call-1", Name: "send",
		Arguments: json.RawMessage(`{"text":"hi"}`), RequestContext: requestContext,
	}
	toolResult := RuntimeToolResult{RequestID: "request-1", Text: "done", Error: "failed"}
	frame := Message{
		Type: MessageCommand, Command: &command, PreviewRetirement: &retirement, Delta: &delta,
		DisplayBarrier: &barrier, ApprovalRequest: &approval, ApprovalDecision: &approvalDecision,
		ApprovalResult: &approvalResult, InteractionRequest: &interaction, InteractionResponse: &interactionResponse,
		InteractionResult: &interactionResult, ConversationBound: &bound, ConversationBoundResult: &boundResult,
		InputRequest: &inputRequest, InputResult: &inputResult, InterruptTurn: &interrupt, Idle: &idle,
		RuntimeToolRequest: &toolRequest, RuntimeToolResult: &toolResult,
	}
	return map[string]any{
		"Message":                      frame,
		"MessageZero":                  Message{Type: MessageWorking},
		"Command":                      command,
		"CommandZero":                  Command{},
		"CommandsPage":                 CommandsPage{Commands: []Command{command}, NextCursor: "7", NextOffset: 42},
		"CommandsPageZero":             CommandsPage{},
		"MessageCommandPayload":        message,
		"SteeringCommandPayload":       steering,
		"PauseCommandPayload":          pause,
		"ProfileCommandPayload":        profile,
		"ApprovalPolicyCommandPayload": policy,
		"CommandImageData":             images[0],
		"PendingApproval":              pending,
		"ApprovalToolDecision":         toolDecision,
		"ApprovalRequest":              approval,
		"ApprovalRequestZero":          ApprovalRequest{},
		"ApprovalDecision":             approvalDecision,
		"ApprovalResult":               approvalResult,
		"InteractionRequester":         requester,
		"InteractionOption":            option,
		"InteractionQuestion":          question,
		"InteractionRequest":           interaction,
		"InteractionRequestZero":       InteractionRequest{},
		"InteractionResponse":          interactionResponse,
		"InteractionResult":            interactionResult,
		"DisplayDelta":                 delta,
		"DisplayDeltaZero":             DisplayDelta{},
		"PreviewRetirement":            retirement,
		"DisplayBarrier":               barrier,
		"ConversationBound":            bound,
		"ConversationBoundResult":      boundResult,
		"InputRequest":                 inputRequest,
		"InputRequestZero":             InputRequest{},
		"InputResult":                  inputResult,
		"InterruptTurn":                interrupt,
		"Idle":                         idle,
		"RuntimeToolRequest":           toolRequest,
		"RuntimeToolResult":            toolResult,
		"ReadyRequest":                 ReadyRequest{Generation: "generation-1"},
		"FinishSessionRequest":         FinishSessionRequest{Error: "failed", ErrorCode: "worker_failed", Generation: "generation-1"},
		"FinishSessionRequestZero":     FinishSessionRequest{},
	}
}

// TestPayloadJSON marshals a populated value of every wire struct and compares
// it with the JSON the daemon and workers exchanged before the types moved to
// this package. A renamed field, a changed tag or a reordered field fails it.
func TestPayloadJSON(t *testing.T) {
	values := payloadValues()
	for name := range goldenJSON {
		if _, ok := values[name]; !ok {
			t.Errorf("golden %s has no value", name)
		}
	}
	for name, value := range values {
		want, ok := goldenJSON[name]
		if !ok {
			t.Errorf("%s has no golden JSON", name)
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Errorf("marshal %s: %v", name, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s wire format changed\n got: %s\nwant: %s", name, data, want)
		}
	}
}

// TestMessageRoundTrip checks that the golden frame decodes back into the
// same value, so the tags are pinned in both directions.
func TestMessageRoundTrip(t *testing.T) {
	var decoded Message
	if err := json.Unmarshal([]byte(goldenJSON["Message"]), &decoded); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != goldenJSON["Message"] {
		t.Errorf("round trip changed the frame\n got: %s\nwant: %s", data, goldenJSON["Message"])
	}
}

// goldenJSON was captured from the definitions as they were in package
// zotigod before the move. Do not regenerate it to make a test pass.
var goldenJSON = map[string]string{
	"ApprovalDecision":             `{"request_id":"request-1","approval_id":"approval-1","decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{}"}],"stop_turn":true}`,
	"ApprovalPolicyCommandPayload": `{"policy":"manual"}`,
	"ApprovalRequest":              `{"id":"approval-1","session_id":"session-1","turn_id":"turn-1","status":"resolved","pending":[{"tool_call_id":"call-1","tool_name":"shell","arguments":"{\"command\":\"ls\"}","description":"list files","reason":"not safe","risk_level":"medium","source":"classifier","requires_snapshot":true}],"decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{\"command\":\"pwd\"}"}],"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"}`,
	"ApprovalRequestZero":          `{"id":"","session_id":"","turn_id":"","status":"","pending":null,"created_at":"0001-01-01T00:00:00Z"}`,
	"ApprovalResult":               `{"request_id":"request-1","approval":{"id":"approval-1","session_id":"session-1","turn_id":"turn-1","status":"resolved","pending":[{"tool_call_id":"call-1","tool_name":"shell","arguments":"{\"command\":\"ls\"}","description":"list files","reason":"not safe","risk_level":"medium","source":"classifier","requires_snapshot":true}],"decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{\"command\":\"pwd\"}"}],"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"error":"failed"}`,
	"ApprovalToolDecision":         `{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{\"command\":\"pwd\"}"}`,
	"Command":                      `{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}}`,
	"CommandImageData":             `{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}`,
	"CommandZero":                  `{"id":"","sequence":0,"type":"","created_at":"0001-01-01T00:00:00Z"}`,
	"CommandsPage":                 `{"commands":[{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}}],"next_cursor":"7","next_offset":42}`,
	"CommandsPageZero":             `{"commands":null,"next_cursor":""}`,
	"ConversationBound":            `{"conversation_id":"conversation-1"}`,
	"ConversationBoundResult":      `{"conversation_id":"conversation-1","error_code":"conflict","error":"already bound"}`,
	"DisplayBarrier":               `{"id":"barrier-1"}`,
	"DisplayDelta":                 `{"before_input_id":"input-1","item_id":"item-1","role":"assistant","part_type":"text","delta":"hi","tool_call_id":"call-1","tool_name":"shell","subagent":{"tool_call_id":"spawn-1","name":"child","agent_type":"explore","workdir":"/w","description":"look","status":"running"}}`,
	"DisplayDeltaZero":             `{"item_id":"","role":"","part_type":"","delta":""}`,
	"FinishSessionRequest":         `{"error":"failed","error_code":"worker_failed","generation":"generation-1"}`,
	"FinishSessionRequestZero":     `{}`,
	"Idle":                         `{"command_sequence":7}`,
	"InputRequest":                 `{"request_id":"request-1","command":{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}},"steering_only":true,"start_only":true,"expected_turn_id":"turn-1","required_approval_policy":"auto","required_prompt_revision":9,"require_prompt_revision":true}`,
	"InputRequestZero":             `{"request_id":"","command":{"id":"","sequence":0,"type":"","created_at":"0001-01-01T00:00:00Z"}}`,
	"InputResult":                  `{"request_id":"request-1","command":{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}},"error_code":"conflict","error":"busy"}`,
	"InteractionOption":            `{"label":"Yes","description":"go ahead"}`,
	"InteractionQuestion":          `{"id":"q1","header":"Confirm","question":"Proceed?","is_other":true,"is_secret":true,"options":[{"label":"Yes","description":"go ahead"}]}`,
	"InteractionRequest":           `{"id":"interaction-1","session_id":"session-1","turn_id":"turn-1","item_id":"item-1","kind":"user_input","status":"resolved","requester":{"agent":"codex","thread_id":"thread-1","name":"child"},"questions":[{"id":"q1","header":"Confirm","question":"Proceed?","is_other":true,"is_secret":true,"options":[{"label":"Yes","description":"go ahead"}]}],"answers":{"q1":["Yes"]},"is_blocking":true,"auto_resolve_ms":1500,"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"}`,
	"InteractionRequestZero":       `{"id":"","session_id":"","turn_id":"","kind":"","status":"","requester":{},"created_at":"0001-01-01T00:00:00Z"}`,
	"InteractionRequester":         `{"agent":"codex","thread_id":"thread-1","name":"child"}`,
	"InteractionResponse":          `{"request_id":"request-1","interaction_id":"interaction-1","answers":{"q1":["Yes"]}}`,
	"InteractionResult":            `{"request_id":"request-1","interaction":{"id":"interaction-1","session_id":"session-1","turn_id":"turn-1","item_id":"item-1","kind":"user_input","status":"resolved","requester":{"agent":"codex","thread_id":"thread-1","name":"child"},"questions":[{"id":"q1","header":"Confirm","question":"Proceed?","is_other":true,"is_secret":true,"options":[{"label":"Yes","description":"go ahead"}]}],"answers":{"q1":["Yes"]},"is_blocking":true,"auto_resolve_ms":1500,"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"error":"failed"}`,
	"InterruptTurn":                `{"thread_id":"thread-1","turn_id":"turn-1"}`,
	"Message":                      `{"type":"command","command":{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}},"preview_retirement":{"item_id":"item-1","tool_call_ids":["call-1"],"terminal":true},"delta":{"before_input_id":"input-1","item_id":"item-1","role":"assistant","part_type":"text","delta":"hi","tool_call_id":"call-1","tool_name":"shell","subagent":{"tool_call_id":"spawn-1","name":"child","agent_type":"explore","workdir":"/w","description":"look","status":"running"}},"display_barrier":{"id":"barrier-1"},"approval_request":{"id":"approval-1","session_id":"session-1","turn_id":"turn-1","status":"resolved","pending":[{"tool_call_id":"call-1","tool_name":"shell","arguments":"{\"command\":\"ls\"}","description":"list files","reason":"not safe","risk_level":"medium","source":"classifier","requires_snapshot":true}],"decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{\"command\":\"pwd\"}"}],"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"approval_decision":{"request_id":"request-1","approval_id":"approval-1","decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{}"}],"stop_turn":true},"approval_result":{"request_id":"request-1","approval":{"id":"approval-1","session_id":"session-1","turn_id":"turn-1","status":"resolved","pending":[{"tool_call_id":"call-1","tool_name":"shell","arguments":"{\"command\":\"ls\"}","description":"list files","reason":"not safe","risk_level":"medium","source":"classifier","requires_snapshot":true}],"decisions":[{"tool_call_id":"call-1","approved":true,"reason":"ok","modified_args":"{\"command\":\"pwd\"}"}],"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"error":"failed"},"interaction_request":{"id":"interaction-1","session_id":"session-1","turn_id":"turn-1","item_id":"item-1","kind":"user_input","status":"resolved","requester":{"agent":"codex","thread_id":"thread-1","name":"child"},"questions":[{"id":"q1","header":"Confirm","question":"Proceed?","is_other":true,"is_secret":true,"options":[{"label":"Yes","description":"go ahead"}]}],"answers":{"q1":["Yes"]},"is_blocking":true,"auto_resolve_ms":1500,"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"interaction_response":{"request_id":"request-1","interaction_id":"interaction-1","answers":{"q1":["Yes"]}},"interaction_result":{"request_id":"request-1","interaction":{"id":"interaction-1","session_id":"session-1","turn_id":"turn-1","item_id":"item-1","kind":"user_input","status":"resolved","requester":{"agent":"codex","thread_id":"thread-1","name":"child"},"questions":[{"id":"q1","header":"Confirm","question":"Proceed?","is_other":true,"is_secret":true,"options":[{"label":"Yes","description":"go ahead"}]}],"answers":{"q1":["Yes"]},"is_blocking":true,"auto_resolve_ms":1500,"created_at":"2026-01-02T03:04:05.000000006Z","resolved_at":"2026-01-02T03:05:00Z"},"error":"failed"},"conversation_bound":{"conversation_id":"conversation-1"},"conversation_bound_result":{"conversation_id":"conversation-1","error_code":"conflict","error":"already bound"},"input_request":{"request_id":"request-1","command":{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}},"steering_only":true,"start_only":true,"expected_turn_id":"turn-1","required_approval_policy":"auto","required_prompt_revision":9,"require_prompt_revision":true},"input_result":{"request_id":"request-1","command":{"id":"command-1","sequence":7,"type":"message","created_at":"2026-01-02T03:04:05.000000006Z","message":{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"steering":{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"pause":{"turn_id":"turn-1","reason":"user_pause"},"profile":{"name":"fast"},"approval_policy":{"policy":"manual"}},"error_code":"conflict","error":"busy"},"interrupt_turn":{"thread_id":"thread-1","turn_id":"turn-1"},"idle":{"command_sequence":7},"runtime_tool_request":{"request_id":"request-1","namespace":"channel","turn_id":"turn-1","call_id":"call-1","name":"send","arguments":{"text":"hi"},"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}},"runtime_tool_result":{"request_id":"request-1","text":"done","error":"failed"}}`,
	"MessageCommandPayload":        `{"text":"hello","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}}`,
	"MessageZero":                  `{"type":"working"}`,
	"PauseCommandPayload":          `{"turn_id":"turn-1","reason":"user_pause"}`,
	"PendingApproval":              `{"tool_call_id":"call-1","tool_name":"shell","arguments":"{\"command\":\"ls\"}","description":"list files","reason":"not safe","risk_level":"medium","source":"classifier","requires_snapshot":true}`,
	"PreviewRetirement":            `{"item_id":"item-1","tool_call_ids":["call-1"],"terminal":true}`,
	"ProfileCommandPayload":        `{"name":"fast"}`,
	"ReadyRequest":                 `{"generation":"generation-1"}`,
	"RuntimeToolRequest":           `{"request_id":"request-1","namespace":"channel","turn_id":"turn-1","call_id":"call-1","name":"send","arguments":{"text":"hi"},"request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}}`,
	"RuntimeToolResult":            `{"request_id":"request-1","text":"done","error":"failed"}`,
	"SteeringCommandPayload":       `{"text":"steer","images":[{"mime_type":"image/png","size_bytes":12,"width":3,"height":4,"data_base64":"AAEC","blob_path":"blobs/one"}],"skills":["review"],"turn_id":"turn-1","request_context":{"source":"channel","connection_id":"connection-1","conversation_id":"conversation-1","actor":{"id":"","role":""}}}`,
}
