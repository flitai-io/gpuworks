# DeepSeek-V4.1-Flash-1017（Strata）

本模板提供 **ISTA-DAS 2.00 bpw（GSQ-RCO IQ2_XXS）**，使用独立的 `strata-dsv41` 引擎。模型专家分布在显存、内存和 SSD，Engram 表从内存或 SSD 读取。输入只支持文本，图像功能关闭。

当前镜像 `quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.59-deepseek41-amd64-r4` 是尚未发布的候选，基于 **Strata 应用 release v0.2.13**，源码提交为 `d2f096475817d4666c364c820b0ca830f751cfc5`，使用 **0.1.59 版原生 `strata-dsv41` 引擎**。应用 release 与原生引擎分别编号。后续上游应用/setup 已不再启动 DeepSeek；本镜像独立准备并服务固定输入。目前匹配范围仅限 RTX 8000 和 RTX 3080；其他显卡及量化未登记。此模板使用独立入口，不改变已有 Qwen Strata r5 部署。

社区文档标题使用 DeepSeek-V4.1-Flash；其固定版本的 [ISTA-DAS 2.00 bpw 配方](https://github.com/Niko1221/Strata/blob/d2f096475817d4666c364c820b0ca830f751cfc5/setup.py)实际选择 `DeepSeek-V4.1-Flash-1017-GSQ-RCO-GGUF`，与本模板一致。

## 验证范围

r4 已从固定的 v0.2.13 源码重新编译。本地镜像 ID 为 `sha256:e1baa913b829ac6187834c0963bed66bbf2bffdd0585a2c9eedf8a5588cc7408`，它不是镜像仓库的 manifest digest；镜像仍未发布。本次 GPUWorks server、agent 和 UI 从 `c495fb3986e1d0aa534e9828f8fb06933ceb6750` 构建，基于 upstream `06272566648bebad8d2bda8750ecd37431b912c9`。当前补丁已移植到 upstream `7f87d8033ad8976cb25ce55b91143997c31a8f88`。以下真机结果来自原被测提交，不代表新 upstream 已重新完成 Web 验证；镜像入口及原生引擎未变。

2026-11-17 的单卡测试使用 20 GiB 显存的 RTX 3080、Xeon Silver 4208（8 个物理核 / 16 线程）及 123 GiB 宿主机内存。主干权重与 Engram 分别放在两块 HDD 上。参数为 8K 上下文、单会话、默认 8 个 CPU 线程、保留全部 6 个路由专家；容器限额为 84 GiB、禁用 swap、12 CPU 配额。每种模式均创建新 pack，使用上游原始专家 profile。在该配置下，DSpark auto 不加载草稿模型。

默认专家自动放置模式下，两轮算术正确返回 42、50，usage 与流结束标志齐全。随后三条提示均完成，输出上限为 64 token、temperature 0、关闭 thinking、180 秒时限且不重试：

| 请求 | 输入 / 输出 token | 首 token 秒数 | 引擎 decode 秒数 | 引擎 token/s |
|---|---:|---:|---:|---:|
| 1 | 19 / 64 | 30.91 | 111.028 | 0.58 |
| 2 | 18 / 64 | 25.87 | 103.857 | 0.62 |
| 3 | 15 / 64 | 13.25 | 112.986 | 0.57 |

**默认模式直连 API 功能：PASS；首 token ≤5 秒：FAIL；持续生成 ≥10 token/s：FAIL。** 容器内存峰值 72.14 GiB，OOM 事件为零。三次性能采样的资源观测窗口中，归属于本容器的主干 HDD 读取约为 8.5–11.5 GB；该配置仍明显受未驻留专家的磁盘读取影响。

另一次 `--mmap-experts off` 测试复制了 80.8 GB 热 CPU 专家，内存峰值 83.56 GiB，OOM 事件为零。两轮算术均正确完成，但可选 GPU 观测超时导致测试脚本在三条性能请求前中止。原始 FAIL 记录保留，此模式持续吞吐为 **NOT_RUN**。随后已确认停止的容器及无其他引用的匿名卷被移除。后续测试脚本修正观测错误处理，推理与资源限额检查不变。

`--ssd-cache 0` 在本次共享主机测试中没有改善效果：算术首 token 为 135.75 / 79.05 秒，两条性能请求达到 180 秒时限，第三条在停止这组回归配置时被取消。因此此模式没有完整性能请求的吞吐结果。内存达到 84 GiB 限额，发生 532 次 max 事件，但没有 OOM 或 swap 使用，清理通过。测试期间 cgroup 计费的文件缓存量明显变化，原因尚未确定；本次结果不支持推荐该设置。

双卡 Web 实测使用 RTX 3080 20 GiB + RTX 8000 48 GiB，显式 `--mmap-experts off`，其他资源限制同上。六组件登记、内置规格部署、两轮算术（42、50）、历史恢复及流式 usage 均通过；三条性能请求各输出 64 token，首 token 分别为 3.35 / 2.52 / 2.40 秒，引擎 decode 分别为 4.43 / 4.76 / 4.64 token/s。9041 个 CPU 专家全部复制到 RAM（77.1 GB）；测试部署关闭了周期推理健康检查。容器峰值达到 84 GiB 上限，发生 3686 次 memory.max 事件，OOM 和 swap 均为零；这表明该内存限额下存在回收压力。Web 流程和定向清理 PASS，首 token 达标，生成速度未达 10 token/s。这个双卡结果不能作为单张 3080 的提速对照。长上下文、并发、取消恢复、空节点拉取镜像和获取权重仍未验证。

宿主机同时存在其他任务。模型加载与编译有重叠，没有清空系统页缓存，前序请求也会影响缓存状态。加载耗时不用于冷启动对比，这些短测试不代表硬件吞吐上限。

### 历史 r3 验证

先前 r3 使用源码 `13eedf552417a6a791a7d13c6597e24300ee3a09` 和原生引擎 0.1.59.1。2026-11-13 的 Web 全流程在 **RTX 8000 加 RTX 3080** 上通过，剩余 77.1 GB CPU 专家全部复制到内存。三条短请求达到 4.5–4.9 token/s、首 token 2.21–3.37 秒，仍未达到 10 token/s 目标。该测试使用不同镜像、双卡放置和 128-token 协议，不能作为 r4 验证或单张 3080 的版本提速对照。

## 权重与存储

GPUWorks 分别获取六个固定版本权重文件，并以只读目录挂载。镜像内置固定的 5,780,589 字节 Engram 元数据 GGUF，入口使用前校验其固定 SHA-256；该文件没有 tensor，不是完整模型，不应单独登记。镜像入口建立可写索引和运行配置。首次准备不复制整套权重，仍需为索引、日志及引擎缓存预留空间。应将外部输入放在本地 NVMe 上；从慢速盘读取未缓存专家和 Engram 行会降低速度。

| 文件组 | 字节数 |
|---|---:|
| IQ2_XXS 四分片 | 138,238,893,056 |
| 共享 Engram IQ4_NL | 110,595,290,816 |
| DSpark Q8_0 | 7,976,914,240 |
| 外部权重小计 | 256,811,098,112 |
| 镜像内置 Engram 参数元数据 | 5,780,589 |
| 全部输入合计 | 256,816,878,701 |

主模型与 Engram 来自 [ISTA-DASLab 的固定 revision](https://huggingface.co/ISTA-DASLab/DeepSeek-V4.1-Flash-1017-GSQ-RCO-GGUF/tree/cb02422c900df489cea5833e8a92171224059b3c)；DSpark 和镜像内置元数据来自 [Unsloth 的固定 revision](https://huggingface.co/unsloth/DeepSeek-V4.1-Flash-1017-GGUF/tree/8a6ec9cf118e785a362ea94ee123c61c70553be6)。ISTA 仓库平铺存放多个量化，因此模板逐文件选择，避免下载其他量化。模板的主 `weight_bytes` 只表示第一分片；存储规划会加上其余五个外部组件，得到上表外部权重小计。原版 safetensors 不可替代这些 GGUF。

采用已有本地权重时，请把完整四分片主模型、Engram、DSpark 分别放在三个目录。主模型与 Engram 混放的目录会识别为两个条目，不能作为一个本地模型登记。使用模板中按仓库、revision、精确文件路径区分的六个制品，逐个选择“登记已有路径”：四个主分片制品记录可以关联同一个完整四分片目录；Engram、DSpark 分别关联自己的目录。六个组件仍各自只读挂载。不要使用相同来源 ID 重复进行本地登记来区分制品，否则可能覆盖已登记路径。

每张分配的卡至少需 12 GiB 显存，平台将此作为显存准入线。宿主机内存不参与该准入：上游文档将约 40 GB 以下 RAM 标为未测试且可能缓慢；低内存时更多专家需要从 SSD 读取。这不是本模板的实测峰值。还应为系统、上下文和其他进程留余量，并按[已知问题 T109](https://github.com/flitai-io/gpuworks/blob/master/known-issues.md#t109-strata-的宿主机内存不参与准入)选择合适节点。

## 部署与运行参数

首次启动包含文件检查和索引准备。请在部署表单中显式设置启动超时，例如 **7200 秒**，并按实际存储速度调整；GPUWorks 默认外层启动超时是 1800 秒，而引擎内部等待 READY 的预算是 3600 秒，两者独立。超时预算不能保证内存或磁盘容量足够。

入口直接准备固定输入并启动服务，运行时不调用 setup 下载缺失权重。缺少组件会明确失败。GPUWorks 设置 `CUDA_VISIBLE_DEVICES`，引擎使用本次部署分配的卡；不要在额外环境变量中覆盖它。

| 参数 | 默认 | 说明 |
|---|---|---|
| `--context N` | 8192 | 服务上下文；输入加输出需在窗口内 |
| `--parallel N` | 1 | 同时处理的会话数；增加会占用更多显存 |
| `--mmap-experts MODE` | 不传；引擎自动 | CPU 专家放置方式：`auto`、`on` 或 `off` |
| `--cpu-threads N` | 引擎默认 | CPU 专家计算线程数 |
| `--ssd-cache N` | 引擎自动 | RAM 专家 store 预算，单位为十进制 GB；0 表示关闭此 store |
| `--max-queue N`、`--queue-timeout N` | 服务器默认 | 排队限制，遵循镜像入口的参数校验 |

可在部署的自定义参数中追加上述参数。模型原生上下文上限为 1,048,576；模板默认 8K，不代表更长上下文已经在本镜像验证。长上下文会占用专家缓存的显存。平台未填写该引擎的 KV 字节/token 测量值，因此不提供推测的 token 容量。

模板默认不传 `--mmap-experts`，保留上游自动放置行为。显式 `auto` 请求相同行为；`on` 保持由模型文件支持的 CPU 专家读取路径，缓存仍由引擎管理；`off` 按 GPU 加载后的可用内存复制专家到 RAM，并保留上游的 8 GiB 余量。若无法全部容纳，引擎只复制最热的部分，其余保留模型文件支持的读取路径。引擎还可能按所选模式调整 prompt 分块、缓冲区和专家放置，因此结果对比不能只归因于复制机制。该引擎预算不会增加 GPUWorks 宿主 RAM 准入检查或内存预留。选择 `off` 前仍需核对节点可用内存和容器限制。

`--ssd-cache 0` 关闭引擎的 RAM 专家 store，不会关闭系统页缓存。在本次 `--mmap-experts auto` 配置下，CPU 专家仍由模型文件支持，原生热页预读和 lookahead 仍可运行；若同时设置 `--mmap-experts off`，仍会按内存预算复制最热的专家。量化和路由专家数不变。这是在切换缓存策略，不是在设置 SSD 文件系统容量。

## API 与观测

文本接口为流式 `POST /v1/chat/completions`，健康接口为 `/health`。指标取自 `/metrics/prometheus`，映射运行/等待请求数、处理的 prompt token、生成 token 和 DSpark 草稿计数。prompt 计数是引擎实际读取的新 token，缓存复用 token 单独由引擎记录。`strata:requests_total` 包含取消等已结束请求，未映射成成功请求数。

回复末尾的 `timings.prompt_ms` 和 `timings.predicted_ms` 分别作为引擎 prefill、decode 时间，换算成秒；没有引擎排队耗时映射。该模板不使用只兼容旧 Qwen 0.1.38 的排队计时补丁。

GGUF 挂载目录没有独立的标准 tokenizer 文件，评测回退来源固定为官方模型仓库的 `tokenizer.json`。引擎回复的 usage 与评测客户端的 tokenizer 计数应分别核对；声明来源不等于两者已完成一致性实测。

固定版本的引擎实现和准备流程见 [Strata v0.2.13 DeepSeek 文档](https://github.com/Niko1221/Strata/blob/d2f096475817d4666c364c820b0ca830f751cfc5/docs/DEEPSEEK41.md)。
