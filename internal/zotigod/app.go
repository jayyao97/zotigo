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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const defaultAddr = "127.0.0.1:8765"

const defaultWorkerConnectTimeout = 3 * time.Second

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
	ID               string       `json:"id"`
	State            SessionState `json:"state"`
	WorkingDirectory string       `json:"working_directory,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	StartedAt        *time.Time   `json:"started_at,omitempty"`
	EndedAt          *time.Time   `json:"ended_at,omitempty"`
	Error            string       `json:"error,omitempty"`
	seq              uint64
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

func (r *sessionRegistry) Add(session Session) Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	if session.ID == "" {
		session.ID = newZotigodID("sess")
	}
	if session.State == "" {
		session.State = SessionStateCreated
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	session.seq = r.nextID
	r.sessions[session.ID] = session
	return session
}

func newSession(workingDirectory string) Session {
	return Session{
		ID:               newZotigodID("sess"),
		State:            SessionStateCreated,
		WorkingDirectory: workingDirectory,
		CreatedAt:        time.Now().UTC(),
	}
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
	registry             *sessionRegistry
	approvals            *approvalRegistry
	items                displayItemSource
	store                zotigosession.Store
	workers              *workerRegistry
	launcher             workerLauncher
	workerConnectTimeout time.Duration
}

type createSessionRequest struct {
	WorkingDirectory string `json:"working_directory,omitempty"`
}

type finishSessionRequest struct {
	Error string `json:"error,omitempty"`
}

// Run starts zotigod and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("zotigod", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", defaultAddr, "Address to listen on")
	workerMode := fs.Bool("worker", false, "Run an internal zotigod worker")
	workerDaemonURL := fs.String("daemon-url", "", "zotigod daemon URL for internal worker mode")
	workerSessionID := fs.String("session-id", "", "zotigod session id for internal worker mode")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workerMode {
		daemonURL := *workerDaemonURL
		if daemonURL == "" {
			daemonURL = "http://" + defaultAddr
		}
		if err := runWorkerClient(context.Background(), workerClientConfig{
			DaemonURL: daemonURL,
			SessionID: *workerSessionID,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "zotigod worker failed: %v\n", err)
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "[zotigod] ", log.LstdFlags)
	launcher, err := newProcessWorkerLauncher("http://"+*addr, logger)
	if err != nil {
		logger.Printf("Worker launcher disabled: %v", err)
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           newDefaultHandler(handlerOptions{launcher: launcher}),
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
	return newDefaultHandler(handlerOptions{})
}

type handlerOptions struct {
	launcher             workerLauncher
	workers              *workerRegistry
	workerConnectTimeout time.Duration
	store                zotigosession.Store
}

func newDefaultHandler(opts handlerOptions) http.Handler {
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		opts.store = unavailableSessionStore{err: err}
		return newHandler(newSessionRegistry(), failingDisplayItemSource{err: err}, opts)
	}
	opts.store = store
	items := storedDisplayItemSource{store: store}
	return newHandler(newSessionRegistry(), items, opts)
}

type unavailableSessionStore struct {
	err error
}

func (s unavailableSessionStore) Get(context.Context, string) (*zotigosession.Session, error) {
	return nil, s.err
}

func (s unavailableSessionStore) Put(context.Context, *zotigosession.Session) error {
	return s.err
}

func (s unavailableSessionStore) AppendDisplayItem(context.Context, string, zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	return zotigosession.DisplayItem{}, s.err
}

func (s unavailableSessionStore) ListDisplayItems(context.Context, string) ([]zotigosession.DisplayItem, bool, error) {
	return nil, false, s.err
}

func (s unavailableSessionStore) Delete(context.Context, string) error {
	return s.err
}

func (s unavailableSessionStore) List(context.Context, zotigosession.ListFilter) ([]zotigosession.Metadata, error) {
	return nil, s.err
}

func (s unavailableSessionStore) Lock(context.Context, string) error {
	return s.err
}

func (s unavailableSessionStore) Unlock(context.Context, string) error {
	return s.err
}

func (s unavailableSessionStore) IsLocked(context.Context, string) (bool, error) {
	return false, s.err
}

func (s unavailableSessionStore) Close() error {
	return nil
}

func newHandler(registry *sessionRegistry, items displayItemSource, opts ...handlerOptions) http.Handler {
	if items == nil {
		items = failingDisplayItemSource{err: errors.New("display item source is not configured")}
	}
	options := handlerOptions{workerConnectTimeout: 0}
	if len(opts) > 0 {
		options = opts[0]
	}
	if options.workers == nil {
		options.workers = newWorkerRegistry()
	}
	if options.store == nil {
		if source, ok := items.(storedDisplayItemSource); ok {
			options.store = source.store
		}
	}
	if options.workerConnectTimeout == 0 && options.launcher != nil {
		options.workerConnectTimeout = defaultWorkerConnectTimeout
	}
	handler := &handler{
		registry:             registry,
		approvals:            newApprovalRegistry(),
		items:                items,
		store:                options.store,
		workers:              options.workers,
		launcher:             options.launcher,
		workerConnectTimeout: options.workerConnectTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.handleHealth)
	mux.HandleFunc("/sessions", handler.handleSessions)
	mux.HandleFunc("/sessions/", handler.handleSession)
	mux.HandleFunc("/internal/sessions/", handler.handleInternalSession)
	mux.HandleFunc("/internal/workers/connect", handler.handleWorkerConnect)
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
		var req createSessionRequest
		if err := readOptionalJSON(r, &req); err != nil {
			http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
			return
		}
		workingDirectory, err := resolveWorkingDirectory(req.WorkingDirectory)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session := newSession(workingDirectory)
		if err := h.persistSession(r.Context(), session); err != nil {
			http.Error(w, fmt.Sprintf("persist session: %v", err), http.StatusInternalServerError)
			return
		}
		session = h.registry.Add(session)
		writeJSON(w, http.StatusCreated, session)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func resolveWorkingDirectory(raw string) (string, error) {
	workDir := strings.TrimSpace(raw)
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working_directory: %w", err)
		}
		workDir = cwd
	}
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("working_directory must be an absolute path")
	}
	workDir = filepath.Clean(workDir)
	info, err := os.Stat(workDir)
	if err != nil {
		return "", fmt.Errorf("working_directory must exist: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working_directory must be a directory")
	}
	return workDir, nil
}

func (h *handler) persistSession(ctx context.Context, session Session) error {
	if h.store == nil {
		return nil
	}
	return h.store.Put(ctx, &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               session.ID,
			WorkingDirectory: session.WorkingDirectory,
			CreatedAt:        session.CreatedAt,
			UpdatedAt:        session.CreatedAt,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: session.CreatedAt,
		},
		Turns: make([]zotigosession.Turn, 0),
	})
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
	case "messages":
		h.handleSessionMessage(w, r, id)
	case "pause":
		h.handleSessionPause(w, r, id)
	case "start":
		h.handleSessionStart(w, r, id)
	case "steering":
		h.handleSessionSteering(w, r, id)
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
	case "commands":
		h.handleWorkerCommands(w, r, id)
	case "turn/interrupted":
		h.handleWorkerTurnInterrupted(w, r, id)
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
	if err != nil {
		h.writeTransition(w, session, err)
		return
	}
	if err := h.launchWorker(r.Context(), id); err != nil {
		_, _ = h.registry.Fail(id, err.Error())
		http.Error(w, fmt.Sprintf("start worker: %v", err), http.StatusInternalServerError)
		return
	}
	if h.launcher != nil && !h.waitForWorker(r.Context(), id) {
		msg := "worker did not connect before timeout"
		_, _ = h.registry.Fail(id, msg)
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}
	if running, ok := h.registry.Get(id); ok {
		session = running
	}
	writeJSON(w, http.StatusOK, session)
}

func (h *handler) launchWorker(ctx context.Context, id string) error {
	if h.launcher == nil {
		return nil
	}
	return h.launcher.Start(ctx, id, h.sessionWorkingDirectory(ctx, id))
}

func (h *handler) sessionWorkingDirectory(ctx context.Context, id string) string {
	if session, ok := h.registry.Get(id); ok && session.WorkingDirectory != "" {
		return session.WorkingDirectory
	}
	if h.store != nil {
		if session, err := h.store.Get(ctx, id); err == nil && session != nil && session.WorkingDirectory != "" {
			return session.WorkingDirectory
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

func (h *handler) waitForWorker(ctx context.Context, id string) bool {
	if h.workerConnectTimeout <= 0 {
		return h.workers.Has(id)
	}
	waitCtx, cancel := context.WithTimeout(ctx, h.workerConnectTimeout)
	defer cancel()
	return h.workers.Wait(waitCtx.Done(), id)
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

	h.approvals.mu.Lock()
	defer h.approvals.mu.Unlock()

	if req.Error != "" {
		session, err := h.registry.Fail(id, req.Error)
		if err == nil {
			h.workers.Close(id)
		}
		h.writeTransition(w, session, err)
		return
	}
	session, err := h.registry.End(id)
	if err == nil {
		h.workers.Close(id)
	}
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
