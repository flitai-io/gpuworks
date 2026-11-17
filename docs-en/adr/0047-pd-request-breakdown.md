# ADR 0047: The Gateway Measures and Separately Reports the Prefill Phase of PD Requests; KV Transfer Has Aggregate Statistics Only

Date: 2026-10-15 · Status: Accepted (revises decision 3 of [ADR 0046](0046-gateway-request-traces.md): prefill requests in sequential handoffs use the `pd.prefill` span as their parent)

Basis: [PD Request Phase Breakdown Plan](../../artifacts/work/2026-10-15-pd-request-breakdown/plan.md).

## Context

In a sequential handoff for a PD disaggregated deployment ([ADR 0028](0028-pd-disaggregated-deployment.md)), the gateway first sends the request (`max_tokens=1`) to a prefill instance and passes it to a decode instance only after receiving the KV parameters. The original breakdown counted this wait as “gateway” time: in the vllm-nixl deployment on gpu3, the “gateway” segment lasted hundreds of milliseconds, of which the gateway itself took about 21 ms and the rest was waiting for the prefill instance. The meaning of decode-instance-reported values also changes under PD: the vLLM scheduler keeps the request in the waiting queue while waiting for remote KV (`WAITING_FOR_REMOTE_KVS`), so its reported queueing time includes KV transfer; its reported Prefill is only the recomputation of the last prompt token.

Available data (verified on 2026-10-15):

- Neither engine exports per-request KV transfer time. vLLM's NIXL connector has only aggregate `vllm:nixl_*` metrics (llm-d's PD monitoring also uses only these and notes that they do not correspond to individual requests). `MooncakeConnectorV1` in vllm-ascend 0.27.1rc1 has no metrics at all; it only writes `KV cache transfer for request <id> took X ms` to decode logs.
- The prefill instance's one-token response also contains per-request statistics (`--enable-per-request-metrics`), read in the same way as the decode response.
- vllm-ascend uses the vLLM scheduler, so KV waiting is likewise counted as queueing.

## Decision

1. **Measure the prefill phase at the gateway and store it as a request-record fact**: `uxrecord.Request.PD` (recorded by `proxy/pd_stage.go`) holds the prefill instance, the time the gateway waits for it (including switching instances after failure), the prefill instance's reported queueing and Prefill times, and the fallback reason. Persist these in 5 appended columns in the `ux.db` request tables, and add `pd_prefill_ms` to the `lat_` aggregate tables. Use the same mechanism as `trace_id`: defaults, columns added on open, no `user_version` bump, and safe rollback. With concurrent handoff (`sglang-nixl`), the gateway does not wait for prefill, so the wait is unknown; record only the instance and fallback.
2. **Keep all breakdown logic in `uxrecord.SegmentsOf`**: add the segment `pd_prefill = min(pd.prefill_ms, dispatch)` immediately after `gateway`, with `gateway = dispatch − pd_prefill`. The segments still sum to the end-to-end duration, keeping the latency breakdown, request color bars, and OTel `gpuworks.phase.*` automatically consistent.
3. **Keep decode-reported values unchanged; change only their labels**: in the UI for PD deployments, label `queue` as “KV transfer & queue” and `prefill` as “First-token recompute.” The API and storage retain the same fields. Do not subtract a “KV transfer” portion from queueing—there is no per-request value to subtract.
4. **Provide only aggregate KV transfer statistics**: the PD card on the service page computes mean / P95 transfer time, mean size, throughput, and failures from the decode instances' collected `nixl_*` values; for expirations, use the prefill instances' counters (vLLM records `nixl_num_kv_expired_reqs` only on the prefill side). Whether the engine for a PD mode exports these metrics is determined solely by `pdmode.Mode.TransferBytesMetric`. If it does not, the API returns `kv_transfer.exposed=false`, and the UI says “Not provided by the engine” instead of showing a row of dashes. Do not parse vllm-ascend logs or patch its metrics.
5. **Include requests in the prefill phase in the in-flight table**: in sequential handoffs, register requests as in flight as soon as waiting for prefill begins (except health probes). At this point, `dispatch_ms` is unknown, and the `pd_prefill` segment extends to the present. Remove requests immediately if prefill fails and `fail` returns 503.
6. **Use the prefill instance's cache hit count, including for the client** (added 2026-10-16): the decode instance's usage counts fetched KV as `cached_tokens`, which always equals the entire prompt. In sequential handoffs, the gateway reads the prefill instance's hit count from its response usage (using the same `cacheFigure` reader as for decode responses) and replaces the value in place in the decode response (`proxy/cached_rewrite.go`: pad shorter numbers with spaces to preserve length). The rewrite sits below the gateway's response observer, so the client, request records, gateway counters, and OTel all read the same number. Prefill speed for PD requests uses the prefill instance's reported Prefill time (`uxrecord.Request.UncachedComputeMS`). Do not rewrite on fallback, concurrent handoff, or when the prefill response lacks this value.
7. **OTel**: requests with sequential handoffs get an additional CLIENT child span, `pd.prefill` (its span ID is generated when the phase begins and kept only in the in-memory record). The `traceparent` sent to the prefill instance uses it as the parent. Attributes include the prefill instance, its reported `gen_ai.latency.time_in_queue` / `time_in_model_prefill` (the same names as in the parent span, each referring to its own engine), and the fallback reason. Add `gpuworks.pd.prefill_instance` and `gpuworks.pd.fallback` to the parent span. Concurrent handoffs have no child span, and prefill requests still use the gateway span as their parent.

## Tradeoffs

- **New persisted facts**: the prefill instance, wait duration, reported values, and fallback reason cannot be derived from other data (gateway counters are aggregate only), and historical request details need them, so persist them. Do not persist the span ID (it is used only for export, which is already complete when the request ends).
- **No per-request KV transfer measurements**: this would require either parsing engine logs (available only in vllm-ascend, with version-dependent formats and a need to send decode logs to the gateway) or patching the engine. Both conflict with a “uniform reader for engine-reported data”; aggregates are sufficient to determine whether the connection is a bottleneck.
- **Rewrite client-visible usage**: the difference from connecting directly to a decode instance is intentional. In OpenAI semantics, `cached_tokens` means prompt tokens that do not need recomputation; clients use it to assess cache effectiveness and calculate cache discounts. The decode value is always 100% under PD and is misleading. Do not add a switch: the original value has no useful meaning.
- **Change labels, not values**: maintain one meaning per stored field, with the PD interpretation confined to the presentation layer. The cost is that API consumers must check whether the deployment is PD to know that `engine_queue_ms` includes KV waiting; this is documented in the reference.

## Consequences

- The request view, latency breakdown, request details (the PD section), and exported traces can all distinguish the gateway's own work, the prefill phase, and the decode instance's KV wait.
- API: the request's `pd` object, `pd_prefill` in `segments`, and service snapshot fields `pd.prefill_stage_p95_ms` and `pd.kv_transfer`; see the [API Reference](../reference/api.md).
- Documentation: [Request Timeline and Phase Breakdown](../reference/request-timeline.md#6-pd-分离部署), [PD Disaggregated Deployment](../how-to/pd-disaggregation.md#运行中), [Export Request Traces](../how-to/export-request-traces.md#附录span-字段).

<!-- translation-source-sha256: a80f00ecbff5fdf77b0d486602502b9605f55ba75262b23f8d9491ff2f21565b -->
