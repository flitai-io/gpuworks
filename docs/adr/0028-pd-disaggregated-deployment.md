# ADR 0028: PD 分离部署是一个部署加实例角色，两阶段由现有网关编排

日期：2026-09-29 · 状态：已接受

依据：[PD 分离计划](../../artifacts/archive/2026-09-28-pd-disaggregation/plan.md)（决策 D1–D16）、
[阶段 0 真机核实](../../artifacts/archive/2026-09-28-pd-disaggregation/evidence/phase0-verification.md)与
[平台真机验收](../../artifacts/archive/2026-09-28-pd-disaggregation/evidence/platform-acceptance.md)。

## 决策

**一个部署，实例带角色。** PD 部署仍是一行 `deployments`；`instances` 新增字符串列 `role`（普通部署为空）。
每个角色的实例数、放置和 TP 写在已有的 `Deployment.Placement` JSON 的 `pd` 对象里：

```json
{"pd": {
  "mode": "vllm-nixl",
  "roles": {
    "prefill": {"tp": 1, "units": [{"node_uuid": "n1", "gpus": [0]}, {"node_uuid": "n2"}]},
    "decode":  {"tp": 2, "units": [{"node_uuid": "n3", "gpus": [0, 1]}]}
  },
  "kv_load_failure": "fail",
  "fallback": "decode"
}}
```

- 一个 `unit` 就是该角色的一个实例；`gpus` 省略时由调度在该节点内选卡。实例对应哪个 unit 不落库，由
  （角色、节点、卡、启动记录）推导（`planning.MatchUnits`，所有部署形态共用，见 ADR 0029）：先 running、再按创建先后匹配，匹配不到的 unit
  就是缺的实例，没有 unit 认领的在用实例就是排空中的实例。controller 与网关用同一个推导。
- 角色可带 `pp`；unit 可带 `followers: [{node_uuid, gpus?}]`，使该实例成为跨机组：leader（rank 0）在 unit 的
  节点上并对外服务，每个 follower 是另一台节点上的一个 headless rank。TP × PP 平均分到各 rank，多机 unit
  必须写明角色的 TP；一个 unit 的 rank 不能落在同一节点。
- `Deployment.Replicas` 对 PD 部署不生效；`NodeID` 指向第一个 decode unit 的节点，`NodeIDs` 是全部 unit 的节点
  去重后的列表（含 follower 的节点），供模型与镜像就绪检查复用。部署行的 `NNodes` 始终为 1：跨机的是角色实例，
  不是部署。
- `kv_load_failure` 是引擎层 KV 加载失败策略（`fail` 默认，`recompute` 可选）；`fallback` 是网关层 P 不可用时的
  行为（`decode` 默认：降级到 D 本地完成；`fail`：返回可重试的 503）。

**PD 模式目录是规则的唯一产地。** 纯叶子包 `internal/server/pdmode` 声明每种模式：引擎、角色名、每角色 KV 连接器
的角色值、线协议、TP 方向规则、每实例需要的端口、是否要求同厂商、KV 传输指标读哪个角色。平台代码不写死
"prefill/decode"之外的规则。现有四种：

| 模式 | 线协议 | TP 规则 | 传输 | 真机 |
|---|---|---|---|---|
| `vllm-nixl` | 顺序两阶段 | decode ≥ prefill | UCX | 已验证（同机、跨机以太、角色内跨机） |
| `sglang-nixl` | 并发双发（`bootstrap_host/port/room`） | 相等 | UCX | 未验证 |
| `vllm-ascend-mooncake` | 顺序两阶段 | prefill ≥ decode | HCCL（NPU 自带 RoCE，挂载 `/etc/hccn.conf`） | 已验证（跨机；P、D 的 NPU 须在 NPU 网络上互通） |
| `vllm-mooncake` | 顺序两阶段，交接参数由网关生成（prefill 不返回 `kv_transfer_params`，网关从其 bootstrap 服务读 engine id） | 相等 | Mooncake 传输引擎（海光 DTK 构建，RDMA 或 TCP） | 已验证（K100 同机两卡、TP 1） |

TP 规则违反时报与规则对应的错误码。引擎档位按 `pdmode.ForEngine` 下发 `pd_modes`，前端据此给出可选模式。

**校验只有一处。** `planning` 里的一个函数解析并校验 `pd` 对象（角色齐全、unit 非空、卡不重叠、TP 规则、每角色
上限 32、rank 划分），评估、创建、扩缩与 controller 共用。

**启动参数由引擎适配层按角色注入。** vLLM 适配器在唯一的组装点生成 `--kv-transfer-config`：PD 连接器与 KV 卸载
（CPU 层或磁盘层）同时存在时合成一个 `MultiConnector`。阶段 0 实测：`--kv-offloading-size` 与 NIXL 的
`--kv-transfer-config` 同时给出时，引擎静默丢掉 NIXL，所以 PD 下 CPU 层也必须走合成。环境变量：
`VLLM_NIXL_SIDE_CHANNEL_HOST` 为该节点在参数网络上的地址，`VLLM_NIXL_SIDE_CHANNEL_PORT` 为分配到的端口，
`UCX_NET_DEVICES` 在 RDMA 网络下取第一个 HCA、在以太网络下取参数网络网卡并同时设
`UCX_TCP_BRIDGE_ENABLE=y`（UCX 默认过滤网桥，指定网桥时 NIXL 创建后端失败）。RDMA 网络下注入的容器参数与多机
推理相同，并把 `UCX_TLS` 限定为 RDMA 与 CUDA 传输，配错即启动失败。`vllm-nixl` 实例另设
`VLLM_SSM_CONV_STATE_LAYOUT=DS`：NixlConnector 只按 dim-first 布局传输 Mamba 类层（gated delta net、KDA、short conv）
的 conv 状态，其他布局下拒绝启动，而该变量对没有这类层的模型不生效；模板或部署显式设置的值优先。所有 PD 实例用 host 网络。

**经以太网的 NIXL 在部分内核上会卡住（UCX 缺陷，平台提示而不绕过）。** UCX 1.20–1.22（NIXL 1.4.1 自带 UCX 1.22.0）
启动时用 netlink 判断对端是否可达：`ucs_netlink_route_exists` 对 `AF_INET6` 发 `RTM_GETROUTE | NLM_F_DUMP`，只检查
**每个数据报的第一条消息**是否为 `NLMSG_DONE`（openucx/ucx 6d96bec1a0）。较新的内核（实测 Ubuntu 5.15.0-198、-203）把最后
几条路由与 `NLMSG_DONE` 放进同一个数据报，UCX 看不到结束标记，再调 `recv()` 就永远阻塞，同时另一个线程空转占满一个核，
引擎初始化 NIXL 时挂住；旧内核（5.15.0-177）把 DONE 单独发一个 20 字节的数据报，没有 IPv6 路由时整个 dump 就只有 DONE，
都不受影响，IPv4 dump 在三种内核上都正常。上游已在 openucx/ucx 32c7a90a45（#12182）修复，尚未进入发布版。
UCX 的环境变量里只有 `UCX_NET_DEVICES=lo` 能避开，但它无法跨机。唯一实测有效的办法是**整机**关闭 IPv6
（`net.ipv6.conf.all.disable_ipv6=1` 与 `net.ipv6.conf.default.disable_ipv6=1`；只关单个网卡无效）。

平台不替运维关 IPv6（它会改变整台主机的网络行为），而是把它做成可见的事实：agent 启动时用纯 Go 的
`NETLINK_ROUTE` 套接字发同样的 dump，若某个数据报里「路由消息之后紧跟 DONE」就判定受影响，之后与节点的运行时检查同频（每
15 分钟）复查，因为 sysctl 随时可能改。探测在独立 goroutine 里以非阻塞套接字加几秒的截止时间运行，读取受数据报数与字节数
上限约束，超时就放弃并记为「未知」而不是「受影响」，任何时候同一进程里至多一个探测在跑，也不起外部命令。结果作为
`NodeStatus.UCXNetlinkHang` 随状态上报，server 只挂在在线会话上、不入库（与 `NVIDIAIsolation` 同一路径），节点详情的
`ucx_netlink_hang` 带 `kernel` 与 `fixes`（字面命令）。创建与评估 PD 部署时，若模式走 UCX（`vllm-nixl`、`sglang-nixl`）、
放置里有节点上报了该事实：参数网络是以太网（含管理网）则自动放置不选该节点、手动指定给警告（见下），在主机 RDMA 网络上 UCX 被限定为 RDMA 传输、
可能根本不做这项检查，所以只给警告 `PD_UCX_NETLINK_HANG_RDMA`，评估里两者都保留。`gpuworks-agent doctor` 报告同一项
（`ucx_netlink_hang`）。

**规则只有一处：候选节点的事实。** 这条规则（模式走 UCX 且网络不是主机 RDMA 时，内核会让 UCX 卡住的节点不能放）写在
`planning` 的候选事实里（`UnitSuggestFacts`，与在线状态一样由调用方注入 hub 的实时事实，不读库），所有选节点的路径共用：
创建与评估的自动放置、controller 对自动放置单元的重新挑选、节点失联后的重调度与排空时的替换节点、PD 副本扩缩容。
这些路径**跳过**受影响节点，而不是挑中后再失败——之前的校验只在创建时挑完节点之后做，controller 后来重新挑选或故障转移
仍可能把一个 rank 放到会卡住的节点上。跳过之后没有候选时沿用原来的「没有节点可放」（`UNIT_PLACEMENT_NONE`，事件
`NODE_REPLACE_NO_CANDIDATE`），并在参数里加上被排除的数量与前几个节点名（`ucx_netlink_hang`、`ucx_netlink_nodes`），
运维知道该去修哪几台。**手动指定**的节点是运维的选择，不替运维换也不拒绝：按同一条规则给警告 `PD_UCX_NETLINK_HANG` 并带修复命令，
说明引擎很可能起不来、部署照常创建；之后引擎卡住由运维按命令修复。主机 RDMA 网络上不排除，只给警告。事实是内存里的查表，每个候选一次，不新增全量扫描，也不新增持久化事实。

**端口一次分配、全部落库。** controller 为一个实例一次取齐 API 端口与 side channel 端口块，全部写进
`Instance.Ports`，后续分配据此避让。

**端口与启动记录。** 多机 unit 的 leader 另取一个集合通信端口。每个 PD 实例启动时把对端要用的事实写进
`schedule_expl.pd`（`planning.PDRecord`：角色、KV 端点地址与端口；leader 另有集合通信端口），leader 所属的 unit 记在 `schedule_expl.unit`（ADR 0029），
follower 与网关都从这里读，不另存。

**对账。** 角色之间互不连坐：单个实例失败只重建该实例。新增与重建都按当前解析的参数网络与镜像启动，不因与在跑成员
不同而拦截：运维可能正在做镜像灰度或网络割接，两边是否还能配对由交接自检与降级率说明。`POST /deployments/{id}/restart`
是手动的整组重启（连同失败的尝试一起移除，重试预算重新计算）。
P、D 的失败重试预算按 unit 各自计算。多机 unit 内部沿用多机推理的语义：follower 失败或离线超过宽限时，leader
记为失败、follower 全部移除，下一轮按新的组重建该 unit，不波及其它 unit。leader 只有在全部 follower 都 running
时才算就绪、才接流量；leader 要等模型与镜像在每个 rank 的节点上都就绪（缺镜像时预拉）才启动，避免 rank 先后错开
而错过集合通信的 rendezvous。

**运行时扩缩。** `PUT /deployments/{id}/pd` 修改各角色的 unit 与 `fallback`；模式、TP、PP、`kv_load_failure`
启动后不可变（`PD_IMMUTABLE`）。被移除的 unit 先排空：网关立即停止向其派发，controller 等它手上的请求与
待取 KV 归零（至少 3 s、至多 5 min）再删除。prefill 可缩到 0（此时请求全部由 D 本地完成，不计降级），decode 不可。

**持续校验。** 每个 prefill 实例首次就绪后，server 经网关发一次内部两阶段请求，读 D 的 NIXL 计数与参数网络的
RDMA 流量判断传输方式，在该实例时间线记 `PD_HANDOFF_VERIFIED{transport: rdma|tcp|local|unmeasured}`、
`PD_HANDOFF_NOT_RDMA` 或 `PD_HANDOFF_FAILED`。监控把 PD 部署的引擎指标限定在 decode 实例上，另算 PD 有效率
（D 收到的提示 token 中经传输得到的比例）、降级率、prefill 阶段耗时与排队，对应诊断规则 `pd_fallback`、
`pd_ineffective`、`pd_prefill_bound`、`pd_short_prompts`。

**KV 卸载只在 prefill。** 部署的 KV 卸载配置只作用于 prefill 实例（与 PD 连接器合成 `MultiConnector`），decode
实例启动时去掉全部 KV 缓存层：阶段 0 实测 D 侧缓存只有写入、没有命中。用户参数里写 KV 连接器相关的旗标会被拒绝。

**网关两阶段。** `internal/server/proxy` 为 PD 部署先选 D（部署的路由策略，默认 least_request），再选 P
（前缀亲和，亲和不起作用时优先与 D 同节点）。`/v1/chat/completions` 与 `/v1/completions` 走两阶段：

1. 发给 P：原请求体，`max_tokens`（及 `max_completion_tokens`）改为 1，`stream=false`，去掉 `stream_options`、
   `min_tokens`，`kv_transfer_params={"do_remote_decode":true}`；
2. 从 P 的响应里取 `kv_transfer_params` 原样放进发给 D 的请求体。网关不解析其内部字段（0.17 与 0.25 的
   `remote_block_ids` 形状已经不同）。

其余接口直接发给 D。客户端传入的 `kv_transfer_params` 在所有部署上一律丢弃：阶段 0 实测，一个缺字段的该参数就能让
vLLM 的 EngineCore 退出。

降级（`fallback=decode`）：没有可用的 P、P 连接失败、P 返回 5xx、P 返回 200 但没有 `kv_transfer_params` 时，
原请求（去掉 KV 参数）直接发给 D，按原因计数；P 返回 4xx 原样返回客户端。D 连接失败时沿用现有重试，在其余 D 之间
重放带同一份 KV 参数的请求。P 的内部请求不计费、不计入用户请求数；计量只记 D 的响应。

**状态不落库。** 部署的"未就绪 / 部分就绪 / 运行中"与期望比例由每角色期望数与就绪数推导。

## 理由

- 实例的角色无法从卡、节点、端口推导，是唯一必须新增的列；unit 与实例的对应可以推导，所以不落库。
- 两阶段放在网关里，鉴权、计量、重试、body 重放、隧道与服务网络通路全部复用，引擎端口不需要暴露给第三方组件。
- 阶段 0 在 vLLM 0.17.1 与 0.25.1 上确认：D 不带 KV 参数时本地完成（降级成立）；D 已缓存前缀时只取缺的部分
  （D 用前缀亲和有收益）；P 重启后 D 在 TCP 下能继续工作；`PYTHONHASHSEED` 不需要统一。

## 以后

- PPU 镜像只带 Mooncake，没有 NIXL；覆盖 PPU 大概率是再加一个 Mooncake 模式，不改数据模型。
- 按角色指定镜像或档位、附属角色（共享 KV 池的协调进程）只需扩展 `pd` 对象与目录；`role` 是字符串，不限定取值。
