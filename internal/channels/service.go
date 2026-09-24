package channels

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
)

type ProgressHandle interface {
	Update(context.Context, Progress) error
	Complete(context.Context, TaskResult, func(string) error) error
	Fail(context.Context, Progress) error
	Recover(context.Context, bool) error
	Receipt() DeliveryReceipt
}

// ProcessingMarkerHandle is implemented by adapters that attach a temporary
// marker to the inbound message while a task is running.
type ProcessingMarkerHandle interface {
	ClearProcessingMarker(context.Context) error
}

// ProcessingMarkerPlanner lets an adapter declare a durable cleanup intent
// before it creates provider-side state. Persisting this first closes the
// remote-success/local-crash window between marker creation and receipt save.
type ProcessingMarkerPlanner interface {
	ProcessingMarkerIntent(InboundMessage) string
}

// RecoveryPreparer initializes provider state needed to reconcile durable
// receipts without starting the adapter's long-lived connection.
type RecoveryPreparer interface {
	PrepareRecovery(context.Context) error
}

type Adapter interface {
	Start(context.Context) error
	Stop(context.Context) error
	OpenProgress(context.Context, InboundMessage) (ProgressHandle, error)
	ResumeProgress(DeliveryReceipt) ProgressHandle
}

type ConversationMetadataResolver interface {
	ResolveConversationName(ctx context.Context, chatID string) (string, error)
}

type GroupDiscoverer interface {
	ListGroups(context.Context) ([]Group, error)
}

type GroupMemberLister interface {
	ListGroupMembers(context.Context, string) ([]Sender, error)
}

// InboundImageResolver resolves provider resource references after the common
// service has authorized and claimed a message.
type InboundImageResolver interface {
	ResolveInboundImages(context.Context, []InboundImage) ([]InboundImage, error)
}

// ReferencedMessageResolver fetches the one provider message identified by
// trusted current-turn context and verifies that it belongs to the bound chat.
type ReferencedMessageResolver interface {
	ResolveReferencedMessage(context.Context, string, string) (ReferencedMessage, error)
}

type AdapterCallbacks struct {
	Inbound func(context.Context, InboundMessage) error
	Ready   func(botOpenID, botName string)
	Owners  func([]string)
	Error   func(error)
}

type AdapterFactory interface {
	New(Connection, string, AdapterCallbacks) (Adapter, error)
	Capabilities() AdapterCapabilities
}

type AdapterCapabilities struct {
	ProgressModes       []string
	DefaultProgressMode string
}

type Service struct {
	store               *Store
	secrets             *SecretStore
	factories           map[string]AdapterFactory
	logger              *log.Logger
	lifecycleMu         sync.Mutex
	mu                  sync.Mutex
	configMu            sync.RWMutex
	inboxLocksMu        sync.Mutex
	inboxLocks          map[string]*inboxLock
	dispatcher          TaskDispatcher
	runs                map[string]*adapterRun
	ctx                 context.Context
	nextRun             uint64
	closed              bool
	storeClosed         bool
	deliveryTimeout     time.Duration
	afterMessageClaimed func()
}

type inboxLock struct {
	mu   sync.Mutex
	refs int
}

type adapterRun struct {
	adapter    Adapter
	cancel     context.CancelFunc
	generation uint64
	ctx        context.Context
	sendMu     sync.Mutex
	deliveries sync.WaitGroup
	markerMu   sync.Mutex
	revoked    bool
}

func NewService(store *Store, secrets *SecretStore, logger *log.Logger, factories map[string]AdapterFactory) *Service {
	return &Service{store: store, secrets: secrets, logger: logger, factories: factories, runs: map[string]*adapterRun{}, inboxLocks: map[string]*inboxLock{}, deliveryTimeout: 15 * time.Second}
}

func (s *Service) lockInbox(key string) func() {
	s.inboxLocksMu.Lock()
	entry := s.inboxLocks[key]
	if entry == nil {
		entry = &inboxLock{}
		s.inboxLocks[key] = entry
	}
	entry.refs++
	s.inboxLocksMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.inboxLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.inboxLocks, key)
		}
		s.inboxLocksMu.Unlock()
	}
}

func (s *Service) SetDispatcher(dispatcher TaskDispatcher) {
	s.mu.Lock()
	s.dispatcher = dispatcher
	s.mu.Unlock()
}

func (s *Service) Start(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	if s.closed {
		s.configMu.Unlock()
		return errors.New("channel service is closed")
	}
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	interrupted, err := s.store.RecoverPendingMessages(ctx)
	if err != nil {
		s.configMu.Unlock()
		return err
	}
	if err := s.store.PruneMessages(ctx, time.Now().UTC()); err != nil {
		s.configMu.Unlock()
		return err
	}
	connections, err := s.store.ListConnections(ctx)
	if err != nil {
		s.configMu.Unlock()
		return err
	}
	s.configMu.Unlock()
	for _, connection := range connections {
		if connection.Enabled {
			if err := s.restart(ctx, connection); err != nil {
				_ = s.store.SetConnectionRuntime(ctx, connection.ID, "error", err.Error(), "", "")
			}
		}
	}
	s.patchInterrupted(ctx, interrupted)
	for _, connection := range connections {
		if connection.Enabled {
			if err := s.reconcileRunProcessingMarkers(ctx, connection.ID); err != nil && s.logger != nil {
				s.logger.Printf("channel %s: reconcile processing markers: %v", connection.ID, err)
			}
		}
	}
	return nil
}

func (s *Service) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	if s.storeClosed {
		s.configMu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Lock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	s.configMu.Unlock()
	var result error
	for _, id := range ids {
		_, err := s.stop(ctx, id)
		result = errors.Join(result, err)
	}
	if result != nil {
		return result
	}
	if err := s.store.Close(); err != nil {
		return err
	}
	s.configMu.Lock()
	s.storeClosed = true
	s.configMu.Unlock()
	return nil
}

func (s *Service) ListConnections(ctx context.Context) ([]Connection, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return nil, errors.New("channel service is closed")
	}
	values, err := s.store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	for i := range values {
		_, values[i].HasSecret, _ = s.secrets.Get(values[i].ID)
		values[i].COTAvailable = slices.Contains(s.factories[values[i].Provider].Capabilities().ProgressModes, ProgressModeCOT)
	}
	return values, nil
}
func (s *Service) GetConnection(ctx context.Context, id string) (Connection, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return Connection{}, errors.New("channel service is closed")
	}
	value, err := s.store.GetConnection(ctx, id)
	if err == nil {
		_, value.HasSecret, _ = s.secrets.Get(id)
		value.COTAvailable = slices.Contains(s.factories[value.Provider].Capabilities().ProgressModes, ProgressModeCOT)
	}
	return value, err
}

func (s *Service) PutConnection(ctx context.Context, id string, input ConnectionInput) (Connection, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	configLocked := true
	defer func() {
		if configLocked {
			s.configMu.Unlock()
		}
	}()
	if s.closed {
		return Connection{}, errors.New("channel service is closed")
	}
	provider := strings.TrimSpace(input.Provider)
	if provider == "" {
		provider = ProviderFeishu
	}
	if _, ok := s.factories[provider]; !ok {
		return Connection{}, fmt.Errorf("unsupported channel provider %q", provider)
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.AppID) == "" {
		return Connection{}, errors.New("name and app_id are required")
	}
	if strings.TrimSpace(input.ApprovalInstructions) != "" && !input.ReviewAllTools {
		return Connection{}, errors.New("approval_instructions require review_all_tools")
	}
	old, err := s.store.GetConnection(ctx, id)
	existed := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Connection{}, err
	}
	connections, err := s.store.ListConnections(ctx)
	if err != nil {
		return Connection{}, err
	}
	for _, existing := range connections {
		if existing.ID != id && existing.Provider == provider && existing.AppID == strings.TrimSpace(input.AppID) {
			return Connection{}, errors.New("app_id is already used by another channel connection on this daemon")
		}
	}
	capabilities := s.factories[provider].Capabilities()
	progressMode := strings.TrimSpace(input.ProgressMode)
	if progressMode == "" {
		progressMode = old.ProgressMode
		if progressMode == "" {
			progressMode = capabilities.DefaultProgressMode
		}
	}
	if !slices.Contains(capabilities.ProgressModes, progressMode) {
		return Connection{}, fmt.Errorf("unsupported progress_mode %q", progressMode)
	}
	created := old.CreatedAt
	ownerSenderIDs := old.OwnerSenderIDs
	if input.OwnerSenderIDs != nil {
		ownerSenderIDs = *input.OwnerSenderIDs
	}
	value := Connection{ID: id, Provider: provider, Name: strings.TrimSpace(input.Name), AppID: strings.TrimSpace(input.AppID), Enabled: input.Enabled, Status: "stopped", AllowChatIDs: input.AllowChatIDs, OwnerSenderIDs: ownerSenderIDs, AgentInstructions: input.AgentInstructions, ApprovalInstructions: input.ApprovalInstructions, ReviewAllTools: input.ReviewAllTools, ProgressMode: progressMode, CreatedAt: created}
	if input.ClearSecret && input.AppSecret != nil {
		return Connection{}, errors.New("clear_secret and app_secret cannot be set together")
	}
	oldSecret, hadOldSecret, err := s.secrets.Get(id)
	if err != nil {
		return Connection{}, err
	}
	candidateSecret, hasCandidateSecret := oldSecret, hadOldSecret
	secretChanged := false
	if input.ClearSecret {
		candidateSecret, hasCandidateSecret, secretChanged = "", false, hadOldSecret
	} else if input.AppSecret != nil {
		candidateSecret = strings.TrimSpace(*input.AppSecret)
		hasCandidateSecret = candidateSecret != ""
		secretChanged = candidateSecret != oldSecret || hasCandidateSecret != hadOldSecret
	}
	if input.Enabled && !hasCandidateSecret {
		return Connection{}, errors.New("an enabled channel requires app_secret")
	}
	needsRestart := s.currentContext() != nil && (connectionNeedsRestart(old, value, existed, secretChanged) || s.connectionRunNeedsRestart(id, value.Enabled))
	var preparedRun *adapterRun
	var cleanupErr error
	if needsRestart && existed {
		// Provider cleanup may need to drain a revoked run. Do not hold configMu
		// while waiting because metadata operations finish under its read lock.
		s.configMu.Unlock()
		configLocked = false
		preparedRun, cleanupErr = s.reconcileProcessingMarkersBeforeReconfiguration(ctx, id, old, oldSecret, value, candidateSecret)
		s.configMu.Lock()
		configLocked = true
		if cleanupErr != nil {
			restoreErr := s.restoreRunAfterDeleteFailure(id, preparedRun)
			return Connection{}, errors.Join(fmt.Errorf("clean channel processing markers before reconfiguration: %w", cleanupErr), restoreErr)
		}
	}
	if secretChanged {
		if err := s.secrets.Set(id, candidateSecret); err != nil {
			return Connection{}, errors.Join(err, s.restoreRunAfterDeleteFailure(id, preparedRun))
		}
	}
	value, err = s.store.PutConnection(ctx, value)
	if err != nil {
		var rollbackErr error
		if secretChanged {
			rollbackSecret := ""
			if hadOldSecret {
				rollbackSecret = oldSecret
			}
			if secretErr := s.secrets.Set(id, rollbackSecret); secretErr != nil {
				rollbackErr = fmt.Errorf("restore channel secret: %w", secretErr)
			}
		}
		return Connection{}, errors.Join(err, rollbackErr, s.restoreRunAfterDeleteFailure(id, preparedRun))
	}
	if needsRestart {
		s.revokeCurrentRun(id)
	}
	s.configMu.Unlock()
	configLocked = false
	if needsRestart {
		if err = s.restart(ctx, value); err != nil {
			_ = s.store.SetConnectionRuntime(ctx, id, "error", err.Error(), "", "")
			current, getErr := s.GetConnection(ctx, id)
			return current, errors.Join(fmt.Errorf("restart channel connection: %w", err), getErr)
		}
	}
	return s.GetConnection(ctx, id)
}

func (s *Service) connectionRunNeedsRestart(connectionID string, enabled bool) bool {
	if !enabled {
		return false
	}
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if run == nil {
		return true
	}
	run.sendMu.Lock()
	defer run.sendMu.Unlock()
	return run.revoked
}

func (s *Service) revokeCurrentRun(connectionID string) {
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	run.sendMu.Lock()
	run.revoked = true
	run.sendMu.Unlock()
}

func (s *Service) reconcileProcessingMarkersBeforeReconfiguration(ctx context.Context, id string, old Connection, oldSecret string, next Connection, nextSecret string) (*adapterRun, error) {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run != nil {
		drainedRun, err := s.quiesce(ctx, id)
		if err != nil {
			return nil, err
		}
		if drainedRun != run {
			return run, errors.New("channel adapter changed while draining")
		}
		run.markerMu.Lock()
		defer run.markerMu.Unlock()
		return run, s.reconcileProcessingMarkers(ctx, id, run.adapter)
	}
	candidates := []recoveryCandidate{{connection: old, secret: oldSecret}}
	// A replacement secret is another credential for the same principal. A new
	// provider or App ID must never be used to prove cleanup of the old bot's state.
	if old.Provider == next.Provider && old.AppID == next.AppID && oldSecret != nextSecret {
		candidates = append(candidates, recoveryCandidate{connection: old, secret: nextSecret})
	}
	return nil, s.reconcileProcessingMarkersWithCandidates(ctx, id, candidates)
}

type recoveryCandidate struct {
	connection Connection
	secret     string
}

func (s *Service) reconcileProcessingMarkersWithCandidates(ctx context.Context, id string, candidates []recoveryCandidate) error {
	messages, err := s.store.ListMessagesWithProcessingMarkers(ctx, id)
	if err != nil || len(messages) == 0 {
		return err
	}
	var result error
	for _, candidate := range candidates {
		factory := s.factories[candidate.connection.Provider]
		if factory == nil || candidate.secret == "" {
			continue
		}
		adapter, createErr := factory.New(candidate.connection, candidate.secret, AdapterCallbacks{})
		if createErr != nil {
			result = errors.Join(result, createErr)
			continue
		}
		if preparer, ok := adapter.(RecoveryPreparer); ok {
			if prepareErr := preparer.PrepareRecovery(ctx); prepareErr != nil {
				result = errors.Join(result, prepareErr, adapter.Stop(ctx))
				continue
			}
		}
		cleanupErr := s.reconcileProcessingMarkers(ctx, id, adapter)
		stopErr := adapter.Stop(ctx)
		if cleanupErr == nil && stopErr == nil {
			return nil
		}
		result = errors.Join(result, cleanupErr, stopErr)
	}
	if result == nil {
		result = errors.New("processing markers remain but no provider adapter can be created")
	}
	return result
}

func connectionNeedsRestart(old, next Connection, existed, secretChanged bool) bool {
	if !existed {
		return next.Enabled
	}
	return secretChanged ||
		old.Provider != next.Provider ||
		old.AppID != next.AppID ||
		old.Enabled != next.Enabled ||
		old.ProgressMode != next.ProgressMode ||
		!sameIDSet(old.AllowChatIDs, next.AllowChatIDs)
}

func sameIDSet(left, right []string) bool {
	left = normalizeIDs(left)
	right = normalizeIDs(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func (s *Service) DeleteConnection(ctx context.Context, id string) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	if s.closed {
		s.configMu.Unlock()
		return errors.New("channel service is closed")
	}
	conversations, err := s.store.ListConversations(ctx, id)
	if err != nil {
		s.configMu.Unlock()
		return err
	}
	for _, conversation := range conversations {
		if conversation.SessionID != "" {
			s.configMu.Unlock()
			return ErrConnectionBound
		}
	}
	// Revoke admission under the same fence as the binding check, so a new
	// Session cannot be provisioned while deletion drains provider operations.
	s.mu.Lock()
	currentRun := s.runs[id]
	s.mu.Unlock()
	if currentRun != nil {
		currentRun.sendMu.Lock()
		currentRun.revoked = true
		currentRun.sendMu.Unlock()
	}
	s.configMu.Unlock()
	// Provider-side processing markers need the live adapter and its credentials.
	// Attempt cleanup before removing either one.
	run, err := s.quiesce(ctx, id)
	if err != nil {
		return fmt.Errorf("stop channel deliveries before delete: %w", err)
	}
	if run != nil {
		if err := s.reconcileProcessingMarkers(ctx, id, run.adapter); err != nil {
			restoreErr := s.restoreRunAfterDeleteFailure(id, run)
			return errors.Join(fmt.Errorf("clean channel processing markers before delete: %w", err), restoreErr)
		}
	} else {
		connection, getErr := s.store.GetConnection(ctx, id)
		if getErr != nil && !errors.Is(getErr, ErrNotFound) {
			return getErr
		}
		secret, _, secretErr := s.secrets.Get(id)
		if secretErr != nil {
			return secretErr
		}
		if getErr == nil {
			if err := s.reconcileProcessingMarkersWithCandidates(ctx, id, []recoveryCandidate{{connection: connection, secret: secret}}); err != nil {
				return fmt.Errorf("clean channel processing markers before delete: %w", err)
			}
		}
	}
	if run != nil {
		if err := s.finishStop(ctx, id, run); err != nil {
			return fmt.Errorf("stop channel adapter before delete: %w", err)
		}
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	oldSecret, hadSecret, err := s.secrets.Get(id)
	if err != nil {
		return err
	}
	if err := s.secrets.Delete(id); err != nil {
		return err
	}
	if err := s.store.DeleteConnection(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if hadSecret {
			return errors.Join(err, s.secrets.Set(id, oldSecret))
		}
		return err
	}
	return nil
}

func (s *Service) restoreRunAfterDeleteFailure(id string, run *adapterRun) error {
	if run == nil {
		return nil
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), s.deliveryTimeout)
	defer cancel()
	if err := s.finishStop(restoreCtx, id, run); err != nil {
		_ = s.store.SetConnectionRuntime(context.Background(), id, "error", err.Error(), "", "")
		return fmt.Errorf("stop channel adapter after failed delete: %w", err)
	}
	connection, err := s.store.GetConnection(restoreCtx, id)
	if err != nil || !connection.Enabled || s.currentContext() == nil {
		return err
	}
	if err := s.restart(restoreCtx, connection); err != nil {
		_ = s.store.SetConnectionRuntime(context.Background(), id, "error", err.Error(), "", "")
		return fmt.Errorf("restore channel after failed delete: %w", err)
	}
	return nil
}
func (s *Service) ListConversations(ctx context.Context, connectionID string) ([]Conversation, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return nil, errors.New("channel service is closed")
	}
	return s.store.ListConversations(ctx, connectionID)
}

func (s *Service) ListGroups(ctx context.Context, connectionID string) ([]Group, error) {
	s.configMu.RLock()
	if s.closed {
		s.configMu.RUnlock()
		return nil, errors.New("channel service is closed")
	}
	connection, err := s.store.GetConnection(ctx, connectionID)
	s.configMu.RUnlock()
	if err != nil {
		return nil, err
	}
	discovered := map[string]Group{}
	discoveryGeneration := uint64(0)
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if run != nil {
		if discoverer, ok := run.adapter.(GroupDiscoverer); ok && s.acquireRunOperation(connectionID, run) {
			defer run.deliveries.Done()
			discoveryGeneration = run.generation
			groups, discoverErr := discoverer.ListGroups(ctx)
			if discoverErr != nil {
				return nil, discoverErr
			}
			for _, group := range groups {
				if group.ChatID == "" {
					continue
				}
				group.Available = true
				discovered[group.ChatID] = group
			}
		}
	}
	// Discovery performs network I/O without holding the configuration lock. Recheck
	// the connection and keep deletion/reconfiguration out while merging the result.
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return nil, errors.New("channel service is closed")
	}
	if _, err := s.store.GetConnection(ctx, connection.ID); err != nil {
		return nil, err
	}
	if discoveryGeneration != 0 && !s.isRunActive(connection.ID, run, discoveryGeneration) {
		// Credentials or connection state changed while the provider request was in
		// flight. Never merge group metadata returned by the superseded adapter.
		discovered = map[string]Group{}
	}
	existingConversations, err := s.store.ListConversations(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	existingNames := make(map[string]string)
	for _, conversation := range existingConversations {
		if conversation.RootID == "" && conversation.ChatType == "group" {
			existingNames[conversation.ChatID] = conversation.ChatName
		}
	}
	for chatID, group := range discovered {
		if strings.TrimSpace(group.Name) == "" {
			group.Name = strings.TrimSpace(existingNames[chatID])
			discovered[chatID] = group
		}
		if err := s.store.EnsureGroupConversation(ctx, connection.ID, group.ChatID); err != nil {
			return nil, err
		}
		if group.ChatMode != "" {
			if err := s.store.PutGroupConversationMode(ctx, connection.ID, group.ChatID, group.ChatMode); err != nil {
				return nil, err
			}
		}
		if group.Name != "" {
			if err := s.store.PutGroupConversationName(ctx, connection.ID, group.ChatID, group.Name); err != nil {
				return nil, err
			}
		}
	}
	conversations, err := s.store.ListConversations(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		if conversation.RootID != "" || conversation.ChatType != "group" {
			continue
		}
		group := discovered[conversation.ChatID]
		group.ChatID = conversation.ChatID
		if group.Name == "" {
			group.Name = conversation.ChatName
		}
		if group.ChatMode == "" {
			group.ChatMode = conversation.ChatMode
		}
		group.ConversationID = conversation.ID
		group.SessionStrategy = conversation.SessionStrategy
		group.SessionID = conversation.SessionID
		group.WorkspaceID = conversation.WorkspaceID
		group.Agent = conversation.Agent
		group.ProfileName = conversation.ProfileName
		group.Model = conversation.Model
		group.ReasoningEffort = conversation.ReasoningEffort
		group.Enabled = conversation.Enabled
		group.SenderPolicy = conversation.SenderPolicy
		group.AllowedSenderIDs = append([]string(nil), conversation.AllowedSenderIDs...)
		discovered[group.ChatID] = group
	}
	groups := make([]Group, 0, len(discovered))
	for _, group := range discovered {
		groups = append(groups, group)
	}
	slices.SortFunc(groups, func(left, right Group) int {
		leftKey := strings.ToLower(left.Name)
		if leftKey == "" {
			leftKey = left.ChatID
		}
		rightKey := strings.ToLower(right.Name)
		if rightKey == "" {
			rightKey = right.ChatID
		}
		return strings.Compare(leftKey, rightKey)
	})
	return groups, nil
}

func (s *Service) ListGroupMembers(ctx context.Context, connectionID, chatID string) ([]Sender, error) {
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if run == nil {
		return nil, errors.New("channel connection is not running")
	}
	lister, ok := run.adapter.(GroupMemberLister)
	if !ok {
		return nil, errors.New("group member discovery is unavailable")
	}
	if !s.acquireRunOperation(connectionID, run) {
		return nil, errors.New("channel connection changed")
	}
	defer run.deliveries.Done()
	return lister.ListGroupMembers(ctx, chatID)
}
func (s *Service) GetConversation(ctx context.Context, id string) (Conversation, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return Conversation{}, errors.New("channel service is closed")
	}
	return s.store.GetConversation(ctx, id)
}

// WithResolvedSessionPrompt resolves the current group and Connection prompt
// for a bound Session. The callback runs while the Channel configuration read
// lock is held so a concurrent save cannot cross the Session admission fence.
func (s *Service) WithResolvedSessionPrompt(ctx context.Context, sessionID string, use func(SessionPromptConfig, bool) error) (bool, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return false, errors.New("channel service is closed")
	}
	conversation, err := s.store.GetConversationBySession(ctx, strings.TrimSpace(sessionID))
	if errors.Is(err, ErrNotFound) {
		if use != nil {
			return false, use(SessionPromptConfig{}, false)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	group, err := s.store.GetConversationByScope(ctx, conversation.ConnectionID, conversation.ChatID, "")
	if err != nil {
		return false, err
	}
	connection, err := s.store.GetConnection(ctx, conversation.ConnectionID)
	if err != nil {
		return false, err
	}
	if use != nil {
		if err := use(resolvePromptSnapshot(connection, group), true); err != nil {
			return true, err
		}
	}
	return true, nil
}

// GuardSessionUnbinding serializes archive/unbind with admission and binding
// edits. Callers acquire their Session lock after this guard, and invoke unbind
// only after the idle Session has been archived successfully.
func (s *Service) GuardSessionUnbinding(ctx context.Context, sessionID string) (unbind func() error, release func(), err error) {
	s.configMu.Lock()
	if s.closed {
		s.configMu.Unlock()
		return nil, nil, errors.New("channel service is closed")
	}
	conversation, err := s.store.GetConversationBySession(ctx, sessionID)
	if errors.Is(err, ErrNotFound) {
		return func() error { return nil }, s.configMu.Unlock, nil
	}
	if err != nil {
		s.configMu.Unlock()
		return nil, nil, err
	}
	return func() error {
		conversation.SessionID = ""
		if conversation.RootID != "" {
			conversation.Enabled = false
		}
		_, err := s.store.PutConversation(ctx, conversation)
		return err
	}, s.configMu.Unlock, nil
}

func (s *Service) PutConversation(ctx context.Context, id string, input ConversationInput) (Conversation, error) {
	return s.PutConversationValidated(ctx, id, input, nil)
}

// PutConversationValidated updates a conversation while holding the channel
// configuration write lock. The validator runs when the Workspace, Session, or
// Session runtime selection changes. Keeping validation inside this lock gives
// callers a single config -> session lock order, matching inbound admission.
func (s *Service) PutConversationValidated(ctx context.Context, id string, input ConversationInput, validateBinding func(context.Context, Conversation, SessionPromptConfig, SessionRuntimeConfig) (SessionPromptConfig, SessionRuntimeConfig, error)) (Conversation, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.closed {
		return Conversation{}, errors.New("channel service is closed")
	}
	c, err := s.store.GetConversation(ctx, id)
	if err != nil {
		return c, err
	}
	previousSessionID := c.SessionID
	previousWorkspaceID := c.WorkspaceID
	previousRuntime := conversationRuntime(c)
	if input.AgentInstructionsMode != "" && input.AgentInstructionsMode != OverrideInherit && input.AgentInstructionsMode != OverrideReplace {
		return c, errors.New("invalid agent_instructions_mode")
	}
	if input.ApprovalInstructionsMode != "" && input.ApprovalInstructionsMode != OverrideInherit && input.ApprovalInstructionsMode != OverrideReplace {
		return c, errors.New("invalid approval_instructions_mode")
	}
	c.DisplayName = strings.TrimSpace(input.DisplayName)
	c.SessionStrategy = strings.TrimSpace(input.SessionStrategy)
	if c.SessionStrategy == "" {
		c.SessionStrategy = SessionStrategyTopic
	}
	if c.SessionStrategy != SessionStrategyTopic && c.SessionStrategy != SessionStrategyShared {
		return c, errors.New("invalid session_strategy")
	}
	if c.RootID == "" && c.ChatMode == ChatModeTopic && c.SessionStrategy == SessionStrategyShared {
		return c, errors.New("topic groups require one session per topic")
	}
	c.SessionID = strings.TrimSpace(input.SessionID)
	c.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	if input.Agent != nil {
		c.Agent = strings.TrimSpace(*input.Agent)
	}
	if input.ProfileName != nil {
		c.ProfileName = strings.TrimSpace(*input.ProfileName)
	}
	if input.Model != nil {
		c.Model = strings.TrimSpace(*input.Model)
	}
	if input.ReasoningEffort != nil {
		c.ReasoningEffort = strings.TrimSpace(*input.ReasoningEffort)
	}
	if c.Agent == "" {
		c.Agent = SessionAgentZotigo
	}
	switch c.Agent {
	case SessionAgentZotigo:
		if c.Model != "" || c.ReasoningEffort != "" {
			return c, errors.New("model and reasoning_effort require agent codex")
		}
	case SessionAgentCodex:
		if c.ProfileName != "" {
			return c, errors.New("profile_name is only valid for agent zotigo")
		}
		if c.Model == "" || c.ReasoningEffort == "" {
			return c, errors.New("codex channel sessions require model and reasoning_effort")
		}
	default:
		return c, errors.New("unsupported channel session agent")
	}
	c.Enabled = input.Enabled
	if input.SenderPolicy != nil {
		c.SenderPolicy = strings.TrimSpace(*input.SenderPolicy)
	}
	if c.SenderPolicy == "" {
		c.SenderPolicy = SenderPolicySelected
	}
	if c.SenderPolicy != SenderPolicyOwners && c.SenderPolicy != SenderPolicyAll && c.SenderPolicy != SenderPolicySelected {
		return c, errors.New("invalid sender_policy")
	}
	c.AllowedSenderIDs = input.AllowedSenderIDs
	c.AgentInstructionsMode = input.AgentInstructionsMode
	c.AgentInstructions = input.AgentInstructions
	c.ApprovalInstructionsMode = input.ApprovalInstructionsMode
	c.ApprovalInstructions = input.ApprovalInstructions
	c.ReviewAllTools = input.ReviewAllTools
	if c.RootID == "" {
		if c.SessionStrategy == SessionStrategyTopic {
			c.SessionID = ""
		}
		if c.Enabled && (c.WorkspaceID == "" || (c.SenderPolicy == SenderPolicySelected && len(normalizeIDs(c.AllowedSenderIDs)) == 0)) {
			return c, errors.New("an enabled group requires a workspace and selected sender IDs when sender_policy is selected")
		}
	} else if c.Enabled && c.SessionID == "" {
		return c, errors.New("an enabled conversation requires a session")
	}
	if c.SessionID != "" {
		bound, bindErr := s.store.GetConversationBySession(ctx, c.SessionID)
		if bindErr == nil && bound.ID != c.ID {
			return c, errors.New("session is already bound to another channel conversation")
		}
		if bindErr != nil && !errors.Is(bindErr, ErrNotFound) {
			return c, bindErr
		}
	}
	connection, err := s.store.GetConnection(ctx, c.ConnectionID)
	if err != nil {
		return c, err
	}
	if c.RootID == "" && c.Enabled && c.SenderPolicy == SenderPolicyOwners && len(connection.OwnerSenderIDs) == 0 {
		return c, errors.New("application owner has not been resolved")
	}
	effectiveApproval := connection.ApprovalInstructions
	if c.ApprovalInstructionsMode == OverrideReplace {
		effectiveApproval = c.ApprovalInstructions
	}
	effectiveReview := connection.ReviewAllTools
	if c.ReviewAllTools != nil {
		effectiveReview = *c.ReviewAllTools
	}
	if strings.TrimSpace(effectiveApproval) != "" && !effectiveReview {
		return c, errors.New("effective approval instructions require review_all_tools")
	}
	prompt := SessionPromptConfig{AgentInstructions: connection.AgentInstructions, ApprovalInstructions: effectiveApproval, ReviewAllTools: effectiveReview}
	if c.AgentInstructionsMode == OverrideReplace {
		prompt.AgentInstructions = c.AgentInstructions
	}
	runtime := conversationRuntime(c)
	if previousSessionID != "" && c.SessionID == previousSessionID && previousRuntime != runtime {
		return c, errors.New("bound session runtime snapshot cannot be changed")
	}
	if validateBinding != nil && (c.SessionID != previousSessionID || c.WorkspaceID != previousWorkspaceID || runtime != previousRuntime) {
		canonicalPrompt, canonicalRuntime, validateErr := validateBinding(ctx, c, prompt, runtime)
		if validateErr != nil {
			return c, validateErr
		}
		if c.SessionID != "" {
			prompt = canonicalPrompt
			applyRuntimeSnapshot(&c, canonicalRuntime)
		}
	}
	// Rooted conversations retain the prompt that created them for diagnostics.
	// Their next Channel turn still resolves the current group policy below, so
	// prompt edits do not rewrite history or strand an existing Session.
	if c.RootID != "" && c.SessionID != "" && c.SessionID != previousSessionID {
		applyPromptSnapshot(&c, prompt)
	}
	return s.store.PutConversation(ctx, c)
}

// GuardSessionRuntimeMutation keeps Channel binding changes serialized with a
// Session runtime mutation. Callers must release the returned guard after the
// mutation completes so the config -> session lock order remains consistent.
func (s *Service) GuardSessionRuntimeMutation(ctx context.Context, sessionID string) (func(), error) {
	s.configMu.RLock()
	if s.closed {
		s.configMu.RUnlock()
		return nil, errors.New("channel service is closed")
	}
	_, err := s.store.GetConversationBySession(ctx, strings.TrimSpace(sessionID))
	if err == nil {
		s.configMu.RUnlock()
		return nil, errors.New("channel-bound session runtime cannot be changed")
	}
	if !errors.Is(err, ErrNotFound) {
		s.configMu.RUnlock()
		return nil, fmt.Errorf("load channel session binding: %w", err)
	}
	return s.configMu.RUnlock, nil
}

func conversationRuntime(conversation Conversation) SessionRuntimeConfig {
	agentName := strings.TrimSpace(conversation.Agent)
	if agentName == "" {
		agentName = SessionAgentZotigo
	}
	return SessionRuntimeConfig{
		Agent: agentName, ProfileName: strings.TrimSpace(conversation.ProfileName),
		Model: strings.TrimSpace(conversation.Model), ReasoningEffort: strings.TrimSpace(conversation.ReasoningEffort),
	}
}

func applyRuntimeSnapshot(conversation *Conversation, runtime SessionRuntimeConfig) {
	conversation.Agent = runtime.Agent
	conversation.ProfileName = runtime.ProfileName
	conversation.Model = runtime.Model
	conversation.ReasoningEffort = runtime.ReasoningEffort
}

func groupRuntime(group Conversation) SessionRuntimeConfig {
	return conversationRuntime(group)
}

func applyPromptSnapshot(conversation *Conversation, prompt SessionPromptConfig) {
	conversation.AgentInstructionsMode = OverrideReplace
	conversation.AgentInstructions = prompt.AgentInstructions
	conversation.ApprovalInstructionsMode = OverrideReplace
	conversation.ApprovalInstructions = prompt.ApprovalInstructions
	reviewAllTools := prompt.ReviewAllTools
	conversation.ReviewAllTools = &reviewAllTools
}

func (s *Service) ListMessages(ctx context.Context, conversationID string, limit int) ([]Message, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return nil, errors.New("channel service is closed")
	}
	return s.store.ListMessages(ctx, conversationID, limit)
}

func (s *Service) currentContext() context.Context { s.mu.Lock(); defer s.mu.Unlock(); return s.ctx }
func (s *Service) quiesce(ctx context.Context, id string) (*adapterRun, error) {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run != nil {
		run.cancel()
		run.sendMu.Lock()
		run.revoked = true
		run.sendMu.Unlock()
		done := make(chan struct{})
		go func() {
			run.deliveries.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			return run, ctx.Err()
		}
		return run, nil
	}
	return nil, nil
}

func (s *Service) finishStop(ctx context.Context, id string, run *adapterRun) error {
	if err := run.adapter.Stop(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	if s.runs[id] == run {
		delete(s.runs, id)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) stop(ctx context.Context, id string) (*adapterRun, error) {
	run, err := s.quiesce(ctx, id)
	if err != nil || run == nil {
		return run, err
	}
	return run, s.finishStop(ctx, id, run)
}
func (s *Service) restart(ctx context.Context, c Connection) error {
	if _, err := s.stop(ctx, c.ID); err != nil {
		return err
	}
	if !c.Enabled {
		_ = s.store.SetConnectionRuntime(ctx, c.ID, "stopped", "", "", "")
		return nil
	}
	secret, ok, err := s.secrets.Get(c.ID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("app_secret is missing")
	}
	factory := s.factories[c.Provider]
	runCtx, cancel := context.WithCancel(s.currentContext())
	s.mu.Lock()
	s.nextRun++
	generation := s.nextRun
	s.mu.Unlock()
	callbacks := AdapterCallbacks{Inbound: func(eventCtx context.Context, in InboundMessage) error {
		return s.handleInbound(eventCtx, c.ID, generation, in)
	}, Owners: func(ownerIDs []string) {
		s.configMu.RLock()
		defer s.configMu.RUnlock()
		if !s.isGenerationActive(c.ID, generation) {
			return
		}
		if err := s.store.SetConnectionOwners(context.Background(), c.ID, ownerIDs); err != nil && s.logger != nil {
			s.logger.Printf("channel %s: store provider owner: %v", c.ID, err)
		}
	}, Ready: func(botID, botName string) {
		s.configMu.RLock()
		if !s.isGenerationActive(c.ID, generation) {
			s.configMu.RUnlock()
			return
		}
		_ = s.store.SetConnectionRuntime(context.Background(), c.ID, "running", "", botID, botName)
		s.configMu.RUnlock()
		go func() {
			if err := s.reconcileRunProcessingMarkers(context.Background(), c.ID); err != nil && s.logger != nil {
				s.logger.Printf("channel %s: reconcile processing markers after ready: %v", c.ID, err)
			}
		}()
	}, Error: func(err error) {
		s.configMu.RLock()
		if !s.isGenerationActive(c.ID, generation) {
			s.configMu.RUnlock()
			return
		}
		_ = s.store.SetConnectionRuntime(context.Background(), c.ID, "error", err.Error(), "", "")
		s.configMu.RUnlock()
		if s.logger != nil {
			s.logger.Printf("channel %s: %v", c.ID, err)
		}
	}}
	adapter, err := factory.New(c, secret, callbacks)
	if err != nil {
		cancel()
		return err
	}
	run := &adapterRun{adapter: adapter, cancel: cancel, generation: generation, ctx: runCtx}
	s.mu.Lock()
	s.runs[c.ID] = run
	s.mu.Unlock()
	_ = s.store.SetConnectionRuntime(ctx, c.ID, "starting", "", "", "")
	go func() {
		if err := adapter.Start(runCtx); err != nil && runCtx.Err() == nil {
			callbacks.Error(err)
		}
	}()
	if resolver, ok := adapter.(ConversationMetadataResolver); ok {
		s.startRunOperation(c.ID, run, func(operationCtx context.Context) {
			s.refreshConversationNames(operationCtx, c, generation, resolver)
		})
	}
	if _, ok := adapter.(GroupDiscoverer); ok {
		go func() {
			if _, err := s.ListGroups(runCtx, c.ID); err != nil && runCtx.Err() == nil && s.logger != nil {
				s.logger.Printf("channel %s: discover groups: %v", c.ID, err)
			}
		}()
	}
	return nil
}

func senderAllowed(connection Connection, group Conversation, senderID string) bool {
	switch group.SenderPolicy {
	case SenderPolicyOwners:
		return slices.Contains(connection.OwnerSenderIDs, senderID)
	case SenderPolicyAll:
		return strings.TrimSpace(senderID) != ""
	default:
		return slices.Contains(group.AllowedSenderIDs, senderID)
	}
}

func (s *Service) acquireRunOperation(connectionID string, run *adapterRun) bool {
	run.sendMu.Lock()
	defer run.sendMu.Unlock()
	if run.revoked || !s.isCurrentRun(connectionID, run.generation) {
		return false
	}
	run.deliveries.Add(1)
	return true
}

func (s *Service) startRunOperation(connectionID string, run *adapterRun, operation func(context.Context)) {
	if !s.acquireRunOperation(connectionID, run) {
		return
	}
	go func() {
		defer run.deliveries.Done()
		operation(run.ctx)
	}()
}

func (s *Service) refreshConversationNames(ctx context.Context, connection Connection, generation uint64, resolver ConversationMetadataResolver) {
	for _, chatID := range connection.AllowChatIDs {
		name, err := resolver.ResolveConversationName(ctx, chatID)
		if err != nil {
			if ctx.Err() == nil && s.logger != nil {
				s.logger.Printf("channel %s: resolve conversation %s: %v", connection.ID, chatID, err)
			}
			continue
		}
		if name == "" {
			continue
		}
		s.configMu.RLock()
		s.mu.Lock()
		run := s.runs[connection.ID]
		s.mu.Unlock()
		current := s.isRunActive(connection.ID, run, generation)
		if current {
			err = s.store.PutGroupConversationName(ctx, connection.ID, chatID, name)
		}
		s.configMu.RUnlock()
		if err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Printf("channel %s: store conversation %s name: %v", connection.ID, chatID, err)
		}
	}
}

func (s *Service) isRunActive(connectionID string, run *adapterRun, generation uint64) bool {
	if run == nil || run.generation != generation {
		return false
	}
	run.sendMu.Lock()
	defer run.sendMu.Unlock()
	return !run.revoked && s.isCurrentRun(connectionID, generation)
}

func (s *Service) isGenerationActive(connectionID string, generation uint64) bool {
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	return s.isRunActive(connectionID, run, generation)
}

func (s *Service) isCurrentRun(connectionID string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[connectionID]
	return run != nil && run.generation == generation
}

func (s *Service) handleInbound(ctx context.Context, connectionID string, generation uint64, in InboundMessage) error {
	if !s.isGenerationActive(connectionID, generation) {
		return nil
	}
	if strings.TrimSpace(in.ConversationKey) == "" {
		return errors.New("channel adapter returned an empty conversation key")
	}
	s.configMu.RLock()
	initialGroup, initialGroupErr := s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
	s.configMu.RUnlock()
	inboxKey := in.ConversationKey
	if initialGroupErr == nil && initialGroup.SessionStrategy == SessionStrategyShared {
		inboxKey = in.ChatID
	}
	unlockInbox := s.lockInbox(connectionID + "\x00" + inboxKey)
	defer unlockInbox()
	if !s.isGenerationActive(connectionID, generation) {
		return nil
	}
	s.configMu.RLock()
	if !s.isGenerationActive(connectionID, generation) {
		s.configMu.RUnlock()
		return nil
	}
	connection, err := s.store.GetConnection(ctx, connectionID)
	if err != nil {
		s.configMu.RUnlock()
		return err
	}
	// Group authorization is checked before durable message content is stored.
	if !connection.Enabled {
		s.configMu.RUnlock()
		return nil
	}
	group, groupErr := s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
	if groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !senderAllowed(connection, group, in.Sender.ID) {
		s.configMu.RUnlock()
		return nil
	}
	if group.ChatMode == ChatModeTopic && group.SessionStrategy == SessionStrategyShared {
		s.configMu.RUnlock()
		return nil
	}
	if !in.CreatedAt.IsZero() && time.Since(in.CreatedAt) > 5*time.Minute {
		s.configMu.RUnlock()
		return nil
	}
	if group.SessionStrategy == SessionStrategyShared {
		in.TriggerAllowed = in.MentionedBot
		in.ReplyMode = ReplyModeDirect
	}
	message, err := s.store.RecordMessage(ctx, connectionID, in)
	s.configMu.RUnlock()
	if err != nil {
		return err
	}
	claimed, err := s.store.ClaimMessage(ctx, message.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if s.afterMessageClaimed != nil {
		s.afterMessageClaimed()
	}
	s.configMu.RLock()
	if !s.isGenerationActive(connectionID, generation) {
		s.configMu.RUnlock()
		return s.store.SetMessageStatus(ctx, message.ID, "rejected", "connection_replaced")
	}
	connection, err = s.store.GetConnection(ctx, connectionID)
	if err != nil || !connection.Enabled {
		s.configMu.RUnlock()
		return s.store.SetMessageStatus(ctx, message.ID, "rejected", "connection_replaced")
	}
	group, groupErr = s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
	if groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !senderAllowed(connection, group, in.Sender.ID) {
		s.configMu.RUnlock()
		return s.store.SetMessageStatus(ctx, message.ID, "rejected", "group_binding_changed")
	}
	conversation, err := s.store.GetConversation(ctx, message.ConversationID)
	if err != nil {
		s.configMu.RUnlock()
		return err
	}
	reject := func(detail string) error { return s.store.SetMessageStatus(ctx, message.ID, "rejected", detail) }
	if time.Since(message.CreatedAt) > 5*time.Minute {
		s.configMu.RUnlock()
		return reject("stale_message")
	}
	if !in.TriggerAllowed {
		s.configMu.RUnlock()
		return reject("mention_required")
	}
	// A bot can first be mentioned in a reply to an existing topic.
	if conversation.SessionID == "" && (in.StartsConversation || in.MentionedBot || group.SessionStrategy == SessionStrategyShared) {
		s.mu.Lock()
		provisioner, canProvision := s.dispatcher.(SessionProvisioner)
		s.mu.Unlock()
		if group.Enabled && group.WorkspaceID != "" && senderAllowed(connection, group, in.Sender.ID) && canProvision {
			prompt := resolvePromptSnapshot(connection, group)
			provisionErr := provisioner.ProvisionChannelSession(ctx, group.WorkspaceID, conversation.DisplayName, prompt, groupRuntime(group), func(sessionID string, resolved SessionRuntimeConfig) error {
				conversation.SessionID = sessionID
				conversation.WorkspaceID = group.WorkspaceID
				conversation.Enabled = true
				conversation.AllowedSenderIDs = append([]string(nil), group.AllowedSenderIDs...)
				conversation.SenderPolicy = group.SenderPolicy
				conversation.AgentInstructionsMode = OverrideReplace
				conversation.AgentInstructions = prompt.AgentInstructions
				conversation.ApprovalInstructionsMode = OverrideReplace
				conversation.ApprovalInstructions = prompt.ApprovalInstructions
				conversation.ReviewAllTools = &prompt.ReviewAllTools
				applyRuntimeSnapshot(&conversation, resolved)
				var bindErr error
				conversation, bindErr = s.store.PutConversation(ctx, conversation)
				return bindErr
			})
			if provisionErr != nil {
				s.configMu.RUnlock()
				statusCtx, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
				statusErr := s.store.SetMessageStatus(statusCtx, message.ID, "failed", "session_provision_failed: "+provisionErr.Error())
				cancelStatus()
				return errors.Join(provisionErr, statusErr)
			}
		}
	}
	if !conversation.Enabled || conversation.SessionID == "" {
		s.configMu.RUnlock()
		return reject("conversation_not_bound")
	}
	prompt := resolvePromptSnapshot(connection, group)
	agentText, approvalText, review := prompt.AgentInstructions, prompt.ApprovalInstructions, prompt.ReviewAllTools
	if strings.TrimSpace(approvalText) != "" && !review {
		s.configMu.RUnlock()
		return reject("approval_instructions_require_review_all_tools")
	}
	s.mu.Lock()
	run := s.runs[connectionID]
	dispatcher := s.dispatcher
	s.mu.Unlock()
	if run == nil || run.generation != generation || dispatcher == nil {
		s.configMu.RUnlock()
		return reject("channel_unavailable")
	}
	isStop := strings.EqualFold(strings.TrimSpace(in.Text), "/stop")
	controller, supportsControl := dispatcher.(TaskController)
	if isStop && !supportsControl {
		s.configMu.RUnlock()
		return reject("control_not_supported")
	}
	run.sendMu.Lock()
	current := s.isCurrentRun(connectionID, generation) && !run.revoked
	if current {
		connection, err = s.store.GetConnection(ctx, connectionID)
		currentGroup, groupErr := s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
		current = err == nil && connection.Enabled && groupErr == nil && currentGroup.Enabled && currentGroup.WorkspaceID != "" && senderAllowed(connection, currentGroup, in.Sender.ID)
	}
	if !current {
		run.sendMu.Unlock()
		s.configMu.RUnlock()
		return reject("connection_replaced")
	}
	run.deliveries.Add(1)
	leaseOwned := true
	defer func() {
		if leaseOwned {
			run.deliveries.Done()
		}
	}()
	openCtx, cancelOpen := context.WithTimeout(ctx, s.deliveryTimeout)
	stopOpen := context.AfterFunc(run.ctx, cancelOpen)
	markerIntent := ""
	if planner, ok := run.adapter.(ProcessingMarkerPlanner); ok {
		if intent := strings.TrimSpace(planner.ProcessingMarkerIntent(in)); intent != "" {
			if err := s.store.SetMessageProcessingMarker(openCtx, message.ID, intent); err != nil {
				stopOpen()
				cancelOpen()
				run.sendMu.Unlock()
				s.configMu.RUnlock()
				return reject("processing_marker_intent_failed: " + err.Error())
			}
			markerIntent = intent
		}
	}
	handle, err := run.adapter.OpenProgress(openCtx, in)
	stopOpen()
	cancelOpen()
	run.sendMu.Unlock()
	if err != nil {
		s.configMu.RUnlock()
		intentReceipt := DeliveryReceipt{ReplyMode: in.ReplyMode, InboundMessageID: in.MessageID, ProcessingMarkerID: markerIntent}
		if IsUncertainDelivery(err) {
			unknownCtx, cancelUnknown := context.WithTimeout(context.Background(), 5*time.Second)
			persistErr := s.store.SetMessageDeliveryUnknown(unknownCtx, message.ID, intentReceipt, "", "progress_message_delivery_unknown: "+err.Error())
			cancelUnknown()
			_ = s.clearProcessingMarker(message.ID, run.adapter.ResumeProgress(intentReceipt))
			return persistErr
		}
		_ = s.clearProcessingMarker(message.ID, run.adapter.ResumeProgress(intentReceipt))
		return reject("progress_message_failed: " + err.Error())
	}
	receiptCtx, cancelReceipt := context.WithTimeout(context.Background(), 5*time.Second)
	receipt := handle.Receipt()
	err = s.store.SetMessageRunning(receiptCtx, message.ID, receipt)
	cancelReceipt()
	if err != nil {
		unknownCtx, cancelUnknown := context.WithTimeout(context.Background(), 5*time.Second)
		persistErr := s.store.SetMessageDeliveryUnknown(unknownCtx, message.ID, receipt, "", "progress_receipt_persistence_failed")
		cancelUnknown()
		_ = s.clearProcessingMarker(message.ID, handle)
		s.configMu.RUnlock()
		return errors.Join(err, persistErr)
	}
	if !s.isCurrentRun(connectionID, generation) {
		unknownCtx, cancelUnknown := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.store.SetMessageDeliveryUnknown(unknownCtx, message.ID, receipt, "", "connection_replaced_after_progress_open")
		cancelUnknown()
		_ = s.clearProcessingMarker(message.ID, handle)
		s.configMu.RUnlock()
		return nil
	}
	actorRole := ActorRoleMember
	if slices.Contains(connection.OwnerSenderIDs, in.Sender.ID) {
		actorRole = ActorRoleOwner
	}
	task := Task{
		ConnectionID: connectionID, ConversationID: conversation.ID, SessionID: conversation.SessionID, MessageID: in.MessageID,
		Text: in.Text, Images: append([]InboundImage(nil), in.Images...), AgentInstructions: agentText, ApprovalInstructions: approvalText, ReviewAllTools: review,
		Origin: TaskOrigin{
			Provider: connection.Provider, ConnectionName: connection.Name, ChannelID: connection.BotOpenID, ChannelName: connection.BotName, ConversationID: conversation.ID,
			ConversationName: conversation.DisplayName, ConversationType: conversation.ChatType,
			ExternalConversation: conversation.ChatID, ExternalConversationName: conversation.ChatName, ExternalRootID: conversation.RootID, ExternalThreadID: conversation.ThreadID, ExternalMessageID: message.ProviderID, ExternalParentMessageID: message.ParentProviderID,
			Sender: in.Sender, ActorRole: actorRole,
		},
	}
	run.sendMu.Lock()
	current = s.isCurrentRun(connectionID, generation) && !run.revoked
	run.sendMu.Unlock()
	if !current {
		_ = s.store.SetMessageDeliveryUnknown(context.Background(), message.ID, receipt, "", "connection_replaced_before_dispatch")
		_ = s.clearProcessingMarker(message.ID, handle)
		s.configMu.RUnlock()
		return nil
	}
	admissionComplete := make(chan struct{})
	var admissionOnce sync.Once
	signalAdmissionComplete := func() { admissionOnce.Do(func() { close(admissionComplete) }) }
	leaseOwned = false
	go func() {
		defer signalAdmissionComplete()
		defer run.deliveries.Done()
		defer func() { _ = s.clearProcessingMarker(message.ID, handle) }()
		if !s.isCurrentRun(connectionID, generation) {
			_ = s.store.SetMessageStatus(context.Background(), message.ID, "failed", "connection_replaced")
			return
		}
		var deliveryMu sync.Mutex
		var deliveryErr error
		deliverProgress := func(progress Progress) {
			deliveryMu.Lock()
			defer deliveryMu.Unlock()
			if deliveryErr == nil {
				deliveryErr = s.updateProgress(context.Background(), run, generation, task, in.ChatID, message.ID, handle, progress)
			}
		}
		var result TaskResult
		var dispatchErr error
		if !isStop && len(task.Images) > 0 {
			resolver, ok := run.adapter.(InboundImageResolver)
			if ok {
				task.Images, dispatchErr = resolver.ResolveInboundImages(run.ctx, task.Images)
			} else {
				for _, image := range task.Images {
					if len(image.Data) == 0 {
						dispatchErr = errors.New("channel adapter cannot resolve inbound images")
						break
					}
				}
			}
		}
		if dispatchErr == nil {
			if isStop {
				result, dispatchErr = controller.StopChannelTask(run.ctx, task, deliverProgress, signalAdmissionComplete)
			} else {
				result, dispatchErr = dispatcher.DispatchChannelTask(run.ctx, task, deliverProgress, signalAdmissionComplete)
			}
		}
		deliveryMu.Lock()
		progressErr := deliveryErr
		deliveryMu.Unlock()
		if progressErr != nil {
			_ = s.store.SetMessageDeliveryUnknown(context.Background(), message.ID, receipt, "", "progress_update_failed: "+progressErr.Error())
			return
		}
		if dispatchErr != nil {
			failErr := s.failProgress(context.Background(), run, generation, task, in.ChatID, handle, Progress{State: "failed", Text: "Zotigo could not complete this task. Open the bound session for details."})
			if failErr != nil {
				_ = s.store.SetMessageDeliveryUnknown(context.Background(), message.ID, receipt, "", "failure_progress_update_failed: "+failErr.Error())
			} else {
				_ = s.store.SetMessageStatus(context.Background(), message.ID, "failed", dispatchErr.Error())
			}
			return
		}
		finalMessageID, completeErr := s.completeProgress(context.Background(), run, generation, task, in.ChatID, message.ID, handle, result)
		if completeErr != nil {
			_ = s.store.SetMessageDeliveryUnknown(context.Background(), message.ID, receipt, finalMessageID, "final_progress_update_failed: "+completeErr.Error())
			return
		}
		_ = s.store.SetMessageProcessed(context.Background(), message.ID, finalMessageID)
	}()
	select {
	case <-admissionComplete:
	case <-ctx.Done():
	}
	s.configMu.RUnlock()
	return nil
}

func (s *Service) clearProcessingMarker(messageID string, handle ProgressHandle) error {
	marker, ok := handle.(ProcessingMarkerHandle)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := marker.ClearProcessingMarker(ctx); err != nil {
		if s.logger != nil {
			s.logger.Printf("clear processing marker for %s: %v", messageID, err)
		}
		return err
	}
	if err := s.store.ClearMessageProcessingMarker(ctx, messageID); err != nil {
		if s.logger != nil {
			s.logger.Printf("clear processing marker receipt for %s: %v", messageID, err)
		}
		return err
	}
	return nil
}

func (s *Service) reconcileProcessingMarkers(ctx context.Context, connectionID string, adapter Adapter) error {
	messages, err := s.store.ListMessagesWithProcessingMarkers(ctx, connectionID)
	if err != nil {
		if ctx.Err() == nil && s.logger != nil {
			s.logger.Printf("channel %s: list processing markers: %v", connectionID, err)
		}
		return err
	}
	var result error
	for _, message := range messages {
		receipt := DeliveryReceipt{Mode: message.DeliveryMode, ReplyMode: message.ReplyMode, MessageID: message.ReplyMessageID, COTID: message.COTID, InboundMessageID: message.ProviderID, ProcessingMarkerID: message.ProcessingMarkerID}
		handle := adapter.ResumeProgress(receipt)
		result = errors.Join(result, s.clearProcessingMarker(message.ID, handle))
	}
	return result
}

func (s *Service) reconcileRunProcessingMarkers(ctx context.Context, connectionID string) error {
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if run != nil {
		if !s.acquireRunOperation(connectionID, run) {
			return errors.New("channel adapter is stopping")
		}
		defer run.deliveries.Done()
		run.markerMu.Lock()
		defer run.markerMu.Unlock()
		return s.reconcileProcessingMarkers(ctx, connectionID, run.adapter)
	}
	messages, err := s.store.ListMessagesWithProcessingMarkers(ctx, connectionID)
	if err != nil {
		return err
	}
	if len(messages) > 0 {
		return errors.New("processing markers remain but no provider adapter is running")
	}
	return nil
}

func resolvePromptSnapshot(connection Connection, conversation Conversation) SessionPromptConfig {
	prompt := SessionPromptConfig{AgentInstructions: connection.AgentInstructions, ApprovalInstructions: connection.ApprovalInstructions, ReviewAllTools: connection.ReviewAllTools}
	if conversation.AgentInstructionsMode == OverrideReplace {
		prompt.AgentInstructions = conversation.AgentInstructions
	}
	if conversation.ApprovalInstructionsMode == OverrideReplace {
		prompt.ApprovalInstructions = conversation.ApprovalInstructions
	}
	if conversation.ReviewAllTools != nil {
		prompt.ReviewAllTools = *conversation.ReviewAllTools
	}
	return prompt
}

func (s *Service) withDeliveryAuthorization(ctx context.Context, run *adapterRun, generation uint64, task Task, chatID string, deliver func(context.Context) error) error {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	run.sendMu.Lock()
	defer run.sendMu.Unlock()
	if !s.isCurrentRun(task.ConnectionID, generation) || run.revoked {
		return errors.New("channel connection was replaced")
	}
	connection, err := s.store.GetConnection(ctx, task.ConnectionID)
	group, groupErr := s.store.GetConversationByScope(ctx, task.ConnectionID, chatID, "")
	if err != nil || !connection.Enabled || groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !senderAllowed(connection, group, task.Origin.Sender.ID) {
		return errors.New("channel destination is no longer allowed")
	}
	conversation, err := s.store.GetConversation(ctx, task.ConversationID)
	if err != nil || !conversation.Enabled || conversation.SessionID != task.SessionID || conversation.ChatID != chatID {
		return errors.New("channel binding is no longer active")
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, s.deliveryTimeout)
	defer cancel()
	stopDelivery := context.AfterFunc(run.ctx, cancel)
	defer stopDelivery()
	return deliver(deliveryCtx)
}

func (s *Service) updateProgress(ctx context.Context, run *adapterRun, generation uint64, task Task, chatID, messageID string, handle ProgressHandle, progress Progress) error {
	err := s.withDeliveryAuthorization(ctx, run, generation, task, chatID, func(deliveryCtx context.Context) error { return handle.Update(deliveryCtx, progress) })
	if err == nil && progress.Sequence > 0 {
		err = s.store.SetMessageProjected(ctx, messageID, progress.Sequence)
	}
	return err
}

func (s *Service) completeProgress(ctx context.Context, run *adapterRun, generation uint64, task Task, chatID, messageID string, handle ProgressHandle, result TaskResult) (string, error) {
	var finalMessageID string
	err := s.withDeliveryAuthorization(ctx, run, generation, task, chatID, func(deliveryCtx context.Context) error {
		return handle.Complete(deliveryCtx, result, func(id string) error {
			finalMessageID = id
			return s.store.SetMessageFinalMessageID(deliveryCtx, messageID, id)
		})
	})
	return finalMessageID, err
}

func (s *Service) failProgress(ctx context.Context, run *adapterRun, generation uint64, task Task, chatID string, handle ProgressHandle, progress Progress) error {
	return s.withDeliveryAuthorization(ctx, run, generation, task, chatID, func(deliveryCtx context.Context) error { return handle.Fail(deliveryCtx, progress) })
}

func (s *Service) patchInterrupted(ctx context.Context, messages []Message) {
	for _, message := range messages {
		if message.ReplyMessageID == "" {
			continue
		}
		s.mu.Lock()
		run := s.runs[message.ConnectionID]
		s.mu.Unlock()
		conversation, conversationErr := s.store.GetConversation(ctx, message.ConversationID)
		if run == nil || conversationErr != nil {
			_ = s.store.SetMessageStatus(ctx, message.ID, "delivery_unknown", "restart_progress_not_closed")
			continue
		}
		task := Task{ConnectionID: message.ConnectionID, ConversationID: message.ConversationID, SessionID: conversation.SessionID, Origin: TaskOrigin{Sender: message.Sender}}
		receipt := DeliveryReceipt{Mode: message.DeliveryMode, ReplyMode: message.ReplyMode, MessageID: message.ReplyMessageID, COTID: message.COTID, InboundMessageID: message.ProviderID, ProcessingMarkerID: message.ProcessingMarkerID}
		handle := run.adapter.ResumeProgress(receipt)
		err := s.withDeliveryAuthorization(ctx, run, run.generation, task, conversation.ChatID, func(deliveryCtx context.Context) error {
			return handle.Recover(deliveryCtx, message.FinalMessageID != "")
		})
		if err != nil {
			_ = s.store.SetMessageStatus(ctx, message.ID, "delivery_unknown", "restart_progress_update_failed: "+err.Error())
		} else if message.FinalMessageID != "" {
			_ = s.store.SetMessageStatus(ctx, message.ID, "processed", "")
		}
		_ = s.clearProcessingMarker(message.ID, handle)
	}
}
