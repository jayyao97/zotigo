package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestCodexUserInputRequestRoundTrip(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, store := newCodexInputTestRuntime(t, "session-question", rpc)
	runtime.interactions = make(map[string]codexPendingInteraction)
	runtime.writer = workerbase.NewUnstartedWriter(nil, 1)
	request := codexapp.Message{
		ID: json.RawMessage(`41`), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"child-thread","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"choice","header":"Mode","question":"Choose","isOther":false,"isSecret":false,"options":[{"label":"Safe","description":"Recommended"}]}]}`),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.handleServerRequest(context.Background(), request) }()
	message := <-runtime.writer.Queue()
	if message.Type == wire.MessageDisplayWake {
		message = <-runtime.writer.Queue()
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if message.InteractionRequest == nil || message.InteractionRequest.Requester.Name != "Subagent" || len(message.InteractionRequest.Questions) != 1 || message.InteractionRequest.Questions[0].Options[0].Label != "Safe" {
		t.Fatalf("interaction request = %#v", message.InteractionRequest)
	}
	result, fatalErr := runtime.resolveInteraction(wire.InteractionResponse{
		RequestID: "submit-1", InteractionID: message.InteractionRequest.ID,
		Answers: map[string][]string{"choice": {"Safe"}},
	})
	if fatalErr != nil || result.Error != "" || rpc.responseID != "41" {
		t.Fatalf("result=%#v responseID=%q", result, rpc.responseID)
	}
	want := map[string]any{"answers": map[string]any{"choice": map[string]any{"answers": []string{"Safe"}}}}
	if !reflect.DeepEqual(rpc.response, want) {
		t.Fatalf("response = %#v", rpc.response)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-question")
	if err != nil || len(items) != 2 || items[0].Interaction == nil || items[1].Interaction == nil || items[1].Interaction.Status != sessionlog.InteractionStatusResolved {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
}

func TestCodexSubagentApprovalRoundTrip(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, store := newCodexInputTestRuntime(t, "session-approval", rpc)
	runtime.approvals = make(map[string]codexPendingApproval)
	runtime.writer = workerbase.NewUnstartedWriter(nil, 1)
	request := codexapp.Message{
		ID: json.RawMessage(`52`), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"child-thread","turnId":"turn-1","itemId":"call-1","reason":"writes files","command":"touch file","cwd":"/tmp"}`),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.handleServerRequest(context.Background(), request) }()
	message := <-runtime.writer.Queue()
	if message.Type == wire.MessageDisplayWake {
		message = <-runtime.writer.Queue()
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if message.ApprovalRequest == nil || message.ApprovalRequest.Pending[0].Source != "codex_subagent:child-thread" {
		t.Fatalf("approval request = %#v", message.ApprovalRequest)
	}
	result, fatalErr := runtime.resolveCodexApproval(wire.ApprovalDecision{
		RequestID: "submit-2", ApprovalID: message.ApprovalRequest.ID,
		Decisions: []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: false, Reason: "not needed"}},
	})
	if fatalErr != nil || result.Error != "" || rpc.responseID != "52" || !reflect.DeepEqual(rpc.response, map[string]any{"decision": "cancel"}) {
		t.Fatalf("result=%#v responseID=%q response=%#v", result, rpc.responseID, rpc.response)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-approval")
	if err != nil || len(items) != 2 || items[1].Approval == nil || items[1].Approval.Decisions[0].Reason != "not needed" {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
}

func TestCodexCommandApprovalPreservesAuthorityAndHonorsOfferedDecision(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, _ := newCodexInputTestRuntime(t, "session-network-approval", rpc)
	runtime.approvals = make(map[string]codexPendingApproval)
	runtime.writer = workerbase.NewUnstartedWriter(nil, 1)
	request := codexapp.Message{
		ID: json.RawMessage(`53`), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{
			"threadId":"thread-1","turnId":"turn-1","itemId":"call-network","kind":"command","environmentId":"local",
			"reason":"connect to the service","networkApprovalContext":{"host":"example.com","protocol":"https"},
			"additionalPermissions":{"network":{"enabled":true},"fileSystem":{"read":["/workspace/shared"]}},
			"availableDecisions":["accept","decline"]
		}`),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.handleServerRequest(context.Background(), request) }()
	message := <-runtime.writer.Queue()
	if message.Type == wire.MessageDisplayWake {
		message = <-runtime.writer.Queue()
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	pending := message.ApprovalRequest.Pending[0]
	var details map[string]json.RawMessage
	if err := json.Unmarshal([]byte(pending.Arguments), &details); err != nil {
		t.Fatalf("decode displayed authority: %v", err)
	}
	for _, field := range []string{"kind", "environment_id", "network_approval_context", "additional_permissions"} {
		if len(details[field]) == 0 {
			t.Fatalf("displayed authority omitted %s: %s", field, pending.Arguments)
		}
	}
	result, fatalErr := runtime.resolveCodexApproval(wire.ApprovalDecision{
		RequestID: "submit-network", ApprovalID: message.ApprovalRequest.ID,
		Decisions: []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-network", Approved: false}},
	})
	if fatalErr != nil || result.Error != "" || !reflect.DeepEqual(rpc.response, map[string]any{"decision": "decline"}) {
		t.Fatalf("result=%#v response=%#v", result, rpc.response)
	}
}

func TestCodexPermissionApprovalRoundTrip(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprint(approved), func(t *testing.T) {
			rpc := &codexWorkerRPC{}
			runtime, _ := newCodexInputTestRuntime(t, "session-permissions", rpc)
			runtime.approvals = make(map[string]codexPendingApproval)
			runtime.writer = workerbase.NewUnstartedWriter(nil, 1)
			request := codexapp.Message{
				ID: json.RawMessage(`61`), Method: "item/permissions/requestApproval",
				Params: json.RawMessage(`{"threadId":"child-thread","turnId":"turn-1","itemId":"call-1","cwd":"/workspace","reason":"write shared files","permissions":{"network":null,"fileSystem":{"write":["/workspace/shared"]}}}`),
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runtime.handleServerRequest(context.Background(), request) }()
			message := <-runtime.writer.Queue()
			if message.Type == wire.MessageDisplayWake {
				message = <-runtime.writer.Queue()
			}
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
			result, fatalErr := runtime.resolveCodexApproval(wire.ApprovalDecision{
				RequestID: "submit", ApprovalID: message.ApprovalRequest.ID,
				Decisions: []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: approved}},
			})
			if fatalErr != nil || result.Error != "" {
				t.Fatal(result.Error)
			}
			encoded, err := json.Marshal(rpc.response)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Permissions map[string]json.RawMessage `json:"permissions"`
				Scope       string                     `json:"scope"`
			}
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			if response.Scope != "turn" || (approved && response.Permissions["fileSystem"] == nil) || (!approved && len(response.Permissions) != 0) {
				t.Fatalf("permission response = %s", encoded)
			}
		})
	}
}

func TestCodexRequestDeliveryFailureRecordsTerminalState(t *testing.T) {
	for _, method := range []string{"item/tool/requestUserInput", "item/commandExecution/requestApproval"} {
		t.Run(method, func(t *testing.T) {
			runtime, store := newCodexInputTestRuntime(t, "session-delivery", &codexWorkerRPC{})
			runtime.interactions = make(map[string]codexPendingInteraction)
			runtime.approvals = make(map[string]codexPendingApproval)
			runtime.writer = workerbase.NewUnstartedWriter(nil, 0)
			runtime.writer.Close()
			params := `{"threadId":"thread-1","turnId":"turn-1","itemId":"call-1","command":"touch file","cwd":"/tmp"}`
			if method == "item/tool/requestUserInput" {
				params = `{"threadId":"thread-1","turnId":"turn-1","itemId":"call-1","questions":[{"id":"q","question":"Answer?"}]}`
			}
			if err := runtime.handleServerRequest(context.Background(), codexapp.Message{ID: json.RawMessage(`7`), Method: method, Params: json.RawMessage(params)}); err == nil {
				t.Fatal("closed worker writer accepted request")
			}
			items, _, err := store.ListDisplayItems(context.Background(), "session-delivery")
			if err != nil || len(items) != 2 || (items[1].Type != zotigosession.DisplayItemInteractionResponse && items[1].Type != zotigosession.DisplayItemApprovalDecision) {
				t.Fatalf("terminal display items = %#v, err=%v", items, err)
			}
		})
	}
}

func TestCodexRequestDeliveryAndTerminalFailureIsFatal(t *testing.T) {
	requests := []codexapp.Message{
		{ID: json.RawMessage(`7`), Method: "item/tool/requestUserInput", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"call-1","questions":[{"id":"q","question":"Answer?"}]}`)},
		{ID: json.RawMessage(`8`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"call-1","command":"touch file"}`)},
	}
	for index, request := range requests {
		runtime, _ := newCodexInputTestRuntime(t, fmt.Sprintf("session-fatal-delivery-%d", index), &codexWorkerRPC{})
		runtime.store = &failTerminalAppendStore{Store: runtime.store}
		runtime.interactions = make(map[string]codexPendingInteraction)
		runtime.approvals = make(map[string]codexPendingApproval)
		runtime.writer = workerbase.NewUnstartedWriter(nil, 0)
		runtime.writer.Close()
		err := runtime.handleServerRequest(context.Background(), request)
		var fatal *codexRequestFatalError
		if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "append terminal state") {
			t.Fatalf("%s delivery failure = %v", request.Method, err)
		}
	}
}

func TestCodexResolvedRequestPersistenceFailureCannotBeAnsweredTwice(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, _ := newCodexInputTestRuntime(t, "session-resolve-fatal", rpc)
	runtime.interactions = make(map[string]codexPendingInteraction)
	req, err := sessionlog.NewUserInputInteraction("session-resolve-fatal", "turn", "item", wire.InteractionRequester{Agent: "codex"}, []wire.InteractionQuestion{{ID: "q", Question: "Answer?"}}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.interactions[req.ID] = codexPendingInteraction{request: req, rpcID: json.RawMessage(`9`)}
	runtime.store = &failTerminalAppendStore{Store: runtime.store, appends: 1}
	response := wire.InteractionResponse{RequestID: "submit", InteractionID: req.ID, Answers: map[string][]string{"q": {"user_note: yes"}}}
	result, fatalErr := runtime.resolveInteraction(response)
	if fatalErr == nil || result.Error == "" || rpc.responseCalls != 1 {
		t.Fatalf("first resolution result=%#v fatal=%v calls=%d", result, fatalErr, rpc.responseCalls)
	}
	result, fatalErr = runtime.resolveInteraction(response)
	if fatalErr != nil || result.Error != "interaction is not pending" || rpc.responseCalls != 1 {
		t.Fatalf("retry result=%#v fatal=%v calls=%d", result, fatalErr, rpc.responseCalls)
	}
}

type failTerminalAppendStore struct {
	zotigosession.Store
	appends int
}

func (s *failTerminalAppendStore) AppendDisplayItem(ctx context.Context, id string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	s.appends++
	if s.appends > 1 {
		return zotigosession.DisplayItem{}, errors.New("append terminal state")
	}
	return s.Store.AppendDisplayItem(ctx, id, item)
}
