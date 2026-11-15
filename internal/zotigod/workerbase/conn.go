package workerbase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/debug"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

const HTTPTimeout = 10 * time.Second

const workerDialErrorBodyLimit = 4 * 1024

const DefaultPingInterval = 15 * time.Second

const DefaultPongWait = 45 * time.Second

const CommandBufferSize = 32

const DeltaBufferSize = 256

type Config struct {
	DaemonURL  string
	SessionID  string
	Activation uint64
	AuthToken  string
}

func EnsureSession(ctx context.Context, store zotigosession.Store, sessionID string, cwd string) (*zotigosession.Session, error) {
	sess, err := store.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if sess != nil {
		return sess, nil
	}
	now := time.Now().UTC()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	sess = &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               sessionID,
			WorkingDirectory: cwd,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	}
	if err := store.Put(ctx, sess); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

func postWorkerJSON(ctx context.Context, client *http.Client, daemonURL string, path string, value any) error {
	data, err := sonic.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(daemonURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("worker post %s failed: %s", path, resp.Status)
	}
	return nil
}

func ReportFinish(ctx context.Context, client *http.Client, daemonURL string, sessionID string, generation string, err error) error {
	body := wire.FinishSessionRequest{Generation: generation}
	if err != nil && !IsExpectedClose(err) {
		body.Error = err.Error()
		if errors.Is(err, ErrRuntimeOccupied) {
			body.ErrorCode = "runtime_occupied"
			body.Error = strings.TrimPrefix(body.Error, ErrRuntimeOccupied.Error()+": ")
		}
	}
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/finish", body)
}

func ReportReady(ctx context.Context, client *http.Client, daemonURL string, sessionID string, generation string) error {
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/attach", wire.ReadyRequest{Generation: generation})
}

func WebSocketDialError(dialErr error, resp *http.Response) error {
	if resp == nil {
		return dialErr
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, workerDialErrorBodyLimit+1))
	if readErr != nil {
		return fmt.Errorf("%s (read response body: %v): %w", resp.Status, readErr, dialErr)
	}
	truncated := len(body) > workerDialErrorBodyLimit
	if truncated {
		body = body[:workerDialErrorBodyLimit]
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Errorf("%s: %w", resp.Status, dialErr)
	}
	if truncated {
		text += "…"
	}
	return fmt.Errorf("%s body=%q: %w", resp.Status, text, dialErr)
}

func LogBootStep(sessionID string, step string, started time.Time, bootStarted time.Time) {
	debug.Logf(
		"worker boot session=%s step=%s elapsed_ms=%d total_ms=%d",
		sessionID,
		step,
		time.Since(started).Milliseconds(),
		time.Since(bootStarted).Milliseconds(),
	)
}

func LogRuntimeStep(sessionID string, step string, started time.Time) {
	debug.Logf("worker runtime session=%s step=%s elapsed_ms=%d", sessionID, step, time.Since(started).Milliseconds())
}

func IsExpectedClose(err error) bool {
	if err == nil {
		return true
	}
	var uncertain *ProfileStateUncertainError
	var completionUncertain *ProfileCompletionUncertainError
	return errors.As(err, &uncertain) || errors.As(err, &completionUncertain) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, zotigotransport.ErrTransportClosed) ||
		strings.Contains(err.Error(), "websocket: close") ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "EOF")
}

func ConnectURL(daemonURL string, sessionID string, activation uint64) (string, error) {
	parsed, err := url.Parse(daemonURL)
	if err != nil {
		return "", fmt.Errorf("parse daemon url: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported daemon url scheme %q", parsed.Scheme)
	}
	parsed.Path = "/internal/workers/connect"
	values := parsed.Query()
	values.Set("session_id", sessionID)
	values.Set("activation", strconv.FormatUint(activation, 10))
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func AcquireSessionLock(ctx context.Context, store zotigosession.Store, sessionID string) (func() error, error) {
	if err := store.Lock(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	return func() error {
		return store.Unlock(context.Background(), sessionID)
	}, nil
}

const WriteWait = 10 * time.Second

func NewHTTPClient(token string) *http.Client {
	client := &http.Client{Timeout: HTTPTimeout}
	if token != "" {
		client.Transport = bearerTransport{base: http.DefaultTransport, token: token}
	}
	return client
}

type ProfileStateUncertainError struct {
	Cause error
}

func (e *ProfileStateUncertainError) Error() string {
	return "profile state is uncertain: " + e.Cause.Error()
}

func (e *ProfileStateUncertainError) Unwrap() error { return e.Cause }

type ProfileCompletionUncertainError struct{ Cause error }

func (e *ProfileCompletionUncertainError) Error() string {
	return "profile command completion is uncertain: " + e.Cause.Error()
}

func (e *ProfileCompletionUncertainError) Unwrap() error { return e.Cause }

const (
	ControlChannelClosedReason    = "control_channel_closed"
	ApprovalWorkerRestartedReason = "worker restarted before approval completed"
)

var ErrRuntimeOccupied = errors.New("runtime session is open in another application")

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
