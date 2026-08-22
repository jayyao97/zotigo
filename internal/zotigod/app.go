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
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/codexapp"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

const defaultAddr = "127.0.0.1:8765"

const defaultWorkerConnectTimeout = 3 * time.Second

const apiProtocolVersion = "1"

type SessionState string

const (
	SessionStateCreated  SessionState = "created"
	SessionStateStarting SessionState = "starting"
	SessionStateRunning  SessionState = "running"
	SessionStatePaused   SessionState = "paused"
	SessionStateOffline  SessionState = "offline"
	SessionStateEnded    SessionState = "ended"
	SessionStateFailed   SessionState = "failed"
)

type Session struct {
	ID               string               `json:"id"`
	State            SessionState         `json:"state"`
	Live             bool                 `json:"live"`
	WorkingDirectory string               `json:"working_directory,omitempty"`
	Agent            string               `json:"agent"`
	ProfileName      string               `json:"profile,omitempty"`
	Model            string               `json:"model,omitempty"`
	ReasoningEffort  string               `json:"reasoning_effort,omitempty"`
	ApprovalPolicy   agent.ApprovalPolicy `json:"approval_policy"`
	CreatedAt        time.Time            `json:"created_at"`
	StartedAt        *time.Time           `json:"started_at,omitempty"`
	EndedAt          *time.Time           `json:"ended_at,omitempty"`
	Error            string               `json:"error,omitempty"`
	seq              uint64
}

var (
	errSessionNotFound               = errors.New("session not found")
	errInvalidSessionTransition      = errors.New("invalid session state transition")
	errSessionProfileNotFound        = errors.New("session profile not found")
	errWorkerDisconnectedBeforeReady = errors.New("worker disconnected before becoming ready")
)

type sessionRegistry struct {
	mu       sync.Mutex
	nextID   uint64
	sessions map[string]Session
	changed  chan struct{}
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{
		sessions: make(map[string]Session),
		changed:  make(chan struct{}),
	}
}

func (r *sessionRegistry) Add(session Session) Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.addLocked(session)
}

func (r *sessionRegistry) GetOrAdd(session Session) Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	if session.ID != "" {
		if existing, ok := r.sessions[session.ID]; ok {
			return existing
		}
	}
	return r.addLocked(session)
}

func (r *sessionRegistry) addLocked(session Session) Session {
	r.nextID++
	if session.ID == "" {
		session.ID = newZotigodID("sess")
	}
	if session.State == "" {
		session.State = SessionStateCreated
	}
	if session.ApprovalPolicy == "" {
		session.ApprovalPolicy = agent.ApprovalPolicyAuto
	}
	session.Live = true
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	session.seq = r.nextID
	r.sessions[session.ID] = session
	r.notifyChangedLocked()
	return session
}

func newSession(workingDirectory string, profileName string) Session {
	return Session{
		ID:               newZotigodID("sess"),
		State:            SessionStateCreated,
		Live:             true,
		WorkingDirectory: workingDirectory,
		Agent:            string(zotigoruntime.AgentZotigo),
		ProfileName:      profileName,
		ApprovalPolicy:   agent.ApprovalPolicyAuto,
		CreatedAt:        time.Now().UTC(),
	}
}

func (r *sessionRegistry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	return session, ok
}

func (r *sessionRegistry) Watch(id string) (Session, bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	return session, ok, r.changed
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

func (r *sessionRegistry) RestartWorker(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateStarting
	})
}

func (r *sessionRegistry) ResumeAfterApproval(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStatePaused}, func(session *Session) {
		session.State = SessionStateRunning
	})
}

func (r *sessionRegistry) Pause(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateStarting, SessionStateRunning}, func(session *Session) {
		session.State = SessionStatePaused
	})
}

func (r *sessionRegistry) UpdateProfile(id string, profileName string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.ProfileName = profileName
	})
}

func (r *sessionRegistry) UpdateApprovalPolicy(id string, policy agent.ApprovalPolicy) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.ApprovalPolicy = policy
	})
}

func (r *sessionRegistry) UpdateCodexSettings(id string, model string, reasoningEffort string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePaused}, func(session *Session) {
		session.Model = model
		session.ReasoningEffort = reasoningEffort
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

func (r *sessionRegistry) FailStarting(id string, message string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateFailed
		session.EndedAt = &now
		session.Error = message
	})
}

func (r *sessionRegistry) ResetStarting(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateCreated
		session.StartedAt = nil
		session.EndedAt = nil
		session.Error = ""
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
	r.notifyChangedLocked()
	return session, nil
}

func (r *sessionRegistry) notifyChangedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
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
	items                displayItemSource
	store                zotigosession.Store
	workers              *workerRegistry
	launcher             workerLauncher
	runtimes             *runtimeRegistry
	workerConnectTimeout time.Duration
	sessionOps           *sessionOperationLocks
	workspaceOps         *sessionOperationLocks
	approvalOps          *sessionOperationLocks
	titleSuggestion      titleSuggestionFunc
	titleTimeout         time.Duration
	events               *displayEventBroker
	catalog              *zotigoworkspace.Store
	catalogErr           error
	logger               *log.Logger
}

type createSessionRequest struct {
	WorkingDirectory string               `json:"working_directory,omitempty"`
	Profile          string               `json:"profile,omitempty"`
	ApprovalPolicy   agent.ApprovalPolicy `json:"approval_policy,omitempty"`
	WorkspaceID      string               `json:"workspace_id,omitempty"`
	Agent            string               `json:"agent,omitempty"`
	Model            string               `json:"model,omitempty"`
	ReasoningEffort  string               `json:"reasoning_effort,omitempty"`
}

type finishSessionRequest struct {
	Error      string `json:"error,omitempty"`
	Generation string `json:"generation,omitempty"`
}

type workerReadyRequest struct {
	Generation string `json:"generation"`
}

// Run starts zotigod and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("zotigod", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", defaultAddr, "Address to listen on")
	authTokenFile := fs.String("auth-token-file", "", "File containing the bearer token required by public APIs")
	workerCallbackURL := fs.String("worker-daemon-url", "", "Daemon URL used by locally spawned workers")
	workerMode := fs.Bool("worker", false, "Run an internal zotigod worker")
	workerDaemonURL := fs.String("daemon-url", "", "zotigod daemon URL for internal worker mode")
	workerSessionID := fs.String("session-id", "", "zotigod session id for internal worker mode")
	sessionStoreRoot := fs.String("session-store-root", "", "Session store root for internal worker mode")
	codexWorkerMode := fs.Bool("codex-worker", false, "Run an internal Codex bridge worker")
	codexSocket := fs.String("codex-socket", "", "Codex app-server Unix socket")
	codexProjectID := fs.String("codex-project-id", "", "Codex Project id")
	codexWorkingDirectory := fs.String("codex-working-directory", "", "Codex worker working directory")
	codexModel := fs.String("codex-model", "", "Codex model")
	codexReasoningEffort := fs.String("codex-reasoning-effort", "", "Codex reasoning effort")
	codexThreadID := fs.String("codex-thread-id", "", "Existing Codex thread id")
	workspaceBindingRevision := fs.Uint64("workspace-binding-revision", 0, "Workspace binding revision")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workerMode {
		daemonURL := *workerDaemonURL
		if daemonURL == "" {
			daemonURL = "http://" + defaultAddr
		}
		workerAuthToken := os.Getenv(workerAuthTokenEnv)
		_ = os.Unsetenv(workerAuthTokenEnv)
		if err := runWorkerClient(context.Background(), workerClientConfig{
			DaemonURL: daemonURL,
			SessionID: *workerSessionID,
			AuthToken: workerAuthToken,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "zotigod worker failed: %v\n", err)
			return 1
		}
		return 0
	}
	if *codexWorkerMode {
		daemonURL := *workerDaemonURL
		if daemonURL == "" {
			daemonURL = "http://" + defaultAddr
		}
		workerAuthToken := os.Getenv(workerAuthTokenEnv)
		_ = os.Unsetenv(workerAuthTokenEnv)
		if err := runCodexWorkerClient(context.Background(), codexWorkerConfig{
			workerClientConfig: workerClientConfig{DaemonURL: daemonURL, SessionID: *workerSessionID, AuthToken: workerAuthToken},
			SocketPath:         *codexSocket, ProjectID: *codexProjectID,
			WorkingDirectory: *codexWorkingDirectory, Model: *codexModel,
			ReasoningEffort: *codexReasoningEffort, ThreadID: *codexThreadID,
			WorkspaceRevision: *workspaceBindingRevision,
			SessionStoreRoot:  *sessionStoreRoot,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "zotigod Codex worker failed: %v\n", err)
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "[zotigod] ", log.LstdFlags)
	var publicAuthToken string
	if strings.TrimSpace(*authTokenFile) != "" {
		var err error
		publicAuthToken, err = loadAuthToken(*authTokenFile)
		if err != nil {
			logger.Printf("Authentication configuration failed: %v", err)
			return 1
		}
	}
	if listenAddressNeedsAuth(*addr) && publicAuthToken == "" {
		logger.Printf("Authentication configuration failed: non-loopback listen address requires --auth-token-file")
		return 1
	}
	workerAuthToken, err := generateAuthToken()
	if err != nil {
		logger.Printf("Worker authentication initialization failed: %v", err)
		return 1
	}
	workerCallback, err := resolveWorkerDaemonURL(*addr, *workerCallbackURL)
	if err != nil {
		logger.Printf("Worker daemon URL configuration failed: %v", err)
		return 1
	}
	configPath, created, err := config.NewManager().EnsureGlobalConfig()
	if err != nil {
		logger.Printf("Config initialization failed: %v", err)
		return 1
	}
	if created {
		logger.Printf("Created default config template at %s. Add a profile, set default_profile, and configure its API key before creating sessions.", configPath)
	}
	launcher, err := newProcessWorkerLauncher(workerCallback, workerAuthToken, logger)
	if err != nil {
		logger.Printf("Worker launcher disabled: %v", err)
	}
	if launcher != nil {
		defer launcher.Close()
	}
	runtimes := newRuntimeRegistry(nativeRuntimeAdapter{launcher: launcher})
	var codexHost *codexapp.Host
	if binaryPath, _, discoverErr := codexapp.Discover(); discoverErr == nil && launcher != nil {
		cacheDir, cacheErr := os.UserCacheDir()
		if cacheErr == nil {
			codexHost, cacheErr = codexapp.NewHost(binaryPath, filepath.Join(cacheDir, "zotigod", "runtime"), os.Stderr)
		}
		if cacheErr != nil {
			logger.Printf("Codex runtime disabled: %v", cacheErr)
		} else {
			codexAdapter := codexapp.NewAdapter(codexHost, launcher.StartCodex)
			runtimes = newRuntimeRegistry(nativeRuntimeAdapter{launcher: launcher}, codexAdapter)
		}
	}
	if codexHost != nil {
		defer func() { _ = codexHost.Close() }()
	}
	server := &http.Server{
		Addr: *addr,
		Handler: newDefaultHandler(handlerOptions{
			launcher:        launcher,
			runtimes:        runtimes,
			publicAuthToken: publicAuthToken,
			workerAuthToken: workerAuthToken,
			logger:          logger,
		}),
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
	runtimes             *runtimeRegistry
	workers              *workerRegistry
	workerConnectTimeout time.Duration
	store                zotigosession.Store
	sessionOps           *sessionOperationLocks
	workspaceOps         *sessionOperationLocks
	titleSuggestion      titleSuggestionFunc
	titleTimeout         time.Duration
	events               *displayEventBroker
	publicAuthToken      string
	workerAuthToken      string
	catalog              *zotigoworkspace.Store
	catalogErr           error
	logger               *log.Logger
}

func newDefaultHandler(opts handlerOptions) http.Handler {
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		opts.store = unavailableSessionStore{err: err}
		return newHandler(newSessionRegistry(), failingDisplayItemSource{err: err}, opts)
	}
	opts.store = store
	catalog, catalogErr := zotigoworkspace.Open(store.RootDir())
	opts.catalog = catalog
	opts.catalogErr = catalogErr
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
	if options.runtimes == nil {
		options.runtimes = newRuntimeRegistry(nativeRuntimeAdapter{launcher: options.launcher})
	}
	if options.sessionOps == nil {
		options.sessionOps = newSessionOperationLocks()
	}
	if options.workspaceOps == nil {
		options.workspaceOps = newSessionOperationLocks()
	}
	if options.titleSuggestion == nil {
		options.titleSuggestion = generateTitleSuggestion
	}
	if options.titleTimeout == 0 {
		options.titleTimeout = defaultTitleSuggestionTimeout
	}
	if options.events == nil {
		options.events = newDisplayEventBroker()
	}
	eventingItems := eventingDisplayItemSource{source: items, events: options.events}
	if offsetItems, ok := items.(offsetDisplayItemSource); ok {
		items = eventingOffsetDisplayItemSource{eventingDisplayItemSource: eventingItems, offset: offsetItems}
	} else {
		items = eventingItems
	}
	handler := &handler{
		registry:             registry,
		items:                items,
		store:                options.store,
		workers:              options.workers,
		launcher:             options.launcher,
		runtimes:             options.runtimes,
		workerConnectTimeout: options.workerConnectTimeout,
		sessionOps:           options.sessionOps,
		workspaceOps:         options.workspaceOps,
		approvalOps:          newSessionOperationLocks(),
		titleSuggestion:      options.titleSuggestion,
		titleTimeout:         options.titleTimeout,
		events:               options.events,
		catalog:              options.catalog,
		catalogErr:           options.catalogErr,
		logger:               options.logger,
	}
	handler.workers.SetDisconnectHandler(handler.handleWorkerDisconnect)
	handler.workers.SetMessageHandler(func(sessionID string, generation string, msg workerMessage) {
		switch msg.Type {
		case workerMessageDelta:
			if msg.Delta == nil || msg.Delta.ItemID == "" || msg.Delta.Delta == "" {
				return
			}
			switch msg.Delta.PartType {
			case string(protocol.ContentTypeText), string(protocol.ContentTypeReasoning):
				handler.events.PublishDelta(sessionID, *msg.Delta)
			}
		case workerMessageDisplayWake:
			handler.events.Wake(sessionID)
		case workerMessageDisplayBarrier:
			if msg.DisplayBarrier != nil && msg.DisplayBarrier.ID != "" {
				handler.events.WakeBarrier(sessionID)
				handler.workers.acknowledgeDisplayBarrier(sessionID, *msg.DisplayBarrier)
			}
		case workerMessageApprovalRequest:
			if msg.ApprovalRequest != nil && msg.ApprovalRequest.Status == approvalStatusPending {
				unlock := handler.sessionOps.lock(sessionID)
				_, _ = handler.registry.Pause(sessionID)
				unlock()
			}
		case workerMessageApprovalResult:
			if msg.ApprovalResult != nil && msg.ApprovalResult.Error == "" && msg.ApprovalResult.Approval != nil && msg.ApprovalResult.Approval.Status == approvalStatusResolved {
				unlock := handler.sessionOps.lock(sessionID)
				_, _ = handler.registry.ResumeAfterApproval(sessionID)
				unlock()
			}
		case workerMessageConversationBound:
			handler.handleConversationBound(sessionID, generation, msg.ConversationBound)
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.handleHealth)
	mux.HandleFunc("/agents", handler.handleAgents)
	mux.HandleFunc("/agents/codex/prepare", handler.handleCodexPrepare)
	mux.HandleFunc("/config/profiles", handler.handleProfiles)
	mux.HandleFunc("/sources/inspect", handler.handleSourceInspection)
	mux.HandleFunc("/projects", handler.handleProjects)
	mux.HandleFunc("/projects/{$}", handler.handleProjectNotFound)
	handleOptionalTrailingSlash(mux, "/projects/{project_id}", withPathValue("project_id", handler.handleProject))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/archive-preview", withPathValue("project_id", handler.handleProjectArchivePreview))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/archive", withPathValue("project_id", handler.handleProjectArchive))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/unarchive", withPathValue("project_id", handler.handleProjectUnarchive))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/delete-preview", withPathValue("project_id", handler.handleProjectDeletePreview))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/delete", withPathValue("project_id", handler.handleProjectDelete))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/sources/inspect", withPathValue("project_id", handler.handleProjectSourceInspection))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/sources", withPathValue("project_id", handler.handleProjectSources))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/sources/{source_id}", withPathValues("project_id", "source_id", handler.handleProjectSource))
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/workspaces", withPathValue("project_id", handler.handleProjectWorkspaces))
	mux.HandleFunc("/projects/{project_id}/{route...}", handler.handleProjectRouteNotFound)
	mux.HandleFunc("/workspaces/{$}", handler.handleWorkspaceRouteNotFound)
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}", withPathValue("workspace_id", handler.handleWorkspace))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/sources", withPathValue("workspace_id", handler.handleWorkspaceSources))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/archive-preview", withPathValue("workspace_id", handler.handleWorkspaceArchivePreview))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/archive", withPathValue("workspace_id", handler.handleWorkspaceArchive))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/unarchive", withPathValue("workspace_id", handler.handleWorkspaceUnarchive))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/delete-preview", withPathValue("workspace_id", handler.handleWorkspaceDeletePreview))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/delete", withPathValue("workspace_id", handler.handleWorkspaceDelete))
	handleOptionalTrailingSlash(mux, "/workspaces/{workspace_id}/retry", withPathValue("workspace_id", handler.handleWorkspaceRetry))
	mux.HandleFunc("/workspaces/{workspace_id}/{route...}", handler.handleWorkspaceRouteNotFound)
	mux.HandleFunc("/catalog/sessions", handler.handleCatalogSessions)
	mux.HandleFunc("/catalog/sessions/{$}", handler.handleCatalogSessionNotFound)
	handleOptionalTrailingSlash(mux, "/catalog/sessions/{id}", withPathValue("id", handler.handleCatalogSession))
	mux.HandleFunc("/catalog/sessions/{id}/{route...}", handler.handleCatalogSessionNotFound)
	mux.HandleFunc("/sessions", handler.handleSessions)
	mux.HandleFunc("/sessions/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/sessions/{id}", withPathValue("id", handler.handleSessionGet))
	mux.HandleFunc("/sessions/{id}/items", withPathValue("id", handler.handleSessionItems))
	mux.HandleFunc("/sessions/{id}/events", withPathValue("id", handler.handleSessionEvents))
	mux.HandleFunc("/sessions/{id}/messages", withPathValue("id", handler.handleSessionMessage))
	mux.HandleFunc("/sessions/{id}/pause", withPathValue("id", handler.handleSessionPause))
	mux.HandleFunc("/sessions/{id}/profile", withPathValue("id", handler.handleSessionProfile))
	mux.HandleFunc("/sessions/{id}/codex-settings", withPathValue("id", handler.handleSessionCodexSettings))
	mux.HandleFunc("/sessions/{id}/approval-policy", withPathValue("id", handler.handleSessionApprovalPolicy))
	mux.HandleFunc("/sessions/{id}/start", withPathValue("id", handler.handleSessionStart))
	mux.HandleFunc("/sessions/{id}/steering", withPathValue("id", handler.handleSessionSteering))
	mux.HandleFunc("/sessions/{id}/title-suggestion", withPathValue("id", handler.handleSessionTitleSuggestion))
	mux.HandleFunc("/sessions/{id}/title", withPathValue("id", handler.handleSessionOrganizationTitle))
	mux.HandleFunc("/sessions/{id}/pinned", withPathValue("id", handler.handleSessionOrganizationPinned))
	mux.HandleFunc("/sessions/{id}/position", withPathValue("id", handler.handleSessionOrganizationPosition))
	mux.HandleFunc("/sessions/{id}/archive", withPathValue("id", func(w http.ResponseWriter, r *http.Request, id string) {
		handler.handleSessionOrganizationArchive(w, r, id, true)
	}))
	mux.HandleFunc("/sessions/{id}/unarchive", withPathValue("id", func(w http.ResponseWriter, r *http.Request, id string) {
		handler.handleSessionOrganizationArchive(w, r, id, false)
	}))
	mux.HandleFunc("/sessions/{id}/images/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/sessions/{id}/images/{name...}", withPathValues("id", "name", handler.handleSessionImage))
	mux.HandleFunc("/sessions/{id}/approvals/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/sessions/{id}/approvals/{approval_id...}", withPathValues("id", "approval_id", handler.handleApprovalDecision))
	mux.HandleFunc("/sessions/{id}/{route...}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/internal/sessions/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/internal/sessions/{id}/commands", withPathValue("id", handler.handleWorkerCommands))
	mux.HandleFunc("/internal/sessions/{id}/turn/interrupted", withPathValue("id", handler.handleWorkerTurnInterrupted))
	mux.HandleFunc("/internal/sessions/{id}/worker/attach", withPathValue("id", handler.handleWorkerAttach))
	mux.HandleFunc("/internal/sessions/{id}/worker/finish", withPathValue("id", handler.handleWorkerFinish))
	mux.HandleFunc("/internal/sessions/{id}/{route...}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/internal/workers/connect", handler.handleWorkerConnect)
	return authenticatedHandler(mux, options.publicAuthToken, options.workerAuthToken)
}

func handleOptionalTrailingSlash(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	mux.HandleFunc(pattern, handler)
	mux.HandleFunc(pattern+"/{$}", handler)
}

func withPathValue(name string, next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value := r.PathValue(name)
		// ServeMux decodes each wildcard segment, so an escaped slash would
		// otherwise turn one path segment into a multi-segment identifier.
		if strings.Contains(value, "/") {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r, value)
	}
}

func withPathValues(first string, second string, next func(http.ResponseWriter, *http.Request, string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		firstValue := r.PathValue(first)
		if strings.Contains(firstValue, "/") {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r, firstValue, r.PathValue(second))
	}
}

func (h *handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{
		"status":           "ok",
		"protocol_version": apiProtocolVersion,
	})
}

func (h *handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions, err := h.listSessions(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]Session{"sessions": sessions})
	case http.MethodPost:
		var req createSessionRequest
		if err := readOptionalJSON(r, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
			return
		}
		workingDirectory := ""
		var assignedWorkspaceID string
		var assignedWorkspace *zotigoworkspace.Workspace
		if req.WorkspaceID != "" {
			if !h.requireCatalog(w) {
				return
			}
			workspace, releaseWorkspace, workspaceErr := h.lockWorkspaceForUse(r.Context(), req.WorkspaceID)
			if workspaceErr != nil {
				h.writeCatalogError(w, workspaceErr)
				return
			}
			defer releaseWorkspace()
			if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
				writeAPIError(w, http.StatusConflict, "workspace is not ready")
				return
			}
			if req.WorkingDirectory != "" && filepath.Clean(req.WorkingDirectory) != filepath.Clean(workspace.RootPath) {
				writeAPIError(w, http.StatusBadRequest, "working_directory must match the selected workspace")
				return
			}
			workingDirectory = workspace.RootPath
			assignedWorkspaceID = workspace.ID
			assignedWorkspace = &workspace
		} else {
			var err error
			workingDirectory, err = resolveWorkingDirectory(req.WorkingDirectory)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		approvalPolicy, err := normalizeSessionApprovalPolicy(req.ApprovalPolicy, true)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		agentKind := zotigoruntime.AgentKind(strings.TrimSpace(req.Agent))
		if agentKind == "" {
			agentKind = zotigoruntime.AgentZotigo
		}
		var profileName string
		switch agentKind {
		case zotigoruntime.AgentZotigo:
			if req.Model != "" || req.ReasoningEffort != "" {
				writeAPIError(w, http.StatusBadRequest, "model and reasoning_effort require agent codex")
				return
			}
			appConfig, configErr := config.NewManager().LoadForDir(workingDirectory)
			if configErr != nil {
				writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", configErr))
				return
			}
			profileName, _, err = appConfig.ResolveProfile(req.Profile)
			if err != nil {
				if strings.TrimSpace(req.Profile) != "" {
					writeAPIError(w, http.StatusBadRequest, err.Error())
				} else {
					writeAPIError(w, http.StatusInternalServerError, "default "+err.Error())
				}
				return
			}
		case zotigoruntime.AgentCodex:
			if assignedWorkspaceID == "" {
				writeAPIError(w, http.StatusBadRequest, "codex sessions require workspace_id")
				return
			}
			if strings.TrimSpace(req.Profile) != "" {
				writeAPIError(w, http.StatusBadRequest, "profile is only valid for agent zotigo")
				return
			}
			if strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.ReasoningEffort) == "" {
				writeAPIError(w, http.StatusBadRequest, "codex sessions require model and reasoning_effort")
				return
			}
			if _, adapterErr := h.runtimes.adapter(agentKind); adapterErr != nil {
				writeAPIError(w, http.StatusServiceUnavailable, adapterErr.Error())
				return
			}
			if h.store == nil || h.sessionStoreRoot() == "" {
				writeAPIError(w, http.StatusServiceUnavailable, "codex sessions require persistent session storage")
				return
			}
			if req.ApprovalPolicy != "" && approvalPolicy != agent.ApprovalPolicyBypass {
				writeAPIError(w, http.StatusBadRequest, "codex sessions do not support approval callbacks; approval_policy must be bypass or omitted")
				return
			}
			approvalPolicy = agent.ApprovalPolicyBypass
			if err := h.validateCodexSettings(r.Context(), strings.TrimSpace(req.Model), strings.TrimSpace(req.ReasoningEffort)); err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			binding, err := h.prepareRuntimeWorkspace(r.Context(), *assignedWorkspace, agentKind)
			if err != nil {
				if errors.Is(err, zotigoruntime.ErrWorkspaceConflict) {
					writeAPIErrorCode(w, http.StatusConflict, "runtime_workspace_conflict", err.Error())
					return
				}
				writeAPIError(w, http.StatusServiceUnavailable, fmt.Sprintf("prepare codex project: %v", err))
				return
			}
			if err := h.fenceRuntimeWorkspaceWorkers(r.Context(), assignedWorkspace.ID, binding.Revision); err != nil {
				writeAPIError(w, http.StatusInternalServerError, err.Error())
				return
			}
			profileName = ""
		default:
			writeAPIError(w, http.StatusBadRequest, "unsupported agent")
			return
		}
		session := newSession(workingDirectory, profileName)
		session.Agent = string(agentKind)
		session.Model = strings.TrimSpace(req.Model)
		session.ReasoningEffort = strings.TrimSpace(req.ReasoningEffort)
		session.ApprovalPolicy = approvalPolicy
		if err := h.persistSession(r.Context(), session); err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("persist session: %v", err))
			return
		}
		if assignedWorkspaceID != "" {
			if _, err := h.catalog.AssignSession(r.Context(), session.ID, assignedWorkspaceID); err != nil {
				if h.store != nil {
					_ = h.store.Delete(r.Context(), session.ID)
				}
				h.writeCatalogError(w, err)
				return
			}
		}
		session = h.registry.Add(session)
		writeAPIJSON(w, http.StatusCreated, session)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
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
	storedProfile := session.ProfileName
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		storedProfile = "__zotigo_backend__:codex"
	}
	return h.store.Put(ctx, &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               session.ID,
			WorkingDirectory: session.WorkingDirectory,
			Agent:            session.Agent,
			ProfileName:      storedProfile,
			Model:            session.Model,
			ReasoningEffort:  session.ReasoningEffort,
			ApprovalPolicy:   session.ApprovalPolicy,
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

func (h *handler) listSessions(ctx context.Context) ([]Session, error) {
	registrySessions := h.registry.List()
	seen := make(map[string]struct{}, len(registrySessions))
	registryIndex := make(map[string]int, len(registrySessions))
	for idx := range registrySessions {
		registrySessions[idx].Live = true
		seen[registrySessions[idx].ID] = struct{}{}
		registryIndex[registrySessions[idx].ID] = idx
	}
	if h.store == nil {
		return registrySessions, nil
	}
	metadata, err := h.store.List(ctx, zotigosession.ListFilter{OrderBy: zotigosession.OrderByUpdatedDesc})
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(registrySessions))
	copy(sessions, registrySessions)
	for _, meta := range metadata {
		if _, ok := seen[meta.ID]; ok {
			sessions[registryIndex[meta.ID]].Agent = meta.Agent
			if meta.Agent == string(zotigoruntime.AgentCodex) {
				sessions[registryIndex[meta.ID]].ProfileName = ""
			} else {
				sessions[registryIndex[meta.ID]].ProfileName = meta.ProfileName
			}
			sessions[registryIndex[meta.ID]].Model = meta.Model
			sessions[registryIndex[meta.ID]].ReasoningEffort = meta.ReasoningEffort
			sessions[registryIndex[meta.ID]].ApprovalPolicy = meta.ApprovalPolicy
			continue
		}
		sessions = append(sessions, sessionFromMetadata(meta, SessionStateOffline, false))
	}
	return sessions, nil
}

func sessionFromMetadata(meta zotigosession.Metadata, state SessionState, live bool) Session {
	agentKind := meta.Agent
	if agentKind == "" {
		agentKind = string(zotigoruntime.AgentZotigo)
	}
	profileName := meta.ProfileName
	if agentKind == string(zotigoruntime.AgentCodex) {
		profileName = ""
	}
	approvalPolicy := meta.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = agent.ApprovalPolicyAuto
	}
	return Session{
		ID:               meta.ID,
		State:            state,
		Live:             live,
		WorkingDirectory: meta.WorkingDirectory,
		Agent:            agentKind,
		ProfileName:      profileName,
		Model:            meta.Model,
		ReasoningEffort:  meta.ReasoningEffort,
		ApprovalPolicy:   approvalPolicy,
		CreatedAt:        meta.CreatedAt,
	}
}

func (h *handler) storedSession(ctx context.Context, id string) (Session, bool, error) {
	if h.store == nil {
		return Session{}, false, nil
	}
	session, err := h.store.Get(ctx, id)
	if err != nil {
		return Session{}, false, err
	}
	if session == nil {
		return Session{}, false, nil
	}
	return sessionFromMetadata(session.Metadata, SessionStateOffline, false), true, nil
}

func (h *handler) loadSessionIntoRegistry(ctx context.Context, id string) (Session, bool, error) {
	if session, ok := h.registry.Get(id); ok {
		session.Live = true
		return session, true, nil
	}
	stored, ok, err := h.storedSession(ctx, id)
	if err != nil || !ok {
		return Session{}, ok, err
	}
	stored.State = SessionStateCreated
	stored.Live = true
	return h.registry.GetOrAdd(stored), true, nil
}

func (h *handler) handleSessionRouteNotFound(w http.ResponseWriter, _ *http.Request) {
	writeAPIError(w, http.StatusNotFound, "not found")
}

func (h *handler) handleSessionGet(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		stored, inStore, err := h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !inStore {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
		writeAPIJSON(w, http.StatusOK, stored)
		return
	}
	if h.store != nil {
		stored, err := h.store.Get(r.Context(), id)
		if err == nil && stored != nil {
			session.Agent = stored.Agent
			if stored.Agent != string(zotigoruntime.AgentCodex) {
				session.ProfileName = stored.ProfileName
			}
			session.Model = stored.Model
			session.ReasoningEffort = stored.ReasoningEffort
			session.ApprovalPolicy = stored.ApprovalPolicy
		}
	}
	session.Live = true
	writeAPIJSON(w, http.StatusOK, session)
}

func (h *handler) handleSessionStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, err := h.ensureSessionRunning(r.Context(), id)
	if err != nil {
		h.writeEnsureRunningError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, session)
}

var (
	errWorkerConnectTimeout = errors.New("worker did not connect before timeout")
	errSessionUnavailable   = errors.New("session is unavailable")
)

func (h *handler) ensureSessionRunning(ctx context.Context, id string) (Session, error) {
	releaseWorkspace := func() {}
	var assignedWorkspace *zotigoworkspace.Workspace
	if h.catalog != nil {
		organization, err := h.catalog.GetSessionOrganization(ctx, id)
		if err == nil && organization.WorkspaceID != nil {
			workspace, release, lockErr := h.lockWorkspaceForUse(ctx, *organization.WorkspaceID)
			err = lockErr
			releaseWorkspace = release
			if err != nil {
				return Session{}, fmt.Errorf("lock session workspace: %w", err)
			}
			assignedWorkspace = &workspace
		} else if err != nil && !errors.Is(err, zotigoworkspace.ErrNotFound) {
			return Session{}, fmt.Errorf("load session organization: %w", err)
		}
	}
	defer releaseWorkspace()
	if assignedWorkspace != nil {
		stored, found, err := h.storedSession(ctx, id)
		if err != nil {
			return Session{}, fmt.Errorf("load session runtime: %w", err)
		}
		if found && zotigoruntime.AgentKind(stored.Agent) == zotigoruntime.AgentCodex {
			binding, err := h.prepareRuntimeWorkspace(ctx, *assignedWorkspace, zotigoruntime.AgentCodex)
			if err != nil {
				return Session{}, fmt.Errorf("prepare codex project: %w", err)
			}
			if err := h.fenceRuntimeWorkspaceWorkers(ctx, assignedWorkspace.ID, binding.Revision); err != nil {
				return Session{}, err
			}
		}
	}
	unlock := h.sessionOps.lock(id)
	if unavailable, err := h.ensureSessionActivatable(ctx, id); err != nil {
		unlock()
		return Session{}, fmt.Errorf("check session availability: %w", err)
	} else if unavailable != "" {
		unlock()
		return Session{}, fmt.Errorf("%w: %s", errSessionUnavailable, unavailable)
	}
	session, launched, err := h.ensureSessionStartedLocked(ctx, id)
	unlock()
	if err != nil {
		return Session{}, err
	}
	if launched {
		h.launchWorkerInBackground(id)
	}
	if !h.sessionUsesWorker(session) || (!launched && session.State != SessionStateStarting) {
		session.Live = true
		return h.sessionWithStoredMetadata(ctx, session)
	}
	if err := h.waitForRunningWorker(ctx, id); err != nil {
		return Session{}, err
	}
	if running, ok := h.registry.Get(id); ok && (running.State == SessionStateRunning || running.State == SessionStatePaused) && h.workers.Has(id) {
		running.Live = true
		return h.sessionWithStoredMetadata(ctx, running)
	}
	return Session{}, errWorkerConnectTimeout
}

func (h *handler) launchWorkerInBackground(id string) {
	launchCtx, cancel := context.WithCancel(context.Background())
	var watchdog *time.Timer
	if h.workerConnectTimeout > 0 {
		watchdog = time.AfterFunc(h.workerConnectTimeout, func() {
			unlock := h.sessionOps.lock(id)
			if !h.workers.Has(id) {
				_, _ = h.registry.FailStarting(id, errWorkerConnectTimeout.Error())
				cancel()
			}
			unlock()
		})
	}
	go func() {
		defer cancel()
		if err := h.launchWorker(launchCtx, id); err != nil {
			if watchdog != nil {
				watchdog.Stop()
			}
			unlock := h.sessionOps.lock(id)
			defer unlock()
			_, _ = h.registry.FailStarting(id, fmt.Sprintf("start worker: %v", err))
			return
		}
		_ = h.waitForRunningWorker(launchCtx, id)
		if watchdog != nil {
			watchdog.Stop()
		}
	}()
}

func (h *handler) waitForRunningWorker(ctx context.Context, id string) error {
	for {
		session, ok, changed := h.registry.Watch(id)
		if !ok {
			return errSessionNotFound
		}
		switch session.State {
		case SessionStateCreated:
			return errWorkerDisconnectedBeforeReady
		case SessionStateRunning:
			if h.workers.Has(id) {
				return nil
			}
			return errWorkerDisconnectedBeforeReady
		case SessionStatePaused:
			if h.workers.Has(id) {
				return nil
			}
		case SessionStateFailed:
			if session.Error == errWorkerConnectTimeout.Error() {
				return errWorkerConnectTimeout
			}
			if session.Error != "" {
				return errors.New(session.Error)
			}
			return errors.New("worker failed to start")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (h *handler) ensureSessionStartedLocked(ctx context.Context, id string) (Session, bool, error) {
	for {
		session, ok, err := h.loadSessionIntoRegistry(ctx, id)
		if err != nil {
			return Session{}, false, fmt.Errorf("load session: %w", err)
		}
		if !ok {
			return Session{}, false, errSessionNotFound
		}

		switch session.State {
		case SessionStateRunning:
			if h.workers.Has(id) || !h.sessionUsesWorker(session) {
				return session, false, nil
			}
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.RestartWorker(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		case SessionStateStarting:
			if !h.sessionUsesWorker(session) {
				return Session{}, false, errInvalidSessionTransition
			}
			return session, false, nil
		case SessionStatePaused:
			if h.workers.Has(id) {
				return session, false, nil
			}
			if !h.sessionUsesWorker(session) {
				return Session{}, false, errInvalidSessionTransition
			}
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.RestartWorker(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		case SessionStateCreated:
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.Start(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		default:
			return Session{}, false, errInvalidSessionTransition
		}
	}
}

func (h *handler) validateSessionProfile(ctx context.Context, session Session) error {
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		return nil
	}
	workingDirectory := session.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = h.sessionWorkingDirectory(ctx, session.ID)
	}
	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		return fmt.Errorf("load session profile configuration: %w", err)
	}
	if _, _, err := appConfig.ResolveProfile(session.ProfileName); err != nil {
		return fmt.Errorf("%w: %v", errSessionProfileNotFound, err)
	}
	return nil
}

func (h *handler) sessionWithStoredMetadata(ctx context.Context, session Session) (Session, error) {
	if h.store == nil {
		return session, nil
	}
	stored, err := h.store.Get(ctx, session.ID)
	if err != nil {
		return Session{}, fmt.Errorf("load session metadata: %w", err)
	}
	if stored != nil {
		session.Agent = stored.Agent
		if stored.Agent != string(zotigoruntime.AgentCodex) {
			session.ProfileName = stored.ProfileName
		}
		session.Model = stored.Model
		session.ReasoningEffort = stored.ReasoningEffort
		session.ApprovalPolicy = stored.ApprovalPolicy
	}
	return session, nil
}

func (h *handler) writeEnsureRunningError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, errInvalidSessionTransition):
		writeAPIError(w, http.StatusConflict, "invalid session state transition")
	case errors.Is(err, errSessionUnavailable):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errWorkerConnectTimeout):
		writeAPIError(w, http.StatusServiceUnavailable, errWorkerConnectTimeout.Error())
	case errors.Is(err, errWorkerDisconnectedBeforeReady):
		writeAPIError(w, http.StatusServiceUnavailable, errWorkerDisconnectedBeforeReady.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, errSessionProfileNotFound):
		writeAPIErrorCode(w, http.StatusConflict, "profile_not_found", err.Error())
	case errors.Is(err, zotigoruntime.ErrWorkspaceConflict):
		writeAPIErrorCode(w, http.StatusConflict, "runtime_workspace_conflict", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("start session: %v", err))
	}
}

func (h *handler) launchWorker(ctx context.Context, id string) error {
	spec, err := h.runtimeLaunchSpec(ctx, id)
	if err != nil {
		return err
	}
	adapter, err := h.runtimes.adapter(spec.Agent)
	if err != nil {
		return err
	}
	return adapter.StartWorker(ctx, spec)
}

func (h *handler) sessionUsesWorker(session Session) bool {
	agentKind := zotigoruntime.AgentKind(session.Agent)
	if agentKind == "" {
		agentKind = zotigoruntime.AgentZotigo
	}
	if agentKind == zotigoruntime.AgentZotigo {
		return h.launcher != nil
	}
	_, err := h.runtimes.adapter(agentKind)
	return err == nil
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
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query, err := parseDisplayItemQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	_, inRegistry := h.registry.Get(id)
	items, inStore, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if !inRegistry && !inStore {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, buildItemsResponse(items, query))
}

func (h *handler) handleWorkerAttach(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req workerReadyRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	req.Generation = strings.TrimSpace(req.Generation)
	if req.Generation == "" {
		writeAPIError(w, http.StatusBadRequest, "worker generation is required")
		return
	}

	unlock := h.sessionOps.lock(id)
	defer unlock()
	session, ok := h.registry.Get(id)
	if !ok {
		h.writeTransition(w, Session{}, errSessionNotFound)
		return
	}
	if !h.workers.Matches(id, req.Generation) {
		writeAPIError(w, http.StatusConflict, "worker ready does not match the active connection")
		return
	}
	if session.State == SessionStatePaused {
		var err error
		session, err = h.reconcileApprovalState(r.Context(), id, session)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("reconcile approval state: %v", err))
			return
		}
	}
	switch session.State {
	case SessionStateStarting:
		session, err := h.registry.MarkRunning(id)
		h.writeTransition(w, session, err)
	case SessionStateRunning:
		writeAPIJSON(w, http.StatusOK, session)
	case SessionStatePaused:
		writeAPIJSON(w, http.StatusOK, session)
	default:
		h.writeTransition(w, Session{}, errInvalidSessionTransition)
	}
}

func (h *handler) handleWorkerDisconnect(id string) {
	h.events.WakeBarrier(id)
	unlock := h.sessionOps.lock(id)
	defer unlock()
	if session, ok := h.registry.Get(id); ok {
		_, _ = h.reconcileApprovalState(context.Background(), id, session)
	}
	_, _ = h.registry.ResetStarting(id)
}

func (h *handler) reconcileApprovalState(ctx context.Context, id string, session Session) (Session, error) {
	if session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePaused {
		return session, nil
	}
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if hasPendingApproval(items) {
		if session.State == SessionStatePaused {
			return session, nil
		}
		return h.registry.Pause(id)
	}
	if session.State == SessionStatePaused {
		return h.registry.ResumeAfterApproval(id)
	}
	return session, nil
}

func (h *handler) handleWorkerFinish(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req finishSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if req.Generation != "" && !h.workers.Matches(id, req.Generation) {
		writeAPIError(w, http.StatusConflict, "worker finish does not match the active connection")
		return
	}

	unlock := h.approvalOps.lock(id)
	defer unlock()

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
		writeAPIJSON(w, http.StatusOK, session)
		return
	}
	switch {
	case errors.Is(err, errSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, errInvalidSessionTransition):
		writeAPIError(w, http.StatusConflict, "invalid session state transition")
	default:
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("update session: %v", err))
	}
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

type apiResponse[T any] struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type apiErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIJSON[T any](w http.ResponseWriter, status int, value T) {
	writeJSON(w, status, apiResponse[T]{
		Code:    "ok",
		Message: "",
		Data:    value,
	})
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeAPIErrorCode(w, status, apiErrorCode(status), message)
}

func writeAPIErrorCode(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, apiErrorResponse{
		Code:    code,
		Message: message,
	})
}

func apiErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "error"
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := sonic.Marshal(value)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"encode response failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
