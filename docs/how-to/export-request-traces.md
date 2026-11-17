# 导出请求 trace 到 OpenTelemetry

**适用场景**：把平台网关的逐请求数据接到自己的追踪系统（Jaeger、Grafana Tempo、Grafana Cloud、Datadog 等），让上游网关、平台网关和你自己的服务在同一条 trace 里，并在控制台与追踪系统之间按 trace id 来回跳。

**前置条件**：

- 能以管理员身份修改控制台设置。
- 有一个可调用的推理部署及其服务名（`served_name`），并已创建推理服务令牌，见入门教程中的[创建推理令牌与调用](../tutorials/getting-started.md)。
- 准备能收 OTLP/HTTP 的端点（OpenTelemetry Collector，或支持该协议的追踪后端），server 能访问它的 OTLP/HTTP 端口（默认 4318）。只有导出网关 trace 时，节点不需要能访问它；尚无端点可按第 1 节启动试用实例。
- 普通内网或 server 本机回环的端点默认无需白名单；仅当开启严格模式时才需要白名单；平台管理员可在**设置 → 安全与访问 → 出站访问策略**管理，或由主机管理员直接编辑 `server.yaml` 的 `outbound_restrict_private_hosts` 和 `outbound_allow_private_hosts` 授权目标主机。两个字段保存后下一次请求生效，无需重启 server，见[出站访问配置](../reference/configuration.md#server-配置文件-serveryaml)。

**会得到什么**：网关为每个转发的 `/v1` 与 Playground 请求，在请求结束后发一个 span：模型、调用方、部署与副本，输入 / 输出 / 缓存命中 / 思考 token，采样参数，首 token 时间，网关、排队、Prefill、思考、回答各阶段耗时，结果（完成、截断、客户端断开、出错）。上游带了 `traceparent` 时，这个 span 挂在上游 span 下面。不导出提示词与回复内容，平台本身也不保存它们。字段见文末[附录](#附录span-字段)。

<a id="1-五分钟上手用-jaeger-看到第一条-trace"></a>

## 1. 用 Jaeger 看到第一条 trace

Jaeger 2.x 的 all-in-one 镜像内核就是 OpenTelemetry Collector，单个容器同时收 OTLP 和提供查看界面（数据在内存里，重启即丢，适合试用）。

1. 在 server 能访问的机器上启动 Jaeger：

   ```bash
   docker run -d --name jaeger -p 4318:4318 -p 16686:16686 jaegertracing/jaeger:2.11.0
   ```

2. 控制台**设置**→**集成**→**请求 Trace 导出（OpenTelemetry）**（需要管理员）：
   - **Collector 地址**填 `http://<jaeger 所在机器>:4318`；
   - **Trace 链接**填 `http://<jaeger 所在机器>:16686/trace/{trace_id}`；
   - 点**发送测试 span**，看到「Collector 已接收测试 span」说明地址通了；
   - 打开**开启导出**，保存。立即生效，不用重启。

3. 在能访问 server 的终端发一个带 trace 的请求。将 `<服务名>` 替换为部署详情中的服务名 `served_name`，不是部署的显示名称；令牌使用前面创建的推理服务令牌。
   `traceparent` 的格式是 `00-<32 位 trace id>-<16 位父 span id>-01`：

   ```bash
   curl http://<server>:8800/v1/chat/completions \
     -H "Authorization: Bearer <推理服务令牌>" -H "Content-Type: application/json" \
     -H "traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" \
     -d '{"model":"<服务名>","stream":true,"messages":[{"role":"user","content":"你好"}]}'
   ```

4. 等待请求结束，确认设置卡片的「已导出」数增长；再打开 `http://<jaeger 所在机器>:16686`，Service 选 `gpuworks`，
   或直接搜 trace id `4bf92f3577b34da6a3ce929d0e0e4736`，点开对应请求的 span 看属性和时间线事件。
   测试 span 成功只说明连通，看到实际请求的 span 才说明这条导出流程完成。
5. 回到控制台**监控中心**，右上角搜索框粘贴同一个 trace id，点开请求详情，**在追踪系统中打开**会跳回 Jaeger。

不带 `traceparent` 的请求同样会导出：网关自己起一个 trace（受采样比例控制）。

## 2. 接入生产环境

**推荐在 server 同机或同机房放一个 OpenTelemetry Collector**，平台只发给它，由它批量转发到远端后端。
部署后仍需观察导出和丢弃计数：Collector 的处理能力、网络往返和请求量都会影响是否跟得上，见[排查](#5-排查)。

Collector 配置示例（收 OTLP/HTTP，转给 Tempo）：

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
processors:
  batch: {}
exporters:
  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo]
```

将示例中的 `tempo:4317` 替换为 Collector 能访问的后端地址，按现有 Collector 的部署方式保存并加载这份配置。
确认启动日志没有配置错误、OTLP/HTTP 接收端已监听 4318，再将平台的 Collector 地址改为该接收端。
重复第 1 节的测试 span 和真实推理请求，并到生产追踪后端确认可查询；仅保存配置文件不会使 Collector 使用它。

设置项说明：

| 设置 | 说明 |
|---|---|
| **Collector 地址** | OTLP/HTTP 地址，例如 `http://otel-collector:4318`；路径不是 `/v1/traces` 时自动补上。只支持 OTLP/HTTP（JSON + gzip），不支持 gRPC 端口 4317 |
| **请求头** | 后端要鉴权或分租户时加，例如 `Authorization: Bearer …`、`X-Scope-OrgID: team-a`。值加密保存，保存后不再显示；已保存的留空表示不改 |
| **采样比例** | 只作用于网关自己新起的 trace；带了 `traceparent` 的请求照调用方的采样决定（ParentBased）。默认 100%。平台自己的请求视图不受影响 |
| **Trace 链接** | 含 `{trace_id}` 的追踪系统地址，例如 Jaeger `http://jaeger:16686/trace/{trace_id}`；填了以后请求详情里的 trace id 可以直接打开 |

**发送测试 span**用当前填写的内容（不必先保存）发一个名为 `gpuworks test span` 的 span，只发一次不重试，结果里给出它的 trace id。卡片下方是自 server 启动以来的运行状态：已导出、丢弃、失败、排队数，以及最近一次成功与最近一次错误。

同样的配置可以用 API 完成：`GET/PUT /api/v1/telemetry/export`、`POST /api/v1/telemetry/export/test`，见 [API 参考](../reference/api.md)。

## 3. 接上上游网关与引擎

- **上游网关**：开启 W3C Trace Context 传播（`traceparent` / `tracestate`），平台网关的 span 就成为上游 span 的子 span，两边用 trace id 对应。平台的请求 ID（响应头 `X-Request-ID`）由 server 分配，客户端发来的 `X-Request-ID` 不会替换它。规则见 [OpenAI 兼容网关](../reference/openai-gateway.md#trace-context)。
- **引擎**：网关转给引擎的请求带 `traceparent`。想看引擎内部的 span，可以在部署的额外参数里给 vLLM 加 `--otlp-traces-endpoint <collector>`，地址和协议须符合该镜像版本的参数要求；引擎容器要能访问 Collector，镜像里要有 OpenTelemetry 包。不加也能导出网关 span；只有引擎实际自报了排队、Prefill、Decode 时间时，网关 span 才包含这些字段。PD 的父子关系见文末附录。

## 4. 在控制台与追踪系统之间跳转

- **从控制台到追踪系统**：请求视图里点开一行（或聚焦后按回车），请求详情里的 trace id 可复制；设置了 Trace 链接时点**在追踪系统中打开**。
- **从追踪系统到控制台**：在监控中心右上角的搜索框粘贴 trace id，列出该 trace 在平台上的全部请求（一个 agent 任务可能有很多次模型调用），点开看详情和所在部署。请求 ID 同样可以查。网关自己起的 trace id 带有时间，直接定位；其余默认查最近 1 小时，可扩大到 24 小时。

## 5. 排查

先看设置卡片下方的运行状态。

| 现象 | 原因与处理 |
|---|---|
| 测试 span 失败：连接被拒绝 / 超时 | server 到 Collector 不通：检查地址、端口（OTLP/HTTP 是 4318，不是 gRPC 的 4317）、防火墙；地址要从 server 所在机器可达 |
| 测试 span 失败：401 / 403 | 后端要鉴权：在请求头里加令牌 |
| 测试 span 失败：404 | 地址路径不对：只填到端口（`http://host:4318`），或填完整的 `/v1/traces` 路径 |
| 状态是「设置无法应用」 | 已存设置解密失败（例如换了数据目录的 `master.key`）；重新填写请求头并保存 |
| 「失败」在增长 | Collector 拒绝或连不上，原因看「最近一次错误」；429 / 502 / 503 / 504 和连接失败会按 `Retry-After` 或退避重试 3 次后才算失败 |
| 「丢弃」在增长 | 发送跟不上：Collector 处理慢或网络往返长，队列（8192 个 span）满了。在 server 旁边放一个 Collector，或调低采样比例 |
| 追踪系统里看不到某个请求 | 采样没选中（网关新起的 trace 按比例，上游 trace 照上游的决定）；或请求在路由之前就被拒绝（401、模型不存在、没有可用副本），这类请求不生成 span |
| span 没有排队 / Prefill / Decode | 引擎没自报：vLLM 部署要带**返回逐请求耗时**（`--enable-per-request-metrics`，0.25.0 及以上镜像的新部署默认打开） |
| 没有思考 token 和思考阶段 | 部署没配 reasoning parser，思考内容混在回答里 |

## 6. 限制

- 导出不阻塞转发：发送队列满时丢弃并计数，推理请求不受影响。
- 只导出已结束的请求；进行中的请求和网关在路由之前就拒绝的请求没有 span。
- 只导出 trace；GPU、引擎等聚合指标仍在平台监控里。
- 控制台的查找有时间窗（最长 24 小时，记录保留 7 天）；请求很多时正常请求按比例保存，未保存的在控制台查不到。
  Trace 导出独立采样；只有被选中且成功导出的请求才可在追踪系统查询，不能据此保证所有请求完整保留。

## 附录：span 字段

span 的类型是 SERVER，名字是 `chat <模型名>`、`text_completion <模型名>`、`embeddings <模型名>`，其他接口是路径加模型名；开始于网关收到请求，结束于回复结束。全部内容都来自请求视图用的那条记录（[ADR 0042](../adr/0042-gateway-request-records.md)），所以和控制台看到的一致。

时间点、阶段的定义与拆分算法见[请求时间线与阶段拆分](../reference/request-timeline.md)。属性名按三条规则取：OTel GenAI / HTTP 语义约定里有的，用约定的名字；约定没有而 vLLM 自己的 span（`llm_request`）有的，用 vLLM 的名字和单位（秒），对着 vLLM 写的查询可以直接用；两边都没有的才用 `gpuworks.*`。没观测到的值不写。

**OTel 语义约定**

| 属性 | 含义 | vLLM 的 span 上 |
|---|---|---|
| `gen_ai.operation.name`、`gen_ai.request.model` | 操作（`chat` / `text_completion` / `embeddings`）与客户端请求的模型名 | 无 |
| `gen_ai.request.max_tokens`、`gen_ai.request.temperature`、`gen_ai.request.top_p` | 客户端在请求里设的采样参数（`max_tokens` 取 `max_completion_tokens`，没有时取 `max_tokens`）；没设的不写，引擎会用自己的默认值 | 同名 |
| `gen_ai.request.choice.count` | 客户端设的 `n` | `gen_ai.request.n`（vLLM 自定义名） |
| `gen_ai.request.stream` | 是否流式 | 无 |
| `gen_ai.usage.input_tokens`、`gen_ai.usage.output_tokens` | 引擎报告的输入（含缓存命中）、输出 token | `gen_ai.usage.prompt_tokens`、`completion_tokens`（约定的旧名） |
| `gen_ai.usage.cache_read.input_tokens`、`gen_ai.usage.reasoning.output_tokens` | 缓存命中的输入、思考 token（引擎报了时） | 无 |
| `gen_ai.response.finish_reasons` | 结束原因 | 无 |
| `gen_ai.response.time_to_first_chunk` | 流式回复从网关收到请求到首个 token（含思考），秒 | `gen_ai.latency.time_to_first_token`，但从请求到达引擎算起，不含网关与网络 |
| `url.path`、`http.response.status_code`、`error.type` | 路径、状态码；出错时的类型（状态码，或 `upstream_error` / `truncated`） | 无 |

**沿用 vLLM 的名字（秒）**

| 属性 | 含义 |
|---|---|
| `gen_ai.latency.e2e` | 端到端，等于本 span 时长（从网关收到请求算起） |
| `gen_ai.latency.time_in_queue` | 引擎自报的排队时间 |
| `gen_ai.latency.time_in_model_prefill` | 引擎自报的 Prefill 时间 |
| `gen_ai.latency.time_in_model_decode` | 引擎自报的 Decode 时间 |
| `gen_ai.latency.time_in_model_inference` | Prefill + Decode，两者都报了才有 |

排队、Prefill、Decode 是引擎在回复里自报的原值（vLLM 需要部署带 `--enable-per-request-metrics`，TensorFold、Strata 报 Prefill 与 Decode），不论引擎是哪个都用这几个名字。

**gpuworks 自定义**

| 属性 | 含义 |
|---|---|
| `gpuworks.request.id`、`gpuworks.request.result` | 请求 ID（即响应头 `X-Request-ID`）；结果：`completed`、`length`、`client_closed`、`interrupted`、`error` |
| `gpuworks.deployment.id`、`gpuworks.instance.uuid` | 部署与处理它的副本 |
| `gpuworks.api_key.id`、`gpuworks.user.id`、`gpuworks.request.source` | 调用方（API key 或控制台用户）与入口（`api` / `playground`） |
| `gpuworks.request.body_bytes`、`gpuworks.request.images` | 请求体大小、图片数 |
| `gpuworks.stream.events`、`gpuworks.stream.max_gap_ms` | 流式 chunk 数与最长停顿 |
| `gpuworks.pd.prefill_instance`、`gpuworks.pd.fallback` | PD 分离部署：处理 prefill 的实例；prefill 不可用、由 decode 单独完成时的原因（如 `prefill_5xx`） |
| `gpuworks.phase.<阶段>_ms` | 请求视图的阶段拆分里网关这一侧的部分，毫秒：`gateway`（网关自身）、`pd_prefill`（PD 的 prefill 阶段）、`preprocess`（引擎预处理）、`waiting`（引擎没报排队时的等首字）、`thinking`、`answering`、`tail`。它们加上引擎的排队与 Prefill 等于端到端；引擎自报的时间长于网关看到的等待时（两边计时起点不同），请求视图按比例收进等待里，这里给的引擎值是原值 |

### PD 分离部署的 prefill 子 span

**顺序交接（`vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake`）**：请求多一个 CLIENT 类型的子 span `pd.prefill`，从网关把请求发给 prefill 实例计到拿到回复。

- 子 span 的属性有 `gpuworks.pd.prefill_instance`、`gpuworks.pd.fallback`，以及 prefill 实例自报的 `gen_ai.latency.time_in_queue`、`gen_ai.latency.time_in_model_prefill`。
- 发给 prefill 实例的 `traceparent` 以该子 span 为父，发给 decode 实例的以网关 span 为父；两个引擎各自导出 span 时分别挂在这两处。
- 父 span 上的 `gen_ai.latency.*` 是 decode 实例的自报值：排队含等远端 KV 的时间，Prefill 只是重算最后一个 token，见[请求时间线](../reference/request-timeline.md#6-pd-分离部署)。

**并发交接（`sglang-nixl`）**：网关不等 prefill，没有子 span，prefill 副本的请求以网关 span 为父。

### 时间线事件与资源属性

时间线是 span event：`gpuworks.dispatch`（交给副本）、`gpuworks.first_byte`、`gpuworks.first_reasoning`、`gpuworks.first_answer`、`gpuworks.last_chunk`。

资源属性是 `service.name=gpuworks`、`service.version`、`service.instance.id`（控制面 ID）与 `gpuworks.control_plane.name`。

### 判断 span 状态

只有平台侧出错（`error`、`interrupted`）的 span 状态是 Error；客户端主动断开、长度截断不算。

**不导出**提示词与回复内容，平台本身也不保存它们。

