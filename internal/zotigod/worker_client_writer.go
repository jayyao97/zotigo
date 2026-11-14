package zotigod

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type workerClientWriter struct {
	conn                  *websocket.Conn
	pingEvery             time.Duration
	displayControlTimeout time.Duration
	sendCh                chan workerMessage
	done                  chan struct{}
	closeOnce             sync.Once
}

type workerDisplayBarrierClient struct {
	writer  *workerClientWriter
	timeout time.Duration
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func newWorkerDisplayBarrierClient(writer *workerClientWriter) *workerDisplayBarrierClient {
	return &workerDisplayBarrierClient{
		writer:  writer,
		timeout: workerHTTPTimeout,
		waiters: make(map[string]chan struct{}),
	}
}

func (b *workerDisplayBarrierClient) Wait(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	barrier := workerDisplayBarrier{ID: newZotigodID("display_barrier")}
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

func (b *workerDisplayBarrierClient) Acknowledge(id string) {
	b.mu.Lock()
	acknowledged := b.waiters[id]
	if acknowledged != nil {
		delete(b.waiters, id)
		close(acknowledged)
	}
	b.mu.Unlock()
}

func newWorkerClientWriter(conn *websocket.Conn, pingInterval time.Duration, pongWait time.Duration) *workerClientWriter {
	if pongWait > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
	}
	writer := &workerClientWriter{
		conn:                  conn,
		pingEvery:             pingInterval,
		displayControlTimeout: workerHTTPTimeout,
		sendCh:                make(chan workerMessage, workerDeltaBufferSize),
		done:                  make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (w *workerClientWriter) SendDelta(delta displayDeltaEvent) {
	msg := workerMessage{Type: workerMessageDelta, Delta: &delta}
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

func (w *workerClientWriter) SendDisplayItem(item zotigosession.DisplayItem) {
	retirement := previewRetirement(item)
	// Unlike an ordinary wake, this retires daemon-owned preview memory even
	// when nobody is reading history. Keep it ordered after earlier deltas and
	// apply backpressure instead of dropping it or adding an unbounded queue.
	// Writer shutdown (including a WebSocket write failure) releases this wait;
	// the daemon then clears that worker's remaining previews on disconnect.
	select {
	case w.sendCh <- workerMessage{Type: workerMessageDisplayWake, PreviewRetirement: &retirement}:
	case <-w.done:
	}
}

func (w *workerClientWriter) SendDisplayWake() {
	w.sendDisplayWake(workerMessage{Type: workerMessageDisplayWake})
}

func (w *workerClientWriter) sendDisplayWake(msg workerMessage) {
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

func (w *workerClientWriter) SendDisplayWakeReliable(ctx context.Context) error {
	timeout := w.displayControlTimeout
	if timeout <= 0 {
		timeout = workerHTTPTimeout
	}
	wakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg := workerMessage{Type: workerMessageDisplayWake}
	select {
	case <-wakeCtx.Done():
		return wakeCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendIdle(ctx context.Context, idle workerIdle) error {
	idleCtx, cancel := context.WithTimeout(ctx, workerHTTPTimeout)
	defer cancel()
	msg := workerMessage{Type: workerMessageIdle, Idle: &idle}
	select {
	case <-idleCtx.Done():
		return idleCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendWorking(ctx context.Context) error {
	workingCtx, cancel := context.WithTimeout(ctx, workerHTTPTimeout)
	defer cancel()
	msg := workerMessage{Type: workerMessageWorking}
	select {
	case <-workingCtx.Done():
		return workingCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendDisplayBarrier(ctx context.Context, barrier workerDisplayBarrier) error {
	msg := workerMessage{Type: workerMessageDisplayBarrier, DisplayBarrier: &barrier}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendApprovalResult(ctx context.Context, result workerApprovalResult) bool {
	msg := workerMessage{Type: workerMessageApprovalResult, ApprovalResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *workerClientWriter) SendInteractionResult(ctx context.Context, result workerInteractionResult) bool {
	msg := workerMessage{Type: workerMessageInteractionResult, InteractionResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *workerClientWriter) SendInputResult(ctx context.Context, result workerInputResult) bool {
	msg := workerMessage{Type: workerMessageInputResult, InputResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *workerClientWriter) SendApprovalRequest(ctx context.Context, approval approvalRequestResponse) error {
	msg := workerMessage{Type: workerMessageApprovalRequest, ApprovalRequest: &approval}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendInteractionRequest(ctx context.Context, interaction interactionRequest) error {
	msg := workerMessage{Type: workerMessageInteractionRequest, InteractionRequest: &interaction}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendConversationBound(ctx context.Context, conversationID string) error {
	msg := workerMessage{
		Type:              workerMessageConversationBound,
		ConversationBound: &workerConversationBound{ConversationID: conversationID},
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

func (w *workerClientWriter) SendRuntimeToolRequest(ctx context.Context, request workerRuntimeToolRequest) error {
	msg := workerMessage{Type: workerMessageRuntimeToolRequest, RuntimeToolRequest: &request}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

func (w *workerClientWriter) run() {
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

func (w *workerClientWriter) writeMessage(messageType int, value any) error {
	var data []byte
	var err error
	if value != nil {
		data, err = sonic.Marshal(value)
		if err != nil {
			return err
		}
	}
	if err := w.conn.SetWriteDeadline(time.Now().Add(workerWriteWait)); err != nil {
		return err
	}
	return w.conn.WriteMessage(messageType, data)
}
