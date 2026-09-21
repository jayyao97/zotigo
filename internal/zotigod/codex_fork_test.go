package zotigod

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestForkCodexConversationExactBoundary(t *testing.T) {
	for _, test := range []struct {
		name, childID, lastID, status, wantError string
		forkError                                error
	}{
		{name: "historical boundary", childID: "child", lastID: "turn-2", status: "completed"},
		{name: "ignored boundary", childID: "child", lastID: "turn-3", status: "completed", wantError: "boundary mismatch"},
		{name: "active boundary", childID: "child", lastID: "turn-2", status: "inProgress", wantError: "boundary mismatch"},
		{name: "source returned", childID: "source", wantError: "distinct child"},
		{name: "empty response", wantError: "distinct child"},
		{name: "uncertain dispatch", forkError: errors.New("connection closed"), wantError: "connection closed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &zotigosession.Session{Metadata: zotigosession.Metadata{
				ConversationID: "source", WorkingDirectory: "/workspace", Model: "test-model",
				ApprovalPolicy: agent.ApprovalPolicyAuto,
				Capabilities:   zotigosession.Capabilities{ChannelToolsVersion: 1, SessionToolsVersion: 1},
				PromptConfig:   zotigosession.PromptConfig{Revision: 1, AgentInstructions: "restricted", ReviewAllTools: true},
			}}
			original := *source
			var methods []string
			host := &codexSyncHost{rpc: codexSyncFuncRPC{call: func(_ context.Context, method string, raw any, result any) error {
				methods = append(methods, method)
				params := raw.(map[string]any)
				var payload any
				switch method {
				case "thread/fork":
					if params["threadId"] != "source" || params["lastTurnId"] != "turn-2" || params["deferGoalContinuation"] != true || params["excludeTurns"] != true {
						t.Fatalf("unsafe fork boundary params: %#v", params)
					}
					if params["approvalPolicy"] != "on-request" || params["approvalsReviewer"] != "auto_review" || params["cwd"] != "/workspace" || !strings.Contains(params["developerInstructions"].(string), "restricted") {
						t.Fatalf("source policy not preserved: %#v", params)
					}
					if _, ok := params["dynamicTools"]; ok {
						t.Fatal("fork must inherit dynamic tools, not use unsupported parameter")
					}
					if test.forkError != nil {
						return test.forkError
					}
					payload = codexThreadSnapshot{Thread: codexThread{ID: test.childID}}
				case "thread/turns/list":
					if params["threadId"] != "child" || params["limit"] != 1 || params["sortDirection"] != "desc" || params["itemsView"] != "notLoaded" {
						t.Fatalf("verification must only read child tail: %#v", params)
					}
					payload = codexTurnList{Data: []codexTurn{{ID: test.lastID, Status: test.status}}}
				default:
					t.Fatalf("unexpected mutation %q", method)
				}
				encoded, err := sonic.Marshal(payload)
				if err != nil {
					return err
				}
				return sonic.Unmarshal(encoded, result)
			}}}
			childID, err := forkCodexConversation(context.Background(), host, source, "turn-2")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || childID != "" {
					t.Fatalf("result = %q, %v", childID, err)
				}
			} else if err != nil || childID != "child" {
				t.Fatalf("result = %q, %v", childID, err)
			}
			if !reflect.DeepEqual(source, &original) {
				t.Fatal("source metadata was changed")
			}
			if len(methods) > 2 {
				t.Fatalf("fork must not retry: %v", methods)
			}
		})
	}
}

func TestForkCodexConversationRequiresBoundary(t *testing.T) {
	host := &codexSyncHost{}
	if _, err := forkCodexConversation(context.Background(), host, &zotigosession.Session{}, ""); err == nil || host.acquires != 0 {
		t.Fatalf("invalid source invoked host: %v", err)
	}
}
