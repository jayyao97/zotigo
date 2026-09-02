package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	asyncWorkerCount = 4
	asyncQueueSize   = 128
)

type DispatchResult struct {
	Denied bool
	Reason string
}

type commandExecutor func(context.Context, Handler, Event) processResult

type Option func(*Dispatcher)

func WithLogger(logger *log.Logger) Option {
	return func(dispatcher *Dispatcher) {
		if logger != nil {
			dispatcher.logger = logger
		}
	}
}

type queuedInvocation struct {
	event        Event
	handler      Handler
	handlerIndex int
}

type Dispatcher struct {
	config      Config
	logger      *log.Logger
	execute     commandExecutor
	asyncCtx    context.Context
	asyncCancel context.CancelFunc
	queue       chan queuedInvocation
	wait        sync.WaitGroup
	mu          sync.Mutex
	closed      bool
}

func New(config Config, opts ...Option) *Dispatcher {
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	dispatcher := &Dispatcher{
		config:      config,
		logger:      log.New(io.Discard, "", 0),
		execute:     executeCommand,
		asyncCtx:    asyncCtx,
		asyncCancel: asyncCancel,
	}
	for _, option := range opts {
		option(dispatcher)
	}
	return dispatcher
}

func LoadDefault(logger *log.Logger) (*Dispatcher, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	config, issues, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if logger != nil {
			logger.Printf("hook_config outcome=skipped error=%q", issue)
		}
	}
	return New(config, WithLogger(logger)), nil
}

func (d *Dispatcher) HasHandlers(eventName EventName) bool {
	return d != nil && len(d.config.Hooks[eventName]) > 0
}

func (d *Dispatcher) Dispatch(ctx context.Context, event Event) DispatchResult {
	if d == nil || !d.HasHandlers(event.EventName) {
		return DispatchResult{}
	}
	if err := prepareEvent(&event); err != nil {
		d.logger.Printf("hook event=%s outcome=failed error=%q", event.EventName, err)
		return DispatchResult{}
	}

	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return DispatchResult{}
	}

	if event.EventName == PreToolUse {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout(PreToolUse))
		defer cancel()
	}
	for index, handler := range d.config.Hooks[event.EventName] {
		if !handler.matchesAgent(event.Agent) {
			continue
		}
		if event.Tool != nil && !handler.matches(event.Tool.Name) {
			continue
		}
		if handler.Async {
			d.enqueue(queuedInvocation{event: event, handler: handler, handlerIndex: index})
			continue
		}
		result := d.run(ctx, event, handler, index)
		if result.Denied {
			return result
		}
		if err := ctx.Err(); err != nil {
			return DispatchResult{}
		}
	}
	return DispatchResult{}
}

func (d *Dispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		d.asyncCancel()
		if d.queue != nil {
			close(d.queue)
		}
	}
	queue := d.queue
	d.mu.Unlock()
	if queue == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		d.wait.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (d *Dispatcher) enqueue(invocation queuedInvocation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if d.queue == nil {
		d.queue = make(chan queuedInvocation, asyncQueueSize)
		for range asyncWorkerCount {
			d.wait.Add(1)
			go d.asyncWorker()
		}
	}
	select {
	case d.queue <- invocation:
	default:
		d.logger.Printf(
			"hook event_id=%s event=%s handler=%d command=%q outcome=dropped error=%q",
			invocation.event.EventID, invocation.event.EventName, invocation.handlerIndex,
			invocation.handler.Command, "async queue full",
		)
	}
}

func (d *Dispatcher) asyncWorker() {
	defer d.wait.Done()
	for invocation := range d.queue {
		d.run(d.asyncCtx, invocation.event, invocation.handler, invocation.handlerIndex)
	}
}

func (d *Dispatcher) run(ctx context.Context, event Event, handler Handler, handlerIndex int) DispatchResult {
	timeout := defaultTimeout(event.EventName)
	if handler.TimeoutMS > 0 {
		timeout = time.Duration(handler.TimeoutMS) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	startedAt := time.Now()
	process := d.execute(runCtx, handler, event)
	cancel()
	duration := time.Since(startedAt)

	if event.EventName == PreToolUse && process.started && process.exitCode == 2 && !process.timedOut && !process.cancelled {
		reason := firstParagraph(process.stderr)
		if reason == "" {
			reason = "hook denied tool use"
		}
		d.logResult(event, handler, handlerIndex, duration, process, "denied", nil)
		return DispatchResult{Denied: true, Reason: reason}
	}
	if process.err != nil {
		outcome := "failed"
		if process.timedOut {
			outcome = "timed_out"
		} else if process.cancelled {
			outcome = "cancelled"
		}
		d.logResult(event, handler, handlerIndex, duration, process, outcome, process.err)
		return DispatchResult{}
	}
	if event.EventName != PreToolUse || strings.TrimSpace(process.stdout) == "" {
		return DispatchResult{}
	}

	var output struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(process.stdout))
	if err := decoder.Decode(&output); err != nil {
		d.logResult(event, handler, handlerIndex, duration, process, "failed", fmt.Errorf("decode stdout: %w", err))
		return DispatchResult{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		d.logResult(event, handler, handlerIndex, duration, process, "failed", fmt.Errorf("stdout must contain one JSON object"))
		return DispatchResult{}
	}
	switch strings.ToLower(strings.TrimSpace(output.Decision)) {
	case "allow":
		return DispatchResult{}
	case "deny":
		reason := strings.TrimSpace(output.Reason)
		if reason == "" {
			reason = "hook denied tool use"
		}
		d.logResult(event, handler, handlerIndex, duration, process, "denied", nil)
		return DispatchResult{Denied: true, Reason: reason}
	default:
		d.logResult(event, handler, handlerIndex, duration, process, "failed", fmt.Errorf("unsupported decision %q", output.Decision))
		return DispatchResult{}
	}
}

func (d *Dispatcher) logResult(event Event, handler Handler, handlerIndex int, duration time.Duration, process processResult, outcome string, err error) {
	errorText := ""
	if err != nil {
		errorText = err.Error()
	}
	d.logger.Printf(
		"hook event_id=%s event=%s session=%s turn=%s handler=%d command=%q duration_ms=%d exit_code=%d outcome=%s stderr=%q stdout_truncated=%t stderr_truncated=%t error=%q",
		event.EventID, event.EventName, event.SessionID, event.TurnID, handlerIndex, handler.Command,
		duration.Milliseconds(), process.exitCode, outcome, strings.TrimSpace(process.stderr),
		process.stdoutTruncated, process.stderrTruncated, errorText,
	)
}

func prepareEvent(event *Event) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = ConfigVersion
	}
	if event.SchemaVersion != ConfigVersion {
		return fmt.Errorf("unsupported schema version %d", event.SchemaVersion)
	}
	if _, ok := ParseEventName(string(event.EventName)); !ok {
		return fmt.Errorf("unsupported event name %q", event.EventName)
	}
	if event.EventID == "" {
		event.EventID = NewEvent(event.EventName, event.SessionID, event.Agent, event.CWD).EventID
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.SessionID == "" || event.Agent == "" || event.CWD == "" {
		return fmt.Errorf("session_id, agent, and cwd are required")
	}
	if event.EventName == PreToolUse || event.EventName == PostToolUse {
		if event.Tool == nil || event.Tool.CallID == "" || event.Tool.Name == "" {
			return fmt.Errorf("tool event requires call_id and name")
		}
	}
	return nil
}

func marshalEvent(event Event) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode hook event: %w", err)
	}
	return payload, nil
}

func defaultTimeout(eventName EventName) time.Duration {
	switch eventName {
	case SessionEnd:
		return 1500 * time.Millisecond
	case PreToolUse:
		return 5 * time.Second
	case PostToolUse:
		return 10 * time.Second
	default:
		return 3 * time.Second
	}
}

func firstParagraph(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	lines := strings.Split(value, "\n")
	paragraph := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		paragraph = append(paragraph, strings.TrimSpace(line))
	}
	return strings.Join(paragraph, "\n")
}
