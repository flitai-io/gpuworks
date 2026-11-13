# dGPU 是什么、又不是什么

dGPU（dynamic GPU）把显存预算和必要的运行库注入容器，使同一物理卡上的多个实例互相受到显存上限约束。
它不是虚拟出两张拥有独立故障域、固定带宽和固定算力的 PCIe 设备。

各后端用的实现与上游：NVIDIA 是 [HAMi-core](https://github.com/Project-HAMi/HAMi-core)（`libvgpu.so`）；昇腾是 openEuler [ubs-virt](https://atomgit.com/openeuler/ubs-virt) 的 vcann-rt（`libvruntime.so`），属于软件层的逻辑限额，不是昇腾 `npu-smi` 的 vNPU 驱动层切分；PPU 与 AMD 分别是 ppu-slicing-shim（拦截 PPU 的 HGGC 运行时与 HGML）和 rocm-slicing-shim（`libvrocm.so`）；海光用驱动原生的 vdev。上游源码版本与本仓库的补丁见 `build/runtime-assets/manifest.json`。

- NVIDIA 由 HAMi-core 实现：libvgpu.so 与 CUDA 环境变量；配额是硬显存限制。HAMi 的算力上限按 kernel 发射限流，
  对普通 kernel 是封顶，但 vLLM 默认的 CUDA graph 与 cuBLAS 不受它限制，多个推理实例同卡时不能靠它按比例分配算力，
  平台因此不开放算力上限。
- PPU ppu-slicing-shim 在长上下文、高并发负载下能把 20/80 配置落实为接近目标的繁忙时吞吐权重；对端
  空闲时会借用余量，因此不是固定 SM 分区。
- 昇腾 vcann-rt/device-share 受宿主机权限和 runtime 组合影响。当前 Gitee 云环境只能证明节点
  探针或整卡对话，不能把云侧拒绝写成 dGPU 通过。
- 海光 Z100 / K100_AI / BW 使用原生 vdev（实验性）：每实例一张卡，显存配额同时决定分配的 CU 数；不能独立调整算力，也不保证推理吞吐与显存配额成比例。支持 x86_64 上的 Docker 或 runveil/udocker，需原生 helper、宿主库并通过逐卡行为探针；仅报告 K100 或其他未适配型号时仍只支持整卡。
- AMD 由 rocm-slicing-shim（libvrocm.so）实现（实验性，用户态，HIP 运行时层拦截）：配额是硬显存限制。ROCm 的算力配额由
  `HSA_CU_MASK` 在硬件层实施，低于任何预加载库能看到或校验的层面，平台因此不对 AMD 开放算力上限——
  这与 NVIDIA 的「技术上开放不了」是同一类限制，但实现机制不同。支持 x86_64 上的 Docker、nerdctl 或 runveil；
  dGPU 已在 MI300X（gfx942）的 Docker 和 runveil 下实测，其他型号与组合未验证。

server 从硬件事实、资产和行为探针推导 partition capability 的 status/reason；agent 不自行
宣布 ready。能力不满足时仍可整卡部署，但带 dGPU 显存限额的请求必须明确失败（如调度器返回
partition_unavailable），不能静默去掉配额。NVIDIA 与 AMD 豁免行为探针，仍须满足资产、架构和运行时条件；昇腾、PPU 与海光须逐卡通过行为探针。平台和环境边界见[支持矩阵](../reference/support-matrix.md)。

## 改配额：平台立即生效，引擎看自己

HAMi 每次分配、每次回答「显存总量多少」都现读实例共享区里的上限，所以平台改配额是当场改：实例的期望状态与调度记账
按新上限，agent 把新上限写进共享区（[调整 dGPU 配额](../how-to/dgpu.md#调整-dgpu-配额)）。

引擎能不能用上是引擎自己的事：
vLLM、SGLang 只在启动时按看到的上限切一次 KV 缓存，调大要重启实例才用得上；按需分配的引擎直接用上。调低到引擎已占用量
以下会让它下一次分配失败而退出，所以这种实例保持原上限，到重启时再按新上限启动。

昇腾 vcann-rt、PPU ppu-slicing-shim 与 AMD rocm-slicing-shim 只在容器启动时
读上限，实例同样要重启。海光 Z100 / K100_AI / BW 也需重启实例，先释放旧 vdev，再按新的显存配额与 CU 数重新创建。

## 调度怎么算一个 dGPU 实例

一个 dGPU 实例在物理卡上记为「显存上限 + 后端允许留在上限之外的部分」，调度准入和 GPU 页的
已分配显存用的是同一个数。NVIDIA 的 HAMi-core 把引擎看到的显存总量改成上限，引擎自己的
预留（CUDA graph、激活等）都花在上限之内，上限外只剩 HAMi 看不到的 CUDA context：实测每个
进程约 400 MiB，按 512 MiB 记。尚未实测上限外开销的后端（昇腾、PPU、海光、AMD）仍按整卡部署的引擎预留记，宁可
多扣一点，也不让一张卡超卖。

请求没有指定后端时，由落到的节点用它自己就绪的后端（同时有本厂商后端和 NVIDIA 后端时优先
前者）；指定了就只能是那一个。自动放置在选出节点之前不知道是哪台，所以总是不指定后端；
部署表单在自动放置下按候选节点能提供的后端决定能不能开 dGPU，而不是按调度器选中的节点——
每张卡上都已经有实例时，整卡部署选不出节点，正是最需要切分的时候。
