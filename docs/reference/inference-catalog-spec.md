# 推理目录 model.yaml 语法参考

本文是 `models/<model-id>/model.yaml` 的逐字段语法参考。一个模型可以有多个 spec（档位）；
每个 spec 描述一套可部署配置，包括适用 GPU、镜像、权重来源和启动参数。权威定义在代码：
结构体与基础校验在 `internal/server/inferencecatalog/flat.go`，可执行契约
（assets / placement / api / components）在 `contracts.go`，操作词表在
`internal/server/inferenceapi`，Starlark 求值器在 `internal/server/placement`，
估算子集在 `pkg/modelconfig`。目录包的布局、digest 与 `model_config` 的背景见
[可执行推理目录包](inference-catalog-package.md)，设计动机见 ADR 0009/0010。
两篇文档冲突时以代码为准，发现不一致先改文档或提 issue。

解析器（`ParseYAML`）的行为约定适用于全文档：

- **未知键直接拒绝**（`KnownFields(true)`），退役字段不可能静默存活于导入；
- 一个文件恰好一个 YAML 文档；
- 导出（`MarshalYAML`）同样先过校验，写出的文档一定能再导入；
- 大小写不敏感的字段（model_id、source、engine、quantization、vendor 等）一律
  小写归一后存储，model_id、spec 名要求在各自范围内唯一。

## 总览

```yaml
models:                       # FlatCatalog：顶层只有一个 models 列表
- model_id: wan-ai/wan2.1-t2v-1.3b-diffusers   # 必填，org/repo 形式，小写唯一
  source: upstream            # upstream | user，默认 upstream
  display_title: wan2.1-t2v-1.3b-diffusers
  parameter_count: 1.3B class # 自由文本
  architecture: WanPipeline / WanTransformer3DModel / ...   # 自由文本
  input_modalities: [text]    # text | image | audio | video，去重
  model_config: {kv_cache: false}                # 估算子集，见包文档
  readme_zh_cn: README.zh-CN.md                  # 仅允许这两个固定值
  readme_en: README.en.md
  kv_bytes_per_token: {vllm: 12288, sglang: 12288}  # 按 engine 实测
  context_length: 40960
  release_date: '2025-03-01'  # YYYY-MM-DD 且是合法日期
  unsupported:                # 模型级"不可用"声明，UI 展示 reason
  - gpus: {vendor: nvidia, cc: '<8.0'}
    quantization: bf16        # 可选：限定到某量化
    reason: 计算能力不足       # 必填
  specs:                      # 每模型至少 1 个 spec（被引用为 component 除外）
  - name: bf16-sglang-nvidia-general   # 模型内唯一
    display_name_zh_cn: A100 / H100 · BF16   # 可选，见下文「显示名」
    display_name_en: A100 / H100 · BF16
    scenario: balanced        # latency | balanced | throughput
    engine: sglang            # vllm | sglang | callable
    image: gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2
    image_sources: []         # 可选：镜像的更多拉取地址，见下文 image_sources
    weights:
      quantization: bf16      # 必填
      sources:                # 至少 1 个，紧凑单键形式
      - modelscope:
          repo_id: Wan-AI/Wan2.1-T2V-1.3B-Diffusers
          revision: 9c0c3b1a54efbd0d1674386cb8dd720fa4e6dc3d
    weight_bytes: 28935653617
    shm_bytes: 429496729600   # 可选：引擎需要的 /dev/shm 字节数（这里是 400 GiB）
    memlock_unlimited: true   # 可选：容器不限锁定内存（--ulimit memlock=-1:-1）
    start_timeout_sec: 5400   # 可选：实例启动超时（秒），首次启动很长的配方用
    docker_args: [--cap-add=SYS_PTRACE]   # 可选：引擎需要的额外 docker run 选项
    gpus:
      vendor: nvidia          # 必填
      cc: '>=8.0'             # 精确值 / >=x / <x / a..b
      name: '(^|[^A-Za-z0-9])(?:A100|H100)([^A-Za-z0-9]|$)'   # 可选，大小写不敏感正则
    env:
      NCCL_NET_PLUGIN: none
    args:
    - --num-gpus
    - ${GPU_COUNT}            # placement/运行时变量展开
    entrypoint: [sglang, serve]   # 覆盖引擎默认入口，见下文
    placement:
      language: starlark      # 目前唯一语言
      resolver: |
        def resolve(gpu):
            if gpu.min_memory_gib < 32:
                return reject("each GPU requires at least 32 GiB")
            return accept(gpu_memory_mib_per_device=32768,
                          variables={"GPU_COUNT": gpu.count})
    api:
      health_endpoint: /health
      operations:
      - type: openai.video.generate
        endpoint: /v1/videos
        mode: async
        inputs:
        - {name: seconds, kind: number}
        - {name: size, kind: select, options: [640x360, 1280x720]}
    components: [...]         # 多权重组合，见下文
```

## 模型级字段

| 字段 | 约束 | 说明 |
|---|---|---|
| `model_id` | 必填，`^\S+(/\S+)*$` 且不含点段/反斜杠 | 小写归一，全目录唯一；同时是包内目录名 |
| `source` | `upstream` \| `user`，默认 `upstream` | 来源分类 |
| `display_title` / `parameter_count` / `architecture` | 自由文本，trim | 展示用 |
| `input_modalities` | 每项 ∈ `text/image/audio/video` | 去重后保序；驱动 UI 输入模态展示 |
| `model_config` | `modelconfig.Config` 形状 | config.json 估算子集 + 目录专属 `kv_cache: false`，详见包文档 |
| `readme_zh_cn` / `readme_en` | 只允许 `README.zh-CN.md` / `README.en.md` | 声明"包内携带该 README" |
| `kv_bytes_per_token` | map：engine 名 → 非负整数 | **引擎自己的实测数**，不是推导值；键缺失表示该引擎无值，部署表单显示"无 token 容量"而不是错值 |
| `context_length` | 非负 | 模型声明上下文长度 |
| `release_date` | `YYYY-MM-DD` 且可解析 | |
| `unsupported[]` | `gpus`（同下文选择器）+ `reason` 必填 + 可选 `quantization` | 命中时 UI 展示不可用原因 |

## spec 级字段

| 字段 | 约束 | 说明 |
|---|---|---|
| `name` | 必填，模型内唯一 | |
| `display_name_zh_cn` / `display_name_en` | 可选；单行，分别不超过 56 / 72 个字符；同一模型、同一引擎内各自唯一（忽略大小写与多余空格） | 档位的中英文显示名，界面按语言显示，缺一种时用另一种，都没有时显示 `name`。内置档位两个都写。写法见下文[显示名](#显示名) |
| `scenario` | `latency` \| `balanced` \| `throughput` | |
| `engine` | 必填；部署时按注册表解析，合法值 `vllm` / `sglang` / `callable`（`internal/engines` 唯一产地） | `callable` 承载目录自带 Serve Adapter，无日志方言与参数字典；**命令行完全归模板**，引擎不追加任何参数，模型路径、端口、多机 rank 等平台事实由模板用[变量](#变量展开)自己摆放，且必须引用 `${GPUWORKS_API_PORT}`（或旧写法 `${API_PORT}`，ADR 0036） |
| `image` | 必填 | 镜像引用语法与漂移问题见 [镜像家族](image-families.md) 与 todo.md T2 |
| `image_sources[]` | 可选；每项 trim 后非空、不含空白、不含 `://`、≤ 512 个字符，不重复；与 `image` 相同的项被丢弃（引用的比较方式见下文） | 模板自己声明的镜像来源，在节点启动副本时作为镜像自身副本参与候选，详见 [image_sources](#image_sources模板声明的镜像来源) |
| `weights.quantization` | 必填，小写 | |
| `weights.sources[]` | 至少 1 个 | 见下 |
| `weight_bytes` / `runtime_weight_bytes` | 非负；后者 ≤ 前者 | 调度与估算输入 |
| `shm_bytes` | 可选整数，字节；> 0 且 ≤ 16 TiB（`1<<44`），否则报稳定错误码 `CATALOG_SHM_BYTES_INVALID`；显式写 `0` 等同于不写 | 引擎自己需要的容器 `/dev/shm` 大小，例如把大块查表数据放在共享内存里的引擎。部署不填共享内存大小时，容器得到「引擎基线 + CPU 内存层」与它之间较大的一个；部署自己填的值优先。详见 [容器 `/dev/shm` 大小](deployment-launch-sources.md#devshm-大小) |
| `memlock_unlimited` | 可选布尔，缺省 `false`；写成非布尔值（数字、字符串等）会被拒绝 | 为 `true` 时该部署的容器带 `--ulimit memlock=-1:-1`（只对这个容器取消锁定内存上限），给要锁页大块主机内存的引擎用（例如 DeepInfer 用 `cudaHostRegister` 锁住约 197 GB）。部署自己的 `docker_args` 已写 `memlock` 时以部署为准，不重复追加；与主机 RDMA 网络注入的 `memlock` 合并为一条。详见 [锁定内存](deployment-launch-sources.md#锁定内存memlock) |
| `start_timeout_sec` | 可选整数，秒；60–86400（与部署自己的 `start_timeout_sec` 同界），否则报稳定错误码 `CATALOG_START_TIMEOUT_INVALID`；显式写 `0` 等同于不写 | 该档位实例的启动健康检查预算，给首次启动要转换权重、预热缓存、耗时数十分钟的配方用。它是启动等待上限的模板一步（来源 `template`）：集群默认 → 模板 → 引擎等待上限抬高 → 部署的 `start_timeout_sec`，顺序与规则见 [启动等待上限](deployment-launch-sources.md#启动等待上限)。随档位快照保存，之后改模板不影响已有部署 |
| `docker_args` | 可选字符串数组；每项 trim 后非空；校验与部署自己的 `container_overrides.docker_args` 完全一致（`deploy.ValidateDockerArgs`）：拒绝 `--name、`--network`、`--gpus`、`--shm-size`、`-e`、`-v`、`--mount`、`--entrypoint` 等平台已管的选项，`--ipc=host` 与本 spec 自己的 `shm_bytes` 同时出现也拒绝；`--privileged`、`--pid` 不拒绝，部署评估时给出警告 | 引擎需要的额外 `docker run` 选项（例如 `--cap-add=SYS_PTRACE`），是部署 `docker_args` 的模板层：先套用模板的，再套用部署自己的，最后是平台追加的（RDMA、memlock）。部署自己若没写 `--ipc=host` 但模板写了，部署自己的 `shm_bytes` 会被拒绝而不是静默失效，因为那个大小不会生效。详见 [docker run 参数的三层顺序](deployment-launch-sources.md#docker-run-参数的三层顺序) |
| `dirs[]` | 可选，至多 8 项；`name` 为 1–32 个小写字母、数字或 `-`（首尾不为 `-`），spec 内唯一；`mount_path` 为容器绝对路径、规整、不是 `/`，与 spec 的其他容器路径不重叠（[一个容器路径空间](#模板与平台的分工)）；`limit_bytes` 为字节数，> 0 且 ≤ 64 TiB（`1<<46`）；`retain` 只能是 `deployment`，缺省时导入写回 | 平台为部署在它运行的每个节点上保留的可写主机目录，见 [dirs：托管的可写目录](#dirs托管的可写目录) |
| `gpus` | 必填 | 见下 |
| `args` | 字符串数组 | 进入引擎命令行，支持 `${VAR}` [变量展开](#变量展开)；部署时按参数字典拆分（见下） |
| `env` | 键非空 | 容器环境变量，值支持 `${VAR}` 展开 |
| `pd_mode` | 可选，PD 模式名（`internal/server/pdmode` 的名字，如 `sglang-mooncake`）；模式的引擎必须与 `engine` 相同 | 声明后该 spec 只能以这个模式的 PD 分离部署，其 `args` 与 `env` 是模型在 PD 下需要的配置。模式见 [PD 分离部署](../how-to/pd-disaggregation.md) |
| `pd_roles` | 可选，按角色写版式；必须同时写 `pd_mode`。每个角色：`tp`（必填，≥ 1）、`pp`、`nodes`（一个实例跨几台节点）、`units`（初始实例数），后三项缺省为 1 | PD 部署表单据此预填角色与实例。见 [PD 版式与手工命令](#pd-版式与手工命令) |
| `manual_command` | 可选字符串，shell 脚本（不是 JSON argv），≤ 64 KiB | 部署默认使用的手工命令。PD spec 的脚本必须引用 `${GPUWORKS_PD_ROLE}`。见 [PD 版式与手工命令](#pd-版式与手工命令) |
| `pre_start_argv` / `pre_start_script` | **二选一**；argv 项非空；script ≤ 64 KiB | 在**容器内**（不是宿主机）执行，**每次容器启动**都跑一遍，跑完才启动引擎：agent 把它写进 `/gpuworks/pre-start.sh`（`/bin/sh`、`set -eu`，最后 `exec` 引擎命令）并设为 entrypoint。能用容器的全部环境变量：spec 与部署的 `env`，以及 agent 注入的加速卡变量；`${GPUWORKS_*}` 变量由服务端在部署时[展开](#变量展开)。任何一步失败（非零退出、未定义变量）都让容器在引擎启动前退出，实例以容器退出失败，输出作为证据；包装脚本本身写不出时实例以 `PRE_START_PREPARE_FAILED` 失败 |
| `assets[]` | 见下 | 随包分发的小文件 |
| `entrypoint` | 项非空 | 见下文专节 |
| `placement` | 见下 | 拓扑推导 |
| `api` | 见下 | 对外 HTTP 契约 |
| `components[]` | 见下 | 多权重组合（投机解码等） |
| `performance_tradeoffs[]` | 见下；每项的 `param` 必须是本 spec 启动参数里确实带着的降速参数 | 说明本 spec 为什么带着降速参数（如 `--enforce-eager`），见 [performance_tradeoffs](#performance_tradeoffs降速参数的理由) |

### args：字典内拆进字段，字典外逐字透传

作者按上游提供的启动配方（recipe）原样填写参数列表（argv）。其中每个元素是一个命令行词元（token）。
部署时（ADR 0020）按参数字典（`internal/server/deploy/param_specs.go`）拆分这份列表：

- 字典认识的 flag 被解码成启动配置字段，供估算、调度和表单读取。
- 字典不认识的 token 按原顺序保留，作为模板剩余参数逐字进入引擎命令行。

一个 flag 被多个 key 共用（vLLM 的 `--speculative-config`、`--kv-transfer-config`）时同样
留在模板剩余参数里，不会被猜成某个字段。只有一个 key 拥有的 flag 若带着字典解不出来的值
（类型不符、枚举里没有的取值、非法 JSON），也不阻止导入：flag 和值原样留在剩余参数里交给引擎，
导入给出 `CATALOG_ARG_PASSED_THROUGH` 警告，点名 model、spec、flag 与值。字典落后于引擎时
（新的 parser、KV 精度等），模板照样能用，只是部署表单和显存估算看不到这个值；内置目录出现这种
警告说明字典该更新了。

大小写与别名不算解不出来：`--dtype half`
与 `--speculative-algorithm eagle3` 都接受，入库时归一成字典的写法（`float16`、`EAGLE3`）。
字典按引擎采用引擎自己的拼写：SGLang 的投机算法是大写的 `EAGLE`/`EAGLE3`/`NEXTN`/`DSPARK`/
`NGRAM`/`STANDALONE`（其模型 hook 在归一化前就按大写比较），是封闭列表；vLLM 是小写的方法名，
且和 tool call parser 一样只是建议列表（含 `dflash`、`dspark`），列表外的方法原样传给引擎，镜像里的
vLLM 是否支持由部署者确认；KV 精度两边
各自列全上游的取值（vLLM 的 `CacheDType`、SGLang 的 `--kv-cache-dtype` choices）。

同名但不同词表：`--quantization` 是引擎的量化方法（如 `modelslim`），`weights.quantization`
是权重格式（如 `int8`），二者互不校验。

若 spec 依赖某个字典参数的非默认值（例如 placement 契约声明的每卡显存接近整卡容量时需要更高
的 `--mem-fraction-static`），必须把它写进 `args`：部署配置只由这里和用户表单决定，不再有
隐式默认兜底。

厂商引擎构建的硬性要求同样逐 spec 写进 `args`，平台不按 vendor 注入。例：海光 DTK 的 SGLang
构建，其 flash-attention 内核只接受 64 token 的分页 KV 块；SGLang 默认的 `--page-size 1`
能完成加载、graph 捕获并通过 `/health`，却在第一次 `forward_extend` 报
`Paged KV cache block size must be 64` 并 VMFault，且容器进程不退出。DCU 上不写
`--attention-backend` 也会自动选 fa3，所以每个 hygon（或 DTK 镜像）的 sglang spec 都必须恰好
带一个 `--page-size 64`；`internal/server/inferencecatalog/seed/hygon_sglang_test.go` 对内置目录
按 spec 全量校验。用户在部署表单显式给的 `--page-size` 优先于模板，启动命令里该 flag 仍只出现一次。

### weights.sources：紧凑单键形式

每个来源是"恰好一个键"的映射，键为 provider（`huggingface` | `modelscope`），
值为仓库 id 或明细：

```yaml
sources:
- huggingface: org/repo                     # 标量形式
- modelscope:
    repo_id: org/repo                       # 必填，不含换行
    revision: <commit>                      # 可选，固定权重内容
    subfolder: sub/dir                      # 可选，归一化（无前导 /、无 ..）
```

`subfolder` 按目录前缀选取仓库里的一棵子树，下载后保留仓库内的相对路径。没有同名目录时，它也可以指向仓库里的单个文件，只下载这一个文件（例如从把多个 LoRA 平铺在根目录的仓库里取一个适配器）；同名目录与文件并存时选目录。

#### 按 rank 拆分的权重

多机配方预先把权重切成每个 rank 一份、放在同一仓库的不同子目录时，spec 自己的 `weights.sources`
或某个[组件](#components多权重组合)的 `weights.sources`
可以在 `subfolder` 里写 rank 变量 `${GPUWORKS_NODE_RANK}`（与模板字段一样，不带前缀的旧写法 `${NODE_RANK}` 是它的别名）：

```yaml
weight_bytes: 137438953472          # 一个 rank 的大小
weights:
  quantization: fp8
  sources:
  - modelscope:
      repo_id: org/model-tp4
      subfolder: rank${GPUWORKS_NODE_RANK}   # rank 0 读 rank0/，rank 3 读 rank3/
```

多机部署的每个 rank 只下载、只挂载自己那一份（决策见 [ADR 0076](../adr/0076-rank-scoped-weights.md)）：

- 创建部署时按节点数把子目录展开成每个 rank 一个模型制品（来源、仓库、revision 相同，子目录不同）；rank 0 的制品就是部署的模型，其余 rank 的绑定记进部署快照，此后不再从目录重新解析。
- revision 以模板现在写的为准：已登记的制品只要 revision 已知且与之不同，就不能代替（revision 为空表示未知，不算不同）。改写 revision 后重新导入，下次部署会为每个 rank 创建新 revision 的制品并下载，不会静默沿用旧 revision 的目录。部署页「使用已登记的模型」只列 rank 0 在该 revision 的制品（部署的模型就是它），其余 rank 的制品由平台创建与绑定，不作为选项。
- 每个节点的下载任务、模型标记与本地模型身份都是它那个 rank 的子目录；准入、磁盘预算与准备进度也按 rank 计算：rank `r` 的节点只为 rank `r` 的制品留磁盘，已有该子目录的节点不再计下载。
- 挂载仍保留仓库内的相对路径：rank `r` 的容器里权重在 `${GPUWORKS_MODEL_PATH}/rank<r>/…`，命令里写 `${GPUWORKS_MODEL_PATH}/rank${GPUWORKS_NODE_RANK}/…`。
- `weight_bytes`（及 `runtime_weight_bytes`）写一个 rank 的大小；显存估算按"每个 rank 一份"乘回整组的权重。
- 子目录里只能出现 rank 变量；同一组权重（spec 自己的或某个组件的）的所有来源要么都写、要么都不写；不能与 `pd_mode` 一起用，也不能以 PD 方式部署（`RANK_WEIGHTS_PD_UNSUPPORTED`）。请求显式指定 `model_id` 时，它必须是 rank 0 的制品（`RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`）。单机部署的 `${GPUWORKS_NODE_RANK}` 为 `0`（与变量表一致），只读 rank 0 那一份；子目录的份数由配方决定，平台不核对部署的节点数与之相符。

组件也可以按 rank 拆分，各组权重分别决定。典型形状是各 rank 相同的主模型加一个按 rank 拆分的辅助组件（例如两台 GB10 上
TP2 的 TensorFold 配方：EXL3 主包两机相同，Engram 表在 `rank0/`、`rank1/` 下各一份）：

```yaml
components:
- name: primary
  role: primary
  weights: {...}                          # 与 spec 自己的 weights 相同，各 rank 共用
  weight_bytes: 203793388493
  mount_path: /model
- name: engram
  role: auxiliary
  weights:
    quantization: bf16
    sources:
    - modelscope:
        repo_id: org/engram-tp2
        subfolder: rank${GPUWORKS_NODE_RANK}
  weight_bytes: 101379024392              # 一个 rank 的大小
  mount_path: /engram                     # rank r 的容器里是 /engram/rank<r>/…
```

- 组件的每个 rank 与上面的规则相同：创建时按节点数解析成每个 rank 一个制品（来源、仓库、revision 相同，revision 是身份），绑定记在部署快照该组件的 `rank_weights` 里，组件的 `store_model_id` 是 rank 0 的制品；rank `r` 的节点只下载、只挂载自己那份，挂载点仍是组件的 `mount_path`，其下保留仓库内的相对路径，命令里写 `/engram/rank${GPUWORKS_NODE_RANK}`。
- 组件的 `weight_bytes` 写一个 rank 的大小：磁盘准入按一个 rank 计，显存估算乘回整组，与主权重一致。部署评估的 `acquisition.components` 对这样的组件每个 rank 列一项。
- primary 组件就是 spec 自己的权重：二者要么都按 rank 拆分、要么都不拆；都拆时与不写 `components` 的按 rank 拆分权重相同（rank 绑定是部署自己的 `rank_weights`）。

同一 spec 多个来源表示"任一可拉"。部署的 `model_source.provider` 选定运行哪一家权重；节点怎么取到权重（已有、同步组内复制、从共享存储读取或下载）由[拉取设置](../how-to/configure-pull-settings.md)决定。

### image_sources：模板声明的镜像来源

和 `weights.sources` 一样，一个 spec 可以为运行时镜像声明多个来源。`image` 仍是必填项，是模板的
身份，也是"部署是否偏离模板"的比较基准；`image_sources` 是同一个镜像可以去哪里拉的更多引用：

```yaml
image: ghcr.io/org/runner@sha256:22789f0c…        # 模板的镜像（上游）
image_sources:                                    # 按偏好顺序
- swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@sha256:22789f0c…   # 逐字节相同的公开副本
- ghcr.nju.edu.cn/org/runner:v0.6.0                                       # ghcr 镜像站，只是 tag 引用
```

**模板写什么就接受什么，平台不替作者判断**：来源不必和 `image` 钉同一个 digest，`image` 钉了 digest 时
来源也可以是 tag 引用，是否可达也不检查。校验只管语法：每项去首尾空白后不能为空、不含空白、不含
`://`（镜像归档的下载地址属于镜像目录，不属于模板）、不超过 512 个字符、不重复。

比较两个引用时按 registry 的解析方式：`nginx:1` 与 `docker.io/library/nginx:1` 是同一个，registry 主机名和仓库名
不区分大小写，tag 区分大小写（`:V1` 与 `:v1` 是两个镜像）。
与 `image` 相同的项被丢弃——`image` 本来就是候选，排在声明的来源之后，重复写一遍没有信息。导出时同样
只写规范化后的列表。

**部署时怎么用**（决策见 [ADR 0038](../adr/0038-template-image-sources.md)、[ADR 0048](../adr/0048-pull-settings.md)）：

- 部署不选择镜像来源。节点启动副本时，候选先是适用于该节点的[来源规则](../how-to/configure-pull-settings.md#来源规则)产生的替代地址，其后是镜像自己的副本：模板的 `image_sources`（按声明顺序）→ 镜像又恰好登记在镜像目录里时，目录的
  `registry_sources` → `image` 自身，去重；目录里登记的镜像归档地址是要下载的文件，不是候选。这批副本由节点探测 `https://<host>/v2/` 按可达性与延迟排序，前一个失败或下载卡住才试下一个；使用模板声明的来源**不算偏离模板**。
- 来源规则对所有镜像生效，包括模板声明的来源：规则的替代地址排在最前，关闭回源的规则会让匹配的副本本身被丢弃。除此之外模板声明的来源原样使用，不被改写。
- 只有部署用的镜像等于 spec 的 `image` 时才有这些来源。请求里覆盖了镜像（`image` 字段不同）就是另一个镜像，
  模板的来源不使用。
- `image` 是 digest 引用（`repo@sha256:…`）时，运行时不能给 digest 引用打 tag，所以节点拉到哪个候选就直接
  用它启动容器，不核对其 digest 是否与 `image` 一致（见[排障](../how-to/troubleshooting.md#镜像拉取失败)）。
  实例记录当时的镜像副本列表（`image_sources`），下发时按当时的来源规则重算候选。

### GPU 选择器（gpus / unsupported[].gpus）

- `vendor`：必填。使用 `internal/accelerator` 定义的 vendor key
  （`nvidia` / `amd` / `ascend` / `ppu` / `hygon`），与节点上报归一化结果匹配；
  写未知值不会报错但永远匹配不到硬件。
- `cc`：可选，四种合法形式——精确（`8.0`）、`>=8.0`、`<9.0`、闭区间 `8.0..9.0`
  （仅 `>=` 与 `<`，没有 `>` / `<=`；区间要求 lo ≤ hi）。
- `name`：可选，正则，匹配时自动加 `(?i)`（大小写不敏感），编写时用
  `(^|[^A-Za-z0-9])...([^A-Za-z0-9]|$)` 这类边界避免 `A100` 误配 `A1000`。

### 显示名

`display_name_zh_cn` / `display_name_en` 由人写，概括这个档位适用的卡和它与兄弟档位的区别，格式为 `适用卡 · 量化 [· 关键门槛或形态]`：

- **适用卡**与 `gpus` 实际匹配的卡一致：只按算力区间时写架构（`>=8.0` 写「Ampere 及以上」/「Ampere+」，`>=9.0` 写「Hopper 及以上」/「Hopper+」，`>=10.0` 写「Blackwell」）；按型号时列型号，如「H20 / H200」「910C (A3)」「K100 / BW」。不写厂商名。
- **量化**总是写，统一大写：`BF16`、`FP8`、`NVFP4`、`INT8 W8A8`、`INT4 AWQ` 等；同一量化有多个权重发布方时加括号，如「NVFP4 (NVIDIA)」。
- **其余部分**只写区分兄弟档位、或用户必须知道的硬约束，依次是：固定卡数（「8 卡」/「8 GPUs」）、显存门槛、上下文长度、形态（「投机解码」「Engram 卸载」「多机」「PD 分离」、embedding 的任务）、场景（只差场景时写「高吞吐」）、镜像（只差镜像时写引擎或 CUDA 版本，如「CUDA 12.9」）。
- 不写引擎名和模型名。

例如 GLM-5.3-Flash 的两个 vLLM Hopper 档位分别是「H20 / H200 141G · FP8 · 8 卡 · 1M 上下文」和「H100 / H20 96G · FP8 · 8 卡 · 256K 上下文」。

早期模板用的 `display_profile` 与 `gpus.display_target` 已不再使用：导入时仍能读，但会忽略，并给出 `CATALOG_DISPLAY_PROFILE_RETIRED` 警告（见[导入前检查与警告](#导入前检查与警告)）。

### assets[]

| 字段 | 约束 |
|---|---|
| `file` | 必须位于本模型自己的 `assets/` 目录下（相对、clean、反斜杠归一为 `/`） |
| `sha256` | 必填，64 位小写十六进制 |
| `size_bytes` | 非负 |
| `unpack_to` | 容器绝对路径（clean、非 `/`） |

镜像、权重、框架库、venv 不能进 assets——边界与原因见包文档。取值为文件的
`--*-plugin` 类参数必须能由 spec 自身（assets / components / pre_start /
entrypoint 文本）或绝对路径供给，否则校验报稳定错误码
`CATALOG_FILE_VALUED_ARG_UNRESOLVED`（该规则的边界与动机详见包文档）。

### PD 版式与手工命令

PD 配方往往不只是「两个角色各一份参数」：角色的 TP、每个实例跨几台节点都是配方的一部分，有的配方两个角色的引擎参数还不同。
`pd_roles` 与 `manual_command` 让 spec 把这些写下来，部署表单据此打开时已经填好。

**`pd_roles`** 只描述起点，部署仍可改实例数、选节点：

- 每个角色一项：`tp`、`pp`（流水线并行度）、`nodes`（一个实例占几台节点，即 leader 加 follower 的总数）、`units`（初始实例数，≤ 32）。
- 校验：模式的每个角色都要写，不能写模式没有的角色；`tp` ≥ 1；`tp × pp` 必须能被 `nodes` 整除；还要满足模式的 TP 与 PP 规则。
  `sglang-mooncake` 不比较两个角色的 TP（SGLang 按注意力 TP 配对，平台看不到它，宽度不等的配对由引擎处理，配不上时 decode 实例启动失败），
  只要求 decode 的 PP 等于 prefill 的 PP 或为 1（`PD_PP_DECODE_MISMATCH`）。
- 推理模板列表接口（`GET /api/v1/inference-templates`）在 spec 上返回 `pd_roles`，缺省值已补全：每个角色给出 `tp`、`pp`、`nodes`、`gpus_per_node`、`units`。
- 部署表单切到 PD 分离并选该 spec 的模式，按版式填各角色的 TP、PP 与实例行。自动放置不接受跨节点的实例，所以实例行是手动放置、follower 槽位留空，由你选节点。

**`manual_command`** 是 spec 自己拥有整条启动命令：

- 请求的 `container_overrides` 既没写 `cmd_display` 也没写 `command_mode` 时，部署的手工命令取这段脚本（`command_mode` 为 `manual`）；写了任一项则按请求，
  所以部署者仍可改写它，或写 `command_mode: engine` 回到平台组装的命令。部署保存的是创建时的副本，之后目录改了不影响它。
- 评估接口把它作为 `manual_command_draft` 返回，命令编辑框从它开始。
- 导入时校验：是 shell 脚本而非以 `[` 开头的 JSON argv，≤ 64 KiB；PD spec 的脚本必须引用 `${GPUWORKS_PD_ROLE}`，有角色跨多台节点时
  还要引用 `${GPUWORKS_NODE_RANK}` 或 `${GPUWORKS_HEADLESS}`。创建部署时再按这次请求的版式检查一次（请求可以比 spec 的版式跨更多节点），
  不满足返回 `MANUAL_COMMAND_RANK_BLIND`。
- 变量展开与系统层环境变量的规则与手写手工命令相同，见[多机与 PD 的手工命令](../explanation/launch-config.md#多机与-pd-的手工命令)：
  只展开带 `GPUWORKS_` 前缀、本次启动给出的名字；rank 仍带系统层环境变量（跨多台节点时有 `NCCL_*`、`GLOO_SOCKET_IFNAME` 等，单机实例没有这些网络变量），
  脚本里可以直接按 shell 变量读，例如 `"$NCCL_IB_HCA"`。本次启动没给出的名字（decode 实例上的 `${GPUWORKS_PD_SIDE_CHANNEL_PORT}`、
  单机角色上的 `${GPUWORKS_MASTER_ADDR}`）原样留给 shell，所以只写在用得上它的分支里。

下面是一个示意（省略 `image`、`weights`、`gpus` 等其他必填字段）：两个角色都跨 2 台节点，TP 不同，引擎参数也不同。

```yaml
- name: pd-mooncake
  engine: sglang
  pd_mode: sglang-mooncake
  pd_roles:
    prefill: {tp: 8, nodes: 2, units: 1}
    decode:  {tp: 16, nodes: 2, units: 2}
  manual_command: |
    set -eu
    : "${NCCL_IB_HCA:?needs a multi-node instance on a host RDMA network}"   # 命令替换会吞掉 set -u，先检查
    IB_DEVICES=$(printf '%s\n' "$NCCL_IB_HCA" | tr ',' '\n' | sed 's/:.*//' | awk 'NF && !seen[$0]++' | paste -sd, -)
    case "${GPUWORKS_PD_ROLE}" in
      prefill)
        exec python3 -m sglang.launch_server --model-path ${GPUWORKS_MODEL_PATH} \
          --served-model-name ${GPUWORKS_SERVED_MODEL_NAME} \
          --host 0.0.0.0 --port ${GPUWORKS_API_PORT} --tp-size 8 \
          --nnodes ${GPUWORKS_NNODES} --node-rank ${GPUWORKS_NODE_RANK} \
          --dist-init-addr ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} \
          --disaggregation-mode prefill --disaggregation-transfer-backend mooncake \
          --disaggregation-bootstrap-port ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} \
          --disaggregation-ib-device "$IB_DEVICES"
        ;;
      decode)
        exec python3 -m sglang.launch_server --model-path ${GPUWORKS_MODEL_PATH} \
          --served-model-name ${GPUWORKS_SERVED_MODEL_NAME} \
          --host 0.0.0.0 --port ${GPUWORKS_API_PORT} --tp-size 16 \
          --nnodes ${GPUWORKS_NNODES} --node-rank ${GPUWORKS_NODE_RANK} \
          --dist-init-addr ${GPUWORKS_MASTER_ADDR}:${GPUWORKS_MASTER_PORT} \
          --disaggregation-mode decode --disaggregation-transfer-backend mooncake \
          --disaggregation-ib-device "$IB_DEVICES"
        ;;
    esac
```

**服务名的写法**：服务名是用户填的任意文本，手工命令里 `--served-model-name` 的值在展开后会被平台改写成已转义的单引号字符串。
所以把 `${GPUWORKS_SERVED_MODEL_NAME}` 直接写在命令行上、不加引号，如上例。不要把它连同 `--served-model-name` 放进一个双引号字符串
（例如 `bash -c "... --served-model-name ${GPUWORKS_SERVED_MODEL_NAME}"`）：平台插入的单引号会作为字面字符留在服务名里，或让外层引号错位。

## 模板与平台的分工

模板只声明自己要什么，平台在每次启动时提供；下表列出模板能声明或能依赖的全部容器内事实。表外的事实（选哪张卡、哪个端口、哪条网络）由平台决定，模板不能声明。

| 类别 | 模板写 | 容器里看到 | 主机侧由谁定 | 生命周期 |
|---|---|---|---|---|
| 启动变量 | 在 `args` / `env` / `entrypoint` / `pre_start_*` 里引用 `${GPUWORKS_*}`（多机 rank 的 `${GPUWORKS_PEERS_FILE}` 也在其中），见[变量展开](#变量展开)；权重 `subfolder` 只能引用 `${GPUWORKS_NODE_RANK}` | 本次启动的值 | 平台 | 每次启动 |
| 指标端口 | `api.metrics.port: separate`，并在启动字段里引用 `${GPUWORKS_METRICS_PORT}`，见 [api.metrics](#apimetrics引擎指标映射) | launcher 监听的本机端口（`${GPUWORKS_METRICS_PORT}`），不由模板指定数值 | 平台（与 API 端口一起保留） | 实例创建时定下，随实例保留 |
| 系统层环境变量 | 不用写；`env` 里同名键覆盖它 | 计算网络的 `NCCL_*` / `GLOO_SOCKET_IFNAME` / `UCX_NET_DEVICES`，见[环境变量分层](deployment-launch-sources.md#环境变量分层)；多机 rank 有 peers 文件时还有 `GPUWORKS_PEERS_FILE`（与同名启动变量同值，给配方启动的程序直接读） | 平台 | 每次启动 |
| 主权重 | `weights`；`subfolder` 可写 `rank${GPUWORKS_NODE_RANK}`，见[按 rank 拆分的权重](#按-rank-拆分的权重) | 只读，在 `${GPUWORKS_MODEL_PATH}`（缺省 `/model`，有 primary 组件时为它的 `mount_path`）；按 rank 拆分时每个 rank 只有自己的子目录 | 平台（模型存储） | 随模型 |
| 组件权重 | `components[].mount_path`；`subfolder` 同样可写 `rank${GPUWORKS_NODE_RANK}` | 只读，在该路径；按 rank 拆分时每个 rank 只有自己的子目录 | 平台（模型存储） | 随模型 |
| 资产 | `assets[].unpack_to` | 只读，展开到该路径 | 平台（模板包） | 随模板版本 |
| 可写目录 | `dirs[]`（`name`、`mount_path`、`limit_bytes`、`retain`），见 [dirs](#dirs托管的可写目录) | 读写，在 `mount_path`；每个节点一份 | 平台（节点托管目录，需 agent 能力 `mount.managed_dirs`） | 随部署，从不修剪 |
| KV 磁盘层 | 不能写（部署参数） | 读写，平台选的路径 | 平台（节点托管目录） | 随节点上的实例，超限修剪 |
| 部署挂载 | 不能写（部署的 `extra_mounts`） | 按行设置 | 运维 | 不由平台管理 |
| 计算网络文件 | 不用写 | 只读，加速卡网络需要的主机文件（同路径） | 平台 | 每次启动 |
| 平台文件 | 不能写；`/gpuworks` 归平台 | `/gpuworks/pre-start.sh`（有 `pre_start_*` 时）；`/gpuworks/peers.json`（多机 rank，只读，`proto.PeersFile` v1，见[多机 peers 文件](deployment-launch-sources.md#多机-peers-文件gpuworks_peers_file)） | 平台（peers 文件需 agent 能力 `launch.peers_file`） | 每次容器启动重写 |
| 启动等待上限 | `start_timeout_sec` | 引擎健康检查的等待时长 | 平台，见[启动等待上限](deployment-launch-sources.md#启动等待上限) | 实例创建时定下 |

规则：

- **一种变量写法**：文档与示例一律写带前缀的 `${GPUWORKS_X}`；不带前缀的旧写法在模板字段里作为别名同样展开，手写手工命令只认带前缀的。
- **一套单位**：spec 里的大小一律是字节（`*_bytes`），时长一律是秒（`*_sec`），容器路径一律是绝对、规整的路径（`mount_path`、`unpack_to`）。
- **一个容器路径空间**：表中所有挂载（主权重、组件、资产、dirs、KV 磁盘层、部署挂载、计算网络文件）与平台目录 `/gpuworks` 两两不能指向同一路径或互相嵌套，规则只有一条（`proto.ContainerPathsOverlap`）。模板自己的路径（主模型挂载点、组件 `mount_path`、资产 `unpack_to`、dir 的 `mount_path`）在导入时互查，冲突报 `CATALOG_MOUNT_PATH_CONFLICT`（参数带两边的字段与路径；多个资产可以展开到同一目录）；连同部署自己的挂载与平台的挂载，在部署评估、创建与每次启动时再查一遍，同一目标报 `MOUNT_TARGET_DUPLICATE`，嵌套报 `MOUNT_TARGET_NESTED`，落在 `/gpuworks` 上报 `MOUNT_TARGET_RESERVED`（部署按原样重启时保留创建时已有的挂载，除非它会盖住这次的 peers 文件）。
- **agent 能力**：只有节点上的 agent 能提供的设施各有一个能力名；用到它的启动只放到宣告该能力的节点上，旧 agent 的节点以调度原因 `agent_capability_missing`（参数 `capability`、`node`）拒绝，部署等待升级或改选节点，而不是起一个缺了模板声明的容器。声明 `dirs` 需要 `mount.managed_dirs`；在启动字段里引用 `${GPUWORKS_PEERS_FILE}` 需要 `launch.peers_file`，没有引用它的多机启动在旧 agent 上照常启动、只是没有 peers 文件与对应的环境变量。

## entrypoint：入口覆盖的唯一声明处

**入口形式只有两个产地**：

1. **engine builder 默认形式**（`internal/server/deploy`）：vLLM 组装
   `vllm serve <model> ...`，SGLang 组装 `python -m sglang.launch_server ...`；
2. **spec 的 `entrypoint` 覆盖**：部署时经 `WithEntrypoint` **替换可执行部分、
   保留完整参数向量**——builder 注入的模型路径、拓扑、API 地址等参数全部保留，
   只有开头的可执行 token 被换掉。

在此基础上用户侧还有容器级覆盖（部署表单的 entrypoint/command 编辑、手动命令
模式），优先级更高。agent 按服务端下发的入口执行，**不做任何入口推断**——入口选择只发生在
server 组装期，不接受运行时对 checkpoint 布局的探测改写（该方案曾作为评审备选，
2026-09-15 否决）；手动部署（不经目录 spec）不提供非默认入口，需要非默认入口的
模型应进目录声明 `entrypoint`。

写法约定：

- **只有 diffusion（diffusers pipeline）类 spec 必须声明 `entrypoint: [sglang, serve]`**。
  diffusion 的分发逻辑只存在于 `sglang serve` CLI（`sglang/cli/serve.py`），legacy
  模块永远到不了。活样板：`wan-ai/wan2.1-t2v-1.3b-diffusers`（A100 端到端验证过）。
- **ASR / VLM 这类多模态不需要覆盖**：它们走 sglang 主 LLM server 的内建扩展，
  legacy 入口可达，且接受主 ServerArgs 的全部参数（实测 Qwen3-ASR-1.7B 经
  `python -m sglang.launch_server` 正常加载 `Qwen3ASRForConditionalGeneration`，
  `--enable-metrics`、`--served-model-name` 均被接受，`/health`、`/metrics` 200）。
  目录里 `qwen3-asr-1.7b` 的 sglang spec 不声明 entrypoint 是正确的。
- **vLLM 不需要覆盖**：`vllm serve` 是统一入口，内置目录里所有 vLLM
  video/image/audio spec 均未声明 entrypoint。
- 厂商构建镜像入口不同时逐 spec 声明。
- 覆盖只换入口不换参数。diffusion 的参数解析器不认 `--mem-fraction-static`、
  `--context-length` 等 LLM 参数，`--enable-cache-report` 要 0.5.18、`--enable-metrics`
  要 0.5.21 起才认（0.5.18 之前也不认 `--served-model-name`）；`--model-type diffusion`
  或不写 `--model-type` 的 spec 上，平台按镜像版本不注入这些参数的默认值（规则见
  [启动配置](../explanation/launch-config.md)；部署别名为空时不注入 served-model-name），
  spec 自带的 args 也必须落在目标 server 解析器接受的词表内。entrypoint 解决不了
  参数冲突。

## placement：Starlark 拓扑推导

```yaml
placement:
  language: starlark        # 唯一合法语言
  resolver: |
    def resolve(gpu):
        if gpu.min_memory_gib < 32:
            return reject("each GPU requires at least 32 GiB")
        return accept(gpu_memory_mib_per_device=32768,
                      variables={"GPU_COUNT": gpu.count, "SP_DEGREE": 1})
```

- 必须定义 `resolve(gpu)`。`gpu` 是结构体：`count`、`vendor`、`names`、
  `memories_gib`、`min_memory_gib`、`indexes`、`automatic`、`nnodes`。
- `gpu` 描述的是**一个节点**分给本实例的卡。多机实例按 rank 逐节点调用 resolver，每次只看到
  该节点的卡：两个节点各出一张卡时，每个 rank 的 `gpu.count` 都是 1。`gpu.nnodes` 是整个实例
  跨的节点数（单机为 1）。约束针对整个实例的档位要一并判断它，例如只能单卡运行的档位写
  `if gpu.count != 1 or gpu.nnodes != 1: return reject(...)`，只写 `gpu.count != 1` 拦不住跨节点的多卡。
- 预声明内建只有两个：`accept(gpu_memory_mib_per_device, variables?, prefer?)` 与
  `reject(reason)`。`gpu_memory_mib_per_device` 是**准入要求**（1..16777216 MiB，
  上限只为防 int64 溢出），不是分配请求。
- `prefer` 是可选整数（默认 0，范围 -1000000..1000000，超出或非整数直接报错），表达
  "多个 GPU 数量都能接受时更想要哪一个"。自动放置对每个候选 GPU 数（1、2、4、8…）各调用一次
  resolver，在**被接受**的候选里先比 `prefer`、取最大者；`prefer` 相同才按原有打分
  （拓扑、装箱、贴合度）决定。没有任何 resolver 传 `prefer`（全为 0）时，自动放置就是
  "能放下的最少 GPU 数"。例如接受 4 或 8 卡的 resolver 写
  `accept(..., prefer=gpu.count)`，空闲 8 卡节点上拿 8 卡，只剩 4 张空闲卡时仍拿 4 卡。
  `prefer` 只影响自动选卡数：用户指定了并行度或 GPU 序号时候选数已被钉死，`prefer`
  不改变它；非 0 的 `prefer` 会在调度解释里以 `placement_preference` 原因出现。
  `prefer=gpu.count` 属于允许的有界算术（`gpu.count`、`gpu.nnodes`、`gpu.min_memory_gib` 及字面量）。
- 求值是确定性、零 I/O 的：禁 `load`/import，`print` 为空操作；限额：源码
  ≤ 32 KiB、≤ 100k 步、≤ 30s 墙钟（只是兜底，确定性上限靠步数）、结果变量 ≤ 64 个 /
  字符串总量 ≤ 4096 字节。
- resolver 返回的变量进入 `${VAR}` 展开上下文；变量名必须匹配
  `^[A-Z][A-Z0-9_]*$`。

只靠张量并行、没有专家/数据/流水线并行的文本档位，resolver 不写死卡数，而是写显存条件：卡数 × （每卡显存 × 引擎显存比例 − 约 2 GiB 运行开销）不小于「权重 + 启动所需 KV cache」。vLLM 启动时要放下一个最大长度请求的 KV cache，按 `--max-model-len`（没写时用模型的 `context_length`）乘 `kv_bytes_per_token` 计算；SGLang 只要求有 KV 池，按 4 GiB 计。满足条件的卡数里，调度器取最少的，并保证张量并行度能整除注意力头数。厂商实测过、只在特定卡数上通过的档位，仍然写死卡数并注明依据。

### 变量展开

`args` / `env` / `entrypoint` / `pre_start_*` 中的变量引用在每次部署时展开，与有没有 placement
无关。规范写法带 `GPUWORKS_` 前缀，如 `${GPUWORKS_NODE_RANK}`；不带前缀的旧写法（`${NODE_RANK}`）在这些模板字段里
作为别名同样展开。可用变量 = resolver 返回值 + 平台为这次启动给出的事实（唯一产地
`internal/server/placement/expand.go` 的词表与 `internal/server/deploy/launch_variables.go`），下表按带前缀的写法列出（平台给的环境变量与容器内文件不是启动变量，见[模板与平台的分工](#模板与平台的分工)）：

| 变量 | 值 | 何时存在 |
|---|---|---|
| resolver 返回的任意变量 | placement 结果（resolver 返回 `SP_DEGREE`，引用写 `${GPUWORKS_SP_DEGREE}`） | 有 resolver 时 |
| `GPUWORKS_GPU_COUNT` | 本节点分配到的 GPU 数（resolver 返回的同名值会被覆盖） | 放置已确定 |
| `GPUWORKS_TP_SIZE` | 整个部署的张量并行度；resolver 返回则以它为准 | 放置已确定 |
| `GPUWORKS_PP_SIZE` | **故意无默认**：多机流水线并行已有唯一 owner（引擎按节点数注入），只能由 resolver 返回 | 仅 resolver 返回时 |
| `GPUWORKS_MODEL_PATH` | 主权重在容器里的挂载路径（含 primary 组件或部署覆盖） | 总是 |
| `GPUWORKS_SERVED_MODEL_NAME` | 部署的服务名，即网关与健康检查发给引擎的 `model` | 总是 |
| `GPUWORKS_API_HOST` | 服务应绑定的地址：callable 为 `0.0.0.0`，其他引擎 `127.0.0.1` | 总是 |
| `GPUWORKS_API_PORT` | 平台为本实例保留、网关与健康检查会访问的端口；headless rank 为 `0` | 总是 |
| `GPUWORKS_METRICS_PORT` | 平台为本实例额外保留的指标端口，launcher 在这个端口上导出引擎指标，agent 抓取它（见 [api.metrics](#apimetrics引擎指标映射)）；headless rank 为 `0` | 仅模板声明了 `api.metrics.port: separate` |
| `GPUWORKS_NNODES` / `GPUWORKS_NODE_RANK` | 节点数与本 rank 序号；单机为 `1` / `0` | 总是 |
| `GPUWORKS_HEADLESS` | 本 rank 不对外服务时为 `1`，否则 `0` | 总是 |
| `GPUWORKS_NODE_ADDR` | 本 rank 所在节点的可路由地址（不是 rank 0 的） | 仅多机 |
| `GPUWORKS_MASTER_ADDR` / `GPUWORKS_MASTER_PORT` | 多机 rank 0 的地址与 rendezvous 端口 | 仅多机 |
| `GPUWORKS_PD_ROLE` | 本实例的 PD 角色：`prefill` 或 `decode` | PD 实例 |
| `GPUWORKS_PD_SIDE_CHANNEL_HOST` | 本实例 KV side channel 的监听地址 | PD 实例且地址已知 |
| `GPUWORKS_PD_SIDE_CHANNEL_PORT` | 本 rank 额外端口段的首端口，即 KV side channel 端口；多机实例的各 rank 各有各的 | PD 实例且端口已分配 |
| `GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT` | 本实例 leader 的 side channel 端口：leader 上就是自己的，follower 上是 leader 的。follower 要连 leader 的 side channel（如 SGLang 的 bootstrap 服务）时用它；平台自己组装的命令不用 | PD 实例且端口已分配 |
| `GPUWORKS_PEERS_FILE` | 本 rank 的 peers 文件路径 `/gpuworks/peers.json`，列出全组 rank 与 rail，见[多机 peers 文件](deployment-launch-sources.md#多机-peers-文件gpuworks_peers_file) | 仅多机；引用它的启动只放到宣告 `launch.peers_file` 的节点上 |

规则：

- **平台事实不能由 resolver 返回**：`MODEL_PATH`、`SERVED_MODEL_NAME`、`API_*`、`METRICS_PORT`、`NNODES`、`NODE_RANK`、`HEADLESS`、`NODE_ADDR`、`MASTER_*`、`PEERS_FILE`、`PD_*`
  出现在 resolver 结果里直接报错——resolver 运行时还没有节点、端口和 rank。
- **缺变量即失败**：args / env / entrypoint / `pre_start_argv` 里未解析的名字在部署时报错；没有 resolver 的
  spec 引用词表外的名字（如拼错的 `${NODE_RNAK}`、无 resolver 的 `${PP_SIZE}`）在导入时就被拒绝。单机部署
  引用 `${MASTER_ADDR}` 同样失败，而不是拿到空串。
- **`pre_start_script` 是 shell 脚本**：词表外的名字（`${HOME}`、`${PATH}`）原样留给 shell；词表内但本次
  启动没有的名字仍然报错，因为 shell 会把它静默展开成空串。
- **服务名**：网关只对 `vllm` / `sglang` 把请求里的 `model` 改写成引擎自己的名字；`callable` 收到的就是部署的服务名，
  所以会校验模型名的引擎（如 callable 里跑的 vLLM）要写 `--served-model-name ${SERVED_MODEL_NAME}`，写死一个名字会让
  所有请求和健康检查 404。值是用户填的任意文本，放进 shell 字符串（`bash -c`、`pre_start_script`）时要加引号。
- **预览**：放置未确定时 `GPU_COUNT`、`TP_SIZE` 与 resolver 变量保留为 `$NAME` 符号，其余平台事实照常展开
  （端口为预览固定的 `30000`）。
- 附加组件（如 DFlash draft）的挂载路径由 spec 自己的 `components[].mount_path` 决定，直接写字面路径即可，
  不需要变量；校验要求该路径出现在启动字段里。
- 用户在部署表单输入的参数与 `engine` / `image-entry` 模式的命令覆盖**从不展开**。`manual` 模式的手写命令只展开
  带前缀的名字，不带前缀的 `${MODEL_PATH}` 与本次启动没有给出的前缀名都原样留给 shell，见
  [多机与 PD 的手工命令](../explanation/launch-config.md#多机与-pd-的手工命令)。

callable 档位的最小写法：

```yaml
engine: callable
entrypoint: [python3, /opt/gpuworks/runtime/gpuworks_serve.py]
args: [--model, ${MODEL_PATH}, --host, ${API_HOST}, --port, ${API_PORT}, --adapter, transformers_adapter]
```

### 门槛与调度准入的关系

placement 的显存门槛和调度准入需要分别通过。声明 `min_memory_gib` 前，应先计算调度器的每卡
准入要求 = `weight_bytes/tp` + 编码器运行时（多模态塔，`pkg/estimate.MMEncoderRuntimeBytes`）
+ `reserve`；**KV 刻意排除在准入外**，只作信息展示。`reserve` 是引擎固定成本、与模型大小
无关，按架构与 CUDA graph 档位查 `internal/server/estimate/reserve.go`（Ampere piecewise
档 1330 MiB、Hopper full-graph 档 4300 MiB，另加按利用率比例的 driver shortfall
768 MiB；turing..maxwell 一律按 piecewise）。据此 1B 级 bf16 模型准入约 4 GiB，
`min_memory_gib < 6` 拒绝即可放行 8G 卡；凭直觉抬高门槛只会挡住能跑的硬件。

`--gpu-memory-utilization` 一类预算参数是 vLLM 的**池填充策略**（KV pool 填到
util×显存），不是显存需求；小卡上它决定 warmup 余量——vLLM 先把预算吃满再做
grammar/多模态 warmup，余量不足即 OOM。因此显存分档优先用 args 表达而不是拆 spec：一组
`--gpu-memory-utilization 0.85` + `--limit-mm-per-prompt '{"image":1}'` 可以让同一个 spec
覆盖 8G→H100（0.85 在大卡上只是少留一点 KV 池，对 KV 需求小的模型无影响；
`--limit-mm-per-prompt` 与 `--max-model-len` 都能压低启动期多模态 profiling 峰值——后者
兼做单 prompt image token 数的上限）。注意 spec 的 `--gpu-memory-utilization` 与部署
表单的 GPU 利用率（planning 的 GPUMemUtil，默认 0.9）是两个独立旋钮，互不同步。

### 引擎实际显存低于估算时（offload、加载时量化）

每卡准入要求默认按已登记模型的**磁盘权重大小**估算。有些引擎实际用得远比这少：把权重卸载到主机内存（offload），或加载时量化（例如 bf16 检查点配 vLLM-Omni `--quantization fp8`，实际峰值明显低于权重大小）。35 GB 的检查点在 24 GB 的卡上，估算要 37 GB 左右，创建部署就会被 400 `DEPLOYMENT_NEVER_FITS`（原因 `no_fit`）拒绝，部署表单的节点列表也把节点标为不可用，而引擎其实跑得起来。

这时模板可以用 `placement` 声明引擎的**实测每卡峰值**。`accept(gpu_memory_mib_per_device=...)` 返回的值取代按权重得出的估算，作为调度的准入要求；放不下这个值的卡仍被 `reject` 拒绝：

```python
def resolve(gpu):
    if gpu.count != 1 or gpu.min_memory_gib < 22:
        return reject("one GPU with at least 22 GiB")
    return accept(gpu_memory_mib_per_device=22528)
```

- 在 spec 里改 `weight_bytes` 没有用：已登记模型的权重大小优先，声明每卡显存只能经 `placement`。
- 数值取实测的总峰值（权重 + 激活 + CUDA graph 等），不是权重大小；偏低会让部署放进放不下的卡后在引擎启动时 OOM。
- 因估算被拒绝时，服务端在拒绝原因上带参数 `memory_contract_hint: true`（`DEPLOYMENT_NEVER_FITS` 的参数和它的 `reason_params` 里各一份，评测的节点原因里也有），界面据此在原因后补一句提示。只有同时满足三条才带：原因是 `no_fit`；每卡需求是权重估算（模板没有用 `placement` 声明显存）；这个估算连一张空卡也放不下。卡数不够、数据类型不支持、resolver 自己 `reject`，以及卡只是暂时被占，都不带。

## api：对外 HTTP 契约

不声明 `api` 时得到保守默认：仅 `/health` 健康端点、无对外操作。**生成类
（图像/视频/音频）模型必须显式声明操作**——产品类别本身推不出端点。

模型行自己的接口（`operations`，可在「模型」页或 `PATCH /api/v1/models/{id}/inference-config`
编辑）优先于 spec：非空时部署按模型行的接口运行，spec 的 `api.operations` 不覆盖它。
模型行接口为空（从未设置，或为 `[]`——登记时认不出任务，或被人清空）时，用该 spec 部署会把
spec 声明的接口写进模型行；清空接口并不能让模型从试验场隐藏，下一次模板部署会重新填上。
spec 也没有声明接口时，模型保持空接口。

```yaml
api:
  health_endpoint: /health     # 绝对路径、clean、无 ?#
  operations:
  - type: openai.video.generate   # 词表见下
    endpoint: /v1/videos          # 缺省用注册表默认值，且必须在该操作允许的路由内
    mode: async                   # 缺省取第一个支持值
    request_encoding: json        # 缺省取第一个支持值
    request_defaults: {...}       # 请求体默认值
    inputs:                       # Playground 表单声明
    - {name: seconds, kind: number}
    - {name: size, kind: select, options: [640x360, 1280x720]}
    - {name: input_reference, kind: image, encoding: data_url}
    example_input:                # 可选：文本主输入（prompt / input / query）的可用示例值
      prompt: "A sunrise over mountains"
```

操作词表（`internal/server/inferenceapi` 唯一产地）：`openai.chat`、
`openai.completions`（`/v1/completions`，原样续写提示词，不套聊天模板）、`openai.embeddings`、`openai.rerank`、`openai.audio.transcribe`、
`openai.audio.speech`、`openai.image.generate`、`openai.image.edit`、`openai.video.generate`、
`openai.realtime`、`systemone.decide`（`/v1/systemone`，System One 决策：共享的 `state` 加一组带类型的问题，回每个问题的概率分布，不生成文本；线格式同 TypeSafe 的 System One API）。同一契约内操作类型不可重复。

特例：
`openai.video.generate` 在 `/v1/videos` 上必须 `async`、在 `/v1/videos/sync`
上必须 `sync`；`openai.audio.speech` 除 `/v1/audio/speech` 外还可声明 `/v1/audio/generate`
（vllm-omni 给 Stable Audio 这类文生音频模型的路由，请求与响应形状相同，它们的
`/v1/audio/speech` 不认片段时长）。`inputs` 的 `name` 不得用保留字 `model`。

`example_input` 给需要特定格式的模型（如 `pfnet/plamo-2-translate` 的标签提示词）提供一个能直接跑通的值：
模板页「调用方法」的 curl / Python 示例和试验场的初始输入用它代替通用示例，按原样保留（多行、结尾换行都不改）。
它只影响示例和初始值，从不改写实际请求；键只能是该操作的文本主输入，值不能为空。示例同时带上 `request_defaults`。

`max_reference_images` 只能用在 `openai.image.edit` 上，声明一次编辑最多接受几张参考图（正整数，缺省为 1；平台不设上限，按引擎实际支持的张数填写）。每张参考图都是 multipart 请求里的一个 `image` 部分，按上传顺序排列；试验场据此允许多选、显示顺序并限制张数。

模板创建部署时，若部署所用的模型还没有自己的操作，模板声明的操作（以及模型级
`input_modalities`）写入该模型，作为它的初始操作；部署页、Playground 及其代理只读模型上的
操作。模型已有的非空操作不会被后续部署覆盖，但登记模型时按仓库任务推断出的、只有类型的操作视为未配置，会被模板声明替换；空列表（包括在模型页显式清空成的 `[]`）会由下一次部署且声明了操作的模板补齐。
预览（evaluate）不写模型。

### api.tokenizer_ref：独立 Tokenizer 来源

权重仓库没有独立 Tokenizer 文件时，可声明经验证的 Tokenizer 仓库，供评测的**自动**来源使用：

```yaml
api:
  tokenizer_ref: ms://Qwen/Qwen3.8-Flash-Next@2741eec155d03a8ce151b993ccce1a7b1e398d6b
  operations:
  - type: openai.chat
```

仅接受 `hf://org/repo@<40位十六进制提交>` 或 `ms://org/repo@<40位十六进制提交>`；不能填任意 URL、文件路径或可变分支。
自动来源仍优先尝试推理服务分词接口和节点已有的词表；缺少节点词表时使用此声明，未声明才使用权重仓库。手动指定的来源不受影响。

新部署将声明保存到快照，已有快照的声明优先。已偏离模板的部署不会自动采用模板声明，包括快照中的声明。旧快照缺少声明时，只有当前模板的模型、档位及来源、引擎、镜像，以及实际权重的提供方、仓库、版本和子目录全部匹配，才采用当前模板补充的声明。旧客户端未传来源且引用为空的托管评测也会采用同一声明；不会按模型名称或 README 猜测 Tokenizer 仓库。

### api.metrics：引擎指标映射

声明引擎的指标端点，以及引擎指标如何映射到平台的[推理指标词表](inference-metrics.md)。写法、查看原始指标的方法与示例见
[为推理模板声明引擎指标](../how-to/declare-engine-metrics.md)。

```yaml
api:
  metrics:
    endpoint: /metrics            # 可选，默认 /metrics；以 / 开头
    port: api                     # 可选：api（默认，端点在 API 端口上）或 separate（平台另给一个端口）
    format: prometheus            # 可选：prometheus（默认，Prometheus 文本）或 json
    inherit: sglang               # 可选：vllm 或 sglang，先套用该引擎的内置映射
    map:                          # 引擎指标族（可带 {k="v"} 选择器）→ 词表短名
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}   # 长写法：换算
```

- **生效映射** = 继承的映射（`inherit` 指定的引擎，或 `vllm` / `sglang` 模板自身引擎的内置映射）逐条叠加 `map`，同一个键以 `map` 为准。
- **`format: json`**：端点返回一个 JSON 文档（如 Strata 的 `/metrics`），`map` 的键写成用点连接的键路径（`totals.output_tokens`），取路径上的数字（布尔值记 1/0，取不到或不是数字则不出该指标）；字符串写成比较 `路径==词` / `路径!=词`（如 `live.state!=idle`），成立记 1、否则 0，只能映射到 gauge。只能映射计数器和 gauge，不能写 `inherit`，也不套用引擎内置映射；文档超过 1 MiB 或不是合法 JSON 时整份不读。
- **`port: separate`**：引擎独占 API 端口、那里没有 `/metrics`（如只有 `/health`、`/v1/models`、`/v1/chat/completions` 的 Rust 前端）时，由镜像里的 launcher 在第二个本机端口上导出指标，不代理请求。声明后平台为每个实例**多保留一个端口**（与 API 端口一起分配，通常紧邻其后），在启动变量 `${GPUWORKS_METRICS_PORT}`（旧写法 `${METRICS_PORT}`）里给出，模板用它告诉 launcher 往哪里监听；agent 改为抓取 `http://127.0.0.1:<该端口><endpoint>`，不再抓 API 端口。只写 `api` 或不写则与过去一样，不保留端口、不产生该变量。
  - **端口是启动的事实**：保留的端口随实例的期望状态存下（实例详情的 `ports` 列表里，API 端口之后），agent 重启或容器重启后沿用同一个端口；它取部署创建时的模板快照，之后改模板的 `port` 不影响已有实例，要换就是新一代实例。`endpoint`、`format`、`map` 仍按[运行中生效](#apimetrics引擎指标映射)取当前声明。
  - **哪个 rank 有**：只有对外服务的 rank 有（单机实例，多机实例的 rank 0 / leader，PD 的每个 leader）；headless rank 与 API 端口一样没有，`${GPUWORKS_METRICS_PORT}` 在其上为 `0`，所以同一份命令可以在所有 rank 上展开。指标只从有端口的那个 rank 抓取。
  - **预览**：部署评估里的命令用占位端口（与 API 端口的占位同理），创建部署时换成实际保留的端口。
  - **两者必须配套**（导入、目录包上传、编辑器保存时检查）：声明了 `separate` 却没有在 `args` / `env` / `entrypoint` / `pre_start_*` / `manual_command` 里引用 `${GPUWORKS_METRICS_PORT}`，报 `CATALOG_METRICS_PORT_UNREFERENCED`（agent 抓的端口上没有服务）；引用了却没有声明，报 `CATALOG_METRICS_PORT_UNDECLARED`（平台不会保留这个端口；resolver 也不能返回同名变量）。`port` 写成别的值（包括具体端口号，端口由平台分配、模板不能指定）报 `CATALOG_METRICS_PORT_INVALID`。
  - 较旧的 agent 不认识该端口，仍抓 API 端口，得到的只是没有引擎指标，不影响部署。
- **不声明时**：`vllm` / `sglang` 用内置映射；`callable` 抓 `/metrics`，按抓到的 `vllm:` / `sglang:` 前缀自动套用对应映射，都没有则不出引擎指标。
- **校验**（导入、目录包上传、编辑器保存）：目标必须是词表短名；直方图用族名映射、不带选择器、不换算；同一已映射直方图族的 `_bucket` / `_sum` / `_count` 行不能再映射；换算系数非负；`inherit` 只能是 `vllm` / `sglang`；`format` 只能是 `prometheus` / `json`，`port` 只能是 `api` / `separate`，`json` 时键必须是键路径（可带 `==词` / `!=词` 比较）、目标不能是直方图、比较不能映射到计数器、不能写 `inherit`。错误码为 `CATALOG_METRICS_*`。
- **运行中生效**：映射取部署所用档位的**当前**声明，不随部署快照冻结；模板保存或内置目录随 server 升级变化后，引用该档位的运行中实例原地更新、不重启（[ADR 0039](../adr/0039-inference-metric-sources.md)）。档位被删除的部署沿用快照里的 `api` 契约。`api.metrics` 不参与部署是否偏离模板的判断。

### api.request_stats：回复里的逐请求统计

声明引擎写在每个回复里的该请求自己的耗时，映射到逐请求统计词表（[`queue_seconds`、`prefill_seconds`、`decode_seconds`](inference-metrics.md#逐请求统计)）。
网关在回复结束时从回复末尾（流式回复的最后一个事件，非流式回复的 JSON 体）读取，写进该请求的记录，监控中心的请求视图据此把已结束的请求拆成排队、Prefill、Decode。

```yaml
api:
  request_stats:                  # 键：从事件顶层对象起的点路径；值：短名，或 {to, scale}
    tensorfold.prefill_s: prefill_seconds
    tensorfold.decode_s: decode_seconds
    timings.prompt_ms: {to: prefill_seconds, scale: 0.001}   # 毫秒换算成秒
```

- **路径**：用点连接的键路径（`timings.prompt_ms`），第一段是回复最后一个事件（或 JSON 体）顶层的键；取到的必须是非负数字，否则该请求不出这一项。
- **生效声明** = 引擎内置声明逐条叠加模板声明，同一路径以模板为准。`vllm` 内置 `metrics.queue_time_ms`、`metrics.time_to_first_token_ms`、`metrics.generation_time_ms`（`scale: 0.001`），需要部署带 `--enable-per-request-metrics`（部署参数「返回逐请求耗时」）；`sglang` 与 `callable` 没有内置声明。
- **内置模板**：TensorFold 档位声明 `tensorfold.prefill_s` / `tensorfold.decode_s`，Strata 档位声明 `timings.prompt_ms` / `timings.predicted_ms`（`scale: 0.001`）。
- **校验**（导入、目录包上传、编辑器保存）：路径必须是键路径，目标必须是逐请求统计短名，换算系数非负，同一路径不能声明两次。错误码为 `CATALOG_REQUEST_STATS_*`。
- **生效时机**：路由表随目录变化重建时按部署所用档位的当前声明解析，不随部署快照冻结，不需要重启实例；取舍见 [ADR 0042](../adr/0042-gateway-request-records.md)。

## components：多权重组合

用于投机解码（draft/verifier）等一个 spec 挂多份权重的场景：

```yaml
components:
- name: target
  role: primary              # 恰好一个
  weights: {...}             # 同 weights 结构
  weight_bytes: 123          # 必填且 > 0
  mount_path: /models/target # 容器绝对路径，唯一且互不嵌套
- name: dspark
  role: speculative_draft    # 或 verifier；二者必须给 model_id 引用目录内模型
  model_id: redhatai/glm-5.2-speculator.dspark
  weights: {...}
  weight_bytes: 45
  mount_path: /models/draft
```

`role: auxiliary` 用于引擎在主模型之外还要读的权重（如 IndexTTS 2.5 的 Wav2Vec2-BERT、
CAMPPlus 与 BigVGAN 声码器）：它不是目录里的独立模型，`model_id` 可省略，省略时和 primary
一样按 spec 所属模型解析。

组件的权重来源同样可以在 `subfolder` 里写 rank 变量，让多机部署的每个 rank 只下载、只挂载自己那份，见[按 rank 拆分的权重](#按-rank-拆分的权重)。

非 primary 组件的 `mount_path` 必须被启动契约文本（args / entrypoint /
pre_start / env）引用，否则校验失败——挂了不用或拼错路径在导入时暴露。
被引用为组件的模型自身可以没有 spec（draft 权重单独成行、不可独立部署）。

`model_id` 引用的模型要么在同一份文档/目录包里，要么已经在 server 上。不同导入形式的处理如下：

- **导入 YAML，包括模板编辑器保存**：server 把已有的组件模型并入校验，但只写入文档自己的模型。
  模板编辑器里的 YAML 由 server 用同一套目录类型写出，只含当前模型，不含它引用的组件模型；空字段不写，也不补默认值。
- **克隆或编辑带说明文档、资产的模板**：生成的目录包会把引用的组件模型一并带上，这些条目不含 spec 和文档。
- **上传目录包**：包内仍须自带它引用的组件模型。

## dirs：托管的可写目录

引擎要一块能写、能跨实例重建保留、随部署删除而清理的主机目录时（会话库、溢出区、日志，或首次启动时推导出来再复用的编译缓存与快照），在 spec 上声明 `dirs`：

```yaml
dirs:
- name: pico-state
  mount_path: /pico
  limit_bytes: 429496729600
  retain: deployment
```

它与权重、资产、KV 磁盘层、部署挂载的分工见[模板与平台的分工](#模板与平台的分工)。

- **每个节点一份**：部署的实例落到哪个节点，agent 就在该节点的运行时挂载根（`GPUWORKS_RUNTIME_MOUNT_ROOT`，缺省 `<state>/runtime-mounts`，docker 节点为 `<state>`）下建 `managed-dir/<部署>-<创建秒>/<name>`，属主是运行时挂载的属主，以读写方式挂到 `mount_path`。docker、nerdctl、runveil（udocker）、Kubernetes 与主机进程（路径改写到主机目录）都按普通挂载处理。同一节点上同一部署的多个实例共用这份目录；需要各自隔离的配方用已有的 `${GPUWORKS_NODE_RANK}` 自己分子目录。目录路径不另设启动变量，配方直接写 `mount_path`。
- **与 KV 磁盘层同一机制**：KV 磁盘层是同一种托管目录，由部署参数打开，放在 `kv-disk/<部署>` 下；它是缓存，节点上最后一个实例离开就删、超限由清扫器修剪。`dirs` 是状态，保留到部署删除、从不修剪。
- **保留到部署删除**：实例停止、重启、故障转移、多机组重组都不动它，回到同一节点的实例看到上次写下的内容。部署删除后（实例确认拆除、部署行删掉之后），server 通知所有在线节点删除该部署的目录；当时离线的节点在下次连接时由 hello 上报自己持有的目录组，server 的重放点名其中部署已不存在的组，节点再删除。仍被期望实例挂载的组不删。目录组名带部署的创建时间，换了数据库（重装 server、恢复备份）后同号的新部署不会拿到旧目录。
- **准入按声明的大小**：调度时节点存放托管目录的磁盘（agent 以 `managed_dirs` 上报其空闲空间）须放得下全部 `limit_bytes`、KV 磁盘层的上限与磁盘保留空间（存储设置里的保留值，未设时为该盘的 1/10、至多 50 GiB）；agent 报告这块盘与模型目录或当前运行时的镜像目录同盘时，待下载的权重与待解压的镜像也计在里面。不满足时以 `insufficient_managed_dir_disk` 拒绝，原因里带需要、可用、目录、保留各占多少。没有上报这块盘的节点（旧 agent）不做此项检查。准入不扣除目录里已经写下的数据，因为它已体现在空闲空间里。
- **平台不修剪、不限额**：目录存的是状态，不是缓存，平台不按大小删除其中的文件，也不设文件系统配额；写不超过 `limit_bytes` 是配方自己的事。
- **路径冲突**：`mount_path` 与其他所有挂载同在一个容器路径空间，规则见[模板与平台的分工](#模板与平台的分工)。
- **agent 能力**：agent 在 hello 里宣告 `mount.managed_dirs`；声明了 `dirs` 的启动只放到宣告它的节点上，旧 agent 的节点以 `agent_capability_missing` 拒绝。准备目录失败时实例以 `MANAGED_DIR_PREPARE_FAILED` 启动失败。

设计取舍见 [ADR 0077](../adr/0077-template-managed-writable-dirs.md)。

## performance_tradeoffs：降速参数的理由

有些参数能让引擎在某种硬件或模型上跑起来，代价是吞吐或延迟，例如关掉图捕获、关掉前缀缓存、把并发压到个位数。`internal/engines` 里有一张按引擎区分写法的登记表（`engines.ScanPerformanceCosts`），识别 spec 启动参数里的这类参数并分级：

| 代价（cost） | vLLM 写法 | SGLang 写法 | 级别 |
|---|---|---|---|
| `graph_disabled` | `--enforce-eager`、`--compilation-config` 里 `cudagraph_mode: NONE`、`-O0` 或 `mode: 0`（都只在没写 `cudagraph_mode` 时）、`--speculative-config` 里 `enforce_eager: true` | `--disable-cuda-graph` | 必须说明 |
| `prefix_cache_disabled` | `--no-enable-prefix-caching` | `--disable-radix-cache` | 必须说明 |
| `concurrency_capped` | `--max-num-seqs` | `--max-running-requests` | ≤ 4 必须说明；5–8 只提示 |
| `chunked_prefill_disabled` | `--no-enable-chunked-prefill` | `--chunked-prefill-size -1` | 必须说明 |
| `overlap_schedule_disabled` | — | `--disable-overlap-schedule` | 必须说明 |
| `prefill_batch_small` | `--max-num-batched-tokens` < 2048 | `--chunked-prefill-size` < 2048 | 只提示 |
| `kv_memory_reduced` | `--gpu-memory-utilization` < 0.8 | `--mem-fraction-static` < 0.7 | 只提示 |
| `graph_capture_narrow` | `cudagraph_mode: FULL_DECODE_ONLY`、`cudagraph_capture_sizes` 最大值 < 16 | `--cuda-graph-max-bs`、`--cuda-graph-max-bs-decode` < 16，`--cuda-graph-backend-prefill disabled` | 只提示 |
| `kernel_fallback` | `--attention-backend TRITON_ATTN`，环境变量 `VLLM_USE_DEEP_GEMM=0` 等关掉快速内核的开关 | — | 只提示 |

登记表读 `args`、`env`，以及 `entrypoint`、`pre_start_argv`、`manual_command` 里按 shell 引号规则切开的参数：引号里带空格的 JSON 值算一个值，`sh -c '…'` 里的命令逐词读。每个输入单独读，写在两个输入里的 `--compilation-config` 不合并。vLLM 的下划线写法（`--max_num_seqs`）等同连字符写法，报出的 `param` 是连字符形式。`--compilation-config` 的 `backend: eager` 只是 torch.compile 的后端，不关图回放，不算 `graph_disabled`。

spec 用 `performance_tradeoffs` 写明理由，每项对应一个参数：

```yaml
performance_tradeoffs:
- param: --disable-cuda-graph
  reason: hardware_plugin_requires
  detail_en: "NPU graph capture hangs on this runner; the eager path completes chat."
  detail_zh: "该镜像上 NPU 图捕获会挂起；eager 路径能完成对话。"
- param: --speculative-config.enforce_eager
  reason: upstream_bug
  detail_en: "The drafter's graph capture crashes in this engine version."
  detail_zh: "该引擎版本中草稿模型的图捕获会崩溃。"
  link: https://example.com/upstream-issue
```

| 字段 | 约束 |
|---|---|
| `param` | 必填，同一 spec 内不重复；写登记表报出的名字：开关写参数本身（`--enforce-eager`），JSON 值里的键写 `<参数>.<键>`（`--compilation-config.cudagraph_mode`、`--speculative-config.enforce_eager`），环境变量写 `env:<名字>`，`-O0` 写 `-O`。启动参数里已经没有这个降速参数时导入失败——理由不能比参数活得久 |
| `reason` | 必填，取值见下表 |
| `detail_en` / `detail_zh` | 都必填，写清楚快的那条路在哪一步、以什么方式失败；这是模板内容，和显示名一样由作者撰写 |
| `link` | `reason: upstream_bug` 时必填，其余可选；必须是 http(s) URL |

| reason | 含义 |
|---|---|
| `hardware_plugin_requires` | 厂商插件、运行时或驱动不支持更快的路径（图捕获、前缀缓存、某个内核） |
| `model_architecture_requires` | 该引擎版本对这种模型结构不支持更快的路径 |
| `accuracy` | 走更快的路径会输出错误或精度下降 |
| `memory_fit` | 为放进这张卡或这个切片，让出显存或并发 |
| `task_type` | 没有解码阶段的任务（embedding、rerank） |
| `upstream_bug` | 上游已知缺陷，必须给 `link` |

`api.operations` 全部是 `openai.embeddings` / `openai.rerank` 的 spec 自动按 `task_type` 处理 `graph_disabled`、`prefix_cache_disabled` 与 `concurrency_capped`，不用写。导入、模板编辑器保存和目录包加载都会校验这个字段；它不写入数据库投影，平台只从内置目录读取这些理由，所以从数据库导出或在编辑器里另存的模板不带它；导入带这个字段的 spec 会收到 `CATALOG_PERFORMANCE_TRADEOFFS_NOT_KEPT` 警告。

内置目录由 `internal/server/inferencecatalog/seed` 的 `TestBuiltinPerformanceCostsAreExplained` 守护：每个内置 spec 里“必须说明”级别的降速参数，要么有 `performance_tradeoffs` 条目，要么属于上述自动豁免，要么列在 `testdata/perf_tradeoff_budget.txt` 里，否则测试失败，失败信息里给出应补的 YAML。预算文件只减不增：某个参数补上理由或被去掉后，必须在同一个改动里删掉对应那一行，否则测试同样失败。行数钉在测试常量 `perfTradeoffBudgetLines` 上，与文件不符就失败：删行时同时调低它；新的降速参数应写 `performance_tradeoffs`，确需新增一行就得调高这个常量并在该行上方写 `# raised: <原因>`，评审在改动里看得到。

## 导入前检查与警告

`POST /api/v1/inference-templates/evaluate` 对同一份请求体（平铺 YAML 或 `.gpuworks-catalog.tar.gz` 目录包，需要管理员）做导入会做的解析与校验，但不写任何东西，返回

```json
{"valid": true, "errors": [], "warnings": [{"code": "CATALOG_CALLABLE_METRICS_UNDECLARED", "params": {"model_id": "acme/adapter", "spec": "serve"}, "error": "…English text…"}]}
```

校验不过时 `valid` 为 `false`，`errors` 里是导入会返回的同一个错误码；没有专属码的解析错误为 `CATALOG_INVALID`（`params.detail` 是解析器原文）。它不检查「同名用户档位已存在」（导入时返回 409），也不去取权重大小。

警告不阻止导入，导入成功的响应带同一份 `warnings`（没有警告时没有该字段）。警告只由 `inferencecatalog.CatalogWarnings` 产生，每条带稳定的 `code`、英文 `error` 与 `params`（`model_id`、`spec`），界面按 `code` 渲染本地化文案：

| 警告码 | 触发条件 | 后果 |
|---|---|---|
| `CATALOG_CALLABLE_METRICS_UNDECLARED` | `engine: callable` 且没有 `api.metrics` | 监控中心没有该档位的引擎指标，除非引擎的 `/metrics` 恰好用 vLLM / SGLang 的指标名（见[为推理模板声明引擎指标](../how-to/declare-engine-metrics.md)） |
| `CATALOG_HEALTH_ENDPOINT_UNDECLARED` | `engine: callable` 且没有 `api.health_endpoint` | 就绪探测用默认的 `/health`；`vllm` / `sglang` 的 `/health` 是引擎自带的，不提示 |
| `CATALOG_ARG_PASSED_THROUGH` | `args` 里某个字典参数的值字典读不出来（`params.flag` / `value` / `key`） | 原样传给引擎；部署表单和显存估算看不到这个值 |
| `CATALOG_DISPLAY_PROFILE_RETIRED` | spec 还写着 `display_profile` 或 `gpus.display_target` | 这两个字段已被忽略，界面显示的是 `display_name_zh_cn` / `display_name_en`（都没有时显示 `name`） |
| `CATALOG_PERFORMANCE_TRADEOFFS_NOT_KEPT` | spec 写了 `performance_tradeoffs` | 已校验但不保留：平台只读取内置目录里的理由（见 [performance_tradeoffs](#performance_tradeoffs降速参数的理由)） |
| `CATALOG_CALLABLE_SERVED_NAME_FIXED` | `engine: callable` 的 `args` / `entrypoint` 里 `--served-model-name` 的值没有用 `${SERVED_MODEL_NAME}`（`params.name` 是写死的值） | 会校验模型名的引擎只认这个写死的名字，部署的服务名与它不同时请求和健康检查都会 404；adapter 不看模型名或作者有意固定时可以忽略 |

## 校验入口与工具

- `ValidateFlatCatalog`：结构校验（逐 spec）+ 跨模型校验（component 引用、
  component-only 模型）。导入、导出、seed 加载都走它。
- `internal/server/inferencecatalog/seed` 的契约测试遍历内置目录，断言权重
  revision、契约、placement 与离线命令组装；`model_config` 相关断言见包文档。
- `cmd/gpuworks-catalog-model-config`：从 spec 声明的权重源回填 `model_config`
  （默认只报告，`-write` 落盘；受限仓库需 `HF_TOKEN`，读不到走
  `fallback-sources.json`）。
- 镜像目录（系统镜像 manifest）是另一个域，格式见
  [镜像目录与推理镜像家族](image-families.md#manifest-格式)。
