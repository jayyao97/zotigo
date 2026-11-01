# 应用内更新

设置中的“版本与更新”分别管理当前客户端和所连接主机的 Daemon。Desktop 更新发生在此电脑，Web 更新发生在提供页面的服务器；Daemon 更新始终在对应主机执行。关闭浏览器或断开远程连接不会取消已经提交给服务管理器的更新任务。

## 首次安装与发布

旧版没有更新接口或独立 updater，需要先通过安装脚本手动升级一次。Daemon 的公开 API 必须配置 bearer token；内部 worker token 不具有更新接口权限。服务升级适用于安装器创建的 systemd 用户服务或 macOS LaunchAgent。源码开发进程不由更新器管理。

本版仍从官方 Git 仓库下载源码并使用固定工具链构建，因此目标主机需要能访问 GitHub 和工具链下载源。构建期间原服务继续运行，只有“应用更新”阶段停止服务。

发布者需要一起维护：

- 程序版本与根目录 `update.json` 的 `version`；它们必须一致。
- `update_protocol`：更新器协议，当前只接受 `1`。
- `api_epoch`：客户端与 Daemon 的兼容代际。当前应用内更新要求目标与已安装组件的代际相同。
- `data_epoch`：持久化格式的双向兼容代际。同一代际必须允许旧版读取新版写入的数据，才允许自动回退。
- UI 的 `daemon-version`：源码构建使用的配套 Daemon revision。

先发布配套 Daemon tag，再发布 UI tag。当前待发布组合为 Daemon `v0.0.4-dev.31`、UI `v0.0.4-dev.38`。开发测试可用 UI 安装器的 `--daemon-source` 指向本地 checkout。代码提交本身不会成为可检测的更新；必须发布版本标签。不要重用或移动已发布标签。

## 渠道与版本检测

更新器读取官方仓库 tags，以数值比较 `vX.Y.Z`、`vX.Y.Z-dev.N`。正式渠道只包含正式标签，开发渠道同时包含开发和正式标签，因此开发版可以自然升级到对应正式版。annotated tag 使用剥离后的 commit，准备阶段固定 fetch 此 commit，避免检查与下载之间标签变化。

UI 和每台 Daemon 独立保存渠道。首次从安装版本推断，后续使用保存的选择。切换渠道只检查版本；开发版切回较旧正式版时也会展示该目标，仍需点击准备和应用。自定义 dirty 构建显示独立标识，用户明确选择已发布版本后才安装。

客户端启动及每四小时做后台检查；设置页面会检查过期结果，“检查更新”立即重查。网络失败会显示错误，不会被视为“已经是最新版”。没有兼容清单或协议、API、数据代际不匹配的发布可以被检测，但准备时会拒绝安装，需要手动迁移。

## 升级与恢复

1. `prepare` 固定源码 commit、验证清单，在安装目录生成 `prepared`。不停止现有服务。
2. `apply` 启动独立更新任务。Linux 使用独立 transient systemd unit，macOS 使用独立 LaunchAgent，任务不会跟随被升级 Daemon 的进程组退出。
3. Daemon 先关闭新输入和队列派发的准入，等待当前 turn、工具调用及等待人工输入的任务结束。尚在等待阶段可取消；持久化队列中的未执行消息保留。升级完成后可能需要用户显式恢复已暂停的队列。
4. 取得与命令行安装器相同的安装锁，停止组件，记录切换日志，激活新版并启动。保留原版本及配置。macOS Daemon 继续使用固定路径和既有签名身份。
5. Daemon 检查健康接口、版本、服务 PID 和鉴权接口；Desktop 等待新窗口加载完成的启动回执；Web 检查 HTTP 启动状态。失败则尝试停止新版、恢复原版本并启动。

状态与日志保存在 `<prefix>/updates/<component>/`，其中 `status.json` 是 UI 状态、`update.log` 是构建与切换日志。敏感 token 不写入命令行。版本归档在 `<prefix>/<component>/releases/`。

恢复记录 `journal` 指向隔离的切换目录。服务的固定 `launch` 入口先调用独立 updater 恢复中断的切换，再启动 `current/run`。状态查询会识别没有活跃 updater 的遗留任务并清除残留准入限制；如果 `current` 缺失，则从日志中的原目录恢复。恢复失败时保留日志和目录，不覆盖未知安装。

## 验证

使用隔离 prefix/HOME，禁止替换开发者正在运行的服务：

```sh
go test ./internal/update ./internal/zotigod
python3 scripts/update_test.py
python3 scripts/install_test.py
```

平台服务命令在事务测试中替换为隔离 stub，目录切换与进程检查使用真实实现。真实 macOS 签名、LaunchAgent、Desktop 启动，以及目标远端服务的完整升级仍需发布前实机验收。
