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
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

const workerWriteWait = 10 * time.Second
const defaultWorkerApprovalWait = 30 * time.Second
const workerRegistrationWait = time.Second

var (
	errWorkerOffline       = errors.New("worker is offline")
	errWorkerInputStopping = errors.New("worker input is stopping")
)

const (
	defaultWorkerPingInterval = 15 * time.Second
	defaultWorkerPongWait     = 45 * time.Second
)

type workerRegistry struct {
	mu           sync.Mutex
	workers      map[string]*workerConnection
	waiters      map[string][]chan struct{}
	onDisconnect func(string, string)
	onMessage    func(string, string, wire.Message)
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

func (r *workerRegistry) SetMessageHandler(handler func(string, string, wire.Message)) {
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

// WaitForMatch bridges the small interval between the WebSocket upgrade
// response reaching a worker and Register publishing that connection.
func (r *workerRegistry) WaitForMatch(ctxDone <-chan struct{}, sessionID string, generation string) bool {
	r.mu.Lock()
	if worker := r.workers[sessionID]; worker != nil {
		matched := !worker.closing && worker.generation == generation
		r.mu.Unlock()
		return matched
	}
	waiter := make(chan struct{})
	r.waiters[sessionID] = append(r.waiters[sessionID], waiter)
	r.mu.Unlock()

	select {
	case <-ctxDone:
		r.removeWaiter(sessionID, waiter)
		return false
	case <-waiter:
		return r.Matches(sessionID, generation)
	}
}

func (r *workerRegistry) SendConversationBoundResult(sessionID string, generation string, result wire.ConversationBoundResult) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing && worker.generation == generation
	r.mu.Unlock()
	if !available {
		return false
	}
	return worker.sendMessage(wire.Message{Type: wire.MessageConversationBoundResult, ConversationBoundResult: &result})
}

func (r *workerRegistry) SendRuntimeToolResult(sessionID string, generation string, result wire.RuntimeToolResult) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing && worker.generation == generation
	r.mu.Unlock()
	if !available {
		return false
	}
	return worker.sendMessage(wire.Message{Type: wire.MessageRuntimeToolResult, RuntimeToolResult: &result})
}

func (r *workerRegistry) Send(sessionID string, command wire.Command) bool {
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

// IsCurrentIdle validates an idle notification without scheduling eviction,
// allowing the handler to drain queued inputs before releasing the worker.
func (r *workerRegistry) IsCurrentIdle(sessionID string, generation string, idle wire.Idle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.workers[sessionID]
	return worker != nil && !worker.closing && worker.generation == generation && worker.lastCommandSequence <= idle.CommandSequence
}

func (r *workerRegistry) CloseWhenIdle(sessionID string, generation string, idle wire.Idle, timeout time.Duration) bool {
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

func (r *workerRegistry) acknowledgeDisplayBarrier(sessionID string, barrier wire.DisplayBarrier) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return false
	}
	return worker.sendMessage(wire.Message{
		Type:           wire.MessageDisplayBarrierOK,
		DisplayBarrier: &barrier,
	})
}

func (r *workerRegistry) SubmitApproval(ctx context.Context, sessionID string, approvalID string, decisions []zotigosession.DisplayApprovalDecision, stopTurn bool) (wire.ApprovalRequest, error) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return wire.ApprovalRequest{}, fmt.Errorf("approval decision requires an online worker")
	}
	if r.approvalWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.approvalWait)
		defer cancel()
	}
	return worker.submitApproval(ctx, approvalID, decisions, stopTurn)
}

func (r *workerRegistry) SubmitInteraction(ctx context.Context, sessionID, interactionID string, answers map[string][]string) (wire.InteractionRequest, error) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return wire.InteractionRequest{}, fmt.Errorf("interaction response requires an online worker")
	}
	if r.approvalWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.approvalWait)
		defer cancel()
	}
	return worker.submitInteraction(ctx, interactionID, answers)
}

func (r *workerRegistry) Has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.workers[sessionID]
	return worker != nil && !worker.closing
}

func (r *workerRegistry) InputStopping(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.workers[sessionID]
	return worker != nil && worker.inputStopping
}

func (r *workerRegistry) Close(sessionID string) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker != nil {
		worker.close()
	}
}

// CloseAsync makes the worker unavailable before returning and runs lifecycle
// callbacks asynchronously. Use it when the caller holds a Session operation
// lock that the disconnect callback also acquires.
func (r *workerRegistry) CloseAsync(sessionID string) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	if worker == nil || worker.closing {
		r.mu.Unlock()
		return false
	}
	worker.closing = true
	r.mu.Unlock()
	go worker.close()
	return true
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
	var inputStopWaiter chan struct{}
	if r.workers[sessionID] == worker {
		if worker.idleTimer != nil {
			worker.idleTimer.Stop()
			worker.idleTimer = nil
		}
		for _, submission := range worker.inputRequests {
			submissions = append(submissions, submission)
		}
		worker.inputRequests = make(map[string]*workerInputSubmission)
		inputStopWaiter = worker.inputStopWaiter
		worker.inputStopWaiter = nil
		delete(r.workers, sessionID)
		removed = true
	}
	onDisconnect := r.onDisconnect
	r.mu.Unlock()
	for _, submission := range submissions {
		submission.finish()
	}
	if inputStopWaiter != nil {
		close(inputStopWaiter)
	}
	if removed && onDisconnect != nil {
		onDisconnect(sessionID, worker.generation)
	}
}

func (r *workerRegistry) receive(worker *workerConnection, msg wire.Message) {
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
	sendCh              chan wire.Message
	doneCh              chan struct{}
	closeOnce           sync.Once
	waitersMu           sync.Mutex
	waiters             map[string]chan wire.ApprovalResult
	interactionWaiters  map[string]chan wire.InteractionResult
	inputWaiters        map[string]chan wire.InputResult
	idleTimer           *time.Timer
	idleEpoch           uint64
	idleCommandSequence uint64
	idleTimeout         time.Duration
	idlePending         bool
	inputRequests       map[string]*workerInputSubmission
	inputStopping       bool
	inputStopWaiter     chan struct{}
	lastCommandSequence uint64
	closing             bool
}

func newWorkerConnection(sessionID string, generation string, conn *websocket.Conn, registry *workerRegistry) *workerConnection {
	return &workerConnection{
		sessionID:          sessionID,
		generation:         generation,
		conn:               conn,
		registry:           registry,
		sendCh:             make(chan wire.Message, 32),
		doneCh:             make(chan struct{}),
		waiters:            make(map[string]chan wire.ApprovalResult),
		interactionWaiters: make(map[string]chan wire.InteractionResult),
		inputWaiters:       make(map[string]chan wire.InputResult),
		inputRequests:      make(map[string]*workerInputSubmission),
	}
}

func (r *workerRegistry) SubmitInput(ctx context.Context, sessionID string, request wire.InputRequest) (wire.Command, error) {
	submission, err := r.BeginInput(sessionID, request)
	if err != nil {
		return wire.Command{}, err
	}
	return submission.Await(ctx)
}

func (r *workerRegistry) BeginInput(sessionID string, request wire.InputRequest) (*workerInputSubmission, error) {
	request.RequestID = sessionlog.NewID("input_submit")
	waiter := make(chan wire.InputResult, 1)

	r.mu.Lock()
	worker := r.workers[sessionID]
	available := worker != nil && !worker.closing
	if !available {
		r.mu.Unlock()
		return nil, errWorkerOffline
	}
	if worker.inputStopping {
		r.mu.Unlock()
		return nil, errWorkerInputStopping
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

	msg := wire.Message{Type: wire.MessageInputRequest, InputRequest: &request}
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

func (r *workerRegistry) finishInput(worker *workerConnection, requestID string, command *wire.Command) {
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
	var inputStopWaiter chan struct{}
	if len(worker.inputRequests) == 0 && worker.inputStopWaiter != nil {
		inputStopWaiter = worker.inputStopWaiter
		worker.inputStopWaiter = nil
	}
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
	if inputStopWaiter != nil {
		close(inputStopWaiter)
	}
	if closeWorker {
		// close invokes the session lifecycle callback; never run it inline with
		// an input result that may beat the handler releasing sessionOps.
		go worker.close()
	}
}

func (r *workerRegistry) BeginInputStop(sessionID string, interrupt *wire.InterruptTurn) (<-chan struct{}, func(), error) {
	r.mu.Lock()
	worker := r.workers[sessionID]
	if worker == nil || worker.closing {
		r.mu.Unlock()
		return nil, nil, errWorkerOffline
	}
	if worker.inputStopping {
		r.mu.Unlock()
		return nil, nil, errWorkerInputStopping
	}
	worker.inputStopping = true
	drained := make(chan struct{})
	hasPendingInput := len(worker.inputRequests) > 0
	if hasPendingInput {
		worker.inputStopWaiter = drained
	} else {
		close(drained)
	}
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		if r.workers[sessionID] == worker {
			worker.inputStopping = false
			if worker.inputStopWaiter == drained {
				worker.inputStopWaiter = nil
			}
		}
		r.mu.Unlock()
	}
	if hasPendingInput && interrupt != nil && !worker.trySendMessage(wire.Message{Type: wire.MessageInterruptTurn, InterruptTurn: interrupt}) {
		release()
		return nil, nil, errWorkerOffline
	}
	return drained, release, nil
}

func (c *workerConnection) send(command wire.Command) bool {
	return c.sendMessage(wire.Message{
		Type:    wire.MessageCommand,
		Command: &command,
	})
}

func (c *workerConnection) sendMessage(msg wire.Message) bool {
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

func (c *workerConnection) trySendMessage(msg wire.Message) bool {
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

func (c *workerConnection) submitApproval(ctx context.Context, approvalID string, decisions []zotigosession.DisplayApprovalDecision, stopTurn bool) (wire.ApprovalRequest, error) {
	requestID := sessionlog.NewID("approval_submit")
	waiter := make(chan wire.ApprovalResult, 1)
	c.waitersMu.Lock()
	c.waiters[requestID] = waiter
	c.waitersMu.Unlock()
	defer func() {
		c.waitersMu.Lock()
		delete(c.waiters, requestID)
		c.waitersMu.Unlock()
	}()

	msg := wire.Message{
		Type: wire.MessageApprovalDecision,
		ApprovalDecision: &wire.ApprovalDecision{
			RequestID:  requestID,
			ApprovalID: approvalID,
			Decisions:  sessionlog.CopyApprovalDecisions(decisions),
			StopTurn:   stopTurn,
		},
	}
	select {
	case <-ctx.Done():
		return wire.ApprovalRequest{}, ctx.Err()
	case <-c.doneCh:
		return wire.ApprovalRequest{}, fmt.Errorf("worker disconnected")
	case c.sendCh <- msg:
	}

	select {
	case <-ctx.Done():
		return wire.ApprovalRequest{}, ctx.Err()
	case <-c.doneCh:
		return wire.ApprovalRequest{}, fmt.Errorf("worker disconnected")
	case result := <-waiter:
		if result.Error != "" {
			return wire.ApprovalRequest{}, fmt.Errorf("worker rejected approval decision: %s", result.Error)
		}
		if result.Approval == nil {
			return wire.ApprovalRequest{}, fmt.Errorf("worker returned an empty approval result")
		}
		return *result.Approval, nil
	}
}

func (c *workerConnection) submitInteraction(ctx context.Context, interactionID string, answers map[string][]string) (wire.InteractionRequest, error) {
	requestID := sessionlog.NewID("interaction_submit")
	waiter := make(chan wire.InteractionResult, 1)
	c.waitersMu.Lock()
	c.interactionWaiters[requestID] = waiter
	c.waitersMu.Unlock()
	defer func() {
		c.waitersMu.Lock()
		delete(c.interactionWaiters, requestID)
		c.waitersMu.Unlock()
	}()
	msg := wire.Message{Type: wire.MessageInteractionResponse, InteractionResponse: &wire.InteractionResponse{
		RequestID: requestID, InteractionID: interactionID, Answers: answers,
	}}
	select {
	case <-ctx.Done():
		return wire.InteractionRequest{}, ctx.Err()
	case <-c.doneCh:
		return wire.InteractionRequest{}, fmt.Errorf("worker disconnected")
	case c.sendCh <- msg:
	}
	select {
	case <-ctx.Done():
		return wire.InteractionRequest{}, ctx.Err()
	case <-c.doneCh:
		return wire.InteractionRequest{}, fmt.Errorf("worker disconnected")
	case result := <-waiter:
		if result.Error != "" {
			return wire.InteractionRequest{}, fmt.Errorf("worker rejected interaction response: %s", result.Error)
		}
		if result.Interaction == nil {
			return wire.InteractionRequest{}, fmt.Errorf("worker returned an empty interaction result")
		}
		return *result.Interaction, nil
	}
}

func (c *workerConnection) resolveApproval(result wire.ApprovalResult) {
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

func (c *workerConnection) resolveInteraction(result wire.InteractionResult) {
	c.waitersMu.Lock()
	waiter := c.interactionWaiters[result.RequestID]
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
	waiter    chan wire.InputResult

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

func (s *workerInputSubmission) Await(ctx context.Context) (wire.Command, error) {
	defer func() {
		s.worker.waitersMu.Lock()
		delete(s.worker.inputWaiters, s.requestID)
		s.worker.waitersMu.Unlock()
	}()

	var result wire.InputResult
	select {
	case <-ctx.Done():
		return wire.Command{}, ctx.Err()
	case <-s.worker.doneCh:
		select {
		case result = <-s.waiter:
		default:
			return wire.Command{}, errWorkerOffline
		}
	case result = <-s.waiter:
	}
	if result.ErrorCode != "" || result.Error != "" {
		return wire.Command{}, &workerInputError{Code: result.ErrorCode, Message: result.Error}
	}
	if result.Command == nil {
		return wire.Command{}, fmt.Errorf("worker returned an empty input result")
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
	case sessionlog.ErrNoActiveTurn:
		return e.Code == "no_active_turn"
	case sessionlog.ErrActiveTurn:
		return e.Code == "active_turn"
	case sessionlog.ErrTurnMismatch:
		return e.Code == "turn_mismatch"
	case sessionlog.ErrCommandIDConflict:
		return e.Code == "command_id_conflict"
	case sessionlog.ErrInputPolicyMismatch:
		return e.Code == "input_policy_mismatch"
	default:
		return false
	}
}

func (c *workerConnection) resolveInput(result wire.InputResult) {
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
		var msg wire.Message
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return
		}
		if msg.Type == wire.MessageApprovalResult && msg.ApprovalResult != nil {
			c.registry.receive(c, msg)
			c.resolveApproval(*msg.ApprovalResult)
			continue
		}
		if msg.Type == wire.MessageInteractionResult && msg.InteractionResult != nil {
			c.registry.receive(c, msg)
			c.resolveInteraction(*msg.InteractionResult)
			continue
		}
		if msg.Type == wire.MessageInputResult && msg.InputResult != nil {
			c.registry.receive(c, msg)
			c.resolveInput(*msg.InputResult)
			continue
		}
		c.registry.receive(c, msg)
	}
}

func (c *workerConnection) writeJSON(msg wire.Message) error {
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
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

func validateWorkerSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	return nil
}
