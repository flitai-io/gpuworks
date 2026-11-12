# 部署启动参数来源

本文说明推理容器最终的 entrypoint、argv、环境变量、挂载、docker 参数、`/dev/shm`、端口、网络与设备，
各自从哪一层来、谁覆盖谁、什么条件下生效。entrypoint 是启动入口，argv 是传给入口的参数列表。
术语以 [`CONTEXT.md`](../../CONTEXT.md) 为准
（[解析后启动方案]、[容器覆盖]、[命令模式]、[实例期望状态]）。

## 唯一真相与两个边界

服务端只有一个合并点：`deploy.ResolveLaunch`（`internal/server/deploy/assemble.go`）。部署页预览与
controller 都经 `planning.PlanLaunch` 调它，产物是 `proto.ResolvedLaunch`，序列化进实例期望状态
（`Instance.DesiredStateJSON` 的 `launch`）后下发 agent。`TestPlanLaunchAgreesBetweenTheControllerAndThePreview`
守护「预览 == 下发」。

**预览与实际运行的差异只有这些**，其余字段逐字相同：

| 差异 | 预览 | 实际 |
|---|---|---|
| API 端口 | 固定 `30000` | 调度分配的端口 |
| 未解析的放置变量 | `$TP_SIZE`、`$GPU_COUNT`、resolver 变量保留为符号 | 按分配到的 GPU 展开 |
| 模型组件挂载源 | `$MODEL_COMPONENT_<NAME>_PATH` | 节点上的真实目录 |
| agent 侧注入（见「agent 注入」） | 不显示 | 设备、dGPU、准备脚本包装 |
| 镜像内置 ENV/ENTRYPOINT/CMD | 不显示 | 容器运行时照常继承；可在镜像详情对节点检查，或看实例「启动事实」里的观察值 |

镜像本身只是运行制品：镜像目录不提供任何会改写部署的启动配置（ADR 0022、ADR 0031）。

## 各层一览

按合并顺序列出。通常后面的层覆盖前面的层；表中标明「补缺」或「平台所有」的项按各自规则处理：
「补缺」只在该项尚未设置时生效；「平台所有」的覆盖限制以对应行说明为准。

| # | 层 | 贡献什么 | 与其他层的关系 | 代码 |
|---|---|---|---|---|
| 1 | 引擎基线 | 可执行文件、模型路径、`--host`/`--port`、`--served-model-name`、TP/PP、基线 env、健康/指标路径、`/dev/shm` 基线（callable 只有可执行文件、基线 env 与路径，不追加参数） | 最底层 | `deploy/vllm.go`、`sglang.go`、`callable.go` |
| 2 | 参数字典 | 已声明的字典参数（类型化字段）→ flag | 模板 args 与用户 args 里的字典 flag 先被拆成字段（ADR 0020），冲突报 `LAUNCH_CONFIG_CONFLICT` | `deploy/param_specs.go`、`materialize.go`、`args.go` |
| 3 | GPU/芯片方言 | 少数 vLLM 兼容性修正 | **补缺**：字段、模板与用户已设置时不生效 | `deploy/quirks.go` |
| 4 | 调度推导值 | 分配到的 GPU 数 → TP；放置变量 | 估算器的值**不进** argv | `planning/launch_plan.go` |
| 5 | 推理模板档位 | 字典外的 args、env、entrypoint、准备脚本、资产、组件、API 契约、`/dev/shm` 下限（`shm_bytes`）、不限锁定内存（`memlock_unlimited`） | 覆盖引擎注入的值；用户输入可再覆盖本层 | `inferencecatalog/flat.go`、`deploy/catalog_selection.go` |
| 6 | 拓扑 | 多机、PD 的角色参数与 env、RDMA docker 参数 | 平台所有，模板与用户不得重写 PD connector | `deploy/pd.go`、`compute_network.go` |
| 7 | 引擎收尾 | 如 SGLang 日志配置 | **补缺** | `deploy/sglang_logging.go` |
| 8 | 用户输入 | 字典字段、`extra_args` | 盖过模板 | `api/deployments_launch_config.go` |
| 9 | 容器覆盖 | env、env_unset、挂载、docker 参数、`/dev/shm`（不填时由引擎基线加 CPU 内存层推导，见「`/dev/shm` 大小」）、模型挂载路径、entrypoint、命令、命令模式 | 部署级最终一层 | `deploy/assemble.go`、`kv_cpu_tier.go`、`effective_command.go` |
| 10 | agent 注入 | 模型目录挂载、准备脚本包装、设备、dGPU 分区 | 设备与 dGPU 的 env **覆盖一切** | `agent/engine/engine.go`、`agent/gpu/*.go`、`agent/engine/partition*.go` |
| 11 | 运行时拼装 | `docker run` 固定部分：名称、`-d`、`--restart no`、网络、`--shm-size` | 平台所有，docker 参数里写这些会被拒绝 | `agent/runtime/docker/docker.go`、`nerdctl/nerdctl.go` |

## 引擎基线

| 引擎 | 命令 | 基线 env |
|---|---|---|
| vLLM | `vllm serve <模型路径>`，`--host 127.0.0.1`、`--port`、`--served-model-name`（headless 时不带）、`--tensor-parallel-size` 总是注入、`--pipeline-parallel-size` 大于 1 时注入 | `HF_HUB_OFFLINE=1`、`TRANSFORMERS_OFFLINE=1`、`VLLM_HOST_IP=127.0.0.1`、`GLOO_SOCKET_IFNAME=lo`、`NCCL_SOCKET_IFNAME=lo` |
| SGLang | `python -m sglang.launch_server --model-path … --tp-size --pp-size --host --port --served-model-name`（全部在 entrypoint 里） | `HF_HUB_OFFLINE=1`、`TRANSFORMERS_OFFLINE=1` |
| callable | `gpuworks-callable`（通常被模板 entrypoint 替换），argv 只有模板参数与用户参数，平台事实由模板以 `${MODEL_PATH}`、`${API_HOST}`、`${API_PORT}` 等变量摆放（ADR 0036） | 同上，另并入模板 env |

三者的 `/dev/shm` 基线都是 16 GiB（引擎 Profile 的 `shm_bytes`，经 `GET /api/v1/engines` 下发）。
这只是基线：部署实际得到的大小在它之上再加 CPU 内存层，规则见「`/dev/shm` 大小」。
健康检查路径默认 `/health`，模板 API 契约的 `health_endpoint` 可以覆盖；指标路径、前缀与映射只来自引擎 Profile。

## 参数字典

参数字典（`GET /api/v1/engines` 的 `param_specs`）列出平台认识的引擎参数，描述每个参数在各引擎里的 flag 拼写与取值格式。

- **只有新请求会写入默认值**。已存部署不补默认值。
- 默认值写进配置，**不等于出现在 argv 里**：
  - 会出 flag：vLLM `gpu_memory_utilization` 0.9、`enable_cache_report`；SGLang `log_level`、`--mem-fraction-static 0.88`、`--enable-cache-report`。
  - 看镜像版本：vLLM `enable_request_metrics`（`--enable-per-request-metrics`）只在镜像目录或镜像 tag 表明为 vLLM 0.25.0 及以上、且模板与 `extra_args` 都没有 `--disable-log-stats` 时套默认值；SGLang diffusion server 从 0.5.18 起套 `enable_cache_report`、从 0.5.21 起套 `enable_metrics`。版本未知或更旧的镜像上不写默认值、不出 flag（表单显示「自动」，用户仍可显式打开），以免引擎因不认识参数起不来。请求没指定镜像时，按目标节点（未选节点时按目标厂商）从镜像目录挑出的镜像判断。
  - 只写配置不出 flag：`dtype auto`、`kv_cache_dtype auto`、`performance_mode balanced`、值为 false 的布尔开关。
  - 没有默认值：`max_model_len`、三态开关（`enable_prefix_caching` 等）与其他 `automatic` 行。
- 平台参数（如 `kv_disk_limit_gib`）永远不进 argv，只影响平台行为（ADR 0021）。
- 当前 entrypoint 方言或镜像版本可能不接受的参数不写默认值，交给引擎决定（`engine_decides_param_keys`），但不隐藏、不拒绝：表单显示「自动」，用户仍可开或关，表单上看到的值与 argv 一一对应。
- 镜像系列的「常用参数/环境变量候选」只是按钮：点了才写进用户的 `extra_args` 或环境变量行，不点不生效。

优先级：显式字段 > `extra_args` > 模板 args；同一参数两处给出不同值时拒绝，而不是悄悄挑一个。

## GPU/芯片方言

argv 层面的硬件规则**只有** vLLM 的三条兼容修正，全部是补缺：字段、模板或用户已经给了同一 flag 时不生效。

| 规则 | 条件 | 注入 |
|---|---|---|
| `turing-no-bf16` | NVIDIA 且 compute capability < 8.0（V100、T4 等） | `--dtype float16` |
| `deepseek-mla-fp8-kv` | DeepSeek MLA 架构，非昇腾 | `--kv-cache-dtype fp8` |
| `minimax-block-size` | MiniMax 架构 | `--block-size 128` |

判断依据取自分配到的第一张 GPU 的厂商、架构与 compute capability。SGLang 没有这类规则；昇腾、海光、PPU
在 argv 层没有硬编码规则。厂商差异来自两处：

- **档位选择**：推理模板按 GPU 匹配条件（谓词）为每类卡选出不同档位（`chipmatch`），各档位自带 args 与 env；每次重调度都会复查。
- **agent 设备注入**：见「agent 注入」。

dtype 兼容矩阵只用于准入与告警，不改写 argv。

## 调度推导值

- 分配到的 GPU 集合决定 `--tensor-parallel-size`/`--tp-size`，最终 argv 还要通过拓扑校验。
- 档位的 placement 解析器可以输出变量，见[变量展开](#变量展开)。
- 模型权重大小决定 KV 磁盘层挂载的容量上限（`size_hint`）。
- 估算器使用的 `max_model_len`、显存利用率等只参与估算，**不会写回 argv**。

## 推理模板档位

档位字段语法见 [推理目录 model.yaml 语法参考](inference-catalog-spec.md)，此处只写它们落到哪里：

| 档位字段 | 落点 |
|---|---|
| `args` | 字典能识别的拆成类型化字段；其余作为模板剩余参数原样透传，排在引擎注入之后。模板写过的 flag 平台不再注入，用户写的同名 flag 覆盖模板 |
| `env` | `env_layers.template` |
| `entrypoint` | 只替换可执行文件部分，保留引擎生成的模型与拓扑参数 |
| `pre_start_argv` / `pre_start_script` | 由 agent 包装在主命令之前执行（见「agent 注入」） |
| `assets` | agent 只读挂载到 `unpack_to` |
| `components` | 主组件成为模型挂载路径，其余以只读挂载进入 |
| `api` | API 契约与健康检查路径 |
| `shm_bytes` | 容器 `/dev/shm` 的下限，见「`/dev/shm` 大小」；随档位快照保存，之后改模板不影响已有部署 |
| `memlock_unlimited` | 为 `true` 时容器带 `--ulimit memlock=-1:-1`，见「锁定内存」；随档位快照保存 |
| `image` | 部署使用的镜像；无需先在镜像目录登记 |

模板档位固定使用 `engine` 命令模式；`image-entry`、`manual` 只能由本次部署显式选择。

## 拓扑

- **多机**：rank 0 为 leader，其余为 headless follower。vLLM 注入 `--distributed-executor-backend mp --nnodes --node-rank --headless --master-addr --master-port`（端口默认 29500），`VLLM_HOST_IP` 取节点的参数网络地址；SGLang 注入 `--nnodes --node-rank --dist-init-addr`。
- **参数网络**：注入 `NCCL_SOCKET_IFNAME`、`GLOO_SOCKET_IFNAME`、`NCCL_IB_*`、`UCX_NET_DEVICES` 等系统层 env；使用 RDMA 时追加 docker 参数 `--device /dev/infiniband --ulimit memlock=-1 --cap-add IPC_LOCK`（ADR 0027）；部署自己的 docker 参数已写 `memlock` 时不再追加这一项，见「锁定内存」。
- **PD 分离**：按角色生成 vLLM `--kv-transfer-config` 或 SGLang `--disaggregation-*`，以及 NIXL/UCX 相关 env 与主机挂载；`vllm-mooncake` 另在 `kv_connector_extra_config` 写 `mooncake_protocol`（主机 RDMA 网络为 `rdma`，否则 `tcp`），并给 prefill 设 `VLLM_MOONCAKE_BOOTSTRAP_PORT`、给所有实例设 `VLLM_HOST_IP`，模型含线性注意力或 Mamba 层（`config.json` 的 `layer_types`）时再加 `--no-disable-hybrid-kv-cache-manager`（模板或用户写了任一极性时不加）；模板或用户写 connector 参数会被拒绝（ADR 0028）。

sleep、LoRA、投机解码、KV 卸载等特性只在对应字典字段被声明时注入参数，例如 vLLM sleep 注入
`--enable-sleep-mode` 与 `VLLM_SERVER_DEV_MODE=1`。

## 用户输入与容器覆盖

### 参数

字典字段与 `extra_args` 见「参数字典」；`quick_params_unset` 表示显式关闭模板或默认开启的参数。

### 命令模式

| 模式 | entrypoint | argv | env |
|---|---|---|---|
| `engine`（默认） | 引擎生成；覆盖 entrypoint > 模板 entrypoint | 引擎参数 + 模板剩余 + 用户剩余 | 系统 → 模板 → 用户 |
| `image-entry` | 空（使用镜像 ENTRYPOINT），可覆盖 | 覆盖命令（若有）+ 引擎参数向量 | 系统 → 模板 → 用户 |
| `manual`（手写命令） | `/bin/sh -c`，整段脚本由用户写 | `--port`、`--served-model-name` 会被改写成平台分配值 | **不含系统层**，只有模板 → 用户 |

手写命令勾选「命令已包含准备脚本」后，模板的准备脚本不再执行，避免重复。

### 容器覆盖字段

| 字段 | 作用 | 限制 |
|---|---|---|
| `env` / `env_unset` | 新增、覆盖环境变量；删除引擎或模板设置的变量 | 设备与 dGPU 的 env 由 agent 在节点上注入，不受影响 |
| `extra_mounts` | 主机目录或命名卷 → 容器路径，可只读 | 路径须绝对、非根；不得与平台挂载重叠或嵌套 |
| `model_mount_path` | 模型挂载路径（默认 `/model`），同时改写命令里的模型路径 | 有模型组件时不得改主组件路径 |
| `shm_bytes` | 容器 `/dev/shm` tmpfs 的大小上限，不是内存预留；不填为引擎基线 16 GiB 加上 CPU 内存层的大小（没开内存层就是 16 GiB），模板档位声明了 `shm_bytes` 且更大时取模板的值；docker 参数含 `--ipc=host` 时启动配置不带大小（也不下发 `--shm-size`），容器共用宿主机 `/dev/shm` | 与 `--ipc=host` 同时设置被拒（`SHM_SIZE_WITH_HOST_IPC`）；小于 CPU 内存层被拒（`SHM_BELOW_KV_CPU_TIER`） |
| `docker_args` | 按顺序追加到 `docker run`；最终的参数里还有平台追加的 RDMA 选项，以及模板档位 `memlock_unlimited` 带来的 `--ulimit memlock=-1:-1` | 见下表 |
| `entrypoint_display`、`cmd_display`、`command_mode`、`command_includes_preparation` | 见命令模式 | — |

### `/dev/shm` 大小

vLLM 把 CPU 内存层（字典参数 `kv_offloading_size`，GiB）放在 `/dev/shm` 里，所需空间等于整个内存层
（所有 worker 的总量，不是每个张量并行 rank 各一份；实测 19 GiB 的内存层要 19450 MiB）。
容器的 `/dev/shm` 比它小，引擎启动就以 `Insufficient space in /dev/shm` 退出，所以 `ResolveLaunch`
按下面的规则推导大小，`--kv-transfer-config` 里的 `cpu_bytes_to_use` 与这里读的是同一个值
（`deploy.KVCPUTierBytes`）：

| 情况 | `/dev/shm` |
|---|---|
| docker 参数含 `--ipc=host` | 不带大小，容器用宿主机的 `/dev/shm`（此时不校验内存层） |
| 部署填了 `shm_bytes` | 用填的值（盖过模板的 `shm_bytes`，即使比它小）；小于 CPU 内存层时 `ResolveLaunch` 就报 `SHM_BELOW_KV_CPU_TIER`（参数 `shm_gib`、`tier_gib`） |
| 没填 | 「引擎基线 + CPU 内存层」与「模板档位的 `shm_bytes`」中较大的一个；没开内存层、模板也没声明就是引擎基线 |

- 模板档位的 `shm_bytes` 是引擎自己需要的 `/dev/shm` 总量（例如 DeepInfer 把约 400 GB 的 Engram 表放在共享内存里；DeepSeek-V4.1-Flash 原版与 1017 的 A3 档各声明 512 GiB，容纳两张约 103 GiB 的本地 DP 共享表），所以与派生值取较大者，而不是相加；它不属于容器覆盖，镜像目录同样不贡献它（ADR 0031 不受影响）。部署自己填的 `shm_bytes` 永远优先，模板的值不会让一个小于内存层的显式值通过校验。模板声明的值超出节点实际内存时，容器照常启动（`--shm-size` 只是上限，不是预留），写入超限时由引擎报错。
- 基线保留给 NCCL、torch IPC 等其他共用 `/dev/shm` 的东西：内存层分配失败时 16 GiB 是完全空闲的，所以是相加而不是从中划出。填写的值只要求不小于内存层，要给这些留余量由部署者自己加。
- 内存层只在引擎能表达该参数、且命令由平台生成时才算。手写命令（`manual`）或完整的 `cmd_display` 覆盖由用户负责自己的命令，平台不按快速参数里残留的内存层推导，也不据此拒绝。
- PD 分离只有 prefill 角色带内存层（decode 角色的卸载配置被去掉），所以 decode 实例仍是引擎基线；部署级的 `shm_bytes` 对两个角色同样适用，因此必须满足 prefill。预览里每个实例的 `shm_bytes` 各自如实给出。
- 评估接口把该错误挂在共享内存控件（`container_shm_gib`）旁，创建接口返回 400，两者都在任何东西开始运行之前。
- 不校验的情况：udocker 没有 `--shm-size`，容器用宿主机的 `/dev/shm`（默认是内存的一半）；`--ipc=host` 也用宿主机的。平台目前不上报宿主机 `/dev/shm` 的大小，这两种情况内存层超出宿主机 `/dev/shm` 时仍会在引擎启动时失败。

### 锁定内存（memlock）

有的引擎要把大块主机内存锁页（例如 DeepInfer 用 `cudaHostRegister` 锁住约 197 GB 的 Engram 表，DeepSeek 的 A3 档用 `aclrtHostRegisterV2` 注册共享 Engram 表），默认的 `memlock` 上限会让它启动失败。模板档位写 `memlock_unlimited: true` 即可只对该部署的容器取消上限，不改宿主机。DeepSeek-V4.1-Flash 原版与 1017 的 A3 档都这样声明，不在模板 `args` 里重复写 Docker 的 memlock 参数。

| 情况 | 最终 docker 参数里的 `memlock` |
|---|---|
| 部署自己的 `docker_args` 写了 `--ulimit memlock=…`（`--ulimit memlock=…`、`--ulimit=memlock=…` 两种写法都算） | 用部署写的那一条，模板与 RDMA 都不再追加 |
| 没写，档位 `memlock_unlimited: true` | `--ulimit memlock=-1:-1` |
| 没写，主机 RDMA 网络上的多机 rank 或 PD 实例 | `--ulimit memlock=-1`（档位同时写了 `memlock_unlimited` 也只有这一条） |
| 没写，档位未声明（缺省或 `false`），也不在主机 RDMA 网络上 | 不带 `memlock` |

- 最终参数里 `memlock` 至多一条，由 `deploy/memlock.go` 的 `launchDockerArgs` 统一决定。
- 这一项在 `ResolveLaunch` 里生成，所以部署页的「运行规格」预览（评估接口的 `docker_args`）和实例详情的运行时信息显示的就是容器真正会带的参数。
- 取消上限需要容器有权限：docker 在默认配置下允许 `--ulimit memlock=-1`；udocker 只解析 `--device`，不应用 ulimit。
- 档位的值随部署的档位快照保存，之后改模板不影响已有部署。

### docker 参数里不允许的项

平台自己的 docker 参数（RDMA）不受此限；这里只校验部署自己写的。

| 写法 | 错误码 | 应改为 |
|---|---|---|
| `--name`、`-d`/`--detach`、`-a`、`--rm`、`--restart`、`--network`/`--net`、`-p`/`--publish`、`-P`、`--gpus`、`--runtime` | `DOCKER_ARG_PLATFORM_MANAGED` | 由平台设置，不可改 |
| `--shm-size` | `DOCKER_ARG_USE_SHM_FIELD` | `shm_bytes` |
| `-e`、`--env`、`--env-file` | `DOCKER_ARG_USE_ENV_FIELD` | 环境变量行 |
| `-v`、`--volume`、`--mount` | `DOCKER_ARG_USE_MOUNT_FIELD` | 挂载行 |
| `--entrypoint` | `DOCKER_ARG_USE_ENTRYPOINT_FIELD` | entrypoint 覆盖 |
| `--privileged`、`--pid`、`-v /` 挂载主机根目录 | 安全拒绝（`FIELD_INVALID`） | 不支持 |

评估接口把这些错误挂在部署表单的 docker 参数或共享内存控件旁；创建接口返回 400。udocker 运行时只解析其中的 `--device`。

## 环境变量分层

`ResolvedLaunch.env_layers` 记录每层来源，`env` 是合并结果：

1. `system`：引擎基线 env + 参数网络 env。
2. `template`：档位 env。
3. `user`：容器覆盖 env。

`env_unset` 在 `user` 之前生效：从合并结果里删除 `system`、`template` 设置的键（例如去掉引擎注入的
`HF_HUB_OFFLINE=1` 让自定义镜像联网下载），`env_layers` 仍保留各层原值以便追溯来源；同名键再写进 `env` 则以部署的值为准。

之后还有两类补充：SGLang 日志配置与 agent 的多机网卡填充都是补缺；agent 的设备与 dGPU env 覆盖以上所有层。

镜像内置 ENV 在容器运行时继承，只有被 `-e` 显式设置的键会被替换（例如昇腾 vcann-rt 会先读出镜像的
`LD_LIBRARY_PATH` 再追加驱动路径）。

udocker 运行时（runveil 与 udocker CLI 两种启动器）在非国产卡上自带
`LD_LIBRARY_PATH=/opt/nvidia-shim:/usr/local/nvidia/lib64:/usr/local/nvidia/lib:/usr/local/cuda/lib64`：部署 env
未设置该键时，它排在镜像值之前合并（去重、丢弃空段）；部署 env 设置了则原样采用部署值。

## 变量展开

每次启动都展开，作用于模板的 args、env、entrypoint 与准备脚本；用户输入**从不展开**。
可用变量：解析器输出，加上平台给出的 `GPU_COUNT`、`TP_SIZE`、`MODEL_PATH`、`SERVED_MODEL_NAME`、`API_HOST`、`API_PORT`、
`NNODES`、`NODE_RANK`、`HEADLESS`，多机时另有 `NODE_ADDR`、`MASTER_ADDR`、`MASTER_PORT`；`PP_SIZE` 没有默认值。
写法 `${NAME}`，名称为大写字母、数字与下划线。完整词表、出现条件与报错规则见
[推理目录语法](inference-catalog-spec.md#变量展开)。

## agent 注入

agent 不重建目录、镜像或命令语义，只做以下节点事实相关的补充，按顺序：

1. 模型目录只读挂载到 `model_mount`。
2. vLLM 多机：`GLOO_SOCKET_IFNAME`、`NCCL_SOCKET_IFNAME` 为空时按 `VLLM_HOST_IP` 所在网卡填充。
3. 档位资产只读挂载。
4. 有准备脚本时写 `/gpuworks/pre-start.sh`（`set -eu`、脚本、`exec "$@"`），entrypoint 变为 `/bin/sh /gpuworks/pre-start.sh`，原命令作为参数。
5. KV 磁盘层挂载源与配额。
6. **设备注入**（覆盖同名 env，替换已有的 `--gpus`/`--runtime`）：
   - NVIDIA：`--gpus "device=…"` + 从 0 开始编号的 `CUDA_VISIBLE_DEVICES`
   - 海光：`--device` kfd/DRM、`/opt/hyhal`、`--group-add video`、`HIP/ROCR/HYGON_VISIBLE_DEVICES`
   - 昇腾 docker：`--runtime ascend` + `ASCEND_VISIBLE_DEVICES` 等；昇腾 nerdctl：`/dev/davinci*` 设备与驱动挂载
   - PPU：`/dev/alixpu*` 设备、`NVIDIA_VISIBLE_DEVICES=void`、`CUDA_VISIBLE_DEVICES`
   - 昆仑芯：Linux amd64 / Docker 整卡 TP；逐卡重新核对 UUID/BDF/minor 与 sysfs 后，注入所选 `/dev/xpuN` 和一个 `/dev/xpuctrl`。设备占用由公共计算进程清单上报，不以显存用量替代占用检查。模板参数遵循公共管理员契约，不另设厂商专属 Docker 参数过滤器。多机原生通信依赖见[厂商支持矩阵](support-matrix.md)。
7. **dGPU 分区**：服务端只下发显存上限、算力百分比与后端名，注入全部在 agent：
   - HAMi-core：挂载 `libvgpu.so`，`LD_PRELOAD`、`CUDA_DEVICE_MEMORY_LIMIT_<i>`，以及部署带算力百分比时的 `CUDA_DEVICE_SM_LIMIT`（部署请求目前不接受算力百分比，见 [dGPU 说明](../explanation/dgpu.md)）
   - vcann-rt：挂载 `libvruntime.so` 等，`ENPU_DSMI_HOOK=1`、`LD_LIBRARY_PATH`
   - HGGC：挂载 `hggc_quota.so` 等，`LD_PRELOAD`、`HGGC_DEVICE_MEMORY_LIMIT_<i>`

详见 [dGPU 说明](../explanation/dgpu.md)。

## 运行时拼装

docker 的参数顺序：

```text
docker run -d --name gpuworks-<uuid前12位> --restart no
  --network host                 # Linux；其他系统改为 -p 127.0.0.1:<端口>:<端口>
  <docker_args>                  # 部署 docker 参数 + 平台 RDMA 参数 + 档位 memlock_unlimited + 设备注入
  --shm-size <shm_bytes>         # shm_bytes 为 0（--ipc=host）时省略
  -e …  -v …
  --entrypoint <entrypoint[0]> <镜像> <entrypoint[1:]> <argv>
```

nerdctl 顺序相同但不带 `--restart`；udocker 不支持网络、shm 与大部分 docker 参数。

## 相关文档

- ADR [0009](../adr/0009-flat-spec-catalog.md)（扁平档位）、[0013](../adr/0013-image-command-mode.md)（命令模式）、
  [0020](../adr/0020-launch-config-single-source.md)（启动配置单一来源）、[0021](../adr/0021-platform-params-copyable-command.md)（平台参数不进 argv）、
  [0022](../adr/0022-template-owned-deployment-launch.md)（模板主导启动）、[0031](../adr/0031-image-catalog-carries-no-launch-config.md)（镜像目录不承载启动配置）
- [推理目录 model.yaml 语法参考](inference-catalog-spec.md)、[镜像目录与推理镜像家族](image-families.md)
- [部署模型](../how-to/deploy-models.md)、[多机推理](../how-to/multi-node-inference.md)、[PD 分离](../how-to/pd-disaggregation.md)
