package hooks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDispatchMatcherORRunsHandlerOnce(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PreToolUse: {{Command: "check", Matchers: []string{"shell", "*"}}},
	}})
	count := 0
	dispatcher.execute = func(context.Context, Handler, Event) processResult {
		count++
		return processResult{started: true, exitCode: 0, stdout: `{"decision":"allow"}`}
	}

	event := testToolEvent(PreToolUse, "shell")
	result := dispatcher.Dispatch(context.Background(), event)
	if result.Denied || count != 1 {
		t.Fatalf("handler should run once: result=%#v count=%d", result, count)
	}
}

func TestDispatchDenialStopsLaterHandlers(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PreToolUse: {{Command: "deny"}, {Command: "must-not-run"}},
	}})
	var commands []string
	dispatcher.execute = func(_ context.Context, handler Handler, _ Event) processResult {
		commands = append(commands, handler.Command)
		return processResult{started: true, exitCode: 0, stdout: `{"decision":"deny","reason":"blocked"}`}
	}

	result := dispatcher.Dispatch(context.Background(), testToolEvent(PreToolUse, "shell"))
	if !result.Denied || result.Reason != "blocked" || len(commands) != 1 {
		t.Fatalf("unexpected denial: result=%#v commands=%v", result, commands)
	}
}

func TestDispatchExitTwoDenies(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PreToolUse: {{Command: "deny"}},
	}})
	dispatcher.execute = func(context.Context, Handler, Event) processResult {
		return processResult{started: true, exitCode: 2, stderr: "policy denied\n\ndetails", err: context.Canceled}
	}

	result := dispatcher.Dispatch(context.Background(), testToolEvent(PreToolUse, "shell"))
	if !result.Denied || result.Reason != "policy denied" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestExecuteCommandProtocolAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.json")
	envPath := filepath.Join(dir, "env.txt")
	handler := Handler{
		Command: "/bin/sh",
		Args:    []string{"-c", `cat > "$1"; printf '%s|%s' "$ZOTIGO_SESSION_ID" "$ZOTIGO_HOOK_EVENT" > "$2"; printf '{"decision":"allow"}'`, "hook", inputPath, envPath},
	}
	event := testToolEvent(PreToolUse, "shell")
	event.CWD = dir
	result := executeCommand(context.Background(), handler, event)
	if result.err != nil || result.stdout != `{"decision":"allow"}` {
		t.Fatalf("unexpected process result: %#v", result)
	}
	payload, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"event_name":"PreToolUse"`) || !strings.Contains(string(payload), `"name":"shell"`) {
		t.Fatalf("unexpected stdin payload: %s", payload)
	}
	environment, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(environment) != "sess-test|PreToolUse" {
		t.Fatalf("unexpected environment: %s", environment)
	}
}

func TestExecuteCommandTimeoutKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := executeCommand(ctx, Handler{Command: "/bin/sh", Args: []string{"-c", "sleep 5"}}, Event{
		EventName: PreToolUse, SessionID: "sess-test", Agent: "zotigo", CWD: t.TempDir(),
	})
	if !result.timedOut {
		t.Fatalf("expected timeout: %#v", result)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("process group was not terminated promptly: %v", time.Since(started))
	}
}

func TestAsyncDispatchUsesBoundedWorker(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PostToolUse: {{Command: "audit", Async: true}},
	}})
	var wait sync.WaitGroup
	wait.Add(1)
	dispatcher.execute = func(context.Context, Handler, Event) processResult {
		wait.Done()
		return processResult{started: true}
	}
	dispatcher.Dispatch(context.Background(), testToolEvent(PostToolUse, "shell"))
	done := make(chan struct{})
	go func() { wait.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("async handler did not run")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCancelsRunningAsyncHandler(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PostToolUse: {{Command: "audit", Async: true}},
	}})
	started := make(chan struct{})
	dispatcher.execute = func(ctx context.Context, _ Handler, _ Event) processResult {
		close(started)
		<-ctx.Done()
		return processResult{started: true, cancelled: true, err: ctx.Err()}
	}
	dispatcher.Dispatch(context.Background(), testToolEvent(PostToolUse, "shell"))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async handler did not start")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(closeCtx); err != nil {
		t.Fatalf("close dispatcher: %v", err)
	}
}

func testToolEvent(name EventName, toolName string) Event {
	event := NewEvent(name, "sess-test", "zotigo", "/tmp")
	event.TurnID = "turn-test"
	event.Tool = &ToolPayload{CallID: "call-test", Name: toolName, NativeName: toolName, Input: map[string]any{}}
	return event
}
