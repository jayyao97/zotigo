// Package zotigodclient is the Go client used by product entrypoints (the
// TUI and the ACP adapter) to drive sessions through the zotigod session
// service. It speaks the public HTTP API documented in docs/zotigod-api.md
// and works the same against the shared daemon and an embedded service.
//
// Third-party code that only needs the agent loop should use core/agent and
// friends directly; this package is product wiring, not an SDK.
package zotigodclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
)

// Client calls one zotigod endpoint.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for baseURL. token may be empty for a loopback daemon
// started without --auth-token-file.
func New(baseURL string, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// No client-wide timeout: event streams are long-lived. Callers bound
		// ordinary requests through their contexts.
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// BaseURL reports the endpoint this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// Via returns a client with the same credentials that reaches the service
// through baseURL, for example a proxy in front of it.
func (c *Client) Via(baseURL string) *Client {
	return New(baseURL, c.token)
}

// APIError is a structured non-2xx response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("zotigod %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("zotigod %d %s: %s", e.Status, e.Code, e.Message)
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

type Health struct {
	Status          string `json:"status"`
	ProtocolVersion string `json:"protocol_version"`
	Version         string `json:"version,omitempty"`
	Commit          string `json:"commit,omitempty"`
	ProcessID       string `json:"process_id,omitempty"`
}

type ContextUsage struct {
	Tokens    int       `json:"tokens,omitempty"`
	Window    int       `json:"window,omitempty"`
	Status    string    `json:"status"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Session mirrors the public session DTO.
type Session struct {
	ID               string        `json:"id"`
	State            string        `json:"state"`
	Live             bool          `json:"live"`
	WorkingDirectory string        `json:"working_directory,omitempty"`
	Agent            string        `json:"agent"`
	Profile          string        `json:"profile,omitempty"`
	Model            string        `json:"model,omitempty"`
	ReasoningEffort  string        `json:"reasoning_effort,omitempty"`
	ApprovalPolicy   string        `json:"approval_policy"`
	LastPrompt       string        `json:"last_prompt,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Error            string        `json:"error,omitempty"`
	ErrorCode        string        `json:"error_code,omitempty"`
	Working          bool          `json:"working"`
	ActiveTool       string        `json:"active_tool,omitempty"`
	ContextUsage     *ContextUsage `json:"context_usage"`
}

// SessionProjection is the catalog view of a session: its runtime metadata,
// its optional Project/Workspace ownership and whether it can be activated.
type SessionProjection struct {
	Runtime      *Session                       `json:"runtime"`
	Organization *workspace.SessionOrganization `json:"organization"`
	Availability string                         `json:"availability"`
}

// WorkspaceID returns the owning workspace, or "" for a directory session.
func (p SessionProjection) WorkspaceID() string {
	if p.Organization == nil || p.Organization.WorkspaceID == nil {
		return ""
	}
	return *p.Organization.WorkspaceID
}

type Workspace struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	RootPath  string `json:"root_path"`
	Status    string `json:"status"`
}

type CreateSessionRequest struct {
	WorkingDirectory string `json:"working_directory,omitempty"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	Profile          string `json:"profile,omitempty"`
	ApprovalPolicy   string `json:"approval_policy,omitempty"`
}

type Image struct {
	MimeType   string `json:"mime_type"`
	DataBase64 string `json:"data_base64"`
}

type MessageRequest struct {
	Text            string   `json:"text,omitempty"`
	Images          []Image  `json:"images,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	ClientMessageID string   `json:"client_message_id,omitempty"`
}

// Command is the accepted durable command returned by message, steering and
// pause requests. Type is "message" when the input started a turn and
// "steering" when the daemon attached it to the open turn TurnID.
type Command struct {
	ID        string    `json:"id"`
	Sequence  uint64    `json:"sequence"`
	Type      string    `json:"type"`
	Text      string    `json:"text,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ApprovalDecision struct {
	ToolCallID string `json:"tool_call_id"`
	Approved   bool   `json:"approved"`
	Reason     string `json:"reason,omitempty"`
}

type ItemsPage struct {
	Items      []session.DisplayItem `json:"items"`
	NextCursor string                `json:"next_cursor"`
	PrevCursor string                `json:"prev_cursor"`
	HasMore    bool                  `json:"has_more"`
}

type Profile struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

type Profiles struct {
	DefaultProfile string    `json:"default_profile"`
	Profiles       []Profile `json:"profiles"`
}

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Enabled     bool   `json:"enabled"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var health Health
	err := c.do(ctx, http.MethodGet, "/health", nil, &health)
	return health, err
}

func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (Session, error) {
	var created Session
	err := c.do(ctx, http.MethodPost, "/sessions", req, &created)
	return created, err
}

func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var found Session
	err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id), nil, &found)
	return found, err
}

func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var listed struct {
		Sessions []Session `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, "/sessions", nil, &listed)
	return listed.Sessions, err
}

func (c *Client) CatalogSession(ctx context.Context, id string) (SessionProjection, error) {
	var projection SessionProjection
	err := c.do(ctx, http.MethodGet, "/catalog/sessions/"+url.PathEscape(id), nil, &projection)
	return projection, err
}

func (c *Client) CatalogSessions(ctx context.Context) ([]SessionProjection, error) {
	var listed struct {
		Sessions []SessionProjection `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, "/catalog/sessions", nil, &listed)
	return listed.Sessions, err
}

func (c *Client) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	var found Workspace
	err := c.do(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(id), nil, &found)
	return found, err
}

func (c *Client) StartSession(ctx context.Context, id string) (Session, error) {
	var started Session
	err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/start", nil, &started)
	return started, err
}

// Items returns one page of durable display items. after=0 means "most recent
// page" when before is also 0.
func (c *Client) Items(ctx context.Context, id string, after uint64, before uint64, limit int) (ItemsPage, error) {
	query := url.Values{}
	if after > 0 {
		query.Set("after", strconv.FormatUint(after, 10))
	}
	if before > 0 {
		query.Set("before", strconv.FormatUint(before, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/sessions/" + url.PathEscape(id) + "/items"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page ItemsPage
	err := c.do(ctx, http.MethodGet, path, nil, &page)
	return page, err
}

// AllItems returns the complete durable display log in sequence order. The
// items API addresses pages relative to existing sequences, so it pages
// backwards from the most recent page.
func (c *Client) AllItems(ctx context.Context, id string) ([]session.DisplayItem, error) {
	page, err := c.Items(ctx, id, 0, 0, 200)
	if err != nil {
		return nil, err
	}
	pages := [][]session.DisplayItem{page.Items}
	for page.PrevCursor != "" && len(page.Items) > 0 {
		page, err = c.Items(ctx, id, 0, page.Items[0].Sequence, 200)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page.Items)
	}
	var all []session.DisplayItem
	for index := len(pages) - 1; index >= 0; index-- {
		all = append(all, pages[index]...)
	}
	return all, nil
}

func (c *Client) SubmitMessage(ctx context.Context, id string, req MessageRequest) (Command, error) {
	var command Command
	err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/messages", req, &command)
	return command, err
}

// Pause asks the worker to interrupt the open turn. turnID may be empty to
// target the currently open turn.
func (c *Client) Pause(ctx context.Context, id string, turnID string) (Command, error) {
	body := map[string]string{}
	if turnID != "" {
		body["turn_id"] = turnID
	}
	var command Command
	err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/pause", body, &command)
	return command, err
}

// DecideApproval submits one decision per pending call. With stopTurn every
// decision must deny, and the service interrupts the turn instead of letting
// the agent continue past the denial.
func (c *Client) DecideApproval(ctx context.Context, id string, approvalID string, decisions []ApprovalDecision, stopTurn bool) error {
	body := struct {
		Decisions []ApprovalDecision `json:"decisions"`
		StopTurn  bool               `json:"stop_turn,omitempty"`
	}{Decisions: decisions, StopTurn: stopTurn}
	return c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/approvals/"+url.PathEscape(approvalID), body, nil)
}

func (c *Client) SetApprovalPolicy(ctx context.Context, id string, policy string) error {
	return c.do(ctx, http.MethodPut, "/sessions/"+url.PathEscape(id)+"/approval-policy", map[string]string{"approval_policy": policy}, nil)
}

func (c *Client) SetProfile(ctx context.Context, id string, profile string) error {
	return c.do(ctx, http.MethodPut, "/sessions/"+url.PathEscape(id)+"/profile", map[string]string{"profile": profile}, nil)
}

// Profiles lists the effective profiles for workingDirectory.
func (c *Client) Profiles(ctx context.Context, workingDirectory string) (Profiles, error) {
	path := "/config/profiles"
	if workingDirectory != "" {
		path += "?working_directory=" + url.QueryEscape(workingDirectory)
	}
	var profiles Profiles
	err := c.do(ctx, http.MethodGet, path, nil, &profiles)
	return profiles, err
}

func (c *Client) Skills(ctx context.Context, sessionID string, forceReload bool) ([]Skill, error) {
	query := url.Values{}
	if sessionID != "" {
		query.Set("session_id", sessionID)
	}
	if forceReload {
		query.Set("force_reload", "true")
	}
	path := "/skills"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var listed struct {
		Skills []Skill `json:"skills"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &listed)
	return listed.Skills, err
}

func (c *Client) do(ctx context.Context, method string, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	envelope := struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if envelope.Code != "ok" {
		return &APIError{Status: resp.StatusCode, Code: envelope.Code}
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode response data: %w", err)
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func decodeAPIError(status int, data []byte) error {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Code == "" {
		return &APIError{Status: status, Code: http.StatusText(status), Message: strings.TrimSpace(string(data))}
	}
	return &APIError{Status: status, Code: body.Code, Message: body.Message}
}

type Project struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (c *Client) GetProject(ctx context.Context, id string) (Project, error) {
	var found Project
	err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(id), nil, &found)
	return found, err
}

// ArchiveSession hides a session from active lists; its history is kept.
func (c *Client) ArchiveSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/archive", nil, nil)
}
