# Request Timeline and Phase Breakdown

The gateway records each forwarded `/v1` and Playground request ([ADR 0042](../adr/0042-gateway-request-records.md)). The request view, request details, end-to-end latency breakdown, distribution cards, and exported OTel spans (OpenTelemetry trace spans, [ADR 0046](../adr/0046-gateway-request-traces.md)) are all derived from this record.

This page describes the timestamps, phase breakdown, and aggregation rules. For field names across views, see [Section 5](#5-各处的名字对照). The breakdown rules have a single implementation: `SegmentsOf` in `internal/server/uxrecord/segments.go` (`LiveSegments` for requests in progress). Aggregation is implemented in `latency.go` in the same directory.

## 1. Recorded Timestamps

Except for `received_ms`, which is an absolute Unix timestamp in milliseconds, all timestamps below are milliseconds relative to **the gateway receiving the request**. Events that do not occur or are not observed are recorded as unknown (`null` in the API; the OTel attribute is omitted).

| Timestamp | Record Field | Value |
|---|---|---|
| Received | `received_ms` (absolute Unix timestamp in milliseconds) | Captured during authentication at the gateway entry point (API key validation for `/v1`, or the Playground entry point); the request body is read to find the model name afterward |
| Dispatched to replica | `dispatch_ms` | After selecting a replica, reading and, when needed, rewriting the request body, but before handing it off for forwarding. With sequential PD handoff, the prefill phase completes before this point; it is unknown while an in-flight request is still in the prefill phase |
| First byte | `first_byte_ms` | Upstream response headers are received |
| First reasoning | `first_think_ms` | The first streaming event with nonempty `reasoning_content` / `reasoning` |
| First answer | `first_answer_ms` | The first streaming event with nonempty answer text (`content`) or nonempty `tool_calls` |
| Last chunk | `last_chunk_ms` | The last streaming event containing text; finish chunks, usage chunks, and `[DONE]` do not count |
| End | `end_ms` (end-to-end duration) | Response copying finishes (or the client leaves or the connection breaks) |

A streaming event's timestamp is when the gateway **reads** those bytes from upstream (multiple events in the same read share a timestamp). It includes the network between the engine and gateway, but not between the gateway and client. Only streaming responses have first reasoning, first answer, and last chunk timestamps; non-streaming responses have only dispatch, first byte, and end timestamps.

The record also includes three durations **reported by the engine itself** in the response ([Per-Request Statistics](inference-metrics.md#逐请求统计)): `engine_queue_ms` (queue), `engine_prefill_ms` (Prefill), and `engine_decode_ms` (Decode). vLLM requires `--enable-per-request-metrics`; TensorFold and Strata report Prefill and Decode, but not queue time; most other engines do not report them.

Requests to PD disaggregated deployments also record a `pd` object ([Section 6](#6-pd-分离部署)): the prefill instance, how long the gateway waits for it (`prefill_ms`), queue and Prefill durations reported by the prefill instance, and the fallback reason.

The **first token** is the earlier of first reasoning and first answer. This defines “First token” in the request view, the TTFT distribution, and OTel `gen_ai.response.time_to_first_chunk`.

## 2. Phases

| Phase | Meaning |
|---|---|
| `gateway` Gateway | Received → dispatched to replica, minus the PD prefill phase: reading the request body, finding the model, selecting a replica, injecting the usage request, and rewriting the request body passed to decode |
| `pd_prefill` PD Prefill | The time the gateway waits for the prefill instance during sequential PD handoff: from sending the request (`max_tokens=1`) to the prefill instance until receiving its response (including retries with another instance after failure) |
| `preprocess` Engine preprocessing | The wait before the first token that is not covered by the engine's reported queue and Prefill durations: work before entering the engine's scheduling queue (template rendering, tokenization, waiting for the current step to finish), plus the network time for the first token to return to the gateway |
| `queue` Engine queue | Queue time reported by the engine |
| `prefill` Prefill | Prefill time reported by the engine |
| `waiting` Waiting for first token | When the engine does not report queue time, the inseparable wait before the first token (queueing, preprocessing, and network combined; engines that report only Prefill have Prefill listed separately) |
| `thinking` Thinking | First reasoning → first answer (or last chunk if there is no answer) |
| `answering` Answer | First answer → last chunk |
| `tail` Engine postprocessing | Last chunk → end: finish chunks, usage chunks, `[DONE]`, and the gateway writing to the client (including a slow-reading client) |

## 3. Breakdown Algorithm

Let `end` be the end-to-end duration (`end_ms`, or `last_chunk_ms` if unknown), `dispatch` be dispatch to replica (no greater than `end`), and `first` be the first token.

**General case:**

1. `pd_prefill = min(pd.prefill_ms, dispatch)` (0 for non-PD requests or concurrent handoff), and `gateway = dispatch − pd_prefill`. The order is Gateway → PD Prefill.
2. The wait is `wait = first − dispatch` (until `end` if there is no first token).
3. When the engine reports Prefill:
   - Take `queue` (0 if not reported) and `prefill`. If `queue + prefill > wait` (the engine and gateway start timing at different points, so the engine's durations can exceed the wait observed by the gateway), **scale both down proportionally** to fill `wait` exactly.
   - If the engine reports queue time: `preprocess = wait − queue − prefill`, in the order Gateway → Engine preprocessing → Engine queue → Prefill.
   - If it does not report queue time (TensorFold, Strata): `waiting = wait − prefill`, in the order Gateway → Waiting for first token → Prefill.
4. If the engine does not report Prefill: the entire interval is `waiting = wait`.
5. When there is a first token, set `last` to `max(first, last_chunk_ms)`:
   - With reasoning: `thinking = (has_answer ? max(first_answer, first_think) : last) − first_think`, where `has_answer` means that an answer token is present;
   - With an answer: `answering = last − first_answer`;
   - `tail = end − last`.

**When there is no first token (a non-streaming response or a stream with no text), and the engine reports all three durations—queue, Prefill, and Decode**, the time after dispatch to the replica is split according to the durations reported by the engine:

- `gateway` and `pd_prefill` follow step 1.
- `queue`, `prefill`, and `answering = decode` appear in that order; if their sum exceeds `end − dispatch`, they are also scaled down proportionally.
- The remaining time is `preprocess`.

Phases with a duration of 0 are omitted. The sum of all phases equals the end-to-end duration.

Requests in progress (in-flight requests) use the same rules to break down the time elapsed so far. Because the engine's reported values are unavailable until the request ends, the time before the first token is always a single “Waiting for first token” phase. For a PD request still in its prefill phase, `pd_prefill` runs from the start of that phase to the current time.

## 4. Aggregation: End-to-End Latency Breakdown and Speed

“End-to-end latency breakdown” on the service monitoring page accumulates data per deployment in 5-minute buckets, counting only **complete responses with an end-to-end duration greater than 0** (a result of `completed` or `length`; client disconnects, broken connections, and errors are excluded):

- Each phase's mean = sum of that phase's durations ÷ number of complete responses (responses without the phase contribute 0). The phase means therefore sum to the end-to-end mean.
- p50 / p95 come from a quarter-octave bucketed histogram (using the geometric midpoint of each bucket), with approximately 10% error. A phase's percentiles include only responses that have that phase.
- Speed is calculated as **total tokens for eligible requests ÷ sum of the corresponding durations**. Durations are converted to seconds, giving a unit of token/s. Each metric includes only responses with `usage` and with both its numerator token count and denominator duration greater than 0. Responses without `usage` can still count toward latency statistics, but not speed.
  The numerator, denominator, and additional conditions for each metric are:
  - Prefill: uncached input tokens ÷ engine Prefill time; the cache hit count must be known. Sequential PD handoff uses Prefill reported by the prefill instance; see [Section 6](#6-pd-分离部署).
  - Thinking: reasoning tokens ÷ thinking phase; the reasoning token count must be known.
  - Answer: (output − reasoning) tokens ÷ answer phase. If usage does not include a reasoning token count, only responses without a thinking phase count, and the numerator is all output tokens.
  - Decode: the output token count must exceed 1. If the Decode duration reported by the engine is greater than 0, use output tokens ÷ engine Decode time. Otherwise, only when the first token timestamp is known and the last chunk is later than the first token, use (output − 1) ÷ (last chunk − first token).
- Aggregation includes all requests and is unaffected by request log sampling.

## 5. Field Name Mapping

| Meaning | Request View / API | OTel Span Attributes |
|---|---|---|
| Received timestamp, end-to-end duration | `received_ms`, `end_ms` | Span start time and duration; `gen_ai.latency.e2e` (seconds) |
| Dispatched to replica, first byte, first reasoning, first answer, last chunk | `dispatch_ms`, `first_byte_ms`, `first_think_ms`, `first_answer_ms`, `last_chunk_ms` | Events `gpuworks.dispatch`, `gpuworks.first_byte`, `gpuworks.first_reasoning`, `gpuworks.first_answer`, `gpuworks.last_chunk` |
| First token | The earlier of `first_think_ms` and `first_answer_ms` | `gen_ai.response.time_to_first_chunk` (seconds) |
| Engine-reported queue, Prefill, Decode (original values) | `engine_queue_ms`, `engine_prefill_ms`, `engine_decode_ms` | `gen_ai.latency.time_in_queue`, `time_in_model_prefill`, `time_in_model_decode` (seconds); Prefill + Decode is `time_in_model_inference` |
| Phases (after breakdown) | `segments[]`: `{kind, ms}` | Gateway-side phases use `gpuworks.phase.<kind>_ms`; `queue` and `prefill` use the original engine values in the previous row and differ from `segments` only when the proportional scaling in Section 3 occurs |
| Streaming chunk count, longest pause | `events`, `max_gap_ms` | `gpuworks.stream.events`, `gpuworks.stream.max_gap_ms` |
| PD prefill instance, fallback reason | `pd.prefill_instance_uuid`, `pd.fallback` | `gpuworks.pd.prefill_instance`, `gpuworks.pd.fallback` (on both the parent span and the `pd.prefill` child span) |
| PD prefill phase | `pd.prefill_ms`; `pd_prefill` in `segments[]` | Duration of the `pd.prefill` child span |
| Queue and Prefill reported by the prefill instance | `pd.prefill_queue_ms`, `pd.prefill_compute_ms` | `gen_ai.latency.time_in_queue`, `time_in_model_prefill` (seconds) on the `pd.prefill` child span |

For the remaining span attributes, see [Export Request Traces to OpenTelemetry](../how-to/export-request-traces.md#附录span-字段). For API fields, see [REST API](api.md).

## 6. PD Disaggregated Deployments

Requests to PD disaggregated deployments ([ADR 0028](../adr/0028-pd-disaggregated-deployment.md)) pass through two groups of instances in sequence. Their records and breakdowns add a phase to the rules above ([ADR 0047](../adr/0047-pd-request-breakdown.md)):

| Handoff Method (PD Mode) | Prefill Phase | Record |
|---|---|---|
| Sequential (`vllm-nixl`, `vllm-ascend-mooncake`, `vllm-mooncake`) | The gateway first sends the request to the prefill instance and waits for KV parameters (for `vllm-mooncake`, generated by the gateway itself) before handing it to decode | `pd.prefill_ms` is the wait measured by the gateway; per-request statistics from the prefill instance's response (read in the same way as the decode response) are recorded as `prefill_queue_ms`, `prefill_compute_ms` |
| Concurrent (`sglang-nixl`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`) | Prefill and decode receive the request at the same time; the gateway does not wait | `prefill_ms` is unknown and there is no `pd_prefill` phase; only the prefill instance and fallback reason are recorded |

With sequential handoff, cache hits and Prefill speed both use data from the prefill instance:

- The request's cache hits (`cached_tokens`) use the value in the prefill instance's response, and the usage received by the client contains the same value (see [OpenAI-Compatible Gateway](openai-gateway.md#用量usage)). This is because the decode instance counts pulled KV as cache hits, so its value always equals the entire prompt.
- Prefill speed uses Prefill reported by the prefill instance (`prefill_compute_ms`), not decode's first-token recomputation.

When the prefill instance is unavailable and decode handles the request alone (`fallback: decode`), `pd.fallback` records the reason (`prefill_unreachable`, `prefill_5xx`, `prefill_no_kv_params`, `prefill_no_bootstrap`, etc., the same reasons used by the PD card), and `prefill_instance_uuid` is empty. `prefill_ms` still records how long the gateway waited before giving up.

**The values reported by the decode instance mean something different under PD**. The UI uses different names, but the breakdown rules stay the same:

- `queue` is displayed as “KV transfer & queue”: the vLLM scheduler keeps requests in the waiting queue while waiting for remote KV (`WAITING_FOR_REMOTE_KVS`), so the queue duration reported by decode includes the time to pull KV from the prefill instance. vllm-ascend uses the same scheduler and behaves the same way.
- `prefill` is displayed as “First-token recompute”: KV has already been transferred, and decode recomputes only the last prompt token.

Engines do not export per-request KV transfer durations (vLLM's NIXL connector provides only aggregate metrics, and vllm-ascend's Mooncake connector writes only a line in the decode log), so there is no separate phase. Aggregate transfer statistics appear in the PD card on the service monitoring page (see [PD Disaggregated Deployments](../how-to/pd-disaggregation.md#运行中)).

<!-- translation-source-sha256: ab22d664427675989fde76600f9d720254dada5cf1765260c861cf0e6014084c -->
