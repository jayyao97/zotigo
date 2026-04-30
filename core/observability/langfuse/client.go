// Package langfuse implements the observability.Observer interface
// against Langfuse's public ingestion HTTP API. The integration is
// designed to be invisible on the hot path: events are queued into a
// buffered channel and flushed by a single background goroutine on a
// timer, with bounded memory and silent drop-on-overflow so
// observability failures never propagate to the user.
package langfuse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Default tunings — overridable via Config when needed. The buffer
// cap and flush cadence are sized for a single interactive user
// session: ingestion of one user turn typically produces ~5–10
// events, so a 256-slot buffer + 5s flush comfortably absorbs short
// bursts. Drop-on-overflow is acceptable; the alternative
// (back-pressuring the agent) defeats the purpose of telemetry.
const (
	defaultFlushInterval = 5 * time.Second
	defaultBufferSize    = 256
	defaultBatchSize     = 50
	defaultHTTPTimeout   = 8 * time.Second
)

// Config holds per-instance settings. Empty strings on PublicKey /
// SecretKey disable the integration entirely (NewClient returns nil,
// caller falls back to a Noop observer upstream).
type Config struct {
	Host          string        // base URL; empty → https://cloud.langfuse.com
	PublicKey     string        // Langfuse PUBLIC_KEY
	SecretKey     string        // Langfuse SECRET_KEY
	FlushInterval time.Duration // 0 → defaultFlushInterval
	BufferSize    int           // 0 → defaultBufferSize

	// StaticTraceMetadata is merged into every trace-create event's
	// metadata field. Use it for process-level facts that don't change
	// across turns (zotigo_session, process_start, resumed) so users
	// can filter Langfuse Sessions by metadata.zotigo_session and
	// aggregate every turn of a logical zotigo thread across multiple
	// --resume runs even though each turn lives in its own session.
	StaticTraceMetadata map[string]any
}

// event is one ingestion record. body is type-specific JSON shape
// defined by Langfuse (trace-create, generation-create, …).
type event struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

type client struct {
	host        string
	auth        string // Basic auth header value, precomputed
	httpClient  *http.Client
	staticTrace map[string]any // optional metadata merged into every trace-create

	mu     sync.Mutex
	closed bool

	queue chan event
	done  chan struct{} // closed when flush goroutine exits
}

// NewClient constructs a live Langfuse ingestion client. Returns nil
// when credentials are missing — callers should treat nil as
// "observability disabled" and use Noop instead.
func NewClient(cfg Config) *client {
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil
	}
	host := cfg.Host
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	flush := cfg.FlushInterval
	if flush <= 0 {
		flush = defaultFlushInterval
	}
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}

	c := &client{
		host:        host,
		auth:        basicAuth(cfg.PublicKey, cfg.SecretKey),
		httpClient:  &http.Client{Timeout: defaultHTTPTimeout},
		staticTrace: cfg.StaticTraceMetadata,
		queue:       make(chan event, bufSize),
		done:        make(chan struct{}),
	}
	go c.run(flush)
	return c
}

// emit queues an event for async flush. Drops silently if the buffer
// is full or the client is closed — the caller must not block on
// telemetry.
//
// The send is performed under the same lock that guards `closed` so
// a concurrent Close() can't observe closed=false then race ahead and
// `close(c.queue)` before this select runs — that ordering would
// panic with "send on closed channel". Holding the lock across the
// non-blocking send is cheap (no I/O, no allocation under lock) and
// correctness-critical.
func (c *client) emit(eventType string, body any) {
	ev := event{
		ID:        newID(),
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Body:      body,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.queue <- ev:
	default:
		// drop silently when buffer is full — telemetry must not
		// back-pressure the agent
	}
}

// run is the flush loop. Exits when queue is closed.
func (c *client) run(flushInterval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]event, 0, defaultBatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// best-effort send; failures drop the batch silently so a
		// flaky backend can't snowball into a memory blow-up
		_ = c.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev, ok := <-c.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= defaultBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops accepting new events, drains the queue with one final
// flush, and waits up to ctx deadline for the background goroutine
// to exit. Idempotent.
func (c *client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.queue)
	c.mu.Unlock()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// send POSTs one batch to the ingestion endpoint. No retry — the next
// flush tick will pick up future events; replaying an entire batch on
// transient network failures isn't worth the complexity here.
func (c *client) send(batch []event) error {
	payload := map[string]any{
		"batch": batch,
		"metadata": map[string]string{
			"sdk_name": "zotigo",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.auth)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 207 Multi-Status is Langfuse's "some succeeded, some failed";
	// treat as success — partial dropping is fine for telemetry.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusMultiStatus {
		return nil
	}
	return fmt.Errorf("http %d", resp.StatusCode)
}

// newID generates a 16-byte hex string suitable as a Langfuse trace
// or observation ID. Not a UUIDv4 (no version/variant bits) but
// Langfuse only requires uniqueness within a project.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}
