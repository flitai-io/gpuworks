# DeepSeek-V4.1-Flash-1017

原始混合 FP8/FP4 权重约 510.3 GB。本模板提供三种引擎的配方：

- **SGLang**：来自 [SGLang 官方文档](https://docs.sglang.io/cookbook/autoregressive/DeepSeek/DeepSeek-V4_1)，使用专用 `lmsysorg/sglang:dev-dsv41` 镜像（CUDA 13），不能替换成 V4 镜像或普通旧版 runner。
- **vLLM（NVIDIA）**：来自 [vLLM 官方配方](https://github.com/vllm-project/recipes/blob/main/models/deepseek-ai/DeepSeek-V4.1-Flash.yaml)，需要 vLLM 0.31.0 及以上；昇腾使用下述独立 vLLM-Ascend 镜像。
- **DeepInfer**：DeepSeek 开源的推理框架 [DeepInfer](https://github.com/deepseek-ai/DeepInfer)（commit `774ce24`）。镜像 `gpuworks/deepinfer:774ce24-r9` 在其上补了 OpenAI 兼容 HTTP 服务（协议层用 DeepSeek 的 deepseek-recipe）、单机 Engram 本机取数和多机请求中继；只服务文本输入，开源版不含 PD 分离。

## 配方

| 配方 | 硬件 | 说明 |
| --- | --- | --- |
| `fp4-sglang-nvidia-h200-tp8` | H200/H20 × 8 | 上游 H200 低延迟命令，1M 上下文，不开推测解码 |
| `fp4-sglang-nvidia-h200-tp8-throughput` | H200/H20 × 8 | 上游 H200 高吞吐命令：最多 256 个并发请求 |
| `fp4-sglang-nvidia-h200-tp4-engram-host` | H200/H20（141 GB）× 4 | 设置 `SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=1`，把约 196 GB 的 Engram 表放到主机内存，每卡只放约 73 GB 主干权重；需要约 250 GB 主机内存；1M 上下文 |
| `fp4-sglang-nvidia-h200-tp4-engram-host-dspark` | H200/H20（141 GB）× 4 | 在上一行基础上开启 DSpark；上下文限制为 256K（1M 时 decode CUDA graph 捕获会再申请 12 GiB，放不下） |
| `fp4-sglang-nvidia-b200-tp4` | B200/B300/GB300 × 4 | 上游 Blackwell 低延迟命令，开启 DSpark |
| `fp4-sglang-nvidia-b200-tp4-throughput` | B200/B300/GB300 × 4 | 上游 Blackwell 高吞吐命令：不开 DSpark，最多 256 个并发请求 |
| `fp4-vllm-nvidia-h200-tp4-engram-host` | H200/H20（141 GB）× 4 | 上游 H200 命令：TP4，Engram 表放主机内存（`--engram-config {"cpu_offload":true}`） |
| `fp4-vllm-nvidia-b200-tp2` | B200 × 2 | 上游 B200 命令：TP2，Engram 表放主机内存（THP），面向低并发 |
| `fp4-vllm-nvidia-b300-tp2` | B300 × 2 | 上游 B300 命令：TP2，Engram 表放主机内存，最多 256 个并发请求 |
| `fp4-vllm-nvidia-blackwell-dep8` | B200/B300 × 8 | 上游单机 DEP 命令：数据并行 8 + 专家并行，高吞吐 |
| `fp4-deepinfer-nvidia-blackwell-ep` | B200/B300 × 4 或 8（单机） | 专家并行 = 卡数，注意力数据并行 = 卡数；自动放置时优先整机 8 卡；不需要 RDMA 网卡 |
| `fp4-deepinfer-nvidia-blackwell-ep-multinode` | B200/B300，每节点 4 或 8 卡，2 节点及以上 | 专家并行跨节点；需要主机 RDMA 参数网络 |
| `int8-vllm-ascend-a3-tp4-dp4` | A3/910C × 16 逻辑 die | 1017 W8A8，DP4 × TP4，150K 上下文，约 781.5 GB 权重；Engram 表放主机内存 |

H200 × 4 的两个 SGLang 配方是本平台补充的组合，所用开关均来自上游文档。

H20 共用这些配方，保留原 spec 名称。默认参数下，96 GB 版本使用两个 SGLang 8 卡方案；三个 4 卡方案仍会被显存准入拒绝，141 GB 版本可评估这些方案。型号匹配不替代卡数和实际显存检查。下方 H200 实测不代表 H20 已验证；H20 尚未完成真机推理验证。

## Ascend A3 的部署前提与实测

910C 默认选择 `int8-vllm-ascend-a3-tp4-dp4`。该配方需要单机 16 个 910C 逻辑
NPU，每个至少 64 GiB。模板声明 512 GiB `/dev/shm` 和 `memlock_unlimited`，
主机需留足空闲内存供两张 Engram 共享表注册与锁页。

离线部署先在目标节点导入与 CPU 架构匹配的
`gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1`。从 Quay 副本导出时，
归档中应保留这一规范引用。若节点已缓存同名镜像，将其镜像 ID 与相同架构的发布镜像 ID
比较。有差异时，在目标节点重新导入已核实的 runner 归档。
原生 Docker 环境应连接 GPUWorks agent 实际使用的 Docker daemon，并执行
`docker load -i <已核实的镜像归档.tar>`。如果通过 GPUWorks 从已登记的归档 URL
替换镜像，向 `POST /api/v1/images/{id}/nodes/{uuid}/sync` 提交
`{"source":"url","archive_url":"<已核实的归档 URL>","force":true}`，等待传输完成。
普通归档同步会跳过已标记镜像存在的节点。完成后重新检查规范标签对应的镜像 ID，
确认它与该架构的发布镜像 ID 一致。
已有部署再次拉取时仍会使用创建时保存的镜像来源；使用更新后的模板创建部署即可采用新的来源。

准备 ModelScope 仓库 `Eco-Tech/DeepSeek-V4.1-Flash-1017-w8a8` 的完整快照
`12b85455a58c7df345c1abc43934624a76bac576`。只有仓库 ID 和 revision 均匹配时，
才在已有 ModelScope 模型的操作中选择“登记计算节点已有路径”，关联目标节点上的快照目录。
若无匹配记录，可以按该 revision 登记 ModelScope 模型后关联目录，也可以将已核实的快照目录
登记为本地模型，并将“来源 ID”设为上述完整仓库名。部署时选择已登记的模型和 W8A8 配方，
确认评估结果显示目标节点已有权重。实例就绪后发送一条聊天请求。

当前 GPUworks 昇腾目录覆盖此 910C W8A8 配方。Eco-Tech W4A8 需要单独验证的
镜像和配方。上游最新的 910C 示例使用 V2 模型运行器和 INT8 Engram。
[vLLM-Ascend 部署指南](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/DeepSeek-V4.1-Flash.html#51-single-node-colocated-deployment)

2026-10-23，该档在 910C × 16 的原生 Docker 环境通过平台 health、`/v1/models`
和真实 chat；该结果不外推到 runveil。

## DeepInfer 的部署前提

- **单机配方**：Engram 表（约 197 GB）放在容器 `/dev/shm`，模板声明 450 GiB 共享内存，主机需要相应的空闲内存；模板声明 `memlock_unlimited`，平台为容器加 `--ulimit memlock=-1:-1` 以锁定这些主机内存。每个 DP rank 并发上限 128，8 卡共 1024。
- **多机配方**：各节点在同一个主机 RDMA 参数网络上，网卡支持 GPUDirect RDMA；平台为该网络自动加 `/dev/infiniband`、`IPC_LOCK` 与不限制的 memlock，不需要 privileged。只有 rank 0 对外服务，其余节点通过 `MASTER_PORT+7` 接收请求。
- 两个配方都声明了 `api.metrics`，监控中心可以看到运行中/排队请求、token 计数与 TTFT/TPOT 分布。

## 实测

### SGLang（2026-10-20，Vast 8×H200 SXM 容器节点，runveil）

每个请求约 1.67k 输入、固定 512 输出，节点本机直连引擎，并发 16 取预热后的稳态：

| 配方 | 并发 | 总输出吞吐 | 单路解码 | 平均 TTFT | 平均 TPOT | 启动到就绪 |
| --- | --- | --- | --- | --- | --- | --- |
| H200 × 8 | 1 | 66 token/s | 67.5 token/s | 0.21 s | 14.8 ms | 18 分钟 |
| | 16 | 329 token/s | 20.6 token/s | 0.52 s | 47.7 ms | |
| H200 × 4 + Engram 主机表 | 1 | 86 token/s | 89.6 token/s | 0.22 s | 11.2 ms | 20 分钟 |
| | 16 | 306 token/s | 19.5 token/s | 0.54 s | 51.4 ms | |
| H200 × 4 + Engram 主机表 + DSpark（256K） | 1 | 112 token/s | 116.8 token/s | 0.22 s | 8.6 ms | 22 分钟 |
| | 16 | 502 token/s | 32.5 token/s | 0.39 s | 30.8 ms | |

对话、思考模式（`chat_template_kwargs.thinking=true`）与工具调用均通过；DSpark 接受长度 2.4–3.0。

DSpark 也可以在 1M 上下文下启动，但要把 `--max-running-requests` 限到 64（那 12 GiB 与最大并发请求数 × 上下文长度成正比）；实测并发 1 为 80 token/s、并发 16 为 200 token/s，比不开 DSpark 还慢，所以不提供该组合的配方。

### DeepInfer（2026-10-21/22，Nebius，平台评测）

随机数据，输入约 4.2k token（含 chat 模板）、输出固定 1024 token（`ignore_eos`），由节点 agent 直接发压，tokenizer 从 ModelScope 加载：

| 配方 | 硬件 | 并发 | 输出吞吐 | TTFT p50 | TPOT p50 |
| --- | --- | --- | --- | --- | --- |
| 单机 | 8×B200 | 8 | 937 token/s | 0.23 s | 2.4 ms |
| | | 256 | 8,337 token/s | 5.3 s | 7.8 ms |
| | | 1024 | 20,222 token/s | 12.1 s | 10.6 ms |
| 单机 | 8×B300 | 256 | 9,309 token/s | 3.4 s | 6.4 ms |
| | | 1024 | 17,035 token/s | 14.6 s | 20.0 ms |
| 多机 | 2 节点 × 8×B300（InfiniBand） | 256 | 3,718 token/s | 17.6 s | 11.5 ms |
| | | 1024 | 12,513 token/s | 17.9 s | 20.1 ms |
| | | 2048 | 13,495 token/s | 66.7 s | 16.8 ms |

全部请求成功。在这种输入与输出混合的负载下，多机配方的跨节点 prefill 每卡吞吐明显低于单机，单机配方更合适；多机配方的价值在于并发上限（每节点 1024）。

## 注意

- 主机驱动低于 580 时，SGLang 引擎报 `No accelerator`：可在部署的容器环境里把镜像自带的 `/usr/local/cuda-13.0/compat` 放到 `LD_LIBRARY_PATH` 最前，并设置 `TRITON_LIBCUDA_PATH` 为同一目录（`/usr/local/cuda/compat` 版本过旧，不能用）。
- runveil（无 docker 的容器节点）上 SGLang 默认按主机核数开线程池，8 卡时约 3600 个线程会让 PRoot 卡死在加载权重；需要把 `OMP_NUM_THREADS` 等限制为 4（平台对 runveil 上的 SGLang 默认设置）。
- vLLM 首次启动会做算子调优，可能超过 10 分钟；配方已把 `VLLM_ENGINE_READY_TIMEOUT_S` 设为 3600。

未声明的 AMD、海光、PPU、Apple 组合不在本模板覆盖范围内；昇腾仅覆盖上述 910C Docker 档。
