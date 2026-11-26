[English](../configuration.md) | **简体中文**

# 配置

本文是 `config.yaml`、模型 profile、审批与安全设置、Skills、Hooks，以及原生提供商请求限制的参考文档。
最小配置见 [README](../../README.zh-CN.md#配置模型)。

## 配置文件与优先级

| 来源 | 路径 | 说明 |
| --- | --- | --- |
| 全局配置 | `~/.zotigo/config.yaml` | 首次运行 `zotigo` 时以模板形式创建。模板中没有 profile；CLI 创建模板后会退出，方便你添加 profile。 |
| 项目配置 | `<cwd>/zotigo.yaml`；若不存在，则为 `<cwd>/.zotigo/config.yaml` | 对在该目录中启动的会话，覆盖合并到全局配置之上。 |
| 环境变量 | `ZOTIGO_<KEY>`，例如 `ZOTIGO_DEFAULT_PROFILE` 或 `ZOTIGO_PROFILES.NAME.MODEL` | 最后应用。 |

Zotigo 没有内置的模型 profile，可用的 profile 就是全局和项目配置文件中定义的那些。
Desktop 和 Web 也可以在 **设置 → 模型配置**（**Settings → Models**）中编辑全局 profile，
写入的是 daemon 所在主机上的同一个文件。

大多数设置在会话 worker 启动时读取，因此修改文件后请新建会话。已有的 worker
会继续使用启动时的配置。

## 模型 profile

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
        profile: gpt-5.6-luna        # 用于审核高风险工具调用的低成本模型
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
    api_key: ...                     # base URL 和 Anthropic 兼容驱动已内置
```

这些模型 ID 只是示例。请使用提供商自己的模型列表中的 ID，例如
[OpenAI 模型指南](https://developers.openai.com/api/docs/guides/model-guidance?model=gpt-5.6)
和 [Claude 模型概览](https://platform.claude.com/docs/en/about-claude/models/overview)。

| 字段 | 含义 |
| --- | --- |
| `provider` | `openai`、`anthropic`、`gemini` 或 `deepseek`。ID 以 `gpt-5`、`o1`、`o3` 或 `o4` 开头的 OpenAI 模型使用 Responses API；其他 OpenAI 模型使用 Chat Completions。DeepSeek 使用 Anthropic 驱动，base URL 为 `https://api.deepseek.com/anthropic`。 |
| `model` | 提供商的模型 ID。 |
| `api_key` | 提供商 API key。 |
| `base_url` | 可选的端点覆盖，用于代理和兼容服务。 |
| `thinking_level` | 留空（使用提供商默认值）、`low`、`medium`、`high`，或 `disabled`（仅 Anthropic）。 |
| `max_output_tokens` | 输出上限。默认 `120000`，必须为正数。对应 Anthropic 的 `max_tokens`、OpenAI Responses 的 `max_output_tokens` 和 Gemini 的 `MaxOutputTokens`。`thinking_level` 不会改变显式设置的值。如果提供商不支持该上限，会在准备 profile 时报错。 |
| `context_window` | 以 token 计的上下文大小，用于上下文指示和压缩。默认 `200000`；对于这个值不正确的模型，请显式设置。 |
| `safety.classifier` | 见 [安全分类器](#安全分类器)。 |
| `params` | 提供商特定的选项，例如 `params.max_request_bytes`。 |

为了兼容，当 `max_output_tokens` 未设置时，Gemini profile 仍会读取旧的
`params.max_tokens`。新配置应使用顶层字段，它的优先级更高。

Codex 会话不使用这些 profile。它们使用 Codex 自己的登录和模型设置，
因此本页的默认值都不适用于 Codex 会话。

## 审批模式

每个会话使用三种审批策略之一。新的 daemon 会话以自动模式启动；ACP 会话以手动模式启动。

| 模式 | API 值 | 行为 |
| --- | --- | --- |
| 手动（Manual） | `manual` | 所有未被分类为安全的工具调用都需要审批。 |
| 自动（Auto） | `auto` | 每次调用都会被分级（`safe`、`low`、`medium`、`high`、`blocked`）。达到或超过 `review_threshold` 的调用会交给安全分类器，由它批准或询问你。 |
| 完全访问（Full access） | `bypass_permissions` | 运行所有已注册的工具，不做分级、快照或审批，包括工具本来会拦截的调用。 |

在 TUI 中，会话空闲时按 Shift+Tab 可循环切换 手动 → 自动 → 完全访问。
`zotigo --dangerously-skip-permissions` 会以完全访问模式启动会话。
该策略同样适用于恢复的待处理操作和派生的子 Agent。只在可以接受不受限制地执行工具的环境中使用完全访问。

### 安全分类器

```yaml
safety:
  classifier:
    enabled: true                    # 设为 false 则完全跳过分类器
    review_threshold: medium         # safe | low | medium | high | off
    profile: gpt-5.6-luna            # 分类器使用的 profile；默认为当前 profile
    timeout_ms: 20000                # 每次尝试的超时；小型推理模型通常需要 10-20 秒
    max_recent_actions: 6            # 作为上下文发送的此前 Agent 操作数量
    capture_raw_audit_context: false # 在审计日志中保存原始工具参数
    max_audit_context_chars: 1200    # 开启原始记录时每条的字符上限
```

这个配置块是可选的，上面的值即为默认值。它按 profile 设置，因此每个主模型都可以选择自己的审核模型。

## 网页搜索

只有当某个搜索后端配置了 key 时才会注册 `web_search`。支持 Tavily 和 DeepSeek；
见 [网页搜索后端](../web-tools.md)（英文）。`web_fetch` 始终可用。

## Skills

Skill 是包含 `SKILL.md` 的文件夹。Zotigo 从以下位置加载：

- Zotigo 自带的内置 Skills；
- `~/.agents/skills/`（用户级）；
- `<project>/.agents/skills/`（工作区级）。

系统提示中只放入索引（名称、描述、路径）。当你提到 `$skill-name`，或任务明显符合描述时，
模型才会读取完整的 `SKILL.md`。在 TUI 中，`/skills` 列出 Skills，`/skills --reload`
重新扫描，`/<skill-name>` 调用其中一个。旧的 `~/.zotigo/skills` 和
`~/.claude/skills` 目录不会被读取。

## 项目指令

会话工作目录中的 `AGENTS.md` 和 `CLAUDE.md` 会被加入系统提示，
同时加入的还有工作目录、平台、日期、时区以及会话或工作区的身份信息。

## Hooks

Hooks 在会话、轮次和工具事件发生时运行本地命令。配置文件为 `~/.zotigo/hooks.yaml`：

```yaml
version: 1
hooks:
  PreToolUse:
    - command: /usr/local/bin/check-tool
      args: ["--strict"]
      matchers: ["shell"]            # 工具名；"*" 匹配全部
      timeout_ms: 5000
  PostToolUse:
    - command: /usr/local/bin/log-tool
```

| 字段 | 含义 |
| --- | --- |
| 事件键 | `SessionStart`、`SessionEnd`、`TurnStart`、`UserPromptSubmit`、`TurnEnd`、`PreToolUse`、`PostToolUse` |
| `command`、`args` | 要运行的程序；必填。事件以 JSON 形式通过 stdin 传入。 |
| `agents` | 可选，handler 适用的 Agent 名称列表。 |
| `matchers` | 可选的匹配值：工具事件匹配工具名，会话事件匹配会话来源或结果。`UserPromptSubmit` 不允许设置。 |
| `timeout_ms` | 默认值：`PreToolUse` 为 5 秒，`PostToolUse` 为 10 秒，`SessionEnd` 和 `TurnEnd` 为 1.5 秒，其他为 3 秒。 |
| `async` | 不等待，直接运行。`PostToolUse` 默认为 true；`PreToolUse` 不允许设置。 |

`PreToolUse` hook 可以通过退出码 2 拦截调用（stderr 的第一段作为原因），
也可以输出一个 JSON 对象，例如 `{"decision": "deny", "reason": "..."}`。
`"allow"` 表示让调用继续走正常的审批策略。无效的 handler 会被跳过并记录日志，
不会导致其他 handler 失效。

## 原生提供商的请求大小限制

这些限制适用于原生的 `openai`、`anthropic`、`gemini` 和 `deepseek` 提供商，
不适用于 Codex 会话。

每个请求的序列化请求体预算为 24 MiB，与以 token 计的上下文窗口相互独立。
可以用 `params.max_request_bytes` 为某个端点覆盖这个值（65536 到 67108864 字节）。

用户消息和工具结果中的图片会在一份仅用于请求的副本上处理。
预览图每边最多 2048 像素、每张最多 1 MiB。较小的 PNG 保持无损；较大的预览可能转为 JPEG。
处理后的图片会被缓存，每个提供商实例最多 32 MiB。当较早的图片超过请求体预算的一半时，
它们会被替换为文本占位符，占位符保留消息和工具结果的结构以及原始路径。
当前用户轮次中的图片会被保留。持久化的历史记录和原始文件不会被修改，
因此这同样适用于恢复的旧会话。

最终请求体（包括工具、系统文本和 base64 数据）会在发送前检查。
遇到 HTTP 413 时，最多用更小的图片重试两次，且仅在尚未返回任何响应时重试。
如果当前输入仍然放不下，请求会失败，并提示你缩减输入；历史记录不会被清除。
远程图片 URL 和文件 ID 保持为引用，不会被下载。

## 子 Agent 报告

`spawn` 运行一个子 Agent（默认 30 分钟，最长 60 分钟）。它的最终报告有单独的预算，
约 16,000 token（64,000 字节），而不是普通工具输出的 2,000 token 预算，
并且显示在执行轨迹之前。完整报告还会保存到所属工作区的
`artifacts/subagent-reports/<content-hash>.md`；对于不属于工作区的会话，
保存到父会话的工作目录中。结果中包含该路径，因此被截断的报告可以用 `read_file`
配合 `byte_offset` 读取（每次 4 KiB）。这些文件会一直保留，直到你删除它们。
如果保存失败，结果中会说明，并且仍然返回报告。

## 可观测性

Langfuse 追踪默认关闭，需要启用并同时提供两个 key：

```yaml
observability:
  langfuse:
    enabled: true
    host: https://cloud.langfuse.com   # 默认值
    public_key: pk-...
    secret_key: sk-...
```

在启动进程前设置 `ZOTIGO_DEBUG=1`，可以输出详细的 Agent 和提供商日志。
对于已安装的 daemon，在 `PREFIX/config/daemon.env` 中加入 `export ZOTIGO_DEBUG=1`
然后重启。诊断日志见 [安装](installation.md#本地诊断日志)。
