# CLI 参考

GPUWorks 提供两个命令行程序：`gpuworks` 用于安装、运行、升级和维护控制面；`gpuworks-agent`
用于连接 GPU 节点、检查节点环境、排查容器问题和执行评测。

本页按子命令列出 flag、默认值和行为要点；以各命令的 `--help`（`-h`）输出为准，flag 定义分别在
`cmd/gpuworks`、`cmd/gpuworks-agent`。环境变量见[配置参考](configuration.md)。

## 示例：重启控制面

修改 `server.yaml` 后，在**已安装控制面的机器**上执行以下命令，使配置生效。需要该安装的管理权限；
命令会短暂停止控制面服务，再按原有服务方式启动并检查是否恢复。
命令中的 `gpuworks` 需能在当前 `PATH` 中找到；否则使用安装目录里的完整路径，例如 `/opt/gpuworks/server/gpuworks restart`。

```bash
gpuworks restart
```

成功时输出类似以下内容，括号内是当前安装的服务方式：

```text
gpuworks restarted (systemd)
```

指定其他安装目录时，在命令后加 `--dir /实际安装目录`。全部参数与限制见下文
[`gpuworks restart`](#gpuworks-restart)；查看命令帮助可执行 `gpuworks restart --help`。

## 命令总览

| 二进制 | 子命令 | 用途 | 谁、什么时候用 |
|---|---|---|---|
| `gpuworks` | [`serve`](#gpuworks-serve) | 运行控制面（Web UI、管理 API、OpenAI 兼容网关） | 由 systemd 单元或 nohup 拉起；开发时手工运行 |
| | [`install`](#gpuworks-install) | 把自身安装成服务、写 `server.yaml`、启动并探活 | 运维，首次部署或修复安装 |
| | [`upgrade`](#gpuworks-upgrade) | 从发布通道（或内网发布站、本地发布包）升级已安装的控制面，失败自动回滚 | 运维，升级控制面 |
| | [`restart`](#gpuworks-restart) | 按当前服务方式重启已安装的控制面 | 运维，改完 `server.yaml` 之后 |
| | [`stop`](#gpuworks-stop) | 按当前服务方式停止已安装的控制面 | 运维，维护或停机前 |
| | [`uninstall`](#gpuworks-uninstall) | 卸载控制面，可选清理配置与数据 | 运维，迁移或下线 |
| | [`expose`](#gpuworks-expose) | 给控制台发布一个公网入口 | 运维，控制面在 NAT 后面时 |
| | [`migrate`](#gpuworks-migrate) | 对指定 SQLite 执行迁移和内置目录 seed 后退出 | 运维/开发，离线检查或预迁移数据库 |
| | [`reset-password`](#gpuworks-reset-password) | 重置管理员账户密码 | 运维，忘记密码时 |
| | `version` | 打印版本 | 任何人 |
| | `console-upgrade-protocol` | 打印本版本支持的控制台升级协议号（当前 `1`）；控制台升级之前的版本没有这个子命令 | 控制台升级前，服务端对待装的新二进制调用 |
| `gpuworks-agent` | [`configure`](#gpuworks-agent-configure) | 把启动配置写进 `<state>/gpuworks-agent.conf` | 安装脚本调用；手工接入的节点 |
| | [`run`](#gpuworks-agent-run) | 连接 server 并提供节点能力 | 由安装脚本/单元拉起，通常不手工运行 |
| | [`smoke`](#gpuworks-agent-smoke) | 只做一次连接握手校验后退出 | agent 自升级前的校验；排查连通性 |
| | [`upgrade`](#gpuworks-agent-upgrade) | 从 server 下载新 agent，校验后原地替换 | 节点管理员，手工升级 agent |
| | [`doctor`](#gpuworks-agent-doctor) | 打印节点环境自检 | 排查节点接入、GPU、运行时问题 |
| | [`grant-device-access`](#gpuworks-agent-grant-device-access) | 让非 root 运行时用户获得加速卡设备节点的访问权 | 安装脚本自动调用；无 root 的 runveil 节点手工补权限 |
| | [`bench`](#gpuworks-agent-bench) | 对 OpenAI 兼容端点做压测 | 性能评估；评测页的命令行等价入口 |
| | [`runveil`](#gpuworks-agent-runveil) | 查看/进入 runveil 容器，导入镜像归档 | 在 runveil 节点上排障；把拷到节点上的镜像归档导入 |
| | [`install-service`](#gpuworks-agent-install-service) | 把本二进制注册为 macOS 的 launchd 任务 | 安装脚本在 Mac 上调用 |
| | [`uninstall`](#gpuworks-agent-uninstall) | 停止并移除 agent 服务 | 节点下线 |
| | `version` | 打印版本 | 任何人 |
| 节点安装脚本 | [`/install.sh`](#节点安装脚本-installsh) | 由 server 提供的 `curl \| sh` 引导脚本 | 添加节点时复制安装命令 |
| 其他 | [`gpuworks-image-manifest`](#gpuworks-image-manifest)、[`gpuworks-dist`](#gpuworks-dist)、[`template-probe`](#template-probe)、[`gpuworks-catalog-model-config`](#gpuworks-catalog-model-config) | 目录、镜像与发布通道维护工具 | 目录维护者与 CI，从源码运行 |

`gpuworks` 和 `gpuworks-agent` 不带子命令或给出未知子命令时均打印用法并以 2 退出。启动 agent 必须显式使用 `gpuworks-agent run`，即使已通过环境变量或配置文件配置好 server 和 token；子命令的 flag 必须跟在子命令之后。`--help`（`-h`）打印帮助并以 0 退出。

## gpuworks serve

运行控制面。不带 `--config` 时在 `--addr` 上提供全部功能；带 `--config` 时读取该 `server.yaml`
（键见[配置参考](configuration.md#server-配置文件-serveryaml)），命令行给出的同名参数覆盖文件里的值。

| flag | 默认值 | 说明 |
|---|---|---|
| `--config PATH` | 空 | `server.yaml` 路径 |
| `--addr ADDR` | `:8800` | 用这一个地址提供全部功能，替换文件里整个 `listen` 节 |
| `--data DIR` | `./data` | 数据目录 |
| `--db PATH` | `<data>/gpuworks.db` | SQLite 路径 |
| `--trusted-proxies CIDRS` | 空 | 允许提供 `X-Forwarded-For` 的代理 IP/CIDR，逗号分隔 |
| `--metrics-retention DURATION` | `72h` | 原始指标和日志行的保留窗口 |
| `--log-file PATH` | `<data>/logs/gpuworks-server.log` | 日志文件；空串等同未给，仍写默认路径 |
| `--log-max-size SIZE` | `10MB` | 日志轮转阈值（KB/MB/GB，1024 进制） |
| `--log-keep N` | `20` | 保留的轮转文件代数 |
| `--log-level LEVEL` | `info` | `debug` / `info` / `warn` / `error`，其他值按 `info` |
| `--proxy-max-body-bytes BYTES` | `268435456`（256 MiB） | `/v1` 请求体上限，超出返回 413 |
| `--recovery-enabled BOOL` | `true` | 启用 SSH 旁路恢复离线 agent |
| `--recovery-grace-period DURATION` | `2m` | 断线后等待多久才开始 SSH 恢复 |
| `--recovery-initial-interval DURATION` | `30s` | 首次恢复重试间隔 |
| `--recovery-backoff-multiplier FLOAT` | `2` | 恢复重试间隔倍数 |
| `--recovery-max-interval DURATION` | `10m` | 恢复重试间隔上限 |
| `--recovery-restart-wait DURATION` | `1m` | 重启或重装后等待 agent 重连的时间 |
| `--stale-check-interval DURATION` | `30s` | 在线节点过期检查间隔 |
| `--stale-after DURATION` | `90s` | 心跳超过该时长即标记离线 |
| `--server-dial-reconnect-interval DURATION` | `10s` | server_dial 节点重连扫描间隔 |
| `--supervise` | `false` | 不直接运行，改为守护进程：用同一个二进制路径、去掉本 flag 的同一组参数启动 server，退出后 3 秒重新拉起 |

行为要点：

- 文件键与 flag 的对应关系、监听规则见[配置参考](configuration.md)；`--recovery-*`、`--stale-*`、`--proxy-max-body-bytes`、`--server-dial-reconnect-interval` 没有文件键，默认值即生产取值。
- 日志同时写 stderr 和文件，按大小轮转；日志配置只有 flag 和 `server.yaml`，没有环境变量。
- server 读取面向操作员的 `GPUWORKS_ADMIN_PASSWORD`（首次创建 admin 时的初始密码）和 `GPUWORKS_USAGE_REPORT=off`（强制关闭使用情况上报），另有守护进程设给子进程的内部标记 `GPUWORKS_SUPERVISED=1`（server 读后即从自身环境删除，不传给它启动的进程）；`GPUWORKS_ADDR`、`GPUWORKS_DATA_DIR`、`GPUWORKS_TRUSTED_PROXIES` 仅作老单元的兼容桥接，详见配置参考。
- 收到 `SIGUSR1` 时临时打开仅回环的 pprof 监听（先试 `127.0.0.1:6060`，被占用则换随机端口），10 分钟后自动关闭；实际地址在日志里。
- `SIGINT`/`SIGTERM` 触发优雅退出；进程在 systemd 下运行（存在 `INVOCATION_ID`）或由 `--supervise` 启动时，控制台的「立即重启」和控制台升级可以让它自行退出并由 `Restart=always` 或守护进程拉起。

### `--supervise` 的进程行为

使用 `--supervise` 时，父进程负责监督 server 子进程，不读配置、不开数据库：

- 平时循环执行「启动 server、等待它退出、3 秒后再启动」。每次都按安装路径启动；控制台升级把新二进制 rename 到这个路径后，下一次启动的是新版本，父进程自己仍保持启动时的版本。
- 收到 SIGINT/SIGTERM 时把信号转给 server，等待 server 退出后，父进程以 0 退出。
- Linux 上 server 带 parent-death 信号：父进程被 SIGKILL 时，server 一起被杀。父进程作为 1 号进程时会回收所有孤儿进程。
- `--config` 指向的目录正是这个二进制所在的安装目录时，父进程把自己的 pid 写进 `<dir>/gpuworks.pid`，干净退出时删除。

## gpuworks install

把当前二进制安装成服务：拷贝到 `--dir`、写 `server.yaml` 和服务定义、启动并探活。操作步骤见[安装和升级控制面](../how-to/install-server.md)，目录布局、权限与凭证文件见[控制面安装布局](install-layout.md)。

| flag | 默认值 | 说明 |
|---|---|---|
| `--dir DIR` | `/opt/gpuworks/server`；macOS 上是 `~/gpuworks/server` | 安装目录；`--dir`、`--data`、`--logs` 开头的 `~` 展开为主目录 |
| `--data DIR` | `<dir>/data` | 数据目录；既有安装沿用其 `server.yaml` 里的值 |
| `--logs DIR` | `<dir>/logs` | 日志目录；既有安装沿用其现值 |
| `--addr IP:PORT` | 默认路由出口 IP、端口 8800 | 监听地址（承载全部功能）；只写 `:端口` 表示 `0.0.0.0` |
| `--trusted-proxies CIDRS` | `127.0.0.1/32,::1/128` | 允许提供 `X-Forwarded-For` 的代理 |
| `--password PW` | 无 | 管理员密码；缺省时的优先级见下 |
| `--service-user USER` | `gpuworks` | systemd 服务账户；`root` 表示不建专用账户。macOS 上忽略：launch daemon 以执行安装的用户运行 |
| `--log-max-size SIZE` | `10MB` | 日志轮转阈值 |
| `--log-keep N` | `20` | 日志保留代数 |
| `--quiet` | `false` | 不打印 banner、不提问，未给密码则随机生成（结束摘要仍会打印） |
| `--autostart` | Linux 上 `true`，macOS 上 `false` | 安装开机启动的 systemd 单元（macOS 上是 launch daemon）；与 `--no-systemd` 同给时报错 |
| `--no-systemd` | `false` | 不用服务管理器（systemd，macOS 上是 launchd），改为 nohup 后台启动 |
| `--skip-check` | `false` | 把预检中预判 server 能否运行的检查（端口占用、磁盘空间、服务账户能否进入安装目录）降为告警继续安装；保护主机的检查不受影响 |

行为要点：

- 预检：需要 root 或可用的 `sudo`（否则降级为 nohup 并告警）；目标端口被别的进程占用则失败（被当前安装占用除外）；安装目录所在文件系统至少要有 2 GiB 空闲；systemd 模式下，安装目录、数据目录和日志目录之上已存在的每一级目录都必须允许 `--service-user` 进入（`x` 位，按属主/属组/其他判定；带 ACL 的目录不拒绝），否则失败，例如装在 `/root`（`0700`）下。这三项可用 `--skip-check` 降为告警；拒绝对 `/`、`/var`、主目录等宽泛目录递归改权限、以及会放宽敏感文件 ACL 的检查不能跳过。
- 提权：install/upgrade/reset-password 以操作员身份运行，特权步骤（建目录、建账户、写单元、`systemctl`）各自以 `sudo` 命令执行；`sudo` 首次问一次密码并缓存，无终端又没有缓存的 `sudo` 会直接失败而不是挂起。
- systemd 模式：创建 `--service-user` 系统账户（已存在则复用），并把操作员加入该组（当前终端需 `newgrp` 或重新登录）；此后 upgrade 备份和替换二进制不再需要 `sudo`，但 `systemctl` 和收尾的 `chown`/`chmod` 仍需要；reset-password 不需要。
- nohup 模式（`--no-systemd`、没有运行中的 systemd、没有 root/`sudo`，或 macOS 上未给 `--autostart`）：忽略 `--service-user`（只在显式给出时告警），以调用用户在后台运行 `serve --supervise --config <dir>/server.yaml`，server 退出后由它重新拉起（因此支持控制台升级）；重启机器或容器后不会自动恢复，用 `<dir>/gpuworks restart` 拉起、`<dir>/gpuworks stop` 停止，或把这条命令设为容器的启动命令，见[在容器里运行](../how-to/install-server.md#在容器里运行)；macOS 上可再执行 `<dir>/gpuworks install --autostart` 改为开机启动。安装摘要会打印这些命令。
- macOS：默认以 nohup 模式运行，安装目录默认 `~/gpuworks/server`（执行安装的用户的主目录，`sudo` 下取 `SUDO_USER` 的）。launchd 模式（有 root/`sudo` 且给了 `--autostart`；重装已有 launch daemon 的安装时不给也沿用）写 `/Library/LaunchDaemons/ai.gpuworks.server.plist` 并 `launchctl bootstrap`，以执行安装的用户运行、开机即起、退出后由 launchd 拉起（支持控制台升级），安装树归该用户。plist 内容见[控制面安装布局 · launchd plist](install-layout.md#launchd-plistmacos)。
- 交互：不带 `--quiet` 时先打印 banner（输出是终端且 locale 为 UTF-8 时是块状字符，否则纯 ASCII）；在终端上再依次问安装目录（回答开头的 `~` 同样展开）、监听地址、是否自启（yes/no，macOS 上默认 no），最后问管理员密码；命令行已给出的项不再问，回车采用括号里的默认值（既有安装的现值或静态默认）。
- 管理员密码优先级：`--password` > 环境变量 `GPUWORKS_ADMIN_PASSWORD`（交互时回显并确认）> 交互提问（回车随机生成）> 随机生成；写入 `<data>/initial-credentials`（默认 `0600`，可信 Linux 平台组为 `0660`），不进单元文件。数据目录已有 `gpuworks.db` 时不再解析密码，`--password` 被忽略并提示用 `reset-password`。
- 重复执行是修复路径：保留既有 `server.yaml`（命令行显式给出才覆盖）、替换二进制、重建服务并重启，保留数据库、`master.key` 和 admin 账户内容，并收紧敏感文件权限。
- server 不管理自己的对外地址：添加节点、云厂商安装等 API 一律要求调用方显式给出 `server_url`。

## gpuworks upgrade

从[发布通道](../adr/0040-signed-multi-source-distribution.md)取当前版本（或用 `--base-url` 读内网发布站、用 `--archive` 指定本地发布包）替换已安装的控制面：停服、备份、原子替换、探活（60 秒），失败自动回滚并以非零码退出。

| flag | 默认值 | 说明 |
|---|---|---|
| `--dir DIR` | 本二进制所属的安装（所在目录有 `server.yaml` 或 `install.json`），否则默认安装目录（`/opt/gpuworks/server`，macOS 上 `~/gpuworks/server`） | 安装目录 |
| `--data DIR` / `--logs DIR` | 安装时记录的目录 | 覆盖数据/日志目录 |
| `--channel NAME` | `server.yaml` 的 `update_channel`，没有时为构建时的渠道（master 构建 `nightly`，`v*` tag 构建 `release`） | 读哪个渠道的发布索引：`release`（正式发布版）或 `nightly`（滚动预览版）；只用于发布通道 |
| `--source URL` | 内置的三个公共源 | 发布通道格式的源（`<URL>/<channel>/index.json`），可重复，替换内置源；用于客户自建镜像站 |
| `--base-url URL` | 空 | 改读发布站（`<URL>/data/release.json` 与 `<URL>/downloads/`），例如自建在内网的发布站 |
| `--archive FILE` | 空 | 本地发布包 `gpuworks-<release_id>-<os>-<arch>.tar.gz`，不发任何网络请求 |
| `--version ID` | 空 | 期望的 release id；与渠道或站点当前版本（或发布包里的版本）不一致时报错，不会静默升到别的版本 |
| `--keep N` | `3` | 保留的升级前备份组数 |
| `--force` | `false` | 允许替换没有 release id 的本地构建（`dev` / `-dirty`） |
| `--check` | `false` | 只报告是否有新版，不做任何修改 |
| `--quiet` | `false` | 只输出警告与错误，成功时无输出 |

行为要点：

- `--dir` 下没有 `server.yaml`、但有 `server.yaml` 之前的版本留下的 `install.json` 时，先把其中的监听地址、数据/日志目录、日志轮转和可信代理写成 `server.yaml`，把用环境变量传配置的单元改写成 `serve --config`，删掉 `install.json`，再继续；这一步只能由已安装的二进制执行（`<dir>/gpuworks upgrade` / `restart`），换别的二进制会报错退出、不做任何修改。两者都没有时视为未采纳的安装，报错并提示先跑 `install`。已是最新版时打印提示并以 0 退出。
- 不给 `--dir` 时操作执行的这个二进制所属的安装：`/opt/gpuworks1/gpuworks upgrade` 升级 `/opt/gpuworks1`，不会去动默认目录里的另一套安装；下载到别处的二进制仍操作默认安装目录。开始时打印 `upgrading the installation at <dir> (<当前版本>, <服务方式>)`。
- 停服后先确认 `server.yaml` 里的监听地址已经释放；仍被占用（例如同机另一套安装监听同一端口）时直接失败，不备份、不替换，停掉的服务原样拉起。Linux 上报出占用进程的 pid 与命令行，占用者是另一套 gpuworks 安装时提示用 `--dir <它的目录>`；查不到进程时报出该地址 `/healthz` 应答的版本。
- 本地构建（`dev` 或 `-dirty`）不加 `--force` 会被拒绝。
- `--source`、`--base-url`、`--archive` 三者互斥；`--channel` 只能与发布通道（默认或 `--source`）一起用。
- 渠道里的版本比当前构建旧时（例如从 nightly 切到 release）不降级，打印提示并以 0 退出；加 `--force` 才安装。
- 发布通道：同时请求所有源的 `<channel>/index.json`，用第一个签名校验通过的（ed25519，公钥编在二进制里，源本身不需要可信）；压缩包先从该源下载，失败或 30 秒没有新数据就换下一个源并用 Range 续传，最后按索引里的大小与 sha256 校验。输出里写明来自哪个源。内置源是阿里云 OSS（`https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com`）、华为云 OBS（`https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`）与 Cloudflare R2（`https://dl.gpuworks.ai`）。
- `--base-url`：发布站的清单不签名，信任来自站点的 TLS，压缩包按清单里的 sha256 校验；用于内网发布站。
- `--archive`：从包里解出 `gpuworks` 到 `<dir>/tmp/`，先执行一次它的 `version`，以输出里的 release id 作为目标版本，之后与在线路径相同。解不出、架构不对（先读 ELF 头比对，不靠试运行，装了 qemu binfmt 的主机也拦得住）、跑不起来或报不出 release id（本地构建）的包在停服前就被拒绝；发布包本身原样保留。完整性靠 gzip 自带校验，sha256 请拷到主机后用发布包的 `SHA256SUMS` 自行核对。
- 用 `<dir>/.upgrade.lock` 互斥，并发升级直接失败；备份含数据库、`master.key`、`server.yaml` 和二进制。
- 回滚还原的是升级前快照，升级窗口内的写入会丢；回滚后仍打印备份路径。每次安装/升级追加一行审计到 `<data>/install-history.jsonl`。
- 只升级控制面；节点上的 agent 不会随之自动升级。

## gpuworks restart

| flag | 默认值 | 说明 |
|---|---|---|
| `--dir DIR` | 本二进制所属的安装（所在目录有 `server.yaml` 或 `install.json`），否则默认安装目录（`/opt/gpuworks/server`，macOS 上 `~/gpuworks/server`） | 安装目录 |

- systemd 安装走 `systemctl stop` + `systemctl restart`；nohup 安装先结束旧的守护进程（server 随之退出），再在后台执行 `serve --supervise --config <dir>/server.yaml`。守护进程是 1 号进程（容器的启动命令）时拒绝执行，因为停掉它就是停掉容器；`upgrade` 与重复 `install` 同样拒绝。
- 两种方式都先停服并确认监听地址已释放（仍被占用时像 `upgrade` 一样报出占用者并失败，不再启动），再启动并等待 `/healthz` 报出已安装二进制的版本，别的服务器在该地址上的应答不算恢复；编辑 `server.yaml` 后用它使配置生效。
- `server.yaml` 之前的安装（只有 `install.json`）先转写成 `server.yaml` 再重启，须由已安装的二进制执行；`install` 遇到这种安装同样先转写。
- launchd 安装（macOS）是 `launchctl bootout` 后重新 `bootstrap` plist，plist 改动随之生效。

## gpuworks uninstall

```bash
sudo gpuworks uninstall --dir /opt/gpuworks/server                 # 交互选择是否清理数据，再确认卸载
sudo gpuworks uninstall --dir /opt/gpuworks/server --yes           # 自动化：保留数据
sudo gpuworks uninstall --dir /opt/gpuworks/server --purge --yes    # 自动化：清理数据
gpuworks uninstall --dir /opt/gpuworks/server --purge --dry-run --json
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--dir DIR` | 同 `restart` | 目标安装目录；配置中的外置数据、日志目录也会识别 |
| `--purge` | `false` | 删除本安装的配置、数据（含数据库及 `master.key`）、备份和日志；无法恢复 |
| `--keep-resources` | `false` | 明确选择仅卸载组件，保留实例、节点和云资源；只允许越过资源检查，不越过目录、进程和服务归属校验 |
| `--yes` / `-y` | `false` | 跳过交互选择和最终确认，按显式参数执行 |
| `--dry-run` | `false` | 只打印清理清单，不停服务、不修改文件，无需确认 |
| `--json` | `false` | 标准输出只返回结构化报告；除预览外必须同时传 `--yes` |
| `--quiet` | `false` | 隐藏普通说明；仍会询问和确认，不代替 `--yes` |

卸载前先用 `--dry-run --json` 查看 `checklist`。每项包含稳定的 `id`、`status`、说明、证据细节和处理建议；状态为 `pass`、`warn`、`blocked` 或 `unknown`。预览完成不表示所有检查通过。

清单区分本机服务及自启、监听端口、安装/数据/备份/日志目录与保留项，以及数据库记录中的部署、实例、节点和云资源。数据库记录是持久化状态，不能证明远端进程已停止、GPU 已释放或租机已停止计费。默认遇到活动负载或关键资源信息无法确认时，在停服和删除前拒绝卸载；`--yes` 只跳过交互，不跳过资源检查。确认后、实际停服前还会重新检查资源。

需保留业务、只移除控制面时，明确加 `--keep-resources`。它不会停止推理实例、删除云资源或替代节点卸载。若同时选择 `--purge`，管理记录和凭据也会随数据清理，应先保存后续独立管理这些资源所需的信息。资源检查通过也不替代原有的目录和服务归属保护。

- 终端交互默认保留数据；未显式传 `--purge` 时会先询问是否彻底清理，再展示清单并确认。管道、脚本和 CI 必须传 `--yes` 或 `--dry-run`，否则在修改前失败。
- 停止并确认 server 退出，取消自启，清理所属 systemd unit / launchd plist / nohup PID 和程序文件。所属 expose 公网入口也会关闭并清理其 unit；其他安装的服务保留，无法确认归属时拒绝清理。
- 默认保留配置、数据、备份、日志和服务账号，便于重装恢复；`--purge` 仅清理已知安装资产，未知文件保留。`.upgrade.lock` 与其安装目录作为互斥锁锚点保留，避免并发升级/卸载取得不同的锁文件。
- 清理拒绝危险根目录、用户 home、软链接、挂载点和覆盖安装目录的数据路径；非空数据目录必须能识别出 GPUWorks 数据库或 master key。无法确认归属的服务定义或数据需先单独检查，命令会返回失败并保留尚未清理的文件。
- 服务账号、组和 home 不会自动删除：账号可能已存在或被同机 agent 复用。报告列出保留项；账号 home 如果就是安装目录，目录内已知资产仍按上面的规则清理。
- 远端 agent、模型服务、云租机和 Cloudflare tunnel / DNS 不会自动删除，仍需分别处置；云资源可能继续计费。
- 返回码 `0` 表示所选卸载完成，`1` 表示失败；重复执行可重试。失败会保留尚未安全清理的内容，并返回已完成的清理结果。`--json` 报告包含 `success`、`error`（失败时）、`layout`、`mode`、`service_user`、`purge`、`dry_run`、`already_absent`、`removed`、`kept`，路径以实际安装为准。

## gpuworks stop

| flag | 默认值 | 说明 |
|---|---|---|
| `--dir DIR` | 同 `restart` | 安装目录 |

- systemd 安装走 `systemctl stop`，launchd 安装走 `launchctl bootout`；单元或 plist 留在原处，所以重启机器后服务照常起来，`gpuworks restart` 可立即拉起。nohup 安装结束守护进程（server 随之退出），之后只有 `gpuworks restart` 能拉起。
- 等 server 退出后返回；守护进程是容器 1 号进程时拒绝执行，原因同 `restart`。

## gpuworks expose

给控制台发布一个公网入口：一个数据目录一个入口，只选择提供方。详见[给控制台一个公网地址](../how-to/expose-public-address.md)。

| 子命令 | 专有 flag | 说明 |
|---|---|---|
| `expose up` | `--provider`（默认沿用已保存的选择，否则 `cloudflare-quick`）、`--systemd`、`--origin URL` | 提交配置、启动连接器服务（默认后台进程；`--systemd` 安装 `gpuworks-expose.service`），等待入口就绪后打印地址 |
| `expose status` | `--url-only` | 打印提供方、期望状态、当前状态和地址；`--url-only` 只在就绪时向 stdout 输出地址 |
| `expose down` | 无 | 关闭入口：停掉 cloudflared，连接器服务随之退出（最多等 45 秒）；`--systemd` 时删除 unit |
| `expose rm` | 无 | 先关闭入口，再删除本地配置和观测文件 |
| `expose logs` | `-f` | 打印连接器日志的末尾 200 行，`-f` 跟随 |

每个子命令都接受 `--data DIR`。

行为要点：

- cloudflared 跑在独立的连接器服务（`gpuworks expose run`）里，不归 server 管：server 重启、升级都不影响它，quick 地址不变。`up` 默认把服务作为后台进程启动，机器重启后不会自动恢复；`--systemd`（需 root）把它装成 `gpuworks-expose.service`，开机自启。服务以数据目录属主的账号运行，入口关闭后自行退出。
- 转发目标按 `--origin` > server 启动时记下的监听地址（`<data>/expose/server-origin`）> 安装目录 `server.yaml` 的第一个 console 地址解析。
- 数据目录按 `--data` > 二进制旁边的 `server.yaml` > 默认安装的 `server.yaml`（`/opt/gpuworks/server/server.yaml`，macOS 上 `~/gpuworks/server/server.yaml`）> `./data` 解析，并把命中的规则打印出来。
- `up` 的等待上限是 3 分钟，超时或 Ctrl-C 只结束等待，不撤销请求（`down` 才停）；配置未变且入口仍在时复用现有入口，不会换新的 quick 地址。
- `status --url-only` 在入口不是「连接器服务在运行、revision 匹配、已就绪」时以非零码退出，原因写到 stderr。
- 当前只开放 `cloudflare-quick`：地址是临时的且不支持 SSE，会在 `up`/`status`/`--url-only` 里提示。`--provider cloudflare-named` 目前会报「尚未开放」。
- `rm` 只删本地文件，保留连接器二进制缓存和日志，不改动 Cloudflare 上的任何东西。
- `expose run` 是连接器服务本身，由 `up` 或 systemd unit 启动；内部子命令 `expose connector-host` 是服务持有 cloudflared 的 helper。两者都不需要手动执行。

## gpuworks migrate

对指定 SQLite 执行迁移和内置目录 seed（含系统镜像目录同步，与 server 启动时相同），然后退出。

| flag | 默认值 | 说明 |
|---|---|---|
| `--data DIR` | `./data` | 数据目录 |
| `--db PATH` | `<data>/gpuworks.db` | SQLite 路径 |

## gpuworks reset-password

重置一个用户的密码，直接改数据库，不经过 server。

| flag | 默认值 | 说明 |
|---|---|---|
| `--password PASSWORD` | 无：在终端里提示输入两次（不回显） | 新密码；stdin 不是终端时必填 |
| `--user USER` | `admin` | 用户名 |
| `--data DIR` | 见下 | 数据目录 |
| `--db PATH` | `<data>/gpuworks.db` | SQLite 路径 |

- 不给 `--data` 时依次取：二进制同目录 `server.yaml` 的 `data_dir`、默认安装（`/opt/gpuworks/server/server.yaml`，macOS 上 `~/gpuworks/server/server.yaml`）的 `data_dir`、`./data`，与当前工作目录无关地找到安装的数据库。
- 数据库文件必须已存在：不存在时报出所用路径及其来源并退出，不会新建空库。
- 成功后，若数据库旁的 `initial-credentials` 记录的正是该用户，尝试删除它（其中的密码已失效）；删除失败只打告警，不影响重置结果。
- 以操作员身份运行、不需要 `sudo`；数据库权限不足时按其自身的权限错误失败。
- 密码和 key 不要写进 shell history 或日志。

## gpuworks support-bundle

不经过 server 收集诊断信息，用于 server 起不来或控制台打不开时；内容与控制台的[收集诊断信息](../how-to/troubleshooting.md#无法登录现场时调试采集与收集诊断信息)相同，但节点那部分收不到（manifest 里标为离线）。完成后打印压缩包路径。压缩包在 `--out` 所在的目录组装，不写数据目录，所以 server 所在磁盘写满时，把 `--out` 指到另一块盘照样能收集。

| flag | 默认值 | 说明 |
|---|---|---|
| `--data DIR` | 同 `reset-password` | 数据目录 |
| `--db PATH` | `<data>/gpuworks.db` | SQLite 路径 |
| `--log-file PATH` | `<data>/logs/gpuworks-server.log` | server 日志文件（连同轮转的各代） |
| `--since DURATION` | `1h` | 收集最近这么久；给了 `--from`/`--to` 时忽略 |
| `--from T`、`--to T` | 无 | 时间范围（RFC 3339），两者都要给 |
| `--deployments IDS` | 无 | 逗号分隔的部署 ID：收集其记录与实例日志 |
| `--instances UUIDS` | 无 | 逗号分隔的实例 UUID：收集其日志 |
| `--out PATH` | `./gpuworks-diagnostics-<id>.tar.gz` | 压缩包路径 |

## gpuworks-agent configure

把节点的启动配置写进 `<state>/gpuworks-agent.conf`（权限 `0600`，整份文件替换）。安装脚本在放好二进制后调用它；每次启动 agent 的方式（systemd 单元、nohup 与 `agent-restart.sh`、launchd 任务、手工升级后的重启）都只执行 `gpuworks-agent run --state DIR`，其余配置都从这份文件读。

节点 token 从标准输入的第一行读取，不作为参数，所以不出现在 `ps`、systemd 单元或 plist 里。文件格式与各字段见[安装布局](install-layout.md#agent-配置文件)。

| flag | 默认值 | 说明 |
|---|---|---|
| `--state DIR` | 无（必填） | state 目录，文件写在它下面 |
| `--server URL` | 空 | server 基址，必须以 `http://` 或 `https://` 开头；`agent_dial` 必填，`server_dial` 不需要也不记录 |
| `--connect-mode MODE` | `agent_dial` | `agent_dial` 或 `server_dial` |
| `--listen ADDR` | `server_dial` 时 `0.0.0.0:18800` | `server_dial` 的监听地址，`host:port` 或 `:port`，端口 1–65535 |
| `--driver NAME` | 空（自动探测） | 强制运行时，取值同 `run` |
| `--mount-root DIR` | 空 | 非 root 运行时放 bind-mount 源的目录 |
| `--udocker-bundle DIR` | 空 | 离线 udocker 包目录 |

- 写入前按与 `run` 相同的规则校验：缺 token、`agent_dial` 缺 server、地址或监听端口非法、值里有换行或控制字符都拒绝，不写文件。
- 先写同目录的临时文件再改名，所以同时启动的 agent 读到的是旧文件或新文件，不会读到半份。

## gpuworks-agent run

连接 server 并提供节点能力。标准用法只有一种：`gpuworks-agent run --state DIR`，配置来自该目录的 `gpuworks-agent.conf`（见 [`configure`](#gpuworks-agent-configure)）。

每个配置项按以下优先级取值：**命令行 flag > 环境变量 > 配置文件 > 默认值**。文件不存在不是错误：较早安装的 agent 没有这份文件，仍靠命令行参数或环境变量运行。

| flag | 环境变量 | 说明 |
|---|---|---|
| `--state DIR` | `$GPUWORKS_STATE_DIR` | state、日志和模型目录，也是读配置文件的位置，默认见下 |
| `--server URL` | `$GPUWORKS_SERVER` | server 基址，必须以 `http://` 或 `https://` 开头；`agent_dial` 必需，`server_dial` 不需要 |
| `--token TOKEN` | `$GPUWORKS_TOKEN` | per-node 连接 token（在界面创建待接入节点获得），必需 |
| `--driver NAME` | `$GPUWORKS_RUNTIME_DRIVER`，空则自动探测 | 强制运行时：`docker` / `udocker` / `nerdctl` / `host` / `none` |
| `--connect-mode MODE` | `$GPUWORKS_CONNECT_MODE`，默认 `agent_dial` | `agent_dial`（agent 外连）或 `server_dial`（server 直连 agent 的 `--listen` 端口） |
| `--listen ADDR` | `$GPUWORKS_SERVER_DIAL_LISTEN`，`server_dial` 时默认 `0.0.0.0:18800` | `server_dial` 模式的监听地址 |
| 无 | `$GPUWORKS_RUNTIME_MOUNT_ROOT`、`$GPUWORKS_UDOCKER_BUNDLE` | 只能来自环境变量或配置文件的 `MOUNT_ROOT`、`UDOCKER_BUNDLE`，agent 启动时设给运行时 |
| `--data-lane BOOL` | 无 | 默认 `true`。开专用高吞吐数据通道；只能走单连接的代理后面设为 `false` |
| `--image-pull-max-connections N` | `$GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS` | runveil（udocker）节点拉取镜像时，到每个仓库 host（经代理时为代理）同时打开的连接数上限；`0` 或不设为自动（最多 16），`1` 为只用一条连接，最大 32，超出范围启动失败。应对只允许少量连接或对多连接限流的仓库与代理，不是日常设置，见[限制镜像拉取的连接数](../how-to/configure-pull-settings.md#限制镜像拉取的连接数runveil-节点) |
| `--dcgm MODE` | `$GPUWORKS_DCGM`，默认 `off` | 可选 DCGM SM 占用探针：`off` / `auto`（仅当 `dcgmi` 在 PATH 上才启用） |

- `--state` 默认：`$GPUWORKS_STATE_DIR`，否则 `/opt/gpuworks`（存在时）；macOS 上其次是 `~/gpuworks`（其中有 `gpuworks-agent.conf` 时）、`~/.gpuworks`（存在时）；否则用户配置目录下的 `gpuworks`，最后 `./gpuworks-state`。
- `--data-lane`、`--dcgm`、`--image-pull-max-connections` 不属于配置文件，不被 `configure` 记录。
- `--driver udocker` 选择 runveil：`udocker` 是保留的 driver 名（节点记录与 API 沿用），实现是 native 镜像处理加直接调用内嵌 PRoot 的启动器，界面显示为 runveil。
- `--driver host` 把引擎作为宿主机进程运行，用于 macOS（Apple silicon）节点：Linux 容器在 macOS 上跑在虚拟机里，拿不到 Metal GPU。镜像是 darwin/arm64 的环境包，解包后由 agent 直接启动。
- 不指定 driver 时，macOS 上直接选 host（即使 Docker Desktop 可用）；其他系统按 docker（daemon 可用）> nerdctl > runveil 的顺序探测；都不可用则该节点禁用部署。`--driver` 给了未知值，或给了当前系统用不了的运行时（Linux 上的 `host`、macOS 上的 `udocker`），会记录错误并且不启用任何运行时。
- 日志同时写 stderr 和 `<state>/agent.log`（10 MiB 轮转，保留上一代 `agent.log.1`）；告警及以上的记录会上送 server 的日志中心。日志级别只由环境变量 `GPUWORKS_LOG_LEVEL` 控制，文件与 stderr 都按它过滤；重复的记录会被压缩，见[server 与 agent 自身的日志](../how-to/monitor-and-logs.md#server-与-agent-自身的日志)。
- 合并后的配置按同一条规则校验，不通过时立即报错退出，并提示配置文件的路径：缺 token；`agent_dial` 缺 server；server URL 缺少协议头、带账号、查询串或片段；`server_dial` 的监听地址不是有效端口。`server_dial` 的 agent 只需要 token 和监听地址，从不连接 server，不需要也不使用 server 地址。

## gpuworks-agent smoke

用新二进制向 server 做一次连接握手校验后退出，不启动任何节点能力；agent 自升级在替换二进制前用它校验下载的新版本。

| flag | 默认值 | 说明 |
|---|---|---|
| `--state DIR` | 同 `run` | 读配置文件的 state 目录 |
| `--server URL` | 同 `run` 的取值顺序（flag > `$GPUWORKS_SERVER` > 配置文件） | server 基址（不做协议头校验，只检查非空） |
| `--token TOKEN` | 同上，`$GPUWORKS_TOKEN` | per-node token |

退出码：`0` 握手成功；`3` 10 秒内未完成；`4` server 拒绝（token 无效等）；`1` 其他错误（含缺少参数）。

## gpuworks-agent upgrade

手工升级 agent：从 server 下载新版本，smoke 校验通过后原地替换并重新执行。

| flag | 默认值 | 说明 |
|---|---|---|
| `--state DIR` | 同 `run` | 读配置文件的目录，也传给替换后的 `run` |
| `--server URL` / `--token TOKEN` | 同 `run` 的取值顺序 | 必填（来自 flag、环境变量或配置文件） |
| `--driver NAME`、`--connect-mode MODE`、`--listen ADDR` | 同 `run` | 只在没有配置文件的老 agent 上传给替换后的 `run`；有配置文件时它们已在文件里 |

- 仅支持 Linux；从 `<server>/dl/gpuworks-agent-linux-<arch>` 下载到 `<bin>.new`，10 秒内 `smoke` 通过后才把旧二进制改名 `<bin>.old` 并换上新版本，smoke 失败则旧二进制保持原样。
- `server_dial` 的 agent 不知道 server 地址，也无从下载：手工 `upgrade` 直接拒绝，提示它由 server 推送升级，在节点页「升级 Agent」操作。
- 替换后的 `run` 在配置来自文件时只带 `--state <dir>`；没有配置文件的老 agent 则带 `--server --token --connect-mode --state`，`server_dial` 再加 `--listen`，有 driver 时加 `--driver`。都不携带 `--data-lane`、`--dcgm`、`--image-pull-max-connections`：替换后的进程继承当前环境变量，所以 `--dcgm` 可改用 `GPUWORKS_DCGM`、`--image-pull-max-connections` 可改用 `GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS` 保留；`--data-lane` 没有对应环境变量，手工升级后恢复默认值 `true`。
- 整个过程有 10 分钟预算；同一时刻只允许一个升级。替换后的进程同样要在确认期限内注册成功，否则回滚到 `<bin>.old`。节点页「升级 Agent」对支持推送的 agent 改由 server 经连接推送二进制，不经 `/dl/` 与 `smoke`，见[升级与版本兼容](../how-to/upgrade-and-compatibility.md)。

## gpuworks-agent doctor

默认打印节点环境自检，可用 `--state DIR` 指定 Agent 目录：

- 主机、CPU、内存、根分区容量及其文件系统类型（挂载标记：网络、noexec、只读、容器层）；节点详情里的「诊断」另含模型目录、udocker 目录、agent 目录等路径，并给出运行时初始化失败、`noexec` / 只读挂载、容器层数据的检查结果，见[排障](../how-to/troubleshooting.md#运行时初始化失败与容器节点的文件系统)；
- `docker`、`python3`、`dcgmi` 是否可用及版本；
- GPU：厂商、驱动版本、每张卡的型号/显存/架构/NUMA/健康状态；
- 运行时插件的检测结果、设备注入是否可规划；
- NVIDIA 节点另有一段手工诊断：驱动与 Fabric Manager、CUDA 运行时冒烟、卡间 peer access、多卡 NCCL。

`--connectivity` 选择轻量连接检查，不执行 GPU、CUDA 或 NCCL 自检：

```bash
gpuworks-agent doctor --connectivity --state /opt/gpuworks
gpuworks-agent doctor --connectivity --state /opt/gpuworks --json
```

- 替换为实际 state 目录。配置优先级与 `run` 相同：显式参数 > 本次命令环境 > `gpuworks-agent.conf`。可用 `--server`、`--token`、`--connect-mode` 覆盖；优先从文件读取 token，避免凭据出现在进程参数中。
- 总预算最多 10 秒，复用真实 WebSocket、yamux 和身份握手。DNS、连接、代理 CONNECT、TLS 仅在真实 transport 回调发生时报告；代理端 DNS、连接复用等未观测阶段不补成成功。
- 文本和 JSON 标明 `source=cli`，代表本次命令的环境，不保证与运行服务相同。JSON 复用 `DoctorReport` 的 `report` 与 primitive `facts`（`connection.*`），日志走 stderr，不混入 stdout JSON。
- 退出码 `0` 表示列明的检查成功或合法 `server_dial` 主动外连检查不适用，`3` 为总超时，`4` 为服务端明确拒绝，`1` 为其他失败（含配置无效），`2` 为参数错误。默认环境 doctor 仍需阅读报告，不能只凭退出码判断硬件健康。
- `control=smoke_verified` 只证明这次短握手，`data_lane=unverified` 明确未测 data。不会启动第二个正式 Agent、替换在线 ctrl/data；结束或超时关闭自身连接。旧 Server 的 Smoke 仍可能清除升级错误，新 Server 仅允许正式会话清理，不宣称混合版本零写。
- 历史仅读取该 state 的 `agent.log` 与 `agent.log.1`，总计最多 512 KiB、32 条新版安全事件。`connection.last_failure.*` 是最近一条有效的已记录失败，保留原时间，与本次测试分开。可选 `last_failure.server_url`、`last_failure.retry_at` 仅取该事件原目标与当时计划的重试时间，缺失不补当前值，不能据旧计划判断 Agent 目前仍在等待。旧格式、未记录、轮转或读不到时为未知/不可用，不输出原始日志；同类重复日志聚合，摘要不枚举每次尝试。

合法 `server_dial` 配置需要 token、不需要 Server URL，在节点上不主动拨控制面，应检查反向监听与网络。在线节点可从页面经既有 ctrl 获取运行态，无需 SSH；离线节点需通过节点控制台运行 CLI。

## gpuworks-agent grant-device-access

给非 root 的运行时用户加上加速卡设备节点所属的组，使 runveil 工作负载不必以 root 运行。

| flag | 默认值 | 说明 |
|---|---|---|
| `--user USER` | 无（必填） | 非 root 运行时用户 |
| `--state DIR` | 同 `run` | agent state 目录（runveil 驱动的根） |
| `--create-user` | `false` | 用户不存在时先 `useradd -m` 创建（需要 root） |

- 探测本机加速卡，按与部署相同的规则算出要注入的设备节点，找出该用户缺少的属组；没有需要授权的设备、或已有读写权限时只打印说明并成功退出。
- 需要补组时必须以 root 运行，执行 `usermod -aG <组> <用户>`；改动后需要重启 agent 才会生效。
- 只作用于 runveil（`udocker`）驱动。节点安装脚本在「非 root + 可用 `sudo` + `--driver udocker`」时会自动调用 `grant-device-access --state <state> --user udocker --create-user`。

## gpuworks-agent bench

对 OpenAI 兼容端点做压测（评测页使用同一套内置 Go loadgen，见[运行评测并解读报告](../how-to/benchmark.md)）。

| flag | 默认值 | 说明 |
|---|---|---|
| `--url URL` | 无（必填） | 端点基址 |
| `--model MODEL` | 无（必填） | 模型 id |
| `--api-key KEY` | `$OPENAI_API_KEY` | API key |
| `--concurrency N` | `1` | 并发数（未给 `--concurrency-list` 时使用） |
| `--concurrency-list LIST` | 空 | 并发网格，如 `1,2,4` |
| `--isl N` | `128` | 目标输入长度（未给 `--isl-list` 时使用） |
| `--isl-list LIST` | 空 | 输入长度网格，如 `128,512` |
| `--osl N` | `32` | 目标输出长度（未给 `--osl-list` 时使用） |
| `--osl-list LIST` | 空 | 输出长度网格，如 `32,128` |
| `--requests N` | `0`（自动） | 闭环时每个网格点的请求数；自动为 `并发×2`，agentic 按并发 1/2/4/8 取 4/8/8/16 |
| `--request-rate-list LIST` | 空 | 开环到达率网格（req/s），如 `1,2,4`；给出后代替并发网格 |
| `--burstiness F` | `1` | 开环到达间隔 gamma 分布的形状参数，1 为泊松 |
| `--max-in-flight N` | `0` | 开环在途请求上限，0 为不限 |
| `--point-duration S` | `60` | 开环每个点位的到达时长（秒）；请求数 = 到达率 × 时长，至少 20 |
| `--warmup-requests N` | `1` | 第一个点位前发送并丢弃的预热请求数，0 为不预热 |
| `--request-timeout S` | `600` | 每个请求（含预热）的超时秒数，范围 10–7200 |
| `--ignore-eos BOOL` | `true` | random / agentic 请求带 `ignore_eos`，生成到目标输出长度 |
| `--dataset KIND` | `random` | `random` / `multiturn` / `agentic` |
| `--preset NAME` | `agentic-80k-v1` | agentic 输入预设：`agentic-{10k,20k,40k,80k,128k,256k,512k,1m}-v1` |
| `--turns N` | `13` | agentic 对话轮数 |
| `--stream BOOL` | `true` | 使用流式响应 |
| `--tokenizer REF` | 空 | tokenizer 路径、URL、`hf://` 或 `ms://` 引用 |

- 并发、输入长度、输出长度三个列表逐项组合，运行所有组合（即笛卡尔积）；给了 `--request-rate-list` 时，以到达率列表代替并发列表。
  每个组合是一个负载点，运行后各打印一行汇总：成功/失败数、输出与总 token 吞吐、单用户输出速度 `1000 / TPOT p50`、TTFT p50/p99、TPOT p50、E2E 延迟 p50/p99。
  并发与长度列表元素必须是正整数，到达率是正数。
- 执行与评测页共用同一套引擎，指标口径见[运行评测并解读报告](../how-to/benchmark.md#指标口径)。
- 给了 `--tokenizer` 是 `exact` 模式，否则是 `estimate`；`agentic` 必须提供 tokenizer，且未给 `--concurrency-list` 时默认并发网格为 `1,2,4,8`，输入/输出长度由预设决定。

## gpuworks-agent runveil

在 runveil 节点上排障和导入镜像：直接读写 agent 的 state 目录，不经过 agent，也不需要 Python。

用法：`gpuworks-agent runveil [--state DIR] <命令>`。`--state` 必须写在命令之前，默认同 `run`（含 `GPUWORKS_STATE_DIR`）。

| 命令 | 说明 |
|---|---|
| `ps [-a]` | 列容器（NAME/IMAGE/STATUS/PID/CREATED）；默认只列运行中的，`-a`（`--all`）列全部 |
| `images` | 列镜像（REPOSITORY/TAG/IMAGE ID/SIZE） |
| `load -i FILE REF` | 把 `docker save` 或 OCI 归档（`.tar` 或 `.tar.gz`）以 `REF` 登记为镜像；`-i` 也可写作 `--input`，可放在 `REF` 前后 |
| `exec NAME [CMD [ARG...]]` | 在容器里执行命令，带上工作负载的环境变量、挂载与 GPU；默认 `/bin/sh -l`，退出码透传 |
| `logs [-f] [-n N] NAME` | 打印容器输出；`-n`（`--tail`）取末尾行数，默认全部；`-f`（`--follow`）跟随至容器停止 |

- `NAME` 可以是容器名的唯一前缀；匹配多个时列出候选并失败。
- `ps`/`images`/`logs` 任意能读 state 目录的用户都能执行，结果与 root 相同；`exec` 必须以 root 或 agent 的运行用户（agent 记录该容器 spec 文件的属主）执行。
- `load` 的 `REF` 必填，填镜像页显示的完整镜像引用（如 `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.22-r1`）：平台按这个引用判断节点上有没有镜像，归档里自带的 tag 可以不同，也不会另外登记。不接受 digest 引用。必须以 root 或 state 目录里 `udocker/` 的属主执行，新文件交给该属主，运行用户能读取和替换。不需要停 agent，agent 下一次上报镜像状态（约一分钟）后镜像页显示「已存在」。失败时不登记镜像，已写入的层随之删除。
- `images` 的 SIZE 是节点实际存储的压缩大小；旧 udocker CLI 登记的镜像没有 manifest，ID 与 SIZE 显示为 `-`。
- state 目录里没有 runveil 状态时报错并提示传 `--state`（agent 的 state 目录若由单元里的 `GPUWORKS_STATE_DIR` 指定，这里需要同样设置）。
- 退出码：`0` 成功；`1` 运行错误；`2` 用法错误。

## gpuworks-agent install-service

仅 macOS。把正在运行的这个二进制注册为 launchd 任务 `ai.gpuworks.agent`。要求 state 目录里已有 [`configure`](#gpuworks-agent-configure) 写的配置文件且通过校验，没有或不合法时报错，提示先执行 `configure`。

| flag | 默认值 | 说明 |
|---|---|---|
| `--state DIR` | 无（必填） | 配置文件所在的 state 目录，也是任务的工作目录；`agent.stderr.log` 写在这里 |
| `--user USER` | 无 | LaunchDaemon 以哪个用户运行；以 root 执行时必填，且不能是 root |

- 任务的 `ProgramArguments` 只有 `<二进制> run --state <dir>`，server、token、连接方向、监听地址和 driver 都不进 plist，由 agent 启动时读配置文件。
- 以 root 执行时写 `/Library/LaunchDaemons/ai.gpuworks.agent.plist`（system 域），删除该用户自己的 LaunchAgent，并把 state 与二进制目录中属于其他账户的文件交给该用户；否则写 `~/Library/LaunchAgents/ai.gpuworks.agent.plist`（`gui/<uid>` 域），用户没有登录会话时报错。
- plist 权限 0600；随后 `bootout` 旧任务、等 launchd 释放后 `bootstrap`，直到 `launchctl print` 连续数秒报同一个运行中的 pid 才退出 0。launchd 拒绝新任务时恢复原 plist；agent 起不来时报错并附上 `agent.stderr.log` 末尾几行。

## gpuworks-agent uninstall

| flag | 默认值 | 说明 |
|---|---|---|
| `--state DIR` | Linux 优先取 systemd unit 的 state，无 unit 时同 `run`；macOS 取 launchd 任务里的 state 目录 | agent state 目录 |

先以 `--dry-run`（或 `--dry-run --json`）查看清单；确认安装与 state 归属、服务/进程、自启、运行时负载候选、GPU 观测和保留目录。共享运行时中仅名称匹配的容器不能证明属于该 agent；缺失的运行时证据标为未知。GPU 查询不支持或失败时不会报告空闲，也不能仅凭进程组长退出证明所有 worker 已退出。

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--dry-run` | `false` | 仅预览检查与清理范围，不停止服务或删除文件 |
| `--json` | `false` | 输出结构化清单和结果；执行卸载时需 `--yes` |
| `--yes` / `-y` | `false` | 跳过交互确认；不跳过资源和归属检查 |
| `--keep-resources` | `false` | 明确保留负载，只移除 agent；不绕过服务/进程归属和清理安全检查 |

默认先要求处理活动负载和关键未知项，再确认并卸载；关键资源在确认后重新检查。GPU 观测未知会明确展示，不能据此承诺 GPU 已释放。二进制、模型和日志仍保留，卸载成功只表示所选组件移除完成。

- Linux 上 unit 的 state 就是这个目录时先 `disable --now gpuworks-agent.service`，再停止相同二进制及显式 `--state` 目录的 `run` 进程（包括 nohup），等待进程退出；随后删除 `/etc/systemd/system/gpuworks-agent.service` 并 `daemon-reload`，最后删除 `<state>/identity.json`、PID 文件和 `<state>/gpuworks-agent.conf`（里面有 token）。unit 属于另一个 state 目录（同一主机上的另一个 agent）时不动它，只停这个目录的进程；unit 里读不出 state 时拒绝执行。
- macOS 上移除 LaunchDaemon 与（执行者的，`sudo` 下是发起 sudo 的用户的）LaunchAgent，并删除各自 state 目录里的 `identity.json` 和 `gpuworks-agent.conf`；存在 LaunchDaemon 而没有 root 权限时报错并以 1 退出。
- 停止、清理失败或超时以非零状态退出；保留二进制、模型、日志和 state 目录。

## 节点安装脚本 install.sh

server 在 `/install.sh` 提供的引导脚本，添加节点时界面生成 `curl … | sh -s -- --server … --token …` 形式的安装命令；接入方式对比见[接入、升级和卸载节点](../how-to/connect-nodes.md)。同一份脚本也由 SSH 向导（Linux 与 Mac）和[离线包](../how-to/connect-nodes.md#离线包)运行：SSH 向导把它和 agent 二进制一起上传，用 `GPUWORKS_AGENT_BINARY` 指向上传的二进制执行，不再有单独的 SSH 启动命令。

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `--token TOKEN` | 无 | 无（必填） | per-node token，脚本不内置集群级秘密 |
| `--server URL` | 无 | 无 | server 基址，脚本不内置任何地址。只有 `agent_dial` 必填；`server_dial` 不需要，也不会记录。没有 `GPUWORKS_AGENT_BINARY` 又没有 `--server` 时，没有可下载的二进制，直接报错 |
| `--connect-mode MODE` | `GPUWORKS_CONNECT_MODE` | `agent_dial` | 同 `run` |
| `--listen ADDR` | `GPUWORKS_SERVER_DIAL_LISTEN` | `server_dial` 时 `0.0.0.0:18800` | 同 `run` |
| `--driver NAME` | `GPUWORKS_RUNTIME_DRIVER` | 空（自动探测） | 同 `run` |
| `--mount-root DIR` | `GPUWORKS_RUNTIME_MOUNT_ROOT` | 空 | 运行时 bind-mount 源的根目录 |
| `--udocker-bundle DIR` | `GPUWORKS_UDOCKER_BUNDLE` | 空 | 离线 udocker 包目录 |
| `--state DIR` | `GPUWORKS_STATE_DIR` | 同 `--install-dir` | state 目录，配置文件写在这里 |
| `--install-dir DIR` | `GPUWORKS_INSTALL_DIR` | `/opt/gpuworks`；macOS 上见下 | agent 二进制所在目录 |
| `--supervisor MODE` | 无 | `systemd` | 谁来保持 Linux agent 运行：`systemd`（有 systemd 且有权写单元时注册 `gpuworks-agent.service`，否则 nohup）、`nohup`（从不动 systemd 单元）、`keep`（沿用主机上这个 agent 现在的方式：本节点的单元在则 systemd；没有单元也没有 `agent-restart.sh` 则 systemd；其余 nohup）。其他值报错；macOS 忽略 |
| `--no-systemd` | `GPUWORKS_NO_SYSTEMD=1` | 关 | 等同 `--supervisor nohup`：不注册 systemd 服务，nohup 后台运行；不动主机上已有的 `gpuworks-agent.service` 和其他目录里的 agent |
| `--clean-existing` | 无 | 关 | 与 nohup（`--no-systemd` 或 `--supervisor nohup`）同用：有 root/免密 sudo 时把已有的 `gpuworks-agent.service` `disable --now` 并删除；界面不生成这个参数 |
| `--no-sudo` | 无 | 关 | 即使 `sudo` 可用也以调用者自己的权限安装。SSH 向导在 `sudo` 无法占用所选安装目录、回退到用户可写的目录时传它 |

- 按 `uname -m` 从 `<server>/dl/gpuworks-agent-linux-<amd64|arm64>` 下载，先写临时文件再原子替换，下载失败时旧二进制保持可用；`GPUWORKS_AGENT_BINARY` 指向一个已有文件时改为复制它（SSH 向导与离线包用这个方式，节点不必能访问 server）。不认识的参数被忽略。
- 放好二进制后，脚本用 [`gpuworks-agent configure`](#gpuworks-agent-configure) 把 server、token、连接方向、监听地址、driver、mount-root 和 udocker-bundle 记进 `<state>/gpuworks-agent.conf`（token 经标准输入），之后所有启动方式都是 `gpuworks-agent run --state <state>`。
- 非 root 时只在 `sudo -n` 免密可用的情况下提权；有 root 权限且 systemd 在运行则写 `gpuworks-agent.service`（`ExecStart=<二进制> run --state <state>`，`Restart=always`、`KillMode=process`，重装不杀推理容器）并启动，否则 nohup 后台运行并把 PID 写到 `<state>/gpuworks-agent.pid`。
- 写 systemd 服务时会停掉 PID 文件里由先前 nohup 安装启动的 agent，并删除 PID 文件与 `agent-restart.sh`。
- nohup 分支同时写 `<state>/agent-restart.sh`（`0700`，每次安装重写）：无 systemd 的主机上没有任何进程监督 agent，崩溃后靠这份脚本按 `nohup <二进制> run --state <state>` 拉起；配置在文件里，脚本不含 token，也不依赖安装时的环境变量。
- 替换已有的 agent 时，脚本先读出它属于哪个 server：这次用 systemd 且单元已存在时取单元所指 state 目录里的配置文件（没有配置文件的老 agent 取单元命令行里的 `--server`），读不到再取本次 state 目录的配置文件。与本次 `--server` 不同就在 stderr 打一行 `WARNING`，照常安装；`server_dial` 的安装没有 `--server`，也按这条比较。
- 非 root + 免密 `sudo` + `--driver udocker` 时，安装后自动执行 [`grant-device-access`](#gpuworks-agent-grant-device-access) 授权 `udocker` 账户。
- macOS：只接受 Apple silicon 与 macOS 15 及以上；下载 `gpuworks-agent-darwin-arm64`（`GPUWORKS_AGENT_BINARY` 同上），写好配置后交给 [`install-service`](#gpuworks-agent-install-service) 注册 launchd 任务。没有免密 `sudo` 但有终端时由 `sudo` 在终端里要密码，装成 LaunchDaemon；都没有时装成 LaunchAgent。未指定 `--install-dir` 时，无论有无管理员权限，目录都是 `~/gpuworks`（执行安装的用户的主目录，`sudo` 下取 `SUDO_USER` 的）；早先的安装在 `~/gpuworks`、`/opt/gpuworks` 或 `~/.gpuworks` 留有 agent（`gpuworks-agent.conf` 或 `gpuworks-agent` 二进制）时，按此顺序沿用找到的目录。`--install-dir`、`--state` 中加了引号的 `~`、`~/x` 展开为执行安装的用户的主目录。同机的控制面默认在 `~/gpuworks/server`，两者互不覆盖。

## 其他 CLI

这些工具不随发布包提供，从源码运行：`go run ./cmd/<名字> …`。

### gpuworks-dist

生成并核对发布通道的目录树（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)），由 `scripts/publish_dist.sh`（`make dist-publish`）
与 CI 的 `release-publish` 调用；上传交给 rclone，本工具不持有存储凭据。

| 子命令 | 说明 |
|---|---|
| `assemble -channel C -dist DIR -out DIR [-commit SHA] [-built-at TIME] [-release-id ID]` | 读 `DIR` 里的 `gpuworks-<id>-<os>-<arch>.tar.gz`（所有包须是同一个 release id，不读两个构建 job 各自的 `release.json`），连同本次检出内嵌的镜像目录与推理模板目录包写成新目录树，用环境变量 `GPUWORKS_DIST_SIGNING_KEY`（`-key-env` 可改）里的 ed25519 种子签名 `<C>/index.json`，并写 `<C>/latest`、`<C>/manifest.json`（与镜像目录同一文件）与 `releases/<id>/SHA256SUMS`；`-out` 已存在时拒绝 |
| `release-id -dist DIR` | 打印 `DIR` 里发布包的 release id（规则同 `assemble`） |
| `adopt -dir DIR -release-id ID` | 核对从某个源拷回的 `releases/<id>/`：包与 `SHA256SUMS` 逐个一致、都属于 `ID`，打印包内 `INSTALL.txt` 记录的构建时间；不一致时非零退出 |
| `verify -channel C -source URL … [-release-id ID] [-index FILE] [-full]` | 逐个源读取并验签索引，核对 release id，并对每个文件发 HEAD 核对大小；`-full` 改为下载并核对 sha256；`-index` 改用本地尚未发布的签名索引核对各源上的文件（不读源自己的索引），用于切换通道前。任一源失败即非零退出 |
| `newer -channel C -built-at TIME [-source URL …]` | 有源在服务构建时间晚于 `TIME` 的同渠道版本时打印它并以 0 退出，没有时以 1 退出，其他错误以 2 退出；发布脚本据此跳过乱序完成的旧流水线 |
| `prune [-keep N] [-protect ID …] < listing.json` | 读 `rclone lsjson -R --files-only <remote>/releases` 的输出，打印可以删除的版本目录：版本 tag（`vYY.M.PATCH`）的目录永不删除，其余（nightly）目录保留最新 N 个（默认 30）与受保护的版本 |

### gpuworks-image-manifest

从 gpuworks-runner 构建矩阵（`build/runner-images/matrix.yaml`）生成系统镜像目录
manifest（schema 3）。同一份输出既是 server 内嵌的种子 `internal/server/imagecatalog/catalog-seed.json`，也随每次发布进入
[发布通道](#gpuworks-dist)。格式与导入规则见[镜像目录](image-families.md#manifest-格式)。

| flag | 默认值 | 说明 |
|---|---|---|
| `-in PATH` | 无（必填） | 构建矩阵 `matrix.yaml` |
| `-out PATH` | `-`（stdout） | 输出 manifest JSON |
| `-registry REPO` | `gpuworks/gpuworks-runner` | 写进 `image_ref` 的规范仓库 |
| `-date YYYY-MM-DD` | 当天（UTC） | `catalog_version` 的日期部分；`catalog_version` = `<日期>.<内容 hash 前 8 位>` |
| `-verify PATH` | 无 | 不生成，改为检查该 manifest 里每个引用都能完整拉取，见下 |

每个矩阵条目产出一个版本 `<registry>:<id>-<revision>`，`registry_sources` 为 quay.io、
swr.cn-southwest-2.myhuaweicloud.com、registry.zstack.ai 三个镜像仓下的同名 tag；引擎、引擎版本、framework 与
framework 版本从 id 解析，架构取自条目的 `platforms`（缺省用矩阵默认值）。id 不合文法、矩阵里声明的
`ENGINE_VERSION`/`FRAMEWORK_VERSION` 与 id 不一致、或出现不支持的平台组合时报错退出。输出按系列名、再按
`image_ref` 排序，可重复 diff；单测守护内嵌种子与矩阵生成结果逐字节一致。

`-verify` 是发布前检查：manifest 先按 server 的规则解析，然后对每个版本的 `image_ref` 与全部
`registry_sources`，匿名取 tag、index 列出的每个平台 manifest（跳过 `unknown/unknown` 的构建证明），再 HEAD
每个 config 与层 blob；镜像仓的平台 manifest digest 必须与规范引用一致（SWR 会把 OCI index 改写成 manifest
list，index digest 不同属正常，平台 digest 不变）。

任何一处取不到、不一致或要求凭据，逐条打印 `FAIL <ref>: <原因>`
并以 1 退出。拦的是用户拉取时才会撞上的两类事故：镜像仓删掉了某 tag 仍在引用的平台 manifest（SWR 删除共用
平台 manifest 的另一个 tag 时会这样），以及目录写了一个从没发布或已删除的版次。

Docker Hub 的 manifest GET
计入匿名拉取限额而 HEAD 不计，所以它放在最后、只做 HEAD，内容按 digest 取自其他镜像仓。

两个 make target：

| target | 作用 |
|---|---|
| `make image-manifest` | 重新生成 `internal/server/imagecatalog/catalog-seed.json`；可用 `IMAGE_MATRIX=<路径>` 换矩阵、`IMAGE_MANIFEST_DATE=<YYYY-MM-DD>` 固定日期 |
| `make image-manifest-verify` | 对已提交的种子跑 `-verify` |

### template-probe

推理模板目录的 L1/L2/L3 校验工具，输出 JSON。子命令的完整 flag 以 `go run ./cmd/template-probe <子命令> -h` 为准：

| 子命令 | 用途 | 主要 flag |
|---|---|---|
| `l1` | 只读模型仓库元数据，估算权重大小并对照镜像能力快照 | `--source`（`huggingface` / `modelscope`，默认 `huggingface`）、`--model`、`--revision`、`--endpoint`、`--token-env`、`--image-digest`、`--architectures`、`--quantizations`、`--max-context-length`、`--index-metadata-only`、`--list-weight-files-fallback`、`--timeout`（30s） |
| `l2-command` | 由模型 `config.json` 推导引擎启动命令 | `--engine`（`vllm`/`sglang`）、`--model`、`--config`（必填，`-` 读 stdin）、`--argv-json` |
| `l3` | 通过 server 部署模板并跑验收用例，结束后清理部署 | `--server`、`--deployment-json`、`--case-json`（三者必填）、`--management-token-env`、`--api-key-env`、`--timeout`（45m）、`--cleanup-timeout`（5m）、`--poll-interval`（2s）、`--keep-deployment` |
| `classify` | 把引擎失败日志归类为确定性签名 | `--log`（`-` 读 stdin）、`--include-forward` |
| `alignment` | 比较完整探测与截断探测的结果是否一致 | `--full-status`、`--full-signature`、`--truncated-status`、`--truncated-signature` |
| `calibrate` | 由硬件校准记录生成校准夹具 | `--input`（`-` 读 stdin）、`--output` |

token 一律通过 `--*-env` 指定的环境变量名传入，不出现在命令行。

### gpuworks-catalog-model-config

维护推理目录包里每个模型的 `model_config`（显存估算所需的 `config.json` 子集）。默认只报告差异。

| flag | 默认值 | 说明 |
|---|---|---|
| `--catalog PATH` | `internal/server/inferencecatalog/seed/catalog` | 目录包根目录 |
| `--fallbacks PATH` | `cmd/gpuworks-catalog-model-config/fallback-sources.json` | 各模型的回退来源（镜像仓库、子目录或手写形状） |
| `--write` | `false` | 写回 `model.yaml`，否则只报告 |
| `--verify` | `false` | 用已发布的权重索引反向核对已有块的层数与注意力类型 |
| `--only MODEL_ID` | 空 | 限定单个模型 |
| `--jobs N` | `8` | 并行读取仓库数 |

- 受限（gated）仓库需要 `export HF_TOKEN=…`，未设置时提示并把这类仓库当作 401；产品本身不带 token 取元数据，所以预制模型必须自带估算事实。
