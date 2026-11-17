# Observability Limits of Per-Request Engine Queue Time

A monitored request's `engine_queue_ms` must come from timing reported in that same inference response.
Queue counts in `/metrics` are instantaneous gauges, and service histograms contain aggregate samples.
Neither identifies an individual request's queue duration. Missing fields and JSON `null` remain unknown;
only an explicit numeric `0` means measured zero. If only queue time is reported, retain that segment and
leave the remainder as waiting rather than inventing zero Prefill time.

GWKS-93 checked the built-in Strata image version 0.1.38. The pinned upstream source,
[`09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py`](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py),
shows that:

- `request_timings` (line 1624) returns known stages such as prompt/decode; standard responses omit queue time.
- Optional `begin_request` (line 1232) and `/api/requests` (line 2147) retain internal `queue_s`, but the internal ID
  is generated independently at line 1237. The OpenAI completion ID is generated separately at line 1751. Full
  nonstreaming response bodies in monitor details can contain that ID, but summaries and streaming monitoring have no stable body-free correlation.
- `api_monitor` is disabled by default. Enabling it retains input/output bodies for the latest 100 requests (line 970), so the platform should not enable it automatically.

Unmodified Strata responses can therefore provide Prefill and Decode timing, but cannot reliably recover an individual
request's 20.3 seconds of queueing. This requires queue timing in the same response or a supported stable
request correlation; matching by timestamp or body is insufficient. vLLM's `--enable-per-request-metrics`, available
since 0.25.0, has separate existing support, and the platform reads its reported fields.
For field declarations and measurement semantics, see [Inference Metric Vocabulary](../reference/inference-metrics.md#per-request-statistics).

## Built-in Strata Adaptation

The Strata catalog carries a lightweight patch and launcher through the existing runtime asset mechanism, keeping
the base image unchanged. The launcher accepts only the pinned 0.1.38 source hash or the already-patched hash,
atomically replaces the HTTP handler in the container's writable layer, then forwards arguments unchanged to the
image's original entrypoint. Unknown source or unwritable unpatched source fails startup explicitly. The asset itself
is content-hash checked and mounted read-only. Build and verification entrypoints are in `build/strata-queue/`.

The patch uses a monotonic clock to measure the initial model-load FIFO wait and first-generation FIFO admission
wait, reporting milliseconds as `timings.queue_ms` in the same streaming or nonstreaming response. Model loading,
prefill, decode and MCP tool execution are excluded. Timing freezes at the first generation admission, so later MCP
rounds cannot move their queueing before the first token. Failed requests without successful terminal timing remain
unknown. The platform reads this through the catalog's `timings.queue_ms → queue_seconds` declaration without
enabling `api_monitor` or retaining bodies for correlation.

The adaptation runs only with the updated catalog configuration. Existing running instances are not modified live;
update a deployment to this configuration and recreate its instances to emit the new field. This adds per-request
observability but cannot recover the original request's missing 20.3-second evidence or fix a native-engine hang.

<!-- translation-source-sha256: 178a2b29a515f01f6856c133aa0810b3b0be5d95b3a38ea07ccd7d3486474cd0 -->
