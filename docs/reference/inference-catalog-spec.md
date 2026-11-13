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
| `engine` | 必填；部署时按注册表解析，合法值 `vllm` / `sglang` / `callable`（`internal/engines` 唯一产地） | `callable` 承载目录自带 Serve Adapter，无日志方言与参数字典；**命令行完全归模板**，引擎不追加任何参数，模型路径、端口、多机 rank 等平台事实由模板用[变量](#变量展开)自己摆放，且必须引用 `${API_PORT}`（ADR 0036） |
| `image` | 必填 | 镜像引用语法与漂移问题见 [镜像家族](image-families.md) 与 todo.md T2 |
| `image_sources[]` | 可选；每项 trim 后非空、不含空白、不含 `://`、≤ 512 个字符，不重复；与 `image` 相同的项被丢弃（引用的比较方式见下文） | 模板自己声明的镜像来源，在节点启动副本时作为镜像自身副本参与候选，详见 [image_sources](#image_sources模板声明的镜像来源) |
| `weights.quantization` | 必填，小写 | |
| `weights.sources[]` | 至少 1 个 | 见下 |
| `weight_bytes` / `runtime_weight_bytes` | 非负；后者 ≤ 前者 | 调度与估算输入 |
| `shm_bytes` | 可选整数，字节；> 0 且 ≤ 16 TiB（`1<<44`），否则报稳定错误码 `CATALOG_SHM_BYTES_INVALID`；显式写 `0` 等同于不写 | 引擎自己需要的容器 `/dev/shm` 大小，例如把大块查表数据放在共享内存里的引擎。部署不填共享内存大小时，容器得到「引擎基线 + CPU 内存层」与它之间较大的一个；部署自己填的值优先。详见 [容器 `/dev/shm` 大小](deployment-launch-sources.md#devshm-大小) |
| `memlock_unlimited` | 可选布尔，缺省 `false`；写成非布尔值（数字、字符串等）会被拒绝 | 为 `true` 时该部署的容器带 `--ulimit memlock=-1:-1`（只对这个容器取消锁定内存上限），给要锁页大块主机内存的引擎用（例如 DeepInfer 用 `cudaHostRegister` 锁住约 197 GB）。部署自己的 `docker_args` 已写 `memlock` 时以部署为准，不重复追加；与主机 RDMA 网络注入的 `memlock` 合并为一条。详见 [锁定内存](deployment-launch-sources.md#锁定内存memlock) |
| `gpus` | 必填 | 见下 |
| `args` | 字符串数组 | 进入引擎命令行，支持 `${VAR}` [变量展开](#变量展开)；部署时按参数字典拆分（见下） |
| `env` | 键非空 | 容器环境变量，值支持 `${VAR}` 展开 |
| `pre_start_argv` / `pre_start_script` | **二选一**；argv 项非空；script ≤ 64 KiB | 引擎启动前在容器内执行 |
| `assets[]` | 见下 | 随包分发的小文件 |
| `entrypoint` | 项非空 | 见下文专节 |
| `placement` | 见下 | 拓扑推导 |
| `api` | 见下 | 对外 HTTP 契约 |
| `components[]` | 见下 | 多权重组合（投机解码等） |

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
`NGRAM`/`STANDALONE`（其模型 hook 在归一化前就按大写比较），vLLM 是小写的方法名；KV 精度两边
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
- 厂商构建镜像入口不同时逐 spec 声明（例：hygon 构建的
  `qwen3-tts-12hz-1.7b-voicedesign`）。
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
  `memories_gib`、`min_memory_gib`、`indexes`、`automatic`。
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
  `prefer=gpu.count` 属于允许的有界算术（`gpu.count`、`gpu.min_memory_gib` 及字面量）。
- 求值是确定性、零 I/O 的：禁 `load`/import，`print` 为空操作；限额：源码
  ≤ 32 KiB、≤ 100k 步、≤ 30s 墙钟（只是兜底，确定性上限靠步数）、结果变量 ≤ 64 个 /
  字符串总量 ≤ 4096 字节。
- resolver 返回的变量进入 `${VAR}` 展开上下文；变量名必须匹配
  `^[A-Z][A-Z0-9_]*$`。

只靠张量并行、没有专家/数据/流水线并行的文本档位，resolver 不写死卡数，而是写显存条件：卡数 × （每卡显存 × 引擎显存比例 − 约 2 GiB 运行开销）不小于「权重 + 启动所需 KV cache」。vLLM 启动时要放下一个最大长度请求的 KV cache，按 `--max-model-len`（没写时用模型的 `context_length`）乘 `kv_bytes_per_token` 计算；SGLang 只要求有 KV 池，按 4 GiB 计。满足条件的卡数里，调度器取最少的，并保证张量并行度能整除注意力头数。厂商实测过、只在特定卡数上通过的档位，仍然写死卡数并注明依据。

### 变量展开

`args` / `env` / `entrypoint` / `pre_start_*` 中的 `${NAME}` 在每次部署时展开，与有没有 placement
无关。可用变量 = resolver 返回值 + 平台为这次启动给出的事实（唯一产地
`internal/server/placement/expand.go` 的词表与 `internal/server/deploy/launch_variables.go`）：

| 变量 | 值 | 何时存在 |
|---|---|---|
| resolver 返回的任意变量 | placement 结果（如 `SP_DEGREE`） | 有 resolver 时 |
| `GPU_COUNT` | 本节点分配到的 GPU 数（resolver 返回的同名值会被覆盖） | 放置已确定 |
| `TP_SIZE` | 整个部署的张量并行度；resolver 返回则以它为准 | 放置已确定 |
| `PP_SIZE` | **故意无默认**：多机流水线并行已有唯一 owner（引擎按节点数注入），只能由 resolver 返回 | 仅 resolver 返回时 |
| `MODEL_PATH` | 主权重在容器里的挂载路径（含 primary 组件或部署覆盖） | 总是 |
| `SERVED_MODEL_NAME` | 部署的服务名，即网关与健康检查发给引擎的 `model` | 总是 |
| `API_HOST` | 服务应绑定的地址：callable 为 `0.0.0.0`，其他引擎 `127.0.0.1` | 总是 |
| `API_PORT` | 平台为本实例保留、网关与健康检查会访问的端口；headless rank 为 `0` | 总是 |
| `NNODES` / `NODE_RANK` | 节点数与本 rank 序号；单机为 `1` / `0` | 总是 |
| `HEADLESS` | 本 rank 不对外服务时为 `1`，否则 `0` | 总是 |
| `NODE_ADDR` | 本 rank 所在节点的可路由地址（不是 rank 0 的） | 仅多机 |
| `MASTER_ADDR` / `MASTER_PORT` | 多机 rank 0 的地址与 rendezvous 端口 | 仅多机 |

规则：

- **平台事实不能由 resolver 返回**：`MODEL_PATH`、`SERVED_MODEL_NAME`、`API_*`、`NNODES`、`NODE_RANK`、`HEADLESS`、`NODE_ADDR`、`MASTER_*`
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
- 用户在部署表单输入的参数与命令覆盖**从不展开**。

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
`openai.realtime`。同一契约内操作类型不可重复。

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
    format: prometheus            # 可选：prometheus（默认，Prometheus 文本）或 json
    inherit: sglang               # 可选：vllm 或 sglang，先套用该引擎的内置映射
    map:                          # 引擎指标族（可带 {k="v"} 选择器）→ 词表短名
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}   # 长写法：换算
```

- **生效映射** = 继承的映射（`inherit` 指定的引擎，或 `vllm` / `sglang` 模板自身引擎的内置映射）逐条叠加 `map`，同一个键以 `map` 为准。
- **`format: json`**：端点返回一个 JSON 文档（如 Strata 的 `/metrics`），`map` 的键写成用点连接的键路径（`totals.output_tokens`），取路径上的数字（布尔值记 1/0，取不到或不是数字则不出该指标）；字符串写成比较 `路径==词` / `路径!=词`（如 `live.state!=idle`），成立记 1、否则 0，只能映射到 gauge。只能映射计数器和 gauge，不能写 `inherit`，也不套用引擎内置映射；文档超过 1 MiB 或不是合法 JSON 时整份不读。
- **不声明时**：`vllm` / `sglang` 用内置映射；`callable` 抓 `/metrics`，按抓到的 `vllm:` / `sglang:` 前缀自动套用对应映射，都没有则不出引擎指标。
- **校验**（导入、目录包上传、编辑器保存）：目标必须是词表短名；直方图用族名映射、不带选择器、不换算；同一已映射直方图族的 `_bucket` / `_sum` / `_count` 行不能再映射；换算系数非负；`inherit` 只能是 `vllm` / `sglang`；`format` 只能是 `prometheus` / `json`，`json` 时键必须是键路径（可带 `==词` / `!=词` 比较）、目标不能是直方图、比较不能映射到计数器、不能写 `inherit`。错误码为 `CATALOG_METRICS_*`。
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

非 primary 组件的 `mount_path` 必须被启动契约文本（args / entrypoint /
pre_start / env）引用，否则校验失败——挂了不用或拼错路径在导入时暴露。
被引用为组件的模型自身可以没有 spec（draft 权重单独成行、不可独立部署）。

`model_id` 引用的模型要么在同一份文档/目录包里，要么已经在 server 上。不同导入形式的处理如下：

- **导入 YAML，包括模板编辑器保存**：server 把已有的组件模型并入校验，但只写入文档自己的模型。
  模板编辑器里的 YAML 由 server 用同一套目录类型写出，只含当前模型，不含它引用的组件模型；空字段不写，也不补默认值。
- **克隆或编辑带说明文档、资产的模板**：生成的目录包会把引用的组件模型一并带上，这些条目不含 spec 和文档。
- **上传目录包**：包内仍须自带它引用的组件模型。

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
