<p align="center">
  <img src="docs/brand/zotigo-app.png" alt="Zotigo" width="112" height="112" />
</p>

<h1 align="center">Zotigo</h1>

<p align="center">
  <a href="README.md">English</a> | <strong>简体中文</strong>
</p>

<p align="center">
  <strong>zotigo</strong>：Agent、daemon 和终端客户端
  ·
  <a href="https://github.com/jayyao97/zotigo-ui/blob/master/README.zh-CN.md">zotigo-ui</a>：Desktop、Web 和 iOS 应用
</p>

Zotigo 是一个用 Go 编写的编码 Agent。本地会话服务 `zotigod` 负责执行 Agent
轮次。终端界面、编辑器集成（ACP）、Desktop、Web 和 IM 渠道都连接到这个服务，
因此在一处启动的会话可以在另一处查看、引导或恢复，客户端关闭后会话也会继续运行。

本仓库包含 Agent、daemon 和终端客户端。Desktop、Web 和 iOS 客户端位于
[jayyao97/zotigo-ui](https://github.com/jayyao97/zotigo-ui)。

## 我该从哪个仓库安装？

| 你需要 | 安装来源 | 安装内容 |
| --- | --- | --- |
| Desktop 应用或 Web UI | [zotigo-ui](https://github.com/jayyao97/zotigo-ui/blob/master/README.zh-CN.md#安装) | UI **以及**配套的 `zotigod`。不包含 `zotigo` 终端命令。 |
| 终端（TUI） | 本仓库 | `zotigod`（`--component daemon`）和 `zotigo`（`--component cli`），需运行两次安装脚本。 |
| 两者都要 | 两个安装脚本，使用相同的 `--prefix`（默认值即可） | 所有客户端共用一个 daemon。 |

## 功能

- 模型提供商：OpenAI、Anthropic、Google Gemini 和 DeepSeek，可按会话切换。
  会话也可以使用你的 Codex 登录，运行在 Codex app-server 上。
- 持久会话：轮次在 daemon 中执行，客户端退出后不会中断，可以恢复、分叉、
  在运行中引导，或排队等待。
- 工具：文件读取、写入和编辑，shell，grep 和 glob，LSP，图片查看，
  网页搜索（Tavily 或 DeepSeek）和网页抓取，子 Agent，以及让 Agent
  读取其他会话、向其发送消息或创建新会话的会话工具。
- 审批模式：手动（Manual）、自动（Auto，高风险调用由 LLM 安全分类器审核）
  和完全访问（Full access）。
- Skills：来自 `~/.agents/skills` 和 `<project>/.agents/skills`，按需读取。
- 项目和工作区（Projects / Workspaces）：把源码仓库、笔记和产物组织在一起，会话可以绑定到工作区。
- 频道（Channels）：接入飞书/Lark 机器人，用群聊或话题驱动会话。
- Hooks：在会话、轮次和工具事件上触发。
- 应用内更新：已安装的 daemon 和客户端可以选择正式版或开发版更新渠道。

## 安装

支持 Linux 和 macOS。需要 Git、curl、tar，以及 `sha256sum` 或 `shasum`。
如果没有安装合适版本的 Go，安装脚本会下载一个经过校验的 Go 到安装前缀中。
不会安装任何系统级文件，也不需要 `sudo`。

```sh
# 会话服务（注册 systemd 用户服务或 macOS LaunchAgent）
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh |
  sh -s -- --component daemon

# 终端客户端
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh | sh
```

然后将 `~/.local/share/zotigo/bin` 加入 `PATH`。

安装脚本默认构建最新的正式版 `vX.Y.Z` 标签。开发版的标签为 `vX.Y.Z-dev.N`，
需通过 `--version` 安装，例如 `sh -s -- --version v0.0.4-dev.45`。其他选项有
`--prefix`、`--source`（构建本地 checkout）和 `--no-service`。

在 macOS 上构建 daemon 需要一个持久的代码签名身份，这样隐私权限才能在升级后保留。
[安装](docs/zh-CN/installation.md)一文介绍了这一点，以及服务命令、文件布局、
升级、日志和卸载。

## 配置模型

首次运行 `zotigo` 会创建 `~/.zotigo/config.yaml` 并退出。至少添加一个 profile，
并设置 `default_profile`：

```yaml
default_profile: sonnet
profiles:
  sonnet:
    provider: anthropic        # openai | anthropic | gemini | deepseek
    model: claude-sonnet-5
    api_key: sk-ant-...
    thinking_level: medium     # 可选：low | medium | high
    context_window: 1000000    # 可选；默认 200000
```

项目目录中的 `zotigo.yaml`（或 `.zotigo/config.yaml`）会覆盖合并到全局配置文件之上。
使用 Desktop 或 Web 时，也可以在 **设置 → 模型配置**（**Settings → Models**）中管理这些 profile。

[配置](docs/zh-CN/configuration.md)一文介绍了 profile 的全部字段、安全分类器、
Skills、Hooks、请求大小限制和可观测性。搜索后端见 [网页搜索](docs/web-tools.md)（英文）。

## 使用终端客户端

```sh
zotigo                          # 在当前目录新建会话
zotigo -r                       # 恢复本目录下的会话
zotigo --resume-all             # 从所有目录的会话中选择
zotigo --workspace <id>         # 将新会话绑定到某个工作区
zotigo --dangerously-skip-permissions   # 以完全访问模式启动
```

如果 `127.0.0.1:8766` 上没有 daemon 在监听，`zotigo` 会启动 `zotigod`，
依次查找自身可执行文件所在目录、`PATH` 和 `ZOTIGOD_BINARY`。如果都找不到，
它会报错退出，不会悄悄在进程内运行会话。

| 按键或命令 | 作用 |
| --- | --- |
| 轮次运行时按 Enter | 将消息追加到正在运行的轮次（引导） |
| Esc | 中断正在运行的轮次；空闲时退出 |
| 轮次运行时按 Ctrl+C | 选择：停止并退出、保持在后台运行，或取消 |
| Shift+Tab（空闲时） | 循环切换审批模式：手动 → 自动 → 完全访问 |
| `/model` | 列出或切换 profile |
| `/skills`、`/<skill-name>` | 列出 Skills，或调用其中一个 |
| `/clear` | 在同一目录开始新会话 |
| `/compress` | 压缩对话上下文 |
| `/cost`、`/stats` | Token 用量和上下文估算 |
| `/help` | 全部命令 |

Codex 会话会出现在恢复列表中，但必须在 Desktop 或 Web 中打开。
详见 [TUI 与 ACP 客户端](docs/terminal-and-acp-clients.md)（英文）。

### 编辑器（ACP）

`zotigo-acp` 通过 stdio 使用 [Agent Client Protocol](https://agentclientprotocol.com)
通信，适用于 Zed 等编辑器。安装脚本不会构建它，需要从 checkout 中构建：

```sh
go build -o ~/.local/share/zotigo/bin/zotigo-acp ./cmd/acp
```

ACP 会话以手动模式启动。可以在编辑器的模式选择器中切换手动、自动和完全访问。

## Daemon

`zotigod` 监听 `127.0.0.1:8766`。安装脚本会在 `PREFIX/config/daemon.token`
生成 bearer token，因此即使是回环地址也会启用认证。监听地址和其他设置位于
`PREFIX/config/daemon.env`。使用非回环地址时必须配置 token 文件。

```sh
systemctl --user status zotigo-daemon.service        # Linux
launchctl print gui/$(id -u)/com.zotigo.daemon       # macOS
```

会话、项目目录（catalog）和渠道设置保存在 `~/.zotigo` 中，诊断日志位于
`~/.zotigo/logs/`。HTTP API 见 [zotigod API](docs/zotigod-api.md)（英文），
更新见 [应用内更新](docs/zh-CN/application-updates.md)。

Codex 会话要求 daemon 能找到 `codex` 可执行文件。它依次检查
`ZOTIGO_CODEX_BINARY`、服务 `PATH` 中的 `codex`、`~/.local/bin/codex`，
在 macOS 上还会检查 ChatGPT 应用包。见
[安装](docs/zh-CN/installation.md#后台服务中的-codex-探测)。

## 从源码构建

需要 Go 1.25 或更高版本（见 `go.mod`）。

```sh
make build                      # ./build/zotigo
make check                      # 分层检查、格式化、vet、lint、race 测试
make test-e2e                   # 提供商端到端测试；见 E2E_TESTING.md

# 让 TUI 连接一个隔离的进程内服务
go run ./cmd/zotigo --no-daemon --state-dir /tmp/zotigo-dev
```

`--no-daemon` 要求 `--state-dir` 不是 `~/.zotigo`。它的会话会随进程退出而停止，
且无法使用频道和 Codex。要以服务方式运行本地构建，使用
`sh install.sh --source "$PWD" --component daemon`。

## 架构

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

| 路径 | 内容 |
| --- | --- |
| `cmd/` | 入口：`zotigo`、`zotigod`、`acp`、`zotigo-update` |
| `cli/` | TUI（`tui/`）和斜杠命令（`commands/`） |
| `internal/zotigod/` | daemon、worker（`worker/`、`codexworker/`）、HTTP API |
| `internal/zotigodclient/` | Go API 客户端、daemon 发现与启动 |
| `internal/`（其他） | `wiring/` Agent 组装，`cliapp/` 和 `acpserver/` 客户端，`codexapp/` Codex 桥接，`channels/` 飞书，`hooks/`，`update/`，`runtime/`，`diagnostics/` |
| `core/` | 可嵌入的 Agent 库：`agent/`、`providers/`、`tools/`、`compaction/`、`skills/`、`subagent/`、`session/`、`workspace/`、`config/`、`acp/` 等 |
| `scripts/` | 安装辅助脚本、工具链清单、更新脚本 |
| `tests/e2e/` | 提供商端到端测试（构建标签 `e2e`） |

`core/` 不依赖服务，可以单独嵌入使用。[架构](docs/architecture.md)（英文）
介绍了进程模型、包分层以及 CI 强制执行的依赖规则。

## 文档

| 文档 | 内容 |
| --- | --- |
| [安装](docs/zh-CN/installation.md) | 安装脚本选项、服务、文件布局、升级、macOS 签名、日志 |
| [配置](docs/zh-CN/configuration.md) | Profile、审批模式、分类器、Skills、Hooks、限制 |
| [应用内更新](docs/zh-CN/application-updates.md) | 更新渠道、发布规则、恢复 |
| [网页搜索](docs/web-tools.md)（英文） | Tavily 和 DeepSeek 搜索后端 |
| [TUI 与 ACP 客户端](docs/terminal-and-acp-clients.md)（英文） | 部署模式、停止与恢复语义、ACP 映射 |
| [zotigod API](docs/zotigod-api.md)（英文） | HTTP 和 SSE 约定、频道、工作区 |
| [会话工具](docs/session-tools-time-and-fork.md)（英文） | 跨会话工具、时间上下文、分叉 |
| [架构](docs/architecture.md)（英文） | 进程模型和包分层 |
| [扩展点](docs/extension-points.md)（英文） | `agent.Middleware` 和 `runner.Listeners` |
| [Catalog 迁移](docs/catalog-migrations.md)（英文） | Schema 变更流程 |

用户指南提供英文和中文版本，开发者参考文档只有英文。

## 参与贡献

请阅读 [AGENTS.md](AGENTS.md) 了解命令、约定和分层规则，并在提交 pull request
前运行 `make check`。修改 `README.md` 或已翻译的指南时，请在同一次变更中更新中文版本。

## 许可证

[Apache-2.0](LICENSE)
