package zotigod

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type SessionState string

const (
	SessionStateCreated  SessionState = "created"
	SessionStateStarting SessionState = "starting"
	SessionStateRunning  SessionState = "running"
	SessionStatePausing  SessionState = "pausing"
	SessionStatePaused   SessionState = "paused"
	SessionStateOffline  SessionState = "offline"
	SessionStateEnded    SessionState = "ended"
	SessionStateFailed   SessionState = "failed"
)

type Session struct {
	ReadState            *zotigosession.ReadState  `json:"read_state,omitempty"`
	ForkedFrom           *zotigosession.ForkOrigin `json:"forked_from,omitempty"`
	ID                   string                    `json:"id"`
	State                SessionState              `json:"state"`
	Live                 bool                      `json:"live"`
	WorkingDirectory     string                    `json:"working_directory,omitempty"`
	Agent                string                    `json:"agent"`
	ProfileName          string                    `json:"profile,omitempty"`
	Model                string                    `json:"model,omitempty"`
	ReasoningEffort      string                    `json:"reasoning_effort,omitempty"`
	ApprovalPolicy       agent.ApprovalPolicy      `json:"approval_policy"`
	LastPrompt           string                    `json:"last_prompt,omitempty"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
	StartedAt            *time.Time                `json:"started_at,omitempty"`
	EndedAt              *time.Time                `json:"ended_at,omitempty"`
	Error                string                    `json:"error,omitempty"`
	ErrorCode            string                    `json:"error_code,omitempty"`
	Working              bool                      `json:"working"`
	ActiveTool           string                    `json:"active_tool,omitempty"`
	ContextUsage         *SessionContextUsage      `json:"context_usage"`
	ChannelToolsVersion  uint64                    `json:"channel_tools_version,omitempty"`
	SessionToolsVersion  uint64                    `json:"session_tools_version,omitempty"`
	ChannelToolsEligible bool                      `json:"channel_tools_eligible"`
	seq                  uint64
	activationSource     string
	workerActivation     uint64
	workerGeneration     string
}

type SessionContextUsage struct {
	Tokens    int       `json:"tokens,omitempty"`
	Window    int       `json:"window,omitempty"`
	Status    string    `json:"status"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

var (
	errSessionNotFound               = errors.New("session not found")
	errInvalidSessionTransition      = errors.New("invalid session state transition")
	errSessionProfileNotFound        = errors.New("session profile not found")
	errWorkerDisconnectedBeforeReady = errors.New("worker disconnected before becoming ready")
	errRuntimeOccupied               = errors.New("runtime session is open in another application")
)

const runtimeOccupiedErrorPrefix = "runtime_occupied: "

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
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	}
	if session.ContextUsage == nil {
		session.ContextUsage = unavailableSessionContextUsage()
	}
	session.seq = r.nextID
	r.sessions[session.ID] = session
	r.notifyChangedLocked()
	return session
}

func newSession(workingDirectory string, profileName string) Session {
	return Session{
		ID:                   newZotigodID("sess"),
		State:                SessionStateCreated,
		Live:                 true,
		WorkingDirectory:     workingDirectory,
		Agent:                string(zotigoruntime.AgentZotigo),
		ProfileName:          profileName,
		ApprovalPolicy:       agent.ApprovalPolicyAuto,
		CreatedAt:            time.Now().UTC(),
		ChannelToolsEligible: true,
		SessionToolsVersion:  1,
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

func (r *sessionRegistry) MarkWorking(id string, activeTool string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || (session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused) {
		return
	}
	changed := !session.Working
	session.Working = true
	if session.ActiveTool != activeTool {
		session.ActiveTool = activeTool
		changed = true
	}
	if !changed {
		return
	}
	r.sessions[id] = session
	r.notifyChangedLocked()
}

func (r *sessionRegistry) MarkIdle(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || (!session.Working && session.ActiveTool == "" && session.State != SessionStatePausing) {
		return
	}
	if session.State == SessionStatePausing {
		session.State = SessionStateRunning
	}
	session.Working = false
	session.ActiveTool = ""
	r.sessions[id] = session
	r.notifyChangedLocked()
}

func (r *sessionRegistry) Start(id string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateCreated}, func(session *Session) {
		session.State = SessionStateStarting
		session.workerActivation++
		session.workerGeneration = ""
		session.StartedAt = &now
		if session.activationSource == "" {
			session.activationSource = "start"
		}
	})
}

func (r *sessionRegistry) MarkRunning(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateRunning
	})
}

func (r *sessionRegistry) RestartWorker(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateStarting
		session.workerActivation++
		session.workerGeneration = ""
		session.activationSource = "restart"
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

func (r *sessionRegistry) BeginStopping(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateRunning}, func(session *Session) {
		session.State = SessionStatePausing
		session.Working = true
	})
}

func (r *sessionRegistry) FinishStopping(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStatePausing}, func(session *Session) {
		session.State = SessionStateRunning
		session.Working = true
	})
}

func (r *sessionRegistry) UpdateProfile(id string, profileName string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.ProfileName = profileName
	})
}

func (r *sessionRegistry) UpdateApprovalPolicy(id string, policy agent.ApprovalPolicy) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.ApprovalPolicy = policy
	})
}

func (r *sessionRegistry) UpdateCodexSettings(id string, model string, reasoningEffort string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateCreated, SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.Model = model
		session.ReasoningEffort = reasoningEffort
	})
}

func (r *sessionRegistry) End(id string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateEnded
		session.EndedAt = &now
	})
}

func (r *sessionRegistry) Fail(id string, message string) (Session, error) {
	return r.FailWithCode(id, "", message)
}

func (r *sessionRegistry) FailWithCode(id string, code string, message string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused}, func(session *Session) {
		session.State = SessionStateFailed
		session.EndedAt = &now
		session.Error = message
		session.ErrorCode = code
	})
}

func (r *sessionRegistry) FailStarting(id string, message string) (Session, error) {
	return r.FailStartingWithCode(id, "", message)
}

func (r *sessionRegistry) FailStartingWithCode(id string, code string, message string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateFailed
		session.EndedAt = &now
		session.Error = message
		session.ErrorCode = code
	})
}

func (r *sessionRegistry) RetryFailedWorker(id string) (Session, error) {
	now := time.Now().UTC()
	return r.transition(id, []SessionState{SessionStateFailed}, func(session *Session) {
		session.State = SessionStateStarting
		session.workerActivation++
		session.StartedAt = &now
		session.EndedAt = nil
		session.Error = ""
		session.ErrorCode = ""
		session.activationSource = "restart"
		session.workerGeneration = ""
	})
}

func (r *sessionRegistry) ResetStarting(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateStarting}, func(session *Session) {
		session.State = SessionStateCreated
		session.StartedAt = nil
		session.EndedAt = nil
		session.Error = ""
		session.ErrorCode = ""
	})
}

func (r *sessionRegistry) ReleaseIdleWorker(id string) (Session, error) {
	return r.transition(id, []SessionState{SessionStateRunning}, func(session *Session) {
		session.State = SessionStateCreated
		session.StartedAt = nil
		session.EndedAt = nil
		session.Error = ""
		session.ErrorCode = ""
		session.activationSource = "resume"
		session.workerGeneration = ""
	})
}

func (r *sessionRegistry) SetWorkerGeneration(id string, generation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return errSessionNotFound
	}
	if session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused {
		return errInvalidSessionTransition
	}
	session.workerGeneration = generation
	r.sessions[id] = session
	return nil
}

func (r *sessionRegistry) WorkerGenerationMatches(id string, generation string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	return ok && generation != "" && session.workerGeneration == generation
}

func (r *sessionRegistry) ClearWorkerGeneration(id string, generation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.workerGeneration != generation {
		return
	}
	session.workerGeneration = ""
	r.sessions[id] = session
}

func isRuntimeOccupiedSession(session Session) bool {
	return session.ErrorCode == "runtime_occupied" || strings.HasPrefix(session.Error, runtimeOccupiedErrorPrefix)
}

func runtimeOccupiedMessage(session Session) string {
	return strings.TrimPrefix(session.Error, runtimeOccupiedErrorPrefix)
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
	if session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused {
		session.Working = false
		session.ActiveTool = ""
		session.workerGeneration = ""
	}
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
