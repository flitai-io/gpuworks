# OpenAI 兼容网关

server 的 `/v1/*` 默认要求 API key 鉴权，规则见下文[认证](#认证)。请求体中的 `model` 指定对外提供推理服务的模型名称（served model）；server 据此找到部署、实例和节点，通过 agent tunnel（agent 隧道）转发到本地端口。管理 API 使用 JWT，不能用管理 JWT 代替网关 key。

## 认证

`/v1` 默认要求 API key：请求不带 `Authorization: Bearer <key>`，或带的 key 不存在、已吊销，都返回 401。

唯一例外由管理员显式打开，开关是安全设置里的 `inference_open_access`（默认 `false`），在控制台**设置 → 安全**的「安全」卡片里改，也可以用 `PATCH /api/v1/security/settings` 修改，见 [REST API 全量路由](api.md#settings--network-presets)。

- 关闭（默认）：每个请求都要带有效 key。库里没有任何 key 时也一样：新装的 server 不再自动创建 `default` 令牌，在操作员创建第一个 key 之前，`/v1` 拒绝所有请求。
- 打开：不带 key 的请求被匿名接受，能访问全部已部署模型；带了 key 的请求仍照常校验，key 无效照样返回 401。此时任何能访问这个监听地址的人都能调用已部署模型、占用它们的 GPU。
- 读不到设置时按关闭处理：设置读取失败不会放行不带 key 的请求，只按「要求 key」处理。
- 吊销 key 不改变这个判定：吊销最后一个 key 不会重新打开网关，`/v1` 会拒绝所有请求，直到再创建一个 key，或管理员打开上面这个开关。

这条规则覆盖全部 `/v1` 路由，包括 `/v1/models`、`/v1/files`、`/v1/batches`、`/v1/tokenize` 与 `/v1/detokenize`。401 的响应形态按入口不同：网关路由返回 `{"error":{"message":"invalid or missing API key","type":"gpuworks_proxy"}}`，Files 与 Batches 返回 OpenAI 格式的 `invalid_api_key`，见[差异与排查](#差异与排查)。

`GET /v1/models` 列出网关此刻能路由的服务名（来自网关按事件更新的路由表，不读数据库）：只含活跃部署，每个服务名一次，按名字排序；带范围的 key 只看到它能调用的部署的服务名。刚创建或刚停止的部署在路由表重建后出现或消失：部署事件后最多约 0.5 秒，没有事件时也每 2 秒刷新一次。

## 支持的操作

| 操作 | 默认 endpoint | 编码/模式 |
|---|---|---|
| chat | /v1/chat/completions | JSON |
| text completion | /v1/completions | JSON；可流式。提示词原样交给引擎，不套聊天模板，用于基座模型和按固定格式驱动的任务模型 |
| embeddings | /v1/embeddings | JSON |
| rerank | /v1/rerank（兼容 /rerank） | JSON |
| audio transcription | /v1/audio/transcriptions | multipart |
| audio speech | /v1/audio/speech | JSON |
| image generation | /v1/images/generations | JSON 或 multipart |
| image editing | /v1/images/edits | multipart；必须以源图作为 `image` 文件字段 |
| video generation | /v1/videos | JSON 或 multipart；可 async |
| realtime | /v1/realtime | WebSocket |
| batch（Files 与 Batches） | /v1/files、/v1/batches | JSON；上传文件为 multipart。异步批处理，见[批处理](#批处理files-与-batches) |

每个档位（spec）的 API contract（接口契约）声明该部署可用的操作和 endpoint（接口路径）。两个入口的检查范围不同：

- 控制台试验场（`/api/v1/playground/v1/*`）按 contract 限定，不在其中的路径返回 404。
- 公网 `/v1` 不做 contract 检查，只按 `model` 选副本并转发；引擎不支持的路径由引擎自己返回（通常是 404）。

视频异步任务还提供 `GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` 和 `DELETE /v1/videos/{id}`。

## 副本选择与重试

多副本部署默认使用 least-request（优先选择请求较少的副本）：网关在内存候选表中按本网关正在向各副本转发的请求数（在途请求数）选择副本，并参考近期有效的引擎排队数/KV 使用率，对排队较多或 KV 使用率达到阈值的副本降低选择优先级（软惩罚）。

- 连接级失败时，请求仍可重放才会换副本重试。
- 已经收到上游响应（包括 5xx）时，不会重试。
- 实例休眠、启动中或 rank 非 0 不可服务时，返回 503，并保留重试语义。
- server 正常停止时，正在等休眠部署唤醒的请求返回 503，带 `Retry-After: 10`，消息为 `the server is restarting; send the request again`，重发即可。

部署可以通过管理 API `PUT /api/v1/deployments/{id}/routing` 选择 `least_request`、`prefix_affinity` 或 `failover`。前缀亲和优先读取 `X-GPUWorks-Affinity-Key`，也支持从 chat messages、prompt 或 input 提取稳定 key；该请求头不会转发给引擎。

## 批处理（Files 与 Batches）

网关支持 OpenAI 的 Batch API：用 `POST /v1/files`（`purpose=batch`）上传 JSONL 文件，用 `POST /v1/batches` 创建任务，再用 `GET /v1/batches/{id}` 查询进度，从 `GET /v1/files/{id}/content` 取输出文件和错误文件。已支持的路由有：

- Files：`POST /v1/files`、`GET /v1/files`、`GET /v1/files/{id}`、`GET /v1/files/{id}/content`、`DELETE /v1/files/{id}`。
- Batches：`POST /v1/batches`、`GET /v1/batches`、`GET /v1/batches/{id}`、`POST /v1/batches/{id}/cancel`。

任务里的 `chat/completions`、`completions`、`embeddings` 请求按 `body.model` 走和普通请求相同的网关路径，计入用量和请求记录，不支持流式。这两组路由和其余 `/v1` 路由一样用 API key 鉴权，错误按 OpenAI 格式返回。步骤和示例见[运行批处理任务](../how-to/batch-api.md)，对象与错误码见 [REST API 全量路由](api.md#batches批处理任务)。

## 分词（tokenize）

`POST /v1/tokenize` 与 `POST /v1/detokenize` 按请求体的 `model` 选部署与副本，和其他 `/v1` 请求一样走 API key 鉴权、模型名改写、休眠/启动中的 503 与连接级重试，但转给引擎的根路径 `/tokenize`、`/detokenize`（vLLM 只在根路径提供，SGLang 根路径与 `/v1` 都有）。请求与回复原样转发，形状是引擎自己的，例如 vLLM：

- `/tokenize` 收 `prompt`，或 `messages`（可带 `add_generation_prompt`，缺省 `true`、`chat_template_kwargs`），另有 `add_special_tokens`、`return_token_strs`；回复 `{"count", "max_model_len", "tokens", "token_strs"}`，`token_strs` 不要时为 `null`。
- `/detokenize` 收 `tokens`（整数数组），回复 `{"prompt": "..."}`。
- 引擎的校验错误（400）与模型不存在（404，`{"error": {"message", "type": "NotFoundError", "param": "model", "code": 404}}`）原样返回。

分词不是推理流量：不计入网关的请求数、耗时与在途数，不留请求记录，不更新部署的最近请求时间，也不导出 trace。只接受 POST，其他方法网关回 405（带 `Allow: POST`）。根路径 `/tokenize` 归控制台，网关不接管。

每个部署可以关掉分词转发：创建时 `tokenize_enabled: false`，或 `PUT /api/v1/deployments/{id}/tokenize`（见 [API 参考](api.md)）；默认开启。关掉后网关直接回 404（`deployment <model> does not expose POST /v1/tokenize: tokenize is turned off for it`），不联系引擎。开关随网关路由表生效，不重启实例。vLLM、SGLang 以外的引擎未必提供分词接口，届时请求收到引擎自己的错误。

## Trace context

追踪上下文用于把**同一次调用在不同系统中的记录关联起来**。例如，请求经过上游网关、GPUWorks 和推理引擎时，可以据此查出哪一段耗时或失败。GPUWorks 会为每个转发请求自动建立追踪上下文；普通推理调用不需要手工填写这些信息。

### 追踪标识与请求 ID

| 名称 | 含义与用途 |
|---|---|
| trace / trace id | 一次完整追踪及其标识。上游网关、GPUWorks 和引擎沿用同一个 trace id，才能在追踪系统中关联各自的记录 |
| span | 追踪中的一段操作。例如，GPUWorks 转发请求是一段，引擎处理请求是另一段；父子关系表示哪段操作调用了下一段 |
| 请求 ID / `X-Request-ID` | GPUWorks 为本次请求分配的标识，通过响应头返回，也与 trace id 一起保存在请求记录中。用于在平台内查找请求；跨系统关联使用 trace id |

客户端传入的 `X-Request-ID` 不会替换 server 分配的请求 ID。

### 如何接上调用方的追踪

系统之间通过 W3C 请求头传递追踪上下文：`traceparent` 携带 trace id、调用方的 span id 和采样标记；`tracestate` 携带追踪系统的附加信息。采样标记用于表达该追踪是否被选中记录。

| 调用方的请求 | GPUWorks 的处理 |
|---|---|
| 带合法的 `traceparent` | 沿用 trace id，以调用方的 span 为父创建网关 span，并沿用调用方的采样决定与 `tracestate` |
| 未带 `traceparent`，或其值不合法 | 新建一个 trace 和网关 span |

因此，Higress、Kong、LiteLLM 等上游网关开启 W3C 上下文传播后，GPUWorks 的处理记录就可以接入同一次追踪。

GPUWorks 转发给引擎时，会更新 `traceparent`，让引擎知道这次调用来自哪一个 span，并继续传递有效上游上下文中的 `tracestate`（重复头合并并去除首尾空白，超过 512 字节时丢弃）：

- 普通转发、重试，以及 PD 分离部署的 decode 请求，都以网关 span 为父。
- PD 分离部署把提示词计算（prefill）和后续内容生成（decode）交给不同实例。顺序交接时，网关另建 `pd.prefill` 子 span，发给 prefill 实例的请求以它为父；并发交接的 prefill 请求仍以网关 span 为父。
- 引擎启用 OpenTelemetry（OTel，例如 vLLM 的 `--otlp-traces-endpoint`）后，才能生成引擎自己的 span，并按上述父子关系接入追踪。

网关新建的 trace id 为 16 字节：前 6 字节保存建立追踪时的 Unix 毫秒时间，后 10 字节随机。它同时设置 Trace Context Level 2 的 random 位，表示最右侧 7 字节符合随机性要求。设计依据见 [ADR 0046](../adr/0046-gateway-request-traces.md)；PD 子 span 见 [ADR 0047](../adr/0047-pd-request-breakdown.md)。

导出到外部追踪系统见[导出请求 trace 到 OpenTelemetry](../how-to/export-request-traces.md)；在控制台按 trace id 或请求 ID 查找请求见[查看指标与检索日志](../how-to/monitor-and-logs.md#2-看请求视图与请求分布)。

## 用量（usage）

`usage` 是引擎在响应中提供的用量统计。对文本生成请求，它通常记录输入、输出各使用了多少 token。token 是模型处理文本的计数单位，不等同于字数或字符数。

| 常见字段 | 含义 |
|---|---|
| `prompt_tokens` | 输入提示词的 token 数 |
| `completion_tokens` | 生成内容的 token 数 |
| `total_tokens` | 输入与生成内容的 token 总数 |
| `prompt_tokens_details.cached_tokens` | 输入中命中缓存、无需重复计算的 token 数；它是输入 token 的一部分，不额外加到总数中 |

具体返回哪些字段取决于引擎。网关通常原样转发 `usage`，下面几种情况有额外处理。

### 流式请求如何获取用量

流式响应会分多次发送生成内容，用量统计可能作为流末尾的一个独立事件返回。**平台需要记录用量，但客户端不一定请求了这个事件。**

网关仅在以下条件全部满足时，自动向引擎索取用量：

- 引擎是 vLLM 或 SGLang。
- 接口是 `/v1/chat/completions` 或 `/v1/completions`。
- 请求体是网关可读取的 JSON，且设置了 `stream: true`。
- 客户端没有设置 `stream_options`。

满足条件后，网关在转发的请求中补上 `"stream_options":{"include_usage":true}`。引擎返回用量时，网关先记录统计，再从响应流中移除因此增加的独立 usage 事件，客户端不会额外收到这个事件。

如果客户端希望在响应流中收到用量，可在请求体中加入以下字段：

```text
"stream_options": {"include_usage": true}
```

只要客户端已经设置 `stream_options`（包括设置为 `null` 或 `include_usage: false`），网关就不再自动补充或覆盖它；是否返回用量由引擎按客户端的设置处理。

### Messages 与 Responses 的用量

Anthropic Messages（`/v1/messages`）与 OpenAI Responses（`/v1/responses`）请求原样转发。网关不往请求里加任何字段，因为这两个协议的流式响应也总带 usage；网关只从回复里读取平台自己的统计。

输入统一记为**总输入**：

| 协议与引擎返回字段 | 总输入 token 数 | 缓存命中 token 数 |
|---|---|---|
| Responses | `input_tokens` | `input_tokens_details.cached_tokens` |
| Messages 带 `cache_read_input_tokens` / `cache_creation_input_tokens`（vLLM） | `input_tokens` 仅为未命中部分；总输入为这三个字段之和 | `cache_read_input_tokens` |
| Messages 不带这两个缓存字段（SGLang） | `input_tokens` | 未知 |

Messages 流式的输入取自 `message_start`，输出取自最后的 `message_delta`（SGLang 最后的 `message_delta` 只有输出）。只有 Responses 报告思考 token 数（`output_tokens_details.reasoning_tokens`）。

结束原因归一成 chat 的写法：`max_tokens`，或状态为 `incomplete` 且原因是 `max_output_tokens`，记为长度截断；`end_turn` 与 `completed` 记为正常完成。vLLM 在截断时仍发 `response.completed`，因此以其中 response 的 `status` 为准。

### PD 分离部署的缓存命中数取自哪里

PD 分离将输入提示词计算和后续生成交给不同实例。vLLM 顺序交接成功时，prefill 实例先计算提示词，再把计算结果（KV）交给 decode 实例继续生成内容。decode 把收到的 KV 全部算作缓存命中，因此它报告的 `cached_tokens` 等于整个提示词长度，不能用来判断 prefill 阶段实际复用了多少缓存。

**顺序交接模式 `vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake` 使用 prefill 实例报告的缓存命中数。** 例如，输入有 1,036 个 token，prefill 实际命中缓存 512 个；即使 decode 报告命中 1,036 个，网关返回的 `cached_tokens` 仍为 512，表示有 512 个输入 token 直接复用了 prefill 的缓存结果。

| 情况 | 返回的 `cached_tokens` |
|---|---|
| 顺序交接成功，prefill 回复提供了缓存命中数 | 用 prefill 的值替换 decode 回复中的 `prompt_tokens_details.cached_tokens` |
| prefill 回复未提供该值 | 保留 decode 的值 |
| 请求降级，由 decode 独立完成输入计算和内容生成 | 保留 decode 的值 |
| SGLang 并发交接 | 保留 decode 的值 |

替换只影响这个缓存命中数，`usage` 其他字段仍来自 decode。网关原地改写数字，数字变短时补空格，保持 `Content-Length` 不变；新值需要的位数超过原值时不改写。客户端、平台请求记录、网关计数和导出的 trace 使用同一个数。

## Playground

模型列表只展示已部署且声明了推理 API 操作的模型；全局设置中的外部模型供 AI 助手使用，不自动加入试验场。

已部署模型可从管理 JWT 保护的 `/api/v1/playground/v1/*` 调用。

外部模型名形如 `external:<config-id>:<model>`，仅允许 chat completions。外部 endpoint key 在 server 端加密解密后转发，不写入浏览器或实例环境。

## 差异与排查

网关不模拟 OpenAI 的全部产品接口，具体能力以 spec contract 和 `GET /api/v1/playground/openapi.json` 为准。

| 状态码 | 检查项 |
|---|---|
| 401 | 先检查 `Authorization: Bearer <key>`。完全不带 key 时，确认是否真的需要匿名访问（见[认证](#认证)）；`/v1` 默认拒绝一切不带 key 的请求 |
| 404 | 检查 `model` 名称；试验场还要看 contract |
| 503 | 检查实例状态、sleep/wake 和节点是否在线 |
| 502 | 查看 tunnel 和引擎日志 |

流式 SSE 与二进制（例如 WAV）按原始 content type 转发，不截断响应。
