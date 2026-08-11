package zotigod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const workerWriteWait = 10 * time.Second
const defaultWorkerApprovalWait = 30 * time.Second

const (
	defaultWorkerPingInterval = 15 * time.Second
	defaultWorkerPongWait     = 45 * time.Second
)

type workerMessageType string

const (
	workerMessageCommand          workerMessageType = "command"
	workerMessageDelta            workerMessageType = "display_delta"
	workerMessageDisplayWake      workerMessageType = "display_wake"
	workerMessageDisplayBarrier   workerMessageType = "display_barrier"
	workerMessageDisplayBarrierOK workerMessageType = "display_barrier_ack"
	workerMessageApprovalRequest  workerMessageType = "approval_request"
	workerMessageApprovalDecision workerMessageType = "approval_decision"
	workerMessageApprovalResult   workerMessageType = "approval_result"
)

type workerMessage struct {
	Type             workerMessageType        `json:"type"`
	Command          *commandResponse         `json:"command,omitempty"`
	Delta            *displayDeltaEvent       `json:"delta,omitempty"`
	DisplayBarrier   *workerDisplayBarrier    `json:"display_barrier,omitempty"`
	ApprovalRequest  *approvalRequestResponse `json:"approval_request,omitempty"`
	ApprovalDecision *workerApprovalDecision  `json:"approval_decision,omitempty"`
	ApprovalResult   *workerApprovalResult    `json:"approval_result,omitempty"`
}

type workerDisplayBarrier struct {
	ID string `json:"id"`
}

type workerApprovalDecision struct {
	RequestID  string                                  `json:"request_id"`
	ApprovalID string                                  `json:"approval_id"`
	Decisions  []zotigosession.DisplayApprovalDecision `json:"decisions"`
}

type workerApprovalResult struct {
	RequestID string                   `json:"request_id"`
	Approval  *approvalRequestResponse `json:"approval,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

type workerRegistry struct {
	mu           sync.Mutex
	workers      map[string]*workerConnection
	waiters      map[string][]chan struct{}
	onDisconnect func(string)
	onMessage    func(string, workerMessage)
	pingInterval time.Duration
	pongWait     time.Duration
	approvalWait time.Duration
}

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{
		workers:      make(map[string]*workerConnection),
		waiters:      make(map[string][]chan struct{}),
		pingInterval: defaultWorkerPingInterval,
		pongWait:     defaultWorkerPongWait,
		approvalWait: defaultWorkerApprovalWait,
	}
}

func (r *workerRegistry) SetDisconnectHandler(handler func(string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onDisconnect = handler
}

func (r *workerRegistry) SetMessageHandler(handler func(string, workerMessage)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMessage = handler
}

func (r *workerRegistry) Register(sessionID string, generation string, conn *websocket.Conn) *workerConnection {
	worker := newWorkerConnection(sessionID, generation, conn, r)

	r.mu.Lock()
	existing := r.workers[sessionID]
	r.workers[sessionID] = worker
	waiters := r.waiters[sessionID]
	delete(r.waiters, sessionID)
	r.mu.Unlock()

	if existing != nil {
		existing.close()
	}
	for _, waiter := range waiters {
		close(waiter)
	}

	go worker.writeLoop()
	go worker.readLoop()
	return worker
}

func (r *workerRegistry) Matches(sessionID string, generation string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.workers[sessionID]
	return worker != nil && worker.generation == generation
}

func (r *workerRegistry) Send(sessionID string, command commandResponse) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return false
	}
	return worker.send(command)
}

func (r *workerRegistry) acknowledgeDisplayBarrier(sessionID string, barrier workerDisplayBarrier) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return false
	}
	return worker.sendMessage(workerMessage{
		Type:           workerMessageDisplayBarrierOK,
		DisplayBarrier: &barrier,
	})
}

func (r *workerRegistry) SubmitApproval(ctx context.Context, sessionID string, approvalID string, decisions []zotigosession.DisplayApprovalDecision) (approvalRequestResponse, error) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return approvalRequestResponse{}, fmt.Errorf("approval decision requires an online worker")
	}
	if r.approvalWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.approvalWait)
		defer cancel()
	}
	return worker.submitApproval(ctx, approvalID, decisions)
}

func (r *workerRegistry) Has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.workers[sessionID]
	return ok
}

func (r *workerRegistry) Close(sessionID string) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker != nil {
		worker.close()
	}
}

func (r *workerRegistry) Wait(ctxDone <-chan struct{}, sessionID string) bool {
	r.mu.Lock()
	if _, ok := r.workers[sessionID]; ok {
		r.mu.Unlock()
		return true
	}
	waiter := make(chan struct{})
	r.waiters[sessionID] = append(r.waiters[sessionID], waiter)
	r.mu.Unlock()

	select {
	case <-ctxDone:
		r.removeWaiter(sessionID, waiter)
		return false
	case <-waiter:
		return true
	}
}

func (r *workerRegistry) removeWaiter(sessionID string, waiter chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	waiters := r.waiters[sessionID]
	for idx, candidate := range waiters {
		if candidate == waiter {
			waiters = append(waiters[:idx], waiters[idx+1:]...)
			break
		}
	}
	if len(waiters) == 0 {
		delete(r.waiters, sessionID)
		return
	}
	r.waiters[sessionID] = waiters
}

func (r *workerRegistry) unregister(sessionID string, worker *workerConnection) {
	r.mu.Lock()
	removed := false
	if r.workers[sessionID] == worker {
		delete(r.workers, sessionID)
		removed = true
	}
	onDisconnect := r.onDisconnect
	r.mu.Unlock()
	if removed && onDisconnect != nil {
		onDisconnect(sessionID)
	}
}

func (r *workerRegistry) receive(worker *workerConnection, msg workerMessage) {
	r.mu.Lock()
	active := r.workers[worker.sessionID] == worker
	handler := r.onMessage
	r.mu.Unlock()
	if active && handler != nil {
		handler(worker.sessionID, msg)
	}
}

type workerConnection struct {
	sessionID  string
	generation string
	conn       *websocket.Conn
	registry   *workerRegistry
	sendCh     chan workerMessage
	doneCh     chan struct{}
	closeOnce  sync.Once
	waitersMu  sync.Mutex
	waiters    map[string]chan workerApprovalResult
}

func newWorkerConnection(sessionID string, generation string, conn *websocket.Conn, registry *workerRegistry) *workerConnection {
	return &workerConnection{
		sessionID:  sessionID,
		generation: generation,
		conn:       conn,
		registry:   registry,
		sendCh:     make(chan workerMessage, 32),
		doneCh:     make(chan struct{}),
		waiters:    make(map[string]chan workerApprovalResult),
	}
}

func (c *workerConnection) send(command commandResponse) bool {
	return c.sendMessage(workerMessage{
		Type:    workerMessageCommand,
		Command: &command,
	})
}

func (c *workerConnection) sendMessage(msg workerMessage) bool {
	select {
	case <-c.doneCh:
		return false
	case c.sendCh <- msg:
		return true
	default:
		c.close()
		return false
	}
}

func (c *workerConnection) submitApproval(ctx context.Context, approvalID string, decisions []zotigosession.DisplayApprovalDecision) (approvalRequestResponse, error) {
	requestID := newZotigodID("approval_submit")
	waiter := make(chan workerApprovalResult, 1)
	c.waitersMu.Lock()
	c.waiters[requestID] = waiter
	c.waitersMu.Unlock()
	defer func() {
		c.waitersMu.Lock()
		delete(c.waiters, requestID)
		c.waitersMu.Unlock()
	}()

	msg := workerMessage{
		Type: workerMessageApprovalDecision,
		ApprovalDecision: &workerApprovalDecision{
			RequestID:  requestID,
			ApprovalID: approvalID,
			Decisions:  copyApprovalDecisions(decisions),
		},
	}
	select {
	case <-ctx.Done():
		return approvalRequestResponse{}, ctx.Err()
	case <-c.doneCh:
		return approvalRequestResponse{}, fmt.Errorf("worker disconnected")
	case c.sendCh <- msg:
	}

	select {
	case <-ctx.Done():
		return approvalRequestResponse{}, ctx.Err()
	case <-c.doneCh:
		return approvalRequestResponse{}, fmt.Errorf("worker disconnected")
	case result := <-waiter:
		if result.Error != "" {
			return approvalRequestResponse{}, fmt.Errorf("worker rejected approval decision: %s", result.Error)
		}
		if result.Approval == nil {
			return approvalRequestResponse{}, fmt.Errorf("worker returned an empty approval result")
		}
		return *result.Approval, nil
	}
}

func (c *workerConnection) resolveApproval(result workerApprovalResult) {
	c.waitersMu.Lock()
	waiter := c.waiters[result.RequestID]
	c.waitersMu.Unlock()
	if waiter == nil {
		return
	}
	select {
	case waiter <- result:
	default:
	}
}

func (c *workerConnection) writeLoop() {
	ticker := time.NewTicker(c.registry.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.doneCh:
			return
		case msg := <-c.sendCh:
			if err := c.writeJSON(msg); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			if err := c.writePing(); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *workerConnection) readLoop() {
	defer c.close()
	_ = c.conn.SetReadDeadline(time.Now().Add(c.registry.pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.registry.pongWait))
	})
	for {
		messageType, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var msg workerMessage
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return
		}
		if msg.Type == workerMessageApprovalResult && msg.ApprovalResult != nil {
			c.registry.receive(c, msg)
			c.resolveApproval(*msg.ApprovalResult)
			continue
		}
		c.registry.receive(c, msg)
	}
}

func (c *workerConnection) writeJSON(msg workerMessage) error {
	data, err := sonic.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(workerWriteWait)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *workerConnection) writePing() error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(workerWriteWait)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.PingMessage, nil)
}

func (c *workerConnection) close() {
	c.closeOnce.Do(func() {
		close(c.doneCh)
		c.registry.unregister(c.sessionID, c)
		_ = c.conn.Close()
	})
}

func validateWorkerSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	return nil
}
