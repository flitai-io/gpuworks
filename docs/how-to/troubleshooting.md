# 排障指南：按症状定位

本文用于排查节点连接、GPU、模型与镜像、实例启动和推理请求故障。先按现象找到对应小节，
用状态、事件或日志确认原因，处理后重复原操作检查是否恢复。

**前置条件**：能查看相关节点或部署；需要运行诊断命令时能登录对应主机。
重启、复位、修改配置等操作另需相应权限，并先确认对当前服务的影响。
文中的地址、节点或实例 ID、`<jwt>` 均替换为自己的值；`<jwt>` 是管理 API 的登录凭据，不是推理令牌。
节点命令在出问题的节点执行，server 命令在控制面主机执行；示例路径按实际安装目录替换。

## 先看哪里

1. 记下故障时间、节点或部署名称、错误原文。
2. 在下表对应入口查看状态和证据，再到同名症状小节处理。
3. 修复后重复原来的连接、部署或调用，确认目标恢复；保存仍未消除的错误，继续缩小原因范围。

| 看什么 | 在哪里 | 用来回答 |
|---|---|---|
| 节点状态 | 节点页徽标：在线 / 离线 / 连接中（`connecting`） | **只看 agent 连没连上**；加速卡的问题不改变它 |
| 加速卡状态 | 节点**详情页**里的「厂商工具未响应」标记（节点列表不显示），以及加速卡列表里每张卡的分配状态（可分配 / 不可分配等）、健康、归属与占用 | 这张卡能不能用、读数是不是旧的 |
| 节点详情 | 「最近拒绝」行、「带外恢复」卡片、「诊断」按钮（`POST /api/v1/nodes/{uuid}/doctor`，与 `gpuworks-agent doctor` 同一检查） | 为什么被拒、恢复到哪一步、环境是否健康 |
| 实例事件时间线 | 实例详情；API `GET /api/v1/instances/{uuid}/events`（部署级：`GET /api/v1/deployments/{id}/events`） | 卡在哪个状态和阶段、失败原因（`params.detail`） |
| 调度解释 | 部署页；实例响应的 `schedule.explain`；`POST /api/v1/evaluate` 响应的 `plan` | 为什么选这张卡、别的候选为什么被拒 |
| 日志页 | 按实例、级别、来源（`source=instance\|node\|agent\|server`）、关键字检索，见[监控与日志](monitor-and-logs.md) | 引擎输出、`CUDA out of memory` 之类 |
| server 日志 | `gpuworks install` 安装的：`/opt/gpuworks/server/logs/gpuworks-server.log`（macOS 上 `~/gpuworks/server/logs/gpuworks-server.log`）；直接 `serve`：`<data>/logs/gpuworks-server.log`；systemd 下 `journalctl -u gpuworks` | 握手失败、恢复轮次、代理错误 |
| agent 日志 | `<state>/agent.log`（安装默认 state 为 `/opt/gpuworks`，macOS 上 `~/gpuworks`，10 MB 轮转、保留一代 `agent.log.1`）；`systemctl status gpuworks-agent` | 连接、GPU 探测、拉取、启动 |
| 环境自检 | 节点上 `gpuworks-agent doctor` | 运行时、GPU、驱动、设备权限 |

实例状态取值：`pending`、`pulling`、`starting`、`optimizing`、`running`、`removing`、`stopped`、`failed`、
`pending-removal`。`pulling` 期间的阶段（phase）依次可能是 `image_verify`、`image_pull`、`image_extract`，
`image_pull_retry` 表示某个镜像来源失败后换下一个；`starting` 期间是 `container_prepare`、`container_launch`、
`health_check`。

管理 API 的错误形态是 `{"error": "...", "code": "...", "params": {...}}`，`code` 稳定，以 `code` 为准。

## 连接检查的范围与证据

排查时先区分检查覆盖的那一段与真正失败的业务链。记录发起检查的主机、实际目标、代理或网络通道、观测时间，以及最后完成的阶段；没有执行或没有返回的检查应保留为未知。

| 检查 | 能确认什么 | 仍需核对什么 |
|---|---|---|
| 节点 SSH / Agent 进程 | 控制面能登录节点、目标 Agent 进程存在 | 节点到控制面的 DNS、TLS、WebSocket 和认证；进程存在不等于控制会话建立 |
| 控制面公网健康地址 | 发起探测的主机能访问该地址的健康接口 | 其他节点或用户网络，以及登录、WebSocket、流式推理 |
| 引擎启动健康 | 节点本机访问该实例健康接口的结果 | 网关到节点的通道，以及用户到网关的入口 |
| 镜像仓库排名探测 | 节点经实际代理路径访问 registry `/v2/` 是否有响应 | 镜像鉴权、tag、manifest、layer 下载与解压；HTTP 401 也可能表示端点可达 |
| 存储测试 | 报告所列节点在那次测试中的挂载、可读和可写结果 | 其他节点、其他身份和之后的挂载状态 |
| 网络内部测试 | 指定节点对与网络上的 TCP、RDMA 或卡级测试结果 | 未测试的节点对、容器内 NCCL/HCCL、PD 的实际 KV 传输 |

缓存或历史观测有自己的时间，重新打开页面不等于重新探测。公网入口与节点保存地址不同只能提示核对，不能据此判断旧地址无效或自动改连新地址。

### 节点地址与会话

节点长时间连接中时，先看最后上报与最近恢复错误。管理员在节点详情的「连接诊断」可获取只读证据：在线且支持连接诊断的 Agent 经既有 ctrl 返回运行态，无需控制面能 SSH 到节点，也不触发完整 GPU doctor。运行态报告标明来源、采集时间、有效 Server URL、当前控制/数据通道和实际观测到的传输阶段。

本次控制往返成功不等于 data 或业务健康。data `attached` 是连接快照；建立后掉线为 `detached`，等待重连，不自动回落 ctrl；禁用、未协商、首次等待、未知各有含义，不能仅凭“节点在线”判断日志/下载能用。RPC 尝试后不会再串行等待 SSH。Agent 自有短预算；页面取消只停止等待，不代表远端操作已经被取消。

离线或旧 Agent 无法返回本次运行态，页面仍展示保存地址、上报/恢复记录及公网历史，可用 SSH 是旁路。没有 SSH、工具或可达控制会话时，未执行项不是网络已断的证据；从节点控制台运行：

```bash
gpuworks-agent doctor --connectivity --state /opt/gpuworks --json
```

替换实际目录。它复用真实短握手，总预算 10 秒，不运行 GPU 自检，`source=cli` 仅描述本次 shell/flags/env/file；服务进程可能不同。`data_lane=unverified` 明确未验证数据通道。最近有效安全日志失败保留原时间，与本次成功/失败分开；日志旧格式、轮转、级别未记录或不可读时为未知/不可用，不输出原始日志。旧 `smoke --state` 仍可用，但同样不能证明双通道或长时间稳定。

平台保存地址、节点配置快照、运行进程事实、公网连接器历史入口属于不同证据。SSH HTTP 使用 SSH 环境，未必等于 Agent 代理；文件可被 flags/env 覆盖；公网历史 URL 也不保证仍可访问。只有本次真实回调能证明 DNS、连接、代理 CONNECT、TLS 等阶段，代理端 DNS 或无回调不能补成成功。

确认稳定地址后，由操作员明确同步平台记录与节点配置，再按安装方式重启。诊断不会原位迁移已纳管节点的 URL，不应把报告的新入口当成地址修复完成。不要分享完整配置、进程参数、环境、token 或代理认证信息。

### 流式请求与文件传输

推理请求详情描述的是网关的观测。`finish_reason` 是引擎报告的模型结束原因，不等于客户端已收到 `data: [DONE]`。需要逐段判断时，在 **设置 → 诊断** 开启限定范围的调试采集后复现，比较同一请求的 `upstream`、`client` 与 `end` 记录，并使用请求 ID / Trace ID 关联日志。

采集中的 `client` 表示网关写给直接下游的字节；存在第三方网关、负载均衡或客户端 SDK 时，还需它们自己的接收证据。不能用网关写入成功证明最终客户端收到完整回复。

存储文件下载已经发出 HTTP 200 后，中途错误只能中断连接，不能再返回 JSON 错误。保留请求 ID，结合控制面日志中的节点、存储路径、已传字节及失败方向，区分从节点读取中断和向浏览器写入失败。节点间模型复制则使用传输 ID，沿目标接收端、源节点直连、数据确认和目标最终状态逐段核对；两个节点在线不等于它们之间的数据网络可达。

## 节点一直「连接中」或不上线

**现象**：节点页一直是「连接中」，或上线后反复变「离线」；agent 日志出现 `connection ended`、
`reconnecting` 或 `server rejected connection`。

**怎么确认与处理**

1. 在节点详情核对连接方向。**Agent 发起连接**时，在节点执行以下命令；state 不是 `/opt/gpuworks` 时替换路径：

   ```bash
   gpuworks-agent doctor --connectivity --state /opt/gpuworks
   ```

   退出码 `0` 表示成功，`3` 表示 10 秒超时，`4` 表示被 server 拒绝，`1` 表示其他错误。
   命令读取该 state 下的连接配置，也可显式传入 `--server` 与 `--token`。
   **Server 发起连接**时，检查控制面到节点监听端口的路由、防火墙和映射，以及 agent 日志中的握手错误；不要用上述主动连接测试代替。

2. 在节点查看 agent 日志，并按下方原因核对地址、token 或协议版本：

   ```bash
   tail -n 100 /opt/gpuworks/agent.log
   ```

   连接配置位于同目录的 `gpuworks-agent.conf`，其中含 token，不要原样分享。
   需要查看最近的拒绝记录时，在能访问控制面的终端调用：

   ```bash
   curl -s -H "Authorization: Bearer <jwt>" \
     "http://<server>:8800/api/v1/agent/rejects"
   ```

3. 修正后确认节点恢复 **在线**，并留意是否再次断线。

- `GET /api/v1/agent/rejects` 返回最近被拒的连接：`token_prefix`（token 前 8 位）、`reason`、`remote`、`at`；
  只在内存里保留最近 64 条，重启即丢。`bad_token` 对应不到节点，所以只在这里和 server 日志
  （`agent session failed`）里可见；节点详情「最近拒绝」只记录 `proto_mismatch`。
- 收到 reject 后 agent 固定等 5 分钟才重连（普通断线从 1 秒翻倍退避到 30 秒）；systemd 安装的节点修好后可执行 `sudo systemctl restart gpuworks-agent` 立即重连，其他安装方式按对应方式重启 agent。接入方式见[接入节点](connect-nodes.md)。

**可能原因**

1. server 地址从节点不可达或填错。安装命令里的 `--server` 就是 agent 永远拨打的地址，装完记在 `<state>/gpuworks-agent.conf` 的 `SERVER`
   里（`systemctl cat gpuworks-agent` 看不到它，也看不到 token，单元里只有 `run --state <目录>`）；`install.sh`
   会先从该地址下载 agent 二进制（下载不了就直接失败），但装完不验证 agent 能否建立长连接。Server 发起连接的节点没有这一项：
   它们不连接 server，要检查的是 server 能否访问节点的监听端口（安全组、防火墙、端口映射）。
2. token 错误。节点被删除、重新生成 token（`POST /api/v1/nodes/{uuid}/token/regenerate` 会踢掉当前连接）
   后，旧 token 会被拒，reason 为 `bad_token`（detail 为 `node token was not recognized`；空 token 是
   `node token is required`）。
3. 反代或监听没有转发 `/api/v1/agent/connect` 的 WebSocket upgrade，或该地址不在 `listen.agent` 里
   （不承载的功能返回 404，见[配置参考](../reference/configuration.md)）。
4. 协议版本不匹配，reason 为 `proto_mismatch`（detail 形如 `server supports 1-1`），需要升级 agent。
5. `stale_session`：只出现在**数据通道**握手，表示它绑定的控制会话已过期。agent 只打 Debug 日志并退避
   重试，控制通道重连后自愈，见 [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md)；持续出现才需要处理。

时钟偏差**不是** reject 原因：握手里没有时间戳校验，`server_dial` 的 HMAC 只签一次性 nonce，不含时间。
server_dial 节点的密钥不符时 agent 报 `invalid server_dial proof`。

## Kubernetes 节点不加入或不上线

**现象**：从 Kubernetes 集群接入的条目迟迟没有节点出现在节点页；「外部平台」页该集群的状态是「未接入」，超过 5 分钟变成「长时间未接入」；
或节点出现了，但离线、被拒绝，或实例起不来。接入步骤与各项设置见[接入 Kubernetes 集群](connect-kubernetes.md)，本节按症状指到对应位置，不重复其中的说明。
节点已经出现、只是一直「连接中」或反复离线时，先看上一节。

**怎么确认**：在集群里执行下面的命令，`<namespace>` 是接入时的命名空间（默认 `gpuworks-system`），`<agent-pod>` 是 `get pods` 列出的 agent Pod。在「外部平台」页点集群状态旁的「排查」，
给出的是同样的命令和常见原因（长时间未接入时才有这个入口）：

```bash
kubectl -n <namespace> get daemonset,pods -o wide
kubectl -n <namespace> describe pod <agent-pod>
kubectl -n <namespace> logs ds/gpuworks-agent
```

| 症状 | 看什么 | 处理 |
|---|---|---|
| agent Pod 是 `ImagePullBackOff` / `ErrImagePull`，或「长时间未接入」 | `describe pod` 的 Events 里写着拉取哪个镜像、失败原因；401 / 403 是缺拉取凭据或凭据不对 | 节点拉不到所选仓库：改选能访问的仓库（中国内地选华为云 SWR），或填自己的镜像、配镜像拉取凭据，见[接入](connect-kubernetes.md#接入)里「高级」的 agent 镜像仓库、agent 镜像与镜像拉取凭据；保存后重新应用，逐条的操作见[接入公有云托管 Kubernetes](connect-managed-kubernetes.md#没有节点出现时)；离线集群的镜像归档见[离线环境部署](deploy-offline.md#kubernetes-集群的-agent-镜像) |
| 实例 Pod 事件 `RuntimeClass "…" not found`，实例起不来 | `kubectl get runtimeclass` 看集群里有没有条目里填的那个；`kubectl -n <namespace> describe pod <实例 Pod>` 看事件。RuntimeClass 只影响 NVIDIA 的实例 Pod，agent Pod 不用它 | 编辑集群条目，把 RuntimeClass 改成集群里有的名字，或留空由 agent 自己判断；保存后立即对在线 agent 生效，不用重新应用，只影响之后新起的实例 Pod，见[修改集群条目](connect-kubernetes.md#修改集群条目) |
| `get daemonset` 的 DESIRED 是 0 | 节点选择器没有选中任何节点：`kubectl get nodes --show-labels` 核对标签 | 编辑条目修正节点选择器（留空表示所有节点），改动要重新应用才进入集群，见[接入](connect-kubernetes.md#接入)与[修改集群条目](connect-kubernetes.md#修改集群条目)；云账号接入的条目改的是节点池 |
| agent Pod 在运行，但节点不出现，agent 日志反复连接失败 | `kubectl -n <namespace> logs ds/gpuworks-agent` 里的连接错误；节点上能否访问「管理节点接入地址」 | 地址必须是集群节点能解析、能访问的（agent 用节点自己的 DNS，不能写集群内的 Service 名；agent 主动外连，不需要节点入站端口）。地址在条目创建后不能改，要改就删掉条目重建，见[接入](connect-kubernetes.md#接入)；连通性的通用检查见上一节 |
| agent Pod 在运行、已连上 server，但节点不出现，或出现后又消失 | `describe pod <agent-pod>` 的 Events 里有 Warning `GpuworksEnrollRefused`：注册被拒绝，事件里写明原因（`node_taken`、`unknown_token`、`other_cluster`）和处理办法；没有这条事件、日志里也没有拒绝时，多半是节点没有受支持的加速卡，agent 待命不注册 | `node_taken`：同集群的另一个条目占着这个节点，等它在该节点离线超过 15 分钟，或调整两个条目的节点选择器；`unknown_token`：接入令牌被吊销，重新创建条目；`other_cluster`：令牌属于另一个集群。详见[加速卡厂商：自动识别](connect-kubernetes.md#加速卡厂商自动识别)。事件需要 manifest 的 Role 里有 `events` 的 `create`，旧 manifest 重新应用后才有，没有时只是看不到事件 |
| 节点在线，GPU 设备上有「GPU 设备为软隔离」标签 | 节点详情「基础信息」里的标签和下面的原因、命令 | 不是故障：集群用 HAMi 等 device plugin 取代了 NVIDIA 的，NVIDIA 容器工具包又处在 CDI 模式，实例能跑，但 Pod 看得到节点的全部 GPU 设备、只有 CUDA 被 `CUDA_VISIBLE_DEVICES` 限制。需要严格隔离时按标签里的命令改，见[加速卡厂商：自动识别](connect-kubernetes.md#加速卡厂商自动识别) |
| 实例起来了，但经网关访问不通、端口像被「吃掉」，或集群里有 NodePort 服务 | 条目的引擎端口段（默认 `40000-42767`）是否与集群的 NodePort 范围（默认 `30000-32767`，由 API server 的 `--service-node-port-range` 决定）重叠 | kube-proxy 会把发往本机地址的 NodePort 流量转走。编辑条目改引擎端口段，保存后立即生效，已在跑的实例保持原端口、之后分配的端口用新范围，保存响应会列出落在新范围之外的运行中实例（`KUBE_ENGINE_PORTS_OUTSIDE`），见[接入](connect-kubernetes.md#接入)与[修改集群条目](connect-kubernetes.md#修改集群条目) |

修好后确认节点出现在「计算节点」页且在线。以上都对不上时，保留 `describe pod` 的 Events 与 agent 日志，见本页末尾「仍未解决？」。

## 节点掉线后的自愈与 recovery

**现象**：节点从「在线」变「离线」，节点详情出现「带外恢复」卡片。

### 查看恢复进度

在节点详情查看「带外恢复」的阶段、尝试次数、上次探测时间和最后错误，分别对应
`recovery_phase`、`recovery_attempts`、`last_probe_at`、`last_recovery_error`。
只有 `recovery_phase` 非空时才显示这张卡片。

平台在 `--stale-after`（默认 90 秒）没有收到心跳后标记离线，依次尝试：

1. agent 自己重连。
2. server 对 `server_dial` 节点反向重拨。
3. server 经 SSH 带外恢复：重启 agent，等待 `--recovery-restart-wait`（默认 1 分钟）；仍不上线才重装。

SSH 恢复通常执行 `systemctl restart gpuworks-agent`，没有 unit 时用 nohup 启动；属于其他 server 的 unit 按下文处理。
重装沿用原 token，根据 server 保存的节点信息重写 `<state>/gpuworks-agent.conf`，并保留主机当前的 systemd 或 nohup 运行方式。
设计说明见 [ADR 0006](../adr/0006-out-of-band-recovery-ssh-credentials.md)。

### 手动重连

1. 查看最后错误，先处理下方对应原因。
2. 在节点页点击离线节点旁的 **重连**。这是界面上的恢复入口，对应 `POST /api/v1/nodes/{uuid}/recovery/probe`。
3. 等待节点恢复 **在线**；若仍失败，查看更新后的阶段与最后错误。

**按钮和凭据的区别**：

- 已保存 SSH 凭据：立即尝试恢复，忽略等待窗口；仍跳过在线、未接入和无凭据的节点。凭据可通过 `PUT /api/v1/nodes/{uuid}/ssh` 写入。
- 未保存 SSH 凭据：界面弹出 agent 安装命令，不执行 SSH 恢复；到节点运行该命令重新接入。
- 阶段为 `stopped`：按钮置灰，悬停查看云实例已销毁的原因。
- server 设置了 `--recovery-enabled=false`：手动触发返回 `recovery is disabled`。参数见[配置参考](../reference/configuration.md)。

### 按最后错误处理

**缺少 SSH 凭据或平台公钥**

自动恢复会跳过无凭据节点；直接调用恢复 API 返回 `ssh credential is not configured`。
补充凭据后再重连。凭据只有地址时，平台使用平台密钥登录；出现 `has not authorized the platform key` 时，按错误给出的命令授权公钥。

**还在等待，或已停止自动恢复**

- 距上次在线不足 `--recovery-grace-period`（默认 2 分钟），或仍在退避：等待下一次尝试，或手动重连。退避从 30 秒起，每次翻倍，上限 10 分钟；`server_dial` 节点还需等反拨退避达到最大档（5 分钟）。
- 同一阶段连续失败两次，变为 `recovery_blocked`：自动恢复停止；处理错误后手动重连。
- SSH 不可达：检查地址与网络；平台会持续退避。目前不校验 SSH 主机密钥，以免云厂商回收并重用公网 IP 后，旧密钥阻断卸载与恢复。

**agent 正在运行，但没有控制会话**

错误为 `agent is running but has not established a control-plane session` 时，恢复不会重启或重装 agent。
按[节点一直「连接中」或不上线](#节点一直连接中或不上线)检查 agent 日志和连接配置。

平台用 unit 所指 agent 的 server 地址判断归属：读取配置文件，较早安装版本读取命令行 `--server`。
地址匹配、读不出地址，或节点采用 Server 发起连接（没有该地址）时，才视为本节点的 agent。

**unit 属于另一个 server**

`foreign_agent:` 后会写出另一个 server 的地址。所有安装共用 `gpuworks-agent` unit 名，后装的会覆盖先装的；恢复不会重启或重装正在运行的他人 unit。

确认那个 server 已不再使用这台主机后：

1. 备份 unit 文件。
2. 在主机执行 `systemctl stop gpuworks-agent`。
3. 手动重连，使用本节点的 token 重装 unit。

他人的 unit 已停止时，恢复会直接重装，不先重启它。

**云实例已销毁**

阶段为 `stopped`，原因为 `provider instance no longer exists`。通过 API 释放成功，或同步发现云端实例已不存在时立即进入此阶段，节点当时可能仍在线；agent 重新连接会清除此状态。
平台保留节点记录，确认不再需要后再手动删除。

### 云平台改了连接地址

关联云实例的节点在 SSH 恢复前会重新读取实例入口：

- 原 SSH 端点不在入口列表中：改用当前入口，保留用户和凭据。比如 RunPod 修改 Pod 配置会重启并重新编号所有端口；普通重启不改端口。
- Vast、RunPod 等使用端口映射的 `server_dial` 节点，agent 映射地址发生变化：更新地址并立即反拨，本轮不再 SSH 登录。

同一云账号的实例清单每轮最多读取一次，30 秒内复用；多个离线节点不会逐一重复调用厂商接口。

## GPU 没被探测到，或厂商工具未响应

**现象**：节点在线但 GPU 数为 0，或节点详情页出现「厂商工具未响应」标记（节点列表不显示）。悬停可看
厂商工具从什么时候起没有返回（连续失败的第一次，探测成功后清除）和具体错误。

> 加速卡探测失败**不会**让节点离线或变成别的状态：节点连着就是「在线」，终端、诊断、重连都照常可用——
> 驱动卡住的节点恰恰最需要登上去看。探测失败时，只有厂商工具才知道的信息（显存、UUID、健康）保留上一次成功探测的结果，
> 并由「厂商工具未响应」标出；哪些卡在机器里、归谁，来自 PCI 总线，不受影响。

**按检查结果处理**

先在 **计算节点 → 节点详情 → 诊断** 查看 **需要处理**中的依据和复核命令，再按下表处理。
CLI 可在节点执行 `gpuworks-agent doctor`；退出码恒为 `0`，应读取 `-- action required --`，不能仅凭退出码判断健康。

| 观察到的结果 | 下一步 |
|---|---|
| GPU 为 0、没有厂商工具 | 在节点核对驱动、工具是否安装，以及 agent 用户的 `PATH`；按硬件选择 `nvidia-smi`、`npu-smi info`、`ppu-smi` 或 `hy-smi` |
| **厂商工具未响应**、`gpu probe failed` | 在节点运行对应工具，查看耗时与报错；持续卡住时检查驱动日志，旧显存读数不能作为当前容量 |
| **运行时无法使用**、**驱动初始化失败** | 查看该卡的诊断依据与其他卡的结果；先恢复驱动，再重做诊断 |
| **读不到信息** | 检查厂商工具是否可执行、能否返回；不据此直接判定硬件损坏 |
| **已直通**、**未绑定驱动**、**被其它驱动占用** | 先确认卡的预期归属；这些卡不参与本节点调度，不要直接按硬件故障处理 |
| 宿主能看到卡，容器看不到 | 查看 **运行时**或**运行时插件**提示；修复插件或工作负载用户的设备权限，再检查调度是否恢复 |
| **被平台外的进程占用** | 核对显示的进程和显存占用，确认可以停止后再释放；不直接终止未知业务进程 |
| `accelerator_unrecognized` | 保存报告中的 BDF、`vendor:device`、class 和驱动信息，用于补充设备识别 |

处理后重新运行诊断，确认卡的健康、分配状态与实际设备一致；节点 **在线**只证明连接正常。

### GPU 为 0，先确认工具与驱动

驱动或 `nvidia-smi` 没装 / 不在 `PATH`：prober 返回 0 张 GPU 且不报错，节点在线但没有 GPU。

**平台如何识别厂商**：agent 按 `hy-smi`、`npu-smi`、`ppu-smi`、`nvidia-smi` 顺序选第一个真正应答的，
NVIDIA 的判据是 `nvidia-smi --query-gpu=index` 能成功。NVIDIA 排最后，因为 PPU SDK 自带一个
CUDA 兼容的 `nvidia-smi`（`/usr/local/PPU_SDK/CUDA_SDK/bin`），它会把 PPU 卡当作 GPU 正常应答。

都不应答时按**厂商设备文件**（`/dev/davinci_manager`、`/dev/alixpu_ctl`、`/dev/nvidiactl`）判，再按**装了哪家 CLI**判
（`npu-smi`、`hy-smi`、`ppu-smi`、`nvidia-smi` 顺序，NVIDIA 最后：别家主机上常留着一个过期的 `nvidia-smi`）；
都没有才是「没有加速卡」，厂商为空。驱动还在加载时（昇腾主机开机后 agent 先起、`npu-smi` 尚未应答、
设备文件也还没建）节点因此仍是昇腾节点，随后的探测能补上卡。

**昇腾启动期间暂时为 0**

昇腾节点 0 张卡或日志里 `gpu command failed vendor=ascend args=[info] err="npu-smi: no answer within 10s: ..."`：
`npu-smi info` 超过 10 秒（驱动刚加载时会）。等下一轮探测即可；若持续，在节点上直接跑 `npu-smi info` 看耗时。

### 厂商工具未响应，或读数已过期

在节点手动运行对应工具，结合 `agent.log` 的耗时和错误判断；旧显存读数不能作为当前容量。

`nvidia-smi` 存在但执行失败（驱动与内核库不匹配、NVML 报错、超过 10 秒）：agent 日志出现
`gpu command failed`（带 `output_tail`）和 `gpu probe failed`；server 收到探测失败后保留原有的 GPU 列表、
把读数标为已过期，dGPU 探针结果也沿用上次的，不因这一轮重跑。节点连接状态不变。

#### 工具超时，读数为什么没变化

厂商 CLI 驱动答得慢时（所有调用都在驱动锁上排队，常见于有卡初始化失败的主机），状态上报和指标用的只读查询
（清单、指标、拓扑、版本）每次最多等 10 秒；等不到时先用同一查询上一次的结果，查询本身在后台继续跑，
最长 180 秒才 kill，跑完的结果留给下一轮。同一个 CLI 同时只跑一个这样的查询；同一查询的调用方共用一个进程。
这期间指标标的是那次查询完成的时间，不是当前时间，所以图上会稀疏，但不会出现伪造的新数据点。

日志 `gpu query slow; callers are served its last answer meanwhile`（带 `took`）说明驱动慢；上一次成功的结果
超过 10 分钟仍没有新结果时，才按探测失败处理（节点仍然在线，卡的读数标为已过期）。超时只算慢，
不会换用字段更少的查询。udocker 解析设备节点用的 `nvidia-smi -q -x` 也走这条路。

#### 指标为什么稀疏，诊断为什么跳过检查

NVIDIA 查询按以下频率更新，未到周期不代表探测故障：

| 查询 | 更新时机 |
|---|---|
| 指标 | 每 10 秒 |
| PCIe 吞吐（`nvidia-smi dmon`） | 每 60 秒；其间的指标样本不带 PCIe 值 |
| 清单、外部进程 | 随状态上报，每 60 秒 |
| 拓扑（`topo -m`）、MIG 列表（`-L`） | 卡的序号、UUID、PCIe 地址变化时，或每小时；健康主机的拓扑查询也可能耗时十几秒 |
| CUDA 版本 | 驱动版本变化时 |

节点诊断（doctor）同样只用这些共用查询：驱动版本取自清单（`nvidia-smi` 答不上时读内核模块的 `/sys/module/nvidia/version`），不再单独跑 `nvidia-smi`；CUDA 运行时检查没通过时，
跳过同样要初始化驱动的 peer access 与 NCCL 检查（显示 `SKIP (the CUDA runtime check did not pass)`）。
server 等 doctor 最多 2 分钟，agent 自己把一次诊断限制在 100 秒内，到点仍在跑的检查被截断，报告照常返回（昇腾的逐 die 检查另外给报告其余部分留 10 秒，放不下的 die 列为未检查）。

#### 日志提示进程 kill 后仍未退出

会改驱动状态的命令（如昇腾 `npu-smi set`）和要求即时结果的调用不用旧结果：仍限时 10 秒，超时后被 kill
仍不退出的（卡在驱动里的 D 状态进程）只等 3 秒就放手，日志 `gpu command did not exit after kill`；
在它退出（日志 `stuck gpu command exited`）之前，同一个 CLI 的这类调用直接失败（`an earlier call has not exited`），不再启动新进程。
同一个 CLI 的这类调用一次只跑一个，两个同时到达也不会各起一个进程；超时时连同它启动的子进程一起 kill。

同样的规则也管着其他会进驱动的命令，所以无论调用多频繁，每一类最多只会有一个卡死的进程：
- **运行时检查**：周期逐卡检查（所有厂商、主机上或容器里）、诊断里的逐卡检查和 NVIDIA 的 CUDA / peer access / NCCL 检查共用一个"运行时检查"名额，同一时刻只有一个在跑。
  其中一个卡死后，新的检查都不启动：周期检查整轮跳过，诊断里显示为未检查或 `SKIP (an earlier runtime check on this node was killed and has not exited ...)`，都不判卡故障。
- **容器里的检查**：容器删不掉（里面的进程卡在驱动里）时记下它，在它消失之前不再起新容器。
- **昇腾 `hccn_tool`**（每分钟读加速卡网口）：按 die 各一个名额、每次限时 3 秒，某个 die 卡死只影响这个 die，节点状态上报不会被它拖住。
- **诊断**：一个节点同一时刻只跑一次，第二次请求直接返回 `a diagnosis is already running on this node`。

持续出现时在节点上 `ps -eo pid,stat,etime,cmd | grep -E -- '-smi|hccn_tool|python3'` 看有没有 `D` 状态的进程，并查 `dmesg` 里的
Xid / NVRM 报错。

### 某张卡运行时不可用或驱动初始化失败

**`runtime_unusable`：加速卡运行时上下文创建失败**

某张卡健康是「运行时无法使用」（`runtime_unusable`）：加速卡运行时冒烟每 15 分钟逐卡跑一轮，
只点名这一张卡；正在跑实例的卡、被平台外进程占用的卡跳过，驱动不应答时整轮跳过，冒烟不改变节点连接状态。
NVIDIA 需要 `python3` 和 `libcuda.so.1`，在这张卡上创建 CUDA 上下文；一张卡初始化失败常常让整机的
`cuInit` 都失败，所以要连别的卡的结论一起看。昇腾在主机装有 CANN 时用 `python3` + `libascendcl.so`
创建 ACL context 并分配 4 KiB，例如 `aclrtSetDevice=507033` 表示该 die 无法使用
（直通给虚拟机的 die 也返回这个码，所以人工复查时要先确认这张 die 没被别的东西占着；
冒烟本身只问本节点拥有的卡）。driver-only 昇腾节点没有 `libascendcl.so` 时跳过检查，不把缺工具算成卡故障。
海光在主机装有 DTK 时用 `python3` + `libamdhip64.so`（`/opt/dtk/lib` 等）按 PCI 地址找到这张卡，`hipSetDevice` 后分配 4 KiB，
例如 `hipMalloc=2` 表示这张卡无法使用；HIP 列不出这张卡或 `hipInit` 失败算主机的问题，不判卡故障；没装 DTK 时跳过检查。
主机上没有厂商运行时的节点（PPU 主机通常只有管理库 `libhgml.so`；只装驱动、没装 CANN 的昇腾主机）改在容器里检查：
借用节点上**已有**的同框架推理镜像（镜像 tag 以 `hggc` / `cann` 开头，优先用本节点实例正在用的镜像），按真实实例的方式只注入这一张卡，
在里面跑同一段检查（PPU 实测每卡约 0.45 秒，含容器启动），用完即删。绝不为检查拉镜像：节点上没有这类镜像时跳过检查，沿用厂商工具的结论。
只在 docker / nerdctl 节点这样做；udocker 节点建容器要把整个镜像解包或复制成一份 rootfs（ext4 上 17 GB 的镜像每次 27–61 秒、占 17 GB 磁盘），
不在 udocker 上做。容器起不来、镜像里没有运行时或 python 都算主机的问题，不判卡故障。Apple 不做。

**点「诊断」会立刻对 NVIDIA、昇腾、海光、PPU 做同一项逐卡（昇腾逐 die）检查**：节点详情的「诊断」与 `gpuworks-agent doctor` 对本节点拥有的每张 die 按需跑一遍上面这项运行时检查
（和周期检查是同一段代码，不另写一套），所以周期检查已经判为 `runtime_unusable` 的 die，诊断里一定看得到。
失败的 die 在加速卡表里显示「运行时无法使用」，并在 **需要处理** 里按 die 列出一项 `runtime_unusable`（级别「错误」，依据与周期检查记录的相同）；
表里已有更具体的厂商结论（如 `reset_required`）时，表中仍显示那一条，`runtime_unusable` 照样单独列出。
「信息」里的 **逐卡运行时检查**（`card_runtime`）给出汇总：通过、失败、未检查各几张。
NVIDIA 另有整机的 CUDA、P2P、NCCL 检查：整机 CUDA 检查让所有卡同时可见，只回答驱动能否初始化、第一张可见卡能否建 context，每张卡的结论看逐卡检查。

**依据里给的是可直接粘贴的命令**：每条发现的「依据」都是一条完整命令，不是对命令的描述，
复制粘贴到节点上就能看到同一个结论。命令里嵌的脚本就是检查自己跑的那一份（唯一来源在
`internal/accelerator`），所以两者不会漂移。昇腾那条会自己定位主机 CANN 安装位置，并用
`npu-smi info -m` 把 die 索引换算成 `ASCEND_RT_VISIBLE_DEVICES` 需要的**逻辑 ACL 设备号**
（两者在直通场景下并不相同）；NVIDIA 那条用 UUID 点名，索引会随驱动丢卡而指向别的卡。
界面上超过 4 行的命令默认折叠，可展开，复制按钮始终复制完整命令（明文 HTTP 下复制 API
不可用时会弹出可手动选中的文本框）。排查 `aclrtSetDevice=507033` 的原因时，在命令前加
`ASCEND_SLOG_PRINT_TO_STDOUT=1 ASCEND_GLOBAL_LOG_LEVEL=1`，CANN 会把 runtime 与驱动的报错
直接打到终端；不加时这些报错在 `~/ascend/log/debug/plog/` 下。**die 自身无法初始化时 `dmesg`
里可能什么都没有，`npu-smi info -t health` 也照样报 OK**，此时直接尝试复位该 die
（`npu-smi set -t reset -i <npu> -c <chip>`，会影响同卡的另一张 die），不行再重启主机。

「未检查」的 die 不会被省略，也不表示正常，都点名写出并附原因：

- 诊断时间用完：逐张顺序检查，每张开始前先看剩余时间，放不下的不启动，剩下的全部列为未检查；
  正在检查的那张被时间截断时也列为未检查，不判故障。健康的 die 约 1 秒答复，故障的 die 最长等 45 秒，
  agent 自己把一次诊断限制在 100 秒内并给报告其余部分留 10 秒，所以故障 die 很多时后面的会显示未检查——这时可以再点一次，或在节点上直接用 `gpuworks-agent doctor`。
- 有实例正在这张 die 上运行（或还不知道哪些 die 被占用）：与周期检查一样不去动它，列为未检查。
- 厂商工具没有给出这张 die 的设备号：无法点名，列为未检查。
- 主机上缺 `python3` 或 `libascendcl.so`（driver-only 节点）、驱动不应答，或检查没有走到 die（宿主侧结论）：
  只在信息里说明一次「未运行」或「检查已停止」，不再继续检查，也不怪罪任何一张 die；已经得到结论的 die 保留。

直通给虚拟机的 die 不属于本节点，不在这项检查里，表中照旧显示「已直通」。独立运行 `gpuworks-agent doctor` 时没有 agent 的实例信息，会对所有 die 都做检查。

只有检查明确说「是这张 die 失败」才会把卡判为 `runtime_unusable`：库加载不了、库不是 CANN、
`aclInit` 失败、进程崩了或输出无法解读，都算宿主侧的结论（这些情况整机每张 die 都会一样失败），
本轮就此中止、不记到任何卡上，原因可以在节点诊断（或 `gpuworks-agent doctor`）的「逐卡运行时检查」里看到。
拿到 die 并分配成功之后的释放动作（`aclrtFree`、`aclrtResetDevice` 等）失败只打印不判故障，
因为这张 die 已经回答了「能不能用」这个问题；输出里已出现 `ASCEND_ACL_SMOKE_OK` 之后进程非零退出
（例如 CANN 在退出清理阶段崩溃）同样按通过记，退出状态只写进 agent 日志。连续失败的卡重试间隔逐轮翻倍
（1、2、4 轮，最多 4 轮，约 1 小时），这张卡一旦答得出来就立刻恢复每轮检查。刚修好的卡想立刻复查，
在节点详情点「诊断」：诊断逐卡做同一检查，结果直接记成这张卡的最新答案——通过就清掉故障和退避，下一次上报（约 1 分钟）转回正常；失败同样计一次失败。诊断因自身时间预算截断、或运行时在宿主上不可用而没问到的卡不记。agent 关闭时正在进行的检查直接丢弃，不记失败也不计退避。

检查进程卡住时（例如卡死在驱动里、超过 30 秒仍不返回）整机的检查会暂停，直到该进程返回：
agent 不会再启动第二个检查去排在同一把驱动锁后面。这期间卡住的那张卡记为失败，其余卡保留上一次结果
（没检查过的卡没有结论，不会被当成故障），原因同样在 `gpuworks-agent doctor` 里可见。

**`driver_init_failed`：驱动没有带起这张卡**

某张卡健康是「驱动初始化失败」（`driver_init_failed`，仅 NVIDIA）：卡在 PCI 上存在、仍绑定 nvidia 驱动，
但 `nvidia-smi` 列出的卡里没有它，通常是驱动没能把它带起来
（`dmesg` 里有 `RmInitAdapter failed`、`Possible bad register read`、`GPU may need to be reset`）。
这类卡仍然出现在加速卡列表里（卡插在机器上是既定事实，agent 直接读 sysfs 枚举 PCI，毫秒级，不依赖驱动），
只是不可调度，型号取自 `pci.ids`。这类主机上 CUDA 往往整体不可用，部署前先在节点上确认，必要时复位这些卡。

**`vendor_cli_unanswered`：暂时读不到卡的信息**

某张卡健康是「读不到信息」（`vendor_cli_unanswered`）：卡绑定着厂商驱动，但本轮厂商 CLI 根本没应答
（超时，或没装），除 PCI 标识外一无所知——这不等于卡坏了。看节点的 GPU 探测错误，并在节点上手动跑一次 CLI。

### 宿主看得到卡，容器看不到

驱动正常但容器里看不到 GPU：缺 NVIDIA Container Toolkit，或（runveil 节点）用户没有设备节点所在的组。

**自动选择运行时的 NVIDIA 节点**

运行时为自动（未指定 `--driver`、偏好为 `auto`）的 NVIDIA 节点上，Docker / nerdctl 给不了容器加速卡时，agent 跳过它们改用 runveil，
节点可继续使用 runveil 调度；节点详情「运行时」一行显示 `auto（当前 runveil）`，并用橙色提示说明被跳过的运行时与原因
（没装 toolkit 为 `gpu_plugin_missing`，装了但 Docker 没配置为 `gpu_runtime_unconfigured`），附安装或配置命令。

agent 每分钟复查一次：被跳过的运行时能用了，节点上没有实例时自动切回；有实例时提示「Docker 已可用，可切换」（`gpu_ready`），
可在「运行时」下拉框手动切换。昇腾节点不做这种回退（ARM 910B 上 runveil 跑不起 CANN）。

切换运行时（手动或自动）后，实例在新运行时里重建。新运行时缺镜像时，agent 先看节点上其他还能用的运行时（docker、nerdctl、runveil）有没有同名镜像，有就在本机导出、直接导入新运行时，不重新下载；进度显示为「copying the image from the local … runtime」。导入失败或都没有时照常下载。按 digest 固定的镜像（`repo@sha256:…`）不走这条路：导入的归档带不回仓库 digest，只能重新拉取；runveil 只能导出其原生存储里的镜像，老版 udocker CLI 登记的镜像也照常下载。

**Docker 装了但 agent 启动时没响应**

开机时 agent 早于 dockerd 启动，或 dockerd 正忙着拉大镜像、5 秒内没有应答时，自动选择运行时的 agent 在启动阶段最多再等 2 分钟；
仍不应答才改用 runveil，节点详情的橙色提示说明原因（`daemon_not_ready`）。之后每分钟复查一次，Docker 应答且节点上没有实例时自动切回，
有实例时同样提示「Docker 已可用，可切换」。agent 的 systemd 单元排在 `docker.service`、`containerd.service` 之后启动（只排顺序，
没有 Docker 的主机不受影响）。agent 用户无权访问 Docker（不在 docker 组）不属于这种情况，不会等待。

**指定运行时的节点，或昇腾节点**

指定了运行时（或昇腾节点）而运行时给不了容器加速卡时（`gpu_plugin_missing` / `gpu_runtime_unconfigured`），
平台会自动停止该节点的调度：节点列表里「已停止调度」标记悬停可看原因，节点详情的「运行时插件」一行标出缺失的插件并可复制安装命令；
原因没解决就手动恢复调度时，界面会先警告该原因，下一次上报时平台还会再次停止调度；

原因消失（插件装好，或 agent 换到不需要插件的运行时）后的第一次上报，平台自动恢复调度。
平台记录停止调度是谁设的，只解除自己设的：运维手动停止的节点（包括在平台停止之后又手动停止的）不会被自动恢复。

**有卡需要重启主机才能恢复时**

卡报告 `reboot_required` 或 `power_cycle_required`（比如 NVIDIA Xid 79、驱动要求的恢复动作是重启）时，平台同样停止整个节点的调度：
重启主机之外没有办法让这张卡恢复，同一驱动下的其他卡也可能受影响。规则和上面一样——悬停「已停止调度」可看原因；
管理员可以先手动恢复调度去试部署一个模型，原因还在时下一次上报（约 1 分钟）会再次停止；
主机重启后卡恢复正常，第一次上报平台自动恢复调度。只需复位卡的故障（`reset_required`）不停止整个节点，只停这张卡。

### 卡已直通、未绑定驱动或被其他驱动占用

加速卡列表里的卡标着「已直通」「未绑定驱动」或「被其它驱动占用」：分别是绑定 `vfio-pci`
（给虚拟机用）、没绑定任何驱动、绑在 nouveau/pci-stub 这类非厂商驱动上。

它们都不是故障，也不计入节点的 GPU 数与容量，不参与调度，但会出现在加速卡列表里。卡的列表来自直接读 `/sys/bus/pci/devices`
（不依赖 `lspci` 命令），四家厂商都一样；厂商 CLI 能列出的卡一定在列表里，PCI 枚举只会补上
CLI 说不了话的那些（已直通、没驱动、驱动没带起来），不会减少任何一张。
实测：昇腾主机 `npu-smi` 报 `dcmi module initialize failed` 时，8 张 910B4 仍然全部列出。

**agent 跑在容器里时不读 PCI 总线**：容器看得到宿主的整条总线，把宿主其它卡当成本节点的会
凭空多报坏卡，所以容器里只认厂商 CLI 列出的卡。

### 卡的健康检查

节点详情的「健康检查」页上半部分是「卡 × 检查项」表格，下半部分是这个节点的失败记录；GPU 详情的「健康检查」页是同样两部分，只看这一张卡。

**每项检查**：厂商健康（厂商工具的结论）、厂商工具应答、平台外占用、PCIe 链路、PCIe 错误、Xid（NVIDIA）、NVLink（NVIDIA）、GPUDirect RDMA（NVIDIA、PPU）、运行时检查。
前五项、Xid 与 GPUDirect RDMA 随每次上报（约 1 分钟）各做一次，NVLink 每 5 分钟、运行时检查每 15 分钟一次。
GPUDirect RDMA 只在宿主有 RDMA 网卡时检查厂商的 peer memory 模块（`nvidia_peermem`、`alixpu_peermem`）是否已加载：未加载是告警，
卡照常可调度，只有经 RDMA 传 KV 的 PD 模式不放到这个节点；依据里给出加载并设为开机自动加载的命令，「诊断」报告同样列出。格子的状态：

| 状态 | 含义 |
|---|---|
| 通过 / 告警 / 失败 | 这项检查最近一次的结论 |
| 跳过 | 最近一次没做，带原因：卡正在服务、被平台外进程占用、平台还不知道节点在跑什么、宿主暂时不能检查（驱动无响应、运行时缺失、上一次检查还没返回），或厂商工具这一轮没应答 |
| 等待 | agent 启动后这项检查还没做过 |
| 陈旧 | 超过两次上报周期（约 2.5 分钟）没有新的上报，或节点离线：显示最后已知的结论，不代表现在 |

对节点上所有卡都不适用的检查（比如没有 NVLink）整列不显示。

点格子看这项检查最近一次做了什么：时间、耗时、读到的依据、可以自己复核的命令；最近一次被跳过时同时给出最近一次真正做了的那次，
更早出过问题时也给出最近一次出问题的那次。这些由 agent 从内存回答，不跑任何命令，agent 重启后要等每项检查再做一次才有。
节点离线时看不到详情。卡行菜单的「重新检查这张卡」立即做一次运行时检查，结果直接记到卡上（同「诊断」的回写）。

**失败记录**：某项检查告警或失败时记一条，持续失败只占一条（次数与最近依据每小时写一次库，原因变了立即写），
连续通过 30 分钟才算结束、期间再失败仍是同一条，24 小时内反复 10 次以上标「抖动」。通过的检查不写库，健康的集群没有任何写入。
记录保留 30 天（持续中的不清），换卡后按卡的身份区分，节点页里已不在的卡标「已移除」。
server 重启后进行中的记录接着记；重启前最多一小时内的次数可能少计。

### 手动停止或临时放开一张卡

节点详情的加速卡表里展开一张卡，「调度」一项可以：

- **停止调度**：这张卡不再接受新部署，已在上面跑的实例不受影响。一直有效，平台不会自动解除，只能手动恢复。卡在健康标签旁显示橙色「停止调度」。
- **临时放开**：平台判定这张卡不可用（故障、被平台外进程占用）而你想亲手验证时使用。放开后它照常接受新部署，直到放开 1 分钟后的第一次上报重新判定：问题还在就再次停用，已经恢复就保持可用。适合手动部署一个模型试一下这张卡是否真的不能用。放开前会弹出确认，卡在健康标签旁显示蓝色「临时放开」。

已直通给虚拟机或没绑定驱动的卡不归本节点使用，不能放开（`CARD_NOT_OWNED`）。

### 卡被平台外进程占用

卡标着「被平台外的进程占用」：卡上有不是平台启动的计算进程（比如用户自己起的 vLLM）。
这张卡在这些进程退出前不参与调度，悬停可看进程名与显存占用。进程退出后要连续 3 次（约 3 分钟）读到空闲才解除，
期间仍显示最后一次看到的进程：别人每分钟起停一次的任务，否则会让卡随每次上报进出调度。agent 刚重启、还没收到自己
该跑哪些实例之前不做这个判断——否则自己的实例会被当成外部进程；本轮探测失败时也不做，
厂商工具刚答不出清单，再问它进程只会再等一个超时。
平台按卡的硬件身份认出自己的实例；较早版本启动、期望状态里只记了设备序号的实例，
升级 server 时迁移会补上身份，agent 也会按最近一次探测把序号对应到卡，同样算作平台自己的。

这张卡同时不做周期运行时检查，实例启动失败后的补检也跳过它：别人的业务不该被打扰，
卡的显存被别人占着也说明不了运行时能不能用它；卡保留上一次检查的结论，进程退出后恢复检查。

运行时检查自己的进程（或容器）在卡上的那一小会儿，以及结束后 15 秒内，这张卡不做平台外占用判断、沿用上一轮结论，
所以检查不会把卡报成「被平台外进程占用」。检查进行中如果这张卡被分给了实例、或出现了平台外进程，这次失败作废，
不记到卡上、也不计入退避；实例启动前会先等卡上正在进行的检查结束（最多 10 秒），避免在只允许一个进程打开的卡上
（未开 device-share 的昇腾 die）互相挤掉。诊断里的检查同样按这套规则。

各厂商从哪里读卡上的进程：

| 厂商 | 来源 | 复核命令 |
|---|---|---|
| NVIDIA | `nvidia-smi --query-compute-apps` | `nvidia-smi --query-compute-apps=pid,process_name,gpu_bus_id,used_gpu_memory --format=csv` |
| PPU | `ppu-smi --query-compute-apps` | `ppu-smi --query-compute-apps=pid,process_name,pci.bus_id --format=csv` |
| 昇腾 | `npu-smi info` 末尾的进程表（与探测共用一次调用），按 NPU/Chip 对到卡的 Bus-Id | `npu-smi info` |
| 海光 | KFD 驱动的 sysfs（`/sys/class/kfd/kfd/proc/<pid>/queues/*/gpuid`），只把在这张卡上建了队列的进程算作占用 | `hy-smi --showpids` |
| Apple | 不判定：一块统一内存的 GPU，系统进程（如 WindowServer）始终在用，进程级占用没有意义 | — |

### 诊断发现未识别的加速卡

doctor 里出现「有未识别的加速卡设备」（`accelerator_unrecognized`）：本机有属于已支持厂商的
PCI 设备，但不符合该厂商任何一条识别规则——通常是我们还没见过的新型号。把报告里的 BDF、
`vendor:device`、class 和绑定驱动发给我们即可补上。这类设备不在加速卡列表里，也不参与调度。

### 读取诊断报告

节点详情的「诊断」与 `gpuworks-agent doctor` 运行同一套检查。界面按当前语言展示概况、运行时、
加速卡（每卡带 BDF 与健康状态）、**需要处理**和信息；CLI 使用英文分节：
`-- runtimes --`、`-- accelerators --`、`-- action required --`、`-- facts --`。
没有 GPU 时显示 `no accelerator tool found (CPU-only node or driver not installed)`。

**读取「需要处理」**

「需要处理」里的每一项都给出 **依据**（取证命令或调用及其返回值）和 **复核** 命令，可在节点执行复核。
只有厂商工具本身给出恢复方式时，才提供 **处理** 命令，例如复位。

检查项目包括加速卡健康（见下一节）、磁盘可用空间不足 10%（根分区、模型目录、容器数据目录、存储）、
已选用的容器运行时无响应、加速卡工具执行失败，NVIDIA 的 CUDA 初始化失败和多卡通信检查失败，以及逐卡（昇腾逐 die）运行时检查失败（`runtime_unusable`）。被平台外进程占用的卡不检查，列为未检查。

运行时与文件系统方面另有五项（见[运行时初始化失败与容器节点的文件系统](#运行时初始化失败与容器节点的文件系统)）：
`runtime_provision_failed`（运行时初始化失败）、`runtime_state_untraversable`（专用运行用户进不了 agent 目录）、`path_noexec`、`path_read_only`、`path_ephemeral`。

**其他栏目**

- 概况里每个路径（根、模型目录、容器数据目录、udocker 目录、agent 目录、存储）后面带文件系统类型和挂载标记（网络、noexec、只读、容器层）。
- 运行时插件、设备访问（runveil）、设备注入方式、驱动与 Fabric Manager、CUDA / 对等访问 / NCCL 结果、
  udocker 工作负载的运行身份（`runtime_identity`）、网络文件系统（`network_filesystem`）列在「信息」里，只陈述事实。
- 具体探测错误不在节点详情里展示，只在 `agent.log`。
- 厂商、探测命令和运行时矩阵见[支持矩阵](../reference/support-matrix.md)。

## 加速卡「待复位」「显存故障」等健康状态

**现象**：节点详情的加速卡列表里，健康列不是「正常」；鼠标移到 ⓘ 上可看说明、依据与复核命令。

**行为**：agent 每轮探测按厂商工具的字段给每张卡定级，并把取证字段原样存下（字段名与厂商工具一致）：

| 级别 | 调度 | 含义 |
|---|---|---|
| 正常 | 照常 | 无发现，或只有已处理的历史错误（`memory_error_history`） |
| 告警 | 照常，但健康卡不够时才用（评估结果带 `degraded_gpus_used`） | `memory_repair_pending`（待复位）、`vendor_health_warning`（厂商工具 Warning）、`pcie_link_degraded`（PCIe 链路降级）、`pcie_errors`（PCIe 可恢复错误）、`nvlink_down`（NVLink 掉线） |
| 故障 | 停止 | `memory_uncorrectable_errors`、`memory_remap_failed`、`reset_required`、`reboot_required`、`power_cycle_required`、`vendor_health_fault`、`runtime_unusable`（运行时用不了这张卡）、`driver_init_failed`（驱动没带起来）、`vendor_cli_unanswered`（厂商 CLI 没应答，读不到信息）、`pcie_fatal_errors`（PCIe 致命错误） |

「能不能用」是一个结论，由 server 在存卡时判定一次（健康等级、卡的归属、平台外占用三者合一），
API 的 `schedulable` 字段就是它，界面与调度都读这一个字段，不各自再推一遍。**不要读 `healthy`**：
直通的卡 `healthy` 为 false 却没有任何故障，被外部进程占用的卡 `healthy` 为 true 却不能调度。
不能用的原因在卡自身的 `health_status`/`health_code`/`ownership`/`foreign_processes` 里。

厂商工具报出的问题（上表告警、故障两级里由厂商工具给出的那些）要连续 3 次（约 3 分钟）读数正常才解除，期间卡保持原来的级别，
依据里多一项 `Recovery`（如 `1 of 3 clean readings`）说明进度：计数来回跳的卡否则会随每次上报进出调度。
这期间想先用这张卡，可以对它「临时放开」。

各厂商的取证字段：

| 厂商 | 命令 | 字段与判定 |
|---|---|---|
| PPU | `ppu-smi --query-ppu=...` | `remapped_rows.pending` / `retired_pages.pending` 为 Yes → 待复位；`ecc.errors.uncorrected.volatile.dram` > 0 → 显存故障；`remapped_rows.failure` 为 Yes → 显存故障；`xid.correctable.ppu_reset` / `os_reboot` / `cold_reboot` 有值 → 需要复位 / 重启主机 / 断电重启；`ecc.errors.uncorrected.aggregate.dram`、`retired_pages.double_bit.count`、`remapped_rows.uncorrectable` 是复位不清零的累计值，只作历史记录 |
| NVIDIA | `nvidia-smi --query-gpu=...` | `ecc.errors.uncorrected.volatile.total` > 0 → 显存故障；`remapped_rows.pending` / `retired_pages.pending` 为 Yes → 待复位；`remapped_rows.failure` 为 Yes → 显存故障；`ecc.errors.uncorrected.aggregate.total` 只作历史记录；卡不支持的字段返回 `[N/A]`，不参与判定；驱动要求的恢复动作 `gpu_recovery_action`（R550 起）为 Reset → 需要复位、Reboot → 需要重启主机，驱动完成恢复后自己清掉。另读内核日志 `/dev/kmsg` 里的 Xid（需 root）：79（卡掉线）→ 需要重启主机；61、62、95、119、120、140 → 需要复位，二者都保持到之后这张卡的运行时检查通过（或主机重启）；13、31、43 等应用自身错误和 48、63、64 这类有状态字段的 Xid 只记日志。NVLink 每 5 分钟读一次 `nvidia-smi nvlink -s -e`（没有活动 NVLink 的主机每小时一次），活动链路少于这张卡曾有的数量、连续两次 → NVLink 掉线（告警），并立刻重新读拓扑，调度随之不再把它和原来互联的卡视为一组 |
| 昇腾 | `npu-smi info`，非 OK 时再 `npu-smi info -t health -i <NPU> -c <Chip>` | Health 为 Warning → 告警；Alarm / Critical → 故障；错误码与错误信息原样展示，含义查华为文档 |
| 海光 | 驱动 sysfs `/sys/bus/pci/devices/<BDF>/ras/`（与 `hy-smi --showrasinfo all`、`--showpagesinfo` 同源，不起进程） | `umc_err_count` 的 `ue` > 0 → 显存故障；其他模块（gfx、sdma 等）`ue` > 0 → 厂商故障；`gpu_vram_bad_pages` 有 P（待退役）→ 待复位，有 F（退役失败）→ 显存故障，只有 R（已退役）→ 历史记录；`ce`（可纠正）只作依据。驱动没有 RAS 文件时不判定 |
| 所有厂商（PCIe） | 卡到根端口路径上各设备的 sysfs：`current/max_link_speed`、`current/max_link_width`、`aer_dev_{correctable,nonfatal,fatal}`（不起进程；判断端口类型要读配置空间，需 root） | 某段链路的通道数低于两端都支持的值 → PCIe 链路降级（告警）；速率低于两端都支持的值也算，但只在卡被本节点实例使用且不在休眠时判断（空闲卡和交换芯片上行口会主动降速，K100 的交换芯片上行口空闲时是 2.5/8 GT/s）；连续 3 次才判、连续 3 次正常才恢复；虚拟机里只看卡自己这一段。AER 计数以 agent 启动时为基准，只看增量：新的非致命错误 → PCIe 可恢复错误（告警，15 分钟无新错误自动恢复）；新的致命错误 → PCIe 致命错误（故障，直到之后这张卡的运行时检查通过）。固件接管错误处理（firmware-first）的平台计数恒为 0，看不出问题 |

**怎么复核与处理**：使用健康状态说明中按该卡 PCI 地址生成的复核命令。以下以 PPU 待复位为例，
`0000:7e:00.0` 要替换为自己的卡地址。

1. 在节点检查 `Remapped Rows → Pending`，并查看卡上是否还有计算进程：

   ```bash
   ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0
   ppu-smi --query-compute-apps=pid,process_name,pci.bus_id --format=csv
   ```

2. 确认该卡上的工作负载已停止后，以有设备管理权限的用户复位：

   ```bash
   ppu-smi -r -i 0000:7e:00.0
   ```

   复位未成功、需要重启主机时，先安排该主机上其他服务的中断。
3. 等待下一次节点状态上报（最长约 60 秒），重新检查健康状态。显存行替换失败或复位后错误计数继续增长，联系硬件厂商。

## 运行时初始化失败与容器节点的文件系统

**现象**：所有部署都失败，出现 `runveil runtime unavailable: <原因>`；较早版本可能显示
`udocker runtime unavailable` 或 `prepare udocker ownership`。节点诊断也可能报告
`runtime_provision_failed`、`runtime_state_untraversable`、`path_noexec`、`path_read_only`、`path_ephemeral`。
启动日志或实例报 `fork/exec …/udocker/bin/proot-…: permission denied` 时，先看 `runtime_state_untraversable`。

### 先确认出问题的路径

1. 在 **节点详情 → 诊断** 查看错误全文、依据、探测过程和复核命令。CLI `doctor` 是独立进程，看不到运行中 agent 的初始化状态与各路径，不能替代这一步。
2. 在节点按报告路径检查文件系统和目录权限。以下为 RunPod 示例，按实际目录替换：

   ```bash
   findmnt -T /workspace/gpuworks/udocker
   ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state
   gpuworks-agent doctor
   ```

SSH 安装还会在登录用户家目录留下 `udocker-debug` 脚本，使用相同的 `UDOCKER_DIR` 和 PRoot，便于复现 udocker 命令。

### 按错误处理

**文件名不允许包含冒号**

错误为 `<目录> does not allow ':' in file names … reinstall the agent with an install directory on a local filesystem`。
udocker 初始化会试建带 `:` 的文件；镜像层 `sha256:<hex>`、Debian 文件 `/var/lib/dpkg/info/<包>:amd64.list` 都需要这种文件名，不能跳过此限制。

在支持该文件名的本地文件系统上重新安装 agent。RunPod 的 `/workspace` 网络卷不满足这一要求，需要使用容器盘，并预留足够空间容纳解包后的镜像。
**容器盘会在容器重建后丢失**，需要重新拉取镜像；有合适的本地持久卷时优先使用持久卷。
见[为什么 udocker 节点的安装目录不能放在网络卷上](connect-nodes.md#为什么-udockerrunveil节点的安装目录不能放在网络卷上)。

**路径不可执行或只读**

| 检查结果 | 处理 |
|---|---|
| `path_noexec`（错误） | udocker 或 agent 目录所在挂载带 `noexec`；调整挂载或换到允许执行的目录 |
| `path_read_only`（错误） | 模型、udocker 或 agent 目录所在挂载只读；调整挂载或换到可写目录 |
| `path_ephemeral`（告警） | 模型、udocker 或 agent 目录位于容器自身的 overlay 层；重建后丢失，按上面的持久性条件选目录 |

路径检查读取 `/proc/self/mountinfo`，可用 `findmnt -T <路径>` 复核。
容器层检查用于存在 `/.dockerenv` 或 `/run/.containerenv` 的节点。
存储（storage）和 `/var/lib/docker` 不做只读检查：共享模型库可以只读，Docker 数据目录由 dockerd 管理。
NFS、CIFS、CephFS 等网络文件系统本身不是故障，仅在 `network_filesystem` 中列出承载路径；应按具体错误判断。

**不能切换到专用用户**

root agent 默认让 runveil（udocker）的工作负载使用专用用户 `udocker`。
若文件系统拒绝更改新文件的归属，例如 RunPod `/workspace` 的 NFSv4 返回 `Operation not permitted`，agent 会改用自己的用户运行。

日志 `dedicated runtime user is unusable; running as the agent's own user` 及诊断信息 `runtime_identity` 记录这一回退和原因。
这条信息本身不是故障；agent 为 root 时，写入模型目录的文件也属于 root。

**专用用户进不了 agent 目录**

root agent 以专用用户 `udocker` 启动工作负载。agent 会自己开放状态目录（0711 或 0710），但状态目录**之上**的目录属于运维：
若其中某一级不允许 `udocker` 进入（典型是装在 `/root/…` 或 0700 的私有主目录下），每次启动都会报
`fork/exec …/udocker/bin/proot-…: permission denied`，而其他功能（镜像管理、`runveil ps`）照常，不易看出原因。

`runtime_state_untraversable`（错误）给出第一个挡住的目录、它的权限和属主、判断所用的运行用户，以及修复命令。处理：

- 执行证据里的 `chmod o+x <目录>`：只授予进入权限，不能列目录或写入；
- 或把 agent 重装到不在该目录之下的安装目录（如 `/opt/gpuworks`）。

安装前，SSH 预检查的 `Install directory path access` 对同一条件给出失败或警告（见[安装目录检查](connect-nodes.md#安装目录检查)）。
探测能确定 SSH 用户是 root、会用 runveil 和专用用户、且目录确实对该用户关闭时为失败，此时 SSH 安装和云主机一键安装在上传 agent 前停止（400 `NODE_INSTALL_DIR_UNTRAVERSABLE`），并在报错中给出被挡住的目录和 `chmod o+x` 命令；SSH 用户不是 root、本机有 docker 或 nerdctl、安装显式选了其他运行时、目录带 ACL 等不确定的情况只是警告，安装继续。
节点页的诊断、agent 的运行日志告警和 CLI `gpuworks-agent doctor --state <目录>`（该目录下已有 `udocker` 目录时）使用同一份判断。
修复后重新诊断即可，不必重启 agent。

**其他初始化失败**

`runtime_provision_failed` 会保留目录创建、PRoot 释放、chown、文件名等错误全文；chown 错误包含命令的 stderr。
按其中的依据和复核命令处理。拉镜像遇到此错误时会立即以 `udocker runtime unavailable: <原因>` 失败，不逐一尝试镜像源，也不写成 `all image sources failed`；此时应修节点环境。

### 确认恢复

修复后等待错误缓存过期，再重新执行原操作，或重启 agent 后重试。
首次失败缓存 30 秒；继续失败时依次为 1、2、4 分钟，上限 5 分钟。缓存期间各调用直接返回同一错误，不重复初始化。
间隔结束后的下一次调用才重试，成功即清除错误；打开诊断或运行 `doctor` 不会触发初始化。

## 没有 Docker 的节点（runveil）

**现象**：节点没装 Docker；界面把运行时显示为 runveil（driver 名 `udocker` 是保留名）。

**怎么确认与处理**

1. 先在节点详情的诊断信息中确认工作负载的实际用户。只有报告设备访问失败时，才给该用户补设备组；
   以下示例适用于工作负载用户为 `udocker` 的节点：

   ```bash
   sudo gpuworks-agent grant-device-access --user udocker --create-user
   ```

2. 在节点查看容器及其日志；需要进入容器继续检查时，再执行 `exec`：

   ```bash
   gpuworks-agent runveil ps -a
   gpuworks-agent runveil logs -n 200 '<容器名前缀>'
   ```

   ```bash
   gpuworks-agent runveil exec '<容器名前缀>'
   ```

`runveil` 子命令直接读 state 目录；state 不是默认值（`GPUWORKS_STATE_DIR`，否则 `/opt/gpuworks`）时需同样设置，或在子命令前传 `--state`
（安装脚本把 state 写在 systemd 单元 `ExecStart` 的 `--state` 里，连接配置在该目录的 `gpuworks-agent.conf`）。
详见 [CLI](../reference/cli.md)。

**行为**：`--driver` 缺省时 agent 按 docker → nerdctl → runveil 自动选（macOS 上只有 `host`：引擎是宿主进程，
Docker Desktop 的容器在没有 Metal 的虚拟机里）；NVIDIA 节点上给不了容器加速卡的 docker / nerdctl 会被跳过（见[GPU 探测与运行时插件](#gpu-没被探测到或厂商工具未响应)）；
runveil 只要 Linux 的 amd64 / arm64，
默认启动器不需要 Python。三者都不可用时日志为
`no usable container runtime (docker/nerdctl/udocker); deployments disabled on this node`，节点仍可上线、
上报 GPU，但调度以 `runtime_unavailable` 拒绝，dGPU 原因为 `no usable docker/udocker runtime on the node`。
可用 `--driver` 或 `GPUWORKS_RUNTIME_DRIVER` 强制（`docker|udocker|nerdctl|host|none`；`host` 只在 macOS、`udocker` 只在 Linux 可用，
强制了本机系统跑不了的 driver 或非法值时只在 agent 日志里记原因，节点按无运行时继续运行），
或 `PATCH /api/v1/nodes/{uuid}/runtime` 改偏好（`auto/docker/nerdctl/udocker`）。

**可能原因（runveil 上容器起不来）**

1. 非 root 运行时缺设备权限：`doctor` 显示 `device access: FAIL`。
2. 镜像是旧 udocker CLI 登记的：runveil 不能作为同步源，需重新拉取或导入。
3. 用到 runveil 不支持的能力：RDMA 参数网络（`COMPUTE_NETWORK_RUNTIME`）、PPU dGPU（只在 Docker 上验证）。
4. agent 重启之前启动的工作负载退出时，退出码无从得知，固定报为 1（`… (code 1)`），OOM 也就认不出来；
   当前 agent 进程启动的工作负载如实上报退出码（被信号杀死记为 128+信号，OOM 被杀即 137）。
5. 磁盘不够：解包镜像前可用空间少于「镜像各层大小 + 2 GiB」时直接失败（`not enough disk space to unpack the image …`）；
   兼容性修复时把宿主驱动库拷进容器失败也直接失败（`copy NVIDIA driver library libnvidia-ml.so.1 into the container: … no space left on device`）。
   启动容器进程前，容器目录或日志目录对普通用户可用的空间少于 1 GiB 时也直接失败（`not enough disk space to start the container: …`）：
   agent 以 root 运行，能用文件系统给 root 预留的空间，引擎却以普通用户运行，写不进去。
   较早版本的 agent 会在拷贝失败后照常启动，引擎随即退出（vLLM 报 `Failed to infer device type`），而且磁盘满时连容器输出都写不进去。

容器启动后退出时，失败信息带上引擎最后一行报错，例如 `container exited (code 2): vllm: error: unrecognized arguments: …`；
完全没有输出时写 `… with no output`，这多半是磁盘已满、日志都写不进去。

## 模型下载卡住、失败或目录不完整

**现象**：模型在节点上长时间停在 `downloading`，或变成 `failed`、`insufficient_disk_space`、`download_required`，部署创建被拒。

### 先确认是否完整

在模型的节点列表查看状态和 message，并在日志页查看同一时间段的 `source=agent` 输出。
状态包括 `pending`、`downloading`、`syncing`、`present`、`registered`、`failed`、`insufficient_disk_space`、`download_required`、`absent`，**没有 `partial`**。

| 看到的状态或文件 | 如何判断 |
|---|---|
| `present` | 下载并校验完整；`.gpuworks-model-complete.json` 的文件清单和大小全部吻合 |
| `registered` | 已登记本地路径，可直接使用，无需等待 `present`；后续检查可更新状态，获取方式仍是本地登记 |
| `<文件>.part`、`<文件>.part.state` | 下载中间文件，重试会尝试续传，不能据此判断完整。若同名文件已由其他工具（如 `hf download`）下完，且大小等于 `.part.state` 记录的大小，这两个文件视为残留：校验与扫描不再据此判为未完成，平台下次下载时自动删除 |

本地来源的模型在节点上不完整时，创建部署返回 409 `MODEL_SOURCE_UNAVAILABLE`。
公网来源在创建时尝试读取元数据，站点不可达可能只留下空元数据而不阻止创建；**创建成功不代表权重已可用**，启动前仍须完成获取。

### 按错误选择处理方式

**磁盘不足：`insufficient_disk_space`**

message 为 `insufficient disk space at <path>: available=… required=…`。
检查该路径的空间，除下载量外还需留 **1 GiB**，补足后重试。

**站点不可达，或所有来源被禁止**

- `list files:` 表示获取文件列表失败，HTTP 错误带 `http <状态码>`；`download <path> from <url>:` 表示下载失败，HTTP 错误带 `unexpected status <状态码>`。核对日志中的 host 和节点到站点的网络。
- 国内网络可在 **设置 → 镜像与模型拉取** 配置代理或站点规则，将 `https://huggingface.co` 指向可用镜像站或内网代理；用规则抽屉的 **测试连通** 验证。见[配置示例](configure-pull-settings.md#常见网络配置示例)。
- `weights_fetch_forbidden` / 409 `WEIGHTS_FETCH_FORBIDDEN` 表示适用于该节点的规则禁止了所有候选（关闭回源且无替代地址）。调整规则，或在已有模型的节点登记路径、使用共享存储。

**私有或 gated 仓库返回 401 / 403**

在 **设置 → 镜像与模型拉取 → [凭据](configure-pull-settings.md#凭据)** 为 `huggingface.co` 添加 token。
此方式只确认过 Hugging Face，ModelScope 私有仓库尚未验证。无凭据时，401 / 403 会使当前地址冷却并切换候选，全部失败才变为 `failed`。

**登记后缺少元数据**

响应带 `metadata_notice`（`MODEL_METADATA_UNREACHABLE` / `MODEL_METADATA_FORBIDDEN`）时，检查管理节点的出站访问。
给可用站点规则勾选 **用于管理节点获取模型元数据**，必要时给代理勾选 **用于管理节点出站访问**。

**源节点离线，或节点间同步失败**

- 显式选择的源节点离线或副本不完整：停在 `download_required`，不会自动换源。
- 自动从同步组复制时源节点不可达：目标改为公网下载，该节点对 **3 小时**内不再尝试复制；无公网来源则停在 `download_required`。
- 公网不通、其他节点已有副本：可用节点间同步；没有共同网络时报 `NO_STORAGE_PATH`。参见[下载与同步模型](download-and-sync-models.md)、[从 HTTP 导入](import-from-http.md)。

模型在某节点为 `failed` 或 `download_required` 时，部署等待原因是 `model_unavailable`，其中写明模型、节点和错误；自动选节点的副本会跳过该节点，尝试放到别处。

### 长时间没完成：看切源与续传进度

候选由站点规则的替代地址及站点本身组成。进度和失败文案中的 host 表示当前尝试的来源及切换原因，见[节点怎样依次尝试](configure-pull-settings.md#节点怎样依次尝试)。

| 失败类型 | 自动处理 |
|---|---|
| 文件列表获取失败 | 地址不可达、HTTP 错误或列表无法解析时，当前地址冷却 30 分钟，直接尝试下一候选；限流（429、503）除外，按下方「源站限流」处理 |
| 源站限流 | 文件列表或下载返回 429、503：按应答里的 `Retry-After` 或 `RateLimit` 头给出的重置时间等待（至少按退避间隔，最多 5 分 1 秒），在原地址重试，进度显示「rate limited by the source」。计入下一行的尝试次数，次数耗尽才冷却换址；最坏情况下一个文件在最后一个候选上可能等待约 75 分钟 |
| 不可重试的来源错误 | 下载返回其他 HTTP 错误（包括 404、401/403）或 TLS 证书验证失败等：不在原地址重试，冷却 30 分钟并换候选；没有下一候选时结束 |
| 可重试的传输错误 | 下载时连接被拒绝或重置、TLS 握手超时、EOF 等：先在原地址尝试，每文件最多 6 次，最后一个候选最多 16 次，均含首次。退避从 1 秒起，上限 15 秒；次数耗尽仍失败才冷却换址 |
| 单次请求超时 | 单个 HTTP 请求持续超过 15 分钟时，切断并尝试续传，计入该地址的总尝试次数 |
| 连续无数据 | 连续 5 分钟没有响应头或新数据时，切断并尝试续传；同一地址连续两次且还有候选时冷却换址。最后一个候选不再切换，仍受 16 次总尝试上限约束 |

**源站不支持 Range 时，每次续传都会从头下载**。慢链路上的大文件可能反复从头开始，直到最后一个地址的尝试次数耗尽；15 分钟是单次 HTTP 请求限制，不是整个下载任务的截止时间。

### 修复后重试并确认结果

需要重试公网下载时，调用 `POST /api/v1/models/{id}/download`，body 为 `{"node_uuid":"<uuid>"}`，状态回到 `pending`。
失败的部署不会自动重新发起下载。完成后确认 `present`；使用本地登记路径时确认 `registered` 即可。

公网下载没有取消接口。需要清理时调用 `DELETE /api/v1/models/{id}/nodes/{uuid}`；仍有活动部署使用时返回 409 `MODEL_IN_USE`。

## 镜像拉取失败

**现象**：实例停在 `pulling`，事件出现 `image source failed: <ref>: …`，最终为 `all image sources failed: …`；或镜像页节点状态为 `failed`。
镜像状态包括 `absent`、`pulling`、`present`、`syncing`、`failed`。

### 先找到失败来源

打开实例事件时间线，查看 **镜像来源失败**（`image_pull_retry`）中每个引用的错误，再按下方原因处理。

| 错误或现象 | 下一步 |
|---|---|
| 节点访问不到 registry | 检查代理、DNS、防火墙；Docker 的代理需要写入 dockerd，见[代理与 dockerd](configure-pull-settings.md#dockerd) |
| 镜像站、Harbor 不可用或缺少 tag | 核对失败引用和 tag，在来源规则中修改替代地址 |
| 私有仓库要求登录 | 在[凭据](configure-pull-settings.md#凭据)中配置，并核对 tag |
| `insufficient_runtime_disk` 或 `insufficient disk space at …` | 检查节点容器数据目录、归档导入目标的空间 |
| 归档导入或节点间传输提示 `this transfer stopped reporting progress` | 检查节点与传输后重试；这是 `syncing` 状态 15 分钟无进度上报导致的失败 |

最后一类若节点实际已有镜像，会改为 `present`；registry 拉取不受这项 15 分钟超时约束。
没有公网时，来源规则可指向内网仓库、归档 URL，或使用节点间同步，见[离线环境部署](deploy-offline.md)、[从 HTTP 导入](import-from-http.md)。

### 来源被禁止，或修改设置后重试

在 **设置 → 镜像与模型拉取 → 来源规则** 修改镜像站；规则不合法返回 `PULL_SETTING_INVALID`。
适用于该节点的规则若没有替代地址、又关闭回源，就没有候选可用：

- 镜像操作返回 `IMAGE_SOURCE_FORBIDDEN`。
- 部署调度原因是 `image_source_forbidden`（带 `node`、`image`），副本不会在该节点启动。

修复后调用 `POST /api/v1/images/{id}/nodes/{uuid}/sync/retry`，再确认节点镜像状态为 `present`。
重试按当前设置重新计算候选，不能在重试接口指定仓库，否则返回 `IMAGE_SOURCE_REMOVED`。

滚动 tag（如 `latest`、`nightly`）仍是旧内容时，在镜像详情节点行点 **重新拉取**，对应
`POST /api/v1/images/{id}/nodes/{uuid}/sync`，带 `"force":true`；完成后核对镜像 ID 列的新 digest。

### 候选为何切换，何时判定卡住

启动、重放、扩副本、多机预拉取、镜像页「拉取到节点」使用相同候选顺序，部署不单独选择来源：

1. 适用于节点的[来源规则](configure-pull-settings.md#来源规则)中的替代地址，按规则顺序。
2. 模板的 `image_sources`，仅当部署使用模板自身的镜像；见[模板语法](../reference/inference-catalog-spec.md#image_sources模板声明的镜像来源)。
3. 目录登记的 `registry_sources`，例如 Docker Hub、quay.io、华为云 SWR 上的镜像副本。
4. 规范（canonical）地址。

镜像自身的副本由节点探测 `https://<host>/v2/` 后按可达性和延迟排序：探测超时 5 秒，结果缓存 30 分钟。前一个失败才试下一个。

nerdctl 上超过 1 GiB 的层按 0.1 GiB 显示进度，卡住时限相应放宽到约 17.5 分钟。出现 `Progress output not understood, stall check off` 时，本次不再判定卡住；agent 日志保留原始行，便于排查运行时版本的格式变化。

- 下载已报告字节进度且尚未完成，连续 3 分钟没有进展：切换候选。Docker 通常也会报告逐层字节进度，适用这项检测。
- 下载有进展但持续很慢：任意连续 5 分钟平均低于 64 KiB/s，且后面还有候选，切换候选；最后一个候选不因此放弃。按粗步长显示进度的 nerdctl 大层不做这项判定。runveil 节点在此之前先在连接层处理：单条明显落后的连接会换新（见[仓库限流或连接被拒](#仓库限流或连接被拒)）。
- 没有字节进度、正在解包或字节已收齐后的镜像登记阶段：不触发上述 3 分钟判定；每个候选从开始拉取到解包完成，最多 60 分钟。
- 网络、传输失败或卡住：仓库在该节点冷却 30 分钟，在测速候选中排到最后但不剔除。镜像不存在、认证或权限被拒时也会切换候选，但不因此冷却整个仓库；先修正引用或凭据，无需等待冷却。运行时不可用则直接结束本次拉取，应先恢复运行时。

### 仓库限流或连接被拒

runveil（udocker）节点拉取镜像时，agent 日志（`<state>/agent.log`）会记录连接预算的决定。出现下列情况，先判断是仓库在限制连接，而不是网络慢：

- 日志里反复出现 `download connections backed off`，原因是 `throttled`（仓库回 429/503 或拒绝连接，拒绝连接会等待后重试）或 `connection resets`（连接被重置）：仓库或代理在限制来源地址，agent 已自动减半连接并等待。通常无需处理；拉取明显变慢但持续有进展，说明预算已降到仓库能接受的水平。
- 拉取报错含 `429` 或 `503`，且重试几次后失败：仓库持续拒绝。确认私有仓库（Harbor、SWR、ACR）或代理对每个来源地址的连接数、请求频率限制，必要时调高，或用 `--image-pull-max-connections 2` 给节点封顶后重启 agent（范围 0 到 32）；多个候选来源时，失败后会换下一个来源。
- 日志出现 `registry ignores Range: reading the layer whole`：仓库或代理对这一层不支持 Range 请求，这一层改用单连接整块读取，速度受单连接限制；换用支持 Range 的来源或调整代理。
- 日志出现 `download connection recycled`（同一原因连续两个窗口才会换）且原因是 `far below its siblings`、`collapsed from its own peak` 或 `below the absolute floor`：某条连接明显慢于同一次拉取里的其他连接，已换新连接并从原位置继续，属于正常自愈。整条链路都慢不会触发更换；此时拉取慢是链路本身的速度，见[候选为何切换，何时判定卡住](#候选为何切换何时判定卡住)。

### digest 引用需要额外核对什么

模板固定 `repo@sha256:…` 时，节点直接使用拉到的候选启动容器，本地已有候选也一样；运行时不能给 digest 引用打 tag，因此不会改回规范引用。
**平台不核对候选 digest 是否与模板一致**，模板作者和部署者需要确认替代地址内容。失败消息会按引用列出各候选原因。

镜像页手动拉取若候选含 digest，可能返回 `digest image refs are not supported`；这是手动拉取路径的限制，不代表部署不支持 digest。

## 部署被拒、显存估算不足或无合适节点

**前提**：`POST /api/v1/deployments` 创建时不按当前占用做显存调度，成功返回 201（例外：单机创建会按空载试算候选池，
没有一种硬件放得下就返回 400 `DEPLOYMENT_NEVER_FITS`；`placement_mode: "auto"` 的单机创建还会把评估时的放置方案重跑一遍，
不再容纳就返回 409 `AUTOMATIC_PLACEMENT_STALE`）；显存和放置结论出现在
`POST /api/v1/evaluate` 的响应里，以及创建后部署事件的 `DEPLOYMENT_UNSCHEDULABLE`（`params.scheduler` 是调度器给出的原因）。

| 错误码 | HTTP | 含义与处理 |
|---|---|---|
| `AUTOMATIC_PLACEMENT_STALE` | 409 | 评估之后资源被占走或节点不再满足节点条件；重新评估 |
| `NODE_REQUIREMENTS_UNMET` | 400 | 所选节点不满足部署的节点条件；`params.detail` 写明哪个键、要求哪些值 |
| `UNIT_PLACEMENT_NONE` | 409 | 自动放置的多机或 PD 请求（创建、评估、扩容）现在没有一组节点放得下；`params` 给出节点总数 `nodes`，以及各项检查排除的台数：`unmet`（不满足节点条件）、`cordoned`（已停止调度）、`offline`、`busy`（空闲卡不够）、`no_network`（没有可用的计算网络），`largest` 是最大的一组有几台。按最大的一项补齐条件，或改手动指定节点 |
| `AUTO_PLACEMENT_TP_REQUIRED` | 400 | 自动放置的多机请求没写 `placement.tp`，平台不知道每台节点要几张空闲卡；填 TP（和 PP） |
| `AUTO_PLACEMENT_PD_MULTI_NODE` | 400 | 自动放置的 PD 实例必须一台节点放得下；带 `followers` 的实例要自己写节点（`params.role` 是哪个角色） |
| `PLACEMENT_MODE_SHAPE` | 400 | `PUT /deployments/{id}/placement-mode` 用在了单机部署上；它的放置写在节点条件里，改节点条件即可 |
| `NODE_SCHEDULABLE` | 409 | 强制驱逐只用于已停止调度或驱逐中的节点；先停止调度或驱逐，否则撤下的副本会被放回原处 |
| `DEPLOYMENT_NEVER_FITS` | 400 | 创建或编辑节点条件时，候选池里没有一台节点在空载时放得下一个副本（卡太少、单卡显存不够、精度不支持、不能承担服务流量）；`params` 给出候选节点数、硬件规格数与第一台的原因。按具体原因调整并行度、量化或节点条件；不要统一调小 TP，`no_fit` 的处理见下文 |
| `NODE_REQUIREMENTS_INVALID` | 400 | 节点条件本身无效（拼错的系统标签键、两个厂商、与 `target_vendor` 冲突、多机/PD 部署写了 `gpuworks.ai/node`） |
| `LAUNCH_CONFIG_CONFLICT` | 400 | 表单字段与自定义参数里同一个键取值不同；只保留一处（参数为 `key`、`field_value`、`flag`、`extra_value`） |
| `ACQUISITION_CANDIDATE_STALE` | 400 / 409 | 请求指定的保存存储已不存在或不再存有模型，或所选权重来源已不在模板里；重新评估，服务端不会替换 |
| `MODEL_SOURCE_UNAVAILABLE` | 409 | 本地模型在目标节点没有可用来源，或来源校验失败；查看错误详情，不将它等同于公网下载超时 |
| `TEMPLATE_MODEL_MISMATCH` | 400 | 模板部署指定的模型（`params.model_id`）的仓库 ID 与模板任何一个权重来源都对不上（`params.repo_id`、`params.revision` 是模板解析出的来源）；平台不会改用别的模型。选该仓库的模型；本地模型可在模型详情里把来源 ID 改成模板的仓库 ID |
| `MANUAL_COMMAND_RANK_BLIND` | 400 | 多机或 PD 部署的手工命令由所有 rank 共用，却没有引用能区分 rank 的变量（`params.missing` 列出缺少的变量名）。多机要引用 `${GPUWORKS_NODE_RANK}` 或 `${GPUWORKS_HEADLESS}`，PD 要引用 `${GPUWORKS_PD_ROLE}`；从部署页「编辑命令文本」得到的脚本初稿已经引用。已存下的部署不受影响 |
| `DISTRIBUTED_*`、`REPLICAS_*`、`MASTER_PORT_INVALID` | 400 | 多机 / 副本参数不合法 |

**显存不足**（`schedule` 原因码，见评估响应与 `schedule.explain`）：

| 原因码 | 含义 |
|---|---|
| `no_fit` | 每卡启动固定需求 > 可用显存；参数含 `required_gib`、`available_gib`、`tp`、`gpu_memory_utilization` |
| `insufficient_empty_gpus` / `insufficient_shared_gpus` | 空闲整卡 / 可共享的卡不够 |
| `pinned_gpus_unavailable` | 手动放置的 PD 实例钉住的卡被其他部署占着；参数给出 `node`、`gpus`（钉住的卡号）和 `holders`（占卡的部署）。实例不会改用同节点的其他空卡，而是等钉住的卡空出来：停掉或迁走占卡的部署，或把这个部署切成自动放置（`PUT /deployments/{id}/placement-mode`），由平台改用空闲的卡。钉住的卡被平台外进程占着、不健康或空着仍放不下时，仍是对应的原因码 |
| `waiting_for_yield` | 副本在共卡的部署按出让策略让出卡后放得下，正在等它们让卡；参数给出 `node`、`holders`（占卡的部署，各带 `at_once`，或 `idle_sec` 与按时自动出让的 `max_serve_sec`：它按什么条件把卡让给这个部署）和 `instances`。占卡者睡下后自行解决，见[新部署怎样放到共用的卡上](scheduling-center.md#新部署怎样放到共用的卡上)；不自动出让的占卡者从不被要求让卡 |
| `reserved_for_sleepers` | 只因为休眠中的部署为请求唤醒留着卡，这个部署放不下；参数给出 `node`、`gpus`（它本来会用的卡）和 `sleepers`（休眠者，每项 `deployment` 与它的出让策略 `policy`）。共卡的休眠部署在请求能唤醒它时才留卡；你或定时计划下的休眠（保持休眠）不留卡。处理：换一张卡，为本部署开启可休眠并选能共卡的出让策略，或手动休眠占卡的部署，见[新部署怎样放到共用的卡上](scheduling-center.md#新部署怎样放到共用的卡上) |
| `does_not_wait_for_yield` | 仅部署表单预览：占卡的部署都睡下就放得下，但本部署不自动出让，不等它们让卡；参数给出 `node`、`gpus`、`holders`（会让给共卡部署的占卡者）和 `blockers`（不自动出让的占卡者），每项 `deployment` 与出让策略 `policy`。处理：为本部署开启可休眠并选能共卡的出让策略；有 `blockers` 时还要换一张卡或为它们开启能共卡的出让策略 |
| `held_by_non_yielding` | 仅部署表单预览：占卡的部署都睡下就放得下，但 `blockers` 里的部署不自动出让，不会把卡让给本部署；参数给出 `node`、`gpus`、`blockers`（每项 `deployment`、`policy`）和 `holders`（会让卡的占卡者，另带 `waiting_for_yield` 的让卡条件）。处理：换一张卡，或为 `blockers` 里的部署开启可休眠并选能共卡的出让策略 |
| `no_free_gpu` | 没有空闲的 GPU |
| `no_healthy_gpu` | 节点自己的卡都不在能服务的健康级别 |
| `no_owned_gpu` | 节点没有归本机使用的卡：一张都没有，或全部直通给虚拟机、未绑定驱动、被其他驱动持有 |
| `gpus_held_outside_platform` | 卡本身健康，但能用的都被平台外的进程占着 |
| `tp_head_count_not_divisible` | TP 不能整除注意力头数 |
| `sleep_residue_blocks_startup` | 休眠实例的残留显存挡住了启动 |
| `engine_ports_exhausted` | 节点引擎端口区间用尽 |
| `insufficient_disk` / `insufficient_runtime_disk` | 权重盘 / 运行时盘不足 |
| `runtime_unavailable` / `serving_unavailable` | 节点没有容器运行时 / 没有可承担服务流量的网络 |
| `image_source_forbidden` | 节点适用的拉取来源规则让镜像无处可拉（参数 `node`、`image`），见[镜像拉取失败](#镜像拉取失败) |
| `partition_unavailable` | 需要 dGPU 后端但节点没有就绪的，见 dGPU 一节 |

启动固定需求 = 权重 / TP + 多模态编码器 / TP + 预留，**不含 KV**；容量 = 显存 × `gpu_memory_utilization`
（dGPU 时改为：需求取显存上限加预留，容量取卡上剩余可用显存）。所以 `no_fit` 时：增大 TP、提高 `gpu_memory_utilization`、选显存更大的节点，或停掉 / 休眠
占卡的其他部署（评估会给出「休眠某某可释放约 N GiB」的建议）。降低 `max_model_len` 只影响 KV 估算，
**不能**解决 `no_fit`。

**怎么确认**：先在部署页看评估结果（显存拆分、候选原因），再用 `GET /api/v1/deployments/{id}/events` 看创建后的调度事件。
估算原理见[调度与显存估算](../explanation/scheduling-and-estimation.md)，选卡与标签见[部署模型](deploy-models.md)。

## 实例启动失败、反复重启或 OOM

**现象**：实例 `failed`，部署反复新建实例又失败，或容器被杀。

### 找到本次失败原因

1. 查看实例事件时间线，失败原因在 `params.detail`：

   ```bash
   curl -s -H "Authorization: Bearer <jwt>" \
     "http://<server>:8800/api/v1/instances/<uuid>/events?limit=20"
   ```

2. 在实例详情查看实时日志（`GET /api/v1/instances/{uuid}/logs`，SSE），或在日志页检索 `CUDA out of memory`、`Error`。
   容器还存在时，也可在节点用 `docker logs` 或 `gpuworks-agent runveil logs`；容器已清理时使用此前采集的历史日志和事件。
3. 按下方错误处理，再重试并确认实例进入 `running`。

### 显存不足或启动命令错误

**`CUDA out of memory`：显存不足**

用 `nvidia-smi` 确认其他进程是否占用显存，检查 `gpu_memory_utilization`、`max_model_len`，按需要降低它们或增大 TP，再重新部署。

**`container OOM-killed`：先确认是哪种内存不足**

这条消息不能单独证明显存不足。先结合引擎日志、容器内存限制、宿主机内存和内核 OOM 日志判断：确认是 CUDA 显存不足时按上方处理；确认是主机或容器内存不足时，释放内存或按容量调整内存限制，再重试。

平台的 OOM 判据是运行时 OOM 标志，或退出码 137 且最近日志含 `OutOfMemoryError` / `CUDA out of memory`；nerdctl 没有 OOM 标志，只用后一项判据。不能把退出码 137 本身当作显存不足的证据。

**`container exited (code N)` / `run failed: …` / `pre-start preparation failed: …`**

结合引擎日志核对镜像和命令。出现 `container glibc too old` 时检查镜像的 glibc 版本。
多机部署任一 rank 失败会让整代失败，消息为 `rank N on <node> failed: …`。

### 区分两种启动超时

**容器创建超时：`run failed: docker run timed out after 2m0s`**

Docker 或 nerdctl 的 `run -d` 超过 120 秒未返回，就判此次启动失败。先检查节点根盘是否被其他负载写满；dockerd 创建容器时可能在等待根文件系统回写。
可在节点用 `time docker create <镜像> true` 复现。这个限制与引擎健康检查的「启动超时」无关，调大后者不能解决。

agent 会等待超时的 `run` 返回并删除它创建的容器；最多等待 10 分钟，之后终止进程并按容器名删除，避免留下无人认领的 `gpuworks-*` 容器。

**引擎就绪超时：`health check timed out`**

多机 follower 对应 `headless worker readiness timed out`。大模型加载、内核编译或 CUDA graph 捕获较慢时，先看日志是否持续推进；若在正常推进，增大启动健康检查预算：

- 集群默认 **1800 秒**：在 **设置 → 节点与部署 → 部署** 修改。
- 单个部署：在部署表单 **运行环境 → 启动超时** 设置 `start_timeout_sec`，范围 **60–86400 秒**；留空使用自动预算，以集群默认为下限。

预算从实例启动后等待健康检查时使用；值在实例创建时确定，修改只影响之后启动的实例。
超过预算就判本次启动失败，按下方规则重建。



实例详情把**启动等待上限**与**粗略预计启动耗时**分开展示。托管 vLLM 的最终环境若声明 `VLLM_ENGINE_READY_TIMEOUT_S`，自动预算至少给它再留 30 秒探测余量，最高 86400 秒；该变量既未显式声明、也未被明确删除时，平台会协调内部等待预算。手写命令或替换入口不推断其内部超时。部署显式设置的预算优先，即使短于引擎等待上限也不擅自扩展，页面会提示冲突。等待心跳不代表模型加载在推进。

基础估算适用于单节点、单模型的托管 vLLM/SGLang 启动。将 checkpoint 权重加载与其他引擎初始化开销分开：

```text
estimated seconds = W / (512 * 1024 * 1024 bytes/second) + I_ref
```

`W` 是本次选择的 checkpoint 字节量，`I_ref` 是按引擎及可识别执行模式选择的综合初始化参考余量：

| 托管引擎/模式 | `I_ref` |
|---|---:|
| SGLang | 240 秒（4 分钟） |
| 普通 vLLM | 300 秒（5 分钟） |
| 最终命令明确含 `--omni` 的 vLLM | 420 秒（7 分钟） |

初始化余量合并考虑框架启动、tokenizer、编译、KV profiling 和图捕获，不是逐项实测之和。最终命令里的参数优先；模板原文里残留的 `--omni`、模型名称或 JSON 中的同名文字不选中该档。手写命令、多节点、PD 或外部多组件配置仍不提供数值估算。

按 GiB 换算，预计秒数约为 `权重 GiB × 2 + I_ref`：

| 权重大小 | 普通 vLLM | SGLang | vLLM `--omni` |
|---|---|---|---|
| 10 GiB | 5 分 20 秒 | 4 分 20 秒 | 7 分 20 秒 |
| 50 GiB | 6 分 40 秒 | 5 分 40 秒 | 8 分 40 秒 |
| 100 GiB | 8 分 20 秒 | 7 分 20 秒 | 10 分 20 秒 |
| 256 GiB | 13 分 32 秒 | 12 分 32 秒 | 15 分 32 秒 |
| 512 GiB | 22 分 4 秒 | 21 分 4 秒 | 24 分 4 秒 |
| 1 TiB（1024 GiB） | 39 分 8 秒 | 38 分 8 秒 | 41 分 8 秒 |
| 2 TiB（2048 GiB） | 1 小时 13 分 16 秒 | 1 小时 12 分 16 秒 | 1 小时 15 分 16 秒 |

表格使用二进制容量单位；若“1T”指十进制 1 TB（10¹² 字节），普通 vLLM 约 36 分 3 秒。以上假设权重已在节点上，不含模型下载、镜像拉取或排队。

**依据与限制**：[Breaking the Ice（MLSys 2026）](https://arxiv.org/abs/2606.07362)将初始化按阶段建模，并要求按环境标定系数；[ServerlessLLM（OSDI 2024）](https://www.usenix.org/conference/osdi24/presentation/fu)的字节量/带宽项用于加载，不能单独代表全部就绪时间。这里采用该分解思路，512 MiB/s 和上述初始化档位则是本项目基于有限真机记录取整的粗略参考假设，**不是论文给出的系数、当前节点测速或全平台标定结果**。

这些参考档主要依据开发环境的 NVIDIA 样本，不能保证所有后端都更准确：DeepSeek 及部分昇腾 SGLang 启动可能被高估。缓存、编译配置、模型结构、CPU及存储争用仍会改变耗时；没有独立证据时，不按 GPU 数或 CPU 核数线性加速、不因关闭图就扣掉一项猜测耗时，也不自动给运行时加时。缺少权重、分布式、多组件或手写命令仍返回未知/不支持；旧实例按其原先记录的参数展示。估算只用于提示，不调整启动等待上限，也不保证在预计时间内完成。

权重加载、编译和 graph 捕获的秒数仅在日志明确给出时显示，重复 rank 取最大单项而不相加；各项可能重叠或来自不同 rank，不能当作可求和的完整启动分解。已有容器被 agent 重连收养时不重采启动耗时。页面的已等待时间从当前健康检查阶段算起，不包含下载或镜像拉取；超出粗略估算仅提示启动较慢，实际失败仍以探测、进程退出或 OOM 等结果为准。


### 已经 running，随后容器退出

agent 每 15 秒检查一次运行中实例的容器。被 `docker stop`、进程崩溃、OOM 或引擎以 0 自行退出，都先回收容器，再报告 `failed` 并进入重试。

| 消息 | 含义 |
|---|---|
| `container exited while running (code N)` | 运行中退出；多机 follower 消息使用 `headless worker …` |
| `container OOM-killed while running (code 137)` | 运行中被 OOM 杀死 |
| `container disappeared while running (removed outside the agent)` | 容器被节点上的其他操作删除 |
| `cleanup pending: …` | 回收失败，状态为 `removing` |

休眠实例的容器仍在运行，不会被当作退出。`docker inspect` 等容器检查失败时，只跳过本轮。

### 修复后重试

平台通过 controller 重建实例，容器使用 `--restart no`，不依赖 Docker 重启策略。
每个 unit 最多尝试 3 次，新尝试距上次尝试创建至少 15 秒；每次使用新 UUID，并保留旧失败记录。
启动前因主机端口被别的进程占用而被拒绝（`ENGINE_PORT_REFUSED`，此时什么都没有启动）的尝试例外：立即换端口重试，最早的 10 次不计入这 3 次。
预算用尽时产生 `UNIT_RETRY_EXHAUSTED`（`params` 含 `attempts`、`reason`），不再自动重建。

处理原因后，调用 `POST /api/v1/deployments/{id}/units/retry`，body 为 `{"role":"","index":0}`（普通部署 `role` 为空）。

| 返回错误 | 检查项 |
|---|---|
| 404 `UNIT_UNKNOWN` | role 和 index 是否对应目标 unit |
| 409 `UNIT_NOT_FAILED` | 是否还没有失败尝试，或仍有实例 |
| 409 `UNIT_RETRY_NOT_ACTIVE` | 部署是否处于运行中 |

重试后重新查看事件与日志，确认新实例进入 `running`。重建机制见 [ADR 0029](../adr/0029-deployments-reconcile-as-units.md)。

## 网关返回 401、404、503

网关错误体形态与管理 API 不同：`{"error":{"message":"…","type":"gpuworks_proxy"}}`，没有 `code`，按 `message` 判断。

| 状态 | message | 原因与处理 |
|---|---|---|
| 401 | `invalid or missing API key` | 缺 `Authorization: Bearer <key>`、key 已删或无效。`/v1` 默认要求 key，库里**一个 key 都没有**时同样拒绝所有请求；确实需要无 key 访问时，由管理员在**设置 → 安全**打开「接受不带令牌的 /v1 推理请求」（`inference_open_access`） |
| 400 | `request body must specify a model` | 请求体没有 `model` |
| 404 | `no deployment serves model <name>` | 模型名不是部署的 served name（`GET /v1/models` 可列出），或部署已停止 |
| 404 | `deployment <name> does not expose <METHOD> <path>` | 只有试验场（`/api/v1/playground/v1/*`）会做 contract 检查并返回它：该部署的 API contract 不包含这个接口（比如对 Chat 模型调 `/v1/embeddings`）；`GET /api/v1/playground/openapi.json` 可查看它声明了哪些接口。公网 `/v1` 不做这项检查，请求原样转给引擎，由引擎自己返回 404 等 |
| 404 | （`/v1` 整个路径 404） | 访问的监听地址不承载推理 API，检查 `listen.gateway` |
| 503 | `model is asleep; wake it first (console, or POST /api/v1/deployments/<id>/wake; a schedule or a waiting wake may also wake it)` | 没有可服务的清醒副本；在控制台或调用 `POST /api/v1/deployments/{id}/wake` 唤醒，定时计划或等位中的唤醒也会唤醒它，见[调度分析](scheduling-center.md)。休眠意图发出后，所选实例即不再接收新请求 |
| 503 | `no running instance for <name> (still starting?)` | 没有 running 副本：还在启动、失败、被停止或节点离线；看实例状态 |
| 503 | `distributed model <name> is not ready: rank N is unavailable`、`distributed model leader is offline for <name>`、`distributed model leader is not ready for <name>`、`distributed deployment node set is incomplete for <name>` | 多机部署的 rank 或 leader 没就绪 |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … did not reattach within 5s` | 节点数据通道断开后 5 秒内没重连；**可重试**，不会回落到控制通道 |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … data lane closed under an open stream: …` | 请求已经发到节点、还没收到响应时数据通道断开；**可重试**。响应开始返回之后才断开的，客户端看到的是被截断的响应 |
| 503 + `Retry-After: 2` | `no prefill instance could take the request (…)` | PD 部署没有可用的 prefill 实例 |
| 502 | `upstream error: …` | 到引擎的隧道或连接失败，查节点在线性和引擎日志 |
| 413 | `request body exceeds N bytes` | 超过 `--proxy-max-body-bytes`（默认 256 MiB） |

只有数据通道不可用和 PD 无 prefill 两类 503 带 `Retry-After`；休眠和启动中的 503 需要先处理实例，而不是重试。
数据通道机制见 [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md)。其余差异见[OpenAI 兼容网关](../reference/openai-gateway.md)。

## dGPU 能力 unavailable

**现象**：部署带显存上限时评估出现 `partition_unavailable`，或创建返回 400 `DISTRIBUTED_PARTITION_BACKEND_UNAVAILABLE`
（文案 `partition backend <id> is unavailable: <reason>`）。

**读取状态**：`GET /api/v1/nodes` / `GET /api/v1/gpus` 响应里的 `partition_capabilities[]`，每项的 `status` 只有
`ready` / `unavailable`，`reason` 是唯一原因来源；非 NVIDIA 后端另有 `probe_status`（`passed` / `skipped` / `failed`）。

**怎么确认与处理**：`nvidia-dgpu` 不需要探针，只看运行时与资产；`ascend-dgpu` / `ppu-dgpu` 要探针通过。
探针每个 agent 进程默认跑一次并缓存（资产、GPU 或运行时变化时会重跑），修好环境后重启 agent 重新探测。不要退回整卡假装成功。
细节见[启用和排查 dGPU](dgpu.md)、[支持矩阵](../reference/support-matrix.md)。

**可能原因（`reason` 文案，按判断顺序）**

1. `container inference unverified: no usable docker/udocker runtime on the node`：节点没有可用运行时。
2. `container inference unverified: <原因>`：后端对运行时有约束（如 PPU 只在 Docker 上验证）。
3. `agent has not reported current dGPU safety capability; upgrade the agent`：agent 太旧。
4. `missing dGPU assets: <文件列表>`：dGPU 资产缺失。
5. `dGPU probe has not completed: …`：探针还没出结果，稍等或看 agent 日志。
6. `dGPU probe failed: <detail>`：探针失败；探针 `skipped` 时回到 `container inference unverified: <探针 detail>`（如 `no container runtime available on the node`）。detail 来自探针容器，如
   `the container runtime cannot load the embedded probe image`、`dGPU probe timed out`（15 秒）、
   `probe container exited with code N`。

有显存额度而未指定 `partition_backend` 时，平台按候选节点选择就绪的后端；没有可用后端时返回该节点的能力原因，
并非固定使用 NVIDIA 后端。显式指定后端会限定该后端；给了后端却没给正数 `vram_limit_gib` 时，
报 `partition_backend requires a positive vram_limit_gib`，应先修正配置。

## sleep / wake 失败

**现象**：点「休眠」「唤醒」报错，或实例在休眠 / 唤醒后变 `failed`。

**怎么确认与处理**：看实例时间线里 `params.phase` 为 `sleep` 的条目及 `params.detail`。
状态已确认回退时，修复原因后可重试；状态不确定且已标为 `failed` 时，先检查实例重建结果，不反复点击按钮。
见 [sleep / wake](sleep-wake.md)。

| 错误码 | HTTP | 含义 |
|---|---|---|
| `SLEEP_NOT_ENABLED` | 400 | 部署创建时没开可休眠 |
| `SLEEP_ENGINE_UNSUPPORTED` | 400 | 引擎不支持（只有 vLLM 支持） |
| `PD_SLEEP_RDMA_UNSUPPORTED` | 400 | 可休眠的 PD 部署不能让 KV 走主机 RDMA 网络：关闭可休眠，或让节点改用以太网参数网络 |
| `ASCEND_SLEEP_WEIGHT_NZ_CONFLICT` | 400 | 昇腾可休眠部署的附加参数把 `weight_nz_mode` 设成了非零值（`params`：`flag`、`value`）：删掉该键，平台会自动设为 0 |
| `ASCEND_SLEEP_EXPANDABLE_SEGMENTS` | 400 | 昇腾可休眠部署的环境变量开启了 `expandable_segments:True`（`params`：`variable`、`value`）：删掉或改为 `False` |
| `ASCEND_SLEEP_ADDITIONAL_CONFIG_INVALID` | 400 | 模板的 `--additional-config` 不是 JSON 对象，平台无法合并 `weight_nz_mode`（`params`：`flag`、`value`）：修正模板 |
| `SLEEP_INVALID_LEVEL` / `SLEEP_INVALID_MODE` | 400 | `level` 只能 `1` 或 `2`，`mode` 只能 `wait` 或 `abort` |
| `WAKE_INSUFFICIENT_GPU_MEMORY` | 409 | 卡已被占用，放不回休眠实例；`params` 含 `gpu_indexes`、`shortfall_gib`、`occupants`，先停止或休眠占用者，或用 `?wait=true` 等位、`?yield=true` 让占用者休眠后唤醒 |
| `WAKE_GPU_HELD_OUTSIDE` | 409 | 卡被平台外的进程占着，平台看不到占用量，不允许唤醒（等位和让位也不行）；`params` 同上，`refused[].held_outside` 为 true。在节点上释放该进程后重试，见[卡被平台外进程占用](#卡被平台外进程占用) |
| `WAKE_YIELD_BLOCKED` | 409 | 让位唤醒做不到，什么都没改：`params.occupants` 里有不可休眠的部署、PD 最后一个醒着的角色或平台外进程；改为等位，或手动处理占用者 |
| `SLEEP_HOST_MEMORY_INSUFFICIENT` | 409 | L1 休眠需要把权重搬进主机内存，可用内存减去余量（总量的 5% 与 4 GiB 中较大者）放不下；`params` 含 `node`、`need_gib`、`available_gib`，改用 L2 |
| `SCHEDULE_INVALID` | 400 | 定时计划的某个字段不合法：`params.field`、`params.reason` 指出哪一项（周几、时间格式 `HH:MM`、休眠级别、副本数 1–32、动作） |
| `SCHEDULE_TIMEZONE_UNKNOWN` | 400 | server 不认识这个时区名；用 IANA 名（如 `Asia/Shanghai`），留空则用 server 时区 |
| `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` | 400 | 手动放置的多机或 PD 部署不能定时改副本数；先用 `PUT /deployments/{id}/placement-mode` 切到自动放置。计划写好后才切到手动放置的，到点时计划失败并记 `SCHEDULE_ACTION_FAILED` |
| `YIELD_POLICY_INVALID` | 400 | 出让策略请求不合法：`params.field` 是字段（`policy`、`sleep_level`、`unfinished`、`idle_sec`、`max_serve_sec`、`unfinished_wait_sec`、`hold_max_sec`），`params.reason` 为 `required`（没给 `policy`：请求体是 `{}`，或只带参数不带 `policy`）、`invalid_value`（不在允许的取值里）或 `out_of_range`（数值越界，另带 `minimum`、`maximum`；按时自动出让的闲置时间超过最长服务时间时，`idle_sec` 的 `maximum` 就是最长服务时间）；见[出让策略](scheduling-center.md#出让策略) |
| `YIELD_POLICY_SHAPE` | 400 | 多机或 PD 部署只能是不自动出让 |
| `YIELD_POLICY_NEEDS_SLEEP` | 400 | 编辑部署时要关掉可休眠，但它的出让策略（`params.policy`）不是不自动出让；先把出让策略改为不自动出让 |
| `NODE_REPLACEMENT_INVALID` | 400 | 重调度等待时间要是 0（从不）或 60–604800 秒 |
| `REPLICAS_PLACE_ON_TOO_MANY` | 400 | `place_on` 的节点数超过本次增加的副本数 |
| `REPLICAS_PLACE_ON_REFUSED` | 409 | `place_on` 里的某个节点不能放新副本，`params.reason` 为 `unknown`（节点不存在）、`offline`、`cordoned`、`requirements`（不满足节点条件）或 `no_room`（试算放不下）；自动放置的多机部署还有 `no_group`（所写的节点凑不成一个放得下的副本：不同计算网络或架构，或卡不够） |

**可能原因（agent 侧失败）**

1. 引擎没有 sleep 端点（自定义镜像或没开 `VLLM_SERVER_DEV_MODE=1`）：事件里是
   `sleep level N → M failed: <url>: http 404`。
2. L2 唤醒失败，或失败后无法确认引擎真实状态（请求超时、状态探测 / 恢复失败）：状态不确定，实例被标 `failed`（phase `sleep`），消息
   `sleep level uncertain after …`，保留完整显存占用并禁止网关转发，controller 会重建这一代实例。
3. 可确认的失败：保留已确认的实际休眠级别；如果期望仍对应这次失败命令，再将期望回退到该实际级别（也适用于 L1 / L2 切换失败），事件 `INSTANCE_SLEEP_RESULT`，
   `params.result` 为 `failed_rollback`（成功为 `applied`）。失败不会自动重试。

### 定时计划、让卡或等位没有按预期执行

**现象**：到点没有休眠 / 唤醒，或部署被意外睡下。

**怎么确认与处理**：先看部署时间线里事件类型为 `automation` 的条目，或调度分析的**最近的自动动作**：

| 事件 | 含义与处理 |
|---|---|
| `SCHEDULE_ACTION_FAILED` | 自动动作被拒绝，`params.reason` 是原因，`reason_code` 是对应错误码（见上表）；失败的时间点不会重试，处理后等下一次，或手动执行 |
| `WAKE_WAITING` | 定时唤醒放不下，已进入等位；到调度看板看谁占着卡 |
| `WAKE_WAIT_TIMEOUT` | 等位满 30 分钟仍没有空位，已放弃；腾出卡后重新唤醒 |
| `AUTO_SLEEP`（`trigger` 为 `preempt`） | 为别的部署让卡，`reason` 是 `idle`（闲置满闲置时间）、`rank`（对方等级更高）或 `turn`（按时自动出让轮到对方，已服务满最长服务时间），`for_deployment` 是为谁让；部署不想被抢就把出让策略设为不自动出让（健康检查等平台自发请求不算用户请求，不重置闲置计时）。没有别的部署等它的卡时，部署不会自己休眠；之前靠“无抢占时自动休眠”睡下的，改用定时计划 |

server 停机错过的计划只补 12 小时内最新的一次。计划的 `next_at` 与时区不符时，检查计划的 `timezone`（空表示 server 所在时区）。

### 节点离线后副本没有换到别的节点

**现象**：节点离线很久，副本仍留在原处。

**怎么确认与处理**：

1. 部署详情**调度策略**卡片里**节点失联重调度**的等待时间是否为 0（从不）或还没到点；默认 10 分钟，从 server 最后一次见到节点算起。
2. 时间线有 `NODE_REPLACE_NO_CANDIDATE`：没有节点能接手。多机与 PD 副本要求替换节点在线、可调度、满足节点条件、与其余 rank 同计算网络、同 GPU 厂商和架构、卡空闲且试算放得下；补齐其中缺的一项即可，平台每分钟重试。
3. 钉在这台节点上的单机副本（节点条件只允许这一台）不会换，等节点回来，或编辑节点条件。

节点在线却放不下副本（卡被占、停止调度）不属于节点失联重调度：自动放置的多机与 PD 部署会在副本要启动时只换放不下的 rank（时间线 `UNIT_PLACED`），手动放置的部署等待，见[节点在线但放不下](node-failover.md#节点在线但放不下自动放置的重新挑选)。

节点失联重调度的规则和撤回见[节点失联重调度](node-failover.md)。

## 多机、NCCL 或 RDMA 网络不通

**现象**：多机部署卡在启动，rank 之间握手或 NCCL 初始化超时；`DISTRIBUTED_NODE_OFFLINE` / `COMPUTE_NETWORK_*` 被拒。

**怎么确认与处理**

- 需要通过 API 检查节点互通时，调用 `POST /api/v1/deployments/preflight`（2–16 个节点；测带宽时设置 `bandwidth:true`）。
  单个节点对失败仍返回 200、`ok:false`；查看 `pairs[].error`、`pairs[].bandwidth_error` 或 `warnings[]` 中的原因。
- 网络详情页的「内部测试」标签页在宿主机上跑 TCP / `ib_write_bw` 互测，「监控」标签页看网卡与 RDMA 速率和错误计数，
  见[配置节点网络](configure-networks.md#内部测试)与[网络指标参考](../reference/network-metrics.md)。
  内部测试不覆盖容器内 rank 之间的 NCCL 路径。
- 日志页检索 `NCCL`，并对照平台注入的 `NCCL_SOCKET_IFNAME`、`GLOO_SOCKET_IFNAME`（实例详情可看保存的 env）。
- 多机流程见[多机推理](multi-node-inference.md)。

**可能原因**

1. 节点之间的路由、防火墙或端口不通（master 端口须在 1024–65535）。
2. 各节点的参数网络不一致：`COMPUTE_NETWORK_MIXED`（有的节点有参数网络、有的没有，且管理网不能承载）、
   `COMPUTE_NETWORK_MISMATCH`（不在同一参数网络，且管理网不能承载）、`COMPUTE_NETWORK_NONE`（没有能承载参数流量的网络），
   以及 runveil 节点不能用 RDMA 的 `COMPUTE_NETWORK_RUNTIME`。管理网允许承载参数流量时，部分 rank 有参数网络会整组回落到管理网并给出
   `COMPUTE_NETWORK_FALLBACK` 警告。
3. RDMA 是强制 IB（平台注入 `NCCL_IB_DISABLE=0`、`NCCL_NET=IB`、`NCCL_IB_HCA`）：容器看不到 verbs 设备时启动即失败，
   **不会**悄悄退回 TCP。
4. HCA 选错或链路有错误计数：看 `gpuworks_node_rdma_events_per_sec` 的 `link_downed`、`symbol_error` 等。
5. 节点列表不合法或有节点离线：`DISTRIBUTED_NODE_COUNT`（`node_uuids` 数量须等于 `nnodes`）、`DISTRIBUTED_DUPLICATE_NODE`、`DISTRIBUTED_NODE_OFFLINE`。

## 反代后流式输出被攒批，或长请求被截断

**现象**：`stream: true` 的回复一次性整段返回（首 token 延迟等于整段生成时间）；长生成在固定时长后断开；
大请求体被拒。

**怎么确认与处理**：直连 server 端口对比同一请求；`/v1/` 的 Nginx 配置要点（`proxy_buffering off`、
`proxy_request_buffering off`、`client_max_body_size`、`proxy_read_timeout`）见[生产环境部署](deploy-production.md)。
需要逐段接收输出时使用 `stream: true`；非流式长请求仍需给反代和客户端留出足够的等待时间。

**可能原因**

1. 反代开了响应缓冲（Nginx 默认 `proxy_buffering on`），SSE 被攒批。
2. 反代的读超时（Nginx `proxy_read_timeout`）小于非流式长生成的耗时。server 自身对 HTTP 连接不设读写超时，
   长流只会被中间的反代、负载均衡或客户端 SDK 的超时切断。
3. 反代的请求体上限过小（Nginx 默认 `client_max_body_size` 为 1m）。区分方法：来自反代的是 HTML 错误页，
   来自 server 的是 JSON 的 `request body exceeds N bytes`。
4. 用了 `cloudflare-quick` 的临时公网地址：它不支持 SSE，见[给控制台一个公网地址](expose-public-address.md)。
5. server 重启、升级或节点 agent 断线会断开在途的流，且不会重试。

## 操作报「服务端主机的磁盘已满」

登记模型、创建部署、修改设置等任何要保存数据的操作返回 HTTP 507、错误码 `SERVER_STORAGE_FULL` 时，满的是 **server 主机**的磁盘，不是部署目标节点的。SQLite 的数据库文件、WAL 或临时文件写不下时都会这样报，提示里会写明数据目录，以及它所在磁盘的剩余量和总量。

磁盘刚满时不是每次写都失败：SQLite 会先用完自己已经占下的空间，所以同一段时间里有的操作成功、有的被拒。控制台的修改操作（包括登录）都要写数据库，都可能被拒。

1. 在 server 主机上找出满的那块盘。要看的目录有数据目录、日志目录、安装目录下的 `backups/`，以及 SQLite 的临时目录。临时目录取 `SQLITE_TMPDIR`、`TMPDIR`、`/var/tmp`、`/usr/tmp`、`/tmp` 中第一个可写的。

   ```bash
   df -h <data> <data>/logs /var/tmp /tmp
   du -xsh <data>/* | sort -h
   ```

   磁盘满着时，控制台的「收集诊断信息」也写不进去，会在列表里显示为失败（`no space left on device`）。这时在 server 主机上执行 [`gpuworks support-bundle`](../reference/cli.md#gpuworks-support-bundle)，用 `--out` 指到另一块有空间的盘。包里的 `server/disk.json` 记录了这些目录所在文件系统的总量和剩余量，以及每个目录下各项的大小。`disk.json` 只列这些目录里的内容：剩余为 0、目录里的内容加起来却远小于总量时，占空间的是同一文件系统上的其他目录（例如同在根分区上的 `/var/log`），用 `du -xsh /*` 接着找。
2. 清出空间后直接重试，不用重启 server。写失败期间 server 日志文件停写，记录仍写进 stderr（systemd 下即 `journalctl -u gpuworks`）。空间恢复后 30 秒内日志文件会续写，第一行是 `log file writes resumed`，带丢弃的条数（`dropped`）和停写的起点（`since`）。
3. 不要在清出空间之前重启 server：剩余为 0 时 server 起不来，SQLite 连共享内存文件都建不了。这时退出原因 `msg="server exited"` 会在原错误后写明数据目录所在磁盘剩多少（`the disk holding the data directory … has 0 B free of …`）。清出空间后再启动即可。

## 忘记 admin 密码

**现象**：无法登录。（还能登录、只想换密码时，在控制台右上角账户菜单选择「修改密码」。）

**处理**

1. 在 server 机器上确认实际数据目录。以下命令适用于默认安装目录；自定义安装或直接 `serve` 时替换 `--data`。
   使用有数据库访问权限的操作员执行，不必停止 server：

   ```bash
   gpuworks reset-password --data /opt/gpuworks/server/data
   ```

2. 用新密码重新登录，确认恢复访问。改密会使该用户已有的管理会话失效；推理令牌不受影响。

`--user` 缺省为 `admin`；不给 `--data` 时先读二进制同目录、再读默认安装的 `server.yaml` 里的 `data_dir`，都没有才用 `./data`（数据库路径可用 `--db` 直接指定）；不给 `--password` 时在终端里提示输入两次（不回显），避免密码进入命令历史；stdin 不是终端时必须给 `--password`。新建或修改管理员密码须为 8–128 个 Unicode 字符、最多 512 字节，不能与用户名相同，并拒绝常见可猜口令、整段重复及序列；不强制大小写或符号组合，允许长口令短语。此策略不阻止已有账户继续登录。
报 `database ... does not exist` 时，按提示用 `--data` 指定 server 实际的数据目录。
用户不存在时报 `not found`，先核对用户名和数据目录；出现权限错误时检查当前用户对该数据库的访问权限。
见 [CLI](../reference/cli.md)。

## 无法登录现场时：调试采集与收集诊断信息

不能登录客户服务器时，让管理员在 **设置 → 诊断** 里操作，把下载的压缩包发回来。

**收集诊断信息**：选时间范围（默认最近 1 小时，最长 7 天）、相关的部署和节点，点**开始收集**；完成后在列表里下载 `gpuworks-diagnostics-<id>.tar.gz`。包里有：

| 路径 | 内容 |
|---|---|
| `manifest.json` | 本次收集的范围、server 版本，以及每一项是已收集、已跳过（节点离线、agent 版本不支持）还是失败 |
| `server/disk.json` | server 的数据、日志、备份目录与 SQLite 临时目录：所在文件系统的总量与剩余量，以及每个目录下各项的大小（从大到小） |
| `server/gpuworks-server.log` | 时间范围内的 server 日志（含 info，日志页只有 warn 以上） |
| `nodes/<uuid>/agent.log`、`doctor.json` | 所选节点（不选时为所选部署的实例所在节点）的 agent 日志与诊断报告 |
| `instances/<uuid>.log` | 所选部署的实例日志 |
| `debug/<会话>/`、`nodes/<uuid>/debug/` | 时间重叠的调试采集 |
| `db/*.json` | 全部节点、所选部署与实例、时间范围内的事件、所选节点的镜像状态、镜像目录、存储、设置、所选部署的请求记录 |

凭据（token、密码、密钥、哈希）只保留前 8 个字符，和界面上的 API key 前缀对得上。压缩包在 server 数据目录的 `diagnostics/bundles/` 下保留 7 天。
server 起不来时，在 server 主机上执行 [`gpuworks support-bundle`](../reference/cli.md#gpuworks-support-bundle)，直接读数据库和日志文件（节点那部分收不到，manifest 会注明）。

**调试采集**：现象要靠 debug 日志或请求原文才能看清时（例如客户端一直收不到流式结束），先开调试采集复现一次，再收集诊断信息：

1. 选时长（默认 10 分钟，最长 1 小时）、总大小（默认 10 GiB，最大 50 GiB）、要记录的部署和要开 debug 日志的节点。
2. 采集期间：server 和所选节点（以及所选部署所在节点）的 agent 把 debug 及以上的日志写进单独的文件，平时的日志级别不变；所选部署的每个请求完整记录为 `requests/<部署>-<请求 ID>.jsonl`：
   请求原文（凭据头已去掉）、网关从引擎读取的原始块（`upstream`）、网关写给直接下游的原始块（`client`）、各自相对开始的毫秒数，以及结束方式（`end` 行的 `observation_scope: "gateway"`、`finish_reason`、`outcome`、读写错误）。单个请求最多记录 16 MiB，每个节点的 debug 文件最多 200 MiB。
3. 到时间、写满大小或手动停止时结束。开启与结束都写一条 server 告警日志（带操作人），作为审计记录。
4. 请求记录含提示词与回复，只保存在 server 和节点本地，7 天后自动删除。agent 版本过旧的节点不支持，会标为已跳过。

## 仍未解决？

提 issue 或求助时，优先附上[诊断信息压缩包](#无法登录现场时调试采集与收集诊断信息)；没有时附上这些信息，能省掉大部分往返：

1. 版本：`gpuworks version`（server）和 `gpuworks-agent version`（出问题的节点）。
2. 出问题的实例 UUID 及其事件时间线：`GET /api/v1/instances/<uuid>/events`（必要时加
   `GET /api/v1/deployments/<id>/events`），以及实例响应里的 `schedule` 字段。
3. 日志，限定在故障时间段前后几分钟：server 的 `gpuworks-server.log`，节点的 `agent.log`，
   日志页里该实例的引擎输出。
4. 节点上 `gpuworks-agent doctor` 的完整输出。
5. 连接类问题附 `GET /api/v1/agent/rejects` 的结果和 `gpuworks-agent smoke` 的退出码，以及部署请求体与网络拓扑（是否反代 / NAT）。
   分享前删除 API key、节点 token 和其他凭据。
