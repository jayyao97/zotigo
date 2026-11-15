package codexworker

import (
	"context"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestResumeUnboundCodexClearsChannelOverrides(t *testing.T) {
	rpc := &codexWorkerRPC{}
	err := resumeCodexThread(context.Background(), rpc, Config{
		ThreadID: "thread", WorkingDirectory: t.TempDir(), Model: "model", ChannelToolsVersion: channels.RuntimeToolsVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := rpc.requests["thread/resume"]
	if value, present := request["developerInstructions"]; !present || value != "" {
		t.Fatalf("developer instructions not explicitly cleared: %#v", request)
	}
	if request["approvalsReviewer"] != "user" {
		t.Fatalf("reviewer not cleared: %#v", request)
	}
}

func TestCodexThreadIdentityOnStartAndResume(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		params := codexThreadParams(Config{
			Config:      workerbase.Config{SessionID: "sess_child"},
			WorkspaceID: "workspace_current", DeveloperInstructions: "Existing instructions.",
		}, dynamic)
		text, _ := params["developerInstructions"].(string)
		for _, want := range []string{"Existing instructions.", "session_id: sess_child", "workspace_id: workspace_current"} {
			if !strings.Contains(text, want) {
				t.Fatalf("instructions missing %q: %s", want, text)
			}
		}
	}
}
