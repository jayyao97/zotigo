package codexapp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHostCannotRestartAfterClose(t *testing.T) {
	host, err := NewHost("/usr/bin/false", t.TempDir(), io.Discard, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Ensure after Close error = %v", err)
	}
}

func TestHostStopsAfterLastLeaseAndRestarts(t *testing.T) {
	host := newTestHost(t, true)
	first, err := host.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := testHostStarts(t); got != 1 {
		t.Fatalf("host starts = %d, want 1", got)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if !testHostRunning(host) {
		t.Fatal("host stopped while another lease was active")
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if testHostRunning(host) {
		t.Fatal("host remained running after its final lease")
	}

	third, err := host.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := testHostStarts(t); got != 2 {
		t.Fatalf("host starts after reacquire = %d, want 2", got)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestHostCanKeepServerWarmAfterLastLease(t *testing.T) {
	host := newTestHost(t, false)
	lease, err := host.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !testHostRunning(host) {
		t.Fatal("host stopped with StopWhenIdle disabled")
	}
}

func TestHostCloseAndLateReleaseDoNotDeadlockOrRestart(t *testing.T) {
	host := newTestHost(t, true)
	lease, err := host.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- lease.Release() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("late lease release deadlocked")
	}
	if testHostRunning(host) {
		t.Fatal("closed host restarted after late release")
	}
	if _, err := host.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Acquire after Close error = %v", err)
	}
}

func newTestHost(t *testing.T, stopWhenIdle bool) *Host {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("/tmp", "zth-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	startsPath := filepath.Join(runtimeDir, "starts")
	t.Setenv("ZOTIGO_HOST_HELPER", "1")
	t.Setenv("ZOTIGO_HOST_HELPER_STARTS", startsPath)
	t.Setenv("ZOTIGO_HOST_HELPER_BINARY", os.Args[0])
	scriptPath := filepath.Join(runtimeDir, "codex-test-helper")
	script := "#!/bin/sh\nexec \"$ZOTIGO_HOST_HELPER_BINARY\" -test.run=^TestHostHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := NewHost(scriptPath, runtimeDir, io.Discard, HostOptions{StopWhenIdle: stopWhenIdle})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

func testHostRunning(host *Host) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.command != nil
}

func testHostStarts(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("ZOTIGO_HOST_HELPER_STARTS"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start\n")
}

func TestHostHelperProcess(t *testing.T) {
	if os.Getenv("ZOTIGO_HOST_HELPER") != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || len(os.Args) <= separator+3 || os.Args[separator+1] != "app-server" || os.Args[separator+2] != "--listen" {
		t.Fatalf("unexpected helper args: %q", os.Args)
	}
	const prefix = "unix://"
	listenArg := os.Args[separator+3]
	if !strings.HasPrefix(listenArg, prefix) {
		t.Fatalf("unexpected listen arg: %q", listenArg)
	}
	starts, err := os.OpenFile(os.Getenv("ZOTIGO_HOST_HELPER_STARTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := starts.WriteString("start\n"); err != nil {
		t.Fatal(err)
	}
	_ = starts.Close()

	listener, err := net.Listen("unix", strings.TrimPrefix(listenArg, prefix))
	if err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request map[string]json.RawMessage
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			id := request["id"]
			if len(id) == 0 {
				continue
			}
			response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"userAgent": "codex-test"}}
			if err := conn.WriteJSON(response); err != nil {
				return
			}
		}
	})}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { _ = server.Serve(listener) }()
	<-ctx.Done()
	_ = server.Close()
}
