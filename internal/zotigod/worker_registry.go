package zotigod

import (
	"fmt"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
)

const workerWriteWait = 10 * time.Second

const (
	defaultWorkerPingInterval = 15 * time.Second
	defaultWorkerPongWait     = 45 * time.Second
)

type workerMessageType string

const (
	workerMessageCommand workerMessageType = "command"
)

type workerMessage struct {
	Type    workerMessageType `json:"type"`
	Command *commandResponse  `json:"command,omitempty"`
}

type workerRegistry struct {
	mu           sync.Mutex
	workers      map[string]*workerConnection
	waiters      map[string][]chan struct{}
	pingInterval time.Duration
	pongWait     time.Duration
}

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{
		workers:      make(map[string]*workerConnection),
		waiters:      make(map[string][]chan struct{}),
		pingInterval: defaultWorkerPingInterval,
		pongWait:     defaultWorkerPongWait,
	}
}

func (r *workerRegistry) Register(sessionID string, conn *websocket.Conn) *workerConnection {
	worker := newWorkerConnection(sessionID, conn, r)

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

func (r *workerRegistry) Send(sessionID string, command commandResponse) bool {
	r.mu.Lock()
	worker := r.workers[sessionID]
	r.mu.Unlock()
	if worker == nil {
		return false
	}
	return worker.send(command)
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
	if r.workers[sessionID] == worker {
		delete(r.workers, sessionID)
	}
	r.mu.Unlock()
}

type workerConnection struct {
	sessionID string
	conn      *websocket.Conn
	registry  *workerRegistry
	sendCh    chan workerMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newWorkerConnection(sessionID string, conn *websocket.Conn, registry *workerRegistry) *workerConnection {
	return &workerConnection{
		sessionID: sessionID,
		conn:      conn,
		registry:  registry,
		sendCh:    make(chan workerMessage, 32),
		doneCh:    make(chan struct{}),
	}
}

func (c *workerConnection) send(command commandResponse) bool {
	msg := workerMessage{
		Type:    workerMessageCommand,
		Command: &command,
	}
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
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
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
