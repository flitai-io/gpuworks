# 镜像目录与推理镜像家族

GPUWorks 自己组装完整引擎命令，不依赖镜像默认 entrypoint。镜像页按「系列 → 版本 → 节点分布」管理运行镜像，
版本按来源分三类。这三类是同一种镜像版本记录上的来源标记，不是三种不同的对象：

| 来源（界面） | 数据库与 API 中的 `source` | 谁维护 | 能改什么 |
|---|---|---|---|
| 系统镜像 | `official` | GPUWorks 发布的镜像目录 manifest | 元数据只读、不能删除；运维可启用/停用系列与版本，可给版本追加获取地址（如归档 URL）。只会被 manifest 更新删除或弃用 |
| 社区镜像 | `community` | 用户在社区仓库里挑 tag 添加 | 可编辑；删除在详情抽屉里 |
| 自定义镜像 | `custom` | 用户填任意引用添加 | 同上 |

## 节点镜像身份与周期盘点

镜像系列详情 `GET /api/v1/images/{id}` 中，版本节点行的 `image_digest_id` 是节点最近上报的**运行时镜像 ID**。
Docker 从 `docker image inspect` 的同一份结果读取 `Id` 与运行时大小；其中 `Id` 是镜像 config 的 digest，
不是 registry manifest/index digest，也不是镜像目录的整数 ID。离线 `docker load` 后也可以取得这个 ID；
不能把它当成 registry digest 来拼拉取地址、校验 manifest 或推断下载包字节。

Agent 的周期 `status.node` 消息在 `images[]` 中上报以下字段（仅节选镜像部分）：

```json
{"images":[{"image_ref":"registry.example/team/runner:v1","present":true,"image_id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","runtime_size_bytes":8192}]}
```

`image_id` 是可选字符串，空值编码时省略；未观测到身份的运行时和旧 Agent 可以只上报存在性。
服务端去除 ID 的首尾空白，结果为空时也按未观测处理。
`runtime_size_bytes` 是独立的运行时测量，省略表示未知；旧 `size_bytes` 或传输进度字节不能代替它。
运行时检查失败时跳过该条报告，不把失败或缺少报告解释为镜像不存在。

服务端仅对目录已知的 `image_ref` 对账，身份更新规则如下：

| 盘点结果与当前状态 | `image_digest_id` 的处理 |
|---|---|
| 存在，带非空 `image_id`，且没有进行中的拉取或同步 | 采纳去除首尾空白后的运行时 ID；同一引用的 ID 改变时更新 |
| 存在，但 `image_id` 省略或为空 | 保留已有 ID；新节点行的身份仍为未知，兼容旧 Agent |
| 拉取或同步中收到存在报告 | 不采纳盘点的新 ID，保留传输状态；完成与失败仍由显式传输结果决定 |
| 明确报告 `present: false` | 清空旧 ID，包括拉取、同步中或失败的行；进行中的传输状态或已记录失败仍保留 |

因此，进行中的传输不会被一次存在性盘点判为完成；明确不存在时清除的身份也不会被当成传输失败。
`present: true` 只说明该引用存在；没有新 ID 时保留的值可能来自之前的观察，不能据此声称本次重新核实了身份。
已稳定的节点行在状态、身份和大小均未改变时不重复写入。

## 系统镜像

内置目录 `internal/server/imagecatalog/catalog-seed.json` 由 [`gpuworks-image-manifest`](cli.md#gpuworks-image-manifest)
从 gpuworks-runner 构建矩阵生成，当前 29 个版本，全部是 `gpuworks/gpuworks-runner:<id>-<revision>`，
按引擎 × 框架分 14 个系列：

| 系列 | 厂商 / framework | 引擎 | 示例 image_ref | 架构 |
|---|---|---|---|---|
| vLLM · CUDA | NVIDIA / cuda | `vllm` | `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4` | multi |
| vLLM Omni · CUDA | NVIDIA / cuda | `vllm-omni` | `gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.30.0-r4` | multi |
| SGLang · CUDA | NVIDIA / cuda | `sglang` | `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2` | multi |
| vLLM · CANN | 昇腾 / cann | `vllm` | `gpuworks/gpuworks-runner:cann9.1-910b-vllm0.27.1rc1-r2` | multi |
| vLLM Omni · CANN | 昇腾 / cann | `vllm-omni` | `gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3` | multi |
| SGLang · CANN | 昇腾 / cann | `sglang` | `gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r2` | multi |
| vLLM · DTK | 海光 / dtk | `vllm` | `gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2` | amd64 |
| SGLang · DTK | 海光 / dtk | `sglang` | `gpuworks/gpuworks-runner:dtk26.04-sglang0.5.12-r2` | amd64 |
| vLLM · HGGC | PPU / hggc | `vllm` | `gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r2` | amd64 |
| SGLang · HGGC | PPU / hggc | `sglang` | `gpuworks/gpuworks-runner:hggc13.0-sglang0.5.13-r3` | amd64 |
| vLLM · Metal | Apple / metal | `vllm` | `gpuworks/gpuworks-runner:metal15.0-vllm0.31.0-r2` | arm64（darwin 环境包） |
| MLX-VLM · Metal | Apple / metal | `callable` | `gpuworks/gpuworks-runner:metal15.0-mlx-vlm0.7.8-r2` | arm64（darwin 环境包） |
| MLX Audio · Metal | Apple / metal | `callable` | `gpuworks/gpuworks-runner:metal15.0-mlx-audio0.5.9-r1` | arm64（darwin 环境包） |
| llama.cpp · Metal | Apple / metal | `callable` | `gpuworks/gpuworks-runner:metal15.0-llama.cpp-b11617-r1` | arm64（darwin 环境包） |

tag 文法是 `<framework><framework 版本>[-<型号>]-<引擎><引擎版本>-<revision>`；llama.cpp 按上游写作 `llama.cpp-b<构建号>`。引擎、引擎版本、framework 与
framework 版本都从 tag 解析；昇腾型号段（`910b`、`a3`、`310p`、`950`）只在 tag 与版本备注里，不是独立字段；
架构取自构建平台（多平台为 `multi`）。每个版本的 `registry_sources` 列出同一 tag 在 Docker Hub 之外的副本：
`quay.io/gpuworks/gpuworks-runner`、`swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner`。
内部仓库 registry.zstack.ai 不在清单里。manifest 带上这份清单，节点拉取时把它们和 Docker Hub 地址一起按可达性排序。

各引擎的命令：

| 引擎 | GPUWorks 命令 |
|---|---|
| `vllm` | `vllm serve /model ...` |
| `sglang` | `python -m sglang.launch_server --model-path /model ...` |
| `vllm-omni` | omni 多模态专用命令 |
| `callable` | 目录自带的 Serve Adapter，包括系统 Metal 镜像的适配器 |

### 启动同步

内嵌种子是随 server 二进制发布的镜像目录。server 根据构建是否变化，决定是否把这份目录同步进数据库
（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)）：

- 换了构建（升级或重装二进制）后的第一次启动，包括执行 `gpuworks migrate` 时，会在打开数据库之后、任何组件解析镜像之前同步内嵌种子。不访问网络，并打印下方的同步日志。
- 同一构建再次启动时不重放种子。数据库保留最后一次导入的目录，无论它来自检查更新、上传 manifest，还是上次重放种子；日志为 `official catalogs kept from their last import`。

```text
image catalog synced catalog_version=2026-10-09.f085e40b created=0 updated=25 deleted=0 deprecated=0 skipped=0
```

同步规则与手动更新目录相同（见下节），只是冲突处理不同：种子条目与社区/自定义行同名系列或同一 ref 冲突时，
启动同步只记一条 `image catalog entry skipped` warn 日志并跳过该条，不阻止启动。

### 更新目录

系统目录只有两条更新路径，都在镜像页「系统镜像」标签上：

- **检查更新**：`POST /api/v1/images/sync`，服务端从发布通道读取当前渠道的签名索引，下载其中列出的镜像目录并按索引里的
  sha256 校验（见[配置参考](configuration.md#镜像目录与镜像拉取)）。没有一个发布源返回可用索引时返回 502
  `UPDATE_SOURCES_UNAVAILABLE`，索引可用但没有源提供与索引一致的目录文件时返回 502 `UPDATE_DOWNLOAD_FAILED`
  （`detail` 都列出每个源的原因），渠道没有发布镜像目录时返回 404 `UPDATE_NOT_PUBLISHED`，库都不动。
- **上传 manifest**：`POST /api/v1/images/import`，请求体就是 manifest JSON 本身。服务器不能访问公网时用它：
  在能联网的机器上从任一发布源下载当前渠道的 `<channel>/manifest.json`（如 `https://dl.gpuworks.ai/release/manifest.json`，与渠道索引里的镜像目录是同一文件）再上传。

两条路径都把本次的 `catalog_version` 记为当前目录版本，设置页「版本与更新」据此显示是否有更新。

两条路径与启动同步共用一套导入规则：

1. 按 `image_ref` 新增或更新（upsert）系列与版本；运维对系列、版本的启用开关保留；系统版本上用户登记的归档 URL
   （`http(s)://` 开头的获取地址）保留。
2. manifest 里已经没有的系统版本（通常是同一档升了版次，例如 r1 换成 r2）：既没有引用、也不在任何节点上的直接删除
   （连同它在各节点的分布记录，系列空了也删）；仍有引用、或有节点已经拉取（分布状态为 `present`）的保留为**旧版本**：
   API 字段 `deprecated` 为 `true`，`deprecated_since` 记为本次 `catalog_version`，镜像页显示「旧版本」，有引用时显示「旧版本 · N 个引用」。
   旧版本照常可用，新部署不会再选它；在各节点删掉它之后，下一次目录同步把这一行清掉。
   「引用」指 `deployments.image`、未结束的 `instances.image`（`stopped`、`failed`、`pending_removal` 不算）
   或 `models.image_id` 任一命中。删除社区/自定义镜像时用的是同一口径：仍被引用的版本或系列拒绝删除（409 `IMAGE_IN_USE`），
   只在节点上存在不算引用、不会阻止删除。
3. 手动更新时 manifest 条目与社区/自定义行同名系列或同一 ref 冲突，整次导入返回 400 `IMAGE_IMPORT_CONFLICT`，
   不写入任何内容。
4. 同一份 manifest 导入两次结果相同。没有版本比较、防降级、签名或来源校验：导入什么就是什么。

响应（启动同步日志里的计数也是这几项）：

```json
{"catalog_version":"2026-10-09.f085e40b","created":0,"updated":25,"deleted":0,"deprecated":0,"skipped":[],"series":[...]}
```

### manifest 格式

内嵌种子与上游 `manifest.json` 是同一个文件。`schema_version` 必须是 `3`（兼容导入 `2`，其他值 400
`IMAGE_MANIFEST_INVALID`）；`catalog_version` 必填，形如 `<日期>.<内容 hash 前 8 位>`，只用于展示与日志，
不参与比较；可选的 `sha256` 是正文完整性哈希（按 `sha256` 字段置空后的规范 JSON 计算），存在时不匹配即拒绝。

```json
{
  "schema_version": 3,
  "catalog_version": "2026-10-09.f085e40b",
  "sha256": "<正文 sha256，可选>",
  "series": [
    {
      "name": "vLLM · CUDA",
      "engine": "vllm",
      "purpose": "inference",
      "framework": "cuda",
      "versions": [
        {
          "name": "cuda13.0-vllm0.30.0-r2",
          "image_ref": "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2",
          "engine_version": "0.30.0",
          "framework_version": "13.0",
          "arch": "multi",
          "registry_sources": [
            "quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2",
            "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"
          ],
          "notes": "..."
        }
      ]
    }
  ]
}
```

每个系列至少一个版本；`image_ref` 与 `registry_sources` 都必须带 tag，不接受 digest；`image_ref` 全文件唯一。
旧文件里的 `run_spec`、版本级 `command_mode`、系列级 `vendors`、版本级 `supported_gpus`/`unsupported_gpus`
在导入时忽略：镜像目录不承载启动配置（ADR 0031），厂商由 `framework` 推导。

## 社区镜像

「社区镜像」标签的「添加社区镜像」抽屉列出内置的社区仓库卡片（`GET /api/v1/images/community/repos`）：

| 仓库 | 注册表类型 | 引擎 / framework |
|---|---|---|
| `vllm/vllm-openai` | Docker Hub | vllm / cuda |
| `vllm/vllm-omni` | Docker Hub | vllm-omni / cuda |
| `lmsysorg/sglang` | Docker Hub | sglang / cuda |
| `quay.io/ascend/vllm-ascend` | quay | vllm / cann |
| `quay.io/ascend/sglang` | quay | sglang / cann |
| `harbor.sourcefind.cn:5443/dcu/admin/base/vllm`，同卡片还有 `…/custom`、`…/sglang` | OCI distribution | vllm / dtk |

点开卡片（或输入任意仓库）时，`GET /api/v1/images/community/browse?repo=<仓库>` 列出该仓库在上游的**全部**
tag，不做 semver 过滤，`vllm/vllm-openai:qwen38` 这类 tag 同样可选；前端按名称搜索。

- Docker Hub 与 quay 的 tag 带上游更新时间（`last_updated`），OCI 的 tag 列表没有时间。
- 未配置的仓库按主机推断类型：没有主机名为 Docker Hub，`quay.io/` 为 quay，其他按 OCI distribution 处理。
- OCI 仓库回 401 时按 Bearer 挑战匿名取 token 再试一次，仍 401 报 `community registry requires credentials`；
  当前不支持私有仓库凭据。
- 翻页最多 10 页，每页请求 8 秒超时；结果在内存缓存 1 小时，不落库。缓存期内上游消失的 tag 在响应里标
  `upstream_gone`。
- `image_community_catalog` 设置可以覆盖或追加卡片，见[配置参考](configuration.md#镜像目录与镜像拉取)。

挑中 tag 后在同一抽屉里补齐框架、版本等字段再添加，才会写入目录（`source=community`）；浏览本身不落库。

## 自定义镜像

「自定义镜像」可以添加任意带 tag 的引用。引擎下拉取自 `GET /api/v1/images/engines`：全部部署引擎（`vllm`、`sglang`、`callable`）
加上目录专用的 `vllm-omni`，取值、顺序与显示名都由 server 给出；框架必填，没有框架的镜像不参与自动选镜像。以 `callable` 登记的自定义镜像，
在部署页选 callable 引擎时可见；callable 引擎自己不提供命令行（[ADR 0036](../adr/0036-callable-command-owned-by-template.md)），
没有推理模板时要在部署的命令编辑里写出适配器的完整命令。添加抽屉里也可以先粘贴 `docker save` 归档的 URL 解析出真实引用，
见[从 HTTP 文件服务器导入](../how-to/import-from-http.md#从-url-导入镜像归档)。

社区与自定义的添加都走 `POST /api/v1/images/import` 的 `{"series":[...]}` 表单（没有 `schema_version`），
不能用它写 `source=official`。

## 滚动 tag

`latest`、`main`、`master`、`release`、`nightly`、`dev`、`edge`（整个 tag 匹配，不区分大小写）是滚动 tag，
内容会随上游变化：

- 自动选镜像（部署时没有显式指定镜像）从不选滚动 tag；手动选择不受限。
- 节点分布表显示每个节点上镜像的 12 位 digest 与更新时间；同一版本在多个节点上 digest 不一致时，详情里给出提示。
- 已拉取的滚动 tag 版本在节点行上有「重新拉取」（`POST /api/v1/images/{id}/nodes/{uuid}/sync` 带 `"force":true`）：
  节点上已经 `present` 也会真的重新 pull，拉完回填新 digest；该节点正在 `pulling`/`syncing` 时不重复开传输。
- 部署不会因为 tag 是滚动的而强制重拉，节点上已有的 digest 也不会被清除。

## 自动选镜像、拉取地址与命令模式

系列未设置专用默认字段；自动部署在匹配厂商、framework、引擎和架构后，从启用、未弃用、非滚动 tag 的版本里
选最新的。驱动与 framework 的兼容矩阵只用于排序和 Warning，不能替代真实硬件事实，也不会阻止用户显式选择镜像。
芯片家族同样只参与排序：家族由 tag 的昇腾型号段推出（`910b` 对应 910B 系列，`a3` 对应 910C，`310p` 对应 310P），节点一侧取所选（或第一张）卡探测到的家族。
为节点家族构建的镜像排在前面，没有型号段的通用镜像其次，为另一家族构建的镜像（例如 910B4 节点上的 `a3` 档）排在最后、低于驱动兼容 Warning 的镜像；
它仍在可选镜像里，手动指定也照常使用。任一边家族未知时（含目前的 `950` 卡，其家族要等探测到它的芯片标识才有）不参与排序。
目录没有候选时，非 NVIDIA 节点不会错误回退到 CUDA profile 的引擎默认镜像（`vllm/vllm-openai:latest`、
`lmsysorg/sglang:latest`）；用户可显式覆写镜像。

`image_ref` 是规范引用（Docker Hub 地址）。节点实际从哪里拉由[拉取设置](../how-to/configure-pull-settings.md)决定：
适用于该节点的来源规则产生的替代地址在前，然后是 `registry_sources` 与规范地址，由节点按可达性排序；拉到后在本地
tag 回规范引用，`image_ref` 语义不变。候选顺序见[排障](../how-to/troubleshooting.md#镜像拉取失败)。

默认的引擎接管命令模式下，Docker、runveil（`udocker` driver）和 nerdctl 都会覆盖镜像默认 entrypoint，
再拼接实际参数，避免重复执行 vllm serve 或 tini --。

命令模式只由部署显式选择（默认引擎接管），不随镜像来源或
镜像目录变化；镜像目录只提供 framework、版本与获取地址，不承载启动配置（ADR 0031），镜像自带的 ENTRYPOINT/CMD/ENV
可在镜像详情里对节点检查查看。

GPU 型号、compute capability 等事实来自节点探测，不在镜像目录中维护自由文本兼容标签。
各启动字段的来源见[部署启动参数来源](deployment-launch-sources.md)。

runveil（`udocker` driver）对所有容器都会创建 canonical ROOT loopback（供 proot 客户机解析 `/proc/self/maps`
泄漏的宿主机路径）；仅 NVIDIA 容器会额外设置 TRITON_LIBCUDA_PATH、FLASHINFER_DISABLE_VERSION_CHECK、
SGLANG_SKIP_SGL_KERNEL_VERSION_CHECK。这些变量只解决库发现/版本检查，不绕过 NCCL 安全检查。完整
runtime 解释见架构说明。
