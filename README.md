<p align="center">
  <img src="docs/brand/zotigo-app.png" alt="Zotigo" width="112" height="112" />
</p>

# Zotigo

Zotigo is a next-generation, protocol-centric CLI Agent written in Golang. It is designed to be a universal interface for AI interaction, capable of switching seamlessly between multiple LLM providers while maintaining a unified conversation context.

Positioned as a competitor to tools like Claude Code, Gemini CLI, and Codex CLI, Zotigo distinguishes itself through a rigorous decoupled architecture that separates the **Presentation Layer**, **Conversation Protocol Layer**, and **Provider Layer**.

## Key Philosophy

- **Protocol First:** All interactions are normalized into a standard internal protocol before processing. Switching models doesn't break context or tools.
- **Transparent Forwarding:** The provider layer acts as a transparent adapter, forwarding the normalized protocol to specific model APIs without leaking implementation details to the core.
- **Human-in-the-Loop:** Deeply integrated approval mechanisms for sensitive actions (file edits, shell execution), with auto-approve for read-only tools.
- **Persistence:** Every interaction is structured and persistable, allowing for long-running sessions and history resumption.

## Architecture

Zotigo is a session service plus thin clients:

- **`zotigod`** (`internal/zotigod`) owns sessions. It runs each live session's
  agent turn in its own worker process, persists a durable display log, and
  serves one HTTP + SSE API.
- **Clients** (`zotigo` TUI, `zotigo-acp`, Desktop, Web) attach to that API.
  A turn keeps running when a client exits and can be reattached from another.
- **`core/`** is the agent library the worker runs: turn loop, tools,
  provider adapters, context compaction, persistence types. It does not depend
  on the service and can be embedded on its own.

[docs/architecture.md](docs/architecture.md) describes the process model, the
package layers and why each exists, the dependency rules CI enforces, reserved
abstractions, compatibility surfaces and known debt.

## Features

- **Multi-Provider Support:** OpenAI, Anthropic, and Google Gemini — switch models on the fly.
- **Built-in Tools:** file, edit/patch, shell, grep/glob, git, LSP, web search (Tavily), web fetch.
- **Skill System:** Extensible skills (builtin, user, project) auto-injected into the prompt — no manual activation needed. Use `/skills` to list, `/<skill-name>` to invoke.
- **Safe Execution:** Tool safety policy with auto-approve for read-only tools, manual approval for mutations. Toggle auto-approve with `Shift+Tab`.
- **Context Awareness:** Project files, git history, loop detection, and automatic context compression.
- **Session Persistence:** Resume conversations across restarts.

## Directory Structure

```
zotigo/
├── cmd/                  # Entrypoints: zotigo, zotigod, acp, zotigo-update
├── cli/                  # TUI (tui/) and slash commands (commands/)
├── internal/             # The product
│   ├── zotigod/          # Session service: daemon, workers, HTTP API
│   ├── zotigodclient/    # Go client + daemon discovery/start
│   ├── wiring/           # How the product assembles an agent
│   ├── cliapp/ acpserver/  # The TUI and ACP client applications
│   └── channels/ hooks/ update/ diagnostics/ ...
├── core/                 # Embeddable agent library
│   ├── protocol/ config/ executor/ skills/ lsp/   # Foundation
│   ├── tools/ providers/ compaction/ transport/   # Capabilities
│   ├── agent/            # Turn loop (prompt/, loopguard/)
│   └── runner/ subagent/ session/ middleware/ acp/  # Built around the agent
├── tests/e2e/            # Provider end-to-end tests (build tag e2e)
└── docs/                 # architecture.md, zotigod-api.md, ...
```

## Installation

For versioned Linux/macOS source installation and daemon services, see [source installation](docs/installation.md).

```bash
go install github.com/jayyao97/zotigo/cmd/zotigo@latest
```

## Configuration

For Tavily or DeepSeek web search configuration, see [Web search backends](docs/web-tools.md).

On first run (`go run ./cmd/zotigo`), Zotigo creates a config template at `~/.zotigo/config.yaml` if it does not exist.

Zotigo does not inject model profiles into the effective configuration. The
available profiles come only from the global config and the current project's
config; project values are merged over global values.

```yaml
default_profile: gpt-5.6-sol
profiles:
  gpt-5.6-sol:
    provider: openai
    model: gpt-5.6-sol
    api_key: sk-...
    thinking_level: medium
    max_output_tokens: 65536
    context_window: 1050000
    safety:
      classifier:
        enabled: true                   # master switch; false skips the classifier entirely
        review_threshold: medium        # safe | low | medium | high | off — calls at or above this level are routed to the LLM classifier
        profile: gpt-5.6-luna           # which profile the classifier itself uses (typically a cheap/fast model, distinct from the main agent)
        timeout_ms: 20000               # per-attempt request timeout; classifier calls on small reasoning models routinely take 10–20s
        max_recent_actions: 6           # how many prior agent actions to include as context when asking the classifier to decide
        capture_raw_audit_context: false  # store raw tool args in the audit log — enable for forensics/debugging, off by default to keep logs small
        max_audit_context_chars: 1200   # per-entry cap applied only when capture_raw_audit_context is true
  gpt-5.6-terra:
    provider: openai
    model: gpt-5.6-terra
    api_key: sk-...
    thinking_level: low
    context_window: 1050000
  gpt-5.6-luna:
    provider: openai
    model: gpt-5.6-luna
    api_key: sk-...
    thinking_level: low
    context_window: 1050000
  claude-sonnet-5:
    provider: anthropic
    model: claude-sonnet-5
    api_key: sk-ant-...
    thinking_level: medium
    context_window: 1000000
  claude-opus-5:
    provider: anthropic
    model: claude-opus-5
    api_key: sk-ant-...
    thinking_level: high
    context_window: 1000000
  gemini-pro:
    provider: gemini
    model: gemini-3.0-pro-latest
    api_key: ...
    max_output_tokens: 65536
  deepseek:
    provider: deepseek
    model: deepseek-v4-flash
    api_key: ...                   # Base URL and Anthropic-compatible driver are built in
```

The `safety.classifier` block is optional. If omitted, Zotigo uses built-in defaults. If `classifier.profile` is omitted, the classifier reuses the current active profile.

The example uses the current API IDs from the
[OpenAI model guide](https://developers.openai.com/api/docs/guides/model-guidance?model=gpt-5.6)
and [Claude model overview](https://platform.claude.com/docs/en/about-claude/models/overview).
Zotigo still has no built-in model Profile; copy only the profiles you need.

Native Zotigo profiles default `max_output_tokens` to `120000`; configured
values must be positive. The setting maps to Anthropic Messages `max_tokens`,
OpenAI Responses `max_output_tokens`, and Gemini `MaxOutputTokens`.
`thinking_level` never changes an explicit output limit; unsupported provider
limits fail when the profile is prepared. For compatibility, Gemini profiles
may still read the old `params.max_tokens` value when top-level
`max_output_tokens` is absent, but new configuration should use the top-level
field and it takes precedence. Codex app-server sessions use Codex's own model
settings and are not given Zotigo's 32K default.

Native provider requests have a separate **24 MiB serialized-body budget**,
independent of the token context window. Override it for a particular endpoint
with `params.max_request_bytes` (65536–67108864 bytes). Images from user messages
and tool results are prepared on a request-only copy: previews are at most 2048
pixels per side and 1 MiB each. Small PNGs remain lossless; larger previews may
use JPEG. Prepared images are cached with a 32 MiB bound per provider instance.
Older images exceeding half of the body budget are replaced with explicit text
placeholders, retaining message/tool-result structure and any source-path text.
Images from the current user turn are retained. Persisted history and original
files are unchanged, so this also applies when resuming older sessions.

The final SDK request body (including tools, system text and base64) is checked
before transmission. HTTP 413 triggers at most two reduced-image retries, only
before any response has been delivered, with strictly smaller wire limits.
If current inputs still cannot fit, the request fails with guidance to reduce
them; it does not erase history or retry token compaction with the oversized body.
Remote image URLs/file IDs remain references and are not downloaded for resizing.

Subagent final reports use a separate prompt budget (approximately 16,000 tokens,
with a 64,000-byte bound) rather than the normal 2,000-token tool-output budget.
Reports precede execution traces. The full report is saved through the parent
executor to `.zotigo/subagent-reports/<content-hash>.md` in its working directory;
the result includes this path so a truncated report can be read in small sections
with `read_file` byte_offset=0 and the returned next-byte cursor (up to 4 KiB per read, including for single-line reports). These files persist for later reads and can be
removed when no longer needed. If saving fails, the result reports that failure
and still returns the completed report. Persisted conversation history is unchanged.

Run:
```bash
go run ./cmd/zotigo
```

To run every registered tool without classification, snapshots, or approval:

```bash
zotigo --dangerously-skip-permissions
```

This mode also bypasses tool-level blocked decisions. Use it only in an
environment where unrestricted tool execution is acceptable. The policy also
applies to resumed pending actions and spawned subagents; unavailable tools
remain blocked. Pressing Shift+Tab exits bypass mode and returns to normal
approval modes.

Note:
- `go install github.com/jayyao97/zotigo/cmd/zotigo@latest` and `go run ./cmd/zotigo` start the same interactive CLI.
