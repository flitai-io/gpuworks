# AMD 接入结构与硬件验收

详细实现设计见 [AMD 支持设计](amd-support-design.md)。

AMD 接入复用其他加速器的 Prober、PCI 库存和 DriverAwareInjector 接口。PCI 库存仅把绑定 `amdgpu` 的 AMD 功能识别为卡；agent 随后读取 amdgpu sysfs/KFD 事实并显式传入所选 render 设备。NVIDIA 保持 Toolkit 的容器设备编号语义，昇腾保持按运行时选择的设备注入。

自动探测保留原有厂商优先级。混合 NVIDIA/AMD 主机，以及安装了残留或兼容版 `nvidia-smi` 的 AMD 主机，应通过节点加速卡选择接口显式选择 `amd`；单个 agent 只管理所选厂商。

## 职责与支持边界

- accelerator 统一声明厂商、宿主要求和功能约束。AMD 的物理 UMA 不等于 Apple 的按内存共享策略，仍按整卡分配。
- deploy.RequestedLaunchFeatures 归一服务端的启动需求；planning 和 scheduler 按模板匹配后的实际候选卡准入。混合厂商节点不整体套用 AMD 限制。
- agent 复核所选设备的身份和字符设备号，通过通用 RuntimeInjection 返回新增项、清除项或拒绝原因；engine 在验证成功后统一应用。AMD 要求非空设备选择；空选择在 agent 校验阶段拒绝，失败不留下部分修改。
- sleep 的平台准入、引擎声明与协议执行分层。AMD 与 NVIDIA 共用 vLLM sleep 协议（L2 分步唤醒），已在 MI300X 上验证。

AMD 使用公共 Prober、DriverAwareInjector 与目标约束接口，不依赖其他厂商的驱动、探测器、拓扑注入或模型模板。共享层只保存宿主事实、能力声明和拒绝合同；AMD 的 KFD/render 映射与 ROCm 设备注入由自己的 prober 完成。缺失 OS、架构或运行时事实报告 runtime_unreported，已知不兼容报告 runtime_unsupported，两者都不能授权启动。模板候选按实际匹配的卡检查；混卡节点上的 NVIDIA 卡不能替代仅匹配 AMD 的卡通过校验，模板允许 NVIDIA 时仍可选择该卡。所有厂商复用 resolved launch 的功能快照；显式受限目标不能用未知命令绕过准入。

当前代码实现的准入范围为 Linux amd64、Docker/nerdctl/runveil、整卡（单机或 vLLM 多机）；支持声明按镜像、模型和目标设备分别验收，不能从一组测试推广到所有 AMD GPU。数据并行与 NVIDIA 走同一路径，显存分区见 `amd-dgpu`；sleep 与 PD（`vllm-nixl`）需按镜像与网络分别验收；多机与流水线并行需按网络与 RCCL 版本分别验收。系统 RAM 不自动成为可调度 GPU 显存。

容量探测对 AMD395 的 UMA 仅启用受严格事实约束的窄路径：PCI device `0x1586`、KFD `gfx1151`，且唯一 KFD 节点与 DRM render minor、PCI domain/location 一致；KFD 公共 bank 必须是单个 system-memory heap，且其容量与同一 DRM 设备的 GTT total 完全相等。满足时上报该 KFD/GTT pool 容量并用 GTT used 采集占用；它表示驱动公开给 GPU 的可分配 pool，不是板载 VRAM，也不是主机总 RAM 或 HIP 当前 free。AMD395 UMA 事实缺失或冲突时上报未知容量并降级，不能将较小的 dedicated carve-out 当作已验证的健康容量。其他 AMD/APU 不从 `local_mem_size=0` 推断 UMA。

ROCm 镜像配方见 [build/runner-images/rocm](../../build/runner-images/rocm/README.md)。官方 runner 矩阵中 vLLM 档已发布并进入内置镜像目录，SGLang 档仍是 `catalog: false` 的未发布候选。组件检查和镜像拉取成功不代表 GPU 推理、模型质量或隔离通过；先核对镜像的 gfx 目标、摘要和模型版本。

## 主机准备与导入

1. 记录 GPU 型号、PCI BDF、OS/kernel、amdgpu/ROCm 和容器运行时版本。APU 还须记录固件、UMA/carveout 设置及主机可用内存。不要伪造 gfx 版本绕过不匹配。
2. 确认运行时用户可访问 /dev/kfd 和所选 /dev/dri/renderD*。核对 /sys/class/kfd/kfd/dev、PCI render 节点的 sysfs dev 与实际字符设备的 major/minor。设备 symlink、映射歧义、身份重复或库存身份变化应拒绝注入。
3. 比较 GPUWorks 上报的 sysfs VRAM 与 ROCm 实际可用容量。128 GB 系统内存或固件 carveout 配置不能证明 27B 模型及运行时、KV cache 能装下。
4. 在目标存储接收、校验镜像归档，再载入 agent 使用的 Docker daemon 或 nerdctl namespace，核对 tag 与 image ID。导入镜像目录元数据不会传输镜像层。
5. 使用产品的 agent 安装流程接入节点，运行 `gpuworks-agent doctor` 检查驱动、KFD、render 设备与运行时，再运行 `gpuworks-agent smoke` 做当前节点的受限检查；记录实际命令与结果。从内置目录选择对应 `arch_id` 的 AMD 规格部署，保持默认引擎命令模式。
6. 准备完整且版本固定的权重和 tokenizer。核对最终模型挂载、端口、dtype、TP 与镜像；检查模板 shm/memlock 和放置偏好已进入启动计划。

## 运行时与模型验证

每次只启动一个引擎，保留日志，并在测试机设置资源限制。

1. 先在镜像内执行小规模 GPU 分配和 FP16/BF16 矩阵运算，同步 GPU，记录设备/gfx、可用与总显存以及结果校验值。
2. 使用对应模板的保守设置。gfx1151 Qwen3-0.6B 和 Qwen3.8-27B 模板采用 BF16、TP1 和 4096 上下文；vLLM 限制为单并发请求，SGLang 限制为 4 个并发请求。Qwen3-0.6B 的显存比例为 vLLM 0.25、SGLang 0.40；Qwen3.8-27B 两个引擎均为 0.75。vLLM 使用 eager，SGLang 使用 Triton attention 并禁用 CUDA graphs。验证相关内核前，两者均禁用 AITER。
3. 要求完成启动、/health 和有界的文本聊天请求。调整上下文或显存比例前保留原始失败日志。文本通过后单独验证图片输入；不能据此推断视频支持。
4. 通过 GPUWorks 重复部署，检查容器只获得 /dev/kfd 与所选 render 节点；验证模型路径、端口、库存/指标、停止、重启和恢复。独立手工启动若映射整个 `/dev/dri`，不能证明平台实现了逐设备选择。
   停止后健康探测历史仍可显示，但当前可用副本数为零时不得把部署标为当前健康。
5. 多卡主机验证选卡可见性与独立实例；单机 TP 只用匹配的镜像和模型验证。单 APU 上把跨卡用例标为不适用。

内置 AMD 规格使用精确的 `gpus.arch_id`（`gfx942` 或 `gfx1151`）。架构 ID 缺失或与规格不符时，模板匹配必须拒绝，即使名称包含 `gfx1151`；名称匹配不能替代可信的探测事实。该 selector 缩小候选范围，但不能证明实际 GPU 内核执行或型号 SKU，仍须完成真机验收。

## W7900 单卡候选档

用**推理模板 → 导入模板**上传 [gfx1100 候选 YAML](../../build/inference-templates/amd-gfx1100/qwen3.yaml)。它为 Qwen3-0.6B 和 Qwen3-8B 各添加一个待真机验证的 vLLM 规格，每个引擎只使用一个节点和一张卡。首个验收目标是 48 GB Radeon PRO W7900，其他上报 gfx1100 的 AMD 卡也能匹配。

1. 使用 Linux amd64 和已有的 Docker 运行时。在节点详情对照 rocminfo 的 gfx1100、PCI 地址和独立显存。记录内核、amdgpu 版本、runner 镜像 digest 与实际权重 revision。
2. 从 Web 的 Qwen3-0.6B 详情选择 RDNA 3 gfx1100 候选档，分配整张 W7900，保留默认的吞吐优先与自动上下文设置。批处理、前缀缓存和图执行由 vLLM 按模型和硬件选定。等待一个就绪副本。通过已授权的 Docker endpoint 检查该实例的设备列表包含 /dev/kfd 和被选卡的 render 节点。确认容器内 HIP 只枚举所选卡；有第二张卡时核对其 PCI 地址未出现。
3. 使用部署页面显示的网关地址和服务模型名，向 /v1/chat/completions 发送简短文本问题，分别以 stream=false 和 stream=true 检查非空、合理的回答和流式结束。记录首次就绪耗时、GPU 显存与错误日志。短文本通过后，用并发请求记录总生成吞吐、首 token 延迟与显存峰值。
4. 停止部署，确认副本清零、容器退出、已用显存回落；重新部署并再次聊天。再用 Qwen3-8B 重复上述步骤。发生错误时保留实际启动参数、dtype、attention 后端与第一条引擎错误；单项结果只覆盖该模型、镜像和设备。

## 记录证据

每个引擎保留 image ID/归档摘要、宿主与驱动事实、模型版本、实际命令和设备映射、GPU/主机内存观测、健康检查及请求结果，以及所有跳过或失败项。CPU 环境中因缺少 HIP 设备而不能完成的检查，仍是待验项。

软件 fixture 覆盖字段损坏、部分库存、重复身份和设备号不一致。不要修改真实 sysfs 或设备节点来制造失败。注入检查用于落实分配结果，不构成管理员自定义容器配置的安全沙箱。
