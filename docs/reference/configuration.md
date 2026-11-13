# 配置参考

## Server 配置文件 server.yaml

server 的配置只有一个来源：安装目录下的 `server.yaml`（`gpuworks install` 写入，单元文件和
nohup 命令只有 `gpuworks serve --config <dir>/server.yaml`）。放在安装目录而不在数据目录，
是为了数据库恢复到另一台机器时不会带着旧机器的地址。

**正常安装不通过环境变量配置 server。** 面向运维的例外有
`GPUWORKS_ADMIN_PASSWORD`（首次创建 admin 账户时提供初始密码）和 `GPUWORKS_USAGE_REPORT=off`（强制关闭使用情况上报）；旧安装另有下述兼容例外。

例外是
`server.yaml` 之前的老安装：那时的单元用 `GPUWORKS_ADDR`、`GPUWORKS_DATA_DIR`、
`GPUWORKS_TRUSTED_PROXIES` 三条 Environment 传配置，新版本在命令行与 `server.yaml` 都没有提供
对应值时仍读这三个变量作桥接，让老版本 `upgrade` 换上来的新二进制照常启动；此后新二进制的
`upgrade`、`install` 或 `restart` 会把 `install.json` 里的设置转写成 `server.yaml`、重写单元
并删除 `install.json`，重写后的单元不再携带这些变量。

命令行参数可以临时覆盖文件里的对应值（开发、救援用），被覆盖的监听
地址在控制台里只读并注明原因。

服务方式、服务用户和版本号不重复保存在配置文件中：分别根据是否存在指向本二进制的 systemd 单元、
单元的 `User=`、已安装二进制的版本输出来确定。安装与升级记录在 `<data>/install-history.jsonl`。

```yaml
listen:                    # 示例地址：管理网 192.0.2.10、节点网 198.51.100.10、服务网 203.0.113.10
  console:                 # Web 控制台 + 管理 API
    - 192.0.2.10:8800
  agent:                   # 节点接入；不写则同 console
    - 192.0.2.10:8800
    - 198.51.100.10:8800
  gateway:                 # 推理 API /v1；不写则同 console
    - 203.0.113.10:8800
data_dir: /opt/gpuworks/server/data
log:
  file: /opt/gpuworks/server/logs/gpuworks-server.log
  max_size: 10MB
  keep: 20
  level: info              # debug / info / warn / error
trusted_proxies: [127.0.0.1/32, "::1/128"]
metrics_retention: 72h
session_idle_timeout: 24h  # 管理会话空闲期限
session_max_lifetime: 168h # 从登录起计算的绝对期限
udocker_bundle_source: ""
ssh_proxy: ""              # socks5://host:port，SSH 到节点经此代理
update_channel: release    # release（正式发布版）/ nightly（滚动预览版）；不写则跟随构建
auto_check_updates: false  # 关闭自动检测新版本；不写即开启
usage_report: false        # 不发日报，自动检查不带节点数/GPU型号但仍带安装标识与版本；不写即加入
```

| 键 | 默认 | 说明 | 覆盖参数 |
|---|---|---|---|
| `listen.console` | 首启检测 | Web 控制台与管理 API 的 `IP:端口` 列表 | `--addr`（替换整个 listen 节，所有功能用这一个地址） |
| `listen.agent` | 同 console | agent 长连接、安装脚本、agent 下载；安装命令指向这里 | 同上 |
| `listen.gateway` | 同 console | OpenAI 兼容 `/v1` | 同上 |
| `data_dir` | `./data` | 数据库、master.key、指标与日志库、推理目录包等 | `--data`（数据库路径另可 `--db`） |
| `log.file` / `max_size` / `keep` | `<data>/logs/gpuworks-server.log`、10MB、20 | 服务自身日志的文件与轮转（`max_size` 用 KB/MB/GB，1024 进制）；留空的项取默认值 | `--log-file`、`--log-max-size`、`--log-keep` |
| `log.level` | info | 日志级别：`debug` / `info` / `warn` / `error`，其他值按 info | `--log-level` |
| `trusted_proxies` | 空（install 写入 `127.0.0.1/32, ::1/128`） | 允许提供 `X-Forwarded-For` / `-Host` / `-Proto` / `-Port` 的代理；公网入口（`gpuworks expose`）启用期间另外信任来自本机的连接，不必列出（[ADR 0049](../adr/0049-expose-trusts-same-host-connector.md)） | `--trusted-proxies` |
| `tls_cert_file` / `tls_key_file` | 空 | 原生 HTTPS 的 PEM 证书链和私钥路径，必须同时设置。非维护监听使用 TLS 1.2 以上；要求显式监听 IP，不支持通配地址。本机 `127.0.0.1` / `::1` 维护入口保留 HTTP，见[生产部署](../how-to/deploy-production.md#原生-https) | — |
| `outbound_restrict_private_hosts` | `false` | 管理节点 HTTP 出站的私网严格模式；默认关闭，普通私网及本机回环无需白名单。设为 `true` 后，仅允许 `outbound_allow_private_hosts` 授权的私网目标。保存后下一次请求生效；特殊元数据、链路本地和保留地址始终拒绝 | — |
| `outbound_allow_private_hosts` | 空 | 严格模式下的私网例外：精确主机名、IP 或 CIDR；关闭严格模式时普通私网及本机回环不需要此列表。保存后下一次请求生效，无需重启。禁止通配域名；特殊地址始终拒绝。平台管理员可在设置页或管理 API 修改这两个字段，也可由主机管理员直接编辑文件 | — |
| `storage_probe_roots` | `/opt/gpuworks/models`、`/opt/gpuworks/cache`、`/mnt/gpuworks` | 不再限制存储路径：新建、修改存储与探测可使用这些目录根之外的路径。server 启动时仍按它检查已有存储，路径（含缓存目录与按节点路径）不在其中时记一条 warn。重启生效 | — |
| `storage_denied_paths` | `/etc`、`/proc`、`/sys`、`/dev`、`/boot`、`/bin`、`/sbin`、`/lib`、`/lib64`、`/usr`、`/run`、`/var/run`、`/var/lib/docker`、`/var/lib/containerd` | 存储路径与缓存目录禁止使用的目录，按路径分段匹配并包含子目录；设置后整体替换内置清单，`[]` 表示不禁用任何目录，不写则用内置清单。每项须是绝对路径，不能是 `/` 或含 `.`、`..` 段。路径格式规则（绝对路径、无 `.`/`..`、非 `/`、节点上不经符号链接）不受此项影响。只能编辑文件，重启生效；只约束本次提交的路径，已保存的路径不会因清单变化被拒绝编辑，见[使用存储](../how-to/storages.md) | — |
| `metrics_retention` | 72h | **只在首次启动时**作为指标原始样本与容器日志保留时长的初值（Go duration，非法或非正值按默认 72h，限定在 24 小时–30 天）；之后以设置页「数据保留」为准（`PUT /api/v1/data-retention`，[ADR 0055](../adr/0055-long-term-statistics.md)），两者不一致时启动日志打一条 warn。清理每小时一次、分批执行（样本每批 5 万行，日志边界日每批 5000 行） | `--metrics-retention` |
| `session_idle_timeout` | 24h | 管理会话无真实操作后的失效期限（Go duration，非法或非正值取默认值） | — |
| `session_max_lifetime` | 168h（7 天） | 从登录起计算的绝对期限，操作不会延长（Go duration，非法或非正值取默认值） | — |
| `udocker_bundle_source` | 空 | server 本机的 udocker 离线包（目录或压缩包）；SSH 安装请求带 `udocker_bundle`（节点上的解压目录）时，server 把它上传并解压到那里，节点服务的 `GPUWORKS_UDOCKER_BUNDLE` 指向该目录 | — |
| `ssh_proxy` | 空 | 到节点的 SSH（含 agent 上传）走的 SOCKS5 代理 | — |
| `update_channel` | 空（跟随构建：master 构建为 `nightly`，`v*` tag 构建为 `release`） | 检查更新、官方目录更新与升级读哪个发布渠道：`release`（正式发布版）或 `nightly`（滚动预览版）。设置页「版本与更新」切换渠道时写入本键并立即生效；`gpuworks upgrade` 不带 `--channel` 时也读它。发布通道之前的版本不认识这个键、会拒绝启动：降级到那些版本前先删掉这一行（`gpuworks upgrade --archive` 降级时旧版本起不来会自动回滚） | `gpuworks upgrade --channel` |
| `auto_check_updates` | 空（开启） | 是否自动检测新版本：开启时打开控制台会触发检查（同时拉取发布方的[通知](usage-report.md#发布方通知)），`false` 时 server 不自动检查，只在设置页点「检查更新」时读取版本索引与通知。自动检查总会附带安装标识、server 版本与 agent 版本分布（用于统计装机数与版本），节点数与 GPU 型号只在加入改进计划（`usage_report`）时附带，见下文「更新检查报告什么」。设置页「版本与更新」的开关写入本键并立即生效，开启时删除该键。只有关闭时才写入；更早的版本不认识这个键、会拒绝启动，降级前先删掉这一行 | — |
| `usage_report` | 空（加入） | 是否加入[用户体验改进计划](usage-report.md)：每天向发布方发送一份匿名日报，并在自动检查更新时附带节点数与 GPU 型号（见下文「更新检查报告什么」）。`false` 时不发日报、检查更新不再附带节点数与 GPU 型号；自动检查仍会附带安装标识与版本，要完全不发送还需关闭 `auto_check_updates`。设置页「用户体验改进计划」的开关写入本键并立即生效，开启时删除该键。**私有化交付在 server 首次启动前写入 `usage_report: false`**（首次启动会立刻发送一份）；环境变量 `GPUWORKS_USAGE_REPORT=off` 同样强制关闭。只有关闭时才写入；更早的版本不认识这个键、会拒绝启动，降级前先删掉这一行 | — |

### 管理会话

管理网页使用服务端持久化会话与 HttpOnly Cookie。会话是否有效同时受空闲期限和绝对期限约束：

- 真实交互由前端每分钟至多报告一次活动，业务写请求也会保活；只读轮询和 SSE 心跳不延长空闲期限。
- 浏览器休眠后，只要空闲与绝对期限都未到，仍可直接使用会话。
- 修改上述两个配置后重启生效；新的最长期限从新登录起计算。
- 修改或重置密码撤销该用户全部登录会话；退出只撤销所提交的登录凭证。平台 API Token 与推理 API key 按各自的生命周期管理。

HTTPS 由反代终止时，必须在 `trusted_proxies` 中配置实际反代地址，并由反代正确转发 Host 与 Proto；反代监听非默认端口时，还要转发带端口的 Host（Nginx 用 `$http_host`）或 `X-Forwarded-Port`，见[生产环境部署](../how-to/deploy-production.md#反代配置要点控制台)。浏览器管理写请求检查 Origin；受信任的 HTTPS 转发头同时用于设置 Secure Cookie，未受信任的转发头会被忽略。

登录按客户端地址与账户分别限速：记录 5 次失败后返回 429 `LOGIN_THROTTLED`，`Retry-After` 表示下一次允许尝试的秒数；后续失败按 1 秒到 15 分钟指数退避，空闲 30 分钟后忘记失败记录。成功登录只清除该账户的记录，不清除共享客户端地址对其他账户的失败记录。未知账户同样处理，伪造的非可信转发头不能切换来源地址。记录数和并发密码校验均有上限。状态保存在当前 server 进程，多副本入口还应在反代配置共享限流。

### 节点间中继的资源限制

模型和镜像的中继共用当前 server 进程的预算，在 `server.yaml` 配置，重启后生效：

```yaml
peer_relay:
  rate_limit_kib_s: 65536
  burst_kib: 64
  no_progress_timeout: 2m
  confirmation_timeout: 5m
```

速率与突发容量使用整数 KiB（1 KiB = 1024 字节）。省略或写 `0` 使用默认值，不表示无限速；
默认合计 64 MiB/s、突发 64 KiB，最多两个中继同时执行，每个节点最多参与一个。
任务设置的较低限速仍生效；直连使用原来的任务限速。预算计算 server 实际写出的内层 TLS 字节，
两个方向共用预算，不把同一份数据的接收与发送重复算成两份模型进度。

超时使用 Go duration（例如 `30s`、`2m`）；省略、空值或 `0` 使用默认值。
无进展超时不计算中继自身等待带宽的时间。确认超时从 server 已写完源端输出并观测到 EOF 后开始，
等待目标验证及 ACK；只收到源端“发送结束”回报不会提前启动计时。
负数、非整数容量、溢出、非法时长、未知键和重复键会让启动失败。

中继容量繁忙时任务按到达顺序排队，每次最多等 1 分钟，仍未轮到时保留所选来源并重试，重试不丢失排队位置。当前任务的路径、阶段、密文字节与速率可在
模型或镜像的传输详情查看；历史事件进入日志中心，聚合指标进入已有监控指标目录。
这些预算与实时计数属于当前 server 进程，多管理节点不共享一个全局带宽池。

### 监听规则

- 地址必须是 `IP:端口` 字面量（不接受主机名）；`server.yaml` 里出现未知键会让 server 拒绝启动，避免拼错的键被悄悄忽略。
- 只绑定列出的地址，不监听 0.0.0.0 后再在程序里过滤；需要全开就显式写 `0.0.0.0:8800` 或 `[::]:8800`。
  通配地址不能与同端口的具体地址并列（内核拒绝同时绑定）；`127.0.0.1`、`::1` 例外，会被通配地址吸收。
- `127.0.0.1` 常驻，承载全部功能，端口取配置里出现的每个端口：安装、升级、健康检查和同机 agent 都从这里连。
  `127.0.0.1` 以外的回环地址（如 `127.0.0.2`）按普通地址处理。
- 相同 `IP:端口` 合并成一个监听、按路径分流；某地址不承载的功能返回 404：
  - 控制台：Web UI、`/api/v1/*`（不含 `/api/v1/agent/connect`）、playground；
  - 节点接入：`/api/v1/agent/connect`、`/install.sh`、`/dl/*`；
  - 推理 API：`/v1`、`/v1/*`；
  - `/healthz` 所有监听都应答。
- 首次启动时 `listen` 为空：按默认路由检测本机地址（没有默认路由则取第一个处于 up 状态、非回环、非虚拟网卡且非链路本地的 IPv4），
  以 `<IP>:8800` 写回文件；仍找不到就启动失败，提示安装时用 `--addr` 指定。
- 列出的地址有任何一个绑定失败，server 不启动（不会只开一部分功能）。

### 在控制台修改监听地址

设置页的「监听地址」卡片默认只显示控制台监听地址（只配了它时，改它等于改全部），节点接入、推理 API
在「高级设置」里。保存时校验：地址必须是本机已有地址（通配和回环地址除外）；当前浏览器访问所用的地址必须
仍在控制台监听地址中（防止把自己锁在外面）。保存只改写 `listen` 一个键，文件里
其他键和注释保留；之后需要重启才生效：

- systemd 安装：卡片上的「立即重启」让 server 自行退出，由 `Restart=always` 拉起，不需要 sudo；
- nohup 安装：在服务器上执行 `gpuworks restart --dir <安装目录>`。

改错地址导致连不上控制台时，直接编辑 `server.yaml` 再 `gpuworks restart`；本机 `127.0.0.1` 始终可用。

「监听地址」只表示 server 在本机绑定哪些地址；节点、用户和客户端实际用哪个地址连过来是他们自己的
选择——域名、反代或 NAT 转发到监听地址都可以，server 不关心也不记录。改节点接入监听地址会影响
已装 agent：它们只会一直拨打安装命令里那个地址，该地址不再转发到新监听地址时就会离线，直到转发
修好或重装。详见[配置节点网络](../how-to/configure-networks.md)里的拓扑说明。

### 仅命令行的调优参数

以下参数没有配置文件键，默认值即生产取值，只在测试或排障时通过命令行调整：`--proxy-max-body-bytes`
（默认 256 MiB，`/v1` 请求体上限，超过 1 MiB 的请求体暂存到 `TMPDIR`，见
[生产环境部署](../how-to/deploy-production.md#网关容量与主机调优)）、`--recovery-enabled`（true）、
`--recovery-grace-period`（2m）、`--recovery-initial-interval`（30s）、`--recovery-backoff-multiplier`（2）、
`--recovery-max-interval`（10m）、`--recovery-restart-wait`（1m）、`--stale-check-interval`（30s）、
`--stale-after`（90s）、`--server-dial-reconnect-interval`（10s）。

### 首启管理员与密钥

- 管理员密码：`gpuworks install --password`、环境变量 `GPUWORKS_ADMIN_PASSWORD` 或交互输入，写入
  `<data>/initial-credentials`（权限见下文），首次启动创建 `admin` 账户时读取。首次启动时若设置了
  `GPUWORKS_ADMIN_PASSWORD`，它优先于该文件（适合容器等不经 install 的部署）；都没有时随机生成并写进
  同一文件。只在创建账户时读取，之后改它不影响已有账户。单元文件从不携带密码。
- 主密钥：`<data>/master.key`（首次启动生成，权限见下文），加密云厂商凭据等；生产应显式备份。

新建或修改管理员密码须为 8–128 个 Unicode 字符、最多 512 字节，不能与用户名相同，并拒绝常见可猜口令、整段重复及序列；不强制大小写或符号组合，允许长口令短语。未指定时生成 24 字节随机值的十六进制口令。此策略不阻止已有账户继续登录。

敏感文件默认仅所有者可读写（`0600`）。明确绑定本安装二进制和非 root 服务账户的 Linux systemd 安装可使用可信服务组（`0660`）；该组成员等价于拥有平台凭据访问权。macOS 与独立运行默认不向普通用户组授权。启动、安装和升级会收紧存量凭据、数据库及其 sidecar、已识别备份；临时数据库快照仅所有者可读。无法保护文件时拒绝继续，按错误路径修复所有者与服务组后重试。生成口令不写入服务日志。

- 模型下载镜像站：在设置 → 拉取设置里配置站点规则，不再有启动参数（`--model-endpoint` 只留给进程内测试夹具：每个站点一条指向该端点、不回源、用于管理节点的站点规则）。

## 拉取代理与出站访问

模型下载和镜像拉取使用的代理在设置 → 拉取设置 → 代理里配置，不再有「网络」设置、节点网络覆盖（`network`、`network:<nodeID>`、`/settings/network`、`/nodes/{uuid}/network`，后者返回 410）。代理是有序条目，字段、选择方式和 `no_proxy` 语法见[配置镜像与模型拉取](../how-to/configure-pull-settings.md#代理)：

| 字段 | 说明 |
|---|---|
| http_proxy / https_proxy | 代理地址 |
| no_proxy | Go `NO_PROXY` 语义：`*`、IP/CIDR、域名含子域名、`.` 或 `*.` 前缀只匹配子域名、可带端口；loopback 与 localhost 永不走代理 |
| 用户名 / 密码 | 密码加密存储，不回显 |
| 适用节点 | 节点标签选择器 |
| 用于管理节点出站访问 | 管理节点使用这条 |

一台节点用第一条满足适用条件的条目，不叠加；没有匹配时用 agent 自己的环境变量。模型下载地址（镜像站、内网代理、禁止回源）用[来源规则](../how-to/configure-pull-settings.md#站点规则模型)，镜像从哪里拉由[拉取设置](../how-to/configure-pull-settings.md)决定，部署不选择来源。

### 出站 HTTP 代理策略

出站 URL 的主机名只接受 ASCII；国际化域名请先转换为 Punycode（例如 `bücher.example` 写为 `xn--bcher-kva.example`），`outbound_allow_private_hosts` 中的域名也使用该形式。原始非 ASCII 主机名会在 DNS 查询前被拒绝；URL 路径和查询参数仍可包含中文等 Unicode 文本。

管理节点的用户可配置 HTTP 来源默认不限制普通私网及本机回环，使用内网模型镜像站、registry、AI 端点或 OTLP collector 不需要新增白名单配置。连接前仍检查全部 DNS 回答，并将连接绑定到已校验 IP；重定向的每一步重新校验，特殊元数据、链路本地和保留地址始终拒绝。URL 凭据、IPv4 映射、DNS 重绑定或代理二次解析不能绕过这些目标检查。

平台管理员可在**设置 → 安全与访问 → 出站访问策略**查看和修改策略。默认关闭**限制私网主机访问**；开启后，在**允许访问的私网主机**中每行填写一个精确主机名、IP 或 CIDR。关闭开关会保留已填写的白名单。点**保存**将两个字段写入当前 `server.yaml`，并从下一次请求或重定向生效，无需重启。

在开启严格模式时，HTTP 模型目录、社区仓库浏览、镜像手填归档来源、AI 服务、Collector、凭据登录测试、用于管理节点的模型站点规则和从 server 执行的外部评测，都可在来源地址旁勾选**自动加入该主机的白名单**。新增和编辑使用相同授权操作。勾选本身不保存；点击保存、解析或测试等原操作时，平台先重新检查目标，再将可授权的当前主机追加到已有名单，并继续原操作。未勾选不会修改名单；更换主机后需重新勾选。

这份授权作用于管理节点访问该主机的所有功能，独立于本次资源是否保存成功。平台保留其他名单条目，发现其他管理员已修改策略时拒绝覆盖；请核对当前策略后重新操作。无管理权限、只读配置、未能解析或永久禁止的地址不能在表单中授权。只读检查不连接来源，不代表凭据有效、目录可读或节点能够下载。计算节点直连与浏览器打开链接不使用这份管理节点白名单；来源规则的节点连通测试也不验证管理节点权限。

也可由主机管理员直接编辑文件：将 `outbound_restrict_private_hosts` 设为 `true`，再用 `outbound_allow_private_hosts` 授权必要的主机或网段。例如，以下配置开启严格模式：

```yaml
outbound_restrict_private_hosts: true
outbound_allow_private_hosts:
  - models.example.internal
  - 10.20.30.0/24
```

`outbound_restrict_private_hosts` 的切换以及 `outbound_allow_private_hosts` 的新增与撤销，均在配置保存后的下一次管理节点 HTTP 请求或重定向时生效，无需重启 server；已开始读取的响应体继续传输。模型元数据、镜像归档与中转、社区目录、发布通道、AI 和遥测共用这项运行时策略。此规则只热更新这两个出站字段，不会让同文件中的监听地址等启动配置同时生效。

热读取配置文件的大小上限为 1 MiB。正常配置中不写 `outbound_restrict_private_hosts` 就使用默认值 `false`；这与配置文件本身超限、缺失、无法读取或 YAML 无效不同：后者临时拒绝普通私网与本机回环，公网访问继续遵循原有安全策略。修复文件后，下次请求重新使用有效策略。平台管理员可通过上述设置页或管理员专用的 `GET` / `PUT /api/v1/server/outbound-policy` 管理这两个字段；主机管理员仍可直接编辑文件。保存保留文件中的其他配置和注释，不会创建另一份数据库策略。若其他管理员或文件编辑已改变配置，旧页面保存会被拒绝；先重新加载并核对当前策略，再保存。未使用 `server.yaml` 启动时页面只读；文件读写失败时按页面提示处理。特殊元数据、链路本地及保留地址始终禁止，关闭严格模式或添加白名单也不能放行。

HTTP/HTTPS 代理需要支持向目标端口的 CONNECT（HTTP 来源也包括 80），以保留原始 Host / TLS SNI 并固定目标 IP；不支持时访问失败，不回退到不安全的解析路径。此规则不替代云平台、SSH 和已注册 agent 的专用管理连接策略。

管理节点（server）自己的出站访问使用第一条勾选了「用于管理节点出站访问」的代理：社区镜像 Tag 查询、模型元数据与目录预览、模型图标获取、发布通道检查和下载、目录 URL 登记，以及镜像归档的预览、大小查询和中转与凭据的测试登录。agent 的下载读取该节点自己匹配到的条目。
修改配置后新请求使用新代理，已开始的传输保留原路线。社区 Tag 的成功结果仍沿用缓存期限。

保存成功表示代理配置已持久化，向节点下发配置在后台完成；离线节点重连后读取最新配置。
向单个节点的控制流写入一次配置时，最多等待 30 秒；写入失败只关闭对应节点会话，由重连恢复配置。
此限制不包含等待数据库或全局状态构建并发名额的时间。

- HTTP 与 HTTPS 分别使用对应代理；只配置其中一个时，两类请求都使用它。
- 选定代理时使用该代理，不再继承进程的 `NO_PROXY` / `ALL_PROXY`，以条目的 `no_proxy` 为准；agent 会自动把 loopback 与 server 地址加入 `no_proxy`。代理不可用时返回错误，不自动直连。
- 管理节点没有勾选的条目、或节点没有匹配的条目时，保留标准环境代理行为，包括 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`。
- 配置的代理地址无效时请求失败，不静默回退；错误信息不包含代理凭据。

nerdctl、udocker 节点的镜像拉取每次按节点匹配的代理条目，立即生效（命令行拉取同样遵守 `no_proxy`）。Docker 镜像由 daemon 拉取，CLI 环境变量不能覆盖
daemon 的代理：agent 把节点代理写进 `/etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf` 并 `daemon-reload`，
不重启 dockerd；节点详情的「拉取代理」卡片显示是否已生效和「重启 dockerd」按钮（需要 root 或免密 sudo），
见[配置镜像与模型拉取](../how-to/configure-pull-settings.md#dockerd)。

代理与来源规则是不同设置。云厂商 API、AI 助手、SSH 代理及 agent 管理连接保留各自现有配置，
不因拉取代理修改而切换网络路线。

旧的 `model_endpoint` 设置和 `default_model_source` 设置已删除，`PUT /settings/default_model_source`
返回 410；从旧版本升级时全局 URL 改写、代理按[升级说明](../how-to/configure-pull-settings.md#从旧版本升级)转成站点规则和代理条目。

## 镜像目录与镜像拉取

**系统镜像目录与推理模板目录的在线更新**来自发布通道（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)）：发布源是代码里的常量
（阿里云 OSS `https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com`、华为云 OBS `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`、
Cloudflare R2 `https://dl.gpuworks.ai`），渠道由 `server.yaml` 的 `update_channel` 决定（见上表，默认跟随构建）。

检查由 server 发起而不是浏览器：打开登录页时浏览器调用 `POST /api/v1/updates/prefetch`（不需登录，不返回结果）让 server 在后台开始检查，
登录后页头读 `GET /api/v1/updates`；server 对同一渠道最多每小时向外检查一次
（失败的检查 5 分钟后可重试），设置页「检查更新」立即重新检查；server 启动和后台定时都不联网。

server 同时请求三个源的签名索引
（每个源连接 10 秒、等响应头 15 秒），用第一个验签通过的；文件按索引的大小与 sha256 校验，失败或 30 秒没有新数据就换源。

请求优先使用管理节点的出站代理（拉取设置里勾选的第一条代理），未配置时继承 `HTTPS_PROXY` / `HTTP_PROXY` 环境变量。不能访问任何源的环境改用「上传 manifest」与「上传目录包」，见
[镜像目录](image-families.md#更新目录)。

**更新检查报告什么。** 自动检查（`auto_check_updates` 开启）向三个发布源请求签名索引时，在 URL 上带一组查询参数，发布方从对象存储的访问日志里统计装机数与版本分布；参数就是下面这些，没有别的。`id`、`v`、`agent` 每次自动检查都带，`nodes`、`online`、`gpu` 只在加入用户体验改进计划（`usage_report` 开启）时带：

| 参数 | 含义 |
|---|---|
| `id` | 安装标识：用 `master.key` 做密钥对固定字符串求 HMAC-SHA256 后取前 8 字节的十六进制，单向，推不出 master.key，用于统计装机数，并让同一安装每小时一次的检查只算一个 |
| `v` | server 版本 |
| `nodes` / `online` | 节点总数 / 当前在线节点数 |
| `agent` | 可重复，`<agent 版本>:<节点数>`；尚未连接过的节点记为 `unknown` |
| `gpu` | 可重复，`<GPU 型号>:<卡数>`；只算节点自己拥有的卡 |

`agent` 与 `gpu` 各最多 16 项，超出的合并为 `other`，名称最长 64 字节。不含主机名、IP、用户名、部署或模型信息。只发给内置的三个发布源：命令行 `--source` 或自建站点收到的是不带参数的普通请求。参数随检查发出，所以频率就是检查的频率（同一渠道每小时最多一次）。关闭 `usage_report` 后只剩 `id`、`v`、`agent`；关闭 `auto_check_updates` 后什么都不发送，手动「检查更新」也从不带参数。改进计划的日报另见[用户体验改进计划](usage-report.md)。

**镜像的拉取地址**不再是一个设置项，由[拉取设置](../how-to/configure-pull-settings.md)决定：设置 → 拉取设置里的
来源规则（`/api/v1/pull/source-rules`，镜像规则与模型站点规则同表）、同步组（`/api/v1/pull/sync-groups`）、代理（`/api/v1/pull/proxies`）与凭据（`/api/v1/pull/credentials`；旧的 `/api/v1/pull/registry-credentials*` 返回 410）。
`PUT /api/v1/settings/image_registry_rewrites`、`image_registry_mirrors` 及镜像加速预设设置返回 410 `SETTING_REMOVED`。
系统镜像不再内置第三方 Docker Hub 加速器；官方镜像在 Docker Hub、quay.io 与华为云 SWR 各有一份，由节点按可达性排序。
候选顺序和失败处理见[排障](../how-to/troubleshooting.md#镜像拉取失败)。来源规则对所有镜像生效，包括推理模板为自己的镜像声明的来源（`image_sources`）。

**`image_community_catalog`**（`PUT /api/v1/settings/image_community_catalog`）覆盖或追加镜像页「社区镜像」的仓库卡片，
与内置卡片按 `repo` 合并（同名替换，新的追加）：

```json
{"repos":[
  {"repo":"registry.example.com/team/vllm","registry":"oci","engine":"vllm","framework":"cuda",
   "siblings":["registry.example.com/team/vllm-dev"]}
]}
```

| 字段 | 说明 |
|---|---|
| `repo` | 仓库路径，必填且不重复 |
| `registry` | `docker-hub`、`quay` 或 `oci`；省略时按主机推断（无主机为 Docker Hub，`quay.io/` 为 quay，其他为 OCI） |
| `upstream_api` | 可选，覆盖 tag 列表第一页的地址，必须是 `https://` |
| `engine` | 必填，添加镜像时预填的引擎 |
| `framework` | 添加镜像时预填的框架 |
| `siblings` | 同一张卡片下并列的其他仓库，各自列 tag，继承本卡片的引擎与框架 |

`schema_version` 可省略（存为 `3`）或写 `3`；不带 `registry`/`siblings` 的旧 `2` 版文档仍被接受，其他值报 `COMMUNITY_CATALOG_SCHEMA_INVALID`。

## 部署策略

设置项 `deployment`（`PUT /api/v1/settings/deployment`，仅管理员）是整个集群的部署边界，存为一个 JSON 对象；
请求体叠加在当前值上，只改其中出现的键，叠加后整体校验，越界回 400 且不写入。

| 键 | 出厂值 | 范围 | 含义 |
|---|---|---|---|
| `pending_timeout_sec` | 600 | 5–86400 | 权重已就位后仍放不下的部署，等多久报为失败（不是排队上限） |
| `start_timeout_sec` | 1800 | 60–86400 | 自动启动健康检查预算的下限，结合已知引擎等待上限可向上扩展（最多 86400 秒）。超时判这次启动失败并按重试规则重建。设置页「部署」卡片编辑它；单个部署可用 `start_timeout_sec` 另设（部署表单「运行环境」的「启动超时」） |
| `event_retention_days` | 180 | 1–3650 | 事件时间线保留天数 |
| `event_retention_bytes` | 50 GiB | 64 MiB–1 TiB | 事件时间线保留字节数，与天数先到者为准 |

旧版本存下的值缺少新键时按出厂值读取；存储的值无法解析或越界时整体回退到出厂值。

## 环境变量

环境变量只服务于 agent 和安装脚本；server 不把环境变量当作配置来源（例外是上文的 `GPUWORKS_ADMIN_PASSWORD`、强制关闭上报的 `GPUWORKS_USAGE_REPORT=off` 和老安装桥接变量）。变量分五类：server 读取的变量、面向运维的 agent 变量、其他工具读取的通用变量、测试专用变量、内部实现细节。

### Server 读取的变量

| 变量 | 默认 | 作用 | 出处或相关命令 |
|---|---|---|---|
| `GPUWORKS_ADMIN_PASSWORD` | 无 | 首次创建 admin 账户时的初始密码；`gpuworks install` 也按 `--password` > 该变量 > 交互 > 随机 的顺序使用它。只在创建账户时读取 | `gpuworks serve`、`gpuworks install` |
| `GPUWORKS_ADDR`、`GPUWORKS_DATA_DIR`、`GPUWORKS_TRUSTED_PROXIES` | 无 | 仅为 `server.yaml` 之前的老单元保留：命令行与 `server.yaml` 都没提供对应值时才生效，`install` 接管后不再使用 | `gpuworks serve` |

### Agent 环境变量

下列变量由 agent 二进制或随它分发的节点安装脚本读取。带「布尔」的变量把 `1`、`true`、`yes`、`on`（不区分大小写）视为开启，其他值视为关闭。

agent 的连接配置按 **命令行 flag > 环境变量 > `<state>/gpuworks-agent.conf` > 默认值** 的顺序取值。
配置文件由 `gpuworks-agent configure` 写入，见[安装布局](install-layout.md#agent-配置文件)。安装脚本会把配置写进文件，
因此节点上通常不再设置对应的环境变量。下表中 server、token、连接方向、监听地址、driver、mount-root、
udocker-bundle 对应的变量，只在需要临时覆盖文件中的值时使用。

| 变量 | 默认 | 作用 | 出处或相关命令 |
|---|---|---|---|
| `GPUWORKS_SERVER` | 无 | server 基址，作为 `--server` 的默认值；覆盖配置文件的 `SERVER`。`server_dial` 的 agent 不用它 | `agent run`、`smoke`、`upgrade` |
| `GPUWORKS_TOKEN` | 无 | per-node token，作为 `--token` 的默认值；覆盖配置文件的 `TOKEN` | 同上 |
| `GPUWORKS_STATE_DIR` | `/opt/gpuworks`（目录存在时），macOS 上其次是 `~/gpuworks`（其中有 `gpuworks-agent.conf` 时）、`~/.gpuworks`（目录存在时），否则 `<用户配置目录>/gpuworks`，再否则 `./gpuworks-state` | state、日志和模型目录，作为 `--state` 的默认值 | `agent run`、`upgrade`、`uninstall`、`runveil`、`grant-device-access`；`install.sh` |
| `GPUWORKS_RUNTIME_DRIVER` | 空（自动探测：docker > nerdctl > runveil） | 强制运行时：`docker`、`udocker`、`nerdctl` 或 `none`；`udocker` 即 runveil（界面显示名），名称为兼容已有节点保留；未知值会使该节点不启用任何运行时 | `agent run`、`upgrade`；`install.sh` |
| `GPUWORKS_CONNECT_MODE` | `agent_dial` | `agent_dial`（agent 外连）或 `server_dial`（server 直连 agent 的 `--listen` 端口） | `agent run`、`upgrade`；`install.sh` |
| `GPUWORKS_SERVER_DIAL_LISTEN` | `0.0.0.0:18800` | `server_dial` 模式 agent 的监听地址（`host:port` 或 `:port`）；只在 `server_dial` 时有意义。已有节点保留各自记录的监听地址，不随默认值改变 | 同上 |
| `GPUWORKS_DCGM` | `off` | 可选 DCGM SM 占用探针：`auto`（不区分大小写）时，只要 `dcgmi` 在 PATH 上就每个采集周期多调用一次 `dcgmi dmon`；其他值（含 `off`）不启用 | `agent run`（`--dcgm`） |
| `GPUWORKS_LOG_LEVEL` | `info` | agent 日志级别，只认精确的 `debug`、`warn`；其他值（含 `info`、`error`）都按 info。日志写 stderr 和 `<state>/agent.log` | 所有 `gpuworks-agent` 子命令 |
| `GPUWORKS_RUNTIME_MOUNT_ROOT` | docker 驱动沿用 `<state>`；其他驱动 `<state>/runtime-mounts` | agent 暴露为容器 bind-mount 源目录的运行时资产根目录（assets、pre-start、dgpu、overlays、kv-disk）；也可由配置文件的 `MOUNT_ROOT` 提供 | `agent run`；`install.sh`（`--mount-root`）写入配置文件 |
| `GPUWORKS_UDOCKER_LAUNCHER` | `runveil` | runveil 节点的容器启动器：默认由 agent 直接调用内嵌的 patched PRoot，节点无需 Python；`udocker`（不区分大小写）恢复 udocker CLI 启动器（需要 python3 ≥ 3.6）。镜像拉取、导入、创建两种启动器都走 native 实现 | runveil 驱动 |
| `GPUWORKS_UDOCKER_DISABLE_NATIVE`（布尔） | 关 | 开启后镜像拉取/导入/创建改走 udocker CLI（需要 python3），只作为 native 不支持的镜像（如 schema1 清单）的显式逃生口；native 失败不会自动回退 | runveil 驱动 |
| `GPUWORKS_UDOCKER_BUNDLE` | `<state>/offline/udocker`（从 agent 内嵌的离线包解压） | udocker CLI 离线包根目录，只在需要 udocker CLI 时使用。SSH 安装请求带 `udocker_bundle` 时由安装脚本（`--udocker-bundle`）写入节点的配置文件 `UDOCKER_BUNDLE`（包内容取自 `server.yaml` 的 `udocker_bundle_source`） | runveil 驱动 |
| `GPUWORKS_ALLOW_ONLINE_UDOCKER_INSTALL`（布尔） | 关 | 离线包缺失或不可用时，允许在线（pip）安装 udocker CLI；默认直接报错，避免悄悄改变来源 | runveil 驱动 |
| `GPUWORKS_INSECURE_REGISTRIES` | 空 | runveil 驱动允许不校验证书的 HTTPS 或明文 HTTP 访问的 registry，逗号分隔的 host、host:port 或 CIDR，相当于 dockerd 的 `insecure-registries`；回环地址（`127.0.0.0/8`、`::1`、`localhost`）始终视为 insecure。对这类 registry 先试不校验证书的 HTTPS，对方只讲 HTTP 时改用 HTTP；其他 registry 只走校验证书的 HTTPS | runveil 驱动 |
| `GPUWORKS_UDOCKER_DISABLE_TAR_CACHE`（布尔） | 关 | 开启后拉取镜像时不再把大层（≥512 MiB）预先展开成 tar 缓存（`<state>/udocker/tarcache`，上限 48 GiB，并为节点保留 20 GiB 空闲），磁盘紧张的节点可用 | runveil 驱动 |
| `GPUWORKS_UDOCKER_UNPACK_DEBUG`（布尔） | 关 | 开启后记录逐层解包耗时，排查镜像创建慢时用 | runveil 驱动 |
| `GPUWORKS_PARTITION_ASSETS` | 空 | dGPU 后端运行时资产（NVIDIA 的 `libvgpu.so`、Ascend 的 `libvruntime.so` 等）的额外搜索根目录，优先于默认位置 `<state>/dgpu-runtime`、`<state>/partition-assets` 和 `/opt/gpuworks/dgpu-runtime` | dGPU 注入与资产上报，见[启用和排查 dGPU](../how-to/dgpu.md) |
| `GPUWORKS_INSTALL_DIR` | `/opt/gpuworks`；macOS 上是 `~/gpuworks`（或沿用早先安装的 agent 目录） | agent 二进制的安装目录（`--install-dir` 的默认值） | `install.sh` |
| `GPUWORKS_NO_SYSTEMD` | `0` | 设为 `1` 强制走 nohup/前台回退，不动 systemd 单元 | `install.sh` |

### 其他被读取的通用变量

| 变量 | 作用 | 出处或相关命令 |
|---|---|---|
| `OPENAI_API_KEY` | `--api-key` 的默认值 | `gpuworks-agent bench` |
| `DOCKER_HOST`、`DOCKER_CONTEXT` | docker 驱动只接受本机 unix socket：`DOCKER_HOST` 不是 `unix://` 或 `DOCKER_CONTEXT` 不是 `default` 时，docker 驱动判为不可用，因为 bind mount 和加速卡设备会落到远端机器上 | 运行时自动探测 |
| `ASCEND_HOME_PATH` | 探测昇腾 CANN 版本时优先查找其下的 `version.info` | agent GPU 探测 |
| `HF_TOKEN` | 读取受限（gated）的 HuggingFace 仓库 | `gpuworks-catalog-model-config` |
| `INVOCATION_ID` | systemd 给单元进程设置；server 据此判断自己被 `Restart=always` 监督，允许控制台「立即重启」 | `gpuworks serve` |
| `LC_ALL`、`LC_CTYPE`、`LANG` | 判断 locale 是否 UTF-8，决定 `install` 打印块状还是纯 ASCII banner | `gpuworks install` |
| `GPUWORKS_API_KEY`、`GPUWORKS_URL`、`GPUWORKS_WS_URL` | 只出现在界面生成的 curl/Python 示例里，由用户在自己的 shell 中设置，程序本身不读取 | 推理示例片段 |

### 测试与开发专用变量

下列前缀只用于测试、CI 和开发脚本，不构成支持契约，可能随时改名或删除，不要写进生产服务；各变量的含义在对应测试或脚本里，测试分层与入口见[测试指南](../dev/testing.md)。

| 前缀 | 用途 |
|---|---|
| `GPUWORKS_TEST_*`、`GPUWORKS_TESTENV_*` | 测试夹具与断言开关（如 `GPUWORKS_TEST_STRICT`、`GPUWORKS_TESTENV_LOG`） |
| `GPUWORKS_FAKE_*` | 伪造 GPU 与 recording 运行时（`GPUWORKS_FAKE_GPU`、`GPUWORKS_FAKE_GPU_*`、`GPUWORKS_FAKE_RUNTIME_*`）；agent 会读取它们，仅供进程级测试 |
| `GPUWORKS_FLEET_*`、`GPUWORKS_GATEWAY_*`、`GPUWORKS_LOG_SAMPLE_SECONDS`、`GPUWORKS_LOG_LINES_PER_INSTANCE_PER_SEC` | 大规模 fleet、网关负载、日志中心规模测试的参数 |
| `GPUWORKS_L3_*`、`GPUWORKS_L4_*`、`GPUWORKS_CLOUD_*`、`GPUWORKS_ASCEND_*`、`GPUWORKS_PPU_*`、`GPUWORKS_NVIDIA_*`、`GPUWORKS_DOCKER_*`、`GPUWORKS_VLLM_*`、`GPUWORKS_RUNPOD_*`、`GPUWORKS_KEEP_ON_FAILURE`、`GPUWORKS_SSH_PROXY` | 容器级（L3）与真机/云端（L4）用例的镜像、主机、账号、失败保留等输入 |
| `GPUWORKS_REAL_*`、`GPUWORKS_OBSERVATION_*`、`GPUWORKS_NATIVE_LOAD_*`、`GPUWORKS_UNPACK_BENCH` | 需要真实依赖的单测、基准 |
| `GPUWORKS_UPDATE_*` | 设置后重写 golden/fixture 文件 |
| `GPUWORKS_RELEASE_*`、`GPUWORKS_DIST_DIR`、`GPUWORKS_MAX_*` 及 `scripts/` 下演示脚本里的变量 | 发布打包、兼容性检查和演示脚本（发布见[发布流程](../dev/release.md)） |

### 内部实现细节

以下名字出现在代码里，但不是用户可配置项，不要设置它们：

- `GPUWORKS_EXPOSE_CONNECTOR_SPEC`：连接器服务（`gpuworks expose run`）拉起公网入口连接器辅助进程（`gpuworks expose connector-host`）时，用环境变量传递启动请求，避免凭据出现在命令行。
- `GPUWORKS_PARTITION_PROBE_BACKEND`、`GPUWORKS_PARTITION_PROBE_QUOTA_MIB`：agent 启动 dGPU 探针容器时注入给探针程序的输入，不是 agent 读取的配置。
- `__GPUWORKS_AGENT_PROCESS_OK__`、`__GPUWORKS_AGENT_PROCESS_WARN__`：云厂商 SSH 安装后检查命令回显的标记字符串，不是环境变量。
