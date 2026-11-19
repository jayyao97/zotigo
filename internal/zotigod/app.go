package zotigod

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/buildinfo"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/update"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

const defaultAddr = "127.0.0.1:8766"

const defaultWorkerLaunchTimeout = 15 * time.Second

const defaultWorkerConnectTimeout = 3 * time.Second

const apiProtocolVersion = "1"

type handler struct {
	callbackBaseURL      string
	updater              *update.Manager
	updateMu             sync.RWMutex
	registry             *sessionRegistry
	items                sessionlog.ItemSource
	store                zotigosession.Store
	workers              *workerRegistry
	launcher             workerLauncher
	runtimes             *runtimeRegistry
	workerConnectTimeout time.Duration
	workerLaunchTimeout  time.Duration
	inputStopTimeout     time.Duration
	sessionOps           *sessionOperationLocks
	workspaceOps         *sessionOperationLocks
	approvalOps          *sessionOperationLocks
	queueOps             *sessionOperationLocks
	titleSuggestion      titleSuggestionFunc
	titleTimeout         time.Duration
	events               *sessionlog.EventBroker
	catalog              *zotigoworkspace.Store
	catalogErr           error
	codexSync            *codexSessionSyncer
	codexUnavailable     error
	logger               *log.Logger
	hooks                workerbase.HookDispatcher
	channels             *channels.Service
	skillsMu             sync.Mutex
	skillManagers        map[string]*skills.SkillManager
	contextUsageMu       sync.Mutex
	contextUsageCache    map[string]contextUsageCacheEntry
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

type handlerOptions struct {
	callbackBaseURL      string
	updater              *update.Manager
	serviceContext       context.Context
	launcher             workerLauncher
	runtimes             *runtimeRegistry
	workers              *workerRegistry
	workerConnectTimeout time.Duration
	workerLaunchTimeout  time.Duration
	inputStopTimeout     time.Duration
	store                zotigosession.Store
	sessionOps           *sessionOperationLocks
	workspaceOps         *sessionOperationLocks
	titleSuggestion      titleSuggestionFunc
	titleTimeout         time.Duration
	events               *sessionlog.EventBroker
	publicAuthToken      string
	workerAuthToken      string
	catalog              *zotigoworkspace.Store
	catalogErr           error
	codexHost            codexapp.HostProvider
	codexUnavailable     error
	logger               *log.Logger
	hooks                workerbase.HookDispatcher
	channels             *channels.Service
}

func newDefaultHandler(opts handlerOptions) http.Handler {
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		opts.store = unavailableSessionStore{err: err}
		return newHandler(newSessionRegistry(), sessionlog.FailingItemSource{Err: err}, opts)
	}
	opts.store = store
	if opts.serviceContext != nil {
		go maintainConversationIndex(opts.serviceContext, store, opts.logger)
	}
	catalog, catalogErr := zotigoworkspace.Open(store.RootDir())
	opts.catalog = catalog
	opts.catalogErr = catalogErr
	if catalogErr == nil {
		opts.catalogErr = catalog.RecoverQueuedInputs(context.Background())
		if opts.serviceContext != nil {
			catalog.StartRepositoryMaintenance(opts.serviceContext, func(err error) {
				if opts.logger != nil {
					opts.logger.Printf("repository_maintenance_failed: %v", err)
				}
			})
		}
	}
	items := sessionlog.StoredItemSource{Store: store}
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

func newHandler(registry *sessionRegistry, items sessionlog.ItemSource, opts ...handlerOptions) http.Handler {
	if items == nil {
		items = sessionlog.FailingItemSource{Err: errors.New("display item source is not configured")}
	}
	options := handlerOptions{workerConnectTimeout: 0}
	if len(opts) > 0 {
		options = opts[0]
	}
	if options.workers == nil {
		options.workers = newWorkerRegistry()
	}
	if options.store == nil {
		if source, ok := items.(sessionlog.StoredItemSource); ok {
			options.store = source.Store
		}
	}
	if options.workerConnectTimeout == 0 && options.launcher != nil {
		options.workerConnectTimeout = defaultWorkerConnectTimeout
	}
	if options.workerLaunchTimeout == 0 && options.launcher != nil {
		options.workerLaunchTimeout = defaultWorkerLaunchTimeout
	}
	if options.inputStopTimeout == 0 {
		options.inputStopTimeout = workerbase.HTTPTimeout
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
		options.events = sessionlog.NewEventBroker()
	}
	eventingItems := sessionlog.EventingItemSource{Source: items, Events: options.events}
	if offsetItems, ok := items.(sessionlog.OffsetItemSource); ok {
		items = sessionlog.EventingOffsetItemSource{EventingItemSource: eventingItems, Offset: offsetItems}
	} else {
		items = eventingItems
	}
	handler := &handler{
		updater:              options.updater,
		registry:             registry,
		items:                items,
		store:                options.store,
		workers:              options.workers,
		launcher:             options.launcher,
		runtimes:             options.runtimes,
		codexUnavailable:     options.codexUnavailable,
		workerConnectTimeout: options.workerConnectTimeout,
		workerLaunchTimeout:  options.workerLaunchTimeout,
		inputStopTimeout:     options.inputStopTimeout,
		sessionOps:           options.sessionOps,
		workspaceOps:         options.workspaceOps,
		approvalOps:          newSessionOperationLocks(),
		queueOps:             newSessionOperationLocks(),
		callbackBaseURL:      options.callbackBaseURL,
		titleSuggestion:      options.titleSuggestion,
		titleTimeout:         options.titleTimeout,
		events:               options.events,
		catalog:              options.catalog,
		catalogErr:           options.catalogErr,
		logger:               options.logger,
		hooks:                options.hooks,
		channels:             options.channels,
	}
	if handler.channels != nil {
		handler.channels.SetDispatcher(handler)
	}
	if options.codexHost != nil && options.store != nil {
		handler.codexSync = newCodexSessionSyncer(options.codexHost, options.store, items, options.catalog, registry, options.sessionOps)
	}
	handler.workers.SetDisconnectHandler(handler.handleWorkerDisconnect)
	handler.workers.SetMessageHandler(func(sessionID string, generation string, msg wire.Message) {
		switch msg.Type {
		case wire.MessageDelta:
			if msg.Delta == nil || msg.Delta.ItemID == "" || (msg.Delta.Delta == "" && msg.Delta.BeforeInputID == "") {
				return
			}
			switch msg.Delta.PartType {
			case string(protocol.ContentTypeText), string(protocol.ContentTypeReasoning), "tool_progress":
				handler.registry.MarkWorking(sessionID, msg.Delta.ToolName)
				handler.events.PublishDelta(sessionID, *msg.Delta)
			}
		case wire.MessageDisplayWake:
			if msg.PreviewRetirement != nil {
				handler.events.Retire(sessionID, *msg.PreviewRetirement)
			}
			handler.events.Wake(sessionID)
		case wire.MessageWorking:
			if session, ok := handler.registry.Get(sessionID); ok && session.State == SessionStatePausing {
				_, _ = handler.registry.FinishStopping(sessionID)
			} else {
				handler.registry.MarkWorking(sessionID, "")
			}
		case wire.MessageDisplayBarrier:
			if msg.DisplayBarrier != nil && msg.DisplayBarrier.ID != "" {
				handler.events.WakeBarrier(sessionID)
				handler.workers.acknowledgeDisplayBarrier(sessionID, *msg.DisplayBarrier)
			}
		case wire.MessageApprovalRequest:
			if msg.ApprovalRequest != nil && msg.ApprovalRequest.Status == sessionlog.ApprovalStatusPending {
				unlock := handler.sessionOps.lockWorkerCallback(sessionID)
				_, _ = handler.registry.Pause(sessionID)
				unlock()
			}
		case wire.MessageApprovalResult:
			if msg.ApprovalResult != nil && msg.ApprovalResult.Error == "" && msg.ApprovalResult.Approval != nil && msg.ApprovalResult.Approval.Status == sessionlog.ApprovalStatusResolved {
				handler.reconcileHumanRequestState(sessionID)
			}
		case wire.MessageInteractionRequest:
			if msg.InteractionRequest != nil && msg.InteractionRequest.Status == sessionlog.InteractionStatusPending && msg.InteractionRequest.IsBlocking {
				unlock := handler.sessionOps.lockWorkerCallback(sessionID)
				_, _ = handler.registry.Pause(sessionID)
				unlock()
			}
		case wire.MessageInteractionResult:
			if msg.InteractionResult != nil && msg.InteractionResult.Error == "" && msg.InteractionResult.Interaction != nil && msg.InteractionResult.Interaction.Status != sessionlog.InteractionStatusPending && msg.InteractionResult.Interaction.IsBlocking {
				handler.reconcileHumanRequestState(sessionID)
			}
		case wire.MessageConversationBound:
			handler.handleConversationBound(sessionID, generation, msg.ConversationBound)
		case wire.MessageRuntimeToolRequest:
			if msg.RuntimeToolRequest != nil {
				request := *msg.RuntimeToolRequest
				go func() {
					result := wire.RuntimeToolResult{RequestID: request.RequestID}
					toolCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					value, err := handler.executeRuntimeTool(toolCtx, sessionID, generation, request)
					cancel()
					if err != nil {
						result.Error = err.Error()
					} else {
						result.Text = value
					}
					handler.workers.SendRuntimeToolResult(sessionID, generation, result)
				}()
			}
		case wire.MessageIdle:
			if msg.Idle != nil {
				idle := *msg.Idle
				if !handler.workers.IsCurrentIdle(sessionID, generation, idle) {
					return
				}
				// Keep Working state changes in worker event order, even while
				// queue preparation runs asynchronously.
				handler.events.Retire(sessionID, wire.PreviewRetirement{Terminal: true})
				handler.registry.MarkIdle(sessionID)
				go func() {
					// Drain before scheduling idle eviction. Admission advances the
					// command sequence, making this old idle notification harmless.
					_ = handler.dispatchQueuedInputs(sessionID, false, true)
					handler.closeWorkerWhenIdle(sessionID, generation, idle)
				}()
			}
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.handleHealth)
	mux.HandleFunc("/updates", handler.handleUpdates)
	mux.HandleFunc("/updates/drain", handler.handleUpdateDrain)
	mux.HandleFunc("/skills", handler.handleSkills)
	mux.HandleFunc("/agents", handler.handleAgents)
	mux.HandleFunc("/agents/codex/prepare", handler.handleCodexPrepare)
	mux.HandleFunc("/config/profiles", handler.handleProfiles)
	mux.HandleFunc("/config/models", handler.handleModelSettings)
	mux.HandleFunc("/config/models/test", handler.handleModelTest)
	mux.HandleFunc("/sources/inspect", handler.handleSourceInspection)
	mux.HandleFunc("/files/events", untilServiceStops(options.serviceContext, handler.handleFileEvents))
	mux.HandleFunc("/files/capabilities", handler.handleWorkspaceFile)
	mux.HandleFunc("/files/open", handler.handleWorkspaceFile)
	mux.HandleFunc("/files/save", handler.handleWorkspaceFile)
	mux.HandleFunc("/files/upload", handler.handleAttachmentUpload)
	mux.HandleFunc("/files/list", handler.handleDirectoryList)
	mux.HandleFunc("/sources/directories", handler.handleDirectoryList)
	mux.HandleFunc("/projects", handler.handleProjects)
	mux.HandleFunc("/repositories", handler.handleRepositories)
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
	handleOptionalTrailingSlash(mux, "/projects/{project_id}/sources/{source_id}/branches", withPathValues("project_id", "source_id", handler.handleSourceBranches))
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
	mux.HandleFunc("GET /catalog/activity", untilServiceStops(options.serviceContext, handler.handleSessionActivity))
	mux.HandleFunc("/catalog/snapshot", handler.handleCatalogSnapshot)
	mux.HandleFunc("/catalog/navigation", handler.handleNavigation)
	mux.HandleFunc("/catalog/navigation/import", handler.handleNavigationImport)
	mux.HandleFunc("/catalog/navigation/order", handler.handleNavigationOrder)
	mux.HandleFunc("/catalog/navigation/pin", handler.handleNavigationPin)
	mux.HandleFunc("/catalog/sessions/{$}", handler.handleCatalogSessionNotFound)
	handleOptionalTrailingSlash(mux, "/catalog/sessions/{id}", withPathValue("id", handler.handleCatalogSession))
	mux.HandleFunc("/catalog/sessions/{id}/{route...}", handler.handleCatalogSessionNotFound)
	mux.HandleFunc("/channels/connections", handler.handleChannelConnections)
	mux.HandleFunc("/channels/connections/{id}/groups", withPathValue("id", handler.handleChannelGroups))
	mux.HandleFunc("/channels/connections/{id}/groups/{chat_id}/members", withPathValue("id", handler.handleChannelGroupMembers))
	mux.HandleFunc("/channels/connections/{id}", withPathValue("id", handler.handleChannelConnection))
	mux.HandleFunc("/channels/conversations", handler.handleChannelConversations)
	mux.HandleFunc("/channels/conversations/{id}", withPathValue("id", handler.handleChannelConversation))
	mux.HandleFunc("/channels/conversations/{id}/messages", withPathValue("id", handler.handleChannelMessages))
	mux.HandleFunc("/sessions", handler.handleSessions)
	mux.HandleFunc("/sessions/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/sessions/{id}", withPathValue("id", handler.handleSessionGet))
	mux.HandleFunc("/sessions/{id}/search", withPathValue("id", handler.handleSessionSearch))
	mux.HandleFunc("/sessions/{id}/items/window", withPathValue("id", handler.handleSessionItemWindow))
	mux.HandleFunc("/sessions/{id}/items", withPathValue("id", handler.handleSessionItems))
	mux.HandleFunc("/sessions/{id}/events", untilServiceStops(options.serviceContext, withPathValue("id", handler.handleSessionEvents)))
	mux.HandleFunc("/sessions/{id}/messages", withPathValue("id", handler.handleSessionMessage))
	mux.HandleFunc("/sessions/{id}/queue", withPathValue("id", handler.handleInputQueue))
	mux.HandleFunc("POST /completion-triggers/{trigger}/fire", handler.handleFireCompletionTrigger)
	mux.HandleFunc("GET /sessions/{id}/completion-triggers", handler.handleCompletionTriggers)
	mux.HandleFunc("DELETE /sessions/{id}/completion-triggers/{trigger}", handler.handleCompletionTriggers)
	mux.HandleFunc("/sessions/{id}/queue/resume", withPathValue("id", handler.handleResumeInputQueue))
	mux.HandleFunc("/sessions/{id}/queue/{item}", handler.handleQueuedInput)
	mux.HandleFunc("/sessions/{id}/queue/{item}/send", handler.handleSendQueuedInput)
	mux.HandleFunc("/sessions/{id}/fork", withPathValue("id", handler.handleSessionFork))
	mux.HandleFunc("/sessions/{id}/pause", withPathValue("id", handler.handleSessionPause))
	mux.HandleFunc("/sessions/{id}/profile", withPathValue("id", handler.handleSessionProfile))
	mux.HandleFunc("/sessions/{id}/codex-settings", withPathValue("id", handler.handleSessionCodexSettings))
	mux.HandleFunc("/sessions/{id}/approval-policy", withPathValue("id", handler.handleSessionApprovalPolicy))
	mux.HandleFunc("/sessions/{id}/start", withPathValue("id", handler.handleSessionStart))
	mux.HandleFunc("/sessions/{id}/steering", withPathValue("id", handler.handleSessionSteering))
	mux.HandleFunc("/sessions/{id}/title-suggestion", withPathValue("id", handler.handleSessionTitleSuggestion))
	mux.HandleFunc("/sessions/{id}/translate", withPathValue("id", handler.handleTranslation))
	mux.HandleFunc("/sessions/{id}/title", withPathValue("id", handler.handleSessionOrganizationTitle))
	mux.HandleFunc("/sessions/{id}/read-state", withPathValue("id", handler.handleSessionReadState))
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
	mux.HandleFunc("/sessions/{id}/interactions/{$}", handler.handleSessionRouteNotFound)
	mux.HandleFunc("/sessions/{id}/interactions/{interaction_id...}", withPathValues("id", "interaction_id", handler.handleInteractionResponse))
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

// http.Server.Shutdown waits for active requests. Cancel long-lived event streams
// when the daemon stops so ordinary requests can drain within the grace period.
func untilServiceStops(serviceContext context.Context, next http.HandlerFunc) http.HandlerFunc {
	if serviceContext == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(serviceContext, cancel)
		defer stop()
		defer cancel()
		next(w, r.WithContext(ctx))
	}
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

	version, commit := buildinfo.Current()
	writeAPIJSON(w, http.StatusOK, map[string]string{
		"version":          version,
		"commit":           commit,
		"process_id":       fmt.Sprint(os.Getpid()),
		"status":           "ok",
		"protocol_version": apiProtocolVersion,
	})
}
