package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/channels"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type fakeCOTAPI struct {
	createReceipt channels.DeliveryReceipt
	createErr     error
	appended      [][]*larkim.MessageCot
	completed     []string
}

func (f *fakeCOTAPI) Create(context.Context, channels.InboundMessage) (channels.DeliveryReceipt, error) {
	return f.createReceipt, f.createErr
}
func (f *fakeCOTAPI) Append(_ context.Context, _ channels.DeliveryReceipt, events []*larkim.MessageCot) error {
	f.appended = append(f.appended, events)
	return nil
}
func (f *fakeCOTAPI) Complete(_ context.Context, _ channels.DeliveryReceipt, reason string) error {
	f.completed = append(f.completed, reason)
	return nil
}

func TestCOTClientUsesDocumentedCreateContract(t *testing.T) {
	client := &cotClient{do: func(_ context.Context, req *larkcore.ApiReq) (*larkcore.ApiResp, error) {
		if req.HttpMethod != http.MethodPost || req.ApiPath != cotEndpoint || req.QueryParams.Get("receive_id_type") != "chat_id" {
			t.Fatalf("request=%+v", req)
		}
		body := req.Body.(map[string]any)
		if body["receive_id"] != "oc_test" || body["origin_message_id"] != "om_test" || body["reply_in_thread"] != true || body["enable_badge"] != false || body["update_feed_rank"] != false || body["cot_hidden"] != false {
			t.Fatalf("body=%+v", body)
		}
		return &larkcore.ApiResp{StatusCode: http.StatusOK, RawBody: []byte(`{"code":0,"msg":"ok","data":{"cot_id":"cot-1","message_id":"om-cot"}}`)}, nil
	}}
	receipt, err := client.Create(context.Background(), channels.InboundMessage{ChatID: "oc_test", MessageID: "om_test"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Mode != channels.ProgressModeCOT || receipt.COTID != "cot-1" || receipt.MessageID != "om-cot" {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestCOTClientBatchesAppendAndUsesDocumentedCompleteContract(t *testing.T) {
	calls := 0
	client := &cotClient{do: func(_ context.Context, req *larkcore.ApiReq) (*larkcore.ApiResp, error) {
		calls++
		switch calls {
		case 1, 2:
			if req.HttpMethod != http.MethodPut || req.ApiPath != cotEndpoint {
				t.Fatalf("append request=%+v", req)
			}
			body := req.Body.(map[string]any)
			events := body["events"].([]*larkim.MessageCot)
			want := 50
			if calls == 2 {
				want = 1
			}
			if len(events) != want || body["message_id"] != "om-cot" || body["cot_id"] != "cot-1" {
				t.Fatalf("append body=%+v", body)
			}
		case 3:
			if req.HttpMethod != http.MethodPost || req.ApiPath != cotEndpoint+"/complete/:cot_id" || req.PathParams.Get("cot_id") != "cot-1" || req.QueryParams.Get("message_id") != "om-cot" || req.QueryParams.Get("reason") != "error" {
				t.Fatalf("complete request=%+v", req)
			}
		default:
			t.Fatalf("unexpected call %d", calls)
		}
		return &larkcore.ApiResp{StatusCode: http.StatusOK, RawBody: []byte(`{"code":0,"msg":"ok","data":{}}`)}, nil
	}}
	receipt := channels.DeliveryReceipt{Mode: channels.ProgressModeCOT, MessageID: "om-cot", COTID: "cot-1"}
	events := make([]*larkim.MessageCot, 51)
	for index := range events {
		events[index] = larkim.NewMessageCotBuilder().EventType("RUN_STARTED").Content(`{}`).Timestamp("1").Build()
	}
	if err := client.Append(context.Background(), receipt, events); err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(context.Background(), receipt, "error"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestCOTRendererDoesNotExposeToolArgumentsOrResults(t *testing.T) {
	events, err := renderCOTEvents("thread-1", "run-1", []channels.PublicExecutionEvent{
		{Type: channels.ExecutionRunStarted, Timestamp: time.Unix(1, 0)},
		{Type: channels.ExecutionToolStarted, Tool: &channels.PublicToolActivity{CallID: "call-1", Kind: "shell", DisplayName: "Run command"}},
		{Type: channels.ExecutionToolFinished, Tool: &channels.PublicToolActivity{CallID: "call-1", Kind: "shell", DisplayName: "Run command"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || *events[1].EventType != "TOOL_CALL_START" || *events[2].EventType != "TOOL_CALL_END" || *events[3].EventType != "TOOL_CALL_RESULT" {
		t.Fatalf("events=%+v", events)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "arguments") || strings.Contains(string(encoded), "result") {
		t.Fatalf("private tool data appeared in events: %s", encoded)
	}
}

func TestAutoFallsBackOnlyOnDefiniteCOTRejection(t *testing.T) {
	for _, test := range []struct {
		name      string
		createErr error
		wantReply int
		wantError bool
	}{
		{name: "platform rejection", createErr: &cotRejectedError{code: 230001, msg: "not enabled"}, wantReply: 1},
		{name: "ambiguous transport error", createErr: errors.New("connection reset"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			replies := 0
			adapter := &Adapter{
				connection: channels.Connection{ProgressMode: channels.ProgressModeAuto},
				cot:        &fakeCOTAPI{createErr: test.createErr},
				reply: func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
					replies++
					return "card-1", nil
				},
				patch: func(context.Context, string, string) error { return nil },
			}
			_, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "om-1", ChatID: "oc-1"})
			if (err != nil) != test.wantError || replies != test.wantReply {
				t.Fatalf("err=%v replies=%d", err, replies)
			}
		})
	}
}

func TestAutoDoesNotFallbackOnStructuredServerError(t *testing.T) {
	replies := 0
	api := &cotClient{do: func(context.Context, *larkcore.ApiReq) (*larkcore.ApiResp, error) {
		return &larkcore.ApiResp{StatusCode: http.StatusServiceUnavailable, RawBody: []byte(`{"code":230001,"msg":"temporarily unavailable"}`)}, nil
	}}
	adapter := &Adapter{
		connection: channels.Connection{ProgressMode: channels.ProgressModeAuto},
		cot:        api,
		reply: func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
			replies++
			return "card-1", nil
		},
		patch: func(context.Context, string, string) error { return nil },
	}
	if _, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "om-1", ChatID: "oc-1"}); err == nil {
		t.Fatal("expected ambiguous server error")
	}
	if replies != 0 {
		t.Fatalf("server error created fallback card %d times", replies)
	}
}

func TestAutoDoesNotFallbackOnSuccessfulHTTPBusinessError(t *testing.T) {
	replies := 0
	api := &cotClient{do: func(context.Context, *larkcore.ApiReq) (*larkcore.ApiResp, error) {
		return &larkcore.ApiResp{StatusCode: http.StatusOK, RawBody: []byte(`{"code":230001,"msg":"not enabled"}`)}, nil
	}}
	adapter := &Adapter{
		connection: channels.Connection{ProgressMode: channels.ProgressModeAuto},
		cot:        api,
		reply: func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
			replies++
			return "card-1", nil
		},
		patch: func(context.Context, string, string) error { return nil },
	}
	_, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "om-1", ChatID: "oc-1"})
	if err == nil || !channels.IsUncertainDelivery(err) {
		t.Fatalf("expected uncertain delivery, got %v", err)
	}
	if replies != 0 {
		t.Fatalf("business error created fallback card %d times", replies)
	}
}

func TestRequestPacerHonorsContextCancellation(t *testing.T) {
	pace := newRequestPacer(time.Hour)
	if err := pace(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := pace(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("pacer ignored canceled context")
	}
}

func TestCOTRequestIntervalStaysWithinDocumentedQuotas(t *testing.T) {
	if cotRequestInterval < time.Second/50 {
		t.Fatalf("COT interval %s exceeds 50 requests/second", cotRequestInterval)
	}
	if cotRequestInterval < time.Minute/1000 {
		t.Fatalf("COT interval %s exceeds 1000 requests/minute", cotRequestInterval)
	}
}

func TestApprovalEventsKeepDistinctStableStepIDs(t *testing.T) {
	events, err := renderCOTEvents("thread-1", "run-1", []channels.PublicExecutionEvent{
		{ID: "request-1", CorrelationID: "approval-1", Type: channels.ExecutionApproval},
		{ID: "decision-1", CorrelationID: "approval-1", Type: channels.ExecutionApprovalDone},
		{ID: "request-2", CorrelationID: "approval-2", Type: channels.ExecutionApproval},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events=%+v", events)
	}
	var content [3]map[string]any
	for index := range events {
		if err := json.Unmarshal([]byte(*events[index].Content), &content[index]); err != nil {
			t.Fatal(err)
		}
	}
	if content[0]["stepId"] != "approval-1" || content[1]["stepId"] != "approval-1" || content[2]["stepId"] != "approval-2" {
		t.Fatalf("content=%+v", content)
	}
}

func TestCOTCompletionSendsStrictFinalReplyThenFinishesRun(t *testing.T) {
	api := &fakeCOTAPI{createReceipt: channels.DeliveryReceipt{Mode: channels.ProgressModeCOT, MessageID: "cot-message", COTID: "cot-1"}}
	var replyTarget, replyUUID string
	handle, err := openCOTProgress(context.Background(), api, func(_ context.Context, target, text, uuid string) (string, error) {
		replyTarget, replyUUID = target, uuid
		if text != "answer" {
			t.Fatalf("text=%q", text)
		}
		return "final-1", nil
	}, channels.InboundMessage{MessageID: "origin-1", ChatID: "oc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Update(context.Background(), channels.Progress{Events: []channels.PublicExecutionEvent{{ID: "started", Type: channels.ExecutionRunStarted}}}); err != nil {
		t.Fatal(err)
	}
	var finalID string
	err = handle.Complete(context.Background(), channels.TaskResult{Text: "answer"}, func(id string) error {
		finalID = id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalID != "final-1" || replyTarget != "origin-1" || replyUUID != "zotigo-final-origin-1" {
		t.Fatalf("final=%q target=%q uuid=%q", finalID, replyTarget, replyUUID)
	}
	if len(api.appended) != 2 || len(api.appended[0]) != 1 || *api.appended[0][0].EventType != "RUN_STARTED" || len(api.appended[1]) != 1 || *api.appended[1][0].EventType != "RUN_FINISHED" || len(api.completed) != 0 {
		t.Fatalf("appended=%+v completed=%v", api.appended, api.completed)
	}
	for _, event := range []*larkim.MessageCot{api.appended[0][0], api.appended[1][0]} {
		var content map[string]any
		if err := json.Unmarshal([]byte(*event.Content), &content); err != nil {
			t.Fatal(err)
		}
		if content["threadId"] != "cot-1" || content["runId"] != "run-cot-1" {
			t.Fatalf("lifecycle identity=%+v", content)
		}
	}
}

func TestCOTRecoveryUsesPersistedFinalReceiptToChooseTerminalState(t *testing.T) {
	receipt := channels.DeliveryReceipt{Mode: channels.ProgressModeCOT, MessageID: "cot-message", COTID: "cot-1"}
	completedAPI := &fakeCOTAPI{}
	completed := resumeCOTProgress(completedAPI, nil, receipt)
	if err := completed.Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(completedAPI.appended) != 1 || *completedAPI.appended[0][0].EventType != "RUN_FINISHED" || len(completedAPI.completed) != 0 {
		t.Fatalf("completed recovery appended=%+v complete=%v", completedAPI.appended, completedAPI.completed)
	}
	var recoveredIdentity map[string]any
	if err := json.Unmarshal([]byte(*completedAPI.appended[0][0].Content), &recoveredIdentity); err != nil {
		t.Fatal(err)
	}
	if recoveredIdentity["threadId"] != "cot-1" || recoveredIdentity["runId"] != "run-cot-1" {
		t.Fatalf("recovered identity=%+v", recoveredIdentity)
	}

	failedAPI := &fakeCOTAPI{}
	failed := resumeCOTProgress(failedAPI, nil, receipt)
	if err := failed.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(failedAPI.appended) != 1 || *failedAPI.appended[0][0].EventType != "RUN_ERROR" || len(failedAPI.completed) != 1 || failedAPI.completed[0] != "error" {
		t.Fatalf("failed recovery appended=%+v complete=%v", failedAPI.appended, failedAPI.completed)
	}
}
