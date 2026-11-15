package zotigod

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

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
	results := make(chan wire.RuntimeToolResult, 1)
	runtime := &codexWorkerRuntime{cfg: cfg, app: rpc, threadID: "thread", activeTurnID: "turn", writer: workerbase.NewUnstartedWriter(nil, 1), runtimeToolResults: results}
	done := make(chan error, 1)
	go func() {
		done <- runtime.executeCodexRuntimeTool(context.Background(), codexapp.Message{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","callId":"call","namespace":"zotigo","tool":"list_sessions","arguments":{}}`)})
	}()
	message := <-runtime.writer.Queue()
	if message.RuntimeToolRequest.Namespace != "zotigo" || message.RuntimeToolRequest.TurnID != "turn" || message.RuntimeToolRequest.CallID != "call" {
		t.Fatalf("request=%+v", message)
	}
	results <- wire.RuntimeToolResult{RequestID: "call", Text: `{"sessions":[]}`}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rpc.response.(map[string]any)["success"] != true {
		t.Fatalf("response=%v", rpc.response)
	}
}
