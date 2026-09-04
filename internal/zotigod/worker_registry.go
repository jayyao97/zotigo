package zotigod

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const workerWriteWait = 10 * time.Second
const defaultWorkerApprovalWait = 30 * time.Second

var errWorkerOffline = errors.New("worker is offline")

const (
	defaultWorkerPingInterval = 15 * time.Second
	defaultWorkerPongWait     = 45 * time.Second
)

type workerMessageType string

const (
	workerMessageCommand                 workerMessageType = "command"
	workerMessageWorking                 workerMessageType = "working"
	workerMessageDelta                   workerMessageType = "display_delta"
	workerMessageDisplayWake             workerMessageType = "display_wake"
	workerMessageDisplayBarrier          workerMessageType = "display_barrier"
	workerMessageDisplayBarrierOK        workerMessageType = "display_barrier_ack"
	workerMessageApprovalRequest         workerMessageType = "approval_request"
	workerMessageApprovalDecision        workerMessageType = "approval_decision"
	workerMessageApprovalResult          workerMessageType = "approval_result"
	workerMessageConversationBound       workerMessageType = "conversation_bound"
	workerMessageConversationBoundResult workerMessageType = "conversation_bound_result"
	workerMessageInputRequest            workerMessageType = "input_request"
	workerMessageInputResult             workerMessageType = "input_result"
	workerMessageIdle                    workerMessageType = "idle"
)

type workerMessage struct {
	Type                    workerMessageType              `json:"type"`
	Command                 *commandResponse               `json:"command,omitempty"`
	Delta                   *displayDeltaEvent             `json:"delta,omitempty"`
	DisplayBarrier          *workerDisplayBarrier          `json:"display_barrier,omitempty"`
	ApprovalRequest         *approvalRequestResponse       `json:"approval_request,omitempty"`
	ApprovalDecision        *workerApprovalDecision        `json:"approval_decision,omitempty"`
	ApprovalResult          *workerApprovalResult          `json:"approval_result,omitempty"`
	ConversationBound       *workerConversationBound       `json:"conversation_bound,omitempty"`
	ConversationBoundResult *workerConversationBoundResult `json:"conversation_bound_result,omitempty"`
	InputRequest            *workerInputRequest            `json:"input_request,omitempty"`
	InputResult             *workerInputResult             `json:"input_result,omitempty"`
	Idle                    *workerIdle                    `json:"idle,omitempty"`
}

type workerInputRequest struct {
	RequestID      string          `json:"request_id"`
	Command        commandResponse `json:"command"`
	SteeringOnly   bool            `json:"steering_only,omitempty"`
	ExpectedTurnID string          `json:"expected_turn_id,omitempty"`
}

type workerInputResult struct {
	RequestID string           `json:"request_id"`
	Command   *commandResponse `json:"command,omitempty"`
	ErrorCode string           `json:"error_code,omitempty"`
	Error     string           `json:"error,omitempty"`
}

type workerIdle struct {
	CommandSequence uint64 `json:"command_sequence"`
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

type workerConversationBound struct {
	ConversationID string `json:"conversation_id"`
}

type workerConversationBoundResult struct {
	ConversationID string `json:"conversation_id"`
	ErrorCode      string `json:"error_code,omitempty"`
	Error          string `json:"error,omitempty"`
}

type workerRegistry struct {
	mu           sync.Mutex
	workers      map[string]*workerConnection
	waiters      map[string][]chan struct{}
	onDisconnect func(string, string)
	onMessage    func(string, string, workerMessage)
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

func (r *workerRegistry) SetDisconnectHandler(handler func(string, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onDisconnect = handler
}

func (r *workerRegistry) SetMessageHandler(handler func(string, string, workerMessage)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMessage = handler
}

func (r *workerRegistry) Register(sessionID string, generation string, conn *websocket.Conn) *workerConnection {
	worker := newWorkerConnection(sessionID, generation, conn, r)

	r.mu.Lock()
	existing := r.workers[sessionID]
	if existing != nil && existing.idleTimer != nil {
		existing.idleTimer.Stop()
		existing.idleTimer = nil
	}
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
	return worker != nil && !worker.closing && worker.generation == generation
}

func (r *workerRegistry) SendConversationBoundResult(sessionID string, generation string, result workerConversationBoundResult) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing && worker.generation == generation
	r.mu.Unlock()
	if !available {
		return false
	}
	return worker.sendMessage(workerMessage{Type: workerMessageConversationBoundResult, ConversationBoundResult: &result})
}

func (r *workerRegistry) Send(sessionID string, command commandResponse) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing
	if available {
		if worker.idleTimer != nil {
			worker.idleTimer.Stop()
			worker.idleTimer = nil
		}
		worker.idleCommandSequence = 0
		worker.idleTimeout = 0
		worker.idlePending = false
		if command.Sequence > worker.lastCommandSequence {
			worker.lastCommandSequence = command.Sequence
		}
	}
	r.mu.Unlock()
	if !available {
		return false
	}
	return worker.send(command)
}

func (r *workerRegistry) CloseWhenIdle(sessionID string, generation string, idle workerIdle, timeout time.Duration) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	if worker == nil || worker.closing || worker.generation != generation || worker.lastCommandSequence > idle.CommandSequence {
		r.mu.Unlock()
		return false
	}
	if worker.idleTimer != nil {
		worker.idleTimer.Stop()
	}
	worker.idleCommandSequence = idle.CommandSequence
	worker.idleTimeout = timeout
	worker.idlePending = true
	if len(worker.inputRequests) > 0 {
		worker.idleTimer = nil
		r.mu.Unlock()
		return true
	}
	if timeout <= 0 {
		worker.idleTimer = nil
		worker.idleCommandSequence = 0
		worker.idleTimeout = 0
		worker.idlePending = false
		worker.closing = true
		r.mu.Unlock()
		go worker.close()
		return true
	}
	worker.idleEpoch++
	idleEpoch := worker.idleEpoch
	worker.idleTimer = time.AfterFunc(timeout, func() {
		r.closeIdleWorker(worker, idleEpoch, idle.CommandSequence)
	})
	r.mu.Unlock()
	return true
}

func (r *workerRegistry) closeIdleWorker(worker *workerConnection, idleEpoch uint64, commandSequence uint64) {
	r.mu.Lock()
	active := r.workers[worker.sessionID] == worker && !worker.closing && worker.idleTimer != nil && worker.idleEpoch == idleEpoch && len(worker.inputRequests) == 0 && worker.lastCommandSequence <= commandSequence
	if active {
		worker.idleTimer = nil
		worker.idleCommandSequence = 0
		worker.idleTimeout = 0
		worker.idlePending = false
		worker.closing = true
	}
	r.mu.Unlock()
	if active {
		worker.close()
	}
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
	worker := r.workers[sessionID]
	return worker != nil && !worker.closing
}

func (r *workerRegistry) Close(sessionID string) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker != nil {
		worker.close()
	}
}

func (r *workerRegistry) Detach(sessionID string) *workerConnection {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.workers[sessionID]
	if worker == nil || worker.closing {
		return nil
	}
	if worker.idleTimer != nil {
		worker.idleTimer.Stop()
		worker.idleTimer = nil
	}
	worker.closing = true
	delete(r.workers, sessionID)
	return worker
}

func (r *workerRegistry) Wait(ctxDone <-chan struct{}, sessionID string) bool {
	r.mu.Lock()
	if worker := r.workers[sessionID]; worker != nil && !worker.closing {
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
	var submissions []*workerInputSubmission
	if r.workers[sessionID] == worker {
		if worker.idleTimer != nil {
			worker.idleTimer.Stop()
			worker.idleTimer = nil
		}
		for _, submission := range worker.inputRequests {
			submissions = append(submissions, submission)
		}
		worker.inputRequests = make(map[string]*workerInputSubmission)
		delete(r.workers, sessionID)
		removed = true
	}
	onDisconnect := r.onDisconnect
	r.mu.Unlock()
	for _, submission := range submissions {
		submission.finish()
	}
	if removed && onDisconnect != nil {
		onDisconnect(sessionID, worker.generation)
	}
}

func (r *workerRegistry) receive(worker *workerConnection, msg workerMessage) {
	r.mu.Lock()
	active := r.workers[worker.sessionID] == worker
	handler := r.onMessage
	r.mu.Unlock()
	if active && handler != nil {
		handler(worker.sessionID, worker.generation, msg)
	}
}

type workerConnection struct {
	sessionID           string
	generation          string
	conn                *websocket.Conn
	registry            *workerRegistry
	sendCh              chan workerMessage
	doneCh              chan struct{}
	closeOnce           sync.Once
	waitersMu           sync.Mutex
	waiters             map[string]chan workerApprovalResult
	inputWaiters        map[string]chan workerInputResult
	idleTimer           *time.Timer
	idleEpoch           uint64
	idleCommandSequence uint64
	idleTimeout         time.Duration
	idlePending         bool
	inputRequests       map[string]*workerInputSubmission
	lastCommandSequence uint64
	closing             bool
}

func newWorkerConnection(sessionID string, generation string, conn *websocket.Conn, registry *workerRegistry) *workerConnection {
	return &workerConnection{
		sessionID:     sessionID,
		generation:    generation,
		conn:          conn,
		registry:      registry,
		sendCh:        make(chan workerMessage, 32),
		doneCh:        make(chan struct{}),
		waiters:       make(map[string]chan workerApprovalResult),
		inputWaiters:  make(map[string]chan workerInputResult),
		inputRequests: make(map[string]*workerInputSubmission),
	}
}

func (r *workerRegistry) SubmitInput(ctx context.Context, sessionID string, request workerInputRequest) (commandResponse, error) {
	submission, err := r.BeginInput(sessionID, request)
	if err != nil {
		return commandResponse{}, err
	}
	return submission.Await(ctx)
}

func (r *workerRegistry) BeginInput(sessionID string, request workerInputRequest) (*workerInputSubmission, error) {
	request.RequestID = newZotigodID("input_submit")
	waiter := make(chan workerInputResult, 1)

	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing
	if !available {
		r.mu.Unlock()
		return nil, errWorkerOffline
	}
	if worker.idleTimer != nil {
		worker.idleTimer.Stop()
		worker.idleTimer = nil
	}
	if worker.inputRequests == nil {
		worker.inputRequests = make(map[string]*workerInputSubmission)
	}
	submission := &workerInputSubmission{worker: worker, requestID: request.RequestID, waiter: waiter}
	worker.inputRequests[request.RequestID] = submission
	worker.waitersMu.Lock()
	worker.inputWaiters[request.RequestID] = waiter
	worker.waitersMu.Unlock()
	r.mu.Unlock()

	msg := workerMessage{Type: workerMessageInputRequest, InputRequest: &request}
	if !worker.trySendMessage(msg) {
		worker.waitersMu.Lock()
		delete(worker.inputWaiters, request.RequestID)
		worker.waitersMu.Unlock()
		r.finishInput(worker, request.RequestID, nil)
		go worker.close()
		return nil, fmt.Errorf("%w: input queue is unavailable", errWorkerOffline)
	}
	return submission, nil
}

func (r *workerRegistry) finishInput(worker *workerConnection, requestID string, command *commandResponse) {
	r.mu.Lock()
	if r.workers[worker.sessionID] != worker {
		r.mu.Unlock()
		return
	}
	submission, ok := worker.inputRequests[requestID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(worker.inputRequests, requestID)
	if command != nil && command.Sequence > worker.lastCommandSequence {
		worker.lastCommandSequence = command.Sequence
	}
	closeWorker := false
	if len(worker.inputRequests) > 0 || !worker.idlePending {
		// No deferred idle transition needs to be restored.
	} else if worker.lastCommandSequence > worker.idleCommandSequence {
		worker.idleCommandSequence = 0
		worker.idleTimeout = 0
		worker.idlePending = false
	} else if worker.idleTimeout <= 0 {
		worker.idleCommandSequence = 0
		worker.idleTimeout = 0
		worker.idlePending = false
		worker.closing = true
		closeWorker = true
	} else {
		worker.idleEpoch++
		idleEpoch := worker.idleEpoch
		commandSequence := worker.idleCommandSequence
		worker.idleTimer = time.AfterFunc(worker.idleTimeout, func() {
			r.closeIdleWorker(worker, idleEpoch, commandSequence)
		})
	}
	r.mu.Unlock()
	submission.finish()
	if closeWorker {
		// close invokes the session lifecycle callback; never run it inline with
		// an input result that may beat the handler releasing sessionOps.
		go worker.close()
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

func (c *workerConnection) trySendMessage(msg workerMessage) bool {
	select {
	case <-c.doneCh:
		return false
	default:
	}
	select {
	case <-c.doneCh:
		return false
	case c.sendCh <- msg:
		return true
	default:
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

type workerInputSubmission struct {
	worker    *workerConnection
	requestID string
	waiter    chan workerInputResult

	mu       sync.Mutex
	finished bool
	onFinish func()
}

func (s *workerInputSubmission) OnFinish(callback func()) {
	s.mu.Lock()
	if !s.finished {
		s.onFinish = callback
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	callback()
}

func (s *workerInputSubmission) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	callback := s.onFinish
	s.onFinish = nil
	s.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func (s *workerInputSubmission) Await(ctx context.Context) (commandResponse, error) {
	defer func() {
		s.worker.waitersMu.Lock()
		delete(s.worker.inputWaiters, s.requestID)
		s.worker.waitersMu.Unlock()
	}()

	var result workerInputResult
	select {
	case <-ctx.Done():
		return commandResponse{}, ctx.Err()
	case <-s.worker.doneCh:
		select {
		case result = <-s.waiter:
		default:
			return commandResponse{}, errWorkerOffline
		}
	case result = <-s.waiter:
	}
	if result.ErrorCode != "" || result.Error != "" {
		return commandResponse{}, &workerInputError{Code: result.ErrorCode, Message: result.Error}
	}
	if result.Command == nil {
		return commandResponse{}, fmt.Errorf("worker returned an empty input result")
	}
	return *result.Command, nil
}

type workerInputError struct {
	Code    string
	Message string
}

func (e *workerInputError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *workerInputError) Is(target error) bool {
	switch target {
	case errSessionBusy:
		return e.Code == "command_pending"
	case errNoActiveTurn:
		return e.Code == "no_active_turn"
	case errTurnMismatch:
		return e.Code == "turn_mismatch"
	case errCommandIDConflict:
		return e.Code == "command_id_conflict"
	default:
		return false
	}
}

func (c *workerConnection) resolveInput(result workerInputResult) {
	c.waitersMu.Lock()
	waiter := c.inputWaiters[result.RequestID]
	c.waitersMu.Unlock()
	if waiter != nil {
		select {
		case waiter <- result:
		default:
		}
	}
	c.registry.finishInput(c, result.RequestID, result.Command)
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
		if msg.Type == workerMessageInputResult && msg.InputResult != nil {
			c.registry.receive(c, msg)
			c.resolveInput(*msg.InputResult)
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
