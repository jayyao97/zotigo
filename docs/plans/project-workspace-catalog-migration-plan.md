# Project / Workspace Catalog 设计与交付记录

Status: implemented, PR open
Branch: `feat/project-workspace-catalog`
Repository: `zotigo`
Pull request: [#53 feat: add daemon-owned project and workspace catalog](https://github.com/jayyao97/zotigo/pull/53)
Commit: `880a7a2`

## 1. 背景与目标

Zotigo 原有 `core/session` 负责运行时会话、消息历史、agent snapshot、JSON/WAL 和
`session_index.sqlite`，但没有独立、持久的 Project、Source、Workspace 和 Session 组织模型。
如果把这些信息继续绑定到可重建的 Session index 或某个具体客户端，会导致文件系统、Git
副作用和会话展示状态缺少统一所有者。

本次改动在 Zotigo 内新增独立 catalog。完成后：

- `zotigod` 是组织数据和受管目录的唯一写入者；
- 公共客户端只调用 HTTP API，不直接操作 catalog、Git 或 Workspace 文件；
- CLI 可只读 catalog，生成跨 Project 的全局 resume 视图；
- 原有 `core/session` JSON、WAL 和 `session_index.sqlite` 保持兼容，不写入新的组织字段；
- 客户端自己的选择项、展开状态和面板布局不进入 catalog。

## 2. v1 范围

实现：

1. Project 与本地 Git/plain-folder Source 注册。
2. Workspace 目录、Git worktree、folder `direct/reference/copy` binding。
3. Workspace provision/retry、archive/unarchive、permanent delete。
4. Session 的 Workspace 归属、标题、pin、自身 archive 和 Workspace 内排序。
5. daemon 公共 HTTP API 与 CLI `--resume-all` 只读接入。

不实现：remote clone/fetch、credential 管理、自动 setup、端口分配、daemon 拉起、
浏览器/电脑控制、既有受管目录的自动接管或数据迁移。

v1 在 pin 时由 daemon 分配稳定的 `pinned_position`，但不提供公开的 pinned 重排 API；
`PUT /sessions/{id}/position` 只修改 Session 在所属 Workspace 内的顺序。

## 3. 模块所有权与并发边界

```text
core/session (runtime history)     core/workspace (catalog + filesystem/Git)
                \                    /
                         zotigod
                        /       \
             public HTTP clients   CLI read-only projection
```

- `core/workspace` 不依赖 HTTP、TUI 或具体客户端。
- HTTP handler 负责 DTO、参数校验、错误映射、操作加锁和跨 runtime/catalog 协调。
- SQL、Git、目录创建、复制、归档和删除由 `core/workspace` 负责。
- handler 使用按 Workspace ID 和 Session ID 的进程内锁，避免 lifecycle 与活跃 Session
  操作并发。
- `core/workspace.Store` 使用一个 `operationMu` 串行化 provision/archive/unarchive/delete
  的文件和 Git 副作用。v1 操作量较小，优先选择简单、保守的全局串行，而不是 keyed lock。
- catalog 写入使用 SQLite 事务；`zotigod` 是唯一 writer，CLI 仅以只读模式打开。
- 公共 API 复用 zotigod 现有 bearer auth，不增加第二套 token。

catalog 位于 `<zotigo-root>/catalog.sqlite`，不复用可重建的
`session_index.sqlite`。

## 4. 领域模型

### Project

- 字段为 `id`, `name`, `created_at`, `updated_at`。
- 受管目录固定为 `<zotigo-root>/projects/<project-id>`，名称不进入路径。
- 一个 Project 可有零到多个 Source 和 Workspace。

### Source

- Git：canonical checkout path、canonical common dir、object format、source key。
- Folder：canonical path、source key、注册时选择的默认 mode。
- 同一 Project 内 canonical path/common dir 唯一；不同 Project 可重复登记。
- 删除 Source 只删登记信息；仍被 Workspace 引用时拒绝。
- 创建 Folder Workspace binding 时必须显式传 `direct`、`reference` 或 `copy`；Source 的
  默认 mode 用于调用方生成默认选择，不替代 Workspace request 的 mode。

v1 用“最终 canonical path + Git common dir”标识 Source。每次产生副作用前重新 probe；
路径消失或指向不同仓库时返回 conflict。Git 命令会清理可能改变仓库语义的继承
`GIT_*` 环境变量，并设置 30 秒 timeout 和 256 KiB 输出上限。

### Workspace

```text
<zotigo-root>/projects/<project-id>/workspaces/<workspace-id>/
├── .zotigo-owner.json
├── code/<source-key>/
├── artifacts/
└── notes/<source-key>/
```

Workspace root 是 assigned Session 的 cwd。状态为：

- `provisioning` / `ready` / `error`
- `archiving` / `archived`
- `deleting` / `deleted`

owner marker 包含 version、Project ID、Workspace ID 和随机 nonce；catalog 保存同一 nonce。
递归删除前必须验证 ID 派生路径、canonical containment、无 symlink 祖先和 marker 完全匹配。
`deleted` 行作为 idempotency/audit tombstone，不出现在 Workspace 列表和详情中。

### Session organization

catalog 单独保存：

- `session_id`, `project_id`, `workspace_id`, `title`
- `pinned_at`, `pinned_position`, `workspace_position`
- `self_archived_at`, `workspace_archived_at`, `revision`

运行时 Session 仍由 `core/session` 保存。新建 assigned Session 时，daemon 从 Workspace
派生 cwd；已有 Session 不自动分配，显示为 `Legacy / Unassigned`。删除 Workspace 时保留
runtime Session，但删除组织关系；如果 runtime cwd 命中 deleted Workspace tombstone，
全局 resume 会将其标记为不可用，避免同路径后来被重建时误恢复。

## 5. 持久化与恢复

`core/workspace` 使用独立 SQLite，启用 WAL、foreign keys 和 busy timeout。schema v1 包含：

- `schema_meta(version)`
- `projects`
- `sources(project_id, kind, canonical_path, git_common_dir, git_object_format,
  folder_mode, source_key)`
- `workspaces(project_id, title, root_path, owner_nonce, status, error,
  archived_at, deleted_at)`
- `workspace_checkouts(workspace_id, source_id, worktree_path, base_ref,
  base_commit, branch_name, owned_head, status, error)`
- `workspace_folders(workspace_id, source_id, mode, target_path,
  direct_canonical_path, status, error)`
- `session_organization`

关键约束：

- Source、Workspace、binding 均由 opaque ID 标识；title/name 不参与路径。
- `(project_id, canonical_path)`、Git `(project_id, git_common_dir)`、
  `(workspace_id, source_id)` 唯一。
- 同一个 canonical folder path 同时只能有一个未删除的 direct binding；该约束跨 Project
  生效。
- schema migration 只修改 catalog，不改 runtime Session store 和外部 Source。

provision/retry 根据 catalog binding 与真实文件/Git 状态对账：完全匹配则采用，完全缺失
则继续，身份冲突则停在 `error`。v1 不引入 operation journal、lease、签名 cursor 或
catalog restore journal；Workspace 状态、binding 状态、owner marker、ownership ref、
进程锁和 SQLite 事务共同承担中断恢复。

## 6. Provisioning

### Git

每次 Git provisioning 都在 `Store.operationMu` 保护下执行：

1. probe Source，并由 daemon 在本地解析 base ref；调用方可选传 expected commit 做乐观
   校验，禁止隐式网络操作；
2. branch 必须合法且不能被其他 worktree 使用；在一个 `git update-ref --stdin` 事务中同时
   create branch 和 `refs/zotigo/workspaces/<workspace-id>/<source-key>` ownership ref；
3. 用参数数组执行 `git worktree add --lock --reason <zotigo-owner> ...`；
4. 将 checkout 状态和 Workspace 状态更新为 `ready`。

如果进程在创建 refs 后中断，retry 只有在 branch、ownership ref、planned base commit、
worktree path 和 owner marker 一致时继续。其余 partial state 返回 conflict，不采用无
Zotigo ownership 的同名 branch。delete 同样先验证 ownership ref 和精确 branch generation，
再用 `git update-ref --stdin` compare-and-swap 删除 local refs。

### Folder

- `direct`：在 `code/<key>` 创建指向 Source 的 symlink；相同 canonical Source 不可重复
  direct 占用。
- `reference`：复制到 `notes/<key>`，regular files 清 writable bit。
- `copy`：复制到 `code/<key>`。

copy/reference 先写入 `<target>.staging`，staging 内包含 matching binding marker；复制过程
拒绝逃逸 symlink 和 device/socket/FIFO，完成后 atomic rename。retry 只清理 marker 与当前
Workspace/Source 匹配的 staging，不覆盖未知 final path。

## 7. Archive / delete

### Archive

- preview 列出 Sessions、worktrees、dirty worktrees 和保留的 local branches；执行时重新
  检查。
- 任一 Session active/locked 或任一 worktree dirty 时拒绝。
- 对每个 checkout 执行非 force `git worktree remove <exact-path>`；保留 local branch、
  Workspace root、artifacts、notes、folder binding 和 runtime Session。
- 移除 worktree 前保存 branch 的精确 tip；unarchive 和 archived delete 都要求 branch
  仍指向该 tip，防止采用或删除外部重建的同名 branch。
- 全部完成后将 Workspace 和关联 organization 标记 archived，并清除 pin。
- unarchive 在 branch generation 仍匹配时重建 locked worktree；Session 自身 archive
  状态不变。

### Permanent delete

- 使用独立 preview；request 必须带与当前 title 完全相同的 `confirmation`。
- active/locked Session 时拒绝。
- 只对计划中且身份匹配的 worktree 使用 `git worktree remove --force`。
- 只删除 Zotigo 创建并记录的精确 local branch；branch 与 ownership ref 在同一个
  `git update-ref --stdin` 事务中按 old OID 删除，不修改 remote ref。
- 验证 owner marker 后，把 Workspace root 原子 rename 到同一 Project 下的
  `.trash-<workspace-id>`，再递归删除该目录。
- 最后删除 binding 和 Session organization，保留 Project、Source、runtime Session，
  并把 Workspace 标记为 deleted tombstone。
- 中断后只向前恢复，不尝试重建已经删除的内容。

## 8. 公共 HTTP API

API 复用现有 envelope、error 和 auth 约定。Project/Source/Workspace 路由：

```text
POST   /projects
GET    /projects
GET    /projects/{id}
POST   /projects/{id}/sources/inspect
POST   /projects/{id}/sources
DELETE /projects/{id}/sources/{source-id}

POST   /projects/{id}/workspaces
GET    /projects/{id}/workspaces?include_archived=true
GET    /workspaces/{id}
POST   /workspaces/{id}/retry
GET    /workspaces/{id}/archive-preview
POST   /workspaces/{id}/archive
POST   /workspaces/{id}/unarchive
GET    /workspaces/{id}/delete-preview
POST   /workspaces/{id}/delete
```

Session organization 路由：

```text
GET    /catalog/sessions
GET    /catalog/sessions/{id}
PUT    /sessions/{id}/title
PUT    /sessions/{id}/pinned
PUT    /sessions/{id}/position
POST   /sessions/{id}/archive
POST   /sessions/{id}/unarchive
POST   /sessions                 # optional workspace_id; cwd is server-derived
```

`GET /catalog/sessions` 支持 `project_id`、`workspace_id`、`pinned=true` 和
`include_archived=true` 过滤。每一项是 runtime 与 organization 的 projection：

```json
{
  "runtime": {},
  "organization": {},
  "availability": "ready"
}
```

`availability` 当前可能为 `ready`、`archived`、`runtime_missing`、
`workspace_not_ready`、`cwd_mismatch` 或 `cwd_unavailable`。调用方应以该字段决定 Session
能否启动，不自行从路径推导状态。

创建与 lifecycle 操作通过 opaque ID、状态机和真实文件/Git 对账保证安全重试。v1 列表
规模较小，直接返回完整结果；出现真实分页需求后再设计 cursor。

## 9. CLI 行为

- `--resume` 保持原有 cwd scope，只从 runtime Session store 列举当前目录下的 Session。
- `--resume-all` 把 runtime Session 与 catalog organization 按 ID join，并展示
  `Project / Workspace / Session title`。
- catalog-only、archived、Workspace unavailable、cwd mismatch 和 cwd unavailable 项会在
  `--resume-all` 中标记为 disabled。
- 全局列表为只读视图，不提供删除操作，避免绕过 catalog 留下悬挂 organization。
- CLI resume 后以 Session 实际 cwd 重新加载 config 和 profile，不沿用启动 CLI 时的 cwd
  配置。

## 10. 测试与交付状态

实现按以下切片完成：

1. Catalog/store/domain CRUD。
2. Project/Source/Workspace HTTP API。
3. Git/folder provisioning 与 recovery。
4. Archive/unarchive/delete。
5. Session organization 与 assigned creation。
6. CLI `--resume-all`。

2026-08-21 的本地验证结果：

- `go test ./...`：通过，覆盖 `core/workspace`、HTTP handler、Session organization 和 CLI
  catalog projection。
- `go build ./...`：通过。
- `git diff --check`：通过。
- `make check`：format 和 vet 已执行；lint 被 4 个本次 diff 之外的既有 staticcheck 告警
  阻断，位置为 `core/lsp/client.go` 和 `core/tools/builtin/lsp.go`。

重点验收场景已有自动化测试：

- catalog 不受 runtime Session index rebuild/delete 影响；
- Project/Source/Workspace 重启后可读取；
- provisioning partial state 不覆盖未知路径；
- archive 删除 clean worktree，同时保留 branch 和 Workspace root；
- delete 只删除 matching owned root/local branch，保留 Source、remote ref 和 runtime Session；
- assigned Session 使用 daemon 派生的 Workspace cwd；
- 原有 Session API、存储格式和 cwd-scoped CLI resume 保持兼容。

## 11. 回滚与已知边界

- 本 PR 尚未发布，不记录虚构的发布版本或上线状态。
- 回滚二进制不会修改已有 runtime Session；`catalog.sqlite` 和已创建的 Workspace 文件会
  保留，可由包含该 schema 的版本重新打开。
- permanent delete 是显式、不可逆操作，依赖 preview、title confirmation、active Session
  检查、owner marker 和 Git ownership ref 防止误删；代码回滚不能恢复已删除内容。
- schema v1 不自动迁移或接管既有受管目录。
- v1 不提供 pinned Session 的公开重排接口，也不提供 Project/Workspace 的展示排序协议。

## 12. 自审结论

实现保留的复杂度只服务三个风险：组织数据与 runtime store 独立、Git/文件副作用的中断
恢复，以及 archive/delete 的误删防护。当前模块边界与代码一致：`core/workspace` 拥有领域
状态和副作用，`internal/zotigod` 拥有公共协议与并发协调，CLI 只读生成全局投影。没有为
尚未出现的分页、跨进程 writer、远程 Source 或通用 operation framework 预埋抽象。
