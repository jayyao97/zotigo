[English](../installation.md) | **简体中文**

# 源码安装（Linux 和 macOS）

## 安装

这种源码安装方式不需要 Homebrew tap、单独的管理器、GitHub Release 或 Apple Developer 账号。请以普通用户身份运行。前置条件是 Git、curl、tar 和一个 SHA-256 工具（`sha256sum` 或 `shasum`）。在 macOS 上，如果没有 Git，请安装 Command Line Tools；构建 daemon 还需要一个持久的代码签名身份，详见下文。Daemon 服务在 Linux 上需要 systemd 用户管理器，在 macOS 上需要已登录的 GUI 会话。

每次运行安装一个组件：

| 组件 | 安装内容 | 说明 |
| --- | --- | --- |
| `cli`（默认） | `zotigo` 终端客户端 | 会启动或连接 `zotigod`，因此也需要安装 daemon。 |
| `daemon` | `zotigod`、`zotigo-update`、一个用户服务 | [zotigo-ui](https://github.com/jayyao97/zotigo-ui) 中的 UI 安装程序会替你执行这一步。 |

```sh
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh |
  sh -s -- --component daemon
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh | sh
```

| 选项 | 含义 |
| --- | --- |
| `--component cli\|daemon` | 要安装的组件（默认 `cli`） |
| `--version TAG\|COMMIT` | 要构建的 tag 或完整的 40 位 commit |
| `--source CHECKOUT` | 构建本地 checkout，而不是从远端获取（不能与 `--version` 同时使用） |
| `--prefix PATH` | 安装前缀（默认 `~/.local/share/zotigo`，或 `ZOTIGO_INSTALL_PREFIX`） |
| `--no-service` | 不注册、不启动 daemon 服务 |

不指定 `--version` 时，安装程序会选择数值最高的正式版 `vX.Y.Z` tag，忽略 `v0.0.4-dev.45` 这类开发版 tag，并在执行该版本的安装程序之前获取选定的 Git 对象。如果不存在正式版 tag，会明确报错失败，绝不会回退到开发分支。Tag 必须不可变。要安装开发版构建、其他 tag 或某个完整 commit：

```sh
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh |
  sh -s -- --component daemon --version v0.0.4-dev.45
```

安装完成后，应用内更新可以跟随正式版或开发版更新渠道；参见[应用内更新](application-updates.md)。对于本地 checkout：

```sh
sh install.sh --source "$PWD" --component daemon
```

发布版本号是 `internal/buildinfo/buildinfo.go` 中的 `Version` 常量，CLI 和 daemon 共用。普通的 `go build`、`make build` 和安装程序都使用同一个值，不通过链接器注入。发布时，先更新该常量并提交，再发布对应的 tag（例如 `0.0.1` → `v0.0.1`）。安装程序会记录该版本号，并单独记录源码 commit 和 dirty 状态；它先构建再切换，并保留配置和会话数据。构建过程中可能会下载 Go 模块依赖。编译器在构建之前选定，安装程序禁用了 Go 工具链的自动下载。不要移动已发布的 tag。

## 构建工具

如果本地已有可用的 Go，且版本不低于 `go.mod` 中 `go` 指令的要求，就直接复用。否则，安装程序会从官方发行渠道下载该确切的 Go 版本，校验 `scripts/toolchains.tsv` 中固定的 SHA-256，并安装到 `PREFIX/toolchains/go-VERSION/` 下。私有下载支持 AMD64 和 ARM64 上的 Linux/macOS。如果较新的源码版本需要清单中没有的工具版本，安装会失败，并提示更新固定版本。

下载内容只有在校验通过后才会解压，只有在可执行文件报告了预期版本后才会发布。已有的版本目录会被复用，不会被覆盖；每个工具有一个锁，防止并发发布。下载失败不会替换已有的工具或已安装的程序。残留的锁或损坏的缓存工具需要人工检查，不会自动删除。

安装程序不会使用 `sudo`、不会通过全局包管理器安装、不会修改 shell profile，也不会做持久的环境变量修改。PATH 的改动只作用于本安装程序及其生成的启动脚本。工具链会保留以便复用；只有在确认没有已安装的服务或启动脚本在使用旧版本后，才手动删除它们。要更新受支持的下载项，在 `scripts/toolchains.tsv` 中固定新的官方 URL/版本/校验和；记录 pnpm 压缩包的 SHA-256 之前，先用 npm registry 的 integrity 值校验该压缩包。

## 文件与版本

默认前缀：`~/.local/share/zotigo`；可用 `--prefix /absolute/path` 或 `ZOTIGO_INSTALL_PREFIX` 覆盖。路径可以包含空格，但不能包含 shell/服务的元字符。请把 `PREFIX/bin` 加入 shell 的 PATH。

- `cli/releases/`、`daemon/releases/`：各自独立的构建，不会原地覆盖。
- 每个组件的 `current` 符号链接指向其当前安装，macOS daemon 除外：它的 `current` 是一个真实目录，这样可执行文件的解析路径保持不变。`previous` 保留上一个发布归档。
- `current/INSTALLATION`：由代码确定的发布版本号、源码 commit 以及 `dirty=true|false`。
- `config/runtime.env`：安装程序探测到的启动默认值，升级时保留。
- `config/daemon.env`：由用户维护的 shell 变量赋值，升级时保留。
- 首次配置时（没有 `daemon.env`，也没有 `daemon/current/INSTALLATION`），安装程序会创建 `config/daemon.token`，内容是 32 字节随机数的十六进制编码，权限为 `0600`，并在 `daemon.env` 中设置 `ZOTIGOD_AUTH_TOKEN_FILE`。生成 token 需要 OpenSSL。即使只监听 loopback，也会启用认证。安装中断后会复用已有的 token；升级时绝不会轮换 token，也不会给已有配置启用认证。如果已安装的 daemon 丢失了 `daemon.env`，请先恢复它再重新安装。
- 诊断日志位于前缀之外，在 `~/.zotigo/logs/cli/` 和 `~/.zotigo/logs/daemon/` 中。

运行 `zotigo --version` 或 `zotigod --version` 查看版本。`GET /health` 除了已有的 status/protocol 字段外，也会返回正在运行的 daemon 的 `version` 和 `commit`。commit 来自可选的 Go VCS 元数据，不可用时为 `unknown`；它不会被注入，也不会用作发布版本号。在服务重启之前，已安装版本和正在运行的版本可能不同。

## 服务

Daemon 以执行安装的用户身份运行，工作目录为该用户的 home 目录，默认监听 loopback 端口 8766。安装程序会记录 Git 和外部 agent 工具所需的 PATH。编辑 `config/daemon.env` 可设置 `ZOTIGOD_ADDR` 和可选的 `ZOTIGOD_AUTH_TOKEN_FILE`；它是受信任的 shell 文件，不是下载来的配置。配置远程访问前，请先阅读 [API 认证文档](../zotigod-api.md)。

Linux：

```sh
systemctl --user status zotigo-daemon.service
systemctl --user stop zotigo-daemon.service
systemctl --user start zotigo-daemon.service
journalctl --user -u zotigo-daemon.service
```

如果需要用户管理器在开机时启动并在注销后继续运行，请使用 `loginctl enable-linger "$USER"`；这可能需要管理员协助。没有 systemd 的主机可以使用 `--no-service`，并在已有的进程管理器下以前台方式运行 `PREFIX/daemon/current/run`。

macOS：

```sh
launchctl print gui/$(id -u)/com.zotigo.daemon
launchctl bootout gui/$(id -u)/com.zotigo.daemon
launchctl bootstrap gui/$(id -u) "$HOME/Library/LaunchAgents/com.zotigo.daemon.plist"
```

LaunchAgent 运行在用户的登录会话中，不会在登录前运行。关闭 Desktop 或终端不会停止该服务。在两个平台上，`--no-service` 都会跳过注册和启动。

## 后台服务中的 Codex 探测

在选择构建工具链之前，安装程序会记录显式设置的 `ZOTIGO_CODEX_BINARY`，或调用方 PATH 中 `codex` 的绝对路径。它会校验 `--version`，并把探测到的默认值和启动脚本的 PATH 保存到 `config/runtime.env`，该文件在升级后保留。已有版本生成的 PATH 和 Codex 赋值会从其 `run` 脚本中迁移过来，但不会执行该脚本。这样，当后台更新 worker 的 PATH 很精简时，自定义的 Codex 路径也能保留。

启动脚本随后会加载用户维护的 `config/daemon.env`，因此显式覆盖仍然优先。如果保存的路径已被删除，请在 `daemon.env` 中把 `ZOTIGO_CODEX_BINARY` 设为新的可执行文件绝对路径，然后重启 daemon。升级时绝不会改写用户配置。

在已登录的终端中执行 `curl ... | sh` 时，安装程序继承该终端的 PATH；它不会加载 shell profile。非交互式 SSH 命令的 PATH 可能不同。找不到 Codex 或 Codex 不可用不会阻止安装，此时仍由运行时探测处理。安装程序会把经过工具链调整的 PATH 保留在 `config/runtime.env` 中，包括基于脚本的 Codex 入口所需的解释器查找路径。它不会把外部 Node/版本管理器的目录固定下来。

Daemon 不会加载交互式 shell 的启动文件。Codex 探测依次检查 `ZOTIGO_CODEX_BINARY`（可执行文件的绝对路径）、daemon `PATH` 中的 `codex`、`~/.local/bin/codex`。在 macOS 上还会检查已有的用户级和系统级 ChatGPT.app 应用包，先检查 `Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex`，再检查旧的 `Contents/Resources/codex` 位置。用户 bin 目录中的条目可以是指向独立 Codex 发行版的符号链接。探测过程不会修改 `PATH`、不会安装 Codex，也不会在多个版本管理器安装之间做选择。

如果 Codex 在自定义位置，请在 `~/.local/share/zotigo/config/daemon.env`（自定义安装前缀时为 `PREFIX/config/daemon.env`）中加入 `export ZOTIGO_CODEX_BINARY=/absolute/path/to/codex`。显式指定的路径无效时会直接失败，而不是改选其他安装。该文件在升级时保留。重启 daemon 以重新加载其环境之前，请先完成正在进行的任务；只重启 Desktop 或 Web 不会重新加载 daemon 配置。基于脚本的安装还要求其解释器（例如 Node）在 daemon 的环境中可用。

## 本地诊断日志

CLI 和 daemon 的启动、关闭及运行错误会自动保存到 `~/.zotigo/logs/cli/` 和 `~/.zotigo/logs/daemon/`。每个组件跨重启和并发进程总共最多保留 100 MiB。单个文件达到 5 MiB 时轮转；另有 128 个文件的上限，限制小文件的累积。小日志在重启后保留，直到总字节数或文件数需要清理为止。不同进程写不同的文件，因此 worker 不会向其他 worker 的文件追加内容或轮转它。清理过程能容忍文件已被其他进程删除的情况。这些是写入后检查的保留上限，并非并发写入期间的即时磁盘配额。旧文件会自动清理；当另一个进程很繁忙时，较安静的进程可能丢失其较早的日志。单条日志超过 64 KiB 会被截断。文件以仅所有者可访问的权限创建。日志写入失败不会中断应用的工作。

用 `ls -lt ~/.zotigo/logs/daemon/` 找到最近的 `run-*.log` 文件，然后执行 `tail -n 200 /path/to/run-file.log`。CLI 日志的布局相同。启动前设置 `ZOTIGO_DEBUG=1` 可获得已有的详细 agent 诊断信息；对于已安装的 daemon，在 `config/daemon.env` 中加入 `export ZOTIGO_DEBUG=1` 并重启它。分享日志前请先检查：错误信息和外部工具的输出可能包含隐私信息。

这些是运行诊断信息，不是完整的终端录屏。会话/项目历史单独保存，日志保留策略绝不会删除它们。前台运行的 daemon 仍然可以看到 stderr。CLI 和 daemon 会启动一个小的子监控进程，通过同一个有上限的写入器保存 Go 运行时崩溃跟踪，包括主 goroutine 之外的 panic。已安装的服务会丢弃重复的 stdout/stderr 输出；应用诊断请使用上述文件，服务管理器层面的故障请使用 `systemctl`/`launchctl`。重新安装会更新旧的服务定义；遗留的 `PREFIX/logs/daemon.log` 会留在磁盘上，停止旧服务后可以手动删除。

## 升级与恢复

针对目标不可变 tag 运行同一个安装程序即可。它会先构建新版本，但如果现有安装的进程仍在运行，则拒绝激活。请先完成任务并停止/卸载服务，再重试。安装程序运行期间不要另行重启服务。安装程序绝不会静默中断正在进行的任务，也不会接管 Homebrew 或手动安装。

构建或签名失败时 `current` 保持不变。之前的构建目录仍然可用。如果激活后启动失败，请检查日志和 `previous` 链接；没有自动的数据回滚。只有当旧程序能读取当前数据格式时才可以降级。安装过程绝不会删除配置和 `~/.zotigo` 中的项目/会话数据。安装成功或升级验证通过后，安装器会清理更早的受管版本和已完成的升级临时目录，保留当前版本和可供回退的上一版。macOS daemon 通过 `current/RELEASE` 标识当前归档。安装器保留激活失败的临时目录，以及带有停止检查记录、无法确认升级结果的旧版符号链接临时目录，供排查使用。已安装用户在下次成功运行新版安装器或更新助手时会自动清理。

## macOS daemon 身份与隐私权限

Daemon 使用签名标识符 `com.zotigo.daemon`。与 Go 的 ad-hoc 签名不同，持久证书能让先后多次构建满足同一个代码身份要求（code identity requirement）。安装程序和 Desktop 开发构建脚本都使用 `scripts/sign-macos-daemon.sh`。如果签名身份缺失、不可用或无法签名，它们会失败，绝不会回退到 ad-hoc 签名。

对于个人源码构建，请用“钥匙串访问”（Keychain Access）的证书助理在登录钥匙串中创建一个持久的 **代码签名**（Code Signing）证书。本地自签名身份足以测试代码身份的连续性；它不是 Developer ID 分发签名，也不是公证（notarization）。升级之间要保留其私钥。不要每次构建都生成新证书。如果它没有被列为有效身份，请在“钥匙串访问”中检查它的代码签名信任设置。用于分发时，请改选你已有的 Developer ID Application 身份。

列出可用的身份，然后选择证书的 40 位 SHA-1 指纹（不是显示名称）：

```sh
security find-identity -v -p codesigning
ZOTIGO_MACOS_SIGNING_IDENTITY=YOUR_40_CHARACTER_CERTIFICATE_FINGERPRINT \
  sh install.sh --source "$PWD" --component daemon
```

签名成功后，指纹会保存到 `~/.config/zotigo/macos-signing-identity`，权限为仅所有者可访问。之后的安装和 Desktop 开发构建会复用它，无需再设置环境变量。`ZOTIGO_MACOS_SIGNING_IDENTITY_FILE` 可指定另一个文件。该文件只包含公开的证书指纹；私钥保留在钥匙串中。钥匙串可能需要解锁或交互式授权。无界面（headless）构建必须显式安排钥匙串访问。更换签名身份可能需要重新进行隐私授权。证书的创建和信任设置不会自动完成。

macOS daemon 运行于 `PREFIX/daemon/current/zotigod`，该路径中不含版本符号链接。升级前照常停止服务。安装程序会把构建归档到 `releases/`，暂存一个完整的活动目录，然后通过重命名将其放到位。如果激活时的重命名失败，会恢复旧目录。这是停止服务后进行的替换，期间有短暂的目录缺失窗口，并不是在线的原子目录交换。强制 kill 或断电后，恢复之前请检查残留的 `releases/build.*/old-current` 和安装锁；`current` 不存在时不要删除该暂存目录。正常的信号会触发清理。已有的基于符号链接的安装会在下一次成功安装时迁移。回滚时，请重新安装包含这种固定路径安装布局的兼容源码版本。旧的安装程序无法发布到新的真实 `current` 目录。不要把 `current` 替换为指向某个发布的符号链接，也不要启动归档中的二进制文件。

从 ad-hoc 签名迁移过来时，可能需要重新授予一次 macOS 权限。旧的权限条目不会自动删除。请在真实的 Mac 上验证：给构建 A 授予所需权限，升级到使用同一身份的另一个构建 B，确认文件访问正常且没有新增条目/提示，然后验证回滚。可以比较两个构建的 `codesign -d -r- PATH` 输出作为辅助证据。不同的前缀以及 Desktop 单独的开发版 daemon 路径属于不同的安装，可能仍需分别授权。安装程序不会修改 TCC 数据库或任何系统级隐私设置。

卸载时，先停止并禁用/卸载服务，删除其服务文件和安装程序创建的二进制链接，然后删除该组件的安装目录。除非明确要删除用户数据，否则请保留 `~/.zotigo` 和 `config/`。

### macOS Desktop 更新与重复的 Dock 图标

Desktop 自带一份更新程序。首次升级**到**包含 launchd 清理修复的版本时，运行的仍是之前安装的辅助程序。此后的 prepare 和 apply 任务会在 worker 退出后卸载各自的服务 label。

旧的辅助程序可能在 GUI 登录域中留下已完成的任务。如果 Dock 中有多个图标但只有一个 Desktop 进程在运行，可以在 `lsappinfo list` 中检查旧的 `com.zotigo.desktop` 记录及其 `loadedJobs`。对每个确切的 label，执行 `launchctl print "gui/$(id -u)/$label"` 进行检查。确认其程序参数指向本安装的 `updates/desktop/runner`、组件为 desktop，并且该任务已经运行过且现已停止。清理时，对每个已确认的服务目标执行 `launchctl bootout "gui/$(id -u)/$label"`，然后重新检查 LaunchServices 和 Dock。共享的 worker.plist 只描述最近一次任务。更新程序不会批量删除较早的注册项。注销后重新登录也会销毁 GUI 登录域。

要在独立的 macOS GUI 账号中验证，运行 `python3 scripts/macos_desktop_update_test.py`。它在临时安装中使用唯一的测试 bundle ID、一个本地 Git remote，并执行两次连续更新。在已经授权 Dock 辅助功能检查的测试账号上，设置 `ZOTIGO_TEST_DOCK_UI=1` 还会通过 System Events 统计可见的测试图标数量。
