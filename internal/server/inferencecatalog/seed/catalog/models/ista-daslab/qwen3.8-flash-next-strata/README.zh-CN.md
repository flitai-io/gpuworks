# Qwen3.8-Flash-Next 125B（Strata 分层推理）

[Strata](https://github.com/Niko1221/Strata) 把 MoE 专家放在显存、内存和 SSD 之间分层调度，本目录提供 125B Qwen3.8-Flash-Next 的四种 GGUF 量化。每张分配的 NVIDIA 卡至少需要 12 GiB 显存，还需满足所选量化的宿主机内存和磁盘需求（见下表）。模板使用 callable 引擎：镜像入口 `/opt/strata/gpuworks-entrypoint.sh` 把平台给出的模型路径、地址、端口以及分配到的 GPU 转成 Strata 的安装参数，然后启动服务。

目录随包携带经哈希校验的轻量排队计时补丁，启动包装器应用补丁后进入原镜像入口，基础镜像、模型参数和 GPU 分配保持原流程。补丁只接受固定 0.1.38 源码，其他版本会明确启动失败。相同回复中的 `timings.queue_ms` 测量首次加载及首次生成准入前的 FIFO 等待，不包含模型加载、工具执行或后续 MCP 轮次的等待，不开启正文监控。旧部署需更新到该目录配置并重建实例后才会输出该字段；不会在线修改已运行实例。

## 内置 Strata 规格

安装包含此目录的 GPUWorks 版本后，打开「推理模板 → Qwen3.8-Flash-Next 125B（Strata）」，选择与权重对应的「Turing 及以上 · <量化> · Strata r5」。服务启动时会自动提供四个规格，无需导入或复制模板。新部署请选择 r5；gw6 标为「旧镜像」，只为已有部署保留，其规格 ID 和已发布的镜像来源不变。

四种规格统一使用已发布的 `quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5`，manifest 摘要为 `sha256:19216f47d43e59068266bb4d6d0150f3dea74f202a97e2e3142eeca8240bde9d`，节点可直接从公网拉取，无需预先导入本地镜像。SWR 的同名标签尚未通过公开读取验证，因此暂不作为回退来源。

在「镜像仓库 → 社区镜像 → 添加社区镜像」中，可打开 GPUWorks runner 仓库、搜索 `strata0.1.38-amd64-slot31-r5` 并选择该标签。表单会识别 Callable、CUDA 13.0、引擎版本 0.1.38 和 amd64 架构。r5 保持相同固定版本的 Strata，修复了模型链接和入口兼容性。新增规格 ID 不包含镜像版本号，版本只体现在显示名和镜像引用中。镜像上传不会自动更新旧版 GPUWorks 内置目录，需安装包含此目录的 GPUWorks 版本。

选择与规格对应的已注册本地 GGUF 量化目录，或通过该规格的来源获取固定版本权重。r5 的 IQ2_XS 规格与 gw6 使用相同权重，其他三个规格各用自己的量化权重；四者共用 tokenizer、GPU 分配、API 和排队计时合同。r5 另支持在自定义参数中设置 `--ple-io direct|mmap|ram`、`--ple-gguf /absolute/path/to/file.gguf`。默认保持 `direct`；模板不会自动指定某台机器的缓存路径、RAM 配额或无限 memlock。重定向 PLE 时，应挂载与对应权重版本逐字节一致的只读副本。RAM 模式需要足够的容器内存和合适的 memlock 上限，并应检查实际驻留和锁页情况。

## 权重格式与量化规格

这四个模板提供原版 Qwen3.8-Flash-Next 的四种 **ISTA-DASLab GSQ-RCO GGUF** 量化。选择与已登记模型的量化目录对应的规格。gw6 的既有规格 ID 与默认参数保持不变。

| 量化 | 权重字节数（两个分片） | 上游 setup 的内存建议 |
|---|---:|---:|
| IQ2_XS | 68,026,093,024 | 48 GB |
| Q2_0 | 66,423,878,624 | 48 GB |
| IQ3_XXS | 75,839,998,528 | 60 GB |
| IQ3_S | 83,617,662,656 | 62 GB |

字节数取自[固定权重 revision](https://huggingface.co/ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF/tree/ed59f92082b1e93c0e96d60a8b11aab089b52f09)的文件大小之和。新增规格声明了 Hugging Face 与 ModelScope 的固定 revision，已核对两边公布的分片大小与 SHA-256 元数据一致；原有 IQ2_XS 来源不变。内存数字是镜像内 setup 的建议，不是 GPUWorks 的内存准入条件或实测峰值；还要为上下文、准备过程和其他进程预留余量。每张分配的 GPU 仍至少需要 12 GiB 显存。宿主机内存目前不参与准入，低内存节点可能通过显存检查后在 setup 阶段失败；请按[已知问题 T109](https://github.com/flitai-io/gpuworks/blob/master/known-issues.md#t109-strata-的宿主机内存不参与准入)选择有足够内存的节点。

非默认规格追加 `--model=Q2_0`、`--model=IQ3_XXS` 或 `--model=IQ3_S`。等号不能省略：镜像将 `--model 路径` 解释成 GPUWorks 的模型挂载，而等号形式会到达 Strata setup，覆盖写死的 IQ2_XS 默认值。只改权重目录会让 setup 查找错误的分片。应选择对应规格，不要脱离权重来源单独修改这个参数；模型来源的一致性校验继续生效。

**当前镜像不支持原版 BF16/FP8 safetensors。** 固定版本的 [Strata setup](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/setup.py)准备的是 GGUF 输入，GPUWorks 镜像会拒绝没有 GGUF 文件的目录。上游 safetensors 工具只提取 token embedding、PLE 表或 MTP 草稿层，不负责加载原版整模型；FP8 PLE 表也不等于 FP8 专家模型。删除模板来源检查无法补齐 checkpoint 加载和专家执行能力，因此这里没有宣称可用的 BF16/FP8 规格，也不自动进行有损转换。GPUWorks 已在推理模板中独立的「Qwen3.8-Flash-Next」条目提供原版权重的 vLLM/SGLang 规格，应按其硬件与资源条件选择。

其他仓库与量化（包括 Unsloth 实验性的 UD-Q4_K_XL）需要各自的 family、准备流程和能力设置；模型名称相同不代表可以互换。Q2_0/IQ3_XXS/IQ3_S 属于固定引擎明确列出的格式。模板及镜像参数检查不代表新增规格已完成真权重生成或性能验收；既有端到端实测证据适用于 r4 镜像的 IQ2_XS，不代表 r5 或其他量化已完成真权重推理验收。

## 图像输入

默认开启。镜像里带有 Strata 的图像编码器 `strata-vision`，以及与权重同一 revision 的视觉投影文件 `mmproj-Qwen3.8-Flash-Next-BF16.gguf`（该文件在仓库根目录，模板只下载所选量化的子目录，所以它随镜像提供；模型目录里若自带同名文件则优先用它）。入口以 `--vision gpu` 安装：编码器和引擎在同一张卡上，引擎会按编码器占掉的显存缩小专家缓存。既有 IQ2_XS 测试数据（单张 RTX 4090D，128K 上下文，非本次 r5 实测）：编码器约占 1.4 GB 显存，解码速度约 51–55 tok/s，与不开图像输入时相当。

- 请求按 OpenAI 格式在消息里放 `image_url`，支持 base64 data URL 和 http(s) 地址；GPU 编码器每张图最多 1024 个图像 token。
- 在「自定义参数」里加 `--vision cpu` 改由 CPU 编码（不占显存，每张图最多 300 个 token，较慢），加 `--vision no` 关闭图像输入、把显存全留给专家缓存。
- 关闭后请同时在模型页把输入模态改回只有文本，Playground 才不再显示传图入口。

## 用几张卡

Strata 使用平台分配给这次部署的卡：自动放置时用一张；在部署表单里选了几张卡就用几张，Strata 会把各层分到这些卡上。卡越多，能放进显存的专家越多，生成越快。每张卡至少 12 GiB，不满足时部署评估直接拒绝。

模板里看不到卡数或卡号，这是有意的。分到的卡是这样传到 Strata 的：

1. 调度器选卡。模板的 placement resolver 只检查每张卡的显存，不指定卡数。
2. agent 启动容器时，把这些卡写进环境变量 `CUDA_VISIBLE_DEVICES`，所有引擎都是这样。udocker 节点上写的是物理卡号；docker 节点上容器只挂进选中的卡，写的是容器内的编号 0..n-1。两种情况下都等于容器里 `nvidia-smi` 显示的编号。
3. 镜像入口读取 `CUDA_VISIBLE_DEVICES`：一张卡传 `--gpu N`，多张卡传 `--gpus A,B`。Strata 本身就是按 `nvidia-smi` 的编号选卡的。

卡号不做成模板变量，是因为它取决于节点的容器运行时，只有 agent 知道；模板变量在 server 上展开，那时还确定不了。因此不要在「自定义参数」里手写 `--gpu`/`--gpus`，写死的卡号可能指向分给别的部署的卡。

想确认实际用了哪几张卡，可以看容器内 Strata 安装配置 `/opt/strata/strata-<quant>.json` 里的 `gpu` 字段（`<quant>` 为小写量化名：`iq2_xs`、`q2_0`、`iq3_xxs` 或 `iq3_s`；部署详情的实例终端里可以直接查看），或在节点上运行 `nvidia-smi`，看占用显存的是不是分配给这个部署的卡。

## 可配置的参数

callable 档位没有参数字典，调整都靠在部署表单的「自定义参数」里追加命令行参数。入口脚本会把它们原样交给 Strata 的 `setup.py`；同名参数以后出现的为准，所以这里写的值会覆盖入口脚本的默认值。

| 参数 | 默认 | 说明 |
|---|---|---|
| `--context N` | Strata 按显卡推荐（见下） | 上下文窗口，即一次请求的输入加输出 token 数的上限 |
| `--kv int8\|q4_0\|k8v4` | `int8` | 8K 以上上下文的 KV 精度。`q4_0` 的 KV 显存减半，质量略降 |
| `--kv-streaming auto\|on\|off` | `auto` | 64K 以上上下文时，把 KV 放在内存里，显存只保留注意力窗口 |
| `--rope-scaling linear\|yarn`、`--rope-scale F` | 超过 262144 时自动选 yarn，系数为上下文 / 262144 | 超出训练长度时的 RoPE 外推方式 |
| `--vision gpu\|cpu\|no` | `gpu` | 图像编码器跑在 GPU、CPU，或关闭图像输入（见「图像输入」） |
| `--vram-reserve-mib N` | 引擎默认 | 给同卡上的其他程序预留的显存 |
| `--gpu N`、`--gpus A,B` | 平台分到的卡 | 不要改：入口由 `CUDA_VISIBLE_DEVICES` 得出（见上），改成分配之外的卡会抢占别的部署的显存 |

请求里的 `max_tokens` 决定这一次最多输出多少。不传时 Strata 会一直生成到上下文用满，所以单次输出的上限是「上下文 − 输入长度」。

## 上下文的默认值与上限

- **默认**：不传 `--context` 时，Strata 按分到的卡里显存最小的那张推荐：小于 14 GB 用 32K，小于 20 GB 用 64K，其余用 128K。固定版本的宿主机内存上下文规则只针对 IQ3_XXS/IQ3_S，且不会把推荐值降到 128K 以下，因此不改变上述默认值。选择超过 128K 的上下文时，setup 会估算 IQ3 两档的内存需求并在不足时提示；IQ2_XS/Q2_0 不适用这条规则，但仍需要足够内存。四种量化在 24 GB 的 RTX 4090 / 4090D 上默认均为 128K；显式设置 `--context` 会覆盖推荐值。
- **上限**：模型训练长度是 262144（256K）。Strata 还提供 384K、512K 两档，靠 yarn 外推实现，属实验性质。
- **代价**：KV 很省，int8 每个 token 约 13.4 KB（`q4_0` 约 7.3 KB），256K 上下文的 KV 约 3.5 GB，由各卡按层分摊。但这部分显存和专家缓存共用：上下文每多占 1 GB，就少缓存 1 GB 专家，生成速度随之下降。显存小的卡建议保持默认，长上下文搭配 `--kv q4_0` 或 `--kv-streaming on` 使用。
- 模型信息里的 `context_length: 262144` 是模型的训练长度，不是一次部署实际使用的上下文。实际值看 Strata 启动日志里的 `context N tokens`，或 `/v1/models` 返回的 `meta.n_ctx`。

## 已知边界

- `/metrics` 返回 JSON，而非 Prometheus 文本；本模板提供平台采集所需的字段映射。
- 首次启动会建立专家索引，比重启慢。
