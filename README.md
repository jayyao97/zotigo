<p align="center">
  <img src="docs/brand/zotigo-app.png" alt="Zotigo" width="112" height="112" />
</p>

<h1 align="center">Zotigo</h1>

<p align="center">
  <strong>English</strong> | <a href="README.zh-CN.md">简体中文</a>
</p>

<p align="center">
  <strong>zotigo</strong>: agent, daemon and terminal client
  ·
  <a href="https://github.com/jayyao97/zotigo-ui">zotigo-ui</a>: Desktop, Web and iOS apps
</p>

Zotigo is a coding agent written in Go. A local session service, `zotigod`,
runs agent turns. The terminal UI, editor integrations (ACP), Desktop, Web and
IM channels all connect to it, so a session started in one place can be watched,
steered or resumed from another, and keeps running when a client closes.

This repository contains the agent, the daemon and the terminal clients. The
Desktop, Web and iOS clients live in
[jayyao97/zotigo-ui](https://github.com/jayyao97/zotigo-ui).

<p align="center">
  <img src="docs/images/desktop-conversation.png" alt="Zotigo Desktop on macOS: the sidebar lists projects, Workspaces and sessions; the conversation shows a plan, a code block, a Mermaid diagram and a table" width="820" />
</p>

<p align="center"><sub>Zotigo Desktop from <a href="https://github.com/jayyao97/zotigo-ui">zotigo-ui</a>, running a session on zotigod. Screenshot uses invented demo data.</sub></p>

## Which repository do I install from?

| You want | Install from | What gets installed |
| --- | --- | --- |
| Desktop app or Web UI | [zotigo-ui](https://github.com/jayyao97/zotigo-ui#install) | The UI **and** a matching `zotigod`. The `zotigo` terminal command is not included. |
| Terminal (TUI) | This repository | `zotigod` (`--component daemon`) and `zotigo` (`--component cli`), installed by two runs of the installer. |
| Both | Both installers, with the same `--prefix` (the default) | One shared daemon used by every client. |

## Features

- Model providers: OpenAI, Anthropic, Google Gemini and DeepSeek, switchable per
  session. Sessions can also run on the Codex app-server with your Codex login.
- Durable sessions: turns run in the daemon, survive client exits and can be
  resumed, forked, steered while running, or queued.
- Tools: file read, write and edit, shell, grep and glob, LSP, image viewing,
  web search (Tavily or DeepSeek) and fetch, subagents, and session tools that
  let an agent read, message or create other sessions.
- Approval modes: Manual, Auto (risky calls reviewed by an LLM safety
  classifier) and Full access.
- Skills from `~/.agents/skills` and `<project>/.agents/skills`, read on demand.
- Projects and Workspaces that group source repositories, notes and artifacts,
  with sessions bound to them.
- Channels: connect a Feishu/Lark bot so a group chat or topic drives a session.
- Hooks for session, turn and tool events.
- In-app updates for installed daemons and clients, on a stable or development
  channel.

## Install

Linux and macOS are supported. You need Git, curl, tar and `sha256sum` or
`shasum`. If a suitable Go is not already installed, the installer downloads a
verified one into the install prefix. Nothing is installed system-wide and no
`sudo` is needed.

```sh
# Session service (registers a systemd user service or a macOS LaunchAgent)
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh |
  sh -s -- --component daemon

# Terminal client
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh | sh
```

Then add `~/.local/share/zotigo/bin` to your `PATH`.

By default the installer builds the newest stable `vX.Y.Z` tag. Development
builds are tagged `vX.Y.Z-dev.N` and are installed with `--version`, for example
`sh -s -- --version v0.0.4-dev.45`. Other options are `--prefix`, `--source`
(build a local checkout) and `--no-service`.

On macOS, building the daemon requires a persistent code-signing identity so
that privacy permissions survive upgrades. [Installation](docs/installation.md)
covers this, along with service commands, file layout, upgrades, logs and
uninstalling.

## Configure a model

The first run of `zotigo` creates `~/.zotigo/config.yaml` and exits. Add at
least one profile and set `default_profile`:

```yaml
default_profile: sonnet
profiles:
  sonnet:
    provider: anthropic        # openai | anthropic | gemini | deepseek
    model: claude-sonnet-5
    api_key: sk-ant-...
    thinking_level: medium     # optional: low | medium | high
    context_window: 1000000    # optional; defaults to 200000
```

A `zotigo.yaml` (or `.zotigo/config.yaml`) in a project directory is merged over
the global file. With Desktop or Web you can manage the same profiles in
**Settings → Models** instead.

[Configuration](docs/configuration.md) covers every profile field, the safety
classifier, skills, hooks, request size limits and observability. Search
backends are described in [Web search](docs/web-tools.md).

## Use the terminal client

```sh
zotigo                          # new session in the current directory
zotigo -r                       # resume a session from this directory
zotigo --resume-all             # pick from sessions in every directory
zotigo --workspace <id>         # bind the new session to a Workspace
zotigo --dangerously-skip-permissions   # start in Full access
```

If no daemon is listening on `127.0.0.1:8766`, `zotigo` starts the `zotigod`
found next to its own executable, on `PATH`, or at `ZOTIGOD_BINARY`. If none is
found it exits with an error; it never silently runs sessions in-process.

| Key or command | Action |
| --- | --- |
| Enter while a turn runs | Add the message to the running turn (steering) |
| Esc | Interrupt the running turn; exit when idle |
| Ctrl+C while a turn runs | Choose: stop and exit, keep it running in the background, or cancel |
| Shift+Tab (idle) | Cycle the approval mode: Manual → Auto → Full access |
| `/model` | List or switch profiles |
| `/skills`, `/<skill-name>` | List skills, or invoke one |
| `/clear` | Start a new session in the same directory |
| `/compress` | Compact the conversation context |
| `/cost`, `/stats` | Token usage and context estimate |
| `/help` | All commands |

Codex sessions appear in the resume list but must be opened in Desktop or Web.
See [TUI and ACP clients](docs/terminal-and-acp-clients.md) for the details.

### Editors (ACP)

`zotigo-acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com)
over stdio, for editors such as Zed. The installer does not build it, so build
it from a checkout:

```sh
go build -o ~/.local/share/zotigo/bin/zotigo-acp ./cmd/acp
```

ACP sessions start in Manual mode. The editor's mode picker switches between
Manual, Auto and Full access.

## The daemon

`zotigod` listens on `127.0.0.1:8766`. The installer generates a bearer token in
`PREFIX/config/daemon.token`, so authentication is on even for loopback. The
listen address and other settings live in `PREFIX/config/daemon.env`. A
non-loopback address requires a token file.

```sh
systemctl --user status zotigo-daemon.service        # Linux
launchctl print gui/$(id -u)/com.zotigo.daemon       # macOS
```

Sessions, the project catalog and channel settings are stored in `~/.zotigo`,
and diagnostic logs in `~/.zotigo/logs/`. The HTTP API is documented in
[zotigod API](docs/zotigod-api.md), and updates in
[Application updates](docs/application-updates.md).

Codex sessions need a `codex` executable visible to the daemon. It checks
`ZOTIGO_CODEX_BINARY`, then `codex` on the service's `PATH`, then
`~/.local/bin/codex` and, on macOS, the ChatGPT app bundle. See
[Installation](docs/installation.md#codex-discovery-in-background-services).

## Build from source

Requires Go 1.25 or newer (see `go.mod`).

```sh
make build                      # ./build/zotigo
make check                      # layering, formatting, vet, lint, race tests
make test-e2e                   # provider end-to-end tests; see E2E_TESTING.md

# Run the TUI against an isolated in-process service
go run ./cmd/zotigo --no-daemon --state-dir /tmp/zotigo-dev
```

`--no-daemon` requires a `--state-dir` other than `~/.zotigo`. Its sessions stop
when the process exits, and Channels and Codex are unavailable. To run a local
build as a service, use `sh install.sh --source "$PWD" --component daemon`.

## Architecture

```
 zotigo (TUI)   zotigo-acp   Desktop / Web / iOS   Feishu
      └─────────────┴──── HTTP + SSE ────┴───────────┘
                           │
                 zotigod (internal/zotigod)
          sessions, catalog, approvals, queue, channels
                           │  one worker process per live session
            ┌──────────────┴──────────────┐
      native worker (core/agent)     Codex worker (codex app-server)
```

| Path | Contents |
| --- | --- |
| `cmd/` | Entrypoints: `zotigo`, `zotigod`, `acp`, `zotigo-update` |
| `cli/` | TUI (`tui/`) and slash commands (`commands/`) |
| `internal/zotigod/` | Daemon, workers (`worker/`, `codexworker/`), HTTP API |
| `internal/zotigodclient/` | Go API client, daemon discovery and start |
| `internal/` (other) | `wiring/` agent assembly, `cliapp/` and `acpserver/` clients, `codexapp/` Codex bridge, `channels/` Feishu, `hooks/`, `update/`, `runtime/`, `diagnostics/` |
| `core/` | Embeddable agent library: `agent/`, `providers/`, `tools/`, `compaction/`, `skills/`, `subagent/`, `session/`, `workspace/`, `config/`, `acp/` and more |
| `scripts/` | Installer helpers, toolchain manifest, update scripts |
| `tests/e2e/` | Provider end-to-end tests (build tag `e2e`) |

`core/` does not depend on the service and can be embedded on its own.
[Architecture](docs/architecture.md) explains the process model, the package
layers and the dependency rules CI enforces.

## Documentation

| Document | Contents |
| --- | --- |
| [Installation](docs/installation.md) ([中文](docs/zh-CN/installation.md)) | Installer options, services, file layout, upgrades, macOS signing, logs |
| [Configuration](docs/configuration.md) ([中文](docs/zh-CN/configuration.md)) | Profiles, approval modes, classifier, skills, hooks, limits |
| [Application updates](docs/application-updates.md) ([中文](docs/zh-CN/application-updates.md)) | Update channels, release rules, recovery |
| [Web search](docs/web-tools.md) | Tavily and DeepSeek search backends |
| [TUI and ACP clients](docs/terminal-and-acp-clients.md) | Deployment modes, stop and resume semantics, ACP mapping |
| [zotigod API](docs/zotigod-api.md) | HTTP and SSE contract, Channels, Workspaces |
| [Session tools](docs/session-tools-time-and-fork.md) | Cross-session tools, time context, fork |
| [Architecture](docs/architecture.md) | Process model and package layers |
| [Extension points](docs/extension-points.md) | `agent.Middleware` and `runner.Listeners` |
| [Catalog migrations](docs/catalog-migrations.md) | Schema change workflow |

User guides are available in English and Chinese. Developer references are in
English only.

## Contributing

Read [AGENTS.md](AGENTS.md) for commands, conventions and layering rules, and run
`make check` before opening a pull request. When you change `README.md` or a
translated guide, update the Chinese version in the same change.

## License

[Apache-2.0](LICENSE)
