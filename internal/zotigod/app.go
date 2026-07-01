package zotigod

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
)

const defaultAddr = "127.0.0.1:8765"

type SessionState string

const (
	SessionStateCreated  SessionState = "created"
	SessionStateStarting SessionState = "starting"
	SessionStateRunning  SessionState = "running"
	SessionStatePaused   SessionState = "paused"
	SessionStateEnded    SessionState = "ended"
	SessionStateFailed   SessionState = "failed"
)

type Session struct {
	ID        string       `json:"id"`
	State     SessionState `json:"state"`
	CreatedAt time.Time    `json:"created_at"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
	Error     string       `json:"error,omitempty"`
	seq       uint64
}

var (
	errSessionNotFound          = errors.New("session not found")
	errInvalidSessionTransition = errors.New("invalid session state transition")
)

type sessionRegistry struct {
	mu       sync.Mutex
	nextID   uint64
	sessions map[string]Session
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]Session)}
}

func (r *sessionRegistry) Create() Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	session := Session{
		ID:        newZotigodID("sess"),
		State:     SessionStateCreated,
		CreatedAt: time.Now().UTC(),
		seq:       r.nextID,
	}
	r.sessions[session.ID] = session
	return session
}

func (r *sessionRegistry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	return session, ok
}

func (r *sessionRegistry) List() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	sessions := make([]Session, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].seq < sessions[j].seq
	})
	return sessions
}

func (r *sessionRegistry) Start(id string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateCreated}, func(session *Session) {
		session.State = SessionStateStarting
		session.StartedAt = &now
	})
}

func (r *sessionRegistry) MarkRunning(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateRunning
	})
}

func (r *sessionRegistry) ResumeAfterApproval(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStatePaused}, func(session *Session) {
		session.State = SessionStateRunning
	})
}

func (r *sessionRegistry) Pause(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateRunning}, func(session *Session) {
		session.State = SessionStatePaused
	})
}

func (r *sessionRegistry) End(id string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting, SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateEnded
		session.EndedAt = &now
	})
}

func (r *sessionRegistry) Fail(id string, message string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting, SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateFailed
		session.EndedAt = &now
		session.Error = message
	})
}

func (r *sessionRegistry) transition(id string, from []SessionState, apply func(*Session)) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return Session{}, errSessionNotFound
	}
	if !canTransition(session.State, from) {
		return Session{}, errInvalidSessionTransition
	}
	apply(&session)
	r.sessions[id] = session
	return session, nil
}

func canTransition(state SessionState, from []SessionState) bool {
	for _, candidate := range from {
		if state == candidate {
			return true
		}
	}
	return false
}

type handler struct {
	registry  *sessionRegistry
	approvals *approvalRegistry
	items     displayItemSource
}

type finishSessionRequest struct {
	Error string `json:"error,omitempty"`
}

// Run starts zotigod and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("zotigod", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", defaultAddr, "Address to listen on")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := log.New(os.Stderr, "[zotigod] ", log.LstdFlags)
	server := &http.Server{
		Addr:              *addr,
		Handler:           NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("Listening on http://%s", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Printf("Shutdown failed: %v", err)
			return 1
		}
		if err := <-errCh; err != nil {
			logger.Printf("Server failed: %v", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil {
			logger.Printf("Server failed: %v", err)
			return 1
		}
		return 0
	}
}

func NewHandler() http.Handler {
	items, err := newStoredDisplayItemSource()
	if err != nil {
		items = failingDisplayItemSource{err: err}
	}
	return newHandler(newSessionRegistry(), items)
}

func newHandler(registry *sessionRegistry, items displayItemSource) http.Handler {
	if items == nil {
		items = failingDisplayItemSource{err: errors.New("display item source is not configured")}
	}
	handler := &handler{
		registry:  registry,
		approvals: newApprovalRegistry(),
		items:     items,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.handleHealth)
	mux.HandleFunc("/sessions", handler.handleSessions)
	mux.HandleFunc("/sessions/", handler.handleSession)
	mux.HandleFunc("/internal/sessions/", handler.handleInternalSession)
	return mux
}

func (h *handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string][]Session{"sessions": h.registry.List()})
	case http.MethodPost:
		writeJSON(w, http.StatusCreated, h.registry.Create())
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) handleSession(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseSessionPath(r.URL.Path, "/sessions/")
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "":
		h.handleSessionGet(w, r, id)
	case "items":
		h.handleSessionItems(w, r, id)
	case "start":
		h.handleSessionStart(w, r, id)
	default:
		if approvalID, ok := strings.CutPrefix(action, "approvals/"); ok {
			h.handleApprovalDecision(w, r, id, approvalID)
			return
		}
		http.NotFound(w, r)
	}
}

func (h *handler) handleInternalSession(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseSessionPath(r.URL.Path, "/internal/sessions/")
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "worker/attach":
		h.handleWorkerAttach(w, r, id)
	case "worker/finish":
		h.handleWorkerFinish(w, r, id)
	case "approvals":
		h.handleApprovalCreate(w, r, id)
	default:
		if approvalID, ok := strings.CutPrefix(action, "approvals/"); ok {
			h.handleApprovalGet(w, r, id, approvalID)
			return
		}
		http.NotFound(w, r)
	}
}

func (h *handler) handleSessionGet(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (h *handler) handleSessionStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, err := h.registry.Start(id)
	h.writeTransition(w, session, err)
}

func (h *handler) handleSessionItems(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query, err := parseDisplayItemQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, inRegistry := h.registry.Get(id)
	items, inStore, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	if !inRegistry && !inStore {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, buildItemsResponse(items, query))
}

func (h *handler) handleWorkerAttach(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, err := h.registry.MarkRunning(id)
	h.writeTransition(w, session, err)
}

func (h *handler) handleWorkerFinish(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req finishSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Error != "" {
		session, err := h.registry.Fail(id, req.Error)
		h.writeTransition(w, session, err)
		return
	}
	session, err := h.registry.End(id)
	h.writeTransition(w, session, err)
}

func (h *handler) writeTransition(w http.ResponseWriter, session Session, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, session)
		return
	}
	switch {
	case errors.Is(err, errSessionNotFound):
		http.Error(w, "session not found", http.StatusNotFound)
	case errors.Is(err, errInvalidSessionTransition):
		http.Error(w, "invalid session state transition", http.StatusConflict)
	default:
		http.Error(w, fmt.Sprintf("update session: %v", err), http.StatusInternalServerError)
	}
}

func parseSessionPath(path string, prefix string) (id string, action string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		return "", "", false
	}
	if len(parts) == 1 {
		return parts[0], "", true
	}
	for _, part := range parts[1:] {
		if part == "" {
			return "", "", false
		}
	}
	return parts[0], strings.Join(parts[1:], "/"), true
}

func readOptionalJSON(r *http.Request, value any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return sonic.Unmarshal(data, value)
}

func readRequiredJSON(r *http.Request, value any) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return errors.New("request body is required")
	}
	return sonic.Unmarshal(data, value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := sonic.Marshal(value)
	if err != nil {
		http.Error(w, fmt.Sprintf("encode response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
