package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

const (
	defaultSpawnTimeout = 10 * time.Minute
	maxSpawnTimeout     = 30 * time.Minute

	spawnAgentGeneral = "general-purpose"
	spawnAgentExplore = "explore"
)

// SpawnTool runs a short-lived child agent for focused research or implementation work.
type SpawnTool struct {
	cfg        config.ProfileConfig
	childTools []tools.Tool
}

// NewSpawnTool constructs a spawn tool. childTools is the tool pool the child
// may receive; spawn is intentionally not added to child agents.
func NewSpawnTool(cfg config.ProfileConfig, childTools []tools.Tool) *SpawnTool {
	copied := make([]tools.Tool, 0, len(childTools))
	for _, tool := range childTools {
		if tool == nil || tool.Name() == "spawn" {
			continue
		}
		copied = append(copied, tool)
	}
	return &SpawnTool{cfg: cfg, childTools: copied}
}

func (t *SpawnTool) Name() string { return "spawn" }

func (t *SpawnTool) Description() string {
	return `Launch a short-lived subagent to handle a focused task, then return its final report.

Use this for complex searches, multi-file analysis, independent review, or scoped implementation work whose intermediate tool output does not need to stay in your main context.

Subagents start fresh: include the goal, relevant context, file paths, constraints, what you have already learned, and the output format you want. Do not delegate vague synthesis like "based on your findings, fix it"; give a concrete task.

Set workdir when the subagent should operate in a specific directory. If workdir is outside the parent agent's current safe scope, Zotigo will ask the user to approve that scoped access before launching the subagent.

Agent types:
- general-purpose: can use the normal child tool pool, including write/edit/shell when available.
- explore: read-only codebase exploration using read_file, grep, glob, web_search, and web_fetch. It cannot edit files or run shell commands.`
}

func (t *SpawnTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Optional short stable name for the subagent, such as session-code-map",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A short 3-7 word description of the subtask",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The complete task briefing for the subagent. Include all context it needs; it does not inherit the conversation.",
			},
			"agent_type": map[string]any{
				"type":        "string",
				"enum":        []string{spawnAgentGeneral, spawnAgentExplore},
				"description": "Subagent type to run (default: general-purpose)",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Optional working directory for the subagent. Relative paths resolve against the parent agent's working directory.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Optional timeout in milliseconds (default: 600000, max: 1800000)",
				"minimum":     1,
			},
		},
		"required": []string{"description", "prompt"},
	}
}

func (t *SpawnTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	var args struct {
		AgentType string `json:"agent_type"`
		WorkDir   string `json:"workdir"`
	}
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	if strings.TrimSpace(args.WorkDir) != "" {
		if !tools.IsInSafeScope(call, []string{"workdir"}) || tools.IsSensitivePath(call, []string{"workdir"}) {
			return tools.SafetyDecision{
				Level:            tools.LevelLow,
				Reason:           "subagent workdir expands safe scope",
				RequiresApproval: true,
			}
		}
	}
	if args.AgentType == spawnAgentExplore {
		return tools.SafetyDecision{
			Level:  tools.LevelSafe,
			Reason: "read-only subagent delegation",
		}
	}
	return tools.SafetyDecision{
		Level:  tools.LevelLow,
		Reason: "subagent delegation may execute child tool calls",
	}
}

func (t *SpawnTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		AgentType   string `json:"agent_type"`
		WorkDir     string `json:"workdir"`
		TimeoutMs   int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	args.Description = strings.TrimSpace(args.Description)
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Description == "" {
		return nil, fmt.Errorf("description is required")
	}
	if args.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if args.AgentType == "" {
		args.AgentType = spawnAgentGeneral
	}
	if args.AgentType != spawnAgentGeneral && args.AgentType != spawnAgentExplore {
		return nil, fmt.Errorf("unknown agent_type %q", args.AgentType)
	}
	if args.Name == "" {
		args.Name = slugifySpawnName(args.Description)
	}

	timeout := defaultSpawnTimeout
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
		if timeout > maxSpawnTimeout {
			timeout = maxSpawnTimeout
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	childWorkDir := resolveSpawnWorkDir(exec.WorkDir(), args.WorkDir)
	info, err := exec.Stat(ctx, childWorkDir)
	if err != nil {
		return nil, fmt.Errorf("invalid subagent workdir: %w", err)
	}
	if !info.IsDir {
		return nil, fmt.Errorf("invalid subagent workdir: not a directory: %s", childWorkDir)
	}

	childTools := t.resolveTools(args.AgentType)
	if len(childTools) == 0 {
		return nil, fmt.Errorf("no tools available for subagent type %q", args.AgentType)
	}
	childExec := executor.Executor(exec)
	if filepath.Clean(childWorkDir) != filepath.Clean(exec.WorkDir()) {
		childExec = &spawnWorkDirExecutor{base: exec, workDir: childWorkDir}
	}

	child, err := agent.New(t.cfg, childExec,
		agent.WithSystemPromptBuilder(spawnPromptBuilder(args.AgentType)),
		agent.WithTools(childTools...),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create subagent: %w", err)
	}

	events, err := child.RunMessage(ctx, protocol.NewUserMessage(args.Prompt))
	if err != nil {
		return nil, fmt.Errorf("failed to run subagent: %w", err)
	}
	var finish protocol.FinishReason
	var trace []string
	eventSink, hasEventSink := agent.ToolEventSinkFromContext(ctx)
	for event := range events {
		if event.Type == protocol.EventTypeFinish {
			finish = event.FinishReason
		}
		if event.Type == protocol.EventTypeToolCallEnd && event.ToolCall != nil {
			line := "[" + args.Name + "] " + formatSpawnTraceToolCall(event.ToolCall)
			trace = append(trace, line)
			if hasEventSink {
				eventSink(protocol.Event{
					Type: protocol.EventTypeToolProgress,
					ToolResult: &protocol.ToolResult{
						ToolCallID: "subagent-progress:" + args.Name,
						ToolName:   "spawn",
						Type:       protocol.ToolResultTypeText,
						Text:       line,
					},
				})
			}
		}
		if event.Type == protocol.EventTypeError && event.Error != nil {
			return nil, fmt.Errorf("subagent error: %w", event.Error)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("subagent timed out or was cancelled: %w", err)
	}
	if finish == "need_approval" {
		return nil, fmt.Errorf("subagent paused for approval; narrow the task or use an agent type/tool set that can complete without approval")
	}

	snap := child.Snapshot()
	report := lastAssistantText(snap.History)
	if strings.TrimSpace(report) == "" {
		report = "(subagent completed without a text report)"
	}
	usage := protocol.SessionUsage(snap.History).Normalized()

	return spawnOutput{
		text:  formatSpawnResult(args.Name, args.AgentType, childWorkDir, args.Description, report, trace, countSubagentToolCalls(snap.History), usage),
		usage: usage,
	}, nil
}

type spawnOutput struct {
	text  string
	usage protocol.Usage
}

func (o spawnOutput) ToolOutputText() string {
	return o.text
}

func (o spawnOutput) ToolResultMetadata() map[string]any {
	return map[string]any{"usage": o.usage}
}

func resolveSpawnWorkDir(parentWorkDir, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return filepath.Clean(parentWorkDir)
	}
	if filepath.IsAbs(requested) {
		return filepath.Clean(requested)
	}
	return filepath.Clean(filepath.Join(parentWorkDir, requested))
}

type spawnWorkDirExecutor struct {
	base    executor.Executor
	workDir string
}

func (e *spawnWorkDirExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return e.base.ReadFile(ctx, e.resolve(path))
}

func (e *spawnWorkDirExecutor) WriteFile(ctx context.Context, path string, content []byte, perm fs.FileMode) error {
	return e.base.WriteFile(ctx, e.resolve(path), content, perm)
}

func (e *spawnWorkDirExecutor) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	return e.base.Stat(ctx, e.resolve(path))
}

func (e *spawnWorkDirExecutor) Exec(ctx context.Context, cmd string, opts executor.ExecOptions) (*executor.ExecResult, error) {
	if opts.WorkDir == "" {
		opts.WorkDir = e.workDir
	} else {
		opts.WorkDir = e.resolve(opts.WorkDir)
	}
	return e.base.Exec(ctx, cmd, opts)
}

func (e *spawnWorkDirExecutor) WorkDir() string {
	return e.workDir
}

func (e *spawnWorkDirExecutor) Platform() string {
	return e.base.Platform()
}

func (e *spawnWorkDirExecutor) Init(context.Context) error {
	return nil
}

func (e *spawnWorkDirExecutor) Close() error {
	return nil
}

func (e *spawnWorkDirExecutor) resolve(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(e.workDir, path))
}

func (t *SpawnTool) resolveTools(agentType string) []tools.Tool {
	allowed := map[string]bool{}
	if agentType == spawnAgentExplore {
		for _, name := range []string{"read_file", "grep", "glob", "web_search", "web_fetch"} {
			allowed[name] = true
		}
	}

	resolved := make([]tools.Tool, 0, len(t.childTools))
	for _, tool := range t.childTools {
		if tool == nil || tool.Name() == "spawn" {
			continue
		}
		if len(allowed) > 0 && !allowed[tool.Name()] {
			continue
		}
		resolved = append(resolved, tool)
	}
	return resolved
}

func spawnPromptBuilder(agentType string) *prompt.SystemPromptBuilder {
	base := `You are a Zotigo subagent. Complete the assigned task fully and return only a concise report for the parent agent to use.

You start with no parent conversation history. Trust only the task briefing and what you inspect with tools.
Do not ask the user questions. Do not spawn other agents. Stay inside the requested scope.
Report concrete findings, relevant file paths, and any errors or uncertainty.`

	if agentType == spawnAgentExplore {
		base += `

You are in read-only exploration mode. Do not create, edit, delete, move, copy, install, commit, or run shell commands. Use read_file, grep, glob, web_search, and web_fetch when available.`
	}

	return prompt.NewSystemPromptBuilder(
		prompt.WithStaticPrompt(base),
		prompt.WithDynamicSection("environment", func(ctx prompt.PromptContext) string {
			return fmt.Sprintf("Working directory: %s\nPlatform: %s", ctx.WorkDir, ctx.Platform)
		}),
	)
}

func lastAssistantText(history []protocol.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != protocol.RoleAssistant {
			continue
		}
		var parts []string
		for _, part := range history[i].Content {
			if part.Type == protocol.ContentTypeText && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func formatSpawnResult(name, agentType, workDir, description, report string, trace []string, toolCalls int, usage protocol.Usage) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"Subagent completed\nName: %s\nAgent type: %s\nWorkdir: %s\nDescription: %s\nTool calls: %d\nTokens: %d",
		name,
		agentType,
		workDir,
		description,
		toolCalls,
		usage.TotalTokens,
	))
	if len(trace) > 0 {
		sb.WriteString("\n\nTrace:\n")
		sb.WriteString(strings.Join(trace, "\n"))
	}
	sb.WriteString("\n\nReport:\n")
	sb.WriteString(strings.TrimSpace(report))
	return sb.String()
}

func countSubagentToolCalls(history []protocol.Message) int {
	var count int
	for _, msg := range history {
		if msg.Role != protocol.RoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			if part.Type == protocol.ContentTypeToolCall && part.ToolCall != nil {
				count++
			}
		}
	}
	return count
}

func slugifySpawnName(description string) string {
	description = strings.ToLower(strings.TrimSpace(description))
	var out strings.Builder
	lastDash := false
	for _, r := range description {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		return "subagent"
	}
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	return name
}

func formatSpawnTraceToolCall(tc *protocol.ToolCall) string {
	name := tc.Name
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || len(args) == 0 {
		return name + "()"
	}
	switch tc.Name {
	case "read_file", "write_file", "edit", "grep", "glob":
		if v, ok := args["path"]; ok {
			return fmt.Sprintf("%s(path=%s)", name, truncateSpawnTraceArg(fmt.Sprintf("%v", v)))
		}
	case "web_search":
		if v, ok := args["query"]; ok {
			return fmt.Sprintf("%s(query=%s)", name, truncateSpawnTraceArg(fmt.Sprintf("%v", v)))
		}
	case "shell":
		if v, ok := args["command"]; ok {
			return fmt.Sprintf("%s(command=%s)", name, truncateSpawnTraceArg(fmt.Sprintf("%v", v)))
		}
	}
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 120 {
			continue
		}
		return fmt.Sprintf("%s(%s=%s)", name, k, truncateSpawnTraceArg(s))
	}
	return name + "(...)"
}

func truncateSpawnTraceArg(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}
