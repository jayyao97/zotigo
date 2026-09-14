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

type AdapterCallbacks struct {
	Inbound func(context.Context, InboundMessage) error
	Ready   func(botOpenID, botName string)
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
	return nil
}

func (s *Service) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	if s.closed {
		s.configMu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Lock()
	runs := s.runs
	s.runs = map[string]*adapterRun{}
	s.mu.Unlock()
	s.configMu.Unlock()
	var result error
	for _, run := range runs {
		run.cancel()
		run.sendMu.Lock()
		run.revoked = true
		run.sendMu.Unlock()
		result = errors.Join(result, run.adapter.Stop(ctx))
	}
	return errors.Join(result, s.store.Close())
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
	if secretChanged {
		if err := s.secrets.Set(id, candidateSecret); err != nil {
			return Connection{}, err
		}
	}
	value, err = s.store.PutConnection(ctx, value)
	if err != nil {
		if secretChanged {
			rollbackSecret := ""
			if hadOldSecret {
				rollbackSecret = oldSecret
			}
			if rollbackErr := s.secrets.Set(id, rollbackSecret); rollbackErr != nil {
				return Connection{}, errors.Join(err, fmt.Errorf("restore channel secret: %w", rollbackErr))
			}
		}
		return Connection{}, err
	}
	s.configMu.Unlock()
	configLocked = false
	if s.currentContext() != nil && connectionNeedsRestart(old, value, existed, secretChanged) {
		if err = s.restart(ctx, value); err != nil {
			_ = s.store.SetConnectionRuntime(ctx, id, "error", err.Error(), "", "")
			return s.GetConnection(ctx, id)
		}
	}
	return s.GetConnection(ctx, id)
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
	s.configMu.RLock()
	if s.closed {
		s.configMu.RUnlock()
		return errors.New("channel service is closed")
	}
	s.configMu.RUnlock()
	s.stop(ctx, id)
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
		if discoverer, ok := run.adapter.(GroupDiscoverer); ok {
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
	if discoveryGeneration != 0 && !s.isCurrentRun(connection.ID, discoveryGeneration) {
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
		group.ConversationID = conversation.ID
		group.WorkspaceID = conversation.WorkspaceID
		group.Enabled = conversation.Enabled
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
func (s *Service) GetConversation(ctx context.Context, id string) (Conversation, error) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return Conversation{}, errors.New("channel service is closed")
	}
	return s.store.GetConversation(ctx, id)
}
func (s *Service) PutConversation(ctx context.Context, id string, input ConversationInput) (Conversation, error) {
	return s.PutConversationValidated(ctx, id, input, nil)
}

// PutConversationValidated updates a conversation while holding the channel
// configuration write lock. The validator is invoked only for a new or changed
// non-empty session binding. Keeping validation inside this lock gives callers
// a single config -> session lock order, matching inbound admission.
func (s *Service) PutConversationValidated(ctx context.Context, id string, input ConversationInput, validateBinding func(context.Context, Conversation, SessionPromptConfig) error) (Conversation, error) {
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
	previous := c
	previousSessionID := c.SessionID
	previousWorkspaceID := c.WorkspaceID
	if input.AgentInstructionsMode != "" && input.AgentInstructionsMode != OverrideInherit && input.AgentInstructionsMode != OverrideReplace {
		return c, errors.New("invalid agent_instructions_mode")
	}
	if input.ApprovalInstructionsMode != "" && input.ApprovalInstructionsMode != OverrideInherit && input.ApprovalInstructionsMode != OverrideReplace {
		return c, errors.New("invalid approval_instructions_mode")
	}
	c.DisplayName = strings.TrimSpace(input.DisplayName)
	c.SessionID = strings.TrimSpace(input.SessionID)
	c.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	c.Enabled = input.Enabled
	c.AllowedSenderIDs = input.AllowedSenderIDs
	c.AgentInstructionsMode = input.AgentInstructionsMode
	c.AgentInstructions = input.AgentInstructions
	c.ApprovalInstructionsMode = input.ApprovalInstructionsMode
	c.ApprovalInstructions = input.ApprovalInstructions
	c.ReviewAllTools = input.ReviewAllTools
	if c.RootID == "" {
		c.SessionID = ""
		if c.Enabled && (c.WorkspaceID == "" || len(normalizeIDs(c.AllowedSenderIDs)) == 0) {
			return c, errors.New("an enabled group requires a workspace and at least one allowed sender")
		}
	} else if c.Enabled && (c.SessionID == "" || len(normalizeIDs(c.AllowedSenderIDs)) == 0) {
		return c, errors.New("an enabled conversation requires a session and at least one allowed sender")
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
	previousPrompt := resolvePromptSnapshot(connection, previous)
	if previous.RootID != "" && previous.SessionID != "" && c.SessionID == previous.SessionID && !hasPromptSnapshot(previous) {
		s.mu.Lock()
		dispatcher := s.dispatcher
		s.mu.Unlock()
		if dispatcher == nil {
			return c, errors.New("bound session prompt snapshot requires an available dispatcher")
		}
		canonical, snapshotErr := dispatcher.EnsureChannelSessionPrompt(ctx, previous.SessionID, previousPrompt)
		if snapshotErr != nil {
			return c, fmt.Errorf("resolve bound session prompt snapshot: %w", snapshotErr)
		}
		applyPromptSnapshot(&previous, canonical)
		if _, storeErr := s.store.PutConversation(ctx, previous); storeErr != nil {
			return c, fmt.Errorf("persist bound conversation prompt snapshot: %w", storeErr)
		}
		previousPrompt = canonical
		// Legacy clients loaded inherit/null fields before this request. Preserve
		// the canonical Session values for omitted/inherited fields while still
		// rejecting an explicit attempt to change the immutable snapshot below.
		if c.AgentInstructionsMode != OverrideReplace {
			c.AgentInstructionsMode, c.AgentInstructions = OverrideReplace, canonical.AgentInstructions
		}
		if c.ApprovalInstructionsMode != OverrideReplace {
			c.ApprovalInstructionsMode, c.ApprovalInstructions = OverrideReplace, canonical.ApprovalInstructions
		}
		if c.ReviewAllTools == nil {
			reviewAllTools := canonical.ReviewAllTools
			c.ReviewAllTools = &reviewAllTools
		}
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
	if c.RootID != "" && previousSessionID != "" && c.SessionID == previousSessionID && previousPrompt != prompt {
		return c, errors.New("bound session prompt snapshot cannot be changed")
	}
	if validateBinding != nil && (c.SessionID != previousSessionID || c.WorkspaceID != previousWorkspaceID) {
		if validateErr := validateBinding(ctx, c, prompt); validateErr != nil {
			return c, validateErr
		}
	}
	// A rooted conversation and its bound Session share one immutable prompt
	// snapshot. Persist the resolved values after binding validation succeeds so
	// later Connection changes cannot make the Conversation appear to inherit a
	// different prompt from the one stored on the Session.
	if c.RootID != "" && c.SessionID != "" && c.SessionID != previousSessionID {
		applyPromptSnapshot(&c, prompt)
	}
	return s.store.PutConversation(ctx, c)
}

func hasPromptSnapshot(conversation Conversation) bool {
	return conversation.AgentInstructionsMode == OverrideReplace && conversation.ApprovalInstructionsMode == OverrideReplace && conversation.ReviewAllTools != nil
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
func (s *Service) stop(ctx context.Context, id string) {
	s.mu.Lock()
	run := s.runs[id]
	delete(s.runs, id)
	s.mu.Unlock()
	if run != nil {
		run.cancel()
		run.sendMu.Lock()
		run.revoked = true
		run.sendMu.Unlock()
		_ = run.adapter.Stop(ctx)
	}
}
func (s *Service) restart(ctx context.Context, c Connection) error {
	s.stop(ctx, c.ID)
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
	}, Ready: func(botID, botName string) {
		if !s.isCurrentRun(c.ID, generation) {
			return
		}
		_ = s.store.SetConnectionRuntime(context.Background(), c.ID, "running", "", botID, botName)
	}, Error: func(err error) {
		if !s.isCurrentRun(c.ID, generation) {
			return
		}
		_ = s.store.SetConnectionRuntime(context.Background(), c.ID, "error", err.Error(), "", "")
		if s.logger != nil {
			s.logger.Printf("channel %s: %v", c.ID, err)
		}
	}}
	adapter, err := factory.New(c, secret, callbacks)
	if err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	s.runs[c.ID] = &adapterRun{adapter: adapter, cancel: cancel, generation: generation, ctx: runCtx}
	s.mu.Unlock()
	_ = s.store.SetConnectionRuntime(ctx, c.ID, "starting", "", "", "")
	go func() {
		if err := adapter.Start(runCtx); err != nil && runCtx.Err() == nil {
			callbacks.Error(err)
		}
	}()
	if resolver, ok := adapter.(ConversationMetadataResolver); ok {
		go s.refreshConversationNames(runCtx, c, generation, resolver)
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

func (s *Service) refreshConversationNames(ctx context.Context, connection Connection, generation uint64, resolver ConversationMetadataResolver) {
	for _, chatID := range connection.AllowChatIDs {
		name, err := resolver.ResolveConversationName(ctx, chatID)
		if err != nil {
			if ctx.Err() == nil && s.logger != nil {
				s.logger.Printf("channel %s: resolve conversation %s: %v", connection.ID, chatID, err)
			}
			continue
		}
		if name == "" || !s.isCurrentRun(connection.ID, generation) {
			continue
		}
		s.configMu.RLock()
		current := s.isCurrentRun(connection.ID, generation)
		if current {
			err = s.store.PutGroupConversationName(ctx, connection.ID, chatID, name)
		}
		s.configMu.RUnlock()
		if err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Printf("channel %s: store conversation %s name: %v", connection.ID, chatID, err)
		}
	}
}

func (s *Service) isCurrentRun(connectionID string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[connectionID]
	return run != nil && run.generation == generation
}

func (s *Service) handleInbound(ctx context.Context, connectionID string, generation uint64, in InboundMessage) error {
	if !s.isCurrentRun(connectionID, generation) {
		return nil
	}
	if strings.TrimSpace(in.ConversationKey) == "" {
		return errors.New("channel adapter returned an empty conversation key")
	}
	unlockInbox := s.lockInbox(connectionID + "\x00" + in.ConversationKey)
	defer unlockInbox()
	if !s.isCurrentRun(connectionID, generation) {
		return nil
	}
	s.configMu.RLock()
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
	if groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !slices.Contains(group.AllowedSenderIDs, in.Sender.ID) {
		s.configMu.RUnlock()
		return nil
	}
	if !in.CreatedAt.IsZero() && time.Since(in.CreatedAt) > 5*time.Minute {
		s.configMu.RUnlock()
		return nil
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
	group, groupErr = s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
	if groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !slices.Contains(group.AllowedSenderIDs, in.Sender.ID) {
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
	if conversation.SessionID == "" && in.StartsConversation {
		s.mu.Lock()
		provisioner, canProvision := s.dispatcher.(SessionProvisioner)
		s.mu.Unlock()
		if group.Enabled && group.WorkspaceID != "" && slices.Contains(group.AllowedSenderIDs, in.Sender.ID) && canProvision {
			prompt := resolvePromptSnapshot(connection, group)
			provisionErr := provisioner.ProvisionChannelSession(ctx, group.WorkspaceID, conversation.DisplayName, prompt, func(sessionID string) error {
				conversation.SessionID = sessionID
				conversation.WorkspaceID = group.WorkspaceID
				conversation.Enabled = true
				conversation.AllowedSenderIDs = append([]string(nil), group.AllowedSenderIDs...)
				conversation.AgentInstructionsMode = OverrideReplace
				conversation.AgentInstructions = prompt.AgentInstructions
				conversation.ApprovalInstructionsMode = OverrideReplace
				conversation.ApprovalInstructions = prompt.ApprovalInstructions
				conversation.ReviewAllTools = &prompt.ReviewAllTools
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
	// Rooted conversations created before prompt snapshots were introduced are
	// resolved once, then persisted in the same explicit form as new Sessions.
	if conversation.AgentInstructionsMode != OverrideReplace || conversation.ApprovalInstructionsMode != OverrideReplace || conversation.ReviewAllTools == nil {
		prompt := resolvePromptSnapshot(connection, conversation)
		s.mu.Lock()
		dispatcher := s.dispatcher
		s.mu.Unlock()
		if dispatcher == nil {
			s.configMu.RUnlock()
			return reject("channel_unavailable")
		}
		var snapshotErr error
		prompt, snapshotErr = dispatcher.EnsureChannelSessionPrompt(ctx, conversation.SessionID, prompt)
		if snapshotErr != nil {
			s.configMu.RUnlock()
			statusCtx, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
			statusErr := s.store.SetMessageStatus(statusCtx, message.ID, "failed", "session_prompt_snapshot_failed")
			cancelStatus()
			return errors.Join(snapshotErr, statusErr)
		}
		conversation.AgentInstructionsMode, conversation.AgentInstructions = OverrideReplace, prompt.AgentInstructions
		conversation.ApprovalInstructionsMode, conversation.ApprovalInstructions = OverrideReplace, prompt.ApprovalInstructions
		conversation.ReviewAllTools = &prompt.ReviewAllTools
		conversation, err = s.store.PutConversation(ctx, conversation)
		if err != nil {
			s.configMu.RUnlock()
			return err
		}
	}
	agentText, approvalText, review := conversation.AgentInstructions, conversation.ApprovalInstructions, *conversation.ReviewAllTools
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
		current = err == nil && connection.Enabled && groupErr == nil && currentGroup.Enabled && currentGroup.WorkspaceID != "" && slices.Contains(currentGroup.AllowedSenderIDs, in.Sender.ID)
	}
	if !current {
		run.sendMu.Unlock()
		s.configMu.RUnlock()
		return reject("connection_replaced")
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, s.deliveryTimeout)
	stopOpen := context.AfterFunc(run.ctx, cancelOpen)
	handle, err := run.adapter.OpenProgress(openCtx, in)
	stopOpen()
	cancelOpen()
	run.sendMu.Unlock()
	if err != nil {
		s.configMu.RUnlock()
		if IsUncertainDelivery(err) {
			unknownCtx, cancelUnknown := context.WithTimeout(context.Background(), 5*time.Second)
			persistErr := s.store.SetMessageDeliveryUnknown(unknownCtx, message.ID, DeliveryReceipt{}, "", "progress_message_delivery_unknown: "+err.Error())
			cancelUnknown()
			return persistErr
		}
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
		s.configMu.RUnlock()
		return errors.Join(err, persistErr)
	}
	if !s.isCurrentRun(connectionID, generation) {
		unknownCtx, cancelUnknown := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.store.SetMessageDeliveryUnknown(unknownCtx, message.ID, receipt, "", "connection_replaced_after_progress_open")
		cancelUnknown()
		s.configMu.RUnlock()
		return nil
	}
	actorRole := ActorRoleMember
	if slices.Contains(connection.OwnerSenderIDs, in.Sender.ID) {
		actorRole = ActorRoleOwner
	}
	task := Task{
		ConnectionID: connectionID, ConversationID: conversation.ID, SessionID: conversation.SessionID, MessageID: in.MessageID,
		Text: in.Text, AgentInstructions: agentText, ApprovalInstructions: approvalText, ReviewAllTools: review,
		Origin: TaskOrigin{
			Provider: connection.Provider, ConnectionName: connection.Name, ConversationID: conversation.ID,
			ConversationName: conversation.DisplayName, ConversationType: conversation.ChatType,
			ExternalConversation: conversation.ChatID, ExternalRootID: conversation.RootID, ExternalThreadID: conversation.ThreadID, ExternalMessageID: in.MessageID,
			Sender: in.Sender, ActorRole: actorRole,
		},
	}
	admissionComplete := make(chan struct{})
	var admissionOnce sync.Once
	signalAdmissionComplete := func() { admissionOnce.Do(func() { close(admissionComplete) }) }
	go func() {
		defer signalAdmissionComplete()
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
		if isStop {
			result, dispatchErr = controller.StopChannelTask(run.ctx, task, deliverProgress, signalAdmissionComplete)
		} else {
			result, dispatchErr = dispatcher.DispatchChannelTask(run.ctx, task, deliverProgress, signalAdmissionComplete)
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
	if err != nil || !connection.Enabled || groupErr != nil || !group.Enabled || group.WorkspaceID == "" || !slices.Contains(group.AllowedSenderIDs, task.Origin.Sender.ID) {
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
		receipt := DeliveryReceipt{Mode: message.DeliveryMode, MessageID: message.ReplyMessageID, COTID: message.COTID}
		handle := run.adapter.ResumeProgress(receipt)
		err := s.withDeliveryAuthorization(ctx, run, run.generation, task, conversation.ChatID, func(deliveryCtx context.Context) error {
			return handle.Recover(deliveryCtx, message.FinalMessageID != "")
		})
		if err != nil {
			_ = s.store.SetMessageStatus(ctx, message.ID, "delivery_unknown", "restart_progress_update_failed: "+err.Error())
		} else if message.FinalMessageID != "" {
			_ = s.store.SetMessageStatus(ctx, message.ID, "processed", "")
		}
	}
}
