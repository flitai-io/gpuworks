# ADR 0046: The Gateway Creates W3C Trace Context for Every Request, Stores the Trace ID in Request Records, and Exports Per-Request Records over OTLP

Date: 2026-10-15 · Status: Accepted (prefill requests in sequential handoffs in decision 3 now use the `pd.prefill` span as their parent; see [ADR 0047](0047-pd-request-breakdown.md))

Basis: [Request-Level Trace Plan](../../artifacts/archive/2026-10-15-otel-export/plan.md).

## Context

Users want to connect the platform's per-request data to their own monitoring. Their upstream gateways (Higress, Kong, LiteLLM, and others) already emit traces, and they want the platform gateway segment to join the same trace. They also want to move between their external tracing system and the platform's request view.

There are two possible data sources:

- **The engine's own OTel** (vLLM `--otlp-traces-endpoint`): covers only vLLM. Spans include queueing, Prefill, and Decode, but not cache hits, thinking tokens, client disconnects, or caller identity. Engine containers must reach the user's Collector, while nodes can usually initiate connections only to the server. The image must also include the OTel SDK.
- **The gateway's per-request records** (`uxrecord.Request`, [ADR 0042](0042-gateway-request-records.md)): available for every engine, with a superset of vLLM span fields (including engine-reported queueing / Prefill / Decode), and located only on the server.

## Decision

1. **Establish request identity once at the gateway**: `proxy.serveSelectedTarget` (used by all forwarded requests, including health probes) assigns the request ID and gateway span (`internal/server/tracecontext`). If a request carries a valid `traceparent` (W3C processing rules: exactly one, version other than `ff`, lowercase hexadecimal, and neither ID all zeroes), join the caller's trace, use the caller's span as the parent, and preserve its sampled bit and `tracestate`; otherwise, create a new trace.
2. **The first 6 bytes of a gateway-created trace ID are a millisecond timestamp**, followed by 10 random bytes. Set the Trace Context Level 2 random bit (the rightmost 7 bytes are random). This lets request storage locate a request by trace ID without an index.
3. **Requests forwarded to engines use the gateway span as their parent**: replace `traceparent` with the gateway span and leave `tracestate` unchanged. Retries and both the prefill / decode phases of PD are child requests of the gateway span. When users add `--otlp-traces-endpoint` to vLLM themselves, engine spans automatically attach under the gateway span; the platform does not manage this parameter.
4. **Include the trace ID in request records without indexing it**: add `trace_id BLOB NOT NULL DEFAULT x''` to the request tables in `ux.db`. On opening the database, add the column to daily partitions that lack it (the same mechanism used to add `lat_` tables), without changing `user_version`. Older versions can still read and write the same file (safe rollback). Lookup (`GET /api/v1/monitor/requests/lookup`) uses a time window: first list deployments with traffic in that window from the day's counter table, then scan the `(deployment_id, ts, seq)` index and compare IDs row by row. The maximum window is 24 hours. Gateway-created trace IDs embed their time, so they can be located directly without specifying a window; other lookups default to the last hour. Request IDs use the same lookup method.
5. **Export from request records**: after a request finishes, `internal/server/traceexport` constructs one SERVER span solely from that record (the timeline becomes span events; gateway-side phases use `uxrecord.SegmentsOf`, the same breakdown as the request view), then pushes batches to the administrator-configured Collector over OTLP/HTTP JSON + gzip. Do not introduce the OTel SDK or gRPC. Attribute names follow three rules: use OTel GenAI / HTTP semantic convention names where defined (including sampling parameters `gen_ai.request.max_tokens`, `temperature`, `top_p`, `choice.count`, and `gen_ai.request.stream`); where no convention exists but vLLM's `llm_request` span has a field, use vLLM's name and unit (`gen_ai.latency.e2e`, `time_in_queue`, `time_in_model_prefill`, `time_in_model_decode`, `time_in_model_inference`, in seconds, retaining the engine's original reported values); otherwise use `gpuworks.*`. The gateway reads sampling parameters in the same scan of the request body, retaining them only in the in-memory record without persisting them.
6. **Sampling**: traces from callers inherit the caller's sampled bit (ParentBased); for gateway-created traces, compare the low 56 bits of the trace ID with the sampling ratio. Do not sample when export is disabled. Sampling does not affect `ux.db`.
7. **Export does not block forwarding**: use a bounded queue, dropping and counting records when full. A single goroutine sends batches; for 429/502/503/504 and connection failures, retry 3 times using `Retry-After` or exponential backoff; discard other failures. Show status (exported, dropped, failed, most recent error) on the settings page.
8. **One configuration source**: platform setting `telemetry_export` (`store.TelemetryExport`), with header values encrypted using secretbox. Read and write only through `/api/v1/telemetry/export` (the generic settings endpoint neither lists nor accepts it). Changes take effect immediately on save and are applied at server startup. Do not read `OTEL_*` environment variables, avoiding a second source.

## Tradeoffs

- **Construct spans afterward instead of instrumenting the hot path with a tracer**: the gateway adds only one `traceparent` parse and span ID generation per request (about 0.3 µs, 1 allocation). Every span value comes from the record, so export contents cannot disagree with the request view. The cost is having only one gateway span, with phases represented as attributes and events rather than child spans.
- **No trace ID index** (2026-10-15 benchmark, 1 million rows): indexing random trace IDs raises per-row write time from 59 µs to 250 µs (each insert dirties a different index page), and increasing the page cache does not help. Without an index, overhead is only 12%. The cost is that lookups require a time window, and normal requests omitted by sampling cannot be found (all abnormal requests are retained; the full set is in the external tracing system).
- **Do not bump the schema version**: the added column has a default value, so older versions writing by column name are unaffected, and rollback does not require moving `ux.db` aside.
- **Do not use engine OTel as the data source**: its coverage is narrow, it requires nodes to reach the Collector, and it has fewer fields. Keep it as an optional supplement for users.
- **Traces only**: exporting aggregate metrics (a Prometheus scrape endpoint or OTLP metrics) is a separate discussion.

## Consequences

- Every request in the request view and lookup endpoint includes `trace_id` and `deployment_id`; in-flight requests also include `trace_id`.
- When an upstream gateway supplies `traceparent`, the platform gateway span is a child of the upstream span in the external tracing system; the trace ID links the two. The server still assigns its own request ID (`rN` in access logs; the client's `X-Request-ID` does not replace it). The span's `gpuworks.request.id` and the response header `X-Request-ID` contain the same value.
- The settings page lets you configure the Collector address, headers, sampling ratio, and trace link template, and send a test span.
- Documentation: [Export Request Traces to OpenTelemetry](../how-to/export-request-traces.md), [OpenAI-Compatible Gateway](../reference/openai-gateway.md#trace-context), [API Reference](../reference/api.md).

<!-- translation-source-sha256: c445dab4b4afc365509569005f4f2aed36b6eea5dc09064a613b94b83c2a466f -->
