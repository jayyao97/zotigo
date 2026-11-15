package workerbase

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type Writer struct {
	conn                  *websocket.Conn
	pingEvery             time.Duration
	displayControlTimeout time.Duration
	sendCh                chan wire.Message
	done                  chan struct{}
	closeOnce             sync.Once
}

type DisplayBarrierClient struct {
	writer  *Writer
	timeout time.Duration
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func NewDisplayBarrierClient(writer *Writer) *DisplayBarrierClient {
	return &DisplayBarrierClient{
		writer:  writer,
		timeout: HTTPTimeout,
		waiters: make(map[string]chan struct{}),
	}
}

func (b *DisplayBarrierClient) Wait(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	barrier := wire.DisplayBarrier{ID: sessionlog.NewID("display_barrier")}
	acknowledged := make(chan struct{})
	b.mu.Lock()
	b.waiters[barrier.ID] = acknowledged
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, barrier.ID)
		b.mu.Unlock()
	}()
	if err := b.writer.SendDisplayBarrier(waitCtx, barrier); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("display barrier acknowledgement timed out")
		}
		return err
	}
	select {
	case <-waitCtx.Done():
		if ctx.Err() == nil {
			return errors.New("display barrier acknowledgement timed out")
		}
		return ctx.Err()
	case <-b.writer.done:
		return errors.New("worker connection is closed")
	case <-acknowledged:
		return nil
	}
}

func (b *DisplayBarrierClient) Acknowledge(id string) {
	b.mu.Lock()
	acknowledged := b.waiters[id]
	if acknowledged != nil {
		delete(b.waiters, id)
		close(acknowledged)
	}
	b.mu.Unlock()
}

func NewWriter(conn *websocket.Conn, pingInterval time.Duration, pongWait time.Duration) *Writer {
	if pongWait > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
	}
	writer := &Writer{
		conn:                  conn,
		pingEvery:             pingInterval,
		displayControlTimeout: HTTPTimeout,
		sendCh:                make(chan wire.Message, DeltaBufferSize),
		done:                  make(chan struct{}),
	}
	go writer.Run()
	return writer
}

func (w *Writer) SendDelta(delta wire.DisplayDelta) {
	msg := wire.Message{Type: wire.MessageDelta, Delta: &delta}
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.sendCh <- msg:
	case <-w.done:
	default:
		// Deltas are previews. Never stall model generation; the completed
		// durable item replaces any partial preview that was dropped here.
	}
}

func (w *Writer) SendDisplayItem(item zotigosession.DisplayItem) {
	retirement := sessionlog.PreviewRetirement(item)
	// Unlike an ordinary wake, this retires daemon-owned preview memory even
	// when nobody is reading history. Keep it ordered after earlier deltas and
	// apply backpressure instead of dropping it or adding an unbounded queue.
	// Writer shutdown (including a WebSocket write failure) releases this wait;
	// the daemon then clears that worker's remaining previews on disconnect.
	select {
	case w.sendCh <- wire.Message{Type: wire.MessageDisplayWake, PreviewRetirement: &retirement}:
	case <-w.done:
	}
}

func (w *Writer) SendDisplayWake() {
	w.sendDisplayWake(wire.Message{Type: wire.MessageDisplayWake})
}

func (w *Writer) sendDisplayWake(msg wire.Message) {
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.sendCh <- msg:
	case <-w.done:
	default:
		// Periodic SSE catch-up makes ordinary display wakes optional.
	}
}

func (w *Writer) SendDisplayWakeReliable(ctx context.Context) error {
	timeout := w.displayControlTimeout
	if timeout <= 0 {
		timeout = HTTPTimeout
	}
	wakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg := wire.Message{Type: wire.MessageDisplayWake}
	select {
	case <-wakeCtx.Done():
		return wakeCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendIdle(ctx context.Context, idle wire.Idle) error {
	idleCtx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	msg := wire.Message{Type: wire.MessageIdle, Idle: &idle}
	select {
	case <-idleCtx.Done():
		return idleCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendWorking(ctx context.Context) error {
	workingCtx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	msg := wire.Message{Type: wire.MessageWorking}
	select {
	case <-workingCtx.Done():
		return workingCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendDisplayBarrier(ctx context.Context, barrier wire.DisplayBarrier) error {
	msg := wire.Message{Type: wire.MessageDisplayBarrier, DisplayBarrier: &barrier}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendApprovalResult(ctx context.Context, result wire.ApprovalResult) bool {
	msg := wire.Message{Type: wire.MessageApprovalResult, ApprovalResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *Writer) SendInteractionResult(ctx context.Context, result wire.InteractionResult) bool {
	msg := wire.Message{Type: wire.MessageInteractionResult, InteractionResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *Writer) SendInputResult(ctx context.Context, result wire.InputResult) bool {
	msg := wire.Message{Type: wire.MessageInputResult, InputResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *Writer) SendApprovalRequest(ctx context.Context, approval wire.ApprovalRequest) error {
	msg := wire.Message{Type: wire.MessageApprovalRequest, ApprovalRequest: &approval}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendInteractionRequest(ctx context.Context, interaction wire.InteractionRequest) error {
	msg := wire.Message{Type: wire.MessageInteractionRequest, InteractionRequest: &interaction}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendConversationBound(ctx context.Context, conversationID string) error {
	msg := wire.Message{
		Type:              wire.MessageConversationBound,
		ConversationBound: &wire.ConversationBound{ConversationID: conversationID},
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) SendRuntimeToolRequest(ctx context.Context, request wire.RuntimeToolRequest) error {
	msg := wire.Message{Type: wire.MessageRuntimeToolRequest, RuntimeToolRequest: &request}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *Writer) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

func (w *Writer) Run() {
	defer w.Close()
	var ticker *time.Ticker
	var ticks <-chan time.Time
	if w.pingEvery > 0 {
		ticker = time.NewTicker(w.pingEvery)
		ticks = ticker.C
		defer ticker.Stop()
	}
	for {
		select {
		case <-w.done:
			return
		case msg := <-w.sendCh:
			if err := w.writeMessage(websocket.TextMessage, msg); err != nil {
				_ = w.conn.Close()
				return
			}
		case <-ticks:
			if err := w.writeMessage(websocket.PingMessage, nil); err != nil {
				_ = w.conn.Close()
				return
			}
		}
	}
}

func (w *Writer) writeMessage(messageType int, value any) error {
	var data []byte
	var err error
	if value != nil {
		data, err = sonic.Marshal(value)
		if err != nil {
			return err
		}
	}
	if err := w.conn.SetWriteDeadline(time.Now().Add(WriteWait)); err != nil {
		return err
	}
	return w.conn.WriteMessage(messageType, data)
}

// NewUnstartedWriter returns a Writer with the given queue capacity whose
// write loop is not running. Test-only: daemon and worker tests in zotigod
// use it to observe what a runtime queues without a live connection.
func NewUnstartedWriter(conn *websocket.Conn, queue int) *Writer {
	return &Writer{conn: conn, sendCh: make(chan wire.Message, queue), done: make(chan struct{})}
}

// Queue exposes the outgoing queue for reading. Test-only.
func (w *Writer) Queue() <-chan wire.Message { return w.sendCh }
