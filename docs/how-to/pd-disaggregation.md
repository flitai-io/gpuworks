# PD 分离部署

**适用场景**：提示长（上千 token）、并发高，prefill 抢占 decode 导致输出不稳时，把推理拆成 prefill 与 decode 两组实例（可跨机）。

完成后，通过同一个服务名调用模型，并从交接事件和请求详情确认 prefill 参与了请求。

**前置条件**：

- 已熟悉普通部署流程，见[部署模型、选卡与解释调度结果](deploy-models.md)。
- 硬件与引擎符合所选 PD 模式：`vllm-nixl` 面向 NVIDIA 与 AMD，`vllm-ascend-mooncake` 面向昇腾，`vllm-mooncake` 面向海光 DCU（要求 vLLM 0.21 的海光镜像，见[海光 DCU](#海光-dcu)），`sglang-ppu-ipc` 面向 PPU（P、D 必须在同一节点），`sglang-ppu-mooncake`、`vllm-ppu-mooncake` 面向经 RoCE 网卡传 KV 的 PPU（可跨机），`sglang-nixl` 未验证。
- PPU Mooncake 模式：节点在主机 RDMA 参数网络上，并已加载 `alixpu_peermem`（见[PPU 经 RDMA 传 KV](#ppu-经-rdma-传-kv)）。
- NVIDIA 与海光模式的 P、D 跨机时 KV 走参数网络，可选 RDMA 或以太网；昇腾模式跨机时必须共用加速器 RDMA 或加速器 HCCS 网络，且 prefill 与 decode 所在 NPU 在对应加速器网络上互通。配置与验证见[配置节点网络](configure-networks.md)。

PD 分离把一个模型的推理拆成两组实例：prefill 实例只算提示，decode 实例从它那里取走 KV 后逐 token 生成。对外仍是
一个部署、一个服务名，请求照常发给 `/v1`，由网关协调两组实例。设计与取舍见
[ADR 0028](../adr/0028-pd-disaggregated-deployment.md)。

## 何时值得用

- 提示长（上千 token）、并发高，prefill 抢占 decode 导致输出不稳时收益最明显；提示很短时 KV 传输的开销可能超过
  收益，服务页 PD 卡片的平均提示长度可以帮助判断。
- NVIDIA 模式跨机时可比较 RDMA 与以太网的交接开销；不能把以太网可用的条件套用到昇腾模式。
- NVIDIA 模式在同一节点上让 P、D 各用各的卡，可用于验证功能，不代表跨机性能。

## 部署

1. 部署页的 **节点放置** 选 **PD 分离**，再选 PD 模式。可选模式来自引擎档位：vLLM 有 `vllm-nixl`（NVIDIA、AMD）、
   `vllm-ascend-mooncake`（昇腾）、`vllm-mooncake`（海光）、`vllm-ppu-mooncake`（PPU，RDMA），SGLang 有 `sglang-ppu-ipc`（PPU，同机）、
   `sglang-ppu-mooncake`（PPU，RDMA）与 `sglang-nixl`（未验证）。

2. 选择放置方式（见[自动放置与手动放置](#自动放置与手动放置)），为 prefill 与 decode 各设 TP，并逐个添加实例：手动时每个实例选一个节点，可选钉卡；自动时实例可以不选节点，由平台挑。模式有 TP 规则：`vllm-nixl` 要求
   decode TP ≥ prefill TP，`vllm-ascend-mooncake` 相反，`vllm-mooncake`、`sglang-nixl`、`sglang-ppu-ipc` 与两个 PPU Mooncake 模式要求相等。
   创建部署时 P、D 每个角色都需要至少一个实例。

3. 一个实例要跨多台节点时，在该实例下 **添加节点**：第一台是 leader（对外服务），其余是 follower。此时必须写 TP，
   TP × PP 平均分到各节点。平台会等模型与镜像在每台节点上都就绪后才同时启动各 rank。

4. 按模式选择故障处理策略，避免把请求成功误认为 PD 交接成功：

   | 模式 | P 不可用时 | decode 取 KV 失败时 |
   |---|---|---|
   | `vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake` | 默认由 decode 本地完成（`fallback: decode`），也可选返回 503（`fail`） | 默认报错（`kv_load_failure: fail`），可选本地重算（`recompute`） |
   | `sglang-nixl`、`sglang-ppu-ipc`、`sglang-ppu-mooncake` | 只支持返回 503，decode 不能独立完成 prefill | 不提供这项配置 |
   | `vllm-ppu-mooncake` | 只支持返回 503（降级未验证） | 不提供这项配置 |

   模式、TP、PP 与 KV 加载失败策略启动后不能改，需要克隆部署；提交前确认这些选择。

5. 昇腾模式的 KV 走所选的加速器网络：910B/910 系列使用 NPU 自带的 RoCE 网口，A3/910C 灵衢网络使用 HCCS。跨机部署时这些节点必须共用一个加速器 RDMA 或加速器 HCCS 网络（见
   [配置节点网络](configure-networks.md)），否则被拒绝；prefill 与 decode 所在的 NPU 还必须在对应网络上互通。RoCE 用
   `hccn_tool -i <卡号> -ping -g address <对端 IP>`，HCCS 用 `hccn_tool -i <卡号> -hccs_ping -g address <对端 IP>` 验证；`hccn_tool -i <卡号> -lldp -g` 可查看 RoCE 口的对端。

   没有交换机、两台机器按卡号直连时，P、D 要选择实际互通的同号卡。
   可先在网络**内部测试**中运行**卡连通性 · 全卡矩阵**确认布线；部署校验不会替你验证卡间链路。

### PPU 经 RDMA 传 KV

`sglang-ppu-mooncake`、`vllm-ppu-mooncake` 用 Mooncake 经主机的 RoCE/IB 网卡直接读写卡的显存，所以：

- 节点必须在一个**主机 RDMA** 参数网络上，单机部署也一样：只有这样实例才拿到 `/dev/infiniband`、不限量的锁页内存和
  `IPC_LOCK`，KV 走该网络声明的 HCA（SGLang 的 `--disaggregation-ib-device`、vLLM 的 `device_name`）。不满足时拒绝
  `PD_NETWORK_RDMA_REQUIRED`。
- 每个节点要加载 PPU 的 peer memory 模块 `alixpu_peermem`，否则网卡注册卡的显存失败（Mooncake 日志
  `Failed to register memory ... Bad address`）。在节点上执行：

  ```bash
  modprobe alixpu_peermem && echo alixpu_peermem > /etc/modules-load.d/alixpu_peermem.conf
  ```

  后半句让它开机自动加载。节点详情的「健康检查」中 **GPUDirect RDMA** 一列显示每张卡的状态（未加载是警告，卡照常可调度），
  **诊断** 报告也会列出这一项。手动放置到未加载的节点被拒绝（`PD_PEER_MEMORY_MISSING`，带修复命令）；自动放置跳过这些
  节点，跳过后放不下时返回 `PD_PEER_MEMORY_NONE`，说明有几个节点因此被排除。
- vLLM 的 MooncakeConnector 在 prefill 的回复里不带交接参数，网关按 prefill 实例的 KV 端点自己生成
  `kv_transfer_params`（`remote_engine_id`、`remote_bootstrap_addr` 和每个请求一个 `transfer_id`）交给 decode。
- 两个模式都开启后不能休眠：KV cache 显存注册给网卡后醒来申请不回（`PD_SLEEP_RDMA_UNSUPPORTED`）。

6. 预览按实例给出放置、显存与完整命令。KV Offload 只在 prefill 上生效，decode 不挂 KV 缓存层；不要在自定义参数里
   写 `--kv-transfer-config` 或 `--kv-offloading-size`，平台会拒绝。

7. 提交部署，在详情中确认 P、D 各角色达到期望的就绪数，再检查 prefill 实例时间线中的交接自检结果。
   通过 Playground 或 `/v1/chat/completions` 发起请求，并在请求详情确认 prefill 实例与降级原因。
   对顺序交接的 vLLM 模式，还能看到「PD Prefill」阶段；出现降级原因时，即使回复成功，也不能据此确认分离生效。

API 形式：`POST /api/v1/deployments` 的 `placement.pd`，字段见 [API 参考](../reference/api.md)。

## 自动放置与手动放置

PD 部署与单机、多机一样有两种放置方式：

| 方式 | 谁选节点和卡 | 怎样用 |
|---|---|---|
| **自动** | 平台挑节点（创建时按空闲卡挑，单元启动时放不下再换）；卡由调度器在启动时现挑，不写进放置 | `placement_mode: "auto"`；`placement.pd` 里的实例可以不写 `node_uuid`，写了的实例留在原处，平台只补没写的 |
| **手动** | 你。每个实例的节点（以及可选的卡）都由你写，并按此钉死 | 省略 `placement_mode`，每个实例写 `node_uuid`，需要时写 `gpus` |

升级前已有的 PD 部署都是手动。部署详情的 `placement_mode` 给出当前方式；`PUT /api/v1/deployments/{id}/placement-mode` 可以在两者之间切换，不重启实例。切到自动会去掉所有实例钉住的卡，之后由调度器启动时挑。

### 自动放置怎样挑节点

P 与 D 要交换 KV，所以整个 PD 部署的实例都在同一张计算网络、同一种 GPU 架构里挑，一次为所有没写节点的实例挑完：

1. 先选组：RDMA 网络优先于以太网，再看哪一组里已有模型文件的节点多。
2. 组里按放置策略分配，PD 默认 `binpack`：
   - **集中（`binpack`）**：只要有一台节点放得下全部实例所需的卡，就把全部实例放在这一台，KV 不经过网络；没有这样的节点，就尽量填满已经用到的节点，用最少的节点。
   - **分散（`spread`）**：每个实例取空闲卡最多的节点，prefill 与 decode 尽量不在同一台。
3. 已有模型文件的节点在两种策略里都优先。

挑中的节点要先过 PD 的常规校验（模式、TP 规则、参数网络能否承载 KV）。没有一组放得下时，创建返回 409 `UNIT_PLACEMENT_NONE`，参数写明各项检查排除了多少台节点。

每个自动放置的实例必须一台节点放得下；带 `followers`（跨节点）的实例要自己写节点，请求里有这样的实例而又没写节点时返回 400 `AUTO_PLACEMENT_PD_MULTI_NODE`。

评估接口对自动放置的 PD 请求返回 `auto_placed: true`，预览显示此刻会挑中的节点；创建时重新挑一次。

### 实例启动时的重新挑选

挑中的节点写进了放置，之后实例照常按它们匹配、解析网络、节点失联重调度和驱逐。一个实例需要启动而没有运行中的实例时，平台用当前的空闲卡试算一次：

- 节点都在线且放得下：照原样启动。
- 节点已停止调度、不再满足节点条件，或者启动会被拒绝（卡被其他部署占了）：只把这个实例放不下的 rank 换到别的节点，同一个部署的其他实例不动，也不重启。时间线记 `UNIT_PLACED`（`role`、`unit`、`from`、`to`、`reason`）。
- 没有地方可去：保留原节点，继续等待，每分钟再找一次。
- 节点离线仍走[节点失联重调度](node-failover.md)的等待时间，驱逐仍走先起后切，两者自动与手动一样。

### 手动放置钉住的卡被占

手动放置钉住的卡被其他部署占着时，实例不会去用同一节点上的其他空卡，而是等到钉住的卡空出来。此时 `DEPLOYMENT_UNSCHEDULABLE` 的调度原因是 `pinned_gpus_unavailable`：它给出节点、钉住的卡号，以及占着这些卡的部署，而不再只写「空闲整卡不够」。处理方法：停掉或迁走占卡的部署，或把这个部署切成自动放置，由平台改用空闲的卡。钉住的卡被平台外的进程占着、不健康，或者卡空着却仍放不下（显存、共享）时，沿用调度器原来的原因。

## 海光 DCU

`vllm-mooncake` 使用 vLLM 自带的 MooncakeConnector，KV 由海光为 DTK 编译的 Mooncake 传输引擎搬运。

- **镜像**：必须是 vLLM 0.21 的海光内置镜像 `gpuworks/gpuworks-runner:dtk26.04-vllm0.21.0-r3`，内置模板的 K100 / BW 启动规格用的就是它。
  `-r1` 的连接器拉取 KV 失败时不报错：decode 仍返回 200 且内容是错的，`kv_load_failure` 不生效，失败的块还会留在前缀缓存里；r2 修复了这一点，PD 部署不要再用 r1。
  `-r2` 不支持下面的混合模型。
  vLLM 0.18.1 的海光镜像没有 Mooncake，不能用于 PD。
- **传输协议**：平台按 P、D 共用的参数网络选择：主机 RDMA 网络用 `rdma`，以太网、管理网络或同机用 `tcp`。
- **TP**：prefill 与 decode 必须相等。
- **GDN 混合模型**（Qwen3.5、Qwen3.6、Qwen3.8 等含线性注意力或 Mamba 层的模型）：需要 r3 镜像。
  vLLM 配了 KV 连接器就会关掉混合 KV cache 管理器，这类模型没有它起不来，平台按模型 `config.json` 的 `layer_types` 自动加
  `--no-disable-hybrid-kv-cache-manager`；部署自己写了 `--disable-hybrid-kv-cache-manager` 或 `--no-disable-hybrid-kv-cache-manager` 时以部署为准。
  KV 按页整页传输，prefill 只算到倒数第二个 prompt token，最后一个由 decode 重算，所以 decode 的 `local_compute` 每个请求多 1 个 token。
  r2 上这类模型 P、D 两端都启动失败（`Hybrid KV cache manager is disabled but failed to convert the KV cache specs to one unified type`）。
- **交接**：prefill 实例启动一个 bootstrap 服务（占该实例的一个额外端口），各 TP rank 把自己的地址登记在那里。
  网关第一次用某个 prefill 实例前，先向这个服务查询它的 engine id 并缓存；每个请求再生成一个 transfer id，
  分别写进发给 prefill 与 decode 的 `kv_transfer_params`，decode 据此到 prefill 拉取 KV。
  查询失败或还有 TP rank 没登记时，网关跳过这个 prefill 实例；所有实例都不可用时按 `fallback` 处理，原因记为 `prefill_no_bootstrap`。
- **自检**：连接器不导出传输字节数，交接自检只根据 decode 是否正常回答判断，传输方式记 `unmeasured`。
- **休眠**：与 NIXL 一样，可休眠的部署不能使用主机 RDMA 参数网络。
- **验证状态**：已在海光 K100 上验证（同机两卡、TP 1，Docker 与 runveil）；跨机、`rdma` 协议与 TP > 1 未验证。
  GDN 混合模型在 K100 上用 Qwen3.5-0.8B 与 `hygon/Qwen3.8-27B-Channel-INT8-w8a8` 验证（同机两卡、TP 1、`tcp`）：输出与不开 PD 的部署逐字一致，
  拉取失败时 `fail` 返回 500 且 decode 继续服务，`recompute` 重算出正确结果；BW 上未验证。

## 运行中

### 状态

部署显示 **未就绪 / 部分就绪 / 运行中**，按每个角色的期望数与就绪数推导；有意缩到 0 的 prefill 不算缺。

### 自检

每个 prefill 实例首次就绪后，平台自动经网关做一次交接，在该实例的时间线记
`PD_HANDOFF_VERIFIED`（传输方式为 rdma、tcp、local 或 unmeasured）；参数网络是 RDMA 却没有 RDMA 流量时记
`PD_HANDOFF_NOT_RDMA`，交接失败记 `PD_HANDOFF_FAILED`。

### 扩缩

在部署详情打开 **调整角色**，修改各角色的实例（`PUT /api/v1/deployments/{id}/pd`）。被去掉的实例先排空再停。

- 自动放置的部署新增实例时可以不写节点：平台把它们放进已有实例所在的那张网络、那种架构里，并按放置策略挑。写了节点的实例按你写的放，并和其他实例一起过 PD 的常规校验。没有节点放得下新实例返回 409 `UNIT_PLACEMENT_NONE`。
- `vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake` 支持将 prefill 缩到 0，此时请求有意全部由 decode 完成，不计为降级。
  这与仍期望 P 实例、但 P 故障时按 `fallback` 选择降级或返回 503 不同。
- `sglang-nixl`、`sglang-ppu-ipc`、`sglang-ppu-mooncake`、`vllm-ppu-mooncake` 的 P、D 都必须保留实例。

### 重试一个实例

一个实例连续失败达到上限后平台不再重建，部署详情会列出它的失败次数与最后一次原因，并给出
**重试**：只清掉该实例的失败记录并重新创建，不影响其他实例。在 **调整角色** 里删掉一个实例，它的失败记录也随之清除，
再加回来就重新计次。

### 镜像与网络变化

新增或重建的实例按当前的镜像与参数网络启动，与在跑实例不同也不拦（例如逐个换成新镜像做
灰度，或网络割接）；两边不兼容或连不通时交接会失败，由自检（`PD_HANDOFF_FAILED`）与降级率反映出来，必要时整组重启。

### 重启

部署详情页头的 **重启**（`POST /api/v1/deployments/{id}/restart`，所有部署形态通用）按当前解析重建全部实例，每个实例的
重试次数重新计算。默认逐个替换（其余实例照常服务，一个实例恢复后才换下一个）；选 **全部一起重启** 时停掉全部实例后一起重建，期间服务中断。

### 监控

服务页的 PD 卡片给出：

- **PD 有效性**：decode 的提示 token 中，经 KV 传输得到的比例；分母包括传输、本地计算和缓存命中的 token。
  偏低不能单独证明交接失败，应结合 P 的期望数量、休眠状态、降级原因和交接自检结果判断。
- **降级率与主要原因**：降级请求数占网关请求数的比例，并按原因汇总。`vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake` 主动将 P 缩到 0 或全部休眠时，
  请求由 decode 独立完成，不计为降级。
- **prefill 阶段平均与 P95**（网关等 prefill 实例的时间）、prefill 实例的排队、平均提示长度；
- **KV 传输**：平均 / P95 传输时间、平均大小、吞吐、失败与过期次数，取自 vLLM NIXL 连接器的 `vllm:nixl_*` 指标，按次传输统计、
  不对应单个请求。

  失败是 decode 拉取 KV 失败的次数；过期是 prefill 实例算好的 KV 在租约（vLLM 0.30 默认 30 秒，decode 收到请求后
  持续续约）到期前没被拉走、只好释放的请求数，说明 prefill 之后请求没有到达
  decode（客户端断开、交给 decode 失败）或 decode 实例出了故障。

  `vllm-ascend-mooncake` 与 `vllm-mooncake` 的连接器不导出这些指标，卡片显示「引擎不提供」；`vllm-ascend-mooncake` 的单次传输时间只在 decode 实例日志里
  （`KV cache transfer for request … took … ms`）。

### 请求

#### 查看详情与色条

请求详情的 PD 一节给出 prefill 实例、prefill 阶段、该实例自报的排队与 Prefill、降级原因；
还在 prefill 阶段的请求也出现在在途表里。字段定义见[请求时间线与阶段拆分](../reference/request-timeline.md#6-pd-分离部署)。

vLLM 模式都采用顺序交接：请求视图在「网关」之后单列「PD Prefill」段；端到端延迟拆解里同一段直接叫「Prefill」。decode 实例自报的排队显示为
「KV 传输·排队」（vLLM、vllm-ascend 与 vllm-hcu 都把等远端 KV 的时间算在排队里），自报的 Prefill 显示为「首 token 重算」；延迟拆解里
它只占端到端平均的 1% 或更少时不画出。

#### 判断缓存命中

顺序交接请求的缓存命中（以及返回给客户端的 `cached_tokens`）取 prefill 实例的值，因为 decode 把拉来的 KV 也算作命中，见 [OpenAI 兼容网关](../reference/openai-gateway.md#用量usage)。

#### 查看 Trace

顺序交接导出的 OTel trace 里 prefill 阶段是 `pd.prefill` 子 span。
`sglang-nixl`、`sglang-ppu-ipc`、`sglang-ppu-mooncake` 并发调用 P、D，网关不等待 P 回复，因此没有独立的 `pd.prefill` 子 span。

### 评测

默认经网关测端到端；指定某个实例时直接测该实例，结果只代表一个普通副本。

## 排查

- **降级率高**：看网关计数的原因（`prefill_unreachable`、`prefill_5xx`、`prefill_no_kv_params`、`prefill_no_bootstrap` 等）与 prefill 实例日志。
  `prefill_no_kv_params` 通常说明 prefill 的 KV 连接器没有生效；`prefill_no_bootstrap` 只出现在 `vllm-mooncake`，说明网关问不到 prefill 实例的
  bootstrap 服务，或还有 TP rank 没有登记上去（见[海光 DCU](#海光-dcu)）。
- **实例曾就绪，后来请求持续失败或降级**：先看实例事件和日志。agent 会定期检查运行中的容器，
  容器退出或消失后按失败处理，并由控制器按重试策略重建；达到重试上限时使用**重试**。
  该检查不等于持续的 HTTP 健康探测：容器仍运行但引擎无响应时，应核对日志和请求错误，必要时重启或调整角色替换实例。
