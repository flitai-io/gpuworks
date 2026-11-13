# 控制面安装布局

本页查表用：`gpuworks install` 产生的默认值、目录与文件权限、凭证文件、systemd 单元、备份内容和交互提问的默认值。安装、升级、卸载的步骤见[安装和升级控制面](../how-to/install-server.md)，命令行参数见 [CLI 与安装脚本](cli.md#gpuworks-install)，`server.yaml` 的键见[配置参考](configuration.md)。

## 默认值

| 项目 | 默认 |
|---|---|
| 安装目录 | `/opt/gpuworks/server`；macOS 上是 `~/gpuworks/server`（执行安装的用户的主目录，`sudo` 下取 `SUDO_USER` 的） |
| 监听地址 | `<本机默认路由出口 IP>:8800` |
| 数据目录 | `<dir>/data` |
| 日志 | `<dir>/logs/gpuworks-server.log`，10 MB × 20 代 |
| 备份 | `<dir>/backups`，保留最近 3 组 |
| 服务 | systemd 单元 `gpuworks.service`，以系统用户 `gpuworks` 运行；macOS 上默认不注册服务，以 nohup 后台进程运行（`serve --supervise`），加 `--autostart` 才写 launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`，以执行安装的用户运行 |
| 受信代理 | `127.0.0.1/32,::1/128` |

安装目录是 agent 默认目录（Linux 上 `/opt/gpuworks`，macOS 上 `~/gpuworks`）的子目录：同机同时装控制面和 agent 时，两者的二进制、state 和数据互不覆盖。

`--dir`、`--data`、`--logs` 以及 `install directory` 提问的回答中，开头的 `~`、`~/x`、`~user/x` 展开为对应用户的主目录。

监听地址取本机默认路由出口 IP、端口 8800；找不到默认路由时取第一个非回环、非虚拟网卡的 IPv4。`--addr` 只写 `:端口` 表示绑定 `0.0.0.0`。

## 目录布局

```
/opt/gpuworks/server/
├── gpuworks                        # 安装的二进制（install 拷贝自身，upgrade 原子替换）
├── server.yaml                     # 配置文件：监听地址、数据目录、日志、受信代理等
├── .upgrade.lock                   # 升级互斥锁
├── gpuworks.pid                    # 仅 nohup 模式
├── tmp/                            # 升级时暂存下载包与解出的新二进制
├── backups/<release_id>-<UTC时间戳>.tar.gz
├── logs/gpuworks-server.log[.1..20]
└── data/
    ├── gpuworks.db(+ -wal / -shm)  # 主库
    ├── master.key                  # 凭证加密密钥
    ├── initial-credentials         # 初始密码记录
    ├── install-history.jsonl       # 安装/升级审计
    └── logs.db / metrics.db / ux.db / images/ / catalog-packages/ …
```

`server.yaml` 放在安装目录而不在数据目录：把数据库恢复到另一台机器时，不会带上旧机器的监听地址。

`server.yaml` 和安装目录本身是重复安装和升级的输入：

- `upgrade` 从既有安装中读取升级所需信息，因此不需要重复提供安装参数：
  - **安装目录**：使用 `--dir` 指定的目录。未指定时，使用当前执行的二进制所属的安装目录；无法确定时使用默认目录。
  - **服务方式**：检查是否存在指向本二进制的 systemd 单元或 launchd plist。
  - **服务用户**：读取 systemd 单元的 `User=` 或 plist 的 `UserName`。
  - **当前版本号**：查询已安装的二进制。
- 重复执行 `install` 时保留既有 `server.yaml` 的值，只有命令行显式给出对应参数才覆盖。
  install 只写监听地址、数据目录、日志文件、日志轮转和受信代理这几项；不管理的键（如 `ssh_proxy`、`metrics_retention`）原样保留。

## 权限模型

安装树是服务账户与操作员的共享工作区：

| 对象 | 权限 |
|---|---|
| Linux systemd 目录 | `2775`（组可写 + setgid，新文件继承平台组 `gpuworks`） |
| 普通配置文件（如 `server.yaml`） | Linux 共享安装为 `0664`；macOS 移除组写权限 |
| 数据库、`master.key`、`initial-credentials`、备份包 | 默认 `0600`；明确授权的 Linux 平台组使用 `0660` |
| 二进制 | `0755` |
| `data/install-history.jsonl` | `0600`（独立运行）或 `0660`（可信 Linux 服务组） |
| systemd 单元 / launchd plist | `0644`（plist 归 `root:wheel`，launchd 要求） |

- install 以**操作员身份**运行，只有少数步骤以单独的 `sudo` 命令执行：首次创建 `/opt` 目录、建服务账户、写 systemd 单元、`systemctl`，以及收尾时把树交给服务组。`sudo` 在第一次特权命令时问一次密码，之后凭据缓存期内（通常 15 分钟，取决于主机 sudoers 配置）免问。
- 首次安装时树内的写入（二进制、配置、凭证）还无法直接落盘，会自动走 `sudo` 兜底，收尾统一 chown；之后的安装和升级都直接写入。
- systemd 模式下操作员会被加入平台保留组 `gpuworks`，因此之后 `reset-password` 不需要 `sudo`，`upgrade` 的备份与替换二进制也不需要；`upgrade` 仍会用 `sudo` 执行 `systemctl` 以及收尾的 `chown`/`chmod`。新组在**当前终端**不生效，需要 `newgrp gpuworks` 或重新登录。
- nohup 模式没有特权步骤：进程以调用者身份运行、目录归调用者，唯一例外是首次创建 `/opt` 目录。nohup 进程归安装者——root 装的只有 root 能用 `gpuworks restart --dir <安装目录>` 重启。
- 预检要求：root 或可用的 `sudo`（否则降级为 nohup 并告警；`sudo` 也没有且 `--dir` 不可写时拒绝安装，提示改用用户级目录）、目标端口未被别的进程占用（被当前安装占用除外）、安装目录所在文件系统至少 2 GiB 空闲。

敏感文件默认仅所有者可读写（`0600`）。明确绑定本安装二进制和非 root 服务账户的 Linux systemd 安装可使用可信服务组（`0660`）；该组成员等价于拥有平台凭据访问权。macOS 与独立运行默认不向普通用户组授权。启动、安装和升级会收紧存量凭据、数据库及其 sidecar、已识别备份；临时数据库快照仅所有者可读。无法保护文件时拒绝继续，按错误路径修复所有者与服务组后重试。生成口令不写入服务日志。

授权组固定为平台保留组 `gpuworks`，不跟随自定义服务账户的普通主组；systemd 单元显式写入 `Group=gpuworks`，文件实际 GID 必须与该组匹配。旧默认服务安装可保留已有 `gpuworks` 运维组授权；其他未知安装按仅所有者策略处理。回滚到未包含此修复的旧版本后，旧代码可能再次放宽文件权限，应重新升级修复版本。 预检发现旧文件需要收紧、但当前操作员没有所有者权限时，会在停服前拒绝安装或升级；以文件所有者或 root 重试。提权写入仅用于经过校验的可信目录，目录校验失败时也应以所有者或 root 运行完整命令。

Linux 的组授权会先清理敏感文件上的 POSIX 扩展 ACL。为避免额外授权通过文件替换或 sidecar 继承生效，共享数据库目录、提权写入的可信目录，以及安装收尾将授予组权限的敏感目录链，不接受扩展或默认 POSIX ACL；若错误指出 ACL，应先检查并清理不必要的 ACL，再重试。

## 凭证文件

单元文件是全局可读的，所以初始密码**不会**写进单元文件，而是走 `<data>/initial-credentials`（JSON，`0600` / `0660`，明文）：

```json
{"user": "admin", "password": "...", "created_at": "2026-09-10T09:00:00Z"}
```

install 时的密码优先级：

1. `--password`；
2. 环境变量 `GPUWORKS_ADMIN_PASSWORD`（交互模式下回显并确认，拒绝则继续提问）；
3. 交互模式提问（关闭回显，回车则随机生成；已有 admin 账户时不提问）；
4. `--quiet` 或 stdin 不是终端时，随机生成。

服务首启时创建 admin 账户的优先级是 `GPUWORKS_ADMIN_PASSWORD` > `initial-credentials` 文件 > 随机生成（不经 install、直接 `gpuworks serve` 时可用环境变量指定初始密码），并把最终使用的密码写回该文件。`GPUWORKS_ADMIN_PASSWORD` 只在创建 admin 时读取。该文件记录的账户修改密码后（控制台或 `reset-password`），server 或命令会尝试删除该文件（其中的密码已失效）；删除失败只记告警，需手动删除。记录的是其他账户时不删除。

数据目录已有 `gpuworks.db` 时视为已有 admin 账户：不再解析密码，数据库和账户不会被改动，`--password` 被忽略并提示改用 `gpuworks reset-password`。

## install 的交互提问

输出是终端且未给 `--quiet` 时，install 先打印 banner（终端且 locale 为 UTF-8 时用块状版本，否则纯 ASCII），再逐个提问，回车采用括号里的默认值：

| 提问 | 新安装的默认 | 已有安装时的默认 | 命令行已给出时 |
|---|---|---|---|
| `install directory` | `/opt/gpuworks/server`；macOS 上为 `~/gpuworks/server` | 同左（由 `--dir` 决定目录） | `--dir` 跳过 |
| `listen address` | 探测到的 `<IP>:8800` | `server.yaml` 里控制台监听地址的第一项 | `--addr` 跳过 |
| `start the service automatically` | `yes`；macOS 上为 `no` | 原安装是 nohup 时为 `no`，否则 `yes`；macOS 上原安装有 launch daemon 时为 `yes`，否则 `no` | `--autostart` 或 `--no-systemd` 跳过 |
| `Admin password`（关闭回显，最后一步） | 随机 | 数据库已有 admin 时不问 | `--password` 或 `GPUWORKS_ADMIN_PASSWORD` 已确认时不问 |

- 监听地址只输入端口号时保留默认里的地址、只替换端口；其他输入必须是 `IP:端口`，否则重新提问。
- 自启问 `yes` 等价于 `--autostart`，`no` 等价于 `--no-systemd`，回答只认 `yes`/`y`/`no`/`n`。
- `--quiet` 或 stdin 不是终端时不打印 banner、不提问，全部按默认值走；结束时的摘要仍会打印（里面有管理员凭证）。

## systemd 单元

服务单元通过 `server.yaml` 读取 server 配置，不在单元文件中写入这些配置值或密码：

- `ExecStart=<dir>/gpuworks serve --config <dir>/server.yaml`；nohup 模式跑的是同一条命令行，日志、数据和探活行为一致。
- `User=` 为 `--service-user` 指定的服务账户；`Restart=always`、`RestartSec=3`；`TimeoutStopSec=30`，给进行中的模型与镜像同步留出优雅退出时间。
- `KillMode` 保持默认（control-group）：server 不拥有推理容器，子进程应随它一起退出（agent 的单元则相反）。

只有在有可用 root/`sudo`、`systemctl` 存在且 `/run/systemd/system` 真的存在（容器里装了 systemctl 但没有 systemd 不算）时才写单元；macOS 上只在 `--autostart`（或提问回答 `yes`、重装已有 launch daemon 的安装）时写下面的 launchd plist；都不满足时使用 nohup 后台进程加 PID 文件。

## launchd plist（macOS）

macOS 上 systemd 单元的对应物是 system 域的 launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`，同样只指向 `server.yaml`、不含设置或密码：

- `ProgramArguments` 与单元的 `ExecStart` 相同；`UserName` 是执行安装的用户（`sudo` 下取 `SUDO_USER`），不是 root，也不建专用账户，`--service-user` 在 macOS 上被忽略（显式给出别的账户时告警）；只有 root 本身执行安装时以 root 运行。
- `RunAtLoad` + `KeepAlive`：开机即起、退出 3 秒后（`ThrottleInterval`）再拉起，对应单元的 `Restart=always`；`ExitTimeOut` 30 秒对应 `TimeoutStopSec`；不设 `AbandonProcessGroup`，子进程随 server 退出。
- 环境变量 `GPUWORKS_LAUNCHD=1` 告诉 server 它由 launchd 托管，因此可以在控制台升级、重启（退出后由 launchd 拉起）。
- `StandardErrorPath` 是 `<dir>/logs/gpuworks-server.stderr.log`，只收 server 日志打开之前的输出和崩溃时运行时打印的内容。
  正常日志记录写在轮转的 `gpuworks-server.log` 里；`gpuworks-server.stderr.log` 超过轮转大小上限时，在启动时清空。
- 安装树归运行用户（`chown -R <用户>:<其主组>`），不加入任何组。

## 探活与备份

- install 与 upgrade 启动服务后轮询 `/healthz`，最长 60 秒；探测地址是监听地址对应的回环地址（通配地址用 `127.0.0.1`）。
- nohup 模式下这个等待会在进程已经退出时提前结束：该模式没有监管者，启动留下的进程一旦消失就不会再有人应答，继续等满只是推迟回滚（例如新版本缺少动态库、或拒绝某项配置而启动即退出）。systemd 单元与 launchd plist 都会拉起崩溃的 server，所以那里的 60 秒正是留给重试的，不提前结束。
- 备份是 `backups/<release_id>-<UTC时间戳>.tar.gz`，包含 `data/gpuworks.db`（含 `-wal` / `-shm`）、`data/master.key`、`server.yaml` 和 `bin/gpuworks`，不含模型、镜像等数据目录里的其他内容。恢复会先删掉旧库的 WAL/SHM，避免新版本写入的前滚日志被旧 schema 重放。
- 每次安装和升级向 `<data>/install-history.jsonl` 追加一行审计：动作、结果、release、操作者（`sudo` 下取 `SUDO_USER`）、时间；失败时含 `previous_release_id` 与错误。

## 日志与轮转

server 同时写 stderr 和文件：`<dir>/logs/gpuworks-server.log`，按 10 MB 轮转、保留 20 代（install 的 `--log-max-size` / `--log-keep` 可调；直接跑 `serve` 时对应 `--log-file`，传空串则只写 stderr）。轮转是原生实现，不依赖 logrotate；systemd 部署下 `journalctl -u gpuworks` 也能看到同一份输出。

launchd 部署下，stderr 指向的文件没有轮转机制。server 发现 stderr 是普通文件时，正常日志只写入可轮转的日志文件（见上节）。日志目录不可写时退回 stderr，不影响启动。

日志文件写失败时（例如磁盘已满）停写，记录仍写进 stderr。之后每 30 秒重试一次，成功后先写一行 `log file writes resumed`，带丢弃的条数（`dropped`）、停写的起点（`since`）和原因（`err`），不用重启。agent 的日志文件也是这样。

server 因错误退出（如监听地址被占用）时，原因以 `msg="server exited"` 写进同一份日志；nohup 模式的 stderr 指向 `/dev/null`，这是唯一能看到原因的地方。

## agent 配置文件

这一节说节点上的 agent，不是控制面。agent 的启动配置只记在一处：state 目录（默认 `/opt/gpuworks`，macOS 上是 `~/gpuworks`）里的 `gpuworks-agent.conf`，权限 `0600`，里面有节点 token。它只由 `gpuworks-agent configure` 写（安装脚本调用，见 [CLI](cli.md#gpuworks-agent-configure)），每次安装整份重写。

```
# gpuworks agent configuration, written by `gpuworks-agent configure`.
SERVER=https://gpuworks.example.com:8800
TOKEN=<节点 token>
CONNECT_MODE=agent_dial
LISTEN=
RUNTIME_DRIVER=
MOUNT_ROOT=
UDOCKER_BUNDLE=
```

- 每行 `KEY=value`，值按字面读取，没有引号、转义和变量展开；`#` 开头的行与空行跳过；不认识的键被忽略，所以新版安装脚本写的文件不会挡住旧 agent 启动。
- `SERVER` 只有 `agent_dial` 的节点有；`server_dial` 的节点留空（它从不连接 server）。`LISTEN` 只对 `server_dial` 有意义，默认 `0.0.0.0:18800`。
- 所有启动方式都只执行 `gpuworks-agent run --state <state>`：

| 启动方式 | 命令 |
|---|---|
| systemd 单元 `gpuworks-agent.service` | `ExecStart=<dir>/gpuworks-agent run --state <state>`；单元其余内容（`ExecStartPre`、`KillMode=process`、`Restart=always`、`RestartSec=3`）与配置无关 |
| nohup | `nohup <dir>/gpuworks-agent run --state <state>`；`<state>/agent-restart.sh`（`0700`）是同一条命令的重启脚本，PID 在 `<state>/gpuworks-agent.pid` |
| launchd（macOS） | `ProgramArguments` 为 `<dir>/gpuworks-agent run --state <state>`，plist 权限 0600 |
| 手工 `gpuworks-agent upgrade` 后的重启 | `run --state <state>`；没有配置文件的老 agent 才带完整参数 |

- token 因此不出现在 `ps`、`systemctl cat gpuworks-agent`、plist 或 `agent-restart.sh` 里。
- 要改配置就重新执行安装（或 `configure`）再重启 agent。临时覆盖用命令行 flag 或环境变量（优先级见 [`run`](cli.md#gpuworks-agent-run)）。
- 文件不存在不是错误：较早安装的 agent 没有它，仍从自己的命令行参数运行，升级二进制不会改动它们的启动方式；重新执行一次安装脚本后才迁移到文件。
- `gpuworks-agent uninstall` 同时删除这份文件。
- 安装脚本、SSH 向导的预检查和带外恢复要知道主机上的 agent 属于哪个 server 或哪个 token 时，读的就是这个文件（找不到文件时才读老 agent 的命令行）。设计依据见 [ADR 0051](../adr/0051-agent-launch-configuration-file.md)。
