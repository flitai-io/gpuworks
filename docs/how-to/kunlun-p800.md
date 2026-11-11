# 昆仑 P800 部署

## 当前能力

P800 PCIe / KL3 使用 `kunlunxin` 厂商和 `xre` 框架，已验证的运行时为 Linux amd64 上的 Docker 和 nerdctl；runveil 尚未开放。该组合支持整卡 TP（Tensor Parallelism，张量并行）、PP（Pipeline Parallelism，流水线并行）和引擎 DP（Data Parallelism，数据并行）。镜像与模型须支持所选组合；跨节点部署另须满足下文通信前提。PD、分区和 sleep 尚未开放。多个平台副本、引擎 DP 和跨机 TP 是不同拓扑。

复用宿主已有 Docker daemon。独立 socket/data-root 不隔离宿主网络；不要在同一网络命名空间另起使用 `--bridge=none` 的 dockerd，这可能删除已有 `docker0`。镜像或推理测试不需要重启现有 Docker，离线探针使用容器级 `--network none`。

原生探测使用 `xpu-smi -q -x`。只兼容已观察到的 process_info 内 XML 闭合标签错误，其他 XML 错误仍失败；未知 ECC 状态显示遥测告警，不据此拒绝调度；已知不可纠正 ECC 错误使该卡不可调度，其余卡继续上报；PCI 清单补充 CLI 未列出的卡。只读库存可以使用命令缓存，设备注入必须重新核对 UUID、PCI BDF、minor、sysfs 字符设备；占用由公共计算进程清单持续上报。

启动准入对手写未知命令 fail closed：无法确认命令是否请求受限功能时，评估/创建返回 `LAUNCH_FEATURES_UNKNOWN`，调度拒绝使用 `launch_features_unknown`，不把未知命令当成未请求这些功能。昆仑必须显式选择至少一张卡，空选不会自动获得全部 XPU。设备注入以本次选卡为准，先清理模板或用户参数中由平台接管的 XPU/XDR 设备映射与冲突 runtime 选项，再注入本次设备；`/dev/infiniband` 等不受该后端管理的设备映射保留。

GPUWorks 单机设备接入只核对并注入所选 XPU 和控制设备，不要求 XDR、RDMA 设备、IPC_LOCK 或 memlock 放宽。单机多卡的镜像与卡间通信组合仍须单独验证；这不等于承诺任意镜像都能运行。单机多卡 BKCL 集合通信不能使用 `lo`：启动装配使用节点管理网络上报的真实非 loopback 接口，agent 派生 `BKCL_SOCKET_IFNAME`；缺少可用接口时在容器创建前明确失败，单卡不受此限制。

## 镜像与模型档位

“镜像 → 系统”提供 `vLLM · XRE`，框架版本 `5.0.21.46`、vLLM `0.25.1`、Linux amd64。内置档位使用不可变引用 `gpuworks/gpuworks-runner:xre5.0.21.46-vllm0.25.1-r1`，通过镜像目录的公共副本获取；模板不依赖私有仓库登录。发布该档位前须完成[镜像发布条件](../../build/runner-images/xre/README.md)。

“推理模板”内置 Qwen3-0.6B 和 Qwen3.8-27B 的 P800 档位。搜索模型并选择 Qwen 组织，即可查看“昆仑 P800 · BF16 权重 / FP16 计算”。这两个档位保留官方 BF16 权重，加载时使用 FP16 计算，不是量化权重，也不表示镜像中的所有算子支持 BF16。选择其他计算精度前须验证对应模型和镜像。27B 档位标注“纯文本”，使用 `--language-model-only`，不表示视觉路径已验证。

这两个内置档位仅接受一张 96 GiB `P800 PCIe`，不将通用 TP/PP/DP 能力当作这两条模型配置的多卡验收；P800 OAM 尚未验证，不能通过修改卡名或放宽谓词视为兼容。

27B 的默认上下文为 8192 token（输入与输出合计），最多并发一个序列；更长上下文需重新评估显存并验证。权重来源可选固定 revision 的 Hugging Face 或 ModelScope 原版。

该配方将 `--max-num-batched-tokens` 设为 256，长输入需要多轮分块预填充，因此首字等待会随输入长度增加。8192 的上下文上限不代表长输入能一次完成预填充；提高分块预算前需重新验证显存及图配置。

新部署的 Qwen3.8-27B r1 档位默认关闭 `enforce_eager`，使用下面的 PIECEWISE 配置，包含 18 个 `splitting_ops`。该默认配置面向单张 96 GiB P800 PCIe、8192 token 上下文、最多 1 个序列，capture size 固定为 `[1]`。`backend: eager` 是编译配置中的后端名称，不表示 `enforce_eager` 已打开。

Qwen3-0.6B 的 eager 默认不变；这不是所有昆仑部署的全局默认。模板更新不自动修改已有部署。若需回退 eager 模式，须恢复完整旧 eager 配置并重新评估；仅将 `enforce_eager` 设为 On，不代表它与下面的 Graph 配置组合已验证。

```json
{
  "mode": 3,
  "cudagraph_mode": "PIECEWISE",
  "backend": "eager",
  "cudagraph_capture_sizes": [
    1
  ],
  "pass_config": {
    "fuse_norm_quant": false,
    "fuse_act_quant": false
  },
  "splitting_ops": [
    "vllm::unified_attention_with_output",
    "vllm::unified_mla_attention_with_output",
    "vllm::mamba_mixer2",
    "vllm::mamba_mixer",
    "vllm::short_conv",
    "vllm::linear_attention",
    "vllm::plamo2_mamba_mixer",
    "vllm::qwen_gdn_attention_core",
    "vllm::gdn_attention_core_xpu",
    "vllm::olmo_hybrid_gdn_full_forward",
    "vllm::kda_attention",
    "vllm::sparse_attn_indexer",
    "vllm::rocm_aiter_sparse_attn_indexer",
    "vllm::deepseek_v4_attention",
    "vllm::hpc_rope_norm_forward",
    "vllm::unified_kv_cache_update",
    "vllm::unified_mla_kv_cache_update",
    "vllm::gdn_attention_core"
  ]
}
```

登记本地权重时选择包含 `config.json` 和权重分片的模型目录，填写与档位相同的仓库身份 `Qwen/Qwen3-0.6B` 或 `Qwen/Qwen3.8-27B`；注册不会解压 `tar` 或复制权重。部署时选择已登记模型与昆仑芯目标，确认档位、镜像和计算精度，再评估、创建。内置目录不包含模型权重或镜像文件，离线节点须预先准备两者。

已有私有环境仍可使用[可导入模板包](../../build/inference-templates/kunlun-p800/README.zh-CN.md)。在“推理模板 → 导入模板”上传该包；不要上传 `docker save` 镜像归档。导入包只添加用户档位，直接锁定私有镜像摘要，不替换内置档位，也不会自动新增镜像系列。需要登记其他已验证镜像时，在“镜像 → 自定义 → 添加自定义镜像”选择 vLLM、XRE、实际框架版本及 amd64。系统目录和自定义目录分别展示。

1. 在两节点准备相同的已验证镜像。记录不可变摘要、引擎版本及镜像中的实际 Python/入口路径，不以同名 CUDA 兼容字段判断原生运行能力。
2. 登记本地权重时填写完整仓库标识，例如 `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`。目录短名不是仓库标识；已有相同文件可以登记规范身份，无需复制或下载。
3. 导入 spec 时，`weights.sources[].repo_id` 必须与模型 `source_id` 对应，`gpus.vendor` 为 `kunlunxin`，卡名谓词匹配 `P800 PCIe`。缺少 spec 或身份不对应会导致 Web 无兼容档位。
4. spec 使用镜像中已验证的直接入口 `/opt/vllm025-venv/bin/vllm serve`，明确 dtype、上下文和显存参数。agent 统一注入容器逻辑编号的 `XPU_VISIBLE_DEVICES`，模板中的 `CUDA_VISIBLE_DEVICES=${LOCAL_VISIBLE_DEVICES}` 也使用容器逻辑编号，不能把宿主编号当作容器逻辑编号。
5. 单卡 spec 可明确固定 TP=1；跨机 spec 的 placement resolver 应核对每节点卡数，并把全局 TP 留给部署。两节点各两卡的请求使用 TP=4、PP=1、nnodes=2，而不是在 resolver 返回 TP=2。

## 单机 PP 与 DP

PP 按层分割一份模型，各 stage 传递中间激活；DP 启动多份模型副本并分发请求，每个副本仍按 TP × PP 切分权重。vLLM 的总卡数为 TP × PP × DP，增加 DP 不会减少每个副本的权重显存。平台副本数控制独立部署单元，不代替引擎 DP。

在 Web 中先选两张空闲整卡。PP2 使用 TP=1、PP=2、DP=1；DP2 使用 TP=1、PP=1、DP=2（高级参数中的“数据并行数”）。核对评估结果确实分配两张卡、预览命令包含相应参数，再创建。它们均不要求 XDR；切换为多机仍执行 XDR 和计算网络检查，不因使用 DP 而绕过。

DP 镜像必须正确处理每个 rank 已分片的设备映射：当 `assigned_physical_gpu_ids` 已指定时，不能再次对本地卡号加 DP 偏移。否则第二个 rank 会在单卡映射中查找编号 1 而失败。使用包含该修复的精确镜像；仅放开平台能力门控不能修复镜像。单机入口不应继承构建机的网卡名称；agent 统一校验并注入 BKCL 接口，模板不再设置 BKCL 通信参数。

## 双机通信前提

- 两节点须具备与内核匹配且可工作的 XDR，这是多机的硬性前置：`/dev/xdrdrv` 必须是真实字符设备，缺失或类型不对时该 rank 在设备注入阶段直接失败，报 `accelerator runtime unavailable: Kunlun multi-node runtime requires XDR`，整个多机单元随之失败。节点本身仍会上线、被探测、被调度，单机也照常可用，所以这个前置不满足时只有多机会坏，排查前先在每个节点上确认 `modinfo xdr` 有输出且 `/dev/xdrdrv` 存在。agent 只验证和接入设备，不安装、编译或加载驱动，不改变 KASLR 或启动项。
- 已配置可达的计算网络，并确认网络清单中的 RDMA 端口、接口和 IP 对应。网络清单不改变主机网络，也不代表链路带宽已测定。
- 主机 RDMA 计算网络由通用网络层提供 `/dev/infiniband`、无限 memlock、`IPC_LOCK` 和 `GLOO_SOCKET_IFNAME`；昆仑 prober 注入所选 XPU、控制设备和 XDR，并派生 BKCL 所需的 XDR/verbs 开关及 `BKCL_SOCKET_IFNAME`，不枚举全部 HCA 或重复添加网络参数。部署环境另须提供所选卡的 RDMA netdev 列表 `BKCL_RDMA_NICS`。XDR 必须为真实字符设备；多机启动缺少这些已解析资源或 netdev、或设置有限 memlock 时明确拒绝。
- `IPC_LOCK` 与 memlock 放宽仅用于通信内存注册。模板和宿主挂载遵循通用管理员信任模型，不应额外声明其他 GPU 或设备目录。

镜像须提供对应 RDMA 用户态库及已验证的通信参数。平台不设置绕过驱动检查的全局默认项，不修改内核启动参数。使用前须核对供应商支持的内核、驱动和镜像组合。

## 验证与边界

内置单卡配置不代表多卡或跨节点的模型与通信组合已验证。PP 和引擎 DP 是已开放能力，使用前仍须验证所选拓扑；PD、sleep/wake 和 runveil 当前不在该镜像的可用范围内。

真实硬件验收应先验证每机单卡，再验证集体通信和双机部署。Web 创建时选择正确档位、两个节点和 TP=4/PP=1；核对每 rank 命令、模型/镜像获取来源，再验证 health、普通 HTTP、SSE DONE 和网页文本。仅握手成功或收到 HTTP 200 不等于返回有效文本。

相关支持边界见 [厂商支持矩阵](../reference/support-matrix.md)。

硬件目录显示 `Kunlunxin P800 PCIe`，厂商存为 `kunlunxin`；spec 匹配前 `NormalizeGPUName` 去掉品牌前缀，因此已审核的 `^P800 PCIe$` 谓词仍可匹配。硬件身份随运行时与模型组合的验证进入能力选择。Partition 可用性由动态分区能力检查给出，不设静态 Partition 功能轴。引擎 PD 模式仍由引擎 Profile 定义；厂商限制只定义在 accelerator 能力表，`ProfileForTarget` 根据它派生该目标可用的 PD 模式。

模型目录保留 Qwen3.8-27B 的多模态元数据；试验场根据正在服务的实例启动参数识别 `--language-model-only`，该部署只显示文本输入，同一权重的其他视觉部署不受影响。
