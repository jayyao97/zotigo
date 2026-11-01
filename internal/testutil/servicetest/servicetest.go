// Package servicetest runs the zotigod session service against a scripted
// model for cross-package integration tests.
package servicetest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

// Provider is a scripted OpenAI Chat Completions endpoint. The reply
// depends on the latest user prompt so tests drive the agent loop through
// real workers without a model.
type Provider struct {
	mu       sync.Mutex
	requests [][]map[string]any
}

func (p *Provider) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.requests = append(p.requests, body.Messages)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(chunk string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}
	text := func(value string) {
		data, _ := json.Marshal(value)
		send(`{"choices":[{"index":0,"delta":{"content":` + string(data) + `},"finish_reason":null}]}`)
	}
	finish := func(reason string) {
		send(`{"choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		send("[DONE]")
	}
	toolCall := func(name string, args map[string]any) {
		encoded, _ := json.Marshal(args)
		quoted, _ := json.Marshal(string(encoded))
		send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_` + name + `","type":"function","function":{"name":"` + name + `","arguments":` + string(quoted) + `}}]},"finish_reason":null}]}`)
		finish("tool_calls")
	}

	slow := func() {
		for index := 0; index < 200; index++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			text(fmt.Sprintf("chunk %d ", index))
		}
		finish("stop")
	}
	last := body.Messages[len(body.Messages)-1]
	if last["role"] == "tool" && strings.Contains(lastUserPrompt(body.Messages), "then-slow") {
		slow()
		return
	}
	if last["role"] == "tool" {
		text("done after tool")
		finish("stop")
		return
	}
	prompt := lastUserPrompt(body.Messages)
	switch {
	case strings.Contains(prompt, "approve-touch"):
		toolCall("shell", map[string]any{"command": "touch created-by-agent.txt"})
	case strings.Contains(prompt, "where-am-i"):
		toolCall("shell", map[string]any{"command": "pwd"})
	case strings.Contains(prompt, "slow"):
		slow()
	default:
		text("hi ")
		text("there")
		finish("stop")
	}
}

// lastUserPrompt returns the newest text block of the newest user message.
// Consecutive user inputs (for example after an interrupted turn) can share
// one message, and only the newest block selects the scripted reply.
func lastUserPrompt(messages []map[string]any) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index]["role"] != "user" {
			continue
		}
		switch content := messages[index]["content"].(type) {
		case string:
			return content
		case []any:
			for part := len(content) - 1; part >= 0; part-- {
				if block, ok := content[part].(map[string]any); ok {
					if text, ok := block["text"].(string); ok {
						return text
					}
				}
			}
		}
		return ""
	}
	return ""
}

// LastRequestContains reports whether the latest model request contains value.
func (p *Provider) LastRequestContains(value string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		return false
	}
	data, _ := json.Marshal(p.requests[len(p.requests)-1])
	return strings.Contains(string(data), value)
}

// Fixture is an isolated HOME with a fake-model profile and one ready
// Workspace, suitable for running the session service with real workers.
type Fixture struct {
	Home string
	// StateDir is the isolated state root to pass as --state-dir; config
	// stays in Home/.zotigo.
	StateDir  string
	Provider  *Provider
	Workspace zotigoworkspace.Workspace
}

// New prepares the fixture and points HOME at it. Workers inherit HOME.
//
// Prompts drive the fake model: "approve-touch" requests a mutating shell
// command, "where-am-i" runs pwd, "slow" streams for ~10s, anything else
// replies "hi there"; after a tool result it replies "done after tool", or
// streams slowly when the prompt contains "then-slow".
func New(t *testing.T) *Fixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Restored after the test; embedding the service exports it.
	t.Setenv(config.StateDirEnv, "")
	stateDir := filepath.Join(home, "state")
	provider := &Provider{}
	server := httptest.NewServer(http.HandlerFunc(provider.handler))
	t.Cleanup(server.Close)
	dir := filepath.Join(home, config.ConfigDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("default_profile: fake\nprofiles:\n  fake:\n    provider: openai\n    model: fake-model\n    api_key: fixture\n    base_url: %s/v1\n", server.URL)
	if err := os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := zotigoworkspace.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project, err := catalog.CreateProject(ctx, "Service")
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalog.CreateWorkspace(ctx, project.ID, "Main")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := catalog.ProvisionWorkspaceScaffold(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = catalog.Close()
	return &Fixture{Home: home, StateDir: stateDir, Provider: provider, Workspace: workspace}
}

// RequestCount reports how many model requests the provider has served.
func (p *Provider) RequestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// DelayEvents returns a proxy to baseURL that holds every event-stream
// response for delay before forwarding it, while other API calls pass
// through at once. It reproduces clients whose view lags the service.
func DelayEvents(t *testing.T, baseURL string, delay time.Duration) string {
	return Proxy(t, baseURL, ProxyOptions{EventDelay: delay})
}

// ProxyOptions configures Proxy.
type ProxyOptions struct {
	// EventDelay holds each event-stream response before forwarding it.
	EventDelay time.Duration
	// DropMessageResponses forwards POST .../messages to the service and
	// then closes the connection without a response, so the client cannot
	// tell whether its input was accepted.
	DropMessageResponses bool
}

// Proxy returns a proxy to baseURL that distorts timing or responses the
// way a flaky network would, without inventing any service state.
func Proxy(t *testing.T, baseURL string, opts ProxyOptions) string {
	t.Helper()
	target, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.EventDelay > 0 && strings.HasSuffix(r.URL.Path, "/events") {
			select {
			case <-time.After(opts.EventDelay):
			case <-r.Context().Done():
				return
			}
		}
		if opts.DropMessageResponses && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			proxy.ServeHTTP(httptest.NewRecorder(), r)
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}
