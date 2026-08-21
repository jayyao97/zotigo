# Project / Workspace 迁移方案

Status: approved for implementation after self-review
Branch: `feat/project-workspace-catalog`
Repository: `zotigo`

## 1. 目标

把 `zotigo-desktop` 当前拥有的 Project、Source、Workspace 和 Session 组织关系迁到
Zotigo。迁移后：

- `zotigod` 是组织数据和受管目录的唯一写入者；
- Desktop 只调用公开 HTTP API，不直接操作 SQLite、Git 或 Workspace 文件；
- CLI 可读取 catalog 做全局 resume，但不直接修改 catalog；
- 原有 `core/session` JSON、WAL 和 `session_index.sqlite` 保持兼容，不塞入新的组织字段。

这不是 App Server/侧边栏协议。展开状态、选择项和面板布局仍属于 Desktop。

## 2. v1 范围

实现：

1. Project 与本地 Git/plain-folder Source 注册。
2. Workspace 目录、Git worktree、folder `direct/reference/copy` binding。
3. Workspace provision/retry、archive/unarchive、permanent delete。
4. Session 的 Workspace 归属、标题、pin、archive 和排序。
5. daemon HTTP API 与 CLI `--resume-all` 读取。
6. 后续由 Desktop 通过 API 做显式导入；本分支不改 dirty 的 Desktop worktree。

不实现：remote clone/fetch、credential 管理、自动 setup、端口分配、daemon 拉起、
浏览器/电脑控制、旧 Desktop Workspace 目录的自动接管或搬迁。

## 3. 所有权和依赖

```text
core/session (legacy runtime)     core/workspace (catalog + filesystem/Git)
              \                    /
                    zotigod
                       ^
                       |
                 Desktop / CLI
```

- 新包 `core/workspace` 不依赖 HTTP 或 React。
- HTTP handler 只做 DTO、校验和错误映射；SQL、Git、文件操作在 service/store 中。
- 复用 zotigod 现有 public bearer auth，不增加第二套 token。
- catalog 位于 `<zotigo-root>/catalog.sqlite`；不复用可重建的
  `session_index.sqlite`。

## 4. 领域模型

### Project

- `id`, `name`, `created_at`, `updated_at`。
- 受管目录固定为 `<zotigo-root>/projects/<project-id>`，名称不进入路径。
- Project 可有零到多个 Source 和 Workspace。

### Source

- Git：canonical checkout path、canonical common dir、object format、source key。
- Folder：canonical path、source key、默认 mode。
- 同一 Project 内 canonical path/common dir 唯一；不同 Project 可重复登记。
- 删除 Source 只删登记信息；仍被 Workspace 引用时拒绝。

v1 用“最终 canonical path + Git common dir”做身份。每次有副作用前重新 probe；
路径消失或指向不同仓库时返回 conflict。无需为本地单进程产品设计自定义跨语言
binary identity protocol。

### Workspace

```text
<zotigo-root>/projects/<project-id>/workspaces/<workspace-id>/
├── .zotigo-owner.json
├── code/<source-key>/
├── artifacts/
└── notes/<source-key>/
```

Workspace root 是 Session cwd。状态为：

- `provisioning` / `ready` / `error`
- `archiving` / `archived`
- `deleting` / `deleted`

owner marker 包含 version、Project ID、Workspace ID 和随机 nonce；catalog 保存同一
nonce。任何递归删除都必须同时满足：ID 派生路径、canonical containment、无 symlink
祖先、marker 完全匹配。`deleted` 行作为 idempotency/audit tombstone，不出现在列表和
详情中。

### Session organization

单独保存：`session_id`, `project_id`, `workspace_id`, `title`, pin/order、Session 自身
archive、Workspace archive、revision。运行时 Session 仍由 `core/session` 保存。

- 新建 assigned Session 时 daemon 从 Workspace 派生 cwd。
- 已有 Session 不自动分配，显示为 Legacy/Unassigned。
- 删除 Workspace 时保留 runtime Session，但删除组织关系；若 runtime cwd 命中 deleted
  Workspace tombstone，则永久显示 disabled，不能因为同路径后来被重建而恢复。

## 5. 持久化

`core/workspace` 使用独立 SQLite，WAL、foreign keys、busy timeout。最小表：

- `schema_meta(version)`
- `projects`
- `sources(project_id, kind, canonical_path, git_common_dir, folder_mode, source_key)`
- `workspaces(project_id, title, root_path, owner_nonce, status, error,
  archived_at, deleted_at)`
- `workspace_checkouts(workspace_id, source_id, worktree_path, base_ref,
  base_commit, branch_name, status, error)`
- `workspace_folders(workspace_id, source_id, mode, target_path, status, error)`
- `session_organization`

关键约束：

- Source、Workspace、binding 均由 opaque ID 标识；title/name 不参与路径。
- `(project_id, canonical_path)`、Git `(project_id, git_common_dir)`、
  `(workspace_id, source_id)` 唯一。
- 一个 Source 的 direct folder 同时只能被一个未删除 Workspace 使用。
- migration 只改 catalog，不改 legacy Session store 和外部文件。

provision/retry 根据 catalog binding 与真实文件/Git 状态对账：完全匹配则采用，完全缺失
则继续，身份冲突则停在 `error`。不引入 operation journal、lease、签名 cursor、catalog
restore journal 等当前产品不需要的机制；单 daemon writer 由进程内锁和 SQLite 事务保证。

## 6. Provisioning

### Git

在 common-dir 级互斥锁内：

1. probe source，并由 daemon 在本地解析 base ref；客户端可选传 expected commit 做乐观校验，禁止网络操作；
2. branch 必须合法且不存在；在一个 `git update-ref --stdin` 事务中同时 create branch 和
   `refs/zotigo/workspaces/<workspace-id>/<source-key>` ownership ref；
3. 用参数数组对已创建 branch 执行 `git worktree add --lock --reason <zotigo-owner> ...`；
4. 保存 branch/worktree/status 后再把 Workspace 置为 ready。

若 Git 在创建 refs 后中断，retry 只有在 branch 与 ownership ref 都仍为 planned base
commit、未被其他 worktree 使用、目标位于 matching owner Workspace 时继续；其余
partial state 进入 conflict，不采用已有同名 branch。delete 也必须验证 ownership ref 后
才能 CAS 删除 branch。所有 Git 调用清理会改变 repository 语义的继承 `GIT_*`
环境变量，设 timeout，不调用 shell。

### Folder

- `direct`：在 `code/<key>` 建指向 Source 的 symlink；Source 不可重叠占用。
- `reference`：复制到 `notes/<key>`，regular files 清 writable bit。
- `copy`：复制到 `code/<key>`。

copy/reference 在受管 staging 目录完成，不跟随逃逸 symlink，不复制 device/socket/FIFO，
完成后 atomic rename。retry 只清理带 matching operation marker 的 staging 目录，绝不
覆盖未知 final path。

## 7. Archive / delete（保持 Desktop 语义）

### Archive

- preview 列出 Sessions、worktrees 和 dirty worktrees；执行时重新检查。
- 任一 Session active/locked 或任一 worktree dirty 时拒绝。
- 对每个 checkout 执行非 force `git worktree remove <exact-path>`；保留 local branch、
  artifacts、notes、copy/reference/direct 内容和 runtime Session。
- 移除 worktree 前持久化 branch 的精确 tip；unarchive 和 archived delete 都要求 branch
  仍指向该 tip，防止同名 branch 被外部重建后误采用或误删。
- 全部完成后 Workspace/Sessions 标记 archived、清 pin。
- unarchive 在 branch 仍匹配时重建 locked worktree，再恢复 Workspace；Session 自己的
  archive 状态不变。

### Permanent delete

- 单独 preview，request 必须带与当前 title 完全相同的 `confirmation`。
- active/locked Session 时拒绝。
- 只对计划中且身份匹配的 worktree 使用 `git worktree remove --force`；仅删除 Zotigo
  创建并记录的精确 local branch；branch 与 ownership ref 在同一 `update-ref --stdin`
  事务中按 old-OID compare-and-swap 删除，不碰 remote ref。
- 验证 owner marker 后，把 Workspace root 原子 rename 到同 Project 下的 operation-specific
  trash，再递归删除该 trash；不跟随 symlink。
- 最后删除 binding 和 Session organization，保留 Project、Source、runtime Session，并把
  Workspace 置为 deleted tombstone。
- 中断后只向前恢复，不尝试重建已删除内容。

## 8. HTTP API

复用现有 envelope/error/auth 习惯。第一阶段公开：

```text
POST   /projects
GET    /projects
GET    /projects/{id}
POST   /projects/{id}/sources/inspect
POST   /projects/{id}/sources
DELETE /projects/{id}/sources/{source-id}

POST   /projects/{id}/workspaces
GET    /projects/{id}/workspaces
GET    /workspaces/{id}
POST   /workspaces/{id}/retry
GET    /workspaces/{id}/archive-preview
POST   /workspaces/{id}/archive
POST   /workspaces/{id}/unarchive
GET    /workspaces/{id}/delete-preview
POST   /workspaces/{id}/delete
```

第二阶段增加 Session organization：

```text
GET    /catalog/sessions
GET    /catalog/sessions/{id}
PUT    /sessions/{id}/title
PUT    /sessions/{id}/pinned
POST   /sessions/{id}/archive
POST   /sessions/{id}/unarchive
POST   /sessions/{id}/position
POST   /sessions               # optional workspace_id, cwd server-derived
```

创建与 lifecycle 操作通过 opaque ID、状态机和真实文件/Git 对账保证安全重试。v1 列表规模
很小，直接返回完整结果；等出现真实分页需求后再增加 cursor。

## 9. CLI 与 Desktop

- `--resume` 保留 cwd scope，但在启动 assigned Session 前检查 Workspace 状态。
- 新增 `--resume-all`，把 runtime Session 与 catalog organization 按 ID join，按
  Project → Workspace → Session 展示；catalog-only 和 cwd unavailable 项 disabled。
- CLI resume 后必须以 Session 实际 cwd 重新加载 config/profile/tools，不能沿用启动 CLI
  时的 cwd 配置。
- 后续 Desktop import 只登记 Project 和仍有效的 Source；不接管旧 Workspace 目录，也不
  自动给现有 daemon Session 分组。用户显式创建新的 daemon Workspace。

## 10. 实施顺序与验收

1. Catalog/store/domain CRUD。
2. Project/Source/Workspace HTTP API。
3. Git/folder provisioning 与 recovery。
4. Archive/unarchive/delete。
5. Session organization 与 assigned creation。
6. CLI `--resume-all`。

每一步只改必要路径并先跑 focused tests。最终运行：

```text
gofmt -w <changed-go-files>
go test ./core/workspace/... ./internal/zotigod/... ./internal/cliapp/... ./cli/tui/... -count=1
go build ./...
make check
git diff --check
```

验收重点：

- catalog 不受 legacy Session index rebuild/delete 影响；
- Project/Source/Workspace 重启后可恢复；
- provisioning partial state 不覆盖未知路径；
- archive 删除 clean worktree、保留 branch/root；
- delete 只删除 matching owned root/local branch，保留 Source/remote/runtime；
- assigned Session 始终使用 daemon 派生的 Workspace cwd；
- 旧 Session API 与 CLI 行为保持兼容。

## 11. 自审结论

已删除与当前目标无关的自定义 identity/token/cursor/restore binary protocol。保留的复杂度
只服务三个真实风险：跨 legacy store 的组织数据独立性、Git/文件副作用的中断恢复、以及
archive/delete 的误删防护。实现按上述六个切片推进，不在 catalog CRUD 阶段预埋后续
抽象。
