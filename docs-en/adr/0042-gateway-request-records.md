# ADR 0042: The Gateway Requests Usage from vLLM / SGLang for Streaming Requests, Strips the Extra Chunks, and Writes Per-Request Records to a Separate ux.db

Date: 2026-10-12 · Status: Accepted (revises D4 of the inference metrics plan: “the gateway does not inject `stream_options` or rewrite any request”)

Basis: [Request-Level User Experience Observability Plan](../../artifacts/work/2026-10-12-request-ux-observability/plan.md) and [Research](../../artifacts/research/2026-10-11-llm-ux-observability/README.md).

## Context

TTFT p95 alone cannot explain the user experience: the same 100-second wait for the first token could be caused by an input of 100,000 tokens, a prefix cache miss, or queueing. To explain it, each request must at least reveal the input length, how much hit the cache, the output length, the thinking duration, and the time spent queueing, in Prefill, and in Decode.

Only the engine knows these facts, and it provides them only in the response:

- For streaming requests, vLLM and SGLang send usage (including `prompt_tokens_details.cached_tokens`) only when the client includes `stream_options.include_usage`. Most clients do not, so the gateway cannot read token counts for these requests; these are what `usage_missing_total` in the inference metrics plan counts. OTel spans in vLLM 0.30, 0.31, and main also lack cache hit counts, and Prometheus scraping provides only aggregates.
- Starting with vLLM 0.25.0, `--enable-per-request-metrics` writes the request's queueing, first-token, and generation durations into the final usage chunk of the response (the `metrics` object). TensorFold and Strata include `tensorfold` and `timings` objects in their final events without requiring the client to ask.

D4 of the inference metrics plan previously decided that the gateway would not inject `stream_options` or rewrite any request.

## Decision

1. **Inject a usage request only for vLLM / SGLang streaming requests**: when an `openai.chat` operation has streaming enabled at the top level of its request body (`"stream": true`) and has no `stream_options`, the gateway inserts `"stream_options":{"include_usage":true}` at the beginning of the object, leaving all other bytes unchanged. If the client supplies `stream_options` itself (regardless of its contents), forward the request unchanged. Do not inject it for callable engines: they may reject unknown fields, and the platform's built-in TensorFold and Strata already always include usage.
2. **Strip the resulting extra chunk so the client receives the same bytes as without injection**: filter injected responses by SSE line, dropping the line whose `choices` is an empty array and whose `usage` is an object, along with the blank line that follows it. SGLang extension chunks can also have empty `choices`, and some versions include `"usage": null` in every chunk, so only an object-valued `usage` qualifies. When usage is requested, vLLM moves `system_fingerprint` from the finish chunk to the usage chunk. The gateway buffers the finish chunk until the next event; if that event is a usage chunk, it restores the fingerprint to the finish chunk. All other bytes pass through unchanged as soon as a complete line is available; lines over 16 KiB pass through directly without buffering. Paired real traffic captures from vLLM 0.30 and SGLang 0.5.21 prove byte-for-byte equivalence (`internal/server/proxy/testdata/vllm-0.30-*.sse`, `sglang-0.5.21-*.sse`), and SGLang requests through the gateway on gpu3 also match the stream returned directly by the engine.
3. **Read engine-reported per-request statistics from responses according to declarations**: the vocabulary is in `internal/inferencemetrics` (`queue_seconds`, `prefill_seconds`, `decode_seconds`); built-in vLLM handling reads `metrics.*`, while other engines declare the fields through template `api.request_stats`. New vLLM deployments include `--enable-per-request-metrics` by default (parameter dictionary key `enable_request_metrics`, visible and disableable in the deployment form), but only for images identified as vLLM 0.25.0 or later by the image catalog or image tag. The `metrics` field remains unchanged for the client because the user confirms this startup parameter at deployment time, just as with `--enable-prompt-tokens-details`.
4. **Write per-request records to a separate `ux.db`**: the gateway produces one `uxrecord.Request` (identity, timeline, usage, engine statistics, outcome) for each forwarded `/v1` and Playground request and passes it to `uxstore` through a nonblocking queue. `ux.db` is a separate SQLite file in the data directory, partitioned by UTC day (three tables: request log, distribution grid, and counters). Expired partitions are removed by dropping entire tables: retain 7 days, and if the file exceeds 2 GB, delete starting with the oldest day. Distribution grids and counters accumulate all requests. When request logs exceed 300 rows per second, retain every abnormal request (non-2xx, disconnect, truncation, or first output ≥ 10 seconds), sample normal requests at a fixed stride, and record how many requests each sample represents in `weight`. Drop and count records when the queue is full; never block forwarding.
5. **Keep in-progress requests only in memory**: use an in-flight table sharded by deployment, with events performing only atomic counter updates. Before a request finishes, combine queueing and Prefill into “waiting for the first token” and do not display Prefill progress.

## Tradeoffs

- **Rewriting request bodies vs leaving bytes unchanged**: relaxing D4 gives most vLLM / SGLang streaming requests usage data, providing coverage for distribution charts and the uncached-input × first-token chart. The cost is an extra request-body scan and insertion copy, plus line-based response filtering. Stripping the extra chunks preserves the response the client sees, so the change remains transparent to clients. Requests where the client asks for usage are not changed by a single byte.
- **Injection is limited to vLLM / SGLang**: whether other engines accept `stream_options` cannot be guaranteed; missing data is preferable to failed requests.
- **A separate file instead of the main database or metrics.db**: request-log write volume grows linearly with traffic, independently of main-database transactions and time-series writes to metrics.db. Dropping whole daily tables enforces retention and capacity limits without row-by-row deletion. Samples are weighted, while counters and grids are not sampled, so distributions and coverage still represent all requests.
- **The gateway cannot measure the boundary between queueing and Prefill**: while a request is in progress, it can provide only a lower bound for “waiting for the first token.” Splitting them requires statistics reported by the engine after the request finishes; engines without those statistics remain unsplit.

## Consequences

- Streaming requests in vLLM / SGLang deployments no longer contribute heavily to `usage_missing_total`; behavior is unchanged when clients request usage themselves.
- Extra gateway overhead per request: one byte scan of the request body, one copy when injecting, one byte match and atomic counter update per event, and parsing the final 4 KiB at completion. The monitoring API reads only one deployment's in-flight table and indexed ranges in `ux.db`.
- Running vLLM deployments do not need to restart: existing deployments lack this parameter in their startup configuration, and recreating them does not add it. Only new deployments include it by default.
- Documentation: [Per-Request Statistics in the Inference Metrics Vocabulary](../reference/inference-metrics.md#逐请求统计), [`api.request_stats` Syntax](../reference/inference-catalog-spec.md#apirequest_stats回复里的逐请求统计), [View Metrics and Search Logs](../how-to/monitor-and-logs.md#2-看请求视图与请求分布).

<!-- translation-source-sha256: c69db0f866cadc08e5cd00d96ae74a5cd5880398ab52f9b3de8339cc53f968ab # ADR 0042: The Gateway Requests Usage from vLLM / SGLang for Streaming Requests, Strips the Extra Chunks, and Writes Per-Request Records to a Separate ux.db -->
<!-- translation-source-sha256: cbcf73d51613e4ccc6b864cad0b348f2080914bbac4befdc1f8914996b8ffd9c ## Context -->
<!-- translation-source-sha256: 9169f050a154455e20ac815a577cc57e8944cc5bdcc0d24ae7ec5104d7f2cd8a ## Decision -->
<!-- translation-source-sha256: fe63dcfa3ad032b08e8e68aa0f47dc894fcee7a83cb678d0290ddc532a4086b8 ## Tradeoffs -->
<!-- translation-source-sha256: 8143ae17c04adc2d22f0c7cbd4596cbc54b7785a45c80f81624d047621393638 ## Consequences -->
