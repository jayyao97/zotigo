**English** | [简体中文](zh-CN/configuration.md)

# Configuration

This page is the reference for `config.yaml`, model profiles, approval and
safety settings, skills, hooks and the request limits applied to native
providers. The [README](../README.md#configure-a-model) has the minimal setup.

## Files and precedence

| Source | Path | Notes |
| --- | --- | --- |
| Global config | `~/.zotigo/config.yaml` | Created as a template the first time `zotigo` runs. The template has no profiles; the CLI exits after creating it so you can add one. |
| Project config | `<cwd>/zotigo.yaml`, or `<cwd>/.zotigo/config.yaml` when the first file is absent | Merged over the global config for sessions started in that directory. |
| Environment | `ZOTIGO_<KEY>`, for example `ZOTIGO_DEFAULT_PROFILE` or `ZOTIGO_PROFILES.NAME.MODEL` | Applied last. |

Zotigo has no built-in model profiles. The available profiles are exactly the
ones in the global and project files. Desktop and Web can also edit the global
profiles in **Settings → Models**; that writes the same file on the daemon host.

Most settings are read when a session worker starts, so start a new session
after editing the file. Existing workers keep the configuration they started
with.

## Model profiles

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
        profile: gpt-5.6-luna        # cheap model used to review risky tool calls
  gpt-5.6-luna:
    provider: openai
    model: gpt-5.6-luna
    api_key: sk-...
    thinking_level: low
    context_window: 1050000
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
  deepseek:
    provider: deepseek
    model: deepseek-v4-flash
    api_key: ...                     # base URL and Anthropic-compatible driver are built in
```

The model IDs are examples. Use the IDs from the provider's own model list, such
as the [OpenAI model guide](https://developers.openai.com/api/docs/guides/model-guidance?model=gpt-5.6)
and the [Claude model overview](https://platform.claude.com/docs/en/about-claude/models/overview).

| Field | Meaning |
| --- | --- |
| `provider` | `openai`, `anthropic`, `gemini` or `deepseek`. OpenAI models whose IDs start with `gpt-5`, `o1`, `o3` or `o4` use the Responses API; other OpenAI models use Chat Completions. DeepSeek uses the Anthropic driver with base URL `https://api.deepseek.com/anthropic`. |
| `model` | Provider model ID. |
| `api_key` | Provider API key. |
| `base_url` | Optional endpoint override, for proxies and compatible servers. |
| `thinking_level` | Empty (provider default), `low`, `medium`, `high`, or `disabled` (Anthropic only). |
| `max_output_tokens` | Output cap. Defaults to `120000`; must be positive. Maps to Anthropic `max_tokens`, OpenAI Responses `max_output_tokens` and Gemini `MaxOutputTokens`. `thinking_level` never changes an explicit value. Limits a provider does not support fail when the profile is prepared. |
| `context_window` | Context size in tokens, used for the context indicator and compaction. Defaults to `200000`; set it for any model where that is wrong. |
| `safety.classifier` | See [Safety classifier](#safety-classifier). |
| `params` | Provider-specific options, for example `params.max_request_bytes`. |

For compatibility, Gemini profiles still read the old `params.max_tokens` when
`max_output_tokens` is absent. New configuration should use the top-level field,
which takes precedence.

Codex sessions do not use these profiles. They use Codex's own login and model
settings, so none of the defaults on this page apply to them.

## Approval modes

Every session has one of three approval policies. New daemon sessions start in
Auto; ACP sessions start in Manual.

| Mode | API value | Behavior |
| --- | --- | --- |
| Manual | `manual` | Every tool call not classified as safe asks for approval. |
| Auto | `auto` | Each call is classified (`safe`, `low`, `medium`, `high`, `blocked`). Calls at or above `review_threshold` are sent to the safety classifier, which approves them or asks you. |
| Full access | `bypass_permissions` | Runs every registered tool without classification, snapshots or approval, including calls a tool would otherwise block. |

In the TUI, Shift+Tab cycles Manual → Auto → Full access while the session is
idle. `zotigo --dangerously-skip-permissions` starts a session in Full access.
The policy also applies to resumed pending actions and spawned subagents. Use
Full access only where unrestricted tool execution is acceptable.

### Safety classifier

```yaml
safety:
  classifier:
    enabled: true                    # false skips the classifier entirely
    review_threshold: medium         # safe | low | medium | high | off
    profile: gpt-5.6-luna            # profile used by the classifier; defaults to the active profile
    timeout_ms: 20000                # per attempt; small reasoning models often take 10-20 s
    max_recent_actions: 6            # prior agent actions sent as context
    capture_raw_audit_context: false # store raw tool arguments in the audit log
    max_audit_context_chars: 1200    # per-entry cap when raw capture is on
```

The block is optional and the values above are the defaults. It is set per
profile, so each main model can choose its own reviewer.

## Web search

`web_search` is registered only when a search backend has a key. Tavily and
DeepSeek are supported; see [Web search backends](web-tools.md). `web_fetch` is
always available.

## Skills

Skills are folders containing a `SKILL.md`. Zotigo loads them from:

- built-in skills shipped with Zotigo;
- `~/.agents/skills/` (user);
- `<project>/.agents/skills/` (workspace).

Only an index (name, description, path) goes into the system prompt. The model
reads the full `SKILL.md` when you mention `$skill-name` or the task clearly
matches the description. In the TUI, `/skills` lists skills, `/skills --reload`
rescans them, and `/<skill-name>` invokes one. Older `~/.zotigo/skills` and
`~/.claude/skills` directories are not read.

## Project instructions

`AGENTS.md` and `CLAUDE.md` in the session's working directory are added to the
system prompt together with the working directory, platform, date, time zone and
the session or Workspace identity.

## Hooks

Hooks run local commands at session, turn and tool events. They are configured
in `~/.zotigo/hooks.yaml`:

```yaml
version: 1
hooks:
  PreToolUse:
    - command: /usr/local/bin/check-tool
      args: ["--strict"]
      matchers: ["shell"]            # tool names; "*" matches all
      timeout_ms: 5000
  PostToolUse:
    - command: /usr/local/bin/log-tool
```

| Field | Meaning |
| --- | --- |
| Event key | `SessionStart`, `SessionEnd`, `TurnStart`, `UserPromptSubmit`, `TurnEnd`, `PreToolUse`, `PostToolUse` |
| `command`, `args` | Program to run; required. The event is passed as JSON on stdin. |
| `agents` | Optional list of agent names the handler applies to. |
| `matchers` | Optional values to match: the tool name for tool events, the session source or result for session events. Not allowed for `UserPromptSubmit`. |
| `timeout_ms` | Defaults: 5 s for `PreToolUse`, 10 s for `PostToolUse`, 1.5 s for `SessionEnd` and `TurnEnd`, 3 s otherwise. |
| `async` | Run without waiting. Defaults to true for `PostToolUse`; not allowed for `PreToolUse`. |

A `PreToolUse` hook can block a call by exiting with code 2 (the first paragraph
of stderr becomes the reason) or by printing one JSON object such as
`{"decision": "deny", "reason": "..."}`. `"allow"` lets the call continue through
the normal approval policy. Invalid handlers are skipped and logged; they do not
disable the other handlers.

## Request size limits for native providers

These limits apply to the native `openai`, `anthropic`, `gemini` and `deepseek`
providers, not to Codex sessions.

Each request has a 24 MiB serialized-body budget, separate from the token
context window. Override it for an endpoint with `params.max_request_bytes`
(65536 to 67108864 bytes).

Images from user messages and tool results are prepared on a request-only copy.
Previews are at most 2048 pixels per side and 1 MiB each. Small PNGs stay
lossless; larger previews may become JPEG. Prepared images are cached, up to
32 MiB per provider instance. When older images exceed half of the body budget,
they are replaced with text placeholders that keep the message and tool-result
structure and any source path. Images from the current user turn are kept.
Persisted history and the original files are not changed, so this also applies
when resuming older sessions.

The final request body, including tools, system text and base64 data, is checked
before sending. An HTTP 413 triggers at most two retries with smaller images, and
only before any response has been delivered. If the current input still does not
fit, the request fails with a message asking you to reduce it; history is not
erased. Remote image URLs and file IDs stay as references and are not downloaded.

## Subagent reports

`spawn` runs a child agent (30 minutes by default, up to 60). Its final report
has its own budget of about 16,000 tokens (64,000 bytes) instead of the normal
2,000-token tool-output budget, and appears before the execution trace. The full
report is also saved to `artifacts/subagent-reports/<content-hash>.md` in the
owning Workspace, or in the parent's working directory for sessions outside a
Workspace. The result includes the path, so a truncated report can be read with
`read_file` using `byte_offset` (4 KiB per read). These files are kept until you
delete them. If saving fails, the result says so and still returns the report.

## Observability

Langfuse tracing is off unless enabled with both keys:

```yaml
observability:
  langfuse:
    enabled: true
    host: https://cloud.langfuse.com   # default
    public_key: pk-...
    secret_key: sk-...
```

Set `ZOTIGO_DEBUG=1` before starting a process for verbose agent and provider
logs. For an installed daemon, add `export ZOTIGO_DEBUG=1` to
`PREFIX/config/daemon.env` and restart it. Diagnostic logs are described in
[Installation](installation.md#local-diagnostic-logs).
