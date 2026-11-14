# 厂商支持矩阵

| 厂商/框架 | 探测 | 整卡部署 | dGPU | 拓扑/备注 |
|---|---|---|---|---|
| NVIDIA / CUDA | nvidia-smi、sysfs | Docker、runveil、nerdctl | HAMi libvgpu.so（x86_64，glibc ≥ 2.17） | 支持 CUDA/NCCL 拓扑；驱动库由宿主机提供 |
| 昇腾 / CANN | npu-smi、驱动和 CANN 事实 | Docker + Ascend runtime；部分环境 runveil | vcann-rt；能力取决于宿主机 device-share 权限 | 外层/内层 CANN 版本不能混为一个结论 |
| AMD / ROCm | amdgpu sysfs、KFD | Linux amd64、Docker/nerdctl/runveil/Kubernetes 上的整卡部署；上游 vLLM ROCm 镜像已在 MI325X（gfx942）与 AMD395（gfx1151）单卡 BF16 文本推理实测，其余组合逐档待验 | amd-dgpu 显存配额（rocm-slicing-shim，用户态；已在 MI300X/gfx942 的 Docker 与 runveil、MI325X 的 Kubernetes 下实测；Docker/nerdctl/runveil/Kubernetes） | 多机与 PP 经 vLLM mp 后端运行（RCCL 读取 NCCL_* 网络变量，已在两台 MI325X 上实测 PP=2 与跨机 TP=2）；vLLM sleep/wake（L1/L2）与 PD（`vllm-nixl`，以太网 UCX TCP）已在 MI300X 上实测；这不是对所有 AMD 卡或 ROCm 组合的兼容性承诺 |
| PPU / HGGC | ppu-smi | provider/runtime 依赖 | ppu-slicing-shim 显存配额与加权 QoS（Docker、Kubernetes） | 20/80 长负载已证明繁忙时吞吐权重，不是硬 SM 分区 |
| 海光 / DTK | hy-smi | 由目录 spec 和 runtime 决定 | Z100 / K100_AI / BW 原生 vdev（实验性，x86_64，Docker 或 runveil/udocker） | 需可选原生 helper、宿主库和逐卡行为探针；其他型号仍只支持整卡 |
| Apple / Metal | sysctl、ioreg（macOS 15+，Apple silicon） | host（引擎为宿主机进程，镜像是 darwin 环境包） | 无；一台 Mac 按统一内存同时跑多个实例 | 1 张统一内存卡，显存为 Metal 工作集；不支持引擎休眠、多机、KV 磁盘层；见 [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) |
| 昆仑芯 / XRE | xpu-smi XML、PCI/sysfs 设备身份 | Linux amd64、Docker/nerdctl/Kubernetes、P800 PCIe / KL3、整卡 TP / PP / DP | 不支持 | TP、PP、引擎 DP；跨节点须满足通信前提；不开放 PD、sleep、runveil；镜像和模型使用显式目录 spec |

上表的 dGPU 能力状态由 server 的 partition capability（分区能力判定）推导：

- `ready`：dGPU 可用。
- `unavailable`：dGPU 不可用，附带 `reason` 说明原因，例如“缺少 dGPU 资产”“dGPU 探针失败”。

dGPU 不可用时，节点仍可使用整卡部署；平台不会把不满足 dGPU 条件的实例静默改成不限制显存的实例来启动。
昇腾实测组合、CANN 和 runtime 细节见[昇腾矩阵](ascend-support-matrix.md)；PPU 未完成项见[开发台账](../dev/ledgers/ppu-support-gaps.md)。

Kubernetes 节点（见[接入 Kubernetes 集群](../how-to/connect-kubernetes.md)）支持 NVIDIA、昇腾、海光、PPU、AMD 与昆仑芯的整卡部署；dGPU 支持 NVIDIA HAMi-core、昇腾 vcann-rt、PPU ppu-slicing-shim 与 AMD rocm-slicing-shim，海光原生 vdev 在 Kubernetes 上不可用（仍只做整卡部署）。


## dGPU 能力对比

| 后端 | 显存限制 | 算力语义 | 修改显存配额 | 行为探针 |
|---|---|---|---|---|
| NVIDIA HAMi-core | 硬显存上限 | 平台不开放算力上限；CUDA graph、cuBLAS 不受 HAMi 发射限流约束 | 支持安全在线调整；引擎可能仍需重启才能利用新额度 | 依赖内嵌资产校验与准入检查，豁免行为探针 |
| 昇腾 vcann-rt | 显存配额，依赖宿主 device-share 权限 | 受 vcann-rt 与宿主组合影响，不据此承诺固定吞吐比例 | 重启实例生效 | 必须逐卡通过 |
| PPU ppu-slicing-shim | 显存配额 | 繁忙时加权 QoS，空闲时可借用余量；非硬 SM 分区 | 重启实例生效 | 必须逐卡通过；Docker 与 Kubernetes |
| 海光 Z100 / K100_AI / BW 原生 vdev（实验性） | 原生 vdev 显存配额；每实例一张卡 | CU 随显存比例分配，不支持独立设置算力配额 | 重启实例生效，并按新显存配额重建 vdev | 必须逐卡通过；x86_64 上的 Docker 或 runveil/udocker |
| AMD rocm-slicing-shim（用户态） | HIP 运行时层拦截的硬显存上限 | 平台不开放算力上限；ROCm 通过 `HSA_CU_MASK` 在硬件层实施算力配额，低于预加载库能看到或校验的层面 | 重启实例生效（限额只在 HIP 运行时初始化时读取一次） | 豁免行为探针（宿主与轻量探针镜像都不保证有 ROCm 用户态运行库可供探针调用），依赖内嵌资产校验；Docker/nerdctl/runveil/Kubernetes |

在线调整的安全条件与待重启原因见[调整 dGPU 配额](../how-to/dgpu.md#调整-dgpu-配额)。
表中能力不代表任意推理镜像均已验证；节点是否可用仍以该卡的 `status` / `reason` 为准。

## AMD 整卡实现边界与验收状态

当前 agent 和 server 实现的 AMD 主机接入范围是 Linux amd64、Docker、nerdctl、Kubernetes 或 runveil（udocker 驱动）、整卡部署（单机或 vLLM 多机）；Kubernetes 上同时支持整卡与 dGPU 显存切分。PCI 库存仅把绑定 `amdgpu` 且具有匹配 KFD 计算节点的 AMD PCI 功能识别为卡；agent 再读取其 amdgpu sysfs/KFD 事实，按 PCI BDF 和可用的 unique ID 核对设备库存。缺少驱动绑定、KFD 或明确的 render 映射时不应推断可部署；运行时只注入所选卡的 render 节点及 `/dev/kfd`。dGPU 显存分区（`amd-dgpu`，libvrocm，仅显存、不含算力限额）在设备注入之上叠加 `LD_PRELOAD`/配额环境变量，详见上节「dGPU 能力对比」；dGPU 支持 Docker、nerdctl、Kubernetes 与 runveil（PRoot 同样绑入 libvrocm.so 并经 `LD_PRELOAD` 加载）。runveil 把宿主整个 `/dev` 与 `/sys` 绑进容器，HIP 能枚举到所有卡，因此 agent 用 `ROCR_VISIBLE_DEVICES=GPU-<unique_id>`（ROCm 的 GPU UUID）锁定所选卡，缺 `unique_id` 的卡在 runveil 下拒绝启动；PRoot 以非 root 运行用户执行，并自动带上设备节点所属的组（如 render），已在 MI300X 上实测。数据并行（DP）与 NVIDIA 走同一路径。sleep 与 PD 沿用 vLLM 的协议与 `vllm-nixl` 模式；有 RDMA 时平台只指定 HCA、不写 CUDA 专用的 `UCX_TLS`，由 UCX 选择 ROCm 传输，这一路径未经真机验证。多机与 PP 走 vLLM mp 后端：RCCL 与 NVIDIA 一样读取 `NCCL_SOCKET_IFNAME`、`NCCL_IB_*`，agent 按节点地址填入网卡，无 RDMA 时关闭 IB。此范围描述 GPUWorks 当前实现，不代表所有 AMD GPU、驱动、内核或 ROCm 用户态组合都兼容。

内置镜像目录提供 `gpuworks/gpuworks-runner:rocm7.2.3-vllm0.30.0-r1`（基于上游 `vllm/vllm-openai-rocm`，ROCm 7.2.3、vLLM 0.30.0），内置推理目录为 Qwen3 0.6B–32B 与 30B-A3B 提供 `arch_id: gfx942`（MI300X/MI325X）的 BF16 vLLM 规格，为 0.6B–8B 提供 `arch_id: gfx1151`（Ryzen AI Max+ 395）的规格；选模型即可部署，无需导入镜像或模板。实测覆盖：MI325X 单卡与两台 MI325X 的 PP=2、跨机 TP=2，AMD395 单卡；只证明短文本推理与部署/停止流程，不代表回答质量、持续性能、长上下文或图片输入已验证。SGLang ROCm 档仍是 `catalog: false` 的未发布候选。UMA 容量按实际 KFD/GTT 事实读取，不可推广为其他 APU 或主机的可用 GPU 内存。`gpus.arch_id` 是精确、大小写不敏感匹配；缺失或不一致的架构事实不会通过。验收步骤见 [AMD 硬件验收指南](../dev/amd-hardware-validation.md)与 [ROCm 镜像配方](../../build/runner-images/rocm/README.md)。

## AMD dGPU 显存配额

`amd-dgpu` 后端用 libvrocm.so（rocm-slicing-shim，`build/runtime-assets/amd/`）在 HIP 运行时层拦截分配与容量查询接口，只链接 libc，一份构建适配任意 ROCm 版本。注入在 AMD 整卡设备选择（render 节点 + `/dev/kfd`）之上叠加：`LD_PRELOAD` 指向挂载的 `/opt/gpuworks/partition/libvrocm.so`，`VROCM_DEVICE_MEMORY_LIMIT_<N>`（MiB，`N` 为容器自身可见设备列表里的位次，不是物理序号）按所选每张卡写入，`VROCM_LEDGER_PATH` 指向宿主机 `/dev/shm` 下的实例专属账本（`/dev/shm/gpuworks-amd-<实例 UUID>`），该路径在 Docker、nerdctl 与 runveil 上不变，容器靠 `--ipc=host` 共享宿主机的 `/dev/shm`。Kubernetes 上 agent 自己的 `/dev/shm` 是 agent Pod 私有的 tmpfs，不是宿主机的：agent 经宿主 root（`/proc/1/root`）在宿主机 `/dev/shm` 预先创建这同一个账本路径（空文件，0666，不是 0777：容器可能以宿主机未知的 UID 运行，需要能写但不需要可执行），再把该宿主路径以 hostPath 原样挂进实例 Pod（可写），`VROCM_LEDGER_PATH` 的值不因此改变；libvrocm.so 仍按普通方式打开并在干净退出时 unlink 该文件，agent 的清理步骤兜底经宿主 root 删除同一路径。限额只在 HIP 运行时初始化时读取一次，调小配额需重启实例才生效。

没有算力（SM/CU）配额：ROCm 用 `HSA_CU_MASK` 在硬件层强制算力配额，低于任何预加载库能看到或校验的层面，因此该后端只做显存、永不暴露算力上限。

与 PPU/昇腾/海光不同，AMD 整卡注入不挂载任何宿主 ROCm 用户态库（只给 `/dev/kfd` 与所选 render 节点），轻量探针镜像也没有 HIP 运行时可调用，因此这个后端豁免行为探针（`SkipsBehaviorProbe`），就绪只看内嵌资产的 sha256 校验与 Docker/nerdctl/udocker/Kubernetes 运行时约束——这与 NVIDIA HAMi-core 的做法一致，但不同于 PPU/昇腾/海光逐卡探针通过才算就绪。

## PPU 设备身份与 dGPU 探针

PPU 使用 `ppu-smi` 查询并经设备节点校验的身份，按选卡顺序分别注入两份数据：`CUDA_VISIBLE_DEVICES` 为计算程序使用的数字设备编号，`GPUWORKS_PPU_DEVICE_UUIDS` 为探针和 HGML 配额库使用的完整 UUID 列表。Docker/nerdctl 的数字编号来自过滤后的设备视图，udocker/runveil 使用宿主编号；两份列表的顺序一致。vLLM 和 SGLang 共用数字选择，不增加引擎判断。HGML 复用 UUID 顺序定位配额与显存账本；新 UUID 变量缺省时兼容旧 Agent 的设备选择变量。

内嵌探针 `gpuworks/partition-probe:v4` 同时检查目标 UUID 和显存配额。缺失、无效、重复或不可查询的身份，以及只有其他卡显示配额的情况都不能通过。升级 agent 后需重新创建已有工作负载，新的设备选择与切分库才会生效；runveil 的设备可见性仍不等于内核设备隔离。

`HGGC_LEDGER_PATH` 指向宿主机 `/dev/shm` 下的实例专属账本（`/dev/shm/gpuworks-ppu-<实例 UUID>`），账本路径与到达实例 Pod 的机制同 AMD（见上节「AMD dGPU 显存配额」）：Docker 上容器靠 `--ipc=host` 共享宿主机的 `/dev/shm`；Kubernetes 上 agent 经宿主 root 预先创建同一路径，再以 hostPath 挂进实例 Pod，路径本身不变。PPU 的 dGPU 分区目前支持 Docker 与 Kubernetes；udocker 尚未验证。

## 海光整卡驱动库

海光整卡工作负载通过结构化挂载将宿主 `/opt/hyhal` 传入容器，Docker、nerdctl 与 udocker/runveil 共用该挂载；例如 DTK 26.04 的 PyTorch 需要其中的 `librocm_smi64.so.2`。节点缺少该目录时，设备注入会提前报错。容器继续使用镜像自带的 DTK 与库搜索路径，升级 Agent 后需重新创建工作负载以应用挂载修复。挂载声明为只读；PRoot 的绑定沿用宿主运行账户权限，不提供内核级只读隔离。

选卡变量 `HIP_VISIBLE_DEVICES` 与 `HYGON_VISIBLE_DEVICES` 按容器内 HIP 看到的编号填写。Docker 与 nerdctl 只传入所选卡的 DRM 节点，HIP 在容器里把它们编为 0..N-1，因此填容器内序号：宿主第 2 张卡在单卡容器里是 `0`。udocker/runveil 绑定宿主整个 `/dev` 与 `/sys`，HIP 能看到全部卡，因此保留宿主序号。升级 Agent 后需重新创建工作负载才会使用新的编号。

部分机器的卡之间不支持 P2P，海光 vLLM 的 custom all-reduce 在 TP > 1 时可能启动失败（`HIP error: invalid device pointer`），可在额外参数里加 `--disable-custom-all-reduce`。

## 海光 Z100 / K100_AI / BW dGPU

Z100、明确识别为 `K100AI` 的 K100_AI，以及型号原样报告为 `BW` 的卡使用 `hygon-dgpu` 后端；`K100_AI`、`K100 AI` 是同一 SKU 的别名。`BW` 不被改写为 `BW1000`，只报告系列 `K100` 时仍不能据此启用 dGPU。agent 通过 `hy-smi virtual -d N --show-device-info` 读取所选物理卡的 CU 总数，再按显存比例向上取整分配 CU；查询失败或结果不完整时拒绝创建 vdev，不回退到固定 CU 数。启用前，在节点构建并安装可选的 `gpuworks-hygon-vdev-helper`（构建步骤见源码的 `build/runtime-assets/hygon-vdev-helper/README.md`），并提供宿主 DTK、HYHAL 和 DCGM 库。DTK 26 环境的 RSMI 库可以位于 HYHAL 的 `lib` 目录。agent 本身仍是静态二进制。支持 x86_64 上的 Docker 和 runveil/udocker；nerdctl 及其他型号不会自动获得此能力。

每个 dGPU 实例只能使用一张受支持的海光卡。请求显存向上取整到 MiB 后必须大于零且严格小于该卡物理显存；CU 数按 `ceil(配额 MiB × 物理卡 CU 总数 / 物理显存 MiB)` 分配，不提供独立算力配额，也不承诺同等比例的推理吞吐。修改配额后需重启实例，释放旧 vdev 后按新配额重新创建。

当前兼容性配置列出 DTK 26.04.x × x86_64，但该配置尚未标记为整套真机验证通过，版本信息仅作兼容性提示，不能替代逐卡探针。

容器配置使用与原生 ID 一致的 `/etc/vdev/docker/vdev<ID>.conf`，且只暴露一份 Docker 配置。Z100 和 K100_AI 的真机验证表明，将非零原生 ID 改名为 `vdev0.conf` 会导致 HIP 初始化失败；旧版 HYHAL 1.4 环境尚未重新验证原生文件名，仍须通过逐卡探针。Docker 轻量探针使用私有 `/var/log/hylog` tmpfs，并按宿主实际存在的 SONAME 补挂 tinfo5/6。

runveil/udocker 为每个实例单独绑定整个 `/etc/vdev` 视图，里面只有该实例的原生配置及 Docker 同名配置，不暴露其他 ID 或归属账本；HYHAL 日志绑定到每实例可写的 sticky `1777` 目录。两种 launcher 复用同一份结构化挂载。PRoot 不提供独立 cgroup 或内核只读挂载，工作负载沿用宿主运行账户权限，不能据此声称敌对程序的安全隔离。BW Notebook 已验证同一个外层 cgroup 中两份不同额度的 HIP context 同时存在、数据保持、超限拒绝，以及停止一个后另一个继续工作；节点仍须通过自己的逐卡探针。

逐卡探针必须确认 HIP 看到请求的显存配额、可分配 1 MiB，并拒绝超额 1 MiB 的分配。探针通过后才能调度 dGPU 实例。推理容器使用自己的 DTK；宿主 DTK 与 ABI 补充库只供轻量探针使用。内置 Qwen3-0.6B 的 `bf16-vllm-hygon-z100` spec 使用经过 Z100 验证的 SourceFind 镜像；DTK 26.04 的 vLLM spec 也接受原样报告的 `BW`。

普通用户可以使用 root 预先创建的 vdev。镜像必须提供运行 UID 对应的用户记录和可写的 HYHAL 日志目录；工作负载必须能读取配置快照、DTK/HYHAL 库，并访问 `/dev/kfd`、`/dev/mkfd` 和所选 DRM 设备；宿主 DTK 若通过符号链接落在 `/root` 下，普通用户仍可能被目录权限阻挡。K100_AI 的 HYHAL 1.5.0 CLI 创建和删除 vdev 均要求 root，因此非 root 工作负载可用不等于整个 agent 的 vdev 生命周期可在非 root 下运行。无需给推理容器 root 来满足宿主侧的生命周期权限。

helper 在宿主锁下创建 vdev，并保存配置快照和归属令牌，阻止旧请求释放后来复用相同 ID 的 gpuworks vdev。容器移除成功后才释放 vdev；清理失败时保留记录，实例维持 `removing`，同一实例不会启动替代容器。agent 重启后不会自动扫描遗留记录，需人工核查；同机其他 vdev 管理工具若绕过 helper 的锁与令牌，不受此归属保护。

## GPU 型号与架构

server 收到节点上报时把型号名规范化后再存（`accelerator.NormalizeGPUName`）：去掉开头的厂商词
（`NVIDIA`、`Ascend`、`Huawei`、`Hygon`、`AMD`、`Apple`、`Kunlunxin`，后面须跟空白），首尾空白去掉、中间空白合并。
`NVIDIA GeForce RTX 4090` 与 `GeForce RTX 4090` 都存为 `GeForce RTX 4090`，`Ascend 910B4` 存为 `910B4`。
推理目录的名字谓词、PD 的同型号检查、GPU 列表读的都是这个值；原始名字只留在 agent 侧（`doctor`、日志）。
已有的 benchmark 快照保留当时的名字。

架构只存原始事实：NVIDIA 存 compute capability，其他厂商在 `arch_id` 存能确定的最具体身份。
代际（API 的 `arch_family`）与 NVIDIA 的 `sm_XY`（API 的 `arch_id`）由 `accelerator.GPUArch` 在读取时算出，
不入库；agent 上报的 `arch_family` 字段 server 不读。

| 厂商 | `arch_id`（存储） | 代际（推导，小写） |
|---|---|---|
| NVIDIA | 不存（由 compute capability 得 `sm_XY`） | `maxwell` 5.x、`pascal` 6.x、`volta` 7.0/7.2、`turing` 7.5、`ampere` 8.0–8.7、`ada` 8.9、`hopper` 9.x、`blackwell` 10.x–12.x |
| 昇腾 | 芯片型号：`910B4`、`910B2C`、`910C`、`310P3` | `910b`、`910c`、`310p`；初代 910 与认不出的为空 |
| 海光 | SKU：`K100AI`、`BW`、`BW1000`；只报系列时存系列 `K100`；认不出为空 | `k100`、`z100`、`bw`、`bw1000`、`bw1100` |
| PPU | `ZW810E`（取自名字） | `beethoven`（`ppu-smi` architecture 列） |
| AMD | KFD `gfx_target_version`，如 `gfx90a`、`gfx942`、`gfx1151`；缺失时为空 | 暂不推导 |
| Apple | SoC 名：`M1 Max`、`M4 Pro`（`ioreg` 的 model 去掉 `Apple`） | 芯片代际 `m1`…`m6` |
| 昆仑芯 | `kl3`（原生探测的 product_architecture 转小写） | `kl3` |

agent 探测不到任何加速卡（各厂商 CLI 都不应答、都没装、也没有厂商设备文件）时节点厂商为空，不再按 NVIDIA 上报。

昆仑芯使用 `kunlunxin` 厂商与 `xre` framework；CUDA 兼容版本不作为 CUDA 身份或 XRE 版本。
能力由 `internal/accelerator` 统一声明，按候选卡及所选卡判断。P800 支持整卡 TP、PP 和引擎 DP，不开放 PD、分区或 sleep；所需宿主为 Linux amd64 和 Docker/nerdctl，或 Kubernetes 节点（整卡，经 agent 的 device plugin 取得设备节点），runveil 尚未开放。不兼容的卡返回结构化放置拒绝，不阻止继续尝试其他候选。2026-11-10 已实测单卡生产启动、网关 chat、设备覆盖清理与 agent 重启并通过；单机多卡越过 loopback 接口问题后仍受测试环境 XRE OAM/XLink 初始化错误阻断，跨机未测，不能据此声明多卡或跨机已验收。

昆仑必须显式选择非空卡组；手写未知命令按 `LAUNCH_FEATURES_UNKNOWN` / `launch_features_unknown` fail closed。启动时先清理模板或用户参数中由后端接管的 XPU/XDR 设备映射与冲突 runtime 选项，再注入本次选卡，保留无关设备映射。单机多卡还需非 loopback 集合通信接口，启动装配使用节点管理网络上报的真实接口，agent 派生 `BKCL_SOCKET_IFNAME`；缺失时在容器创建前拒绝，单卡不受此限制。

原生 XML 中单卡身份或 ECC 异常只使该卡不可调度，其他卡继续上报；缺失卡由 PCI 清单补充。设备注入重新查询并核对 UUID、PCI BDF、minor 与 sysfs 字符设备号，只自动注入所选 `/dev/xpuN` 和 `/dev/xpuctrl`。计算进程占用通过公共清单上报，显存为零不代表空闲。

单机设备接入不要求 XDR 或 RDMA。多机 TP 要求每个 rank 的宿主都有 XDR（`/dev/xdrdrv` 为真实字符设备），缺失则该 rank 在设备注入阶段失败；`/dev/infiniband`、无限 memlock、`IPC_LOCK` 与 `GLOO_SOCKET_IFNAME` 由主机 RDMA 计算网络提供；昆仑探针追加所选 XPU、控制设备与 XDR 字符设备，并派生 BKCL 开关和 `BKCL_SOCKET_IFNAME`，不枚举 HCA，也不重复添加网络已解析的参数。部署环境另需提供 `BKCL_RDMA_NICS`。已解析参数缺少这些资源或 netdev、或把 memlock 设成有限值时，多机启动直接拒绝而非补注入。平台不加载驱动或变更宿主网络。

容器参数遵循公共管理员模板契约；平台选卡注入不是对任意特权参数、镜像或宿主挂载的安全沙箱。
agent 注入容器逻辑编号的 `XPU_VISIBLE_DEVICES`，镜像入口的 `CUDA_VISIBLE_DEVICES` 也使用容器逻辑编号。镜像和模型需显式导入适配的目录 spec，详见 [昆仑 P800 部署](../how-to/kunlun-p800.md)。
