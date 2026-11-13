# 昇腾支持矩阵

更新日期：2026-11-02。这里把三件容易混淆的东西分开写：

- **外层 CANN**：节点或云市场租用镜像提供的 toolkit。自有裸机可能只有驱动，记为 `driver-only`，不等于没有容器 runtime。
- **内层 CANN**：GPUWorks 启动的业务镜像内的 CANN 用户态二进制接口（userspace ABI）；它影响业务镜像能否运行，但不参与 `libvruntime.so` 资产选择。
- **runtime（容器运行时）**：Docker、nerdctl 或 runveil（`udocker` driver）。Gitee 的租机镜像只是外层环境，不是业务镜像；表中的 `udocker` 场景通过 PRoot 在外层容器中运行业务容器（container-in-container）。下表中标 `udocker` 的结果是在旧的 udocker CLI 启动器下验证的；runveil 的实测单独列出。

## 当前实测矩阵

| 环境 | 架构 / NPU | 外层 CANN | 内层 CANN / runtime | 已通过的最高层级 | 结论 |
|---|---|---:|---|---|---|
| Gitee x86_64 | x86_64 / 910B2C | 9.0 | 9.0 / udocker | 平台整卡部署 + `/v1/chat/completions` | 支持；测试显式断言 `runtime_driver=udocker` |
| Gitee x86_64 | x86_64 / 910B2C | 8.5.1 | 8.5 / udocker | 平台部署到 healthy；dGPU 能力探针正确返回云侧拒绝 | 外 8.5 + 内 8.5 可启动；尚缺一份可重复 CI 的成功 chat 证据 |
| Gitee ARM | arm64 / 910B1 | 9.0 | 9.0 / udocker | 平台整卡部署 + `/v1/chat/completions` | 支持；真实预取 arm64 manifest/layers |
| Gitee ARM | arm64 / 910B1 | 9.0 | 8.5 / udocker | 容器启动到 CANN FE | 旧 PRoot 下未通过；`platform_config` realpath 失败。ARM64 `faccessat2` 已修复，该云端组合尚未复测 |
| Gitee ARM 目录项 | arm64 / 910B1 | 8.5 | 未进入内层 | 租机前置校验 | 云市场实际返回“镜像和主机不匹配”，不能作为 CI fixture；这不等于外 8.5 + 内 8.5 已被判定不兼容 |
| 自有池 Docker 成员 | arm64 / 910B4 | 9.0/驱动 | 9.0 / Docker | 平台 API 双副本同卡 dGPU、真实推理/chat、超配拒绝、整卡 chat | 支持；完整平台 dGPU 主验收 |
| 自有 910C / Docker | arm64 / 910C | 9.0/驱动 | A3 镜像 / Docker | 下表标明 Docker 的档完成平台部署、health/模型发现和真实 chat | 只支持下表逐项列出的模型、拓扑与运行时；不把结果外推到 runveil 或未测档。`910c_1` 的 Phy-ID 0 已被逐 die 检查排除 |
| 自有池 nerdctl 成员 | arm64 / 910B4 | driver-only，26.1.1 | 内嵌 probe v2 / nerdctl | 生产 Engine + Driver：物理卡 1→容器逻辑卡 0，精确返回 4 GiB；关闭钩子返回 32 GiB 并判失败 | 节点级 dGPU 探针通过；本次不加载业务镜像、模型或推理引擎 |
| 自有裸机临时 Docker daemon | arm64 / 910B4 | driver-only，26.1.1 | 内嵌 probe v2 / Docker 27.5.1 + Ascend runtime | 生产 Engine + Driver：物理卡 1→容器逻辑卡 0，4 GiB 配额与关闭钩子的 32 GiB 负向对照均符合预期 | 补验 v2 的容器内 DSMI 映射；本次未重跑平台推理 |
| 直通 VM / runveil | arm64 / 910B4 | driver-only，25.5.1 | 内嵌 probe v2 / runveil，无 vcann-rt 钩子 | 物理卡 7→逻辑卡 1，与生产推理注入一致；HBM 查询返回真实整卡 32 GiB，4 GiB 预期判失败 | 仅验证设备映射；device-share 为 False，驱动不支持 VM 开启，不能记为 dGPU 通过 |
| 自有裸机 runveil（物理卡号 2–7） | arm64 / 910B4 | driver-only，26.0.rc1 | 内嵌 probe v2 / runveil | 生产 Engine + Runner 在物理卡 7 上返回 4 GiB；关闭 DSMI 钩子返回整卡 32 GiB 并判失败 | 节点级 dGPU 探针通过；旧 v1 可复现 `rc=87 total_kb=0` |
| 同一自有裸机 runveil | arm64 / 910B4 | driver-only，26.0.rc1 | 8.5.1 / vLLM 0.18.0 / runveil | 默认 agent 路径：平台 API 同卡双副本 dGPU、网关 chat、逐实例超配拒绝，随后整卡部署及 chat | 已通过；每份配额 8 GiB，9 GiB 申请均拒绝。使用 `gpuworks/gpuworks-runner:cann8.5-910b-vllm0.18.0-r2`，自动兼容仅适用于下述已校验的 ARM64 TSD 库 |
| 同一自有裸机 runveil | arm64 / 910B4 | driver-only，26.0.rc1 | 9.0 / vLLM 0.20.2 / runveil | 生产 Engine + runveil Driver 自动选卡，Qwen3-0.6B、8 GiB 配额，真实 chat 返回 HTTP 200 | 通过单实例 dGPU 推理；容器报告 8589934592 bytes，9 GiB 申请被拒绝，随后 chat 返回 `Hello! 😊`。本次未走平台 API/调度/网关 |

“外层 8.5、内层 8.5 已测过”指上表的 Gitee x86_64 环境：已有真实平台部署到 healthy 的现场记录，但当前仓库没有保留成功 chat 响应，因此矩阵不会把它写成完整对话通过。下一步应在现有 x86_64/CANN8.5 租机用例内复用同一实例补 chat，避免增加一次租机。

## 910C 目录契约与实测边界

2026-10-22 的复验确认，runner 镜像只携带一个芯片族的算子包：910C die 上运行
`cann9.0-910b-*` 会在 `AclOpKernelInit` 失败，换用同版本 `cann9.0-a3-*` 才通过。
因此模板谓词和固定镜像必须描述同一芯片族。代际继承（[ADR 0009](../adr/0009-flat-spec-catalog.md)
第 10 条）仍然保留，但后继卡使用独立档位和自己的镜像，不再只扩大旧档谓词。

- 当前目录有 **88 个代际继承 A3 档**，共有 **111 个谓词匹配 910C 的档**。契约测试逐档检查
  910B 原档与 A3 档的参数、权重和镜像关系，以及 910C 档能否解析到 A3 镜像。
  这些数字只是目录契约计数，**不是逐档真机通过数**；真机结论只按下表逐项记录。
- 950 不套用这套继承：目前只有 `qwen/qwen3.8-27b` 的单卡 950PR 档使用对应 950 镜像，
  依据 2026-10-07 的真机测量；没有 950DT 实测档。
- MiniMax-H3 的 SGLang diffusion 仍受 CUDA full-loop denoise 限制，只声明 910B4，不声称
  支持 910C。Gemma-3-4B-IT 和 Gemma-3-27B-IT 的 A3 SGLang 档使用
  `--disable-cuda-graph` 绕过 NPU graph capture 卡死；4B 另有可用的 A3 vLLM 档。
  两种尺寸都已完成文本和真实图像 chat。

### 前沿模型目录与真机状态

“有目录档”表示存在固定权重、镜像和启动参数的规格，不等于已经完成真机验收。

| 模型 | 当前 Ascend 目录 | 910C 实测结论与边界 |
|---|---|---|
| GLM-5.3（非 Flash） | **无 Ascend 档** | 上游虽有 A3 配方，但尚缺本档真机依据，因此未发布 A3 spec |
| GLM-5.3-Flash | A3 TP16/EP16、W8A8、INT8 KV、1M；标准档及 graph-16、graph-8、DFlash16 自定义档 | 原生 Docker 的 health、`/v1/models` 和真实 chat 已通过；三个自定义档也分别进入 running 并返回 chat HTTP 200；runveil/PRoot 未通过 |
| DeepSeek-V4.1-Flash | A3 DP4 × TP4、W8A8、150K | 原生 Docker 的 health、模型发现和真实 chat 已通过 |
| DeepSeek-V4.1-Flash-1017 | 独立 1017 W8A8 权重，A3 DP4 × TP4、150K | 原生 Docker 的 health、模型发现和真实 chat 已通过；不与原版共用权重快照 |
| DeepSeek-V4-Flash-0731 | A3 DP4 × TP4、W8A8 + DSpark、1M | 原生 Docker 的 health、1M `/v1/models` 和真实 chat 已通过 |
| MiniMax-M3 | A3 DP4 × TP4、133K | 原生 Docker 的 16/16 worker、health 和真实 chat 已通过 |
| Qwen3.5-397B-A17B | A3 TP16/EP16、W8A8 + MTP、133K，eager | 原生 Docker 的 health、`/v1/models` 和真实 chat 已通过；eager 避开当前缺失的 `causal_conv1d_update_npu` graph 路径 |
| Qwen3-235B-A22B-W8A8 | A3 TP16、W8A8、vLLM 0.26，eager | 16 个 rank 同时 capture 默认 ACL graph 时触发 `EE1023 no notify resource`；改为 `--enforce-eager` 后 health 和真实 chat 均通过，返回 `Qwen3-235B-910C-ok` |
| Qwen3.8-Flash-Next | 实验性 A3 TP8、W8A8-MTP、135168 上下文 | Docker 的 health、`/v1/models`、真实 chat 和自动选卡已通过 |
| Qwen3.8-27B | 910B 档；继承的 W8A8 A3 档；实测 BF16 A3 档 | W8A8 TP2 的文本请求已通过；BF16 TP2 的文本和真实图像请求均返回 HTTP 200。两者都使用原生 Docker；这只是接口与多模态路径验证，不是输出质量评测 |
| Qwen3.6-35B-A3B-W8A8 | A3 TP2 W8A8；vLLM 0.30 eager 与 SGLang 档 | vLLM 0.26 在 KV cache 初始化时缺少 `linear_attn`；改用 runner vLLM 0.30 并关闭 graph 后，runner 启动和精确复述 chat 均通过。SGLang 档也已通过 |
| QwQ-32B-W8A8 | 原 A3 SGLang 档；A3 TP2 vLLM 0.30 eager 档 | 原 SGLang runner 缺少 `quant_linear` 关键字；通过模板内幂等兼容补丁后 TP2 启动、graph capture、真实 chat 均通过。新增 vLLM 档也精确返回 `QwQ32B-VLLM-A3-910C-ok` |
| Qwen3-14B-GGUF | A3 TP2 SGLang，显式 Q4_K_M，eager | health、权重加载、KV cache 和 chat HTTP 200 均通过；固定复述提示仍在 64 至 96 token 预算内被推理文本占满，因此只记为引擎和接口通过 |
| Qwen3-1.7B | 新增实测 A3 TP1 BF16 SGLang 档 | 权重先下载到 `.29` 再 peer-copy 到 `.30`；evaluate、runner 启动和精确复述 chat 均通过 |
| Qwen3-4B | 新增实测 A3 TP1 BF16 SGLang 档 | evaluate、runner 启动和精确复述 chat 均通过；原目录只有 NVIDIA、PPU 和 Hygon 配方 |
| PaddleOCR-VL-1.6 | 实测 A3 TP1 BF16 vLLM 档 | 640×220 的真实文字图经 OCR 请求返回了图中两行原文；模板在 runner `cann9.1-a3-vllm0.26.0rc1-r2` 上启动和请求均通过 |
| Qwen2.5-Coder-0.5B-Instruct | 新增实测 A3 TP1 BF16 SGLang 档 | evaluate、runner 启动和精确复述 chat 均通过；原目录只有 NVIDIA、PPU 配方 |
| Qwen2.5-Coder-1.5B-Instruct | 新增实测 A3 TP1 BF16 SGLang 档 | API 与 Chrome 模板页均完成 evaluate、部署、running 和 Playground 精确复述；原目录只有 PPU 配方 |
| SmolLM2-360M-Instruct | 新增实测 A3 TP1 BF16 SGLang 档 | evaluate、runner 启动和 chat HTTP 200 均通过；小模型的固定复述提示遵循性较弱 |
| GPT-OSS-20B | 新增实测 A3 TP2 BF16 vLLM 档 | API 与 Chrome 模板页均完成 evaluate、部署、running 和精确复述；eager 避开 NPU graph 路径，模板指向 runner 内已公证的 tiktoken 词表以支持离线 Harmony tokenizer |
| Janus-Pro-7B | A3 TP1 BF16 SGLang | 文本 chat 精确复述通过；API 的 1×1 占位图和后续 Chrome 中真实的 640×220 PNG 都生成了 576 个 image tokens 并返回 HTTP 200，但模型没有给出有效视觉描述，因此只确认多模态链路，不作为视觉质量通过 |
| Phi-4-multimodal-instruct | A3 TP1 BF16 SGLang | 模型声明补充 image modality 后，真实 448×448 图像 chat 返回 HTTP 200 与 `White` |
| MiniCPM3-4B | A3 TP1 BF16 SGLang，eager | 原 graph 路径在子进程 SIGQUIT；`--disable-cuda-graph` 后 evaluate、runner 启动和文本 chat 均通过 |
| MiniCPM-V-4.5-AWQ | A3 TP1 INT4 SGLang，eager | AWQ 配置显式跳过未量化的 `vpm` 和 `resampler` 后，权重、KV cache、health、精确文本复述和真实 448×448 图像请求均通过 |
| Trinity-Mini | A3 TP2 BF16 SGLang，ModelScope 固定 revision | Hugging Face 文件列表超时；改走 ModelScope 完整快照后 evaluate、部署、running 和 chat 均通过，模型返回 `TrinityMini-910C-ok` |
| Qwen3-Omni-30B-A3B-Instruct | A3 TP4 BF16 SGLang，eager | 原 graph 路径停在 health check；`--disable-cuda-graph` 后 runner 启动和文本 chat 返回 `Qwen3Omni30B-910C-ok` |
| Qwen3-30B-A3B-Instruct-2507 | A3 TP4 BF16 SGLang，eager | 原 graph 路径未在观察窗口内 healthy；`--disable-cuda-graph` 后启动和精确复述 chat 均通过 |
| Qwen3-TTS-12Hz-1.7B-CustomVoice | A3 TP1 BF16 vLLM-Omni r3 | 显式 deploy config 后生成 24 kHz 非静音 WAV；Qwen3-ASR 回环识别为 `GPUWorks 910c Speech Validation.` |
| Qwen3-TTS-12Hz-0.6B-CustomVoice | 新增实测 A3 TP1 BF16 vLLM-Omni r3 | 生成 24 kHz 非静音 WAV；Qwen3-ASR 回环识别出句子主体 |
| Ming-omni-tts-0.5B | A3 TP1 BF16 vLLM-Omni r3 | 生成 44.1 kHz 非静音 WAV；Qwen3-ASR 回环识别出句子主体 |
| Gemma-4-31B-IT | A3 TP4 BF16 vLLM | 使用 checkpoint 自带 chat template 后 runner 启动成功，文本及真实图像 chat 返回 HTTP 200 |
| Gemma-4-26B-A4B-IT | A3 TP4 BF16 vLLM，eager | 使用 checkpoint 自带 chat template 并关闭 graph capture 后，权重、KV cache 和 health 通过；文本精确返回 `Gemma426B-910C-ok`，真实 448×448 图像 chat 返回 `White` 和 258 个 image tokens |
| Gemma-3-4B-IT / Gemma-3-27B-IT | 4B 的 A3 SGLang / vLLM；27B 的 A3 SGLang | TP2 的 4B 两种引擎和 TP4 的 27B eager 档都完成了文本及真实图像 chat；SGLang 模板显式关闭 NPU graph capture，避免启动卡死 |
| Kimi-K2.6-W4A8 | A3 TP4 × DP4，16 dies；W4A8 + DFlash15 | 147 文件、499.1 GiB 全量 SHA-256 校验通过；显式声明 TP4 后预检通过，deployment 进入 running，真实 chat 返回 `Kimi-K2.6-910C-ok` marker |
| Kimi-K3 | 无 | 上游最小配方为 4 台 A3；当前没有目录档 |

Qwen3-VL-8B-Instruct 的 A3 vLLM 档把默认 `--max-model-len` 固定为 **196608**，这是
单张 64 GiB 910C die 的实测安全值。TP2 及以上部署已经实测覆盖完整 262144 上下文，
但 vLLM 的 `max_model_len` 是结构化的整型启动参数，模板不能按卡数动态替换；使用
多卡时需要在部署的快速参数中把 `max_model_len` 显式改为 `262144`。默认值保留
TP1 安全上限，不把 TP1 部署暴露给会超显存的 262144 默认值。

DeepSeek-V4.1-Flash 原版与 1017 的 A3 档都显式声明 **512 GiB `/dev/shm`** 和
`memlock_unlimited: true`，用于两张约 103 GiB 的 Engram 本地 DP 共享表；不要在模板
`args` 里重复写 Docker memlock 参数。真机验收曾确认 128 GiB 无法完成第二张表注册，
256 GiB 可以完成，目录采用 512 GiB 隔离 tmpfs 作为统一规格。

启动前校验以最终生效的命令为准，只在平台能证明启动必然失败、且用户无法绕过时才拒绝；其余"证明不了"的组合只给提示，部署照常创建。

- **创建前拒绝（400）**：compat 镜像不得与 `pre_start_argv` 并存；V1 也不接受额外的 `pre_start_script`（V2 可合并脚本）；合并后的启动前脚本不得超过 agent 的 64 KiB 上限。这些都发生在 udocker/runveil 节点上（compat 脚本只注入那里），并以该节点解析出的镜像为准；自动放置尚未选定节点时不检查，由 controller 在选定节点后给出明确原因。
- **评估提示（`warnings[]`，不阻挡创建）**：rc2 A3 V1 兼容 rendezvous 仅限单节点，多节点部署会提示改用 `VLLM_USE_V2_MODEL_RUNNER=1`、Docker/nerdctl 或单节点；V1 与数据并行大于 1 的组合尚未在 runveil 真机上验证；compat 镜像上的手写 shell（包括 JSON 中的 `/bin/sh -c`）平台无法确认实际 runner；Engram 的 `cpu_offload + dp_shared_memory` 组合在 udocker/runveil 上直接使用宿主机的 `/dev/shm` 与 ulimit，平台看不到宿主机是否够用，提示"需要约 512 GiB 共享内存与不限 memlock，建议 Docker/nerdctl"；无法确认的 Engram 配置同理。
- **静默放行**：与 Engram 无关的手写命令（含变量、`&&`、管道、重定向、`["sh","-c",...]`），在任何厂商上都不检查也不提示。命令文本含 `engram` 且能静态解析时，按与引擎模式相同的规则推导共享内存与 memlock；含 `engram` 却无法解析时只提示，不推导。

另有部分继承档完成了单项真机验证：GLM-4.6V-Flash、Qwen3.6-35B-A3B、
Qwen3.6-27B-W8A8、Qwen3.5-122B-A10B、Qwen3-Next-80B-A3B-Instruct 的 health/chat，
Qwen3-8B EAGLE3 的 target+draft 启动/chat，以及 Qwen3-TTS 的
A3 镜像选择、health 和语音请求。这些记录只覆盖对应档位，不能把其余继承档一并写成
真机通过。

新增 Ascend 模板时必须固定完整不可变权重快照。ModelScope 文件 API 的逐文件
`Revision` 不一定是完整仓库快照；不能用“某个文件的 revision”冒充整个模型的快照。

### 启动与运行时边界

GLM-5.3-Flash 的 Docker 通过结果只适用于固定 rc2 A3 镜像的原生 Docker 路径。
runveil/PRoot 下即使绕过 Python 算子和 group 初始化问题，仍会遇到 CANN Fill/Transpose
tiling 注册失败或线程饥饿，因此该档不把 runveil 写成支持路径；Docker 不注入这套
PRoot 兼容脚本。MiniMax-M3 的 Docker 结果同样不外推到 runveil。

昇腾容器的共享设备节点（`davinci_manager`、`devmm_svm`、`hisi_hdc`、`ummu`、`uburma`）
按宿主实际存在与否下发；调度选中的 `/dev/davinciN` 必须交给容器。多机档不在模板里
写死网卡名，agent 按节点广播 IP 反查网卡并填充集合通信变量，显式设置仍优先。

有宿主 CANN 的节点按 die 做 ACL context 与 4 KiB 分配检查；只有明确归因于该 die 的
失败才标记 `runtime_unusable`。driver-only 节点缺少 `libascendcl.so` 时跳过，不把缺工具
当成硬件故障。失败分级、退避和排查见[运行时不可用](../how-to/troubleshooting.md#某张卡运行时不可用或驱动初始化失败)。

## 覆盖层级

1. **探测**：架构、NPU、driver/toolkit、runtime 和dGPU 能力原因。
2. **设备注入**：内层容器可见指定的物理 NPU。
3. **dGPU 行为探针**：走生产 `Engine` 生成 vcann-rt 配置并启动真实 runtime；内嵌 2.5 MiB `/probe` 调用 `dsmi_get_hbm_info`，断言看到的总显存恰好是 4 GiB。该层刻意不加载业务镜像或模型。
4. **平台 dGPU 推理**：必须经 GPUWorks API 创建部署和实例，验证同卡多副本、配额及真实推理。
5. **完整整卡对话**：必须经平台 API 部署，并从 `/v1/chat/completions` 得到真实回复。

自有池覆盖 1–5；Gitee 因租用容器无权执行 `npu-smi set -t device-share`，只能覆盖探测和整卡 container-in-container 对话，不能把云侧权限拒绝伪装成 dGPU 通过。

## 已修复的误判

### 行为探针固定查询逻辑设备 0

旧探针固定调用 `dsmi_get_hbm_info(0)`。PRoot 不建立 OCI 设备隔离，DSMI 的编号取决于驱动所见的设备：可能保留宿主编号，也可能将物理卡 5、7 映射为逻辑卡 0、1。固定查 0 可能查询不存在的设备，也可能查询另一张卡；直接使用物理卡号同样可能出错。因此不能仅凭容器运行时名称决定查询编号。

agent 现在通过 `GPUWORKS_PARTITION_PROBE_DEVICE` 传入所选物理卡号；探针先用 `dsmi_get_logicid_from_phyid` 解析当前运行环境中的逻辑号，再查询该设备的 HBM。缺少映射接口、映射失败或显存不等于配额均返回失败，不回退到 0 号卡。诊断输出同时记录物理号和逻辑号。

2026-10-24 补测确认：Docker + Ascend runtime 与 nerdctl 的单卡容器内，v2 均能将物理卡 1 解析为逻辑卡 0；runveil 直通 VM 内则将物理卡 7 解析为逻辑卡 1。前两者走生产 Engine 的 4 GiB 切分配置并通过关闭钩子的负向对照；VM 只查询整卡 HBM，未绕过 device-share 前置条件。这些结果不外推到未测试的驱动或 910C。

内嵌探针镜像升级为 `gpuworks/partition-probe:v4`，避免升级 agent 后继续命中已缓存的 v1/v2/v3。该修复针对节点级行为探针；真实推理的支持范围仍以实测矩阵为准。

### runveil 推理设备选择与 ARM64 路径解析

runveil 保留宿主设备视图，不能沿用 Docker 的紧凑设备号。agent 现在结合 `npu-smi info` 的物理卡/芯片地址与 `npu-smi info -m` 的 `Chip Logic ID`，生成 `ASCEND_RT_VISIBLE_DEVICES` 和 `NPU_VISIBLE_DEVICES`；缺失或冲突的映射会阻止启动，不回退到 0。真机验证了裸机物理 7→逻辑 7，以及直通 VM 物理 7→逻辑 1 的生产注入结果。

此前 ARM64 PRoot 的系统调用表缺少 `faccessat2`（439），导致 glibc `realpath()` 对目录的检查使用宿主路径，误报 `ENOENT`。CANN 因而找不到 platform 配置或算子注册库；CANN 9.0 表现为 `ZerosLike` / `aclnnInplaceZero` 的 `361001`。该错误在关闭 dGPU 的对照中也能复现，不能据此判定 vcann-rt 不支持 runveil。

已补齐系统调用映射并重建 ARM64 内嵌 PRoot。构建门禁验证 guest bind 下的 `faccessat2`、相对 dirfd、缺失路径和带符号链接/父目录/尾斜杠的 `realpath`；原二进制失败，新二进制在 ARM64 真机通过。使用更新后的内嵌资产和自动设备映射，CANN 9.0 / vLLM 0.20.2 的单实例 dGPU 推理、超额分配拒绝及拒绝后的 chat 均通过。范围仍以上表为准。

### CANN 8.5.1 的 DCMI 检测与 TSD 设备转换

CANN 8.5.1 在 runveil 下有两个初始化问题。DCMI 在镜像缺少容器标记和 `systemd-detect-virt` 时，检测子进程可能卡在 CANN dump 析构锁；runveil 现在为昇腾工作负载自动绑定 `/.dockerenv`，避免进入该检测分支。

另一个问题出现在 TSD：runtime 把选中的逻辑卡 7 转成用户设备号 0 后，TSD 在映射回逻辑号前查询芯片型号，导致 `TsdOpen` / `507033`。runveil 现在仅为已确认受影响的 ARM64 CANN 8.5.1 `libtsdclient.so`（SHA256 `4f6648daad462247e23782946b1a10b3d224d42d99aa2f468bd6b45c2a08676a`）加载内嵌兼容库，将来自该 TSD 的芯片型号查询按 `ASCEND_RT_VISIBLE_DEVICES` 转换，再调用真实驱动。它不会伪造成功，也不会把所有对卡 0 的查询改写。未知或已修复的库不应用这项转换；“CANN 8.5”版本标签本身不足以判定兼容。

兼容文件位于可删除的容器目录，不修改宿主驱动或共享镜像；既有 `LD_PRELOAD` 保留。启动、交互终端及非交互 `Exec` 均恢复同一工作负载环境和挂载，agent 重启后也从持久化 spec 恢复。使用此修复需要升级 agent 并重新创建工作负载。

### “没有 runtime”

历史 ARM fixture 报“节点没有可用的 docker/udocker 运行时”，不是 CANN 声明，也不是硬件不支持。该外层镜像的 Python 位于 `/root/miniconda3/bin/python3` 或 `/opt/conda/bin/python3`，不在非交互 shell 的默认 `PATH`，旧探测因此没有启用内嵌 udocker/PRoot。

现在探测会检查这些绝对路径，runtime 可正确识别为 `udocker`。这一探测现在只对 `GPUWORKS_UDOCKER_LAUNCHER=udocker` 有意义：默认的 runveil 启动器不需要 Python。

### `libvruntime.so` 只按 CPU 架构区分

历史上 CANN 8.5.1 和 9.0.0 目录中的同架构产物字节级相同：arm64 SHA256 均为 `3dbe727a…d3d49`，amd64 均为 `57a9641a…9d010`。它们来自同一固定 ubs-virt 源码和 patch，并不是两套运行时 ABI。

仓库现在只保留 `linux-arm64/libvruntime.so` 与 `linux-amd64/libvruntime.so` 各一份，作为各架构唯一的标准路径（canonical 路径）。运行时只接受这两个路径，不读取外层或内层 CANN 版本，也不存在跨 CANN 版本回退。

agent 升级并成功安装标准路径下的资产后，会精确删除旧的 `cann-8.5.1/` 与 `cann-9.0.0/` state 目录。CANN 版本只保留为实测环境记录和兼容警告。

节点行为探针通过只证明“节点 + runtime + 该 SO（共享库）”的 dGPU 链路可用，不自动证明任意业务镜像可用。业务镜像是否支持，应由使用该镜像用户态环境（userspace）的专用行为探针（image-specific probe）判断；现有版本矩阵在完成这层探针前只能标为 warning/unknown，不能作为支持断言。ARM 外 9 + 内 8.5 的 PRoot `platform_config` 失败正说明二者不能混为一个结论。

`PartitionProfile.Verified` 是代码中的验证标记，不能替代上面的真机矩阵：

- **定义位置**：`internal/accelerator/ascend.go` 的 profiles 表，按 backend/framework/版本前缀/架构硬编码的静态布尔位。
- **标记含义**：只表示该组合在代码里被标记为已在预置真机环境验证过。为 false 时，`partitioncap` 会追加“该运行时组合尚未经预置真机环境验证”提示。
- **维护方式**：随代码提交手动维护，不会随本文档的真机矩阵自动同步。例如 CANN 9.0.x × x86_64 已有完整 chat 记录，但该字段尚未标记为 Verified。
- **校验边界**：不表示对应 `libvruntime.so` 资产本身经过额外的构建/来源校验；该校验由 `build/runtime-assets/verify.sh` 独立完成。

支持结论以上面的真机矩阵为准。

## CI 归属

- `l4-gitee-ascend-partition-capability`：租用 x86_64/CANN8.5 实例，验证节点能力探测和已知的 device-share 限制，不下载业务模型。
- `l4-gitee-ascend-deploy-chat`：显式低频运行 x86_64/CANN9 和 arm64/CANN9 的整卡部署与真实对话。
- `l4-ascend-pool-partition-inference`：把 `.31/.32` 当能力池而非绑定节点；验证平台 API dGPU 推理/整卡对话，以及 nerdctl 设备注入和内嵌行为探针。
- nerdctl helper 是仓库内约 9 MiB 的 gzip arm64 资产。CI 和 GPU 节点不执行 `go build`；更新时只在 内部构建机（见 `secrets/test-environments.md`） 构建后签入。

失败调试默认先保留最后一个现场：云租机用 `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`，nerdctl helper 用 `GPUWORKS_KEEP_ON_FAILURE=1`。确认原因后按本次 run 的精确实例、容器和临时目录清理。

## 卡与芯片：识别与编号

每颗芯片是一个独立设备（自己的 `/dev/davinciN`、PCIe 功能、HBM 与健康状态），GPUWorks 按芯片上报与调度。
卡的布局按 `npu-smi info` 的表格判定，与 zstack-utility 的 `resolve_handler` 一致：

- **一卡一芯片（910B）**：每个 NPU 一行芯片、表头没有 `Phy-ID` 列。设备编号是 NPU ID。
- **一卡多芯片（910C）**：表头有 `Phy-ID` 列，或同一 NPU 下出现多个 Chip ID，或芯片名含 `910C`。
  910C 的芯片名列写的是 `Ascend910`（不带后缀），每个 NPU 两颗芯片（Chip 0/1）；GPUWorks 把这种卡的芯片记为型号 `910C`。
  初代 910 是同名的一卡一芯片卡，不会被认成 910C。设备编号是芯片的 Phy-ID；没有 Phy-ID 的芯片无法对应设备，不上报。

设备编号统一取 Phy-ID，依据是 2026-10-02 对两台 910C 主机所做的只读核实：

| 用途 | 编号 |
|---|---|
| `/dev/davinciN`、`ASCEND_VISIBLE_DEVICES`（Ascend Docker Runtime）、nerdctl `--device` | Phy-ID |
| `npu-smi info -t topo` 矩阵 | `Phy-ID<n>` 行列（芯片间 `SIO`、卡间 `HCCS_SW`，都按高速互联） |
| `hccn_tool -i` | Phy-ID（zstack-utility `fedf490a2` 同样改用物理 ID） |
| `npu-smi info -t health/board/device-share -i <npu> -c <chip>` | NPU ID + Chip ID，由 agent 从表格换算 |
| 容器内 `ASCEND_RT_VISIBLE_DEVICES` | 从 0 起的紧凑编号 |

`npu-smi info -m` 里 910C 的 Logic ID 与 Phy-ID 相同，且不随缺卡压缩（缺 NPU 3–6 的主机上 NPU 7 的两颗芯片仍是 14、15），
`/dev/davinci0..15` 全部存在，与 Phy-ID 一一对应（`Phy-ID = 2 × NPU ID + Chip ID`）。
910B 的 NPU ID 即 Phy-ID，因此一卡一芯片卡沿用 NPU ID。
