# 推理指标词表

平台的推理指标只在 `internal/inferencemetrics` 定义一次：短名、类型、单位，以及由其他指标派生出的指标。推理模板的
[`api.metrics`](inference-catalog-spec.md#apimetrics引擎指标映射) 用**短名**指向这里的指标，导入时按本表校验；存储的序列名是
`gpuworks_infer_<短名>`，例如 `ttft_seconds` 存为 `gpuworks_infer_ttft_seconds`。每条序列都带 `instance`、`deployment`、`engine`、`node` 标签。

指标从哪里来、模板怎么声明，见[为推理模板声明引擎指标](../how-to/declare-engine-metrics.md)；取舍见 [ADR 0039](../adr/0039-inference-metric-sources.md)。

## 类型

| 类型 | 含义 | 映射要求 |
|---|---|---|
| `counter` | 只增的累计值，面板按时间窗求速率 | 引擎侧同名的多行（如按 `model_name` 分开的 `_total`）相加后存一条 |
| `gauge` | 当前值 | — |
| `histogram` | Prometheus 直方图族。原始桶不落库：采集写入时，按相邻两次的桶增量算出 `<名>{quantile="0.5"\|"0.95"\|"0.99"}` 三条分位数 gauge，另存 `<名>_sum`、`<名>_count` | 用族名（不带 `_bucket`）映射，不能带标签选择器、不能换算 |

单位：`requests` 请求数，`tokens` token 数，`ratio` 0～1 的比例（面板渲染成百分比），`seconds` 秒，`bytes` 字节，`events` 次数。

## 指标

<!-- 本表由 internal/inferencemetrics 的测试与代码比对，增删指标时同步修改。 -->

| 短名 | 类型 | 单位 | 含义 |
|---|---|---|---|
| `decode_time_seconds` | histogram | seconds | 请求在 decode 阶段的耗时 |
| `e2e_seconds` | histogram | seconds | 请求端到端耗时（引擎口径） |
| `generation_tokens_total` | counter | tokens | 生成（输出）token 累计 |
| `gpu_cache_usage_percent` | gauge | ratio | KV cache 使用率；名字沿用历史拼写，存的是 0～1 的比例。引擎只报容量和空闲量时由它们派生，见下 |
| `kv_capacity_tokens` | gauge | tokens | KV 池容量（token） |
| `kv_disk_hits_total` | counter | events | KV 卸载磁盘层命中次数 |
| `kv_disk_queries_total` | counter | events | KV 卸载磁盘层查询次数 |
| `kv_disk_write_bytes_total` | counter | bytes | KV 卸载写入磁盘的字节 |
| `kv_disk_write_failures_total` | counter | events | KV 卸载写磁盘失败次数 |
| `kv_free_tokens` | gauge | tokens | KV 池空闲量（token） |
| `kv_hit_rate_instant` | gauge | ratio | 引擎直接报告的瞬时前缀缓存命中率 |
| `kv_hits_total` | counter | tokens | 前缀缓存命中 token 累计 |
| `kv_memory_hits_total` | counter | events | KV 卸载内存层命中次数 |
| `kv_memory_queries_total` | counter | events | KV 卸载内存层查询次数 |
| `kv_queries_total` | counter | tokens | 前缀缓存查询 token 累计 |
| `nixl_failed_transfers_total` | counter | events | PD 分离 NIXL 传输失败次数 |
| `nixl_kv_expired_total` | counter | events | PD 分离中过期的 KV 请求数 |
| `nixl_transfer_bytes` | histogram | bytes | PD 分离单次 NIXL 传输字节 |
| `nixl_transfer_seconds` | histogram | seconds | PD 分离单次 NIXL 传输耗时 |
| `preemptions_total` | counter | events | 请求被抢占次数 |
| `prefill_time_seconds` | histogram | seconds | 请求在 prefill 阶段的耗时 |
| `prompt_tokens_cache_hit_total` | counter | tokens | 输入 token 中命中本地缓存的累计 |
| `prompt_tokens_external_total` | counter | tokens | 输入 token 中由外部 KV 传输得到的累计 |
| `prompt_tokens_local_compute_total` | counter | tokens | 输入 token 中本地计算的累计 |
| `prompt_tokens_total` | counter | tokens | 输入 token 累计 |
| `queue_time_seconds` | histogram | seconds | 请求排队耗时 |
| `requests_running` | gauge | requests | 正在执行的请求数 |
| `requests_success_total` | counter | requests | 成功完成的请求累计 |
| `requests_waiting` | gauge | requests | 排队中的请求数 |
| `spec_accept_length` | gauge | tokens | 投机解码平均接受长度 |
| `spec_accept_rate` | gauge | ratio | 投机解码接受率（引擎直接报告时） |
| `spec_accepted_tokens_total` | counter | tokens | 投机解码被接受的草稿 token 累计 |
| `spec_draft_tokens_total` | counter | tokens | 投机解码提出的草稿 token 累计 |
| `tpot_seconds` | histogram | seconds | 每个输出 token 的耗时 |
| `ttft_seconds` | histogram | seconds | 首 token 耗时（引擎口径） |

## 网关指标

网关对经过它的每个请求自行测量，不依赖引擎的 `/metrics`，所以每个部署都有网关指标，包括没有 `/metrics` 的引擎（如 Strata）。

存储名是 `gpuworks_proxy_<短名>`，标签为 `deployment`、`node="server"`（`requests_total` 另带 `class`）；直方图同样按部署算成分位数 gauge 存储。

<!-- 本表由 internal/inferencemetrics 的测试与代码比对，增删指标时同步修改。 -->

| 短名 | 类型 | 单位 | 含义 |
|---|---|---|---|
| `cached_tokens_total` | counter | tokens | 回复 `usage` 里引擎报告的命中缓存的输入 token |
| `completion_tokens_total` | counter | tokens | 回复 `usage` 里引擎报告的输出 token |
| `prompt_tokens_total` | counter | tokens | 回复 `usage` 里引擎报告的输入 token |
| `request_duration_ms` | histogram | milliseconds | 请求交给实例到回复最后一个字节的耗时 |
| `requests_inflight` | gauge | requests | 网关正在转发的请求数 |
| `reported_decode_ms` | histogram | milliseconds | 引擎在回复里自报的该请求 Decode 耗时（逐请求统计 `decode_seconds`），兜底用，见下 |
| `reported_prefill_ms` | histogram | milliseconds | 引擎在回复里自报的该请求 Prefill 耗时（`prefill_seconds`），兜底用 |
| `reported_queue_ms` | histogram | milliseconds | 引擎在回复里自报的该请求排队耗时（`queue_seconds`），兜底用 |
| `requests_total` | counter | requests | 网关转发或拒绝的请求，按 `class`（2xx/4xx/429/5xx/asleep_503）分 |
| `tpot_ms` | histogram | milliseconds | 网关口径的 TPOT：每个流式回复从第一个到最后一个生成 token 的时间，除以第一个之后的输出 token 数，兜底用 |
| `ttft_ms` | histogram | milliseconds | 请求交给实例到流式回复第一个生成 token 的耗时 |
| `usage_missing_total` | counter | requests | 完整收到、但没有带 `usage` 的回复数；token 计数少算的就是这些请求 |
| `wake_hold_wait_ms` | histogram | milliseconds | 网关为正在唤醒的部署挂起的请求，从被挂起到被放行、被拒绝或客户端离开的等待时间；每个被挂起的请求都计一次 |

### token 用量的读取与计数

**token 只取引擎报告的 `usage`**，不按流式 chunk（数据块）计数：一个 chunk 可能含多个 token，网关也不估算 token 数。

- 哪种操作的 `usage` 在哪些字段，由协议注册表（`inferenceapi.Usage`）定义；目前支持 `openai.chat`、`openai.completions`、`openai.embeddings` 与 `systemone.decide`。决策不生成文本，只记 `usage.input_tokens`：引擎回复的 `output_tokens` 是回答的问题数，不是生成的 token，所以不计入输出 token。
- 补全流的文本在 choice 的 `text` 里（没有 `delta`），首 token 按它计时。
- 网关只读回复末尾 4 KiB，usage 必须在回复的最后一个事件或 JSON 对象里。
- 被压缩（`Content-Encoding` 非 identity）或非 JSON / SSE 的回复不读 usage。

**网关只为 vLLM / SGLang 的流式文本请求自动请求 usage**：接口是 `/v1/chat/completions` 或 `/v1/completions`，请求体是网关可读取的 JSON，设置了 `stream: true`，且客户端没写 `stream_options`。

满足这些条件时，网关替客户端加上 `include_usage`，再把引擎因此多发的 usage 块从回复里去掉，客户端收到的字节与不加时一致（[ADR 0042](../adr/0042-gateway-request-records.md)）。客户端自己写了 `stream_options` 的请求原样转发。

callable 引擎不加这项请求参数；TensorFold、Strata 总是带 usage。其他不带 usage 的回复计入 `usage_missing_total`。

### 什么情况下回复算完整

客户端中途断开、被截断的回复既不计 token，也不计 `usage_missing_total`。

如果回复的最后一个事件（`finish_reason`，以及与它同一事件或在它之后的 `usage`）已经到达网关，则不算中途断开：客户端拿到最后一个事件就关闭连接、只剩 `data: [DONE]` 没读时，回复仍是完整的，token 照常计入，请求记录的结局也是正常结束。

### 缓存命中数

缓存命中数取 `usage.prompt_tokens_details.cached_tokens`。字段缺失时，按以下条件判断：

- SGLang 开着缓存统计（`--enable-cache-report`）时，命中为 0 就不写 `prompt_tokens_details`；网关对这样的部署把缺省值读成 0。
- vLLM 开着 `--enable-prompt-tokens-details` 时，命中为 0 也照写。
- 其他情况下，没有这个字段就是未知；请求不进「非缓存输入 × TTFT」分布。

### 网关 TTFT 的统计口径

**网关 TTFT（首 token 耗时）与引擎 `ttft_seconds` 口径不同**：网关从请求交给实例开始计时，包含排队和网络。

首个 token 是流式回复里第一个非空的 `content`、`reasoning_content`、`reasoning` 或 `tool_calls`。网关按 SSE 行读完整个流，超过 64 KiB 的单行不计入。非流式回复不计 TTFT。

### 引擎缺少指标时的补充统计

`tpot_ms` 与三个 `reported_*_ms` 分别补充引擎的 `tpot_seconds`、`queue_time_seconds`、`prefill_time_seconds`、`decode_time_seconds`。**只有引擎的内置映射缺少对应指标时，网关才记录这项兜底直方图**（`inferencemetrics.GatewayFallbacksFor`）：

- vLLM 四项都自己导出，网关一项不记。
- SGLang 导出 TPOT 与排队，网关只记 Prefill、Decode；目前 SGLang 回复里不带逐请求统计，因此这两项实际为空。
- callable 引擎（TensorFold、Strata 等）四项都记。

`tpot_ms` 要求流式回复带 `usage` 且输出至少 2 个 token。它从网关看到的第一个生成 token 算到最后一个文本事件，含网络抖动；引擎一次发多个 token（投机解码）时，也按 token 数平均。

`reported_*_ms` 把回复里引擎自报的数（[逐请求统计](#逐请求统计)）按部署汇总成分布；回复没带就不记。

## 逐请求统计

逐请求统计是引擎在每个回复中报告的该请求耗时。网关在回复结束时读出，写进该请求的记录（`ux.db`，见[查看指标与检索日志](../how-to/monitor-and-logs.md#2-看请求视图与请求分布)）。这些统计有两种用途：

- 请求视图用它把已结束的请求拆成排队、Prefill、Decode。
- 引擎自己不导出分段直方图时，网关把它们汇总成 `reported_*_ms` 直方图，供「分段时长」图使用。

读哪些字段由引擎内置声明或模板的 [`api.request_stats`](inference-catalog-spec.md#apirequest_stats回复里的逐请求统计) 决定。

| 短名 | 单位 | 含义 |
|---|---|---|
| `queue_seconds` | seconds | 请求到达引擎后等待调度的时间 |
| `prefill_seconds` | seconds | 从被调度到产出第一个 token |
| `decode_seconds` | seconds | 从第一个 token 到最后一个 token |

vLLM 0.25.0 起由 `--enable-per-request-metrics` 打开，三项都有；TensorFold 与 Strata 报 Prefill 与 Decode，没有排队。

## 派生指标

agent 每次抓取后，对没有被引擎直接报告的指标按下表计算；引擎自己报了目标指标时以引擎的值为准。

| 目标 | 输入 | 计算 |
|---|---|---|
| `gpu_cache_usage_percent` | `kv_capacity_tokens`、`kv_free_tokens` | `(capacity − free) / capacity`；容量 ≤ 0 或空闲量不在 `[0, capacity]` 时不出值 |

### 排队请求数与逐请求排队耗时

`requests_waiting`（例如 Strata `/metrics` 的 `live.queued`）是采样时刻的排队请求数，
不是毫秒数，也不能用它或服务级直方图给某一个请求补写 `engine_queue_ms`。
请求记录里的该字段只来自同一个回复的 `queue_seconds`；未上报或显式 `null` 均为未知，
显式 `0` 才表示已测得零排队。只有排队耗时而没有 Prefill 耗时时，时间条保留已知的排队，
剩余时间继续标为未拆分的等待。

未经适配的 Strata 0.1.38 标准回复只有 `timings.prompt_ms` 和 `timings.predicted_ms`，因此排队耗时未知，
等待中还可能包含预处理及网络耗时，不能直接把全部等待改名为引擎排队。
更新后的内置 Strata 目录携带经源码哈希校验的运行时补丁，在同一回复中提供首次生成准入前的
`timings.queue_ms`；它不包含模型加载及后续 MCP 轮次排队，也不启用正文监控。
旧实例需更新部署并重建后才会输出该字段。具体源码证据及接入边界见
[逐请求引擎排队耗时的观测边界](../dev/engine-queue-observation.md)。
