# 可执行推理目录包

推理目录包用于分发**模型的部署配置和配套的轻量文件**。一个包可以包含多个模型，每个模型又可以提供多个 spec（档位），对应不同的部署配置。导入后，可以在**推理模板**页面查看并选择这些配置。

“可执行”指包里描述了怎样启动模型：使用什么 GPU、引擎和镜像，从哪里获取权重，以及采用哪些启动参数。镜像、模型权重和运行时由各自的来源交付。

本文说明目录包的结构、导入与覆盖规则、辅助文件的交付方式，以及模型估算信息的维护。编写具体字段时，查阅[推理目录 model.yaml 语法参考](inference-catalog-spec.md)。

## 包里包含什么

基本文件布局如下：

```text
manifest.yaml
models/<model-id>/model.yaml
models/<model-id>/assets/<small-file>
```

| 文件 | 用途 |
|---|---|
| `manifest.yaml` | 记录包格式版本、发布信息及模型、spec 数量 |
| `models/<model-id>/model.yaml` | 保存该模型的元数据和 spec 列表 |
| 模型目录下的 `assets/` | 携带该模型需要的轻量辅助文件 |

新增或修改内置推理模板时，只需改动对应的模型目录。源码描述文件 `internal/server/inferencecatalog/seed/catalog/manifest.yaml` 只保存 `schema_version`。打包时，构建工具从 Go 实际嵌入的文件统计模型数和 spec 数。`published_at` 默认取最近一次修改内置目录的 Git 提交的作者时间，统一写为 UTC。发布流水线生成一次完整清单，服务端和渠道包共用这份清单。

**assets 只用于轻量文件。** 镜像、权重、框架库、venv 和共享运行时不能放入 assets，必须通过镜像、权重源或节点资产交付。

目录包按内容摘要（digest）标识。server 校验包内路径安全、schema（文件结构和字段约束），并计算内容摘要；同一 digest 的包可以复用。缓存损坏后，不能继续以旧 digest 提供服务。

`manifest.yaml` 的 `schema_version` 是包格式版本，当前为 3。server 读取不高于自己的版本、拒绝更高的版本；在发布渠道里看到更高的版本时把目录更新标为不兼容，不去下载。2 起档位带 `display_name_zh_cn` / `display_name_en`；3 标记含 `gpus.arch_id` 与 `tokenizer_ref` 的目录，只认 2 的 server 逐字段严格解析 `model.yaml`，读不了它们。认识 3 的 server 对官方目录包容错读取（见下文「官方目录包里读不懂的内容」），此后 `model.yaml` 新增字段不再提升版本，只有包的文件布局或清单发生变化时才提升。
数据库保存 digest 和从包中提取的结构化记录，包的文件内容保存在包仓库中。

### model.yaml 与 spec 的关系

`model.yaml` 使用扁平 spec 结构：模型元数据在模型一级声明，各套可部署配置分别放在 spec 中。每个 spec 说明以下内容：

| 配置 | 回答的问题 |
|---|---|
| GPU 匹配条件（谓词） | 哪些 GPU 适用这套配置 |
| `engine`、`image`、`weights` | 使用哪个引擎、镜像和权重来源 |
| `args`、`env` | 启动参数和环境变量是什么 |
| `pre_start_argv` 或 `pre_start_script` | 引擎启动前需要执行什么 |
| API contract（接口契约） | 启动后的服务提供哪些接口、如何调用 |

spec 不写 `tp`/`nodes`；部署时根据实际分配的 GPU 推导并行度。
`pre_start_argv` 与 `pre_start_script` 二选一，脚本上限为 64 KiB。

扁平结构和多文件包的设计依据分别见 [ADR 0009](../adr/0009-flat-spec-catalog.md) 与 [ADR 0010](../adr/0010-executable-inference-catalog-package.md)。导入、导出和同步接口见 [API 参考](api.md)。

## 导入与覆盖更新

### 导入入口和文件格式

Web UI 的入口分为两类：

- 自定义模板：**推理模板 → 导入模板**，调用 `POST /api/v1/inference-templates/import`。
- 官方目录包：**设置 → 版本与更新**中的上传入口，调用 `POST /api/v1/inference-templates/official`。

自定义模板的文件选择框接受 `.yaml`、`.yml`、`.json`、`.tar.gz` 和 `.tgz`。调用导入 API 时，请求体直接发送**文件原始字节**，不使用 multipart；覆盖已有模板时添加 `?overwrite=true`。

服务端根据输入开头的 gzip 格式标记（magic）选择解析方式：

| 输入 | 解析方式与文件限制 |
|---|---|
| gzip 数据 | 按目录包解析，可以携带 assets 和 README |
| 非 gzip 数据 | 按裸目录 YAML 解析，不能用这种方式上传 assets 或 README 的文件内容 |

裸 YAML 引用了本地文件、且未指定 `overwrite=true` 时，返回 `CATALOG_PACKAGE_REQUIRED_FOR_FILES`。
指定 `overwrite=true` 后，可以修改既有打包模板的结构化内容并复用已有文件：assets 必须来自对应 user spec 当前的包，README 也必须已经存在；服务端重新组包并校验资产 SHA。新增文件仍需上传目录包。

大小限制分别为：

- 导入 API 请求体：最多 **32 MiB**。
- gzip 目录包：压缩后最多 **16 MiB**，展开后的文件总量最多 **64 MiB**。

成功响应为 `{models, package_digest}`，分别表示导入的模型数和包的内容摘要。

### tar 包的路径与文件要求

以下内容会导致导入返回 HTTP 400：

| 包内问题 | 错误信息 |
|---|---|
| 条目名不是规范化的相对路径，例如带多余的 `./` 前缀 | `unsafe inference catalog package path "./manifest.yaml"` |
| 软链等非普通文件条目 | `is not a regular file` |
| macOS AppleDouble 文件，例如 `._x` | `contains unexpected file` |

目录条目在通过路径校验后会被跳过。不要用 `tar -C pkg .` 打包，它会生成带 `./` 前缀的条目。

在包含 `manifest.yaml` 的包目录中执行下面的命令。用 `-T` 显式列出文件，`COPYFILE_DISABLE=1` 避免 macOS 写入 AppleDouble 文件；输出放在上一级目录，避免再次打包时把旧归档包含进去。

```bash
COPYFILE_DISABLE=1 tar -czf ../pkg.tgz --no-recursion \
  -T <(find . -type f ! -name '._*' | sed 's|^\./||' | sort)
```

zsh 不会对裸变量做单词拆分。`tar ... $FILES` 会把多行文件列表当作一个参数，因此应使用上面的 `-T` 文件列表方式。

### 覆盖导入会保留什么

**覆盖导入只新增或更新，不隐式删除。** 服务端按名称新增或更新 model/spec：

- 已有的 user 来源记录不会因新包未包含它而删除。
- 新包中缺少的旧 spec 仍然可见、可选、可部署。

下线 user spec 必须显式删除，可以使用模板详情页的删除按钮，或调用：

```text
DELETE /api/v1/inference-templates/spec?model_id=…&spec=…
```

仍有部署引用该 spec 时，删除会被拒绝。因此发布方从包中移除 spec 后，还需要安排这一步显式删除。

### 官方目录包里读不懂的内容

官方目录按最新的 server 编写。「检查更新」或上传官方目录包时，旧 server 读不懂的部分被跳过，其余照常应用（设计依据见 [ADR 0065](../adr/0065-official-catalog-read-tolerantly.md)）：

- 某个 spec 解析不了或校验不过：跳过这个 spec。
- 模型自己的字段读不懂：跳过整个模型。引用了它作为组件的 spec 也一并跳过。
- 被跳过的模型或 spec 如果以前已经导入过，保持原样、仍可部署；新增的在这台 server 上暂不可见。
- 响应的 `skipped` 列出每一项的 `model_id`、`spec`（跳过整个模型时省略）和 `reason`，设置页会一直显示这份列表。升级 server 后再更新一次即可读全。

包本身的校验不放宽：文件布局、清单里的模型与 spec 数、保留下来的 spec 的 assets 摘要有任何不符，整包拒绝。这种容错只用于官方目录包；通过「导入模板」导入时，未知字段照旧报错。

## 辅助文件如何交付和使用

### assets 的交付路径

包体包含资产的实际字节，按内容摘要保存在包仓库中。部署时由平台完成以下交付：

1. server 将资产内容和 sha256 校验值写入 `RuntimeAsset`，随实例的 `DesiredState`（期望状态）下发。
2. agent 校验资产，解包到只读缓存。
3. agent 将缓存内容挂载到 `unpack_to` 指定的位置。

节点获取这些包内资产时，不需要访问外部地址。

**优先把辅助文件打成单个 `.tar.gz`，并让 `unpack_to` 指向目录。** 这是为了兼容不同的容器启动器：

- 旧 udocker CLI 启动器只对目录挂载生效，单文件 bind mount 会静默失效，表现为“部署成功但文件不在”。
- runveil 是 `udocker` driver 的默认启动器，直接使用 PRoot 绑定挂载；单文件资产尚未单独验证，因此仍按目录分发。

### 插件参数必须有文件来源

`args` 中的 `--reasoning-parser-plugin`、`--tool-call-parser-plugin` 等 `--*-plugin` 参数需要指向插件文件。静态校验按路径形式处理：

- **绝对路径**：允许通过，但这不证明文件实际存在。
- **相对路径**：按容器工作目录 `/` 解析，必须能从 spec 的 assets、component 挂载、启动前命令/脚本或入口命令中找到来源依据。

例如，曾有 spec 声明 `--reasoning-parser-plugin nano_v3_reasoning_parser.py`，但镜像、权重仓库和 assets 都没有提供该文件，引擎启动时出现 `FileNotFoundError`。
对于 spec 内找不到来源的这类参数，校验返回稳定错误码 `CATALOG_FILE_VALUED_ARG_UNRESOLVED`。

该检查只能拦截静态可判定的“文件没有声明来源”。控制面无法读取镜像内容，文件是否实际存在仍需 L2/L3 探测确认；脚本中出现文件名也不等于已经验证脚本会正确生成文件。

## model_config：模型级的估算事实

`model_config` 在**模型一级**声明，同一模型的各个 spec 共用。它保存该模型 `config.json` 中与部署估算相关的字段，沿用原字段名。字段写法示意：

```yaml
model_config:
  model_type: qwen3
  num_hidden_layers: 64
  hidden_size: 5120
  num_attention_heads: 64
  num_key_value_heads: 8
  head_dim: 128
  max_position_embeddings: 40960
```

### 为什么要随目录提供

目录模板部署时不会注册模型，因此没有本地 `config.json` 可读。部署时再从权重仓库获取，也不能覆盖所有情况：

- 受限（gated）仓库需要凭据，服务端无法直接读取。
- 部分仓库的 `config.json` 没有完整的注意力结构信息，例如 Gemma 3。
- 扩散类模型可能没有 `config.json`。

因此目录随包提供这些模型事实。ModelScope 与 HuggingFace 使用同一份 `config.json` 布局，`model_config` 与选择哪个权重来源无关。

### model_config 与 KV 容量的区别

| 信息 | 用途 |
|---|---|
| `model_config` | 提供注意力结构、上下文长度和编码器估算所需的信息，包括判断张量并行度是否能整除相应维度 |
| `kv_bytes_per_token` | 保存各引擎实测的每 token KV cache 字节数，用于估算 KV pool 的 token 容量；缺少对应引擎的值时，部署表单显示“无 token 容量” |
| `model_config.kv_cache: false` | 明确声明模型没有按 token 索引的 KV cache，例如扩散或纯编码器模型；此时 KV 容量估算“不适用” |

`kv_cache: false` 是目录扩展的声明，不是 `config.json` 原有字段。“不适用”是已知模型不需要 KV cache，不能与缺少配置或实测数据混为一谈。完整字段约束见[模型级字段](inference-catalog-spec.md#模型级字段)。

### 如何维护和校验这些事实

维护工具是 `cmd/gpuworks-catalog-model-config`：

- 从 spec 已声明的权重源读取真实 `config.json`，解析模型结构信息；默认只报告变更，实际回写使用 `-write`。受限仓库需要 `HF_TOKEN`。
- 使用 `-verify`，再根据已发布的权重索引核对层数和注意力类型，包括 MLA 潜在量与显式 K/V 的区别。
- 少数仓库无法读取结构信息时，在 `fallback-sources.json` 中记录使用的镜像仓库、子目录，或逐字段声明的结构信息及其依据。

`internal/server/inferencecatalog/seed` 的测试检查 `model_config` 在存储后是否保留相同信息，以及 KV 实测值是否合理，并统计实测覆盖情况。尚未实测的模型会记为覆盖缺口；当前测试不会因为未达到全覆盖而失败，不能把测试通过当作所有模型都已有 KV 容量数据。

核对权重索引时还需识别以下差异：

- DeepSeek-V3 系可能比配置多一个 MTP 层（61+1）；视觉、音频编码器可能含多个并行堆栈。`-verify` 已按这些情况归类。
- `dots-studio/dots3-note-prev` 的 checkpoint 比 config 多一层。服务引擎按 config 建模，因此以 config 为准。
