# Inference Metric Vocabulary

The platform defines inference metrics only once, in `internal/inferencemetrics`: short names, types, units, and metrics derived from other metrics. An inference template's
[`api.metrics`](inference-catalog-spec.md#apimetrics引擎指标映射) references these metrics by **short name**, and imports are validated against this table. Stored series names use
`gpuworks_infer_<short-name>`; for example, `ttft_seconds` is stored as `gpuworks_infer_ttft_seconds`. Every series carries the `instance`, `deployment`, `engine`, and `node` labels.

For metric sources and how to declare them in templates, see [Declare Engine Metrics for an Inference Template](../how-to/declare-engine-metrics.md). For tradeoffs, see [ADR 0039](../adr/0039-inference-metric-sources.md).

## Types

| Type | Meaning | Mapping Requirements |
|---|---|---|
| `counter` | A cumulative value that only increases; panels calculate its rate over a time window | Multiple engine-side lines with the same name (such as `_total` split by `model_name`) are summed and stored as a single series |
| `gauge` | Current value | — |
| `histogram` | A Prometheus histogram family. Raw buckets are not stored: when collected data is written, bucket deltas between consecutive scrapes are used to calculate three quantile gauges, `<name>{quantile="0.5"\|"0.95"\|"0.99"}`; `<name>_sum` and `<name>_count` are stored separately | Map by family name (without `_bucket`); label selectors and scaling are not allowed |

Units: `requests` for request counts, `tokens` for token counts, `ratio` for a ratio from 0 to 1 (rendered as a percentage in panels), `seconds` for seconds, `bytes` for bytes, and `events` for event counts.

## Metrics

<!-- Tests in internal/inferencemetrics compare this table against the code. Update it when adding or removing metrics. -->

| Short Name | Type | Unit | Meaning |
|---|---|---|---|
| `decode_time_seconds` | histogram | seconds | Time a request spends in the decode stage |
| `e2e_seconds` | histogram | seconds | End-to-end request duration, as measured by the engine |
| `generation_tokens_total` | counter | tokens | Cumulative generated (output) tokens |
| `gpu_cache_usage_percent` | gauge | ratio | KV cache utilization; the name retains its historical spelling, but the stored value is a ratio from 0 to 1. Derived from capacity and free space when the engine only reports those values; see below |
| `kv_capacity_tokens` | gauge | tokens | KV pool capacity in tokens |
| `kv_disk_hits_total` | counter | events | KV offload disk-tier hits |
| `kv_disk_queries_total` | counter | events | KV offload disk-tier queries |
| `kv_disk_write_bytes_total` | counter | bytes | Bytes written to disk by KV offload |
| `kv_disk_write_failures_total` | counter | events | KV offload disk-write failures |
| `kv_free_tokens` | gauge | tokens | Free KV pool capacity in tokens |
| `kv_hit_rate_instant` | gauge | ratio | Instantaneous prefix cache hit rate reported directly by the engine |
| `kv_hits_total` | counter | tokens | Cumulative prefix cache hit tokens |
| `kv_memory_hits_total` | counter | events | KV offload memory-tier hits |
| `kv_memory_queries_total` | counter | events | KV offload memory-tier queries |
| `kv_queries_total` | counter | tokens | Cumulative prefix cache query tokens |
| `nixl_failed_transfers_total` | counter | events | Failed NIXL transfers in PD disaggregation |
| `nixl_kv_expired_total` | counter | events | Expired KV requests in PD disaggregation |
| `nixl_transfer_bytes` | histogram | bytes | Bytes per NIXL transfer in PD disaggregation |
| `nixl_transfer_seconds` | histogram | seconds | Duration of a single NIXL transfer in PD disaggregation |
| `preemptions_total` | counter | events | Request preemptions |
| `prefill_time_seconds` | histogram | seconds | Time a request spends in the prefill stage |
| `prompt_tokens_cache_hit_total` | counter | tokens | Cumulative input tokens that hit the local cache |
| `prompt_tokens_external_total` | counter | tokens | Cumulative input tokens obtained through external KV transfers |
| `prompt_tokens_local_compute_total` | counter | tokens | Cumulative input tokens computed locally |
| `prompt_tokens_total` | counter | tokens | Cumulative input tokens |
| `queue_time_seconds` | histogram | seconds | Time requests spend in the queue |
| `requests_running` | gauge | requests | Requests currently executing |
| `requests_success_total` | counter | requests | Cumulative successfully completed requests |
| `requests_waiting` | gauge | requests | Requests waiting in the queue |
| `spec_accept_length` | gauge | tokens | Average accepted length in speculative decoding |
| `spec_accept_rate` | gauge | ratio | Speculative decoding acceptance rate, when reported directly by the engine |
| `spec_accepted_tokens_total` | counter | tokens | Cumulative draft tokens accepted in speculative decoding |
| `spec_draft_tokens_total` | counter | tokens | Cumulative draft tokens proposed in speculative decoding |
| `tpot_seconds` | histogram | seconds | Time per output token |
| `ttft_seconds` | histogram | seconds | Time to first token, as measured by the engine |

## Gateway Metrics

The gateway measures each request passing through it independently of the engine's `/metrics`, so every deployment has gateway metrics, including engines without `/metrics` (such as Strata).

Stored names use `gpuworks_proxy_<short-name>`, with the labels `deployment` and `node="server"` (`requests_total` also carries `class`). Histograms are likewise converted into quantile gauges per deployment for storage.

<!-- Tests in internal/inferencemetrics compare this table against the code. Update it when adding or removing metrics. -->

| Short Name | Type | Unit | Meaning |
|---|---|---|---|
| `cached_tokens_total` | counter | tokens | Cached input tokens reported by the engine in response `usage` |
| `completion_tokens_total` | counter | tokens | Output tokens reported by the engine in response `usage` |
| `prompt_tokens_total` | counter | tokens | Input tokens reported by the engine in response `usage` |
| `request_duration_ms` | histogram | milliseconds | Time from handing the request to the instance until the last response byte |
| `requests_inflight` | gauge | requests | Requests the gateway is currently forwarding |
| `reported_decode_ms` | histogram | milliseconds | The request's Decode duration reported by the engine in the response (per-request statistic `decode_seconds`), used as a fallback; see below |
| `reported_prefill_ms` | histogram | milliseconds | The request's Prefill duration reported by the engine in the response (`prefill_seconds`), used as a fallback |
| `reported_queue_ms` | histogram | milliseconds | The request's queue duration reported by the engine in the response (`queue_seconds`), used as a fallback |
| `requests_total` | counter | requests | Requests forwarded or rejected by the gateway, grouped by `class` (2xx/4xx/429/5xx/asleep_503) |
| `tpot_ms` | histogram | milliseconds | Gateway-measured TPOT: time from the first to the last generated token in each streaming response, divided by the number of output tokens after the first; used as a fallback |
| `ttft_ms` | histogram | milliseconds | Time from handing the request to the instance until the first generated token in the streaming response |
| `usage_missing_total` | counter | requests | Responses received in full but without `usage`; these are the requests omitted from token counts |
| `wake_hold_wait_ms` | histogram | milliseconds | How long a request the gateway held while its deployment woke waited, from being held until it was served, refused or abandoned by the client; every held request counts once |

### Reading and Counting Token Usage

**Token counts come only from the engine-reported `usage`**, not from counting streaming chunks: a chunk can contain multiple tokens, and the gateway does not estimate token counts.

- The protocol registry (`inferenceapi.Usage`) defines which fields contain `usage` for each operation. Currently, it supports `openai.chat`, `openai.completions`, `openai.embeddings`, and `systemone.decide`. A decision generates no text and records only `usage.input_tokens`: the `output_tokens` the engine replies with counts answered questions, not generated tokens, so it is not counted as output tokens.
- Completion streams carry text in a choice's `text` field (there is no `delta`), which is used for first-token timing.
- The gateway reads only the final 4 KiB of a response; usage must be in the response's last event or JSON object.
- Usage is not read from compressed responses (non-identity `Content-Encoding`) or responses that are neither JSON nor SSE.

**The gateway automatically requests usage only for vLLM / SGLang streaming text requests**: the endpoint is `/v1/chat/completions` or `/v1/completions`, the request body is JSON the gateway can read, `stream: true` is set, and the client has not specified `stream_options`.

When these conditions are met, the gateway adds `include_usage` on behalf of the client, then removes the extra usage chunk the engine sends as a result. The client receives the same bytes it would receive without this addition ([ADR 0042](../adr/0042-gateway-request-records.md)). Requests for which the client specifies `stream_options` are forwarded unchanged.

This request parameter is not added for callable engines. TensorFold and Strata always include usage. Other responses without usage count toward `usage_missing_total`.

### When a Response Counts as Complete

Responses truncated by a client disconnect count toward neither tokens nor `usage_missing_total`.

If the last response event (`finish_reason`, along with `usage` in the same event or after it) has already reached the gateway, this is not considered an interrupted response: if the client closes the connection after receiving the last event and leaves only `data: [DONE]` unread, the response is still complete. Tokens are counted normally, and the request record also records a normal completion.

### Cache Hit Counts

Cache hit counts come from `usage.prompt_tokens_details.cached_tokens`. When the field is absent, the following rules apply:

- When SGLang has cache reporting enabled (`--enable-cache-report`), it omits `prompt_tokens_details` when there are 0 hits; for these deployments, the gateway reads an absent value as 0.
- When vLLM has `--enable-prompt-tokens-details` enabled, it includes the field even when there are 0 hits.
- In all other cases, an absent field means unknown; the request is excluded from the "Uncached input × TTFT" distribution.

### How Gateway TTFT Is Measured

**Gateway TTFT (time to first token) differs from the engine's `ttft_seconds` measurement**: the gateway starts timing when it hands the request to the instance, including queue and network time.

The first token is the first nonempty `content`, `reasoning_content`, `reasoning`, or `tool_calls` in a streaming response. The gateway reads the entire stream line by line as SSE; individual lines larger than 64 KiB are excluded. TTFT is not recorded for non-streaming responses.

### Fallback Statistics for Missing Engine Metrics

`tpot_ms` and the three `reported_*_ms` metrics supplement the engine's `tpot_seconds`, `queue_time_seconds`, `prefill_time_seconds`, and `decode_time_seconds`, respectively. **The gateway records a fallback histogram only when the engine's built-in mapping lacks the corresponding metric** (`inferencemetrics.GatewayFallbacksFor`):

- vLLM exports all four itself; the gateway records none of them.
- SGLang exports TPOT and queue time, so the gateway records only Prefill and Decode. Currently, SGLang responses do not include per-request statistics, so these two are empty in practice.
- For callable engines (TensorFold, Strata, and others), the gateway records all four.

`tpot_ms` requires a streaming response with `usage` and at least 2 output tokens. It measures from the first generated token observed by the gateway to the last text event, including network jitter. When the engine emits multiple tokens at once (speculative decoding), the time is still averaged by token count.

`reported_*_ms` aggregates engine-reported values in responses ([per-request statistics](#逐请求统计)) into per-deployment distributions. If a response does not include a value, nothing is recorded.

## Per-Request Statistics

Per-request statistics are timings the engine reports for each request in its response. When the response ends, the gateway reads them and writes them into the request record (`ux.db`; see [View Metrics and Search Logs](../how-to/monitor-and-logs.md#2-看请求视图与请求分布)). These statistics serve two purposes:

- The request view uses them to break completed requests into queue, Prefill, and Decode stages.
- When the engine does not export stage histograms itself, the gateway aggregates them into `reported_*_ms` histograms for the "Stage duration" chart.

The fields read are determined by the engine's built-in declarations or the template's [`api.request_stats`](inference-catalog-spec.md#apirequest_stats回复里的逐请求统计).

| Short Name | Unit | Meaning |
|---|---|---|
| `queue_seconds` | seconds | Time spent waiting for scheduling after the request reaches the engine |
| `prefill_seconds` | seconds | Time from being scheduled to producing the first token |
| `decode_seconds` | seconds | Time from the first token to the last token |

Starting with vLLM 0.25.0, `--enable-per-request-metrics` enables all three. TensorFold and Strata report Prefill and Decode, but not queue time.

## Derived Metrics

After each scrape, the agent calculates metrics not directly reported by the engine according to the following table. If the engine reports the target metric itself, the engine's value takes precedence.

| Target | Inputs | Calculation |
|---|---|---|
| `gpu_cache_usage_percent` | `kv_capacity_tokens`, `kv_free_tokens` | `(capacity − free) / capacity`; no value is produced if capacity ≤ 0 or free capacity is outside `[0, capacity]` |

### Queued Request Counts and Per-Request Queue Duration

`requests_waiting` (for example, Strata's `/metrics` field `live.queued`) counts queued requests at the sampling instant.
It is not a millisecond duration. Neither this gauge nor a service histogram can populate an individual request's `engine_queue_ms`.
That field comes only from `queue_seconds` reported in the same response. Missing fields and explicit `null` mean unknown;
an explicit `0` means measured zero queue time. If queue timing is available without Prefill timing, retain the known queue
segment and leave the remainder as unsplit waiting.

Unmodified Strata 0.1.38 standard responses contain only `timings.prompt_ms` and `timings.predicted_ms`, so queue duration remains unknown.
Waiting may also include preprocessing and network time; renaming all waiting to engine queueing would be inaccurate.
The updated built-in Strata catalog carries a source-hash-checked runtime patch that reports initial-generation admission
waiting as `timings.queue_ms` in the same response. It excludes model loading and later MCP rounds without enabling body
monitoring. Update the deployment and recreate old instances to emit the field. For pinned source evidence and integration
constraints, see [Observability Limits of Per-Request Engine Queue Time](../dev/engine-queue-observation.md).
