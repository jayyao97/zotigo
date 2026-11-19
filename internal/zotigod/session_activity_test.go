package zotigod

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActivityStreamTracksTurnAndReconcilesOnReconnect(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "one", State: SessionStateRunning})
	server := httptest.NewServer(newHandler(registry, nil))
	defer server.Close()
	connect := func() (*bufio.Reader, func()) {
		resp, err := server.Client().Get(server.URL + "/catalog/activity")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("activity stream returned %s", resp.Status)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}
	read := func(r *bufio.Reader) sessionActivitySnapshot {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var snap sessionActivitySnapshot
				if err := json.Unmarshal([]byte(data), &snap); err != nil {
					t.Fatal(err)
				}
				return snap
			}
		}
	}
	r, closeStream := connect()
	defer closeStream()
	if len(read(r).Active) != 0 {
		t.Fatal("idle worker appeared active")
	}
	registry.MarkWorking("one", "")
	started := read(r)
	if started.Active["one"] != SessionStateRunning {
		t.Fatal(started)
	}
	registry.MarkIdle("one")
	if idle := read(r); len(idle.Active) != 0 {
		t.Fatal(idle)
	}
	registry.MarkWorking("one", "shell")
	if working := read(r); working.Active["one"] != SessionStateRunning {
		t.Fatal(working)
	}
	closeStream()
	registry.MarkIdle("one")
	r, closeNext := connect()
	defer closeNext()
	ended := read(r)
	if ended.Revision <= started.Revision || len(ended.Active) != 0 {
		t.Fatal(ended)
	}
}

func TestStoppingDaemonClosesActivityStream(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	h := newHandler(newSessionRegistry(), nil, handlerOptions{serviceContext: ctx})
	server := httptest.NewServer(h)
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/catalog/activity")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: activity\n" {
		t.Fatalf("activity stream: %q (%v)", line, err)
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Config.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activity stream status %s", resp.Status)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
}
