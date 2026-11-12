# 怎么选：dGPU、sleep/wake、多机、副本、PD

已经能跑通第一个模型（见[从零启动并部署第一个模型](../tutorials/getting-started.md)）之后，下一个问题通常是：
一张卡要不要分给多个模型、模型放不下怎么办、流量大了加什么、要不要上 PD。这些特性名字相近、
解决的问题不同，也不能随意叠加。本文按"解决什么问题 / 代价与限制 / 什么时候不该用"逐个讲清楚，
操作步骤不在这里重复，统一链到对应的操作指南。

厂商相关的结论以[厂商支持矩阵](../reference/support-matrix.md)为准；矩阵和代码都没有给出验证
结论的组合，本文一律写"没有验证结论"，不替你推断能用。

## 决策表

| 你的情况 | 首选 | 备选 / 叠加 | 详见 |
|---|---|---|---|
| 一个模型要吃满一张卡，追求单实例性能 | [整卡独占](#整卡独占默认)（默认） | — | [部署模型](../how-to/deploy-models.md) |
| 几个小模型要**同时在线**，各有固定显存预算 | [dGPU](#dgpu显存配额) | 每个实例再开"可休眠"，见[组合速查](#组合速查) | [启用和排查 dGPU](../how-to/dgpu.md) |
| 几个模型**轮流使用**，任一时刻只有一两个在服务 | [sleep/wake](#sleepwake多个模型轮流用一张卡) | 与 dGPU 叠加（NVIDIA 教程已覆盖） | [sleep / wake](../how-to/sleep-wake.md) |
| 单卡放不下，但一台机器的几张卡放得下 | [单机多卡 TP](#单机多卡-tp-与多机推理) | — | [部署模型](../how-to/deploy-models.md) |
| 一台机器的所有卡都放不下 | [多机推理](#单机多卡-tp-与多机推理) | 先确认[参数网络](../how-to/configure-networks.md) | [多机推理](../how-to/multi-node-inference.md) |
| 单副本放得下，但吞吐不够或要容忍单实例故障 | [多副本](#副本数与网关路由策略) | 按流量特征选路由策略 | [部署模型](../how-to/deploy-models.md) |
| 提示很长、并发很高，decode 输出被 prefill 打断 | [PD 分离](#pd-分离) | 节点间配 RDMA 参数网络 | [PD 分离部署](../how-to/pd-disaggregation.md) |
| 手头没有 GPU 机器，或只需要短期算力 | [云厂商租机](#云厂商租机与自有节点) | 租来的机器与自有节点混用 | [租用云 GPU](../how-to/rent-cloud-gpus.md) |
| 集群不能访问公网 | [离线环境](#离线环境) | 内网镜像站 / HTTP 文件服务器 | [离线环境部署](../how-to/deploy-offline.md) |

几条总原则：

- **先整卡、后共享**。整卡是默认和最简单的形态；dGPU 与 sleep 都是为"卡不够用"准备的，带来
  额外的限制和需要核对的厂商边界。
- **能在一台机器内解决，就不要跨机**。跨机引入参数网络、防火墙和 rank 整组重建这些额外的
  故障面。
- **特性不是越多越好**。多个特性叠加时，部分组合被服务端直接拒绝，部分只给"未验证"警告，
  见[组合速查](#组合速查)。

## 整卡独占（默认）

**解决什么**：一个实例独占一张（或 TP 张）物理卡，引擎按 `gpu_memory_utilization` 使用卡上的
显存。没有任何共享层，行为最接近直接跑引擎。各厂商的整卡部署条件见支持矩阵"整卡部署"一列（PPU、海光取决于 provider / runtime 与目录 spec）。

**代价与限制**：

- 调度上，整卡请求要求这张卡当前没有其他占用；唯一的例外是休眠实例留下的少量残留，残留会从
  可用显存里扣掉，引擎启动预留仍放得下时才接受。
- 一张卡同一时刻只服务一个实例，模型小于整卡显存时多出来的显存闲置。

**什么时候不该用**：几个小模型各自只要几 GiB，却各占一整张卡，这时应考虑 dGPU 或 sleep/wake。

**详见**：[部署模型、选卡与解释调度结果](../how-to/deploy-models.md)、
[调度与显存估算](scheduling-and-estimation.md)。

## dGPU（显存配额）

**解决什么**：把一张物理卡的显存按配额分给多个实例，所有实例**同时在线**，每个实例被限制在
自己的显存上限内，互相不会吃掉对方的显存。部署时在请求里填 dGPU 显存上限即可，平台按节点
硬件选择后端。

**它不是什么**：不是把一张卡变成两张独立的 PCIe 设备——没有独立故障域、固定带宽和固定算力。
算力侧的语义因厂商而异，见下表（语义细节见[dGPU 的语义与边界](dgpu.md)）。

**各厂商的支持边界**（依据：[支持矩阵](../reference/support-matrix.md)、
[昇腾支持矩阵](../reference/ascend-support-matrix.md)和 `internal/accelerator` 中注册的后端）：

| 厂商 | dGPU | 成熟度 | 需要满足的条件 | 语义与已知边界 |
|---|---|---|---|---|
| NVIDIA | 支持 | stable | x86_64、glibc ≥ 2.17，宿主机驱动提供 `libcuda.so.1` 与 `libnvidia-ml.so.1` | 显存是硬上限；不提供算力上限（HAMi 的限流管不住 vLLM，见[语义](dgpu.md)） |
| 昇腾 | 支持 | experimental | 宿主机的 device-share 权限满足要求；推理镜像需 glibc ≥ 2.33 | 在自有环境的 910B4 上，Docker 通过了平台 dGPU 推理与超配拒绝验证，nerdctl 只通过了节点级 dGPU 行为探针；云租机（矩阵中的 Gitee 环境）无权限执行 device-share，只能证明探测和整卡对话，不能当作 dGPU 通过；`aicore-quota` 不能据此承诺 20/80 吞吐比例 |
| PPU | 支持 | experimental | HGGC 13.x、x86_64，且**只在 Docker 运行时**可用 | 20/80 配置在繁忙时表现为加权 QoS（对端空闲时会借用余量），不是硬 SM 分区 |
| 海光（DTK） | 支持 | experimental | Z100 / K100_AI / BW、x86_64，Docker 或 runveil/udocker；需原生 helper、宿主库和逐卡行为探针 | 原生 vdev，每实例一张卡；CU 随显存比例分配，不支持独立算力配额；仅报告 K100 或其他未适配型号时仍只支持整卡 |
| AMD（ROCm） | 支持 | experimental | x86_64，Docker、nerdctl 或 runveil；需 libvrocm 资产 | 用户态硬显存上限，不提供算力上限；dGPU 已在 MI300X（gfx942）的 Docker 和 runveil 下实测，其他型号与组合未验证 |

能力是否可用由 server 推导成 `ready` 或 `unavailable`（附 `reason`），agent 自己不宣布。不满足
条件时节点仍可整卡部署，但带 dGPU 显存限额的请求会明确失败（如 `partition_unavailable`），
**不会静默退回无限显存**。

**代价与限制**：

- 实例的调度占用是显存上限加后端在限额之外的预留。NVIDIA 的引擎开销已包含在限额内，调度另加 512 MiB 的 CUDA context；昇腾、PPU、海光和 AMD 保守加上完整引擎预留。限额要留出权重、KV cache 和引擎开销，过小会启动失败。详见 [dGPU 的语义与边界](dgpu.md)。
- 昇腾、PPU 与海光后端还须通过 agent 在该卡上运行的行为探针，资产、GPU 或运行时变化时重跑。NVIDIA 与 AMD 豁免行为探针，仍须满足资产校验、架构和运行时条件；具体以该卡的 `status` 和 `reason` 为准。
- dGPU 与 PD 叠加时评估会给"未验证"警告（见[组合速查](#组合速查)）；多机 + dGPU 没有验证结论。

**什么时候不该用**：

- 需要硬算力隔离、独立故障域或固定带宽——dGPU 都不保证。
- 模型本来就要吃满整卡。
- 该卡的 dGPU 能力为 `unavailable`，例如海光型号未适配、缺少资产或探针未通过，或 PPU 运行时不是 Docker。
- 只是想让几个模型"轮流"用一张卡、不要求同时在线——sleep/wake 更省心，也没有固定配额的碎片问题。

**详见**：[启用和排查 dGPU](../how-to/dgpu.md)、[一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md)、
[dGPU 的语义与边界](dgpu.md)。

## sleep/wake：多个模型轮流用一张卡

**解决什么**：让暂时不用的实例通过引擎的 sleep mode 释放显存，容器继续运行、实例状态仍是
`running`，需要时再唤醒。休眠实例在调度账本里只占它自己的残留（实测，读不到时按模型估算，约 0.75–4 GiB），其余显存可以部署给别的模型。

**与 dGPU 的区别**：

| | dGPU | sleep/wake |
|---|---|---|
| 同时在线 | 是，各自固定配额 | 否，休眠的实例不服务请求 |
| 显存占用 | 配额常驻 | 休眠时只占残留（每实例实测或估算） |
| 切换成本 | 无 | 唤醒需要时间（见下） |
| 依赖 | 各厂商的 dGPU 后端 | vLLM 的 sleep mode |

**代价与限制**：

- 唤醒耗时：L1（权重挪到主机内存）一步、秒级，但实测要钉住约 1.9 倍权重大小的主机 RAM，唤醒后也不释放；L2（权重直接丢弃）
  唤醒时从模型目录重新加载，模型文件在页缓存里时几秒，要读盘时十几秒到几分钟，不占主机 RAM。两级都会丢弃 KV cache。
- 手动休眠时的**有请求时自动唤醒**决定后续行为：勾选后网关挂起请求、自动唤醒；不勾选则保持选中实例休眠，请求不会唤醒它们；所有休眠副本都被保持、且没有清醒副本可服务时返回 HTTP 503。默认值由出让策略决定：`never` 默认不勾选，其他策略默认勾选。详见 [sleep / wake](../how-to/sleep-wake.md#休眠时在途请求怎么办)。
- 平台按[出让策略](service-levels.md)让实例睡下后，有请求时会自动唤醒。策略包括不自动出让（`never`）、闲置自动出让（`idle`）、按时自动出让（`timeslice`）和随时出让（`anytime`）；通过 sleep/wake 让共卡的部署轮流用卡，唤醒放不下时可以等位。[调度分析](../how-to/scheduling-center.md)也支持定时休眠、唤醒。
- 自动出让（非 `never` 策略）要求部署已开启**可休眠**、引擎支持 sleep，且仅适用于单节点、非 PD 部署。多机部署可以手动休眠，但不能配置自动出让。
- 唤醒要先过准入：这张卡已被新部署占用时返回 409 `WAKE_INSUFFICIENT_GPU_MEMORY`，要先停止或
  休眠占用者，或选择等位、让占用者休眠后唤醒；卡被平台外进程占着时直接拒绝（`WAKE_GPU_HELD_OUTSIDE`）。
- 休眠时可选"等待完成"（默认，先跑完在途请求）或"立即中止"。
- 只支持 **vLLM**；SGLang 部署不能声明可休眠（创建时即被拒绝：`SLEEP_ENGINE_UNSUPPORTED`）。必须在创建部署时
  打开"可休眠"。多机部署可以休眠：指令发给 leader，由它的引擎带着 follower 一起睡醒。KV 走主机 RDMA 网络的 PD
  部署不能声明可休眠（`PD_SLEEP_RDMA_UNSUPPORTED`，见 [sleep / wake](../how-to/sleep-wake.md)）。
- 自定义镜像不做探测，镜像缺 sleep 端点时休眠会在实例事件里报错。

**厂商边界**：sleep 同时受引擎、镜像、运行时和启动配置限制。Apple 节点不支持引擎休眠（[ADR 0044](../adr/0044-mac-engines-as-host-processes.md)）。除 NVIDIA 外，以下单卡组合已经过真机验证，但不能据此推断其他版本、模型或运行时：

- **昇腾 910C**：vLLM-Ascend 0.26.0rc1 的 L1/L2 均通过。平台在声明"可休眠"时自动把 `weight_nz_mode` 合并为 `0`，保留其他 `additional-config` 键；模板环境里的 `PYTORCH_NPU_ALLOC_CONF` 若含 `expandable_segments:True`，平台把它改为 `expandable_segments:False`，其余分配器设置不变，并在评估/预览里给出警告 `ASCEND_SLEEP_EXPANDABLE_SEGMENTS_DISABLED`（`params`：`variable`、`template_value`、`value`）。关闭 expandable segments 可能增加 NPU 显存碎片，KV cache 可能变小，显存余量紧的配置（例如大模型配 `gpu_memory_utilization` 0.9 以上）可能在启动或高负载时显存不足；出现时调低 `gpu_memory_utilization` 或 `max_model_len`，或关闭"可休眠"。这些大模型模板开启休眠后尚未经过真机验证。用户在附加参数里显式指定非零 NZ（`ASCEND_SLEEP_WEIGHT_NZ_CONFLICT`），或在部署环境变量里开启 `expandable_segments:True`（`ASCEND_SLEEP_EXPANDABLE_SEGMENTS`），会在创建/预览阶段被拒绝。用命令编辑器完整覆盖启动命令时，平台不注入也不检查这两项。
- **PPU ZW810E**：当前 hggc runner（vLLM 0.23.0）的 L1/L2 均通过，实际运行 V1。`/is_sleeping` 的逻辑状态可能先于实际操作完成切换，必须以 POST 完成、显存和醒后推理共同确认。
- **海光 K100**：vLLM 0.18.1 在 Docker 和 nerdctl 下的 L1/L2 均通过。平台在声明"可休眠"时为 Hygon 注入 `VLLM_CUDART_SO_PATH`，已有模板或用户环境值仍可覆盖。udocker/PRoot 路径未通过基础 Torch kernel 验证，不能用于证明此组合可用。
- **AMD Instinct MI300X**：gpuworks-runner ROCm 档（vLLM 0.30.0、ROCm 7.2.3）的 L1/L2 均通过：休眠后另一进程可申请到约 185 GiB 中的 150 GiB，L2 按分步唤醒（先恢复权重缓冲、重载权重、再恢复 KV 并清空前缀缓存）后推理正确。`amd-smi` 报告的已用显存在该 VF 上不随休眠更新，以 sysfs `mem_info_vram_used` 为准。Ryzen AI Max（gfx1151）未验证。

海光 ROCm sleep allocator 还需要宿主内存后备：在 62 GiB RAM、无 swap 的 K100 上，`gpu_memory_utilization=0.9` 曾导致 EngineCore 被 OOM killer 杀掉，降到 `0.25` 后通过。平台目前**没有针对该后备内存的专门准入**，创建前必须自行评估节点 RAM、其他实例和系统保留。以上结论均绑定所测镜像与配置；自定义镜像仍不做能力探测。

**什么时候不该用**：

- 需要多个模型同时低延迟服务。
- 主机内存不够放下 L1 所需的权重副本、又不能接受 L2 的唤醒时长。
- 用的是不支持休眠的 SGLang。
- 用 PD 分离：PD 上声明可休眠，评估会给"未验证"警告。

**详见**：[操作 sleep / wake](../how-to/sleep-wake.md)、[一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md)。

## 单机多卡 TP 与多机推理

**单机多卡 TP**：模型权重单卡放不下、但一台机器的几张卡放得下时使用。调度器枚举 TP 方案，
按卡间互联质量、GPU 打包紧凑度（TP 越小越优）和显存贴合度打分；TP 必须整除注意力头数。
卡间带宽由硬件决定（NVLink 等），这是单机方案的主要优势。

**多机推理**：一个部署的多个 rank 分布在多台节点上，解决"一台机器所有卡都放不下"。

**代价与限制**：

- 所有节点需要共用同一个参数网络，或都允许管理网承担参数同步；否则整组回退管理网（评估给
  `COMPUTE_NETWORK_FALLBACK`），或直接拒绝部署。节点间所选网络的地址上需要 TCP 双向放行。
- 默认把 TP 取为所选节点中最少的 GPU 数、PP 取为节点数，也就是张量并行尽量留在节点内、流水线并行
  跨节点；TP 也可以跨节点。
- 多机部署创建时是 **1 个副本**（`replicas > 1` 返回 `DISTRIBUTED_REPLICA_COUNT`）；创建后在部署详情的"调整副本"
  （`PUT /api/v1/deployments/{id}/replicas`）按副本列出各自的节点，扩到多个多机副本。可以休眠。
- RDMA 参数网络强制走 IB 传输：容器看不到 verbs 设备时启动直接失败，而不是静默退回 TCP（退回 TCP
  会让吞吐腰斩，这是设计成"大声失败"的原因，见 [ADR 0030](../adr/0030-network-objects.md)）。
- **厂商边界**：参数网络分四种类型——主机以太网、主机 RDMA（NVIDIA，NCCL/UCX）、加速器 RDMA（昇腾 RoCE）和加速器 HCCS（昇腾 A3/910C 灵衢）；
  昇腾多机必须共用一个加速器 RDMA 或加速器 HCCS 网络（`COMPUTE_NETWORK_ACCELERATOR_REQUIRED`）。主机 RDMA 参数网络不能用在 runveil
  （`udocker` driver）节点上（`COMPUTE_NETWORK_RUNTIME`）。其他厂商的多机推理没有验证结论。
- 一个 rank 所在节点离线会让整组失败并重建，跨机越多故障面越大。节点离线超过部署的重调度等待时间（默认 10 分钟）后，平台会只换掉失联的 rank、不消耗重试次数，见[节点失联重调度](../how-to/node-failover.md)。

**什么时候不该用多机**：单机放得下。跨机只应在显存或算力确实不够时使用。

**详见**：[多机推理](../how-to/multi-node-inference.md)、[配置节点网络](../how-to/configure-networks.md)、
[典型部署拓扑](topologies.md)。

## 副本数与网关路由策略

**副本**：同一个部署独立调度的多个副本（上限 32），每个副本可以是单 rank，也可以是一组跨节点的 rank。网关在各副本的 rank 0 实例之间分发请求。解决吞吐不足和单实例
故障。副本不增加单个请求的能力（上下文长度、单请求延迟）；每个副本占用自己的卡（或 dGPU 配额）。

**路由策略**在部署上设置（创建时的 `routing_policy`，或 `PUT /api/v1/deployments/{id}/routing`）。
它属于路由配置、不是引擎启动参数，所以**修改策略不会重启实例**（[ADR 0023](../adr/0023-inference-routing.md)）。

| 策略 | 行为 | 适合 |
|---|---|---|
| `least_request`（默认） | 选本机在途请求数最少的副本，新鲜的引擎排队数与 KV 使用率作为软惩罚；并列时随机 | 大多数场景；请求之间没有明显关联 |
| `prefix_affinity` | 相同提示前缀尽量固定到同一个副本（稳定哈希），该副本过热时溢出到其他副本。优先读请求头 `X-GPUWorks-Affinity-Key`，也可从 chat messages、prompt 或 input 提取稳定 key；该请求头不会转发给引擎 | 大量请求共享长系统提示或多轮对话，希望复用同一副本上的前缀缓存 |
| `failover` | 优先使用稳定的主副本，主副本无法接收请求时才切换 | 把副本当"热备"用，不希望流量分散 |

网关行为要点：

- 连接级失败（还没拿到上游响应）会在候选副本之间重放请求，失败的副本短暂冷却；**一旦拿到上游响应
  （包括 5xx）就原样返回，不再重试**。
- 休眠、启动中、非 rank 0 的实例不参与转发；没有可用副本时返回 503。
- 选路信息来自 server 内存里的候选快照，不在请求热路径查数据库；不做跨 server 的负载共享。

**限制与不该用**：

- 多机部署创建时副本数只能是 1，之后经"调整副本"增加；PD 部署不使用 `Replicas`，各角色的实例数在 `placement.pd` 里设置。
- 卡紧张、流量不大时不要加副本，它们会白占显存。
- 想提高单个长请求的速度，副本没有帮助，应看 TP 或 PD。

**详见**：[部署模型](../how-to/deploy-models.md)、[OpenAI 兼容网关](../reference/openai-gateway.md)、
[生产环境部署](../how-to/deploy-production.md)（网关容量与指标）。

## PD 分离

**解决什么**：把推理拆成 prefill 与 decode 两组实例。对外仍是一个部署、一个服务名，由网关先后调用
两组实例。提示长（上千 token）、并发高，prefill 抢占 decode 导致输出不稳时收益最明显。

**各厂商的支持边界**（依据：`internal/server/pdmode` 与 [ADR 0028](../adr/0028-pd-disaggregated-deployment.md)）：

| PD 模式 | 引擎 / 厂商 | KV 传输 | 验证状态 |
|---|---|---|---|
| `vllm-nixl` | vLLM / NVIDIA | UCX（走参数网络） | 已验证：同机、跨机以太、角色内跨机 |
| `vllm-ascend-mooncake` | vLLM / 昇腾 | NPU 自带 RoCE 口（HCCL） | 已验证：两台 910B4 跨机；P、D 的 NPU 必须在 NPU 网络上互通 |
| `vllm-mooncake` | vLLM 0.21 / 海光 | Mooncake 传输引擎（走参数网络，RDMA 或 TCP） | 已验证：海光 K100 同机两卡、TP 1，含 GDN 混合模型（需 r3 镜像）；跨机与 RDMA 未验证 |
| `sglang-nixl` | SGLang / NVIDIA | UCX | **未验证** |
| `sglang-ppu-ipc` | SGLang / PPU | 同机卡间 IPC（Mooncake NVLink 传输） | 已验证：PPU ZW810E 同机两卡、TP 1；P、D 必须在同一节点 |
| `sglang-ppu-mooncake` | SGLang / PPU | Mooncake RDMA（走主机 RDMA 参数网络） | 已验证：PPU ZW810E 经 RoCE 网卡，TP 1；节点须加载 `alixpu_peermem` |
| `vllm-ppu-mooncake` | vLLM / PPU | Mooncake RDMA（走主机 RDMA 参数网络） | 已验证：PPU ZW810E 经 RoCE 网卡，TP 1；节点须加载 `alixpu_peermem` |

`sglang-ppu-ipc` 只在一台主机的卡之间传 KV，放置到多个节点会被拒绝（`PD_SAME_NODE_REQUIRED`）；
它需要带 PD 补丁的 PPU SGLang 镜像（`hggc13.0-sglang0.5.13-r3` 起），并关闭重叠调度。
`sglang-ppu-mooncake`、`vllm-ppu-mooncake` 经 RoCE 网卡传 KV，可以跨机：节点必须在主机 RDMA 参数网络上（单机也一样，
否则拒绝 `PD_NETWORK_RDMA_REQUIRED`），并加载 PPU 的 peer memory 模块 `alixpu_peermem`，让网卡能注册卡的显存。
没加载的节点在节点详情「健康检查」的 **GPUDirect RDMA** 一列显示警告并给出修复命令，诊断报告同样列出；
手动选到这样的节点会被拒绝（`PD_PEER_MEMORY_MISSING`），自动放置跳过它们，一个都不剩时返回 `PD_PEER_MEMORY_NONE`。
海光只有 vLLM 0.21 的镜像带 Mooncake，0.18.1 的镜像不能用于 PD。
TP 规则按模式不同：`vllm-nixl` 要求 decode TP ≥ prefill TP，`vllm-ascend-mooncake` 要求
prefill TP ≥ decode TP，`vllm-mooncake`、`sglang-nixl`、`sglang-ppu-ipc` 与两个 PPU Mooncake 模式要求相等。

**代价与限制**：

- 至少要有 prefill 与 decode 两组实例，占用的 GPU 比单部署多；模式、TP、PP 和 KV 加载失败策略
  启动后不能改，要改只能克隆部署。
- 提示很短时 KV 传输的开销可能超过收益；服务页的 PD 卡片给出平均提示长度，可以据此判断。
- 跨机时 KV 走参数网络：RDMA 最快；以太可用，但每个请求多一次 KV 拷贝。同机 P、D 的 KV 走本机 TCP，
  适合验证，不代表跨机性能。昇腾的 KV 走 NPU 自带 RoCE 口，跨机时必须在“加速器 RDMA”网络上，该网络声明加速卡网口的连接关系。
- KV Offload 只在 prefill 上生效；不能在自定义参数里写 `--kv-transfer-config` 或
  `--kv-offloading-size`。
- 与 sleep、dGPU 叠加时评估会给"未验证"警告（`PD_UNVERIFIED_COMBINATION`）。
- `sglang-nixl`、`sglang-ppu-ipc`、`sglang-ppu-mooncake` 的 decode 不能独立计算 prefill，所以没有降级、也不能把 prefill 缩到 0；
  `vllm-ppu-mooncake` 也只支持返回 503（降级未验证）；其余模式默认
  在 prefill 不可用时降级到 decode 本地完成（`fallback: decode`），也可设为直接返回 503。
- 已知缺陷：引擎进程在实例就绪后退出时，平台暂时不会发现（`todo.md` T26），PD 下表现为网关持续降级。

**什么时候不该用**：提示很短；只有一张卡或一台机器的卡不够分成两组；硬件不是 NVIDIA、昇腾、PPU 或海光；想要
靠"副本数"扩容（PD 忽略 `Replicas`）。

**怎么确认真的在分离**：每个 prefill 实例首次就绪后平台自动做一次交接自检，在时间线里记
`PD_HANDOFF_VERIFIED`（传输方式 rdma、tcp、local 或 unmeasured）；参数网络是 RDMA 却没有 RDMA
流量时记 `PD_HANDOFF_NOT_RDMA`。服务页另有 PD 有效率与降级率。

**详见**：[PD 分离部署](../how-to/pd-disaggregation.md)、[配置节点网络](../how-to/configure-networks.md)、
[典型部署拓扑](topologies.md#f-多机推理与-pd-分离)。

## 云厂商租机与自有节点

**云厂商租机**解决"现在就要算力、不想买机器"：在"外部平台"页配置 provider 凭证（RunPod、CompShare、
AutoDL、Vast、ZStack），列出实例、确认型号/卡数/区域/价格后，创建、做 SSH 预检查并自动安装 agent。
凭证由 server 加密存储，不写入 install script 或日志。

**自有节点**解决"长期稳定运行、可以自己规划网络"：你掌握驱动、网卡与交换机，可以配置
[节点网络](../how-to/configure-networks.md)（存储同步、参数、服务）并使用带外恢复等运维手段。

**租机的代价与限制**：

- 云实例必须能访问你填的 `server_url`。server 在 NAT 后面时请给一个稳定的域名或反代地址，
  **不要用**[临时公网地址](../how-to/expose-public-address.md)；云实例无法出站到 server 时，可以在安装时
  选 `server_dial`（server 拨入实例的监听端口）。
- 云实例能用什么由厂商决定：例如矩阵中的 Gitee 昇腾租机环境无权限执行 device-share，dGPU 不可用（Gitee 不在上面的 provider 列表里，只是测试环境）；容器形态的实例没有
  Docker（或 NVIDIA 节点上 Docker 没有 NVIDIA Container Toolkit 支持）时 agent 使用 runveil，而 RDMA 参数网络与 PPU dGPU 都不支持 runveil。
- 各 provider 的 API 字段、区域约束和清理方式由各自的实现决定。文档与验收只覆盖"租机 → SSH → agent
  上线 → inventory"；云实例上的多机 RDMA、dGPU 没有系统化的验收记录，以节点页的实际探测结果为准。
- 租来的实例按厂商计价（创建前先确认价格），用完后要在"外部平台"页或厂商控制台确认已经释放。

**什么时候该用自有节点**：需要稳定的多机 RDMA 集群、长期常驻、或合规要求数据不出内网。

**详见**：[租用云 GPU](../how-to/rent-cloud-gpus.md)、[接入、升级和卸载节点](../how-to/connect-nodes.md)、
[典型部署拓扑](topologies.md#g-云厂商租机接入)。

## 离线环境

**解决什么**：集群不能访问公网或 registry 时，仍然能安装、接入节点、部署模型。

**哪些事离线可以做、怎么做**：

| 环节 | 离线做法 |
|---|---|
| 安装与升级控制面 | 发布包拷到目标机直接 `gpuworks install`；升级用 `gpuworks upgrade --base-url <内网镜像站>` |
| 接入节点 | agent 二进制内嵌在 server 里，节点从 server 的 `/install.sh` 与 `/dl/` 获取，不需要公网 |
| 推理镜像 | 用来源规则指向内网镜像仓；或联网环境 `docker save` 后放到内网 HTTP 文件服务器，把 URL 登记为镜像的获取地址、在镜像页由节点下载；或让同步组内的节点从已有镜像的节点复制 |
| 模型 | 注册节点本地的模型目录、用离线模型包、从内网 HTTP 目录导入，或在节点间同步 |
| 镜像目录与推理目录 | 系统镜像目录随 server 内嵌，不需要联网。启动时只有 server 构建与上次重放内嵌目录时不同才重放；同一构建重启会保留最近一次导入结果（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)）；要更新时在联网机器下载 manifest JSON 再在镜像页「上传 manifest」；推理目录可用目录包经 API 导入 |
| AI 助手 | 指向内网的 OpenAI 兼容端点；不配置时确定性估算与其余功能照常可用 |

**限制**：

- server 不接收、不保存镜像 tar 包；镜像只能经内网镜像仓、HTTP 归档 URL 或节点间同步到达节点。
- 需要公网的功能：从 Hugging Face / ModelScope 下载、公网 registry 拉取、云厂商租机、`gpuworks expose`。
- "离线声明"不等于 OS 级断网证明；要证明没有公网访问，需要另外做网络隔离或抓包。

**怎么选**：内网已有 registry 或镜像站时，镜像用拉取设置里的[来源规则](../how-to/configure-pull-settings.md)指向内网地址（关闭回源就不会去公网），
模型站点同样用[站点规则](../how-to/configure-pull-settings.md#站点规则模型)指向内网镜像站（关闭回源）；整个集群离线时再加两条[「禁止」规则](../how-to/configure-pull-settings.md#完全离线两条禁止规则)；节点要经代理出网用拉取设置里的「代理」。只有普通 HTTP 文件服务器时，按
[从 HTTP 文件服务器导入](../how-to/import-from-http.md)注册模型和镜像归档。

**详见**：[离线环境部署](../how-to/deploy-offline.md)、[镜像目录与推理镜像家族](../reference/image-families.md)、
[下载、缓存和节点间同步模型](../how-to/download-and-sync-models.md)、
[配置与环境变量](../reference/configuration.md)（拉取代理与出站访问）。

## 组合速查

| 组合 | 结果 | 依据 |
|---|---|---|
| dGPU + sleep（同一实例） | 可以；唤醒按"显存上限加预留"的完整占用做准入。已有 NVIDIA 教程 | [一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md) |
| dGPU + 多副本（同卡） | 可以，每个副本各占自己的显存上限；昇腾自有池有同卡双副本的验证 | [昇腾支持矩阵](../reference/ascend-support-matrix.md) |
| sleep + SGLang | 拒绝（`SLEEP_ENGINE_UNSUPPORTED`） | [sleep / wake](../how-to/sleep-wake.md) |
| sleep + 多机 | 可以；leader 的引擎带着 follower 一起睡醒 | 同上 |
| sleep + PD 且 KV 走主机 RDMA | 拒绝（`PD_SLEEP_RDMA_UNSUPPORTED`） | 同上 |
| 多机 + 副本数 > 1 | 创建时拒绝（`DISTRIBUTED_REPLICA_COUNT`）；创建后经"调整副本"增加 | [API 参考](../reference/api.md) |
| 多机 + RDMA 参数网络 + runveil 节点 | 拒绝（`COMPUTE_NETWORK_RUNTIME`） | [配置节点网络](../how-to/configure-networks.md) |
| 多机 + NVIDIA / 昇腾以外的厂商 | 没有验证结论（参数网络只建模了 NVIDIA 主机网络与昇腾加速卡网口） | 同上 |
| PD + sleep / dGPU | 允许但评估给"未验证"警告 | [PD 分离部署](../how-to/pd-disaggregation.md) |
| PD + `Replicas` | `Replicas` 不生效，实例数在 `placement.pd` | [ADR 0028](../adr/0028-pd-disaggregated-deployment.md) |
| dGPU + PPU 非 Docker 运行时 | 能力 `unavailable` | [支持矩阵](../reference/support-matrix.md) |

组合之外的"能否一起用"，以部署页评估返回的错误和警告为准：评估不预留资源，可以反复试。
